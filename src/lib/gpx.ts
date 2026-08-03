/*
 * GPX reading and writing.
 *
 * Parsing goes through DOMParser rather than regexes: real files from OsmAnd,
 * Wikiloc, Garmin and Gaia differ in attribute order, quoting, namespace
 * prefixes and self-closing tags, and a regex that assumes one shape silently
 * returns zero points for the others.
 */

import type { Coordinate, GpxWaypoint, Track } from './types'

const GPX_NS = 'http://www.topografix.com/GPX/1/1'
const XSI_NS = 'http://www.w3.org/2001/XMLSchema-instance'

/* -- DOM helpers (namespace-agnostic) -------------------------------- */

/*
 * Child lookups walk firstElementChild/nextElementSibling rather than indexing
 * `parent.children`. `children` is a live HTMLCollection, and indexing it can
 * cost O(n) per access, which made parsing a long track quadratic — a
 * 7.6k-point file took ~31 s. Sibling traversal is O(1) per step.
 */

function directChildren(parent: Element, localName: string): Element[] {
  const out: Element[] = []
  for (let c = parent.firstElementChild; c; c = c.nextElementSibling) {
    if (c.localName === localName) out.push(c)
  }
  return out
}

function directChild(parent: Element, localName: string): Element | undefined {
  for (let c = parent.firstElementChild; c; c = c.nextElementSibling) {
    if (c.localName === localName) return c
  }
  return undefined
}

function childText(parent: Element, localName: string): string | undefined {
  const v = directChild(parent, localName)?.textContent?.trim()
  return v ? v : undefined
}

function childNumber(parent: Element, localName: string): number | undefined {
  const raw = childText(parent, localName)
  if (raw === undefined) return undefined
  const n = parseFloat(raw)
  return Number.isFinite(n) ? n : undefined
}

/* -- Parsing ---------------------------------------------------------- */

function parsePoint(el: Element): Coordinate | null {
  const lat = parseFloat(el.getAttribute('lat') ?? '')
  const lon = parseFloat(el.getAttribute('lon') ?? '')
  if (!Number.isFinite(lat) || !Number.isFinite(lon)) return null
  return {
    lat,
    lon,
    elevation: childNumber(el, 'ele'),
    time: childText(el, 'time'),
  }
}

function parseWaypoint(el: Element): GpxWaypoint | null {
  const pt = parsePoint(el)
  if (!pt) return null
  return {
    lat: pt.lat,
    lon: pt.lon,
    elevation: pt.elevation,
    name: childText(el, 'name'),
    desc: childText(el, 'desc') ?? childText(el, 'cmt'),
    sym: childText(el, 'sym'),
    type: childText(el, 'type'),
  }
}

function toTrack(
  name: string,
  coordinates: Coordinate[],
  extra: Partial<Track>,
): Track {
  return {
    name,
    filename: '',
    coordinates,
    elevations: coordinates.map(c => (c.elevation === undefined ? null : c.elevation)),
    waypoints: [],
    ...extra,
  }
}

/**
 * Parse a GPX document into tracks.
 *
 * - `<trkseg>`s inside one `<trk>` are concatenated: segments mark recording
 *   gaps within a single track, they are not separate tracks.
 * - `<rte>` elements are read too, so planner exports (which contain no
 *   `<trk>` at all) still load.
 *
 * @throws if the content is not parseable XML or is not a GPX document.
 */
export function parseGPX(content: string): Track[] {
  const doc = new DOMParser().parseFromString(content, 'application/xml')
  if (doc.getElementsByTagName('parsererror').length > 0) {
    throw new Error('File is not valid XML')
  }

  const root = doc.documentElement
  if (!root || root.localName !== 'gpx') {
    throw new Error('Not a GPX file — no <gpx> root element')
  }

  const creator = root.getAttribute('creator') ?? undefined
  const metadata = directChild(root, 'metadata')
  const metaName = metadata ? childText(metadata, 'name') : undefined
  const metaTime = metadata ? childText(metadata, 'time') : undefined

  // <wpt> lives at file level, not inside a track, so every track parsed from
  // this file carries the same waypoint list.
  const waypoints = directChildren(root, 'wpt')
    .map(parseWaypoint)
    .filter((w): w is GpxWaypoint => w !== null)

  const tracks: Track[] = []

  for (const trk of directChildren(root, 'trk')) {
    const coordinates: Coordinate[] = []
    for (const seg of directChildren(trk, 'trkseg')) {
      for (const pt of directChildren(seg, 'trkpt')) {
        const c = parsePoint(pt)
        if (c) coordinates.push(c)
      }
    }
    if (coordinates.length === 0) continue
    tracks.push(
      toTrack(childText(trk, 'name') ?? metaName ?? `Track ${tracks.length + 1}`, coordinates, {
        waypoints,
        creator,
        time: metaTime ?? coordinates[0].time,
      }),
    )
  }

  for (const rte of directChildren(root, 'rte')) {
    const coordinates: Coordinate[] = []
    for (const pt of directChildren(rte, 'rtept')) {
      const c = parsePoint(pt)
      if (c) coordinates.push(c)
    }
    if (coordinates.length === 0) continue
    tracks.push(
      toTrack(childText(rte, 'name') ?? metaName ?? `Route ${tracks.length + 1}`, coordinates, {
        waypoints,
        creator,
        time: metaTime,
        isRoute: true,
      }),
    )
  }

  return tracks
}

/* -- Writing ---------------------------------------------------------- */

function esc(value: string): string {
  return value
    .replace(/&/g, '&amp;')
    .replace(/</g, '&lt;')
    .replace(/>/g, '&gt;')
    .replace(/"/g, '&quot;')
}

/** Trim to the 7 decimals GPX consumers expect; avoids 1e-7 exponent output. */
function coord(n: number): string {
  return n.toFixed(7).replace(/\.?0+$/, '')
}

export interface GpxExportOptions {
  name: string
  coordinates: Coordinate[]
  waypoints?: GpxWaypoint[]
  /** Document timestamp; defaults to now. */
  time?: string
  description?: string
}

/**
 * Serialise to GPX 1.1. Emits the namespace declarations that strict
 * consumers (several Garmin units among them) require, escapes text, and
 * preserves per-point elevation and timestamps plus file-level waypoints.
 */
export function buildGPX({
  name,
  coordinates,
  waypoints = [],
  time,
  description,
}: GpxExportOptions): string {
  const lines: string[] = []
  lines.push('<?xml version="1.0" encoding="UTF-8"?>')
  lines.push(
    `<gpx version="1.1" creator="GPX Editor" xmlns="${GPX_NS}" xmlns:xsi="${XSI_NS}" ` +
      `xsi:schemaLocation="${GPX_NS} http://www.topografix.com/GPX/1/1/gpx.xsd">`,
  )

  lines.push('  <metadata>')
  lines.push(`    <name>${esc(name)}</name>`)
  if (description) lines.push(`    <desc>${esc(description)}</desc>`)
  lines.push(`    <time>${esc(time ?? new Date().toISOString())}</time>`)
  lines.push('  </metadata>')

  for (const w of waypoints) {
    lines.push(`  <wpt lat="${coord(w.lat)}" lon="${coord(w.lon)}">`)
    if (w.elevation !== undefined && Number.isFinite(w.elevation)) {
      lines.push(`    <ele>${w.elevation.toFixed(2)}</ele>`)
    }
    if (w.name) lines.push(`    <name>${esc(w.name)}</name>`)
    if (w.desc) lines.push(`    <desc>${esc(w.desc)}</desc>`)
    if (w.sym) lines.push(`    <sym>${esc(w.sym)}</sym>`)
    if (w.type) lines.push(`    <type>${esc(w.type)}</type>`)
    lines.push('  </wpt>')
  }

  lines.push('  <trk>')
  lines.push(`    <name>${esc(name)}</name>`)
  lines.push('    <trkseg>')
  for (const c of coordinates) {
    lines.push(`      <trkpt lat="${coord(c.lat)}" lon="${coord(c.lon)}">`)
    if (c.elevation !== undefined && Number.isFinite(c.elevation)) {
      lines.push(`        <ele>${c.elevation.toFixed(2)}</ele>`)
    }
    if (c.time) lines.push(`        <time>${esc(c.time)}</time>`)
    lines.push('      </trkpt>')
  }
  lines.push('    </trkseg>')
  lines.push('  </trk>')
  lines.push('</gpx>')
  return lines.join('\n')
}

/**
 * Display title for a stored track, derived from its filename.
 *
 * The filename is the identity of a track — a GPX `<name>` is whatever the
 * exporter felt like writing, is frequently absent, and is routinely shared by
 * unrelated files (four tracks in the sample library all call themselves
 * "Created Track"). Titling the library off the filename keeps one card per
 * file and keeps those cards tellable apart.
 *
 * Underscores become spaces and slug hyphens become spaces, but a hyphen
 * between digits is left alone so dates and times survive:
 * `2020-01-01_09-30_Wed.gpx` → `2020-01-01 09-30 Wed`.
 */
export function fromGpxFilename(filename: string): string {
  const title = filename
    .replace(/\.gpx$/i, '')
    .replace(/_+/g, ' ')
    .replace(/(?<![0-9])-|-(?![0-9])/g, ' ')
    .replace(/\s+/g, ' ')
    .trim()
  return title.replace(/^\p{Ll}/u, c => c.toUpperCase()) || filename
}

/** Filesystem-safe .gpx filename derived from a display name. */
export function toGpxFilename(name: string): string {
  const slug = name
    .trim()
    .toLowerCase()
    .normalize('NFD')
    .replace(/[\u0300-\u036f]/g, '')
    .replace(/[^a-z0-9]+/g, '-')
    .replace(/^-+|-+$/g, '')
  return `${slug || 'track'}.gpx`
}
