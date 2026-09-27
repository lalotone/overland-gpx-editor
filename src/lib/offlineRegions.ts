import {
  PackTooLargeError,
  estimatePack,
  fetchRoutingDataStatus,
  formatBytes,
  normalizePackBounds,
  prepareRoutingData,
  responseError,
  startPack,
} from './offline'
import type {
  RoutingDataJob,
  PackBounds,
  PackEstimateRequest,
  PackResourceProgress,
  PackSummary,
  RoutingDataStatus,
  RuntimeConfig,
} from './offline'
import { intersectsSpain } from './fuel'

/**
 * Region downloads shared by the web and mobile frontends. A region offers
 * four resources that download independently, as in OsmAnd: its Broom routing
 * data, which planning needs even online, and three pack groups — maps,
 * terrain and points of interest — needed only offline, since online they are
 * fetched as they are viewed. Each group downloads as regional map packs over
 * the same bounds. One pack is capped at 100,000 resources, so
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

/** Offline-only resources, each stored in its own packs. */
export type ResourceGroup = 'maps' | 'terrain' | 'places'
export type DownloadResource = 'routing' | ResourceGroup

export const RESOURCE_GROUPS: Record<ResourceGroup, { label: string; kinds: string[]; layers: string[]; scopes: string[] }> = {
  maps: { label: 'Maps', kinds: ['vector-map'], layers: ['openfreemap'], scopes: [] },
  terrain: { label: 'Terrain', kinds: ['elevation'], layers: [], scopes: ['elevation'] },
  places: { label: 'Points of interest', kinds: ['fuel-stations', 'water', 'campsites', 'fuel-prices'], layers: [], scopes: ['pois', 'fuel'] },
}
export const PACK_GROUPS: ResourceGroup[] = ['maps', 'terrain', 'places']
export const ALL_RESOURCES: DownloadResource[] = ['routing', ...PACK_GROUPS]

export function isPackGroup(resource: DownloadResource): resource is ResourceGroup {
  return resource !== 'routing'
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

/** The four steps a routing preparation goes through, as users see them. */
export const ROUTING_STEPS = [
  { id: 'data', label: 'Road data' },
  { id: 'terrain', label: 'Terrain' },
  { id: 'graph', label: 'Routing graph' },
  { id: 'profiles', label: 'Riding profiles' },
] as const

// Broom's progress phases grouped into those steps.
const PHASE_STEPS: Record<string, number> = {
  region: 0, index: 0, pbf: 0,
  planning: 1, elevation: 1,
  build: 2,
  warmup: 3,
}

// Broom's graph-build sub-stages in plain words. Unknown ones fall back to a
// generic label rather than leaking internal names.
const BUILD_STAGES: Record<string, string> = {
  'build-prepare': 'Preparing the build',
  'pbf-node-index': 'Indexing road nodes',
  'pbf-required-nodes': 'Reading road nodes',
  'topology-vertices': 'Assembling the road network',
  'topology-segments': 'Assembling the road network',
  'topology-assemble': 'Assembling the road network',
  'dem-geometry': 'Adding elevation to roads',
  'sample-elevation': 'Sampling elevation',
  restrictions: 'Applying turn restrictions',
  components: 'Connecting the network',
  partition: 'Partitioning the graph',
  write: 'Writing the graph',
  'build-complete': 'Finishing the build',
}

function formatDuration(seconds: number): string {
  const minutes = Math.floor(seconds / 60)
  return minutes ? `${minutes}m ${Math.floor(seconds % 60)}s` : `${Math.floor(seconds)}s`
}

export interface RoutingJobView {
  /** Index into ROUTING_STEPS; -1 before the first phase reports. */
  step: number
  /** Plain-language account of what is happening right now. */
  detail: string
  /**
   * Progress of the current activity, 0..1, only when Broom measures it.
   * Null means working without a measure: show liveness, never a bar.
   */
  fraction: number | null
}

/**
 * What a routing preparation is doing, in terms a rider can follow. Broom
 * reports progress per phase and per build sub-stage, several of which have
 * no measure at all, so there is no honest single percentage: the view is a
 * step checklist plus the current activity, measured when it can be.
 */
export function routingJobView(job: RoutingDataJob | undefined): RoutingJobView {
  if (!job) return { step: -1, detail: 'Starting…', fraction: null }
  const step = PHASE_STEPS[job.phase ?? ''] ?? -1
  const elapsed = job.elapsedSeconds ? ` · ${formatDuration(job.elapsedSeconds)}` : ''
  const measured = (done: number | undefined, total: number | undefined) =>
    total ? Math.min(1, Math.max(0, (done ?? 0) / total)) : null
  if (job.retrying) {
    return { step, detail: `Connection problem · retrying${job.retrySeconds ? ` in ${Math.ceil(job.retrySeconds)}s` : ''} (attempt ${job.attempt ?? 1})`, fraction: null }
  }
  switch (job.phase) {
    case 'region':
    case 'index':
      return { step, detail: 'Looking up the region', fraction: null }
    case 'pbf':
      return {
        step,
        detail: job.total
          ? `Downloading road data · ${formatBytes(job.done ?? 0)} of ${formatBytes(job.total)}`
          : `Downloading road data${elapsed}`,
        fraction: measured(job.done, job.total),
      }
    case 'planning':
      return { step, detail: `Choosing terrain tiles${elapsed}`, fraction: null }
    case 'elevation':
      return {
        step,
        detail: job.itemsTotal
          ? `Downloading terrain · ${(job.completedItems ?? 0).toLocaleString()} of ${job.itemsTotal.toLocaleString()} tiles (${(job.itemsDownloaded ?? 0).toLocaleString()} new, ${(job.itemsReused ?? 0).toLocaleString()} reused)`
          : `Downloading terrain${elapsed}`,
        fraction: measured(job.completedItems, job.itemsTotal),
      }
    case 'build': {
      const label = BUILD_STAGES[job.stage ?? job.item ?? ''] ?? 'Building the routing graph'
      const fraction = measured(job.done, job.total)
      return { step, detail: fraction === null ? `${label}${elapsed}` : `${label} · ${Math.round(fraction * 100)}%`, fraction }
    }
    case 'warmup':
      return { step, detail: `Preparing riding profiles${job.item ? ` · ${job.item}` : ''}${elapsed}`, fraction: null }
    default:
      return { step, detail: `Working${elapsed}`, fraction: null }
  }
}

const DIAGNOSTIC_LABELS: Record<string, string> = {
  'malformed-restrictions': 'malformed turn restrictions',
  'unsupported-conditional-restrictions': 'conditional turn restrictions',
  'unsupported-mode-restrictions': 'vehicle-specific turn restrictions',
  'unmapped-restrictions': 'turn restrictions Broom cannot represent',
}

/**
 * Broom's build notes in plain words, or null when there are none. These are
 * data-quality remarks about the map, not failures: the graph is complete.
 */
export function routingDiagnosticsText(job: RoutingDataJob | undefined): string | null {
  const notes = (job?.diagnostics ?? []).filter(item => item.Count > 0)
  if (!notes.length) return null
  const total = notes.reduce((sum, item) => sum + item.Count, 0)
  const parts = notes.map(item => `${item.Count.toLocaleString()} ${DIAGNOSTIC_LABELS[item.Code] ?? item.Code.replace(/-/g, ' ')}`)
  return `Skipped ${total.toLocaleString()} restriction${total === 1 ? '' : 's'} in the map data: ${parts.join(', ')}. Routes still work; those junctions may allow a turn the signs do not.`
}

/** A readable name for a routing region id when the catalogue has none yet. */
export function regionIdName(id: string): string {
  const last = id.split('/').pop() ?? id
  return last
    .split('-')
    .map(word => (['of', 'and', 'the', 'de', 'del', 'la', 'y'].includes(word) ? word : word.charAt(0).toUpperCase() + word.slice(1)))
    .join(' ')
}

export function packAreaName(pack: PackSummary): string {
  return pack.name?.replace(/^(Map|Maps|Route|Terrain|Points of interest|[\w ]+ \+ [\w +]+):\s*/i, '').trim() || 'Downloaded area'
}

/** The resource groups a pack holds, read from what it reports progress for. */
export function packGroups(pack: PackSummary): ResourceGroup[] {
  return PACK_GROUPS.filter(group => RESOURCE_GROUPS[group].kinds.some(kind =>
    pack.resources[kind] !== undefined ||
    pack.unavailable?.some(item => item.resource === kind || (kind === 'vector-map' && item.layer === 'openfreemap'))))
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

/** "fuel stations (the request timed out)" for each resource that failed. */
export function failedResourcesText(pack: PackSummary): string {
  return Object.entries(pack.resources)
    .filter(([, progress]) => progress.failed > 0)
    .map(([kind, progress]) => `${(RESOURCE_LABELS[kind] ?? kind).toLowerCase()}${progress.error ? ` (${progress.error})` : ''}`)
    .join(', ')
}

export function packWasStopped(pack: PackSummary): boolean {
  return pack.detail === 'cancelled'
}

export function packFailure(pack: PackSummary): string {
  const failed = failedResourcesText(pack)
  if (packWasStopped(pack))
    return `Stopped at ${(pack.done ?? 0).toLocaleString()} of ${(pack.total ?? 0).toLocaleString()} resources. Downloaded data is kept; download again to continue.`
  if (pack.detail === 'interrupted')
    return 'Download interrupted. Download again to resume using the cached data.'
  if (pack.detail === 'resource_limit')
    return 'The download reached its storage budget. Choose a smaller area.'
  if (pack.detail === 'start_failed')
    return `Could not start this queued download${pack.reason ? `: ${pack.reason}` : ''}. Download again to retry.`
  if (pack.detail === 'provider_limits')
    return 'Available resources downloaded. Place-provider limits require city-sized areas for fresh stops.'
  if (failed)
    return `Could not finish ${failed}. Download again to retry the missing resources.`
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

// Zooms every region download stores (regionalPackRequest) and the zoom the
// server keeps terrain at.
const MAP_MIN_ZOOM = 5
const MAP_MAX_ZOOM = 14
const TERRAIN_ZOOM = 13
// One pack holds 100,000 resources and 16,384 terrain tiles. Parts aim well
// below both: neighbouring parts repeat low-zoom tiles, and the server adds
// glyphs, POIs and fuel on top.
const PART_MAP_TILES = 75_000
const PART_TERRAIN_TILES = 14_000
const MAX_GRID_PARTS = 400
// Halvings allowed when the server still finds a part too large.
const SPLIT_DEPTH = 2

function longitudeSpan(bounds: PackBounds): number {
  return bounds.east >= bounds.west ? bounds.east - bounds.west : bounds.east + 360 - bounds.west
}

function wrapLongitude(value: number): number {
  return value >= 180 ? value - 360 : value
}

/** Web Mercator y in [0, 1], north at 0. */
function mercatorY(lat: number): number {
  const clamped = Math.max(-85.05112878, Math.min(85.05112878, lat)) * Math.PI / 180
  return (1 - Math.log(Math.tan(clamped) + 1 / Math.cos(clamped)) / Math.PI) / 2
}

function latitudeAt(y: number): number {
  return Math.atan(Math.sinh(Math.PI * (1 - 2 * y))) * 180 / Math.PI
}

function tilesAt(bounds: PackBounds, zoom: number): number {
  const n = 2 ** zoom
  const column = (lon: number) => Math.min(n - 1, Math.floor(((lon + 180) / 360) * n))
  const x0 = column(bounds.west)
  const x1 = column(bounds.east)
  const columns = bounds.east >= bounds.west ? x1 - x0 + 1 : n - x0 + x1 + 1
  const rows = Math.min(n - 1, Math.floor(mercatorY(bounds.south) * n)) - Math.floor(mercatorY(bounds.north) * n) + 1
  return columns * rows
}

/** Tiles a bbox spans over a zoom range, as the server enumerates them. */
export function tileCount(bounds: PackBounds, minZoom = MAP_MIN_ZOOM, maxZoom = MAP_MAX_ZOOM): number {
  let total = 0
  for (let zoom = minZoom; zoom <= maxZoom; zoom++) total += tilesAt(bounds, zoom)
  return total
}

function fitsOnePack(bounds: PackBounds): boolean {
  return tileCount(bounds) <= PART_MAP_TILES && tilesAt(bounds, TERRAIN_ZOOM) <= PART_TERRAIN_TILES
}

const ROW_WORDS = [[''], ['north', 'south'], ['north', 'central', 'south'], ['far north', 'north', 'south', 'far south'], ['far north', 'north', 'central', 'south', 'far south']]
const COLUMN_WORDS = [[''], ['west', 'east'], ['west', 'central', 'east'], ['far west', 'west', 'east', 'far east'], ['far west', 'west', 'central', 'east', 'far east']]

/** "north-west", "far south-central": a compass name a rider can place. */
export function gridLabel(row: number, rows: number, column: number, columns: number): string {
  const vertical = ROW_WORDS[rows - 1]?.[row]
  const horizontal = COLUMN_WORDS[columns - 1]?.[column]
  if (vertical === undefined || horizontal === undefined) return `row ${row + 1} of ${rows}, column ${column + 1} of ${columns}`
  if (!vertical || !horizontal) return vertical || horizontal
  if (vertical === 'central' && horizontal === 'central') return 'centre'
  if (vertical === 'central') return horizontal
  if (horizontal === 'central') return vertical
  return `${vertical}-${horizontal}`
}

/**
 * Split an area too large for one pack into a grid of parts that each fit,
 * or nothing when it fits already. Rows are even in Mercator space, so parts
 * carry similar tile counts. Deterministic: the same bounds always give the
 * same parts, which is how a later visit recognises a gridded download.
 */
export function gridParts(area: DownloadArea): DownloadArea[] {
  if (fitsOnePack(area.bounds)) return []
  const top = mercatorY(area.bounds.north)
  const bottom = mercatorY(area.bounds.south)
  const span = longitudeSpan(area.bounds)
  const aspect = Math.max(1e-6, (span / 360) / Math.max(1e-6, bottom - top))
  let count = Math.ceil(Math.max(
    tileCount(area.bounds) / PART_MAP_TILES,
    tilesAt(area.bounds, TERRAIN_ZOOM) / PART_TERRAIN_TILES,
  ))
  for (;;) {
    const columns = Math.max(1, Math.round(Math.sqrt(count * aspect)))
    const rows = Math.max(1, Math.ceil(count / columns))
    const parts: DownloadArea[] = []
    for (let row = 0; row < rows; row++) {
      const north = latitudeAt(top + ((bottom - top) * row) / rows)
      const south = latitudeAt(top + ((bottom - top) * (row + 1)) / rows)
      for (let column = 0; column < columns; column++) {
        const west = wrapLongitude(area.bounds.west + (span * column) / columns)
        const east = column === columns - 1 ? area.bounds.east : wrapLongitude(area.bounds.west + (span * (column + 1)) / columns)
        parts.push({
          id: `${area.id}:${row}-${column}`,
          name: `${area.name} · ${gridLabel(row, rows, column, columns)}`,
          kind: 'area',
          bounds: { south: row === rows - 1 ? area.bounds.south : south, west, north: row === 0 ? area.bounds.north : north, east },
          ...(area.regionId ? { regionId: area.regionId } : {}),
        })
      }
    }
    if (parts.every(part => fitsOnePack(part.bounds)) || parts.length >= MAX_GRID_PARTS) return parts
    count++
  }
}

/** The parts a too-large area downloads as: its catalogue sub-regions, else a grid. */
export function downloadParts(area: DownloadArea): DownloadArea[] {
  if (fitsOnePack(area.bounds)) return []
  return area.parts?.length ? area.parts : gridParts(area)
}

/** Halve an area across its longer side, when the server finds a part too large. */
export function splitArea(area: DownloadArea): DownloadArea[] {
  const { south, west, north, east } = area.bounds
  const span = longitudeSpan(area.bounds)
  const half = (suffix: string, bounds: PackBounds): DownloadArea =>
    ({ id: `${area.id}:${suffix}`, name: `${area.name} · ${suffix}`, kind: 'area', bounds, ...(area.regionId ? { regionId: area.regionId } : {}) })
  if (span * Math.cos(((south + north) / 2) * Math.PI / 180) >= north - south) {
    const middle = wrapLongitude(west + span / 2)
    return [half('west', { south, west, north, east: middle }), half('east', { south, west: middle, north, east })]
  }
  const middle = latitudeAt((mercatorY(south) + mercatorY(north)) / 2)
  return [half('south', { south, west, north: middle, east }), half('north', { south: middle, west, north, east })]
}

function packSettled(pack: PackSummary): boolean {
  return pack.status === 'complete' || pack.detail === 'provider_limits' || pack.detail === 'resource_failures'
}

/** A settled pack whose resources in the group all arrived. */
function packFinished(pack: PackSummary, group?: ResourceGroup): boolean {
  if (!packSettled(pack)) return false
  const kinds = group ? RESOURCE_GROUPS[group].kinds : Object.keys(pack.resources)
  const present = kinds.map(kind => pack.resources[kind]).filter(progress => progress !== undefined)
  return present.every(progress => progress.failed === 0 && progress.done === progress.total) &&
    (group !== 'maps' || present.some(progress => progress.total > 0))
}

function coveringPack(packs: PackSummary[], bounds: PackBounds, group?: ResourceGroup): PackSummary | undefined {
  // A finished pack already holds the maps; otherwise follow the live attempt.
  const rank = (pack: PackSummary) => (packFinished(pack, group) ? 0 : packIsActive(pack) ? 1 : 2)
  return packs
    .filter(pack => coversBounds(pack.bbox, bounds))
    .sort((a, b) => rank(a) - rank(b) || (b.updatedAt ?? '').localeCompare(a.updatedAt ?? ''))[0]
}

function areaParts(packs: PackSummary[], area: DownloadArea, depth: number): DownloadArea[] {
  const planned = area.parts?.length ? area.parts : gridParts(area)
  if (planned.length) return planned
  // A part the server still found too large was halved; its halves lie inside.
  const halved = depth < SPLIT_DEPTH && packs.some(pack => pack.bbox && coversBounds(area.bounds, pack.bbox) && !coversBounds(pack.bbox, area.bounds))
  return halved ? splitArea(area) : []
}

function collectAreaPacks(packs: PackSummary[], area: DownloadArea, depth: number, found: Map<string, PackSummary>, group?: ResourceGroup): number {
  const direct = coveringPack(packs, area.bounds, group)
  if (direct) {
    found.set(direct.id, direct)
    return 0
  }
  const parts = areaParts(packs, area, depth)
  if (!parts.length) return 1
  return parts.reduce((missing, part) => missing + collectAreaPacks(packs, part, depth + 1, found, group), 0)
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

/**
 * Combine the packs that make up an area, whether it took one or many. With a
 * group, only packs holding that resource count, and completion is judged on
 * it alone; without one, any pack counts.
 */
export function areaDownloadState(packs: PackSummary[], area: DownloadArea | null, group?: ResourceGroup): AreaDownloadState {
  const found = new Map<string, PackSummary>()
  const relevant = group ? packs.filter(pack => packGroups(pack).includes(group)) : packs
  const missing = area ? collectAreaPacks(relevant, area, 0, found, group) : 1
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
    complete: missing === 0 && members.length > 0 && members.every(pack => packFinished(pack, group)),
    active: members.some(packIsActive),
    resources,
    unavailable: members.flatMap(pack => pack.unavailable ?? []),
    // A pack that stopped short of another group is not this group's failure:
    // after a retry the maps pack may still carry a dead POI query.
    failed: members.find(pack => !packIsActive(pack) && (pack.status === 'failed' || pack.incomplete) && !packFinished(pack, group)),
  }
}

/** Whether the area's maps are fully downloaded, in one pack or several. */
export function mapsComplete(packs: PackSummary[], area: DownloadArea | null): boolean {
  return areaDownloadState(packs, area, 'maps').complete
}

export type GroupState = 'done' | 'running' | 'queued' | 'stopped' | 'failed' | 'partial' | 'unavailable' | 'none'

export interface GroupStatus {
  state: GroupState
  /** Share done while running, 0..1. */
  fraction?: number
  /** Resources done and expected, summed over the area's packs. */
  done: number
  total: number
  /** Bytes stored so far for this group. */
  bytes: number
}

/** One resource group's state for one area, from the packs that hold it. */
export function groupStatus(packs: PackSummary[], area: DownloadArea | null, group: ResourceGroup): GroupStatus {
  const state = areaDownloadState(packs, area, group)
  const kinds = RESOURCE_GROUPS[group].kinds
  const present = kinds.map(kind => state.resources[kind]).filter(progress => progress !== undefined)
  const done = present.reduce((sum, progress) => sum + progress.done, 0)
  const total = present.reduce((sum, progress) => sum + progress.total, 0)
  const bytes = present.reduce((sum, progress) => sum + progress.bytes, 0)
  const counts = { done, total, bytes }
  if (state.complete) return { state: 'done', ...counts }
  if (state.active) {
    return state.packs.some(pack => pack.status === 'running')
      ? { state: 'running', fraction: total ? done / total : 0, ...counts }
      : { state: 'queued', ...counts }
  }
  if (!present.length && state.unavailable.some(item => (item.resource && kinds.includes(item.resource)) || (group === 'maps' && item.layer === 'openfreemap')))
    return { state: 'unavailable', ...counts }
  if (state.failed) {
    if (packWasStopped(state.failed) && present.every(progress => progress.failed === 0)) return { state: 'stopped', ...counts }
    return { state: state.covers ? 'failed' : 'partial', ...counts }
  }
  if (state.packs.length) return { state: 'partial', ...counts }
  return { state: 'none', ...counts }
}

/** How many of an area's parts have a group, for "7 of 19 regions". */
export function partsProgress(packs: PackSummary[], parts: DownloadArea[], group: ResourceGroup): { done: number; active: number; total: number } {
  const statuses = parts.map(part => groupStatus(packs, part, group).state)
  return {
    done: statuses.filter(state => state === 'done').length,
    active: statuses.filter(state => state === 'running' || state === 'queued').length,
    total: parts.length,
  }
}

/** Finished or partial area packs, one per name and extent (retries reuse names). */
export function savedMapAreas(packs: PackSummary[]): PackSummary[] {
  return packs.filter((pack, index, all) =>
    pack.bbox &&
    !pack.name?.startsWith('Route:') &&
    packGroups(pack).length > 0 &&
    (pack.status === 'complete' || pack.incomplete) &&
    all.findIndex(p => p.name === pack.name && JSON.stringify(p.bbox) === JSON.stringify(pack.bbox)) === index,
  )
}

/**
 * The pack for some resource groups of an area. All three together keep the
 * original "Map:" name and request, so earlier downloads still match.
 */
export function regionalPackRequest(area: DownloadArea, groups: ResourceGroup[] = PACK_GROUPS): PackEstimateRequest {
  const chosen = PACK_GROUPS.filter(group => groups.includes(group))
  const label = chosen.length === PACK_GROUPS.length ? 'Map' : chosen.map(group => RESOURCE_GROUPS[group].label).join(' + ')
  return {
    regional: true,
    coverageKind: area.kind,
    name: `${label}: ${area.name}`,
    bbox: [area.bounds.south, area.bounds.west, area.bounds.north, area.bounds.east],
    paddingKm: 0,
    minZoom: 5,
    maxZoom: 14,
    layers: chosen.flatMap(group => RESOURCE_GROUPS[group].layers),
    // The fuel snapshot is Spain's national price file; elsewhere it would
    // only add 12 MiB of stations the rider cannot reach.
    scopes: chosen.flatMap(group => RESOURCE_GROUPS[group].scopes).filter(scope => scope !== 'fuel' || intersectsSpain(area.bounds)),
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
  /** The area was resolved (and, with routing, matched to a region); fires before any download starts. */
  resolved?: (area: DownloadArea, regionId?: string) => void
}

interface MapPlan {
  requests: { name: string; request: PackEstimateRequest; bytes: number }[]
  /** Parts that could not be downloaded, with the reason. */
  skipped: string[]
  /** Storage the cache reported free at the last estimate. */
  available?: number
}

/** One pack request per piece of the area that fits the pack limits. */
async function planMapPacks(runtime: RuntimeConfig, area: DownloadArea, groups: ResourceGroup[], hooks: RegionDownloadHooks, depth = 0, top = true): Promise<MapPlan> {
  // Parts do not depend on the groups chosen, so every group's packs line up.
  const known = downloadParts(area)
  if (known.length) return planPartPacks(runtime, known, groups, hooks, depth)
  const request = regionalPackRequest(area, groups)
  try {
    const estimate = await estimatePack(runtime, request)
    // Map/elevation batches can cover whole regions. Do not turn provider
    // limits on broad POI searches into tiled Overpass harvesting.
    const blockedMaps = estimate.blocked.filter(item => item.layer)
    if (blockedMaps.length) throw new Error(blockedMaps.map(item => item.reason).join(' · '))
    if (!estimate.resources) throw new Error('Nothing of this kind is available for this area.')
    return { requests: [{ name: area.name, request, bytes: estimate.genericBytes ?? 0 }], skipped: [], available: estimate.quotaRemaining }
  } catch (reason) {
    // The arithmetic says it fits but the server disagrees (more map sources,
    // say): halve it, a bounded number of times.
    const parts = reason instanceof PackTooLargeError && depth < SPLIT_DEPTH
      ? area.parts?.length ? area.parts : splitArea(area)
      : []
    if (!parts.length) {
      if (top) throw reason
      return { requests: [], skipped: [`${area.name}: ${(reason as Error).message}`] }
    }
    return planPartPacks(runtime, parts, groups, hooks, depth + 1)
  }
}

async function planPartPacks(runtime: RuntimeConfig, parts: DownloadArea[], groups: ResourceGroup[], hooks: RegionDownloadHooks, depth: number): Promise<MapPlan> {
  const plan: MapPlan = { requests: [], skipped: [] }
  for (const [index, part] of parts.entries()) {
    hooks.phase?.(`Checking maps for ${part.name} (${index + 1}/${parts.length})…`)
    const partPlan = await planMapPacks(runtime, part, groups, hooks, depth, false)
    plan.requests.push(...partPlan.requests)
    plan.skipped.push(...partPlan.skipped)
    plan.available = partPlan.available ?? plan.available
  }
  return plan
}

/**
 * Start routing preparation, or share one already running for the same
 * region — downloading one area of a country while another area of it is
 * preparing. A different region's preparation must finish first.
 */
async function prepareRegionRouting(runtime: RuntimeConfig, regionId: string): Promise<void> {
  try {
    await prepareRoutingData(runtime, regionId)
  } catch (reason) {
    const status = await fetchRoutingDataStatus(runtime).catch(() => null)
    const job = status?.job
    if (job && (job.state === 'queued' || job.state === 'running')) {
      if (job.regionId === regionId) return
      throw new Error(`Routing for ${status.name && status.regionId === job.regionId ? status.name : job.regionId} is still being prepared. Start this download when it finishes.`)
    }
    throw reason
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
  resources: DownloadResource[] = ALL_RESOURCES,
): Promise<{ area: DownloadArea; regionId?: string; pack?: PackSummary; packs: PackSummary[]; skipped: string[]; warning?: string }> {
  const routing = resources.includes('routing')
  const groups = PACK_GROUPS.filter(group => resources.includes(group))
  if (!routing && !groups.length) throw new Error('Choose something to download.')
  if (routing && !runtime.offline?.routing) throw new Error('The local routing backend is unavailable.')
  const bounds = normalizePackBounds(
    { lat: area.bounds.south, lon: area.bounds.west },
    { lat: area.bounds.north, lon: area.bounds.east },
  )
  if (!bounds) throw new Error('Choose a smaller region before downloading.')
  let regionId = area.regionId
  let name = area.name
  if (!regionId && routing) {
    hooks.phase?.('Finding routing coverage…')
    const region = await suggestRoutingRegion(runtime, bounds)
    if (!region || !region.coversView)
      throw new Error('This area is not covered by one local routing region. Choose a smaller area or a named region.')
    regionId = region.regionId
    if (area.kind === 'area') name = `Visible area near ${region.name}`
  }
  const selected: DownloadArea = { ...area, name, bounds, ...(regionId ? { regionId } : {}) }
  hooks.resolved?.(selected, regionId)
  let plan: MapPlan = { requests: [], skipped: [] }
  let warning: string | undefined
  if (groups.length) {
    hooks.phase?.('Checking maps and storage…')
    plan = await planMapPacks(runtime, selected, groups, hooks)
    if (!plan.requests.length) throw new Error(plan.skipped.join(' · ') || 'Nothing of this kind is available for this area.')
    // Check storage before the routing build starts. Only a part that cannot fit
    // on its own is refused: summed estimates count tiles shared by neighbouring
    // parts more than once, so a larger total is a warning, not a verdict.
    const available = plan.available
    const largest = Math.max(...plan.requests.map(part => part.bytes))
    if (available !== undefined && largest > available)
      throw new Error(`${name} needs about ${formatBytes(largest)} for its largest download, but only ${formatBytes(available)} of storage is available for offline data.`)
    const total = plan.requests.reduce((sum, part) => sum + part.bytes, 0)
    if (available !== undefined && total > available)
      warning = `${name} may need up to ${formatBytes(total)} and ${formatBytes(available)} is available. Downloads that run out of space stop and can be resumed later.`
  }
  if (routing && regionId) {
    hooks.phase?.(`Preparing routing for ${name}…`)
    await prepareRegionRouting(runtime, regionId)
  }
  const packs: PackSummary[] = []
  const skipped = [...plan.skipped]
  for (const [index, part] of plan.requests.entries()) {
    if (plan.requests.length > 1) hooks.phase?.(`Queueing downloads ${index + 1} of ${plan.requests.length}…`)
    try {
      const pack = await startPack(runtime, part.request)
      if (pack) packs.push(pack)
    } catch (reason) {
      // Keep queueing the other parts; one refusal should not strand them.
      if (plan.requests.length === 1) throw reason
      skipped.push(`${part.name}: ${(reason as Error).message}`)
    }
  }
  if (groups.length && !packs.length) throw new Error(skipped.join(' · ') || 'No downloads could be started.')
  return { area: selected, regionId, pack: packs[0], packs, skipped, warning }
}
