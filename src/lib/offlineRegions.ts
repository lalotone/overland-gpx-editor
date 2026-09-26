import {
  PackTooLargeError,
  estimatePack,
  normalizePackBounds,
  prepareRoutingData,
  responseError,
  startPack,
} from './offline'
import type {
  PackBounds,
  PackEstimateRequest,
  PackResourceProgress,
  PackSummary,
  RoutingDataStatus,
  RuntimeConfig,
} from './offline'

/**
 * Region downloads shared by the web and mobile frontends. A download is a
 * Broom routing region plus regional map packs (vector maps, elevation, POIs
 * and fuel) over the same bounds. One pack is capped at 100,000 resources, so
 * an area that is too large for it — any sizeable country — is downloaded as
 * one pack per catalogue sub-region, halving areas that have none. Routing
 * still uses the single covering extract, so routes cross those boundaries.
 */

export type DownloadAreaKind = 'city' | 'region' | 'country' | 'area'

export interface DownloadArea {
  id: string
  name: string
  kind: DownloadAreaKind
  bounds: PackBounds
  regionId?: string
  /** Sub-regions to download instead when the area is too large for one pack. */
  parts?: DownloadArea[]
}

export interface DownloadRegion {
  id: string
  name: string
  parent?: string
  kind: 'continent' | 'country' | 'region'
  bbox?: PackBounds
  installed: boolean
  active: boolean
}

export const RESOURCE_LABELS: Record<string, string> = {
  'vector-map': 'Vector maps',
  elevation: 'Elevation',
  'fuel-prices': 'Fuel prices',
  'fuel-stations': 'Fuel stations',
  water: 'Water',
  campsites: 'Campsites',
}

const ACTIVE_PACK_STATES = new Set(['queued', 'running', 'cancelling'])

export function packIsActive(pack: PackSummary): boolean {
  return ACTIVE_PACK_STATES.has(pack.status)
}

export function routingJobActive(status: RoutingDataStatus | null | undefined): boolean {
  return status?.job?.state === 'queued' || status?.job?.state === 'running'
}

export function packAreaName(pack: PackSummary): string {
  return pack.name?.replace(/^(Map|Route):\s*/i, '').trim() || 'Downloaded area'
}

export function resourceTransferText(progress: PackResourceProgress | undefined): string | null {
  if (!progress || (progress.reused === undefined && progress.downloaded === undefined && progress.revalidated === undefined)) return null
  const parts = [`${(progress.reused ?? 0).toLocaleString()} cached`, `${(progress.downloaded ?? 0).toLocaleString()} downloaded`]
  if (progress.revalidated) parts.push(`${progress.revalidated.toLocaleString()} checked online`)
  return parts.join(' · ')
}

export function coverageLabel(kind: DownloadAreaKind | undefined): string {
  switch (kind) {
    case 'region': return 'Whole region'
    case 'country': return 'Whole country'
    case 'city': return 'City area'
    case 'area': return 'Selected map area'
    default: return 'Saved map extent'
  }
}

export function boundsLabel(bounds: PackBounds): string {
  return `${bounds.south.toFixed(3)}, ${bounds.west.toFixed(3)} → ${bounds.north.toFixed(3)}, ${bounds.east.toFixed(3)}`
}

export function coversBounds(pack: PackBounds | undefined, area: PackBounds): boolean {
  return Boolean(
    pack &&
      pack.south <= area.south + 1e-6 &&
      pack.west <= area.west + 1e-6 &&
      pack.north >= area.north - 1e-6 &&
      pack.east >= area.east - 1e-6,
  )
}

export function packFailure(pack: PackSummary): string {
  const failed = Object.entries(pack.resources)
    .filter(([, progress]) => progress.failed > 0)
    .map(([kind]) => RESOURCE_LABELS[kind] ?? kind)
  if (pack.detail === 'interrupted')
    return 'Download interrupted. Download again to resume using the cached data.'
  if (pack.detail === 'resource_limit')
    return 'The download reached its storage budget. Choose a smaller area.'
  if (pack.detail === 'start_failed')
    return `Could not start this queued download${pack.reason ? `: ${pack.reason}` : ''}. Download again to retry.`
  if (pack.detail === 'provider_limits')
    return 'Available resources downloaded. Place-provider limits require city-sized areas for fresh stops.'
  if (failed.length)
    return `Could not finish ${failed.join(', ').toLowerCase()}. Download again to retry the missing resources.`
  return 'Some resources are missing. Download again to finish the area.'
}

export function areaForRegion(region: DownloadRegion, catalogue: DownloadRegion[] = [], seen = new Set<string>()): DownloadArea | null {
  if (!region.bbox) return null
  seen.add(region.id)
  const parts = catalogue
    .filter(child => child.parent === region.id && child.kind === 'region' && !seen.has(child.id))
    .map(child => areaForRegion(child, catalogue, seen))
    .filter((area): area is DownloadArea => area !== null)
  return {
    id: region.id,
    name: region.name,
    kind: region.kind === 'country' ? 'country' : 'region',
    bounds: region.bbox,
    regionId: region.id,
    ...(parts.length ? { parts } : {}),
  }
}

const SPLIT_DEPTH = 3

function longitudeSpan(bounds: PackBounds): number {
  return bounds.east >= bounds.west ? bounds.east - bounds.west : bounds.east + 360 - bounds.west
}

/** Halve an area across its longer side; used when it has no sub-regions. */
export function splitArea(area: DownloadArea): DownloadArea[] {
  const { south, west, north, east } = area.bounds
  const span = longitudeSpan(area.bounds)
  const half = (suffix: string, bounds: PackBounds): DownloadArea =>
    ({ id: `${area.id}:${suffix}`, name: `${area.name} · ${suffix}`, kind: 'area', bounds })
  if (span * Math.cos(((south + north) / 2) * Math.PI / 180) >= north - south) {
    let middle = west + span / 2
    if (middle >= 180) middle -= 360
    return [half('west', { south, west, north, east: middle }), half('east', { south, west: middle, north, east })]
  }
  const middle = (south + north) / 2
  return [half('south', { south, west, north: middle, east }), half('north', { south: middle, west, north, east })]
}

function packFinished(pack: PackSummary): boolean {
  const maps = pack.resources['vector-map']
  return (pack.status === 'complete' || pack.detail === 'provider_limits' || pack.detail === 'resource_failures') &&
    maps?.failed === 0 && maps.total > 0 && maps.done === maps.total
}

function coveringPack(packs: PackSummary[], bounds: PackBounds): PackSummary | undefined {
  // A finished pack already holds the maps; otherwise follow the live attempt.
  const rank = (pack: PackSummary) => (packFinished(pack) ? 0 : packIsActive(pack) ? 1 : 2)
  return packs
    .filter(pack => coversBounds(pack.bbox, bounds))
    .sort((a, b) => rank(a) - rank(b) || (b.updatedAt ?? '').localeCompare(a.updatedAt ?? ''))[0]
}

function areaParts(packs: PackSummary[], area: DownloadArea, depth: number): DownloadArea[] {
  if (area.parts?.length) return area.parts
  const prefix = `Map: ${area.name} · `
  return depth < SPLIT_DEPTH && packs.some(pack => pack.name?.startsWith(prefix)) ? splitArea(area) : []
}

function collectAreaPacks(packs: PackSummary[], area: DownloadArea, depth: number, found: Map<string, PackSummary>): number {
  const direct = coveringPack(packs, area.bounds)
  if (direct) {
    found.set(direct.id, direct)
    return 0
  }
  const parts = areaParts(packs, area, depth)
  if (!parts.length) return 1
  return parts.reduce((missing, part) => missing + collectAreaPacks(packs, part, depth + 1, found), 0)
}

export interface AreaDownloadState {
  /** The packs that together hold the area's maps. */
  packs: PackSummary[]
  /** Every part of the area has a pack, finished or not. */
  covers: boolean
  /** Every part has finished its vector maps. */
  complete: boolean
  active: boolean
  resources: Record<string, PackResourceProgress>
  unavailable: NonNullable<PackSummary['unavailable']>
  /** The first pack that stopped short, for its explanation. */
  failed?: PackSummary
}

/** Combine the packs that make up an area, whether it took one or many. */
export function areaDownloadState(packs: PackSummary[], area: DownloadArea | null): AreaDownloadState {
  const found = new Map<string, PackSummary>()
  const missing = area ? collectAreaPacks(packs, area, 0, found) : 1
  const members = [...found.values()]
  const resources: Record<string, PackResourceProgress> = {}
  for (const pack of members) {
    for (const [kind, progress] of Object.entries(pack.resources)) {
      const total = resources[kind] ?? { done: 0, total: 0, failed: 0, bytes: 0, items: 0 }
      resources[kind] = {
        done: total.done + progress.done,
        total: total.total + progress.total,
        failed: total.failed + progress.failed,
        bytes: total.bytes + progress.bytes,
        items: total.items + progress.items,
        ...(progress.reused !== undefined || total.reused !== undefined ? { reused: (total.reused ?? 0) + (progress.reused ?? 0) } : {}),
        ...(progress.downloaded !== undefined || total.downloaded !== undefined ? { downloaded: (total.downloaded ?? 0) + (progress.downloaded ?? 0) } : {}),
        ...(progress.revalidated !== undefined || total.revalidated !== undefined ? { revalidated: (total.revalidated ?? 0) + (progress.revalidated ?? 0) } : {}),
      }
    }
  }
  return {
    packs: members,
    covers: missing === 0 && members.length > 0,
    complete: missing === 0 && members.length > 0 && members.every(packFinished),
    active: members.some(packIsActive),
    resources,
    unavailable: members.flatMap(pack => pack.unavailable ?? []),
    failed: members.find(pack => !packIsActive(pack) && (pack.status === 'failed' || pack.incomplete)),
  }
}

/** Whether the area's maps are fully downloaded, in one pack or several. */
export function mapsComplete(packs: PackSummary[], area: DownloadArea | null): boolean {
  return areaDownloadState(packs, area).complete
}

/** Finished or partial map packs, one per name and extent (retries reuse names). */
export function savedMapAreas(packs: PackSummary[]): PackSummary[] {
  return packs.filter((pack, index, all) =>
    pack.bbox &&
    pack.name?.startsWith('Map:') &&
    (pack.status === 'complete' || pack.incomplete) &&
    all.findIndex(p => p.name === pack.name && JSON.stringify(p.bbox) === JSON.stringify(pack.bbox)) === index,
  )
}

export function regionalPackRequest(area: DownloadArea): PackEstimateRequest {
  return {
    regional: true,
    coverageKind: area.kind,
    name: `Map: ${area.name}`,
    bbox: [area.bounds.south, area.bounds.west, area.bounds.north, area.bounds.east],
    paddingKm: 0,
    minZoom: 5,
    maxZoom: 14,
    layers: ['openfreemap'],
    scopes: ['elevation', 'pois', 'fuel'],
  }
}

function decodeBounds(value: unknown): PackBounds | undefined {
  if (!value || typeof value !== 'object') return undefined
  const { south, west, north, east } = value as Record<string, unknown>
  return [south, west, north, east].every(n => typeof n === 'number' && Number.isFinite(n))
    ? { south, west, north, east } as PackBounds
    : undefined
}

export function decodeRoutingRegions(value: unknown): { regions: DownloadRegion[]; cachedOnly: boolean } {
  const body = value && typeof value === 'object' ? value as Record<string, unknown> : {}
  const regions: DownloadRegion[] = []
  for (const item of Array.isArray(body.regions) ? body.regions : []) {
    if (!item || typeof item !== 'object') continue
    const entry = item as Record<string, unknown>
    if (typeof entry.id !== 'string' || typeof entry.name !== 'string') continue
    regions.push({
      id: entry.id,
      name: entry.name,
      parent: typeof entry.parent === 'string' && entry.parent ? entry.parent : undefined,
      kind: entry.kind === 'continent' || entry.kind === 'country' ? entry.kind : 'region',
      bbox: decodeBounds(entry.bbox),
      installed: entry.installed === true,
      active: entry.active === true,
    })
  }
  return { regions, cachedOnly: body.cachedOnly === true }
}

export async function fetchRoutingRegions(
  runtime: RuntimeConfig,
  signal?: AbortSignal,
): Promise<{ regions: DownloadRegion[]; cachedOnly: boolean }> {
  if (!runtime.offline?.routing) throw new Error('Local routing is unavailable')
  const response = await fetch(`${runtime.offline.routing}/regions`, { signal })
  if (!response.ok) throw await responseError(response, 'Could not load the region catalogue')
  return decodeRoutingRegions(await response.json())
}

async function suggestRoutingRegion(
  runtime: RuntimeConfig,
  bbox: PackBounds,
): Promise<{ regionId: string; name: string; coversView?: boolean } | null> {
  const response = await fetch(`${runtime.offline!.routing}/suggest`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json', 'X-GPX-Editor': '1' },
    body: JSON.stringify({ bbox }),
  })
  if (!response.ok) throw await responseError(response, 'Routing downloads are unavailable')
  const { region } = await response.json() as { region?: { regionId?: unknown; name?: unknown; coversView?: unknown } | null }
  if (!region || typeof region.regionId !== 'string' || typeof region.name !== 'string') return null
  return { regionId: region.regionId, name: region.name, coversView: region.coversView !== false }
}

export interface RegionDownloadHooks {
  /** Short human-readable step, for a status line. */
  phase?: (text: string) => void
  /** The area was matched to a routing region; fires before any download starts. */
  resolved?: (area: DownloadArea, regionId: string) => void
}

interface MapPlan {
  requests: PackEstimateRequest[]
  /** Parts that could not be downloaded, with the reason. */
  skipped: string[]
}

/** One pack request per piece of the area that fits the pack limits. */
async function planMapPacks(runtime: RuntimeConfig, area: DownloadArea, hooks: RegionDownloadHooks, depth = 0): Promise<MapPlan> {
  const request = regionalPackRequest(area)
  try {
    const estimate = await estimatePack(runtime, request)
    // Map/elevation batches can cover whole regions. Do not turn provider
    // limits on broad POI searches into tiled Overpass harvesting.
    const blockedMaps = estimate.blocked.filter(item => item.layer)
    if (blockedMaps.length) throw new Error(blockedMaps.map(item => item.reason).join(' · '))
    if (!estimate.resources) throw new Error('No map resources are available for this area.')
    return { requests: [request], skipped: [] }
  } catch (reason) {
    const parts = reason instanceof PackTooLargeError
      ? area.parts?.length ? area.parts : depth < SPLIT_DEPTH ? splitArea(area) : []
      : []
    if (!parts.length) {
      if (depth === 0) throw reason
      return { requests: [], skipped: [`${area.name}: ${(reason as Error).message}`] }
    }
    const plan: MapPlan = { requests: [], skipped: [] }
    for (const [index, part] of parts.entries()) {
      hooks.phase?.(`Checking maps for ${part.name} (${index + 1}/${parts.length})…`)
      const partPlan = await planMapPacks(runtime, part, hooks, depth + 1)
      plan.requests.push(...partPlan.requests)
      plan.skipped.push(...partPlan.skipped)
    }
    return plan
  }
}

/**
 * Start (or resume) a region: resolve its routing region, plan map packs that
 * fit provider and storage limits, then start routing and every map pack.
 * They run in the background — map packs beyond the server's job slots wait
 * in its queue — and callers follow them through the routing status and the
 * pack list.
 */
export async function startRegionDownload(
  runtime: RuntimeConfig,
  area: DownloadArea,
  hooks: RegionDownloadHooks = {},
): Promise<{ area: DownloadArea; regionId: string; pack?: PackSummary; packs: PackSummary[]; skipped: string[] }> {
  if (!runtime.offline?.routing) throw new Error('The local routing backend is unavailable.')
  const bounds = normalizePackBounds(
    { lat: area.bounds.south, lon: area.bounds.west },
    { lat: area.bounds.north, lon: area.bounds.east },
  )
  if (!bounds) throw new Error('Choose a smaller region before downloading.')
  let regionId = area.regionId
  let name = area.name
  hooks.phase?.('Finding routing coverage…')
  if (!regionId) {
    const region = await suggestRoutingRegion(runtime, bounds)
    if (!region || !region.coversView)
      throw new Error('This area is not covered by one local routing region. Choose a smaller area or a named region.')
    regionId = region.regionId
    if (area.kind === 'area') name = `Visible area near ${region.name}`
  }
  const selected: DownloadArea = { ...area, name, bounds, regionId }
  hooks.resolved?.(selected, regionId)
  hooks.phase?.('Checking maps and storage…')
  const plan = await planMapPacks(runtime, selected, hooks)
  if (!plan.requests.length) throw new Error(plan.skipped.join(' · ') || 'No map resources are available for this area.')
  hooks.phase?.(`Preparing ${name}…`)
  await prepareRoutingData(runtime, regionId)
  const packs: PackSummary[] = []
  for (const [index, request] of plan.requests.entries()) {
    if (plan.requests.length > 1) hooks.phase?.(`Queueing maps ${index + 1} of ${plan.requests.length}…`)
    const pack = await startPack(runtime, request)
    if (pack) packs.push(pack)
  }
  return { area: selected, regionId, pack: packs[0], packs, skipped: plan.skipped }
}
