import {
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
 * Broom routing region plus one regional map pack (vector maps, elevation,
 * POIs and fuel) over the same bounds.
 */

export type DownloadAreaKind = 'city' | 'region' | 'country' | 'area'

export interface DownloadArea {
  id: string
  name: string
  kind: DownloadAreaKind
  bounds: PackBounds
  regionId?: string
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
  if (pack.detail === 'provider_limits')
    return 'Available resources downloaded. Place-provider limits require city-sized areas for fresh stops.'
  if (failed.length)
    return `Could not finish ${failed.join(', ').toLowerCase()}. Download again to retry the missing resources.`
  return 'Some resources are missing. Download again to finish the area.'
}

export function areaForRegion(region: DownloadRegion): DownloadArea | null {
  return region.bbox
    ? {
        id: region.id,
        name: region.name,
        kind: region.kind === 'country' ? 'country' : 'region',
        bounds: region.bbox,
        regionId: region.id,
      }
    : null
}

/** A finished map pack whose vector maps fully cover the area. */
export function completeMapPack(packs: PackSummary[], area: DownloadArea | null): PackSummary | undefined {
  if (!area) return undefined
  return packs.find(pack => {
    const maps = pack.resources['vector-map']
    return (pack.status === 'complete' || pack.detail === 'provider_limits' || pack.detail === 'resource_failures') &&
      coversBounds(pack.bbox, area.bounds) &&
      maps?.failed === 0 && maps.total > 0 && maps.done === maps.total
  })
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

/**
 * Start (or resume) a region: resolve its routing region, check the map pack
 * fits provider and storage limits, then start routing and map downloads.
 * Both run in the background; callers follow them through the routing status
 * and the pack list.
 */
export async function startRegionDownload(
  runtime: RuntimeConfig,
  area: DownloadArea,
  hooks: RegionDownloadHooks = {},
): Promise<{ area: DownloadArea; regionId: string; pack?: PackSummary }> {
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
  const request = regionalPackRequest(selected)
  hooks.phase?.('Checking maps and storage…')
  const estimate = await estimatePack(runtime, request)
  // Map/elevation batches can cover whole regions. Do not turn provider
  // limits on broad POI searches into tiled Overpass harvesting.
  const blockedMaps = estimate.blocked.filter(item => item.layer)
  if (blockedMaps.length) throw new Error(blockedMaps.map(item => item.reason).join(' · '))
  if (!estimate.resources) throw new Error('No map resources are available for this area.')
  hooks.phase?.(`Preparing ${name}…`)
  await prepareRoutingData(runtime, regionId)
  const pack = await startPack(runtime, request)
  return { area: selected, regionId, pack }
}
