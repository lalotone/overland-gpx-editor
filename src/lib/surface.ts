/*
 * What the route is actually made of.
 *
 * The routing profiles bias a route towards or away from dirt, but they never
 * say what came back: a "Dirt" route through farmland can be 90% tarmac, and
 * a "Road" route can drop onto a gravel link for two kilometres. Valhalla's
 * `/trace_attributes` walks a shape back along the graph edges it came from
 * and reports each edge's OSM `surface` tag, which is what turns that guess
 * into a number.
 *
 * Surface is advisory, not survey data: it is only as good as the OSM tagging
 * along the route, and untagged edges come back as `unknown` rather than
 * being quietly counted as sealed.
 */

import { haversineDistance } from './geo'
import { motorcycleCosting, VALHALLA_API } from './routing'
import type { RoutingProfile } from './routing'
import type { Coordinate } from './types'

export type SurfaceClass = 'paved' | 'compacted' | 'dirt' | 'path' | 'unknown'

export interface SurfaceDefinition {
  id: SurfaceClass
  label: string
  color: string
  hint: string
  /** Counted towards the "unpaved" share. `unknown` counts towards neither. */
  unpaved: boolean
}

/*
 * Colours are deliberately unlike the gradient and altitude ramps — surface is
 * a different question from steepness, and reusing those hues would have the
 * two modes read as the same picture.
 */
export const SURFACE_CLASSES: SurfaceDefinition[] = [
  {
    id: 'paved',
    label: 'Sealed road',
    color: '#3b4a5a',
    hint: 'Asphalt or concrete — tarmac of any quality',
    unpaved: false,
  },
  {
    id: 'compacted',
    label: 'Gravel / compacted',
    color: '#c9a227',
    hint: 'Graded gravel or compacted hardcore — an easy dirt road',
    unpaved: true,
  },
  {
    id: 'dirt',
    label: 'Dirt track',
    color: '#b5651d',
    hint: 'Unsurfaced earth or sand — the classic offroad track',
    unpaved: true,
  },
  {
    id: 'path',
    label: 'Path / rough',
    color: '#a13d2d',
    hint: 'Narrow path or ground with no made surface at all',
    unpaved: true,
  },
  {
    id: 'unknown',
    label: 'Unknown',
    color: '#94a3b8',
    hint: 'No surface tag in OSM here — could be anything',
    unpaved: false,
  },
]

const SURFACE_BY_ID = new Map(SURFACE_CLASSES.map(s => [s.id, s]))

export function surfaceDefinition(id: SurfaceClass): SurfaceDefinition {
  return SURFACE_BY_ID.get(id) ?? SURFACE_CLASSES[SURFACE_CLASSES.length - 1]
}

export function surfaceColor(id: SurfaceClass): string {
  return surfaceDefinition(id).color
}

/** Valhalla's surface enum, collapsed to the distinctions a rider cares about. */
export function classifySurface(surface: string | undefined): SurfaceClass {
  switch (surface) {
    case 'paved_smooth':
    case 'paved':
    case 'paved_rough':
      return 'paved'
    case 'compacted':
    case 'gravel':
      return 'compacted'
    case 'dirt':
      return 'dirt'
    case 'path':
    case 'impassable':
      return 'path'
    default:
      return 'unknown'
  }
}

/* -- Tracing ---------------------------------------------------------- */

/*
 * Server-side limits on a single trace request. The public instance rejects a
 * trace whose path exceeds 200 km outright (error 154), so long routes are cut
 * into chunks and stitched. Both limits are set below the server's so a route
 * that sits just under one never round-trips only to fail.
 */
const MAX_TRACE_KM = 150
const MAX_TRACE_POINTS = 5000
/** Public instance — chunks go two at a time rather than all at once. */
const CONCURRENCY = 2
/*
 * How far the matched shape may drift from the one we sent before the result
 * is called approximate: a few points for a chunk that ends mid-edge, or 1%
 * for the routine snapping a trail route picks up along its length.
 *
 * This catches a shape that came back grossly different. It cannot catch a
 * matcher that put the route on the wrong roads while returning the same
 * number of points — nothing in the response would show that.
 */
const TOLERATED_POINT_DRIFT = 4
const TOLERATED_DRIFT_FRACTION = 0.01

export class SurfaceUnavailableError extends Error {
  constructor(message = 'Surface data unavailable') {
    super(message)
    this.name = 'SurfaceUnavailableError'
  }
}

export interface SurfaceResult {
  /**
   * One class per segment, so `segments[i]` describes the leg from
   * `coordinates[i]` to `coordinates[i + 1]`. Length is `coordinates.length - 1`.
   */
  segments: SurfaceClass[]
  /**
   * True when the matcher returned a shape that did not line up with the one
   * we sent, so classes had to be mapped across proportionally. Surfaced to
   * the UI rather than passed off as an exact read.
   */
  approximate: boolean
}

interface TraceEdge {
  surface?: string
  begin_shape_index?: number
  end_shape_index?: number
}

/**
 * Cut the shape into pieces each server-legal on its own. Exported so the
 * verify harness can assert the limits without going near the network.
 *
 * Chunks overlap by one point so the segment between the last point of one
 * chunk and the first of the next still gets classified — without the overlap
 * every chunk boundary would leave a hole in the line.
 */
export function chunkShape(coords: Coordinate[]): { start: number; points: Coordinate[] }[] {
  const chunks: { start: number; points: Coordinate[] }[] = []
  let start = 0

  while (start < coords.length - 1) {
    let km = 0
    let end = start
    while (end < coords.length - 1) {
      // haversineDistance is in metres; the trace limit is expressed in km.
      const step = haversineDistance(coords[end], coords[end + 1]) / 1000
      if (end > start && (km + step > MAX_TRACE_KM || end - start + 1 >= MAX_TRACE_POINTS)) break
      km += step
      end++
    }
    chunks.push({ start, points: coords.slice(start, end + 1) })
    start = end
  }

  return chunks
}

async function traceChunk(
  points: Coordinate[],
  profile: RoutingProfile,
  signal?: AbortSignal,
): Promise<{ classes: SurfaceClass[]; approximate: boolean }> {
  const costing = motorcycleCosting(profile)
  const res = await fetch(`${VALHALLA_API}/trace_attributes`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({
      shape: points.map(p => ({ lat: p.lat, lon: p.lon })),
      // walk_or_snap follows our own shape edge by edge where it can and only
      // falls back to map matching where it cannot, which keeps the returned
      // shape indices aligned with the points we sent.
      shape_match: 'walk_or_snap',
      ...costing,
      filters: {
        action: 'include',
        attributes: ['edge.surface', 'edge.begin_shape_index', 'edge.end_shape_index', 'shape'],
      },
    }),
    signal,
  })

  if (!res.ok) {
    throw new SurfaceUnavailableError(
      res.status === 400
        ? 'The routing server could not match this route to known roads'
        : `Surface lookup failed (${res.status})`,
    )
  }

  const data = (await res.json()) as { edges?: TraceEdge[]; shape?: string }
  const edges = data.edges
  if (!Array.isArray(edges)) throw new SurfaceUnavailableError('Malformed surface response')

  const segmentCount = points.length - 1
  const classes: SurfaceClass[] = new Array(segmentCount).fill('unknown')

  // Indices returned by the matcher address its own shape, which is normally
  // ours unchanged. Any difference is scaled across, but only a large one is
  // worth telling the user about — a warning on every long route would train
  // itself to be ignored.
  const matchedPoints = countPolylinePoints(data.shape)
  const drift = matchedPoints > 0 ? matchedPoints - points.length : 0
  const scale = matchedPoints > 1 && drift !== 0 ? (points.length - 1) / (matchedPoints - 1) : 1
  const approximate =
    Math.abs(drift) > Math.max(TOLERATED_POINT_DRIFT, points.length * TOLERATED_DRIFT_FRACTION)

  for (const edge of edges) {
    const rawBegin = edge.begin_shape_index
    const rawEnd = edge.end_shape_index
    if (typeof rawBegin !== 'number' || typeof rawEnd !== 'number') continue

    const begin = Math.max(0, Math.round(rawBegin * scale))
    const end = Math.min(segmentCount, Math.round(rawEnd * scale))
    const cls = classifySurface(edge.surface)
    for (let i = begin; i < end; i++) classes[i] = cls
  }

  return { classes, approximate }
}

/** Point count of a precision-6 polyline, without materialising the points. */
function countPolylinePoints(encoded: string | undefined): number {
  if (!encoded) return 0
  let count = 0
  let values = 0
  for (let i = 0; i < encoded.length; i++) {
    // Each value ends on a byte below 0x20; two values make one point.
    if (encoded.charCodeAt(i) - 63 < 0x20) {
      values++
      if (values === 2) { count++; values = 0 }
    }
  }
  return count
}

/**
 * Classify every segment of a routed line by surface.
 *
 * Throws `SurfaceUnavailableError` when the trace service cannot answer, which
 * callers should treat as "no surface data" rather than a broken route — the
 * route itself is unaffected.
 */
export async function fetchRouteSurface(
  coordinates: Coordinate[],
  profile: RoutingProfile,
  signal?: AbortSignal,
): Promise<SurfaceResult> {
  if (coordinates.length < 2) return { segments: [], approximate: false }

  const chunks = chunkShape(coordinates)
  const segments: SurfaceClass[] = new Array(coordinates.length - 1).fill('unknown')
  let approximate = false

  for (let i = 0; i < chunks.length; i += CONCURRENCY) {
    const batch = chunks.slice(i, i + CONCURRENCY)
    const results = await Promise.all(
      batch.map(c => traceChunk(c.points, profile, signal)),
    )
    results.forEach((result, j) => {
      const offset = batch[j].start
      result.classes.forEach((cls, k) => { segments[offset + k] = cls })
      approximate = approximate || result.approximate
    })
  }

  return { segments, approximate }
}

/* -- Summary ---------------------------------------------------------- */

export interface SurfaceShare {
  id: SurfaceClass
  km: number
  fraction: number
}

export interface SurfaceSummary {
  /** Non-empty classes, longest first. */
  shares: SurfaceShare[]
  totalKm: number
  unpavedKm: number
  /** Share of the route that is known to be unpaved, 0..1. */
  unpavedFraction: number
  /** Distance with no surface tag — the caveat on the numbers above. */
  unknownKm: number
}

/**
 * Distance per surface class.
 *
 * Weighted by segment length rather than segment count: a route is mostly a
 * few long road segments and many short twisty dirt ones, so counting
 * segments would report the opposite of the truth.
 */
export function summarizeSurface(segments: SurfaceClass[], cumKm: number[]): SurfaceSummary {
  const byClass = new Map<SurfaceClass, number>()
  let totalKm = 0

  for (let i = 0; i < segments.length && i + 1 < cumKm.length; i++) {
    const km = cumKm[i + 1] - cumKm[i]
    if (!(km > 0)) continue
    byClass.set(segments[i], (byClass.get(segments[i]) ?? 0) + km)
    totalKm += km
  }

  const shares: SurfaceShare[] = SURFACE_CLASSES
    .map(def => ({ id: def.id, km: byClass.get(def.id) ?? 0 }))
    .filter(s => s.km > 0)
    .map(s => ({ ...s, fraction: totalKm > 0 ? s.km / totalKm : 0 }))
    .sort((a, b) => b.km - a.km)

  const unpavedKm = shares
    .filter(s => surfaceDefinition(s.id).unpaved)
    .reduce((sum, s) => sum + s.km, 0)

  return {
    shares,
    totalKm,
    unpavedKm,
    unpavedFraction: totalKm > 0 ? unpavedKm / totalKm : 0,
    unknownKm: byClass.get('unknown') ?? 0,
  }
}
