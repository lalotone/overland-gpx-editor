/*
 * Points of interest along a route, from OpenStreetMap via Overpass.
 *
 * Fuel range is the planning failure that actually strands people on long
 * dirt routes, so the fuel layer doubles as an input to the "longest gap
 * without fuel" figure shown next to the track stats.
 */

import { fetchSpanishFuelPois, intersectsSpain } from './fuel'
import type { Coordinate } from './types'

const OVERPASS_ENDPOINT = 'https://overpass-api.de/api/interpreter'

export type PoiKind = 'fuel' | 'water' | 'camp'

/** Extra popup content, for sources that carry more than a name. */
export interface PoiDetail {
  lines: { label: string; value: string }[]
  note?: string
  /** Who said so, and when — shown so a stale price is not read as current. */
  source?: string
}

export interface Poi {
  id: string
  kind: PoiKind
  lat: number
  lon: number
  name?: string
  detail?: PoiDetail
  /** Numeric values the source attached, e.g. fuel prices in €/L by label. */
  prices?: Record<string, number>
}

export interface PoiKindDefinition {
  id: PoiKind
  label: string
  title: string
  /** Single-glyph marker label. */
  glyph: string
  color: string
}

export const POI_KINDS: PoiKindDefinition[] = [
  { id: 'fuel', label: 'Fuel', title: 'Fuel stations near the route', glyph: '⛽', color: '#ef4444' },
  { id: 'water', label: 'Water', title: 'Drinking water near the route', glyph: '💧', color: '#0ea5e9' },
  { id: 'camp', label: 'Camp', title: 'Campsites near the route', glyph: '⛺', color: '#16a34a' },
]

const SELECTORS: Record<PoiKind, string> = {
  fuel: 'nwr["amenity"="fuel"]',
  water: 'nwr["amenity"="drinking_water"]',
  camp: 'nwr["tourism"~"^(camp_site|caravan_site)$"]',
}

export interface BoundingBox {
  south: number
  west: number
  north: number
  east: number
}

/** Track bounds padded by roughly `padKm` on each side. */
export function boundsAround(coords: Coordinate[], padKm = 5): BoundingBox | null {
  if (coords.length === 0) return null
  let south = Infinity
  let north = -Infinity
  let west = Infinity
  let east = -Infinity
  for (const c of coords) {
    if (c.lat < south) south = c.lat
    if (c.lat > north) north = c.lat
    if (c.lon < west) west = c.lon
    if (c.lon > east) east = c.lon
  }
  const latPad = padKm / 111
  const midLat = (south + north) / 2
  const lonPad = padKm / (111 * Math.max(0.2, Math.cos((midLat * Math.PI) / 180)))
  return {
    south: south - latPad,
    west: west - lonPad,
    north: north + latPad,
    east: east + lonPad,
  }
}

/** Overpass caps a reply here; the Spanish fuel layer matches it. */
const RESULT_LIMIT = 400

export interface PoiSearchResult {
  pois: Poi[]
  /** Human-readable source, when it is not plain OpenStreetMap. */
  source?: string
  /** True when more were found than the limit allows. */
  truncated?: boolean
}

/**
 * POIs of one kind inside an area, from the best source for that area.
 *
 * Inside Spain the ministry's fuel dataset beats the OSM fuel layer outright:
 * it is the authoritative list and it carries prices. If it is unreachable
 * the OSM layer still answers, so fuel never stops working — it just loses
 * the prices.
 */
export async function fetchPoisForArea(
  kind: PoiKind,
  bbox: BoundingBox,
  signal?: AbortSignal,
): Promise<PoiSearchResult> {
  if (kind === 'fuel' && intersectsSpain(bbox)) {
    try {
      const { pois, published, truncated } = await fetchSpanishFuelPois(bbox, RESULT_LIMIT, signal)
      // Outside Spain but inside the envelope — say, Perpignan — the official
      // list is simply empty, which is not an answer. Fall through to OSM.
      if (pois.length > 0) {
        return {
          pois,
          source: published ? `official prices, ${published}` : 'official prices',
          truncated,
        }
      }
    } catch (err) {
      if ((err as Error).name === 'AbortError') throw err
      // Fall through to OSM below.
    }
  }

  return { pois: await fetchPois(kind, bbox, signal) }
}

/** Approximate width and height of a bounding box, in kilometres. */
export function boundingBoxSpanKm(bbox: BoundingBox): { widthKm: number; heightKm: number } {
  const midLat = (bbox.south + bbox.north) / 2
  return {
    widthKm: (bbox.east - bbox.west) * 111 * Math.cos((midLat * Math.PI) / 180),
    heightKm: (bbox.north - bbox.south) * 111,
  }
}

/**
 * Largest view worth searching. Overpass returns at most 400 results, and 400
 * points spread over half a country reads as "no fuel here" in the gaps where
 * really we just stopped counting — better to ask for a smaller area.
 */
export const MAX_SEARCH_SPAN_KM = 250

interface OverpassElement {
  type: string
  id: number
  lat?: number
  lon?: number
  center?: { lat: number; lon: number }
  tags?: Record<string, string>
}

export async function fetchPois(
  kind: PoiKind,
  bbox: BoundingBox,
  signal?: AbortSignal,
): Promise<Poi[]> {
  const area = `${bbox.south.toFixed(5)},${bbox.west.toFixed(5)},${bbox.north.toFixed(5)},${bbox.east.toFixed(5)}`
  const query = `[out:json][timeout:30];${SELECTORS[kind]}(${area});out center 400;`

  const res = await fetch(OVERPASS_ENDPOINT, {
    method: 'POST',
    headers: { 'Content-Type': 'application/x-www-form-urlencoded' },
    body: `data=${encodeURIComponent(query)}`,
    signal,
  })
  // Overpass is a free shared service and sheds load often enough that it is
  // worth naming: 429/504 mean "come back later", not "your query is wrong".
  if (res.status === 429 || res.status === 504) {
    throw new Error('Overpass is busy right now — try again in a moment')
  }
  if (!res.ok) throw new Error(`Overpass returned ${res.status}`)

  // An overloaded Overpass answers HTTP 200 with an HTML error page, so
  // parsing has to be guarded — otherwise the user sees a JSON syntax error.
  const body = await res.text()
  let data: { elements?: OverpassElement[] }
  try {
    data = JSON.parse(body)
  } catch {
    throw new Error(
      /too busy|timeout|rate_?limit/i.test(body)
        ? 'Overpass is busy right now — try again in a moment'
        : 'Overpass returned a response that could not be read',
    )
  }

  const elements: OverpassElement[] = data.elements ?? []
  const pois: Poi[] = []
  for (const el of elements) {
    const lat = el.lat ?? el.center?.lat
    const lon = el.lon ?? el.center?.lon
    if (lat === undefined || lon === undefined) continue
    pois.push({
      id: `${el.type}/${el.id}`,
      kind,
      lat,
      lon,
      name: el.tags?.name ?? el.tags?.brand ?? el.tags?.operator,
    })
  }
  return pois
}
