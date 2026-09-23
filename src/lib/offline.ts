import { simplifyCoordinates } from './edit'
import type { Coordinate } from './types'

export type OfflineMode = 'auto' | 'cache-only'

export type RuntimeService =
  | 'fuel'
  | 'places'
  | 'pois'
  | 'broomRoute'

export type RuntimeRasterMap = 'osm' | 'opentopo' | 'cyclosm'

export interface OfflineCapability {
  enabled: boolean
  mode: OfflineMode
  status: string
  packs: string
  modeControl?: string
  routing?: string
}

export interface RuntimeConfig {
  apiBase: string
  nominatimUrl?: string
  offline?: OfflineCapability
  services: Partial<Record<RuntimeService, string>>
  maps: {
    raster: Partial<Record<RuntimeRasterMap, string>>
    openfreemap?: { style: string; allowBulk: boolean }
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

export interface RoutingDataJob {
  upgrading?: boolean
  id: string
  regionId: string
  state: 'queued' | 'running' | 'complete' | 'failed' | 'cancelled' | string
  phase?: string
  item?: string
  done?: number
  total?: number
  detail?: string
  completedItems?: number
  itemsTotal?: number
  itemsDownloaded?: number
  itemsReused?: number
  stage?: string
  elapsedSeconds?: number
  retrySeconds?: number
  attempt?: number
  retrying?: boolean
}

export interface RoutingDataRegion {
  regionId: string
  name: string
  generationId: string
  selected: boolean
  pinned: boolean
  installedAt?: string
}

export interface RoutingDataStatus {
  /** Byte counters are a verified snapshot, not recalculated by progress polls. */
  inventoryUpdatedAt?: string
  summaryBytes?: number
  summaryKnown?: boolean
  summaryStale?: boolean
  summaryUpdatedAt?: string
  upgradePending?: boolean
  enabled: boolean
  ready: boolean
  regionId?: string
  generationId?: string
  name?: string
  error?: string
  job?: RoutingDataJob
  cached: RoutingDataRegion[]
  cacheBytes: number
  pinnedBytes: number
  inUseBytes: number
  reclaimableBytes: number
}

export interface PackSummary extends OfflineJob {
  coverageKind?: 'area' | 'city' | 'region' | 'country'
  unavailable?: { resource?: string; layer?: string; reason: string }[]
  batchesDone?: number
  batchesTotal?: number
  bbox?: PackBounds
  createdAt?: string
  updatedAt?: string
  incomplete?: boolean
  durableBytes?: number
  failed?: number
  resources: Record<string, PackResourceProgress>
}

export interface PackResourceProgress {
  reused?: number
  downloaded?: number
  revalidated?: number
  done: number
  total: number
  failed: number
  bytes: number
  items: number
}

export interface PackEstimateRequest {
  coverageKind?: 'area' | 'city' | 'region' | 'country'
  regional?: boolean
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

export interface PackBounds {
  south: number
  west: number
  north: number
  east: number
}

export interface PackRequestDraft {
  name: string
  area: PackArea
  route: { lat: number; lon: number }[]
  bbox: PackBounds | null
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

const WEB_MERCATOR_LATITUDE = 85.05112878
const PACK_LAYERS = new Set(['openfreemap', 'osm', 'opentopo', 'cyclosm', 'satellite', 'relief', 'hillshade'])
const PACK_SCOPES = new Set(['elevation', 'pois', 'fuel', 'places'])

function normalizedLongitude(value: number): number {
  const normalized = ((value + 180) % 360 + 360) % 360 - 180
  return Object.is(normalized, -0) ? 0 : normalized
}

/** Convert two unwrapped map corners to the backend's antimeridian-aware bbox. */
export function normalizePackBounds(
  first: { lat: number; lon: number },
  second: { lat: number; lon: number },
): PackBounds | null {
  if (![first.lat, first.lon, second.lat, second.lon].every(Number.isFinite)) return null
  const south = Math.max(-WEB_MERCATOR_LATITUDE, Math.min(first.lat, second.lat))
  const north = Math.min(WEB_MERCATOR_LATITUDE, Math.max(first.lat, second.lat))
  const rawWest = Math.min(first.lon, second.lon)
  const rawEast = Math.max(first.lon, second.lon)
  const longitudeSpan = rawEast - rawWest
  if (south >= north || longitudeSpan <= 0 || longitudeSpan >= 360) return null
  return { south, west: normalizedLongitude(rawWest), north, east: normalizedLongitude(rawEast) }
}

function validPackBounds(bounds: PackBounds): boolean {
  return [bounds.south, bounds.west, bounds.north, bounds.east].every(Number.isFinite) &&
    bounds.south >= -WEB_MERCATOR_LATITUDE && bounds.north <= WEB_MERCATOR_LATITUDE &&
    bounds.south < bounds.north && bounds.west >= -180 && bounds.west <= 180 &&
    bounds.east >= -180 && bounds.east <= 180 && bounds.west !== bounds.east
}

export function buildPackEstimateRequest(draft: PackRequestDraft): PackEstimateRequest | null {
  const name = draft.name.trim()
  if (!name || name.length > 100 || !Number.isFinite(draft.paddingKm) || draft.paddingKm < 0 || draft.paddingKm > 100) return null
  if (!Number.isInteger(draft.minZoom) || !Number.isInteger(draft.maxZoom) ||
      draft.minZoom < 0 || draft.minZoom > 19 || draft.maxZoom < 0 || draft.maxZoom > 19) return null
  if (draft.layers.some(layer => !PACK_LAYERS.has(layer)) || draft.scopes.some(scope => !PACK_SCOPES.has(scope))) return null
  const request: PackEstimateRequest = {
    name,
    paddingKm: draft.paddingKm,
    minZoom: Math.min(draft.minZoom, draft.maxZoom),
    maxZoom: Math.max(draft.minZoom, draft.maxZoom),
    layers: [...new Set(draft.layers)].sort(),
    scopes: [...new Set(draft.scopes)].sort(),
  }
  if (draft.area === 'route') {
    if (draft.route.length < 2) return null
    if (draft.route.some(({ lat, lon }) => !Number.isFinite(lat) || !Number.isFinite(lon) || lat < -90 || lat > 90 || lon < -180 || lon > 180)) return null
    request.route = limitPackRoute(draft.route)
  } else {
    if (!draft.bbox || !validPackBounds(draft.bbox)) return null
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
  'broomRoute',
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

function decodePackBounds(value: unknown): PackBounds | undefined {
  const item = record(value)
  if (!item) return undefined
  const bounds = {
    south: item.south,
    west: item.west,
    north: item.north,
    east: item.east,
  }
  if (!Object.values(bounds).every(coordinate => typeof coordinate === 'number' && Number.isFinite(coordinate))) {
    return undefined
  }
  return validPackBounds(bounds as PackBounds) ? bounds as PackBounds : undefined
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
  const allowBulk = boolean(openfreemapRaw?.allowBulk) ?? false
  const offlineRaw = record(root?.offline)
  const enabled = offlineRaw?.enabled === true
  const mode: OfflineMode = offlineRaw?.mode === 'cache-only' ? 'cache-only' : 'auto'
  const status = text(offlineRaw?.status)
  const packs = text(offlineRaw?.packs)
  const modeControl = text(offlineRaw?.modeControl)
  const routing = text(offlineRaw?.routing)

  return {
    apiBase,
    nominatimUrl: text(root?.nominatimUrl),
    offline: enabled
      ? {
          enabled: true,
          mode,
          status: resolveApiUrl(status ?? '/offline/status', apiBase),
          packs: resolveApiUrl(packs ?? '/offline/packs', apiBase),
          modeControl: modeControl ? resolveApiUrl(modeControl, apiBase) : undefined,
          routing: routing ? resolveApiUrl(routing, apiBase) : undefined,
        }
      : undefined,
    services,
    maps: {
      raster,
      openfreemap: style ? { style: resolveApiUrl(style, apiBase), allowBulk } : undefined,
    },
  }
}

export function bootstrapRuntimeConfig(apiBase = '', embeddedMode?: string): RuntimeConfig {
  if (embeddedMode === 'auto' || embeddedMode === 'cache-only') {
    return decodeRuntimeConfig({ offline: { enabled: true, mode: embeddedMode } }, apiBase)
  }
  // A configured remote backend is authoritative. Until its config arrives,
  // fail closed rather than leaking requests through standalone fallbacks.
  if (apiBase.trim()) {
    return decodeRuntimeConfig({ offline: { enabled: true, mode: 'cache-only' } }, apiBase)
  }
  return decodeRuntimeConfig(null, apiBase)
}

/** A missing config means standalone mode unless server-rendered policy supplied a fallback. */
export async function loadRuntimeConfig(
  apiBase = '',
  signal?: AbortSignal,
  fetcher: typeof fetch = fetch,
  fallback?: RuntimeConfig,
): Promise<RuntimeConfig> {
  try {
    const response = await fetcher(resolveApiUrl('/config', apiBase), { signal })
    if (!response.ok) return fallback ?? decodeRuntimeConfig(null, apiBase)
    return decodeRuntimeConfig(await response.json(), apiBase)
  } catch (error) {
    if ((error as Error)?.name === 'AbortError') throw error
    return fallback ?? decodeRuntimeConfig(null, apiBase)
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

function decodeRoutingDataStatus(value: unknown): RoutingDataStatus {
  const root = record(value)
  const job = record(root?.job)
  const cached = Array.isArray(root?.cached) ? root.cached.flatMap(value => {
    const region = record(value)
    const regionId = text(region?.regionId)
    const generationId = text(region?.generationId)
    if (!regionId || !generationId) return []
    return [{
      regionId,
      generationId,
      name: text(region?.name) ?? regionId,
      selected: region?.selected === true,
      pinned: region?.pinned === true,
      installedAt: text(region?.installedAt),
    }]
  }) : []
  const jobId = text(job?.id)
  const jobRegion = text(job?.regionId)
  return {
    enabled: root?.enabled === true,
    ready: root?.ready === true,
    inventoryUpdatedAt: text(root?.inventoryUpdatedAt),
    summaryBytes: number(root?.summaryBytes),
    summaryKnown: root?.summaryKnown === true,
    summaryStale: root?.summaryStale === true,
    summaryUpdatedAt: text(root?.summaryUpdatedAt),
    upgradePending: root?.upgradePending === true,
    regionId: text(root?.regionId),
    generationId: text(root?.generationId),
    name: text(root?.name),
    error: text(root?.error),
    job: jobId && jobRegion ? {
      id: jobId,
      regionId: jobRegion,
      upgrading: job?.upgrading === true,
      state: text(job?.state) ?? 'unknown',
      phase: text(job?.phase),
      item: text(job?.item),
      done: number(job?.done),
      total: number(job?.total),
      detail: text(job?.detail),
      completedItems: number(job?.completedItems),
      itemsTotal: number(job?.itemsTotal),
      itemsDownloaded: number(job?.itemsDownloaded),
      itemsReused: number(job?.itemsReused),
      stage: text(job?.stage),
      elapsedSeconds: number(job?.elapsedSeconds),
      retrySeconds: number(job?.retrySeconds),
      attempt: number(job?.attempt),
      retrying: job?.retrying === true,
    } : undefined,
    cached,
    cacheBytes: number(root?.cacheBytes) ?? 0,
    pinnedBytes: number(root?.pinnedBytes) ?? 0,
    inUseBytes: number(root?.inUseBytes) ?? 0,
    reclaimableBytes: number(root?.reclaimableBytes) ?? 0,
  }
}

export async function fetchRoutingDataStatus(runtime: RuntimeConfig, signal?: AbortSignal): Promise<RoutingDataStatus | null> {
  if (!runtime.offline?.routing) return null
  const response = await fetch(runtime.offline.routing, { signal })
  if (!response.ok) throw await responseError(response, `Routing data status returned ${response.status}`)
  return decodeRoutingDataStatus(await response.json())
}

/** Explicit storage inspection; never call this from a progress polling loop. */
export async function fetchRoutingSummary(runtime: RuntimeConfig, signal?: AbortSignal): Promise<RoutingDataStatus | null> {
  if (!runtime.offline?.routing) return null
  const endpoint = runtime.offline.routing
  const response = await fetch(`${endpoint}${endpoint.includes('?') ? '&' : '?'}summary=1`, { signal })
  if (!response.ok) throw await responseError(response, 'Could not inspect routing storage')
  return decodeRoutingDataStatus(await response.json())
}

export async function prepareRoutingData(runtime: RuntimeConfig, regionId: string, update = false): Promise<RoutingDataStatus> {
  if (!runtime.offline?.routing) throw new Error('Local routing is unavailable')
  const response = await fetch(`${runtime.offline.routing}/prepare`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json', 'X-GPX-Editor': '1' },
    body: JSON.stringify({ regionId, update }),
  })
  if (!response.ok) throw await responseError(response, `Routing data preparation returned ${response.status}`)
  return decodeRoutingDataStatus(await response.json())
}

export async function cancelRoutingData(runtime: RuntimeConfig): Promise<RoutingDataStatus> {
  if (!runtime.offline?.routing) throw new Error('Local routing is unavailable')
  const response = await fetch(`${runtime.offline.routing}/cancel`, {
    method: 'POST',
    headers: { 'X-GPX-Editor': '1' },
  })
  if (!response.ok) throw await responseError(response, `Routing data cancellation returned ${response.status}`)
  return decodeRoutingDataStatus(await response.json())
}

export async function pinRoutingData(
  runtime: RuntimeConfig,
  regionId: string,
  generationId: string,
  pinned: boolean,
): Promise<RoutingDataStatus> {
  if (!runtime.offline?.routing) throw new Error('Local routing is unavailable')
  const response = await fetch(`${runtime.offline.routing}/pin`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json', 'X-GPX-Editor': '1' },
    body: JSON.stringify({ regionId, generationId, pinned }),
  })
  if (!response.ok) throw await responseError(response, `Routing data pin returned ${response.status}`)
  return decodeRoutingDataStatus(await response.json())
}

export async function pruneRoutingData(runtime: RuntimeConfig): Promise<RoutingDataStatus> {
  if (!runtime.offline?.routing) throw new Error('Local routing is unavailable')
  const response = await fetch(`${runtime.offline.routing}/prune`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json', 'X-GPX-Editor': '1' },
    body: JSON.stringify({ keepGenerations: 1, removeSources: true, removeMetrics: true }),
  })
  if (!response.ok) throw await responseError(response, `Routing data cleanup returned ${response.status}`)
  return decodeRoutingDataStatus(await response.json())
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
      reused: number(progress.reused),
      downloaded: number(progress.downloaded),
      revalidated: number(progress.revalidated),
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
    const kind = item.coverageKind
    const coverageKind: PackSummary['coverageKind'] = kind === 'area' || kind === 'city' || kind === 'region' || kind === 'country' ? kind : undefined
    return [{
      ...job,
      coverageKind,
      unavailable: Array.isArray(item.unavailable) ? item.unavailable.flatMap(value => {
        const blocked = record(value)
        const reason = text(blocked?.reason)
        return reason ? [{ reason, resource: text(blocked?.resource), layer: text(blocked?.layer) }] : []
      }) : [],
      batchesDone: number(item.batchesDone),
      batchesTotal: number(item.batchesTotal),
      bbox: decodePackBounds(item.bbox),
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

export async function setRuntimeOfflineMode(
  runtime: RuntimeConfig,
  mode: OfflineMode,
  fetcher: typeof fetch = fetch,
): Promise<OfflineMode> {
  const endpoint = runtime.offline?.modeControl
  if (!endpoint) throw new Error('Runtime offline mode control is unavailable')
  const response = await fetcher(endpoint, {
    method: 'PUT',
    headers: managementHeaders,
    body: JSON.stringify({ mode }),
  })
  await managementResponse(response, 'Offline mode change')
  const payload = record(await response.json().catch(() => null))
  return payload?.mode === 'cache-only' ? 'cache-only' : 'auto'
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
