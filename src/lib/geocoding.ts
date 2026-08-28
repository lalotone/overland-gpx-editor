import { createRateLimitedFetch } from './rateLimit'

export const DEFAULT_NOMINATIM_API = 'https://nominatim.openstreetmap.org'
export const NOMINATIM_REQUEST_INTERVAL_MS = 1000

export interface PlaceResult {
  place_id: number
  display_name: string
  lat: string
  lon: string
}

const nominatimFetch = createRateLimitedFetch(NOMINATIM_REQUEST_INTERVAL_MS)
const resultCache = new Map<string, PlaceResult[]>()
const MAX_CACHED_SEARCHES = 100

/** User-triggered place search with the public service's required rate and cache. */
export async function searchPlaces(
  query: string,
  signal?: AbortSignal,
  apiBase = DEFAULT_NOMINATIM_API,
): Promise<PlaceResult[]> {
  const normalized = query.trim().replace(/\s+/g, ' ')
  if (!normalized) return []

  const base = apiBase.replace(/\/+$/, '')
  const cacheKey = `${base}:${normalized.toLowerCase()}`
  const cached = resultCache.get(cacheKey)
  if (cached) return cached

  const params = new URLSearchParams({ q: normalized, format: 'jsonv2', limit: '5' })
  const res = await nominatimFetch(`${base}/search?${params}`, {
    headers: { 'Accept-Language': 'en' },
    signal,
  })
  if (!res.ok) {
    throw new Error(
      res.status === 429
        ? 'Place search is busy right now - try again in a moment'
        : `Place search returned ${res.status}`,
    )
  }

  const data = (await res.json()) as Partial<PlaceResult>[]
  const results = Array.isArray(data)
    ? data.filter((result): result is PlaceResult =>
        typeof result.place_id === 'number' &&
        typeof result.display_name === 'string' &&
        typeof result.lat === 'string' &&
        typeof result.lon === 'string',
      )
    : []

  if (resultCache.size >= MAX_CACHED_SEARCHES) {
    const oldest = resultCache.keys().next().value
    if (oldest !== undefined) resultCache.delete(oldest)
  }
  resultCache.set(cacheKey, results)
  return results
}
