import { simplifyCoordinates } from './edit'
import type { Coordinate } from './types'

export type OfflineMode = 'auto' | 'cache-only'

export type RuntimeService =
  | 'fuel'
  | 'places'
  | 'pois'
  | 'valhallaRoute'
  | 'osrmRoute'
  | 'surface'

export type RuntimeRasterMap = 'osm' | 'opentopo' | 'cyclosm'

export interface OfflineCapability {
  enabled: boolean
  mode: OfflineMode
  status: string
  packs: string
}

export interface RuntimeConfig {
  apiBase: string
  nominatimUrl?: string
  offline?: OfflineCapability
  services: Partial<Record<RuntimeService, string>>
  maps: {
    raster: Partial<Record<RuntimeRasterMap, string>>
    openfreemap?: { style: string }
  }
}

export interface CacheMetadata {
  state: 'hit' | 'miss' | 'revalidated' | 'stale' | 'bypass' | string
  cachedAt?: string
  ageSeconds?: number
  stale: boolean
}

export interface RuntimeRequestContext {
  runtime?: RuntimeConfig
  onCacheMetadata?: (metadata: CacheMetadata) => void
  fetcher?: typeof fetch
}

export interface CacheScopeStatus {
  bytes?: number
  entries?: number
  maxBytes?: number
  maxEntries?: number
  durableBytes?: number
  temporaryBytes?: number
}

export interface ProviderStatus {
  healthy?: boolean
  available?: boolean
  failures?: number
  circuitOpen?: boolean
  state?: string
  detail?: string
  lastError?: string
  checkedAt?: string
}

export interface OfflineJob {
  id: string
  name?: string
  status: string
  done?: number
  total?: number
  bytes?: number
  detail?: string
}

export interface OfflineStatus {
  mode: OfflineMode
  writable?: boolean
  bytes?: number
  maxBytes?: number
  entries?: number
  maxEntries?: number
  scopes: Record<string, CacheScopeStatus>
  elevationTiles?: CacheScopeStatus
  providers: Record<string, ProviderStatus>
  jobs: OfflineJob[]
}

export interface PackSummary extends OfflineJob {
  createdAt?: string
  updatedAt?: string
  incomplete?: boolean
  durableBytes?: number
  failed?: number
  resources: Record<string, PackResourceProgress>
}

export interface PackResourceProgress {
  done: number
  total: number
  failed: number
  bytes: number
  items: number
}

export interface PackEstimateRequest {
  name: string
  automatic?: boolean
  route?: { lat: number; lon: number }[]
  bbox?: [number, number, number, number]
  paddingKm: number
  minZoom: number
  maxZoom: number
  layers: string[]
  scopes: string[]
}

export type PackArea = 'route' | 'bbox'

export interface PackRequestDraft {
  name: string
  area: PackArea
  route: { lat: number; lon: number }[]
  bbox: { south: number; west: number; north: number; east: number } | null
  paddingKm: number
  minZoom: number
  maxZoom: number
  layers: string[]
  scopes: string[]
}

export interface PackEstimate {
  resources?: number
  bytes?: number
  reusedBytes?: number
  quotaRemaining?: number
  finalBytes?: number
  counts: Record<string, number>
  blocked: { provider?: string; layer?: string; resource?: string; reason: string }[]
  scopes: Record<string, CacheScopeStatus>
  detail?: string
}

export function limitPackRoute(route: Coordinate[], maxPoints = 5000): { lat: number; lon: number }[] {
  if (route.length <= maxPoints) return route.map(({ lat, lon }) => ({ lat, lon }))
  let lower = 0
  let upper = 10
  let best = simplifyCoordinates(route, upper)
  while (best.length > maxPoints && upper < 20_000_000) {
    lower = upper
    upper *= 2
    best = simplifyCoordinates(route, upper)
  }
  for (let iteration = 0; iteration < 24 && upper - lower > 0.5; iteration++) {
    const tolerance = (lower + upper) / 2
    const candidate = simplifyCoordinates(route, tolerance)
    if (candidate.length > maxPoints) lower = tolerance
    else { upper = tolerance; best = candidate }
  }
  return best.map(({ lat, lon }) => ({ lat, lon }))
}

export function buildAutomaticPackRequest(name: string, route: Coordinate[]): PackEstimateRequest | null {
  if (route.length < 2) return null
  const routeName = name.trim() || 'Loaded route'
  return {
    name: `Route: ${routeName}`.slice(0, 100),
    automatic: true,
    route: limitPackRoute(route),
    paddingKm: 5,
    minZoom: 5,
    maxZoom: 14,
    layers: ['openfreemap'],
    scopes: ['elevation', 'pois', 'fuel'],
  }
}

export function packLayerId(layerId: string): string {
  return layerId === 'topo' ? 'opentopo' : layerId
}

export function validPackArea(preference: PackArea | null, routeAvailable: boolean): PackArea {
  if (!routeAvailable) return 'bbox'
  return preference === 'bbox' ? 'bbox' : 'route'
}

export function syncPackLayers(
  selected: string[],
  activeLayerId: string,
  userEdited: boolean,
): string[] {
  return userEdited ? selected : [packLayerId(activeLayerId)]
}

export function buildPackEstimateRequest(draft: PackRequestDraft): PackEstimateRequest | null {
  const name = draft.name.trim()
  if (!name) return null
  const request: PackEstimateRequest = {
    name,
    paddingKm: draft.paddingKm,
    minZoom: Math.min(draft.minZoom, draft.maxZoom),
    maxZoom: Math.max(draft.minZoom, draft.maxZoom),
    layers: [...draft.layers],
    scopes: [...draft.scopes],
  }
  if (draft.area === 'route') {
    if (draft.route.length < 2) return null
    request.route = draft.route.map(({ lat, lon }) => ({ lat, lon }))
  } else {
    if (!draft.bbox) return null
    request.bbox = [draft.bbox.south, draft.bbox.west, draft.bbox.north, draft.bbox.east]
  }
  return request
}

export function packRequestSignature(request: PackEstimateRequest | null): string | null {
  if (!request) return null
  return JSON.stringify({
    ...request,
    layers: [...request.layers].sort(),
    scopes: [...request.scopes].sort(),
  })
}

type UnknownRecord = Record<string, unknown>

const SERVICE_KEYS: RuntimeService[] = [
  'fuel',
  'places',
  'pois',
  'valhallaRoute',
  'osrmRoute',
  'surface',
]
const RASTER_KEYS: RuntimeRasterMap[] = ['osm', 'opentopo', 'cyclosm']

function record(value: unknown): UnknownRecord | null {
  return value !== null && typeof value === 'object' && !Array.isArray(value)
    ? value as UnknownRecord
    : null
}

function text(value: unknown): string | undefined {
  return typeof value === 'string' && value.trim() ? value.trim() : undefined
}

function number(value: unknown): number | undefined {
  return typeof value === 'number' && Number.isFinite(value) && value >= 0 ? value : undefined
}

function boolean(value: unknown): boolean | undefined {
  return typeof value === 'boolean' ? value : undefined
}

/** Resolve an advertised application route without consuming map URL templates. */
export function resolveApiUrl(endpoint: string, apiBase = ''): string {
  if (/^[a-z][a-z\d+.-]*:\/\//i.test(endpoint)) return endpoint
  const base = apiBase.replace(/\/+$/, '')
  const path = endpoint.startsWith('/') ? endpoint : `/${endpoint}`
  return base ? `${base}${path}` : path
}

/** Decode only the public capabilities the browser understands. */
export function decodeRuntimeConfig(value: unknown, apiBase = ''): RuntimeConfig {
  const root = record(value)
  const servicesRaw = record(root?.services)
  const services: Partial<Record<RuntimeService, string>> = {}
  for (const key of SERVICE_KEYS) {
    const endpoint = text(servicesRaw?.[key])
    if (endpoint) services[key] = resolveApiUrl(endpoint, apiBase)
  }

  const mapsRaw = record(root?.maps)
  const rasterRaw = record(mapsRaw?.raster)
  const raster: Partial<Record<RuntimeRasterMap, string>> = {}
  for (const key of RASTER_KEYS) {
    const endpoint = text(rasterRaw?.[key])
    if (endpoint) raster[key] = resolveApiUrl(endpoint, apiBase)
  }

  const openfreemapRaw = record(mapsRaw?.openfreemap)
  const style = text(openfreemapRaw?.style)
  const offlineRaw = record(root?.offline)
  const enabled = offlineRaw?.enabled === true
  const mode: OfflineMode = offlineRaw?.mode === 'cache-only' ? 'cache-only' : 'auto'
  const status = text(offlineRaw?.status)
  const packs = text(offlineRaw?.packs)

  return {
    apiBase,
    nominatimUrl: text(root?.nominatimUrl),
    offline: enabled
      ? {
          enabled: true,
          mode,
          status: resolveApiUrl(status ?? '/offline/status', apiBase),
          packs: resolveApiUrl(packs ?? '/offline/packs', apiBase),
        }
      : undefined,
    services,
    maps: {
      raster,
      openfreemap: style ? { style: resolveApiUrl(style, apiBase) } : undefined,
    },
  }
}

/** A missing or unreachable config endpoint intentionally means standalone mode. */
export async function loadRuntimeConfig(
  apiBase = '',
  signal?: AbortSignal,
  fetcher: typeof fetch = fetch,
): Promise<RuntimeConfig> {
  try {
    const response = await fetcher(resolveApiUrl('/config', apiBase), { signal })
    if (!response.ok) return decodeRuntimeConfig(null, apiBase)
    return decodeRuntimeConfig(await response.json(), apiBase)
  } catch (error) {
    if ((error as Error)?.name === 'AbortError') throw error
    return decodeRuntimeConfig(null, apiBase)
  }
}

export type RuntimeTransport =
  | { kind: 'backend' | 'direct'; url: string }
  | { kind: 'unavailable'; url: null }

/** Pure policy decision used by every provider adapter. */
export function selectRuntimeTransport(
  runtime: RuntimeConfig | undefined,
  service: RuntimeService,
  directUrl: string,
): RuntimeTransport {
  const backend = runtime?.services[service]
  if (backend) return { kind: 'backend', url: backend }
  if (runtime?.offline?.mode === 'cache-only') return { kind: 'unavailable', url: null }
  return { kind: 'direct', url: directUrl }
}

export function parseCacheMetadata(headers: Headers): CacheMetadata | undefined {
  const state = headers.get('X-GPX-Cache')?.trim().toLowerCase()
  if (!state) return undefined
  const rawCachedAt = headers.get('X-GPX-Cached-At')?.trim()
  const cachedDate = rawCachedAt ? new Date(rawCachedAt) : null
  const rawAge = headers.get('Age')
  const parsedAge = rawAge === null ? NaN : Number(rawAge)
  return {
    state,
    cachedAt: cachedDate && Number.isFinite(cachedDate.getTime())
      ? cachedDate.toISOString()
      : rawCachedAt || undefined,
    ageSeconds: Number.isFinite(parsedAge) && parsedAge >= 0 ? parsedAge : undefined,
    stale: state === 'stale',
  }
}

export function emitCacheMetadata(
  response: Response,
  callback?: (metadata: CacheMetadata) => void,
): CacheMetadata | undefined {
  const metadata = parseCacheMetadata(response.headers)
  if (metadata) callback?.(metadata)
  return metadata
}

export class OfflineCacheMissError extends Error {
  readonly scope?: string

  constructor(message = 'This resource is not available in the offline cache', scope?: string) {
    super(message)
    this.name = 'OfflineCacheMissError'
    this.scope = scope
  }
}

export async function responseError(response: Response, fallback: string): Promise<Error> {
  const body = await response.clone().json().catch(() => null) as unknown
  const detail = text(record(body)?.detail)
  const code = text(record(body)?.code)
  const scope = text(record(body)?.scope)
  if (code === 'offline_cache_miss') return new OfflineCacheMissError(detail, scope)
  return new Error(detail ?? fallback)
}

export interface RuntimeFetchOptions extends RuntimeRequestContext {
  service: RuntimeService
  directUrl: string
  /** Advertised endpoint with operation-specific query parameters appended. */
  backendUrl?: string
  backendInit?: RequestInit
  directInit?: RequestInit
  signal?: AbortSignal
  fetcher?: typeof fetch
}

/**
 * Select once, then make exactly one request. Backend errors and aborts are
 * returned to the caller and can never bypass cache-only or server rate policy.
 */
export async function fetchRuntimeService(options: RuntimeFetchOptions): Promise<{
  response: Response
  transport: Exclude<RuntimeTransport, { kind: 'unavailable' }>
  cache?: CacheMetadata
}> {
  const transport = selectRuntimeTransport(options.runtime, options.service, options.directUrl)
  if (transport.kind === 'unavailable') throw new OfflineCacheMissError()
  const init = transport.kind === 'backend' ? options.backendInit : options.directInit
  const url = transport.kind === 'backend' && options.backendUrl ? options.backendUrl : transport.url
  const response = await (options.fetcher ?? fetch)(url, { ...init, signal: options.signal })
  const cache = emitCacheMetadata(response, options.onCacheMetadata)
  return { response, transport, cache }
}

function decodeScope(value: unknown): CacheScopeStatus | undefined {
  const item = record(value)
  if (!item) return undefined
  return {
    bytes: number(item.bytes),
    entries: number(item.entries) ?? number(item.files) ?? number(item.tiles),
    maxBytes: number(item.maxBytes),
    maxEntries: number(item.maxEntries),
    durableBytes: number(item.durableBytes),
    temporaryBytes: number(item.temporaryBytes),
  }
}

function decodeScopes(value: unknown): Record<string, CacheScopeStatus> {
  const raw = record(value)
  const result: Record<string, CacheScopeStatus> = {}
  if (!raw) return result
  for (const [key, entry] of Object.entries(raw)) {
    const decoded = decodeScope(entry)
    if (decoded) result[key] = decoded
  }
  return result
}

function decodePackResources(value: unknown): Record<string, PackResourceProgress> {
  const raw = record(value)
  const resources: Record<string, PackResourceProgress> = {}
  for (const [category, entry] of Object.entries(raw ?? {})) {
    const progress = record(entry)
    if (!progress) continue
    resources[category] = {
      done: number(progress.done) ?? 0,
      total: number(progress.total) ?? 0,
      failed: number(progress.failed) ?? number(progress.failures) ?? 0,
      bytes: number(progress.bytes) ?? 0,
      items: number(progress.items) ?? 0,
    }
  }
  return resources
}

function decodeProvider(value: unknown): ProviderStatus | undefined {
  const item = record(value)
  if (!item) return undefined
  const circuitOpen = boolean(item.circuitOpen)
  const failures = number(item.failures)
  return {
    healthy: boolean(item.healthy) ?? (circuitOpen === undefined ? undefined : !circuitOpen),
    available: boolean(item.available) ?? (circuitOpen === undefined ? undefined : !circuitOpen),
    failures,
    circuitOpen,
    state: text(item.state) ?? text(item.status) ??
      (circuitOpen ? 'circuit open' : failures && failures > 0 ? 'degraded' : undefined),
    detail: text(item.detail),
    lastError: text(item.lastError),
    checkedAt: text(item.checkedAt),
  }
}

function decodeJob(value: unknown): OfflineJob | undefined {
  const item = record(value)
  const id = text(item?.id)
  if (!item || !id) return undefined
  const progress = record(item.progress)
  return {
    id,
    name: text(item.name),
    status: text(item.status) ?? text(item.state) ?? 'unknown',
    done: number(item.done) ?? number(item.completed) ?? number(progress?.done) ?? number(progress?.completed),
    total: number(item.total) ?? number(progress?.total),
    bytes: number(item.bytes) ?? number(progress?.bytes),
    detail: text(item.detail) ?? text(item.error),
  }
}

export function decodeOfflineStatus(value: unknown): OfflineStatus {
  const root = record(value)
  const providersRaw = record(root?.providers)
  const providers: Record<string, ProviderStatus> = {}
  for (const [key, entry] of Object.entries(providersRaw ?? {})) {
    const decoded = decodeProvider(entry)
    if (decoded) providers[key] = decoded
  }
  return {
    mode: root?.mode === 'cache-only' ? 'cache-only' : 'auto',
    writable: boolean(root?.writable),
    bytes: number(root?.bytes),
    maxBytes: number(root?.maxBytes),
    entries: number(root?.entries),
    maxEntries: number(root?.maxEntries),
    scopes: decodeScopes(root?.scopes),
    elevationTiles: decodeScope(root?.elevationTiles),
    providers,
    jobs: Array.isArray(root?.jobs)
      ? root.jobs.map(decodeJob).filter((job): job is OfflineJob => Boolean(job))
      : [],
  }
}

export function decodePacks(value: unknown): PackSummary[] {
  const root = record(value)
  const list = Array.isArray(value) ? value : Array.isArray(root?.packs) ? root.packs : []
  return list.flatMap(entry => {
    const job = decodeJob(entry)
    const item = record(entry)
    if (!job || !item) return []
    return [{
      ...job,
      createdAt: text(item.createdAt),
      updatedAt: text(item.updatedAt),
      incomplete: boolean(item.incomplete) ?? job.status === 'incomplete',
      durableBytes: number(item.durableBytes),
      failed: number(item.failed),
      resources: decodePackResources(item.resources),
    }]
  })
}

export function decodePackEstimate(value: unknown): PackEstimate {
  const root = record(value)
  const quota = record(root?.quota)
  const blockedRaw = Array.isArray(root?.blocked) ? root.blocked : []
  const countsRaw = record(root?.counts)
  const counts: Record<string, number> = {}
  for (const [category, value] of Object.entries(countsRaw ?? {})) {
    const count = number(value)
    if (count !== undefined) counts[category] = count
  }
  return {
    resources: number(root?.resources) ?? number(root?.resourceCount) ?? number(root?.estimatedResources),
    bytes: number(root?.bytes) ?? number(root?.estimatedBytes),
    reusedBytes: number(root?.reusedBytes) ?? number(root?.reuseBytes),
    quotaRemaining: number(root?.quotaRemaining) ?? number(root?.remainingQuota) ?? number(quota?.remaining),
    finalBytes: number(root?.finalBytes) ?? number(root?.expectedFinalBytes),
    counts,
    blocked: blockedRaw.flatMap(value => {
      const item = record(value)
      const reason = text(item?.reason)
      return reason ? [{ provider: text(item?.provider), layer: text(item?.layer), resource: text(item?.resource), reason }] : []
    }),
    scopes: decodeScopes(root?.scopes),
    detail: text(root?.detail),
  }
}

export function formatBytes(value: number | undefined): string {
  if (value === undefined || !Number.isFinite(value)) return 'Unknown'
  const units = ['B', 'KiB', 'MiB', 'GiB', 'TiB']
  let amount = Math.max(0, value)
  let unit = 0
  while (amount >= 1024 && unit < units.length - 1) { amount /= 1024; unit++ }
  const digits = unit === 0 || amount >= 100 ? 0 : amount >= 10 ? 1 : 2
  return `${amount.toFixed(digits)} ${units[unit]}`
}

export function formatCacheDate(value: string | undefined): string | undefined {
  if (!value) return undefined
  const date = new Date(value)
  return Number.isFinite(date.getTime()) ? date.toLocaleString() : value
}

export function formatCacheContext(metadata: CacheMetadata, compact = false): string {
  const state = metadata.stale ? 'stale' : metadata.state.toLowerCase()
  const label = compact
    ? {
        hit: 'cached',
        stale: 'stale',
        miss: 'fresh',
        revalidated: 'refreshed',
        bypass: 'live',
      }[state] ?? state
    : {
        hit: 'Cached',
        stale: 'Stale cache',
        miss: 'Fresh response',
        revalidated: 'Cache revalidated',
        bypass: 'Live response',
      }[state] ?? `Cache ${state}`
  if (compact) return label

  const cachedAt = formatCacheDate(metadata.cachedAt)
  if (cachedAt) return `${label} from ${cachedAt}`
  if (metadata.ageSeconds !== undefined) return `${label}, ${Math.round(metadata.ageSeconds)}s old`
  return label
}

const managementHeaders = { 'Content-Type': 'application/json', 'X-GPX-Editor': '1' }

async function managementResponse(response: Response, action: string): Promise<Response> {
  if (!response.ok) throw await responseError(response, `${action} failed (${response.status})`)
  return response
}

export async function fetchOfflineStatus(
  runtime: RuntimeConfig,
  signal?: AbortSignal,
): Promise<OfflineStatus> {
  if (!runtime.offline) throw new Error('Offline cache is unavailable')
  const response = await fetch(runtime.offline.status, { signal })
  await managementResponse(response, 'Storage status')
  return decodeOfflineStatus(await response.json())
}

export async function fetchPacks(runtime: RuntimeConfig, signal?: AbortSignal): Promise<PackSummary[]> {
  if (!runtime.offline) return []
  const response = await fetch(runtime.offline.packs, { signal })
  await managementResponse(response, 'Trip packs')
  return decodePacks(await response.json())
}

export async function estimatePack(
  runtime: RuntimeConfig,
  request: PackEstimateRequest,
  signal?: AbortSignal,
): Promise<PackEstimate> {
  if (!runtime.offline) throw new Error('Offline cache is unavailable')
  const response = await fetch(`${runtime.offline.packs}/estimate`, {
    method: 'POST',
    headers: managementHeaders,
    body: JSON.stringify(request),
    signal,
  })
  await managementResponse(response, 'Pack estimate')
  return decodePackEstimate(await response.json())
}

export async function startPack(
  runtime: RuntimeConfig,
  request: PackEstimateRequest,
  signal?: AbortSignal,
): Promise<PackSummary | undefined> {
  if (!runtime.offline) throw new Error('Offline cache is unavailable')
  const response = await fetch(runtime.offline.packs, {
    method: 'POST',
    headers: managementHeaders,
    body: JSON.stringify(request),
    signal,
  })
  await managementResponse(response, 'Start pack')
  const body = await response.json().catch(() => null)
  return decodePacks(Array.isArray(body) ? body : [body])[0]
}

export async function cancelPack(runtime: RuntimeConfig, id: string): Promise<void> {
  if (!runtime.offline) return
  const response = await fetch(`${runtime.offline.packs}/${encodeURIComponent(id)}/cancel`, {
    method: 'POST',
    headers: { 'X-GPX-Editor': '1' },
  })
  await managementResponse(response, 'Cancel pack')
}

export async function deletePack(runtime: RuntimeConfig, id: string): Promise<void> {
  if (!runtime.offline) return
  const response = await fetch(`${runtime.offline.packs}/${encodeURIComponent(id)}`, {
    method: 'DELETE',
    headers: { 'X-GPX-Editor': '1' },
  })
  await managementResponse(response, 'Delete pack')
}

export async function clearRuntimeCache(runtime: RuntimeConfig, scope: string): Promise<void> {
  if (!runtime.offline) return
  const url = resolveApiUrl(`/offline/cache?scope=${encodeURIComponent(scope)}`, runtime.apiBase)
  const response = await fetch(url, {
    method: 'DELETE',
    headers: { 'X-GPX-Editor': '1' },
  })
  await managementResponse(response, 'Clear cache')
}
