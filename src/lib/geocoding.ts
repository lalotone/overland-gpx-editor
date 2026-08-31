import { createRateLimitedFetch } from './rateLimit'
import { fetchRuntimeService, responseError } from './offline'
import type { CacheMetadata, RuntimeRequestContext } from './offline'

export const DEFAULT_NOMINATIM_API = 'https://nominatim.openstreetmap.org'
export const NOMINATIM_REQUEST_INTERVAL_MS = 1000

export interface PlaceResult {
  place_id: number
  display_name: string
  lat: string
  lon: string
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

  const base = apiBase.replace(/\/+$/, '')
  const cacheKey = `${base}:${normalized.toLowerCase()}`
  const cached = resultCache.get(cacheKey)
  if (cached) {
    if (cached.cache) context.onCacheMetadata?.(cached.cache)
    return cached.results
  }

  const language = 'en'
  const directParams = new URLSearchParams({ q: normalized, format: 'jsonv2', limit: '5' })
  const backendParams = new URLSearchParams({ q: normalized, language })
  const backendEndpoint = context.runtime?.services.places
  const { response: res, cache } = await fetchRuntimeService({
    ...context,
    service: 'places',
    directUrl: `${base}/search?${directParams}`,
    backendUrl: backendEndpoint ? `${backendEndpoint}?${backendParams}` : undefined,
    backendInit: { headers: { 'Accept-Language': language } },
    directInit: { headers: { 'Accept-Language': language } },
    signal,
    fetcher: backendEndpoint ? fetch : nominatimFetch,
  })
  if (!res.ok) {
    if (res.status === 429) throw new Error('Place search is busy right now - try again in a moment')
    throw await responseError(res, `Place search returned ${res.status}`)
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
  resultCache.set(cacheKey, { results, cache })
  return results
}
