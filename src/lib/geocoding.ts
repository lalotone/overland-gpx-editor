import { createRateLimitedFetch } from './rateLimit'
import { fetchRuntimeService, responseError, selectRuntimeTransport } from './offline'
import type { CacheMetadata, RuntimeRequestContext } from './offline'

export const DEFAULT_NOMINATIM_API = 'https://nominatim.openstreetmap.org'
export const NOMINATIM_REQUEST_INTERVAL_MS = 1000

export interface PlaceResult {
  place_id: number
  display_name: string
  lat: number
  lon: number
  bounds?: { south: number; west: number; north: number; east: number }
  category?: string
  type?: string
}

const nominatimFetch = createRateLimitedFetch(NOMINATIM_REQUEST_INTERVAL_MS)
const resultCache = new Map<string, { results: PlaceResult[]; cache?: CacheMetadata }>()
const MAX_CACHED_SEARCHES = 100

/** User-triggered place search with the public service's required rate and cache. */
export async function searchPlaces(
  query: string,
  signal?: AbortSignal,
  apiBase = DEFAULT_NOMINATIM_API,
  context: RuntimeRequestContext = {},
): Promise<PlaceResult[]> {
  const normalized = query.trim().replace(/\s+/g, ' ')
  if (!normalized) return []
  if (new TextEncoder().encode(normalized).length > 200) {
    throw new Error('Place search must not exceed 200 bytes')
  }

  const base = apiBase.replace(/\/+$/, '')
  const language = 'en'
  const directParams = new URLSearchParams({ q: normalized, format: 'jsonv2', limit: '5' })
  const directUrl = `${base}/search?${directParams}`
  const transport = selectRuntimeTransport(context.runtime, 'places', directUrl)
  const cacheKey = `${transport.kind}:${transport.url ?? 'unavailable'}:${language}:${normalized.toLowerCase()}`
  const cached = resultCache.get(cacheKey)
  if (cached) {
    if (cached.cache) context.onCacheMetadata?.(cached.cache)
    return cached.results
  }

  const backendParams = new URLSearchParams({ q: normalized, language })
  const backendEndpoint = context.runtime?.services.places
  const { response: res, cache } = await fetchRuntimeService({
    ...context,
    service: 'places',
    directUrl,
    backendUrl: backendEndpoint ? `${backendEndpoint}?${backendParams}` : undefined,
    backendInit: { headers: { 'Accept-Language': language } },
    directInit: { headers: { 'Accept-Language': language } },
    signal,
    fetcher: context.fetcher ?? (backendEndpoint ? fetch : nominatimFetch),
  })
  if (!res.ok) {
    if (res.status === 429) throw new Error('Place search is busy right now - try again in a moment')
    throw await responseError(res, `Place search returned ${res.status}`)
  }

  const data = await res.json() as unknown
  const results = Array.isArray(data)
    ? data.flatMap(parsePlaceResult)
    : []

  if (resultCache.size >= MAX_CACHED_SEARCHES) {
    const oldest = resultCache.keys().next().value
    if (oldest !== undefined) resultCache.delete(oldest)
  }
  resultCache.set(cacheKey, { results, cache })
  return results
}

function coordinate(value: unknown, min: number, max: number): number | undefined {
  if (typeof value !== 'string' && typeof value !== 'number') return undefined
  if (typeof value === 'string' && !value.trim()) return undefined
  const parsed = Number(value)
  return Number.isFinite(parsed) && parsed >= min && parsed <= max ? parsed : undefined
}

function parsePlaceResult(value: unknown): PlaceResult[] {
  if (value === null || typeof value !== 'object' || Array.isArray(value)) return []
  const raw = value as Record<string, unknown>
  const lat = coordinate(raw.lat, -90, 90)
  const lon = coordinate(raw.lon, -180, 180)
  if (typeof raw.place_id !== 'number' || raw.place_id <= 0 ||
      typeof raw.display_name !== 'string' || !raw.display_name.trim() ||
      lat === undefined || lon === undefined) return []

  let bounds: PlaceResult['bounds']
  if (Array.isArray(raw.boundingbox) && raw.boundingbox.length === 4) {
    const south = coordinate(raw.boundingbox[0], -90, 90)
    const north = coordinate(raw.boundingbox[1], -90, 90)
    const west = coordinate(raw.boundingbox[2], -180, 180)
    const east = coordinate(raw.boundingbox[3], -180, 180)
    if (south !== undefined && north !== undefined && west !== undefined && east !== undefined && south < north && west !== east) {
      bounds = { south, west, north, east }
    }
  }

  return [{
    place_id: raw.place_id,
    display_name: raw.display_name.trim(),
    lat,
    lon,
    bounds,
    category: typeof raw.category === 'string' ? raw.category : typeof raw.class === 'string' ? raw.class : undefined,
    type: typeof raw.type === 'string' ? raw.type : undefined,
  }]
}
