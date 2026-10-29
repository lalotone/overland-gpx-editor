/*
 * "What's around this point": water, huts, camps, fuel, repairs and
 * viewpoints within a radius of a planner point, from OpenStreetMap.
 *
 * Overpass is a free shared service, so a search is built to cost it as
 * little as possible:
 *
 * - One union query fetches every category at once; the client filters.
 * - The search area is the circle's bounding box rounded outward to a fixed
 *   grid, so nearby points ask for the identical area and hit the backend's
 *   shared cache instead of Overpass.
 * - A session cache answers any circle that lies inside an area already
 *   fetched, so a smaller radius or a point a few km on never refetches.
 * - It only runs when asked, one search at a time, behind the backend's
 *   one-request-per-second Overpass limit.
 *
 * What comes back is what OSM says, no more: a spring is not drinkable
 * unless tagged so, and a hut's access or season is shown when tagged.
 */

import { haversineDistance } from './geo'
import { fetchOverpassElements, overpassArea } from './poi'
import type { BoundingBox, OverpassElement } from './poi'
import type { CacheMetadata, RuntimeRequestContext } from './offline'
import type { WaypointMarkerId } from './waypointMarkers'
import type { Coordinate } from './types'

export type AroundCategory = 'water' | 'hut' | 'camp' | 'fuel' | 'repair' | 'viewpoint'

export interface AroundCategoryDefinition {
  id: AroundCategory
  label: string
  glyph: string
  color: string
  /** Marker used when the result is kept as a place in the GPX. */
  marker: WaypointMarkerId
}

export const AROUND_CATEGORIES: AroundCategoryDefinition[] = [
  { id: 'water', label: 'Water', glyph: '💧', color: '#0284c7', marker: 'water' },
  { id: 'hut', label: 'Huts & shelters', glyph: '🛖', color: '#7c3aed', marker: 'lodging' },
  { id: 'camp', label: 'Camping', glyph: '⛺', color: '#15803d', marker: 'camp' },
  { id: 'fuel', label: 'Fuel', glyph: '⛽', color: '#dc2626', marker: 'fuel' },
  { id: 'repair', label: 'Moto shops', glyph: '🔧', color: '#b45309', marker: 'repair' },
  { id: 'viewpoint', label: 'Viewpoints', glyph: '◉', color: '#0891b2', marker: 'viewpoint' },
]

const CATEGORY_BY_ID = new Map(AROUND_CATEGORIES.map(category => [category.id, category]))

export function aroundCategory(id: AroundCategory): AroundCategoryDefinition {
  return CATEGORY_BY_ID.get(id) ?? AROUND_CATEGORIES[0]
}

export const AROUND_RADII_KM = [2, 5, 10, 20, 30] as const
export const DEFAULT_AROUND_RADIUS_KM = 10
/** Matches the backend's `out center` cap for the union query. */
export const AROUND_RESULT_LIMIT = 1500
/** Grid the search area snaps outward to, in degrees (~5.5 km of latitude). */
export const AROUND_GRID_DEG = 0.05

/** Mirrors the backend union, for the direct no-backend fallback. */
const AROUND_SELECTORS =
  'nwr["amenity"~"^(drinking_water|water_point|fuel)$"];' +
  'nwr["natural"="spring"];' +
  'nwr["tourism"~"^(alpine_hut|wilderness_hut|camp_site|caravan_site|viewpoint)$"];' +
  'nwr["amenity"="shelter"]["shelter_type"!="public_transport"];' +
  'nwr["shop"~"^(motorcycle|motorcycle_repair)$"];'

export function classifyAround(tags: Record<string, string> | undefined): AroundCategory | null {
  if (!tags) return null
  const { amenity, tourism, natural, shop } = tags
  if (amenity === 'drinking_water' || amenity === 'water_point' || natural === 'spring') return 'water'
  if (tourism === 'alpine_hut' || tourism === 'wilderness_hut') return 'hut'
  if (amenity === 'shelter' && tags.shelter_type !== 'public_transport') return 'hut'
  if (tourism === 'camp_site' || tourism === 'caravan_site') return 'camp'
  if (amenity === 'fuel') return 'fuel'
  if (shop === 'motorcycle' || shop === 'motorcycle_repair') return 'repair'
  if (tourism === 'viewpoint') return 'viewpoint'
  return null
}

function kindLabel(tags: Record<string, string>): string {
  if (tags.amenity === 'drinking_water') return 'Drinking water'
  if (tags.amenity === 'water_point') return 'Water point'
  if (tags.natural === 'spring') return 'Spring'
  if (tags.tourism === 'alpine_hut') return 'Mountain hut'
  if (tags.tourism === 'wilderness_hut') return 'Wilderness hut'
  if (tags.amenity === 'shelter') return tags.shelter_type ? `Shelter (${tags.shelter_type.replace(/_/g, ' ')})` : 'Shelter'
  if (tags.tourism === 'caravan_site') return 'Caravan site'
  if (tags.tourism === 'camp_site') return 'Campsite'
  if (tags.amenity === 'fuel') return 'Fuel'
  if (tags.shop === 'motorcycle_repair') return 'Motorcycle repair'
  if (tags.shop === 'motorcycle') return tags['service:vehicle:repair'] === 'yes' || tags.repair === 'yes' ? 'Motorcycle shop & repair' : 'Motorcycle shop'
  return 'Viewpoint'
}

const YES_NO: Record<string, string> = { yes: 'yes', no: 'no', conditional: 'conditional' }

/** Facts worth knowing before riding there, only as far as they are tagged. */
export function aroundFacts(tags: Record<string, string>, category: AroundCategory): { label: string; value: string }[] {
  const facts: { label: string; value: string }[] = []
  const add = (label: string, value: string | undefined) => {
    if (value) facts.push({ label, value })
  }
  if (category === 'water') {
    // A spring or water point says nothing about potability unless tagged.
    const drinkable = tags.drinking_water ?? (tags.amenity === 'drinking_water' ? 'yes' : undefined)
    add('Drinkable', drinkable ? YES_NO[drinkable] ?? drinkable : 'not stated')
    if (tags['drinking_water:legal'] === 'no') add('Legal status', 'not officially potable')
  }
  if (category === 'hut') {
    add('Beds', tags.beds ?? tags.capacity)
    add('Fireplace', tags.fireplace)
    add('Locked', tags.locked)
  }
  add('Access', tags.access && tags.access !== 'yes' ? tags.access : undefined)
  add('Fee', tags.fee)
  add('Seasonal', tags.seasonal)
  add('Hours', tags.opening_hours)
  add('Elevation', tags.ele ? `${tags.ele} m` : undefined)
  add('Operator', tags.operator)
  add('Phone', tags.phone ?? tags['contact:phone'])
  return facts
}

export interface AroundPoi {
  id: string
  category: AroundCategory
  lat: number
  lon: number
  name?: string
  kind: string
  facts: { label: string; value: string }[]
  /** Straight-line distance from the search point, metres. */
  distanceM: number
}

interface AreaResult {
  area: BoundingBox
  elements: OverpassElement[]
  truncated: boolean
  cache?: CacheMetadata
}

/** The circle's bounding box, rounded outward to the grid. */
export function aroundSearchArea(center: Coordinate, radiusKm: number, grid = AROUND_GRID_DEG): BoundingBox {
  const latPad = radiusKm / 111.195
  const lonPad = radiusKm / (111.195 * Math.max(0.05, Math.cos((center.lat * Math.PI) / 180)))
  const down = (value: number) => Math.floor(value / grid + 1e-9) * grid
  const up = (value: number) => Math.ceil(value / grid - 1e-9) * grid
  // Rounded to the backend's five decimals so equal areas are equal strings.
  const fix = (value: number) => Number(value.toFixed(5))
  return {
    south: fix(Math.max(-90, down(center.lat - latPad))),
    west: fix(Math.max(-180, down(center.lon - lonPad))),
    north: fix(Math.min(90, up(center.lat + latPad))),
    east: fix(Math.min(180, up(center.lon + lonPad))),
  }
}

function contains(outer: BoundingBox, inner: BoundingBox): boolean {
  return outer.south <= inner.south && outer.west <= inner.west &&
    outer.north >= inner.north && outer.east >= inner.east
}

/**
 * Areas fetched this session. A truncated area is never reused for a
 * different circle: its missing results could be exactly the ones asked for.
 */
export class AroundAreaCache {
  private areas: AreaResult[] = []

  constructor(private readonly maxAreas = 24) {}

  covering(area: BoundingBox): AreaResult | undefined {
    const key = overpassArea(area)
    return this.areas.find(result => overpassArea(result.area) === key) ??
      this.areas.find(result => !result.truncated && contains(result.area, area))
  }

  put(result: AreaResult): void {
    this.areas = [result, ...this.areas.filter(existing => overpassArea(existing.area) !== overpassArea(result.area))]
      .slice(0, this.maxAreas)
  }
}

export interface AroundSearchResult {
  center: Coordinate
  radiusKm: number
  pois: AroundPoi[]
  /** Overpass hit its result cap; some places may be missing. */
  truncated: boolean
  /** Answered from this session's cache, with no request at all. */
  reused: boolean
  cache?: CacheMetadata
}

export function aroundPoisWithin(elements: OverpassElement[], center: Coordinate, radiusKm: number): AroundPoi[] {
  const limitM = radiusKm * 1000
  const pois: AroundPoi[] = []
  const seen = new Set<string>()
  for (const element of elements) {
    const lat = element.lat ?? element.center?.lat
    const lon = element.lon ?? element.center?.lon
    const category = classifyAround(element.tags)
    if (lat === undefined || lon === undefined || !category || !element.tags) continue
    const id = `${element.type}/${element.id}`
    if (seen.has(id)) continue
    const distanceM = haversineDistance(center, { lat, lon })
    if (distanceM > limitM) continue
    seen.add(id)
    pois.push({
      id,
      category,
      lat,
      lon,
      name: element.tags.name ?? element.tags.brand ?? element.tags.operator,
      kind: kindLabel(element.tags),
      facts: aroundFacts(element.tags, category),
      distanceM,
    })
  }
  return pois.sort((a, b) => a.distanceM - b.distanceM)
}

export async function searchAround(
  center: Coordinate,
  radiusKm: number,
  cache: AroundAreaCache,
  signal?: AbortSignal,
  context: RuntimeRequestContext = {},
): Promise<AroundSearchResult> {
  const radius = Math.min(Math.max(radiusKm, 0.5), AROUND_RADII_KM[AROUND_RADII_KM.length - 1])
  const area = aroundSearchArea(center, radius)
  let result = cache.covering(area)
  const reused = Boolean(result)
  if (!result) {
    const query = `[out:json][timeout:60][bbox:${overpassArea(area)}];(${AROUND_SELECTORS});out center ${AROUND_RESULT_LIMIT};`
    const fetched = await fetchOverpassElements('around', query, area, signal, context)
    result = {
      area,
      elements: fetched.elements,
      truncated: fetched.elements.length >= AROUND_RESULT_LIMIT,
      cache: fetched.cache,
    }
    cache.put(result)
  }
  return {
    center,
    radiusKm: radius,
    pois: aroundPoisWithin(result.elements, center, radius),
    truncated: result.truncated,
    reused,
    cache: result.cache,
  }
}
