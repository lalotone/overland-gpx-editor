import { useEffect, useMemo, useState } from 'react'
import type { FormEvent } from 'react'
import { fetchRoutingSummary, formatBytes } from '../lib/offline'
import type { PackSummary, RoutingDataJob, RoutingDataStatus, RuntimeConfig } from '../lib/offline'
import { searchPlaces } from '../lib/geocoding'
import type { PlaceResult } from '../lib/geocoding'
import {
  PACK_GROUPS,
  ROUTING_STAGES,
  routingJobProgress,
  RESOURCE_GROUPS,
  RESOURCE_LABELS,
  areaForRegion,
  boundsLabel,
  areaDownloadState,
  coverageLabel,
  fetchRoutingRegions,
  gridParts,
  mapsComplete,
  packAreaName,
  packFailure,
  packGroups,
  packIsActive,
  resourceTransferText,
} from '../lib/offlineRegions'
import type { AreaDownloadState, DownloadArea, DownloadRegion, DownloadResource, ResourceGroup } from '../lib/offlineRegions'
import type { RegionDownloads } from './useRegionDownloads'
import { ESCAPE_PRIORITY, useEscapeDismiss } from './useEscapeDismiss'
import './OfflineRegions.css'


const GROUP_HINTS: Record<ResourceGroup, string> = {
  maps: 'Vector map tiles, zoom 5–14',
  terrain: 'Elevation for profiles and slope colours',
  places: 'Fuel stations and prices, water, campsites',
}

/** Sum a group's resources across the packs that hold them. */
function groupProgress(state: AreaDownloadState, group: ResourceGroup) {
  const kinds = RESOURCE_GROUPS[group].kinds
  const present = kinds.map(kind => state.resources[kind]).filter(progress => progress !== undefined)
  const unavailable = state.unavailable.filter(item => (item.resource && kinds.includes(item.resource)) || (group === 'maps' && item.layer === 'openfreemap'))
  return {
    done: present.reduce((sum, progress) => sum + progress.done, 0),
    total: present.reduce((sum, progress) => sum + progress.total, 0),
    failed: present.reduce((sum, progress) => sum + progress.failed, 0),
    bytes: present.reduce((sum, progress) => sum + progress.bytes, 0),
    transfer: group === 'maps' ? resourceTransferText(state.resources['vector-map']) : null,
    unavailable,
    allUnavailable: present.length === 0 && unavailable.length > 0,
  }
}

type Tab = 'downloads' | 'browse'
type Mode = 'cities' | 'regions' | 'countries'
interface Detail { area: DownloadArea | null; record: DownloadRegion | null }

function normalized(value: string) {
  return value.normalize('NFD').replace(/\p{Diacritic}/gu, '').toLowerCase()
}

function percent(done: number | undefined, total: number | undefined): number | null {
  return total ? Math.min(100, Math.round(((done ?? 0) / total) * 100)) : null
}

function packDate(pack: PackSummary): string {
  const date = new Date(pack.updatedAt ?? pack.createdAt ?? '')
  return Number.isFinite(date.getTime()) ? date.toLocaleDateString() : ''
}

function packState(pack: PackSummary): { label: string; tone: 'ready' | 'partial' | 'failed' } {
  if (pack.status === 'complete' && !pack.incomplete) return { label: 'Downloaded', tone: 'ready' }
  if (pack.detail === 'cancelled') return { label: 'Stopped', tone: 'partial' }
  if (pack.status === 'failed') return { label: 'Failed', tone: 'failed' }
  return { label: 'Partial', tone: 'partial' }
}

function stageLabel(job: RoutingDataJob | undefined): string {
  return routingJobProgress(job).label
}

function Icon({ name }: { name: 'download' | 'check' | 'close' | 'back' | 'forward' | 'search' | 'trash' | 'route' | 'map' | 'stop' }) {
  const path = {
    download: 'M12 4v11m0 0-4.5-4.5M12 15l4.5-4.5M5 19h14',
    check: 'm5 12.5 4.2 4.2L19 7',
    close: 'M6 6l12 12M18 6 6 18',
    back: 'M15 5l-7 7 7 7',
    forward: 'M9 5l7 7-7 7',
    search: 'm20 20-4.2-4.2m1.7-5a6.7 6.7 0 1 1-13.4 0 6.7 6.7 0 0 1 13.4 0Z',
    trash: 'M4 7h16M9 7V4h6v3m3 0-1 13H7L6 7m4 4v5m4-5v5',
    route: 'M6 19a2 2 0 1 0 0-4 2 2 0 0 0 0 4Zm12-10a2 2 0 1 0 0-4 2 2 0 0 0 0 4ZM8 17h7a3 3 0 0 0 0-6H9a3 3 0 0 1 0-6h7',
    map: 'M3 6.5 8 4l8 3 5-2.5v13L16 20l-8-3-5 2.5v-13ZM8 4v13m8-10v13',
    stop: 'M7 7h10v10H7z',
  }[name]
  return (
    <svg viewBox="0 0 24 24" aria-hidden="true" className="offline-regions-icon">
      <path d={path} />
    </svg>
  )
}

/** Toolbar entry point; doubles as the download status indicator. */
export function OfflineRegionsButton({
  routing,
  downloads,
  open,
  onOpen,
}: {
  routing: RoutingDataStatus | null
  downloads: RegionDownloads
  open: boolean
  onOpen: () => void
}) {
  const { activePacks, routingBusy } = downloads
  const done = activePacks.reduce((sum, pack) => sum + (pack.done ?? 0), 0)
  const total = activePacks.reduce((sum, pack) => sum + (pack.total ?? 0), 0)
  const mapPercent = percent(done, total)
  const failed = routing?.job?.state === 'failed'
  const label = activePacks.length
    ? `Downloading${mapPercent === null ? '…' : ` ${mapPercent}%`}`
    : routingBusy
      ? `Routing · ${stageLabel(routing?.job)}`
      : 'Offline regions'
  const detail = routing?.upgradePending
    ? 'Routing update pending'
    : failed
      ? 'Routing download failed'
      : routing?.ready
        ? `Routing ready${routing.name ? ` · ${routing.name}` : ''}`
        : 'No routing region in use'
  const tone = downloads.downloading ? 'busy' : failed || routing?.upgradePending ? 'warn' : routing?.ready ? 'ready' : 'idle'
  return (
    <button
      type="button"
      className={`offline-regions-button is-${tone}`}
      onClick={onOpen}
      aria-haspopup="dialog"
      aria-expanded={open}
      title={`Offline regions · ${detail}`}
      data-testid="offline-regions-button"
    >
      {downloads.downloading ? <span className="offline-regions-spinner" aria-hidden="true" /> : <Icon name="download" />}
      <span className="offline-regions-label">{label}</span>
      <i className="offline-regions-dot" aria-hidden="true" />
    </button>
  )
}

export function OfflineRegionsDialog({
  runtime,
  routing,
  downloads,
  nominatimApi,
  onClose,
  onNotify,
}: {
  runtime: RuntimeConfig
  routing: RoutingDataStatus | null
  downloads: RegionDownloads
  nominatimApi: string
  onClose: () => void
  onNotify: (message: string, type?: 'info' | 'success' | 'error') => void
}) {
  const [tab, setTab] = useState<Tab>(() => (downloads.downloading || routing?.cached.length || downloads.packs.length ? 'downloads' : 'browse'))
  const [regions, setRegions] = useState<DownloadRegion[]>([])
  const [loading, setLoading] = useState(Boolean(runtime.offline?.routing))
  const [catalogMessage, setCatalogMessage] = useState('')
  const [mode, setMode] = useState<Mode>('regions')
  const [country, setCountry] = useState('')
  const [query, setQuery] = useState('')
  const [places, setPlaces] = useState<PlaceResult[]>([])
  const [searching, setSearching] = useState(false)
  const [detail, setDetail] = useState<Detail | null>(null)
  const [trail, setTrail] = useState<Detail[]>([])
  const [confirmRemove, setConfirmRemove] = useState<string | null>(null)
  const [routingBytes, setRoutingBytes] = useState<{ bytes: number; stale: boolean } | null>(null)
  const offline = runtime.offline?.mode === 'cache-only'
  const routingAvailable = downloads.available

  const back = () => {
    if (confirmRemove) setConfirmRemove(null)
    else if (trail.length) {
      setDetail(trail[trail.length - 1])
      setTrail(trail.slice(0, -1))
    } else if (detail) setDetail(null)
    else onClose()
  }
  useEscapeDismiss(true, back, ESCAPE_PRIORITY.modal)

  useEffect(() => {
    if (!runtime.offline?.routing) return
    const controller = new AbortController()
    void fetchRoutingRegions(runtime, controller.signal)
      .then(result => {
        setRegions(result.regions)
        if (result.cachedOnly) setCatalogMessage('Showing downloaded regions only. Go online once to load the full catalogue.')
        const active = result.regions.find(region => region.active)
        const parent = active?.parent && result.regions.find(region => region.id === active.parent && region.kind === 'country')
        if (parent) setCountry(parent.id)
      })
      .catch(reason => {
        if (!controller.signal.aborted) setCatalogMessage((reason as Error).message)
      })
      .finally(() => {
        if (!controller.signal.aborted) setLoading(false)
      })
    return () => controller.abort()
    // The catalogue is loaded once per opening; the runtime mode toggle does not change it.
  }, [runtime.offline?.routing])

  useEffect(() => {
    // Explicit storage inspection, once per opening — never from a polling loop.
    const controller = new AbortController()
    void fetchRoutingSummary(runtime, controller.signal)
      .then(summary => {
        if (summary?.summaryKnown && !controller.signal.aborted) {
          setRoutingBytes({ bytes: summary.summaryBytes ?? 0, stale: summary.summaryStale === true })
        }
      })
      .catch(() => { /* Storage totals are advisory. */ })
    return () => controller.abort()
  }, [])

  const installed = useMemo(
    () => new Set(routing?.cached.filter(region => region.selected).map(region => region.regionId) ?? []),
    [routing?.cached],
  )
  const catalogue = useMemo(() => {
    const merged = new Map(regions.map(region => [region.id, region]))
    for (const region of routing?.cached ?? []) {
      if (region.selected && !merged.has(region.regionId)) {
        merged.set(region.regionId, {
          id: region.regionId,
          name: region.name,
          kind: 'region',
          installed: true,
          active: region.regionId === routing?.regionId,
        })
      }
    }
    return [...merged.values()].sort((a, b) => a.name.localeCompare(b.name))
  }, [regions, routing?.cached, routing?.regionId])
  const byID = useMemo(() => new Map(catalogue.map(region => [region.id, region])), [catalogue])
  const countries = catalogue.filter(region => region.kind === 'country')
  const downloadedRegions = catalogue.filter(region => installed.has(region.id) || region.installed)
  const regionName = (id: string | undefined) => (id && (byID.get(id)?.name ?? routing?.cached.find(r => r.regionId === id)?.name)) || id || 'Routing region'
  const countryOf = (region: DownloadRegion) => {
    let current: DownloadRegion | undefined = region
    const seen = new Set<string>()
    while (current && !seen.has(current.id)) {
      if (current.kind === 'country') return current.id
      seen.add(current.id)
      current = byID.get(current.parent ?? '')
    }
    return ''
  }

  const openDetail = (next: Detail) => {
    if (detail) setTrail(previous => [...previous, detail])
    setDetail(next)
  }
  const chooseRegion = (region: DownloadRegion, download = false) => {
    const area = areaForRegion(region, catalogue)
    openDetail({ area, record: region })
    if (download && area) void downloads.start(area)
  }
  const findCities = async (event: FormEvent) => {
    event.preventDefault()
    if (mode !== 'cities' || query.trim().length < 2) return
    setSearching(true)
    try {
      setPlaces(await searchPlaces(query, undefined, nominatimApi, { runtime }))
    } catch (reason) {
      onNotify((reason as Error).message, 'error')
    } finally {
      setSearching(false)
    }
  }
  const chooseCity = (place: PlaceResult) => {
    if (!place.bounds) {
      onNotify('This result has no area boundary. Choose a city or town result.', 'info')
      return
    }
    openDetail({
      record: null,
      area: { id: `city:${place.place_id}`, name: place.display_name.split(',')[0], kind: 'city', bounds: place.bounds },
    })
  }
  const packArea = (pack: PackSummary): DownloadArea | null => pack.bbox
    ? { id: `pack:${pack.id}`, name: packAreaName(pack), kind: pack.coverageKind ?? 'area', bounds: pack.bbox }
    : null

  // Most countries are a single extract with no catalogue regions: offer the
  // country itself and its map areas instead of an empty list.
  const unsplitCountry = mode === 'regions' && country && !catalogue.some(region => region.parent === country)
    ? byID.get(country)
    : undefined
  const unsplitArea = unsplitCountry ? areaForRegion(unsplitCountry, catalogue) : null
  const unsplitAreas = unsplitArea ? gridParts(unsplitArea) : []
  const filtered = catalogue.filter(region =>
    region.kind === (mode === 'countries' ? 'country' : 'region') &&
    (!country || mode === 'countries' || countryOf(region) === country) &&
    normalized(region.name).includes(normalized(query)),
  )

  const partRow = (part: DownloadArea) => {
    const state = areaDownloadState(downloads.packs, part)
    const status = state.complete ? 'Downloaded' : state.active ? 'Downloading…' : state.covers ? 'Partial download' : boundsLabel(part.bounds)
    return (
      <li className="offline-regions-row" key={part.id}>
        <button type="button" className="offline-regions-row-main" onClick={() => openDetail({ area: part, record: null })}>
          <span className={`offline-regions-symbol${state.complete ? ' is-ready' : state.active ? ' is-busy' : ''}`}><Icon name={state.complete ? 'check' : 'map'} /></span>
          <span className="offline-regions-row-text">
            <strong>{part.name}</strong>
            <small>{status}</small>
          </span>
        </button>
        <button
          type="button"
          className="offline-regions-icon-button"
          aria-label={state.complete ? `Details for ${part.name}` : `Download ${part.name}`}
          title={state.complete ? `Details for ${part.name}` : `Download ${part.name}`}
          disabled={!state.complete && (downloads.preparing || offline || !routingAvailable)}
          onClick={() => {
            openDetail({ area: part, record: null })
            if (!state.complete) void downloads.start(part)
          }}
        >
          <Icon name={state.complete ? 'forward' : 'download'} />
        </button>
      </li>
    )
  }

  const regionRow = (region: DownloadRegion) => {
    const downloaded = installed.has(region.id) || region.installed
    const mapped = mapsComplete(downloads.packs, areaForRegion(region, catalogue))
    const inUse = routing?.ready && routing.regionId === region.id
    const working = routing?.job?.regionId === region.id && downloads.routingBusy
    const parentName = byID.get(region.parent ?? '')?.name
    return (
      <li className="offline-regions-row" key={region.id}>
        <button type="button" className="offline-regions-row-main" onClick={() => chooseRegion(region)}>
          <span className={`offline-regions-symbol${downloaded ? ' is-ready' : ''}`}><Icon name={downloaded ? 'check' : 'map'} /></span>
          <span className="offline-regions-row-text">
            <strong>{region.name}</strong>
            <small>
              {working
                ? 'Downloading routing…'
                : downloaded
                  ? mapped ? 'Routing and maps downloaded' : 'Routing downloaded · maps not complete'
                  : parentName || 'Available to download'}
            </small>
          </span>
          {inUse && <span className="offline-regions-badge">In use</span>}
        </button>
        <button
          type="button"
          className="offline-regions-icon-button"
          aria-label={downloaded ? `Details for ${region.name}` : `Download ${region.name}`}
          title={downloaded ? `Details for ${region.name}` : `Download ${region.name}`}
          disabled={!downloaded && (downloads.preparing || downloads.downloading || offline || !region.bbox)}
          onClick={() => chooseRegion(region, !downloaded)}
        >
          <Icon name={downloaded ? 'forward' : 'download'} />
        </button>
      </li>
    )
  }

  const routingJob = routing?.job
  const routingSteps = routingJobProgress(routingJob)
  const routingPercent = routingSteps.overall === null ? null : Math.round(routingSteps.overall * 100)
  const stageIndex = ROUTING_STAGES.findIndex(stage => stage.id === routingJob?.phase)
  const routingProgress = downloads.routingBusy && routingJob && (
    <article className="offline-regions-card" aria-label={`Routing download ${regionName(routingJob.regionId)}`}>
      <header>
        <span className="offline-regions-symbol is-busy"><Icon name="route" /></span>
        <span className="offline-regions-row-text">
          <strong>{regionName(routingJob.regionId)}</strong>
          <small>{routingJob.upgrading ? 'Updating routing data for this version of Overland' : 'Routing data'}</small>
        </span>
        <button
          type="button"
          className="btn btn-ghost btn-xs"
          disabled={downloads.busyPack !== null}
          onClick={() => void downloads.stopRouting()}
        >
          {routingJob.upgrading ? 'Pause update' : 'Stop'}
        </button>
      </header>
      <ol className="offline-regions-stages">
        {ROUTING_STAGES.map((stage, index) => (
          <li key={stage.id} className={index < stageIndex ? 'is-done' : index === stageIndex ? 'is-current' : ''}>{stage.label}</li>
        ))}
      </ol>
      <progress max={100} value={routingPercent ?? undefined} aria-label="Routing download progress" />
      {routingSteps.step > 0 && <small className="offline-regions-step">Step {routingSteps.step} of {routingSteps.steps} · {routingSteps.label}{routingPercent !== null ? ` · ${routingPercent}% overall` : ''}</small>}
      <small role="status">
        {routingJob.retrying
          ? `Retrying · attempt ${routingJob.attempt ?? 1}${routingJob.retrySeconds ? ` in ${Math.ceil(routingJob.retrySeconds)}s` : ''}`
          : routingJob.phase === 'elevation'
            ? `${routingJob.completedItems ?? 0}${routingJob.itemsTotal ? ` of ${routingJob.itemsTotal}` : ''} terrain tiles · ${routingJob.itemsDownloaded ?? 0} downloaded · ${routingJob.itemsReused ?? 0} reused`
            : routingJob.phase === 'pbf'
              ? `${formatBytes(routingJob.done ?? 0)}${routingJob.total ? ` of ${formatBytes(routingJob.total)}` : ''}`
              : `${stageLabel(routingJob)}…`}
      </small>
      {routingJob.upgrading && routing?.ready && <small>Your downloaded region stays available while it rebuilds.</small>}
    </article>
  )

  const packProgress = (pack: PackSummary) => {
    const value = percent(pack.done, pack.total)
    const name = packAreaName(pack)
    return (
      <article className="offline-regions-card" key={pack.id} aria-label={`Map download ${name}`}>
        <header>
          <span className="offline-regions-symbol is-busy"><Icon name="map" /></span>
          <span className="offline-regions-row-text">
            <strong>{name}</strong>
            <small>
              {pack.status === 'queued' ? 'Queued' : `${(pack.done ?? 0).toLocaleString()} / ${(pack.total ?? 0).toLocaleString()} resources`}
              {pack.batchesTotal && pack.batchesTotal > 1 ? ` · batch ${Math.min((pack.batchesDone ?? 0) + 1, pack.batchesTotal)} of ${pack.batchesTotal}` : ''}
            </small>
          </span>
          <button
            type="button"
            className="btn btn-ghost btn-xs"
            disabled={downloads.busyPack !== null || pack.status === 'cancelling'}
            onClick={() => void downloads.stopPack(pack.id)}
          >
            {pack.status === 'cancelling' ? 'Stopping…' : 'Stop'}
          </button>
        </header>
        <progress max={100} value={value ?? undefined} aria-label={`${name} progress`} />
        <ul className="offline-regions-resources is-compact">
          {Object.entries(RESOURCE_LABELS).map(([kind, label]) => {
            const progress = pack.resources[kind]
            if (!progress?.total) return null
            return (
              <li key={kind}>
                <span>{label}</span>
                <strong>{progress.failed ? `${progress.failed} missing` : `${progress.done.toLocaleString()} / ${progress.total.toLocaleString()}`}</strong>
              </li>
            )
          })}
        </ul>
      </article>
    )
  }

  const storedPacks = downloads.packs.filter(pack => !packIsActive(pack))
  const waitingPacks = downloads.activePacks.filter(pack => pack.status === 'queued')
  const packBytes = downloads.packs.reduce((sum, pack) => sum + (pack.bytes ?? 0), 0)
  const downloadsView = (
    <div className="offline-regions-scroll">
      {routing?.upgradePending && !downloads.routingBusy && (
        <div className="offline-regions-notice">
          <p>
            {offline
              ? 'Your downloaded region is available. Its routing update will resume when you go online.'
              : 'Your downloaded region is available. Finish its routing update to refresh road-access connectivity.'}
          </p>
          {!offline && routing.regionId && (
            <button type="button" className="btn btn-primary btn-sm" disabled={downloads.busyPack !== null} onClick={() => void downloads.useRegion(routing.regionId!)}>
              Resume routing update
            </button>
          )}
        </div>
      )}
      {routingJob?.state === 'failed' && (
        <p className="offline-regions-error" role="alert">
          {regionName(routingJob.regionId)}: {routing?.error || routingJob.detail || 'Routing download failed.'}
        </p>
      )}
      {(downloads.routingBusy || downloads.activePacks.length > 0 || downloads.preparing) && (
        <section className="offline-regions-section" aria-label="Downloading">
          <div className="offline-regions-heading">
            <h3>Downloading</h3>
            {downloads.downloading && (
              <button type="button" className="btn btn-ghost btn-xs" disabled={downloads.busyPack !== null} onClick={() => void downloads.stopAll()}>
                Stop all
              </button>
            )}
          </div>
          {downloads.preparing && <p className="offline-regions-working" role="status"><span className="offline-regions-spinner" />{downloads.phase || 'Preparing…'}</p>}
          {routingProgress}
          {downloads.activePacks.filter(pack => pack.status !== 'queued').map(packProgress)}
          {waitingPacks.length > 0 && (
            <article className="offline-regions-card" aria-label="Queued map downloads">
              <header>
                <span className="offline-regions-symbol"><Icon name="map" /></span>
                <span className="offline-regions-row-text">
                  <strong>Queued</strong>
                  <small>{waitingPacks.length} map download{waitingPacks.length === 1 ? '' : 's'} · each starts when a running one finishes</small>
                </span>
              </header>
              <ul className="offline-regions-queue">
                {waitingPacks.map(pack => (
                  <li key={pack.id}>
                    <span>{packAreaName(pack)}</span>
                    <button
                      type="button"
                      className="offline-regions-icon-button"
                      aria-label={`Stop ${packAreaName(pack)}`}
                      title={`Stop ${packAreaName(pack)}`}
                      disabled={downloads.busyPack !== null}
                      onClick={() => void downloads.stopPack(pack.id)}
                    >
                      <Icon name="close" />
                    </button>
                  </li>
                ))}
              </ul>
            </article>
          )}
        </section>
      )}

      <section className="offline-regions-section" aria-label="Downloaded regions">
        <div className="offline-regions-heading">
          <h3>Routing regions</h3>
          <span>{downloadedRegions.length}</span>
        </div>
        {downloadedRegions.length > 0
          ? <ul className="offline-regions-list">{downloadedRegions.map(regionRow)}</ul>
          : <p className="offline-regions-muted">No routing region downloaded yet.</p>}
      </section>

      <section className="offline-regions-section" aria-label="Map downloads">
        <div className="offline-regions-heading">
          <h3>Map downloads</h3>
          <span>{storedPacks.length}</span>
        </div>
        {downloads.packsError && <p className="offline-regions-error" role="alert">{downloads.packsError}</p>}
        {storedPacks.length === 0 && <p className="offline-regions-muted">No maps stored yet.</p>}
        <ul className="offline-regions-list">
          {storedPacks.map(pack => {
            const name = packAreaName(pack)
            const state = packState(pack)
            const area = packArea(pack)
            // Resume only what this pack holds; routing is managed separately.
            const packResources = packGroups(pack)
            const resumable = state.tone !== 'ready' && area && packResources.length > 0 && !pack.name?.startsWith('Route:')
            return (
              <li className="offline-regions-row" key={pack.id}>
                <button
                  type="button"
                  className="offline-regions-row-main"
                  disabled={!area}
                  onClick={() => area && openDetail({ area, record: null })}
                >
                  <span className={`offline-regions-symbol is-${state.tone}`}><Icon name={state.tone === 'ready' ? 'check' : 'map'} /></span>
                  <span className="offline-regions-row-text">
                    <strong>{name}</strong>
                    <small>{[coverageLabel(pack.coverageKind), formatBytes(pack.bytes), packDate(pack)].filter(Boolean).join(' · ')}</small>
                  </span>
                  <span className={`offline-regions-badge is-${state.tone}`}>{state.label}</span>
                </button>
                {confirmRemove === pack.id ? (
                  <span className="offline-regions-confirm">
                    <button type="button" className="btn btn-danger btn-xs" disabled={downloads.busyPack !== null} onClick={() => { setConfirmRemove(null); void downloads.removePack(pack.id) }}>
                      Remove
                    </button>
                    <button type="button" className="btn btn-ghost btn-xs" onClick={() => setConfirmRemove(null)}>Keep</button>
                  </span>
                ) : (
                  <span className="offline-regions-actions">
                    {resumable && (
                      <button
                        type="button"
                        className="offline-regions-icon-button"
                        aria-label={`Resume ${name}`}
                        title={`Resume ${name}`}
                        disabled={downloads.preparing || downloads.downloading || offline || !routingAvailable}
                        onClick={() => { openDetail({ area, record: null }); void downloads.start(area, packResources) }}
                      >
                        <Icon name="download" />
                      </button>
                    )}
                    <button
                      type="button"
                      className="offline-regions-icon-button is-danger"
                      aria-label={`Remove ${name}`}
                      title={`Remove ${name}`}
                      disabled={downloads.busyPack !== null}
                      onClick={() => setConfirmRemove(pack.id)}
                    >
                      <Icon name="trash" />
                    </button>
                  </span>
                )}
              </li>
            )
          })}
        </ul>
      </section>

      <p className="offline-regions-storage">
        {formatBytes(packBytes)} of maps
        {routingBytes && <> · {formatBytes(routingBytes.bytes)} of routing data{routingBytes.stale ? ' (last measured)' : ''}</>}
      </p>
      {downloadedRegions.length === 0 && storedPacks.length === 0 && !downloads.downloading && (
        <button type="button" className="btn btn-primary btn-sm offline-regions-cta" onClick={() => setTab('browse')}>
          Browse regions to download
        </button>
      )}
    </div>
  )

  const browseView = (
    <>
      <div className="offline-regions-controls">
        <div className="offline-regions-segments" role="group" aria-label="Area type">
          {(['cities', 'regions', 'countries'] as const).map(item => (
            <button
              type="button"
              key={item}
              className={mode === item ? 'is-selected' : ''}
              aria-pressed={mode === item}
              onClick={() => { setMode(item); setQuery(''); setPlaces([]) }}
            >
              {item === 'cities' ? 'City' : item === 'regions' ? 'Region' : 'Country'}
            </button>
          ))}
        </div>
        <form className="offline-regions-search" onSubmit={event => void findCities(event)} role="search">
          <Icon name="search" />
          <input
            aria-label={mode === 'cities' ? 'Search city' : 'Filter regions'}
            placeholder={mode === 'cities' ? 'Search a city or town' : 'Find a region'}
            value={query}
            onChange={event => setQuery(event.target.value)}
          />
          {mode === 'cities' && <button type="submit" className="btn btn-primary btn-xs" disabled={searching || query.trim().length < 2}>Search</button>}
        </form>
        {mode === 'regions' && countries.length > 0 && (
          <select aria-label="Country filter" value={country} onChange={event => setCountry(event.target.value)}>
            <option value="">All countries</option>
            {countries.map(item => <option key={item.id} value={item.id}>{item.name}</option>)}
          </select>
        )}
      </div>
      <div className="offline-regions-scroll">
        {!routingAvailable && <p className="offline-regions-error">Local routing is disabled on this server, so regions cannot be downloaded.</p>}
        {loading && <p className="offline-regions-working"><span className="offline-regions-spinner" />Loading regions…</p>}
        {catalogMessage && <p className="offline-regions-muted">{catalogMessage}</p>}
        {mode === 'cities' ? (
          <>
            {searching && <p className="offline-regions-working"><span className="offline-regions-spinner" />Finding cities…</p>}
            <ul className="offline-regions-list">
              {places.map(place => (
                <li className="offline-regions-row" key={place.place_id}>
                  <button type="button" className="offline-regions-row-main" onClick={() => chooseCity(place)}>
                    <span className="offline-regions-symbol"><Icon name="map" /></span>
                    <span className="offline-regions-row-text">
                      <strong>{place.display_name.split(',')[0]}</strong>
                      <small>{place.display_name}</small>
                    </span>
                  </button>
                </li>
              ))}
            </ul>
            {!places.length && !searching && (
              <p className="offline-regions-muted">Search for a city to download its map area and the routing region that covers it.</p>
            )}
          </>
        ) : (
          <>
            <div className="offline-regions-heading">
              <h3>{mode === 'countries' ? 'Countries' : country ? `${byID.get(country)?.name} · regions` : 'Regions'}</h3>
              <span>{filtered.length}</span>
            </div>
            <ul className="offline-regions-list">{filtered.map(regionRow)}</ul>
            {!loading && !filtered.length && (unsplitCountry && !query ? (
              <>
                <p className="offline-regions-muted">
                  The routing catalogue does not divide {unsplitCountry.name} into regions; only some countries are.
                  Download the whole country{unsplitAreas.length ? ', or just the map areas you need' : ''}. Routing
                  always uses the {unsplitCountry.name} extract.
                </p>
                <ul className="offline-regions-list">{regionRow(unsplitCountry)}</ul>
                {unsplitAreas.length > 0 && (
                  <>
                    <div className="offline-regions-heading"><h3>{unsplitCountry.name} · areas</h3><span>{unsplitAreas.length}</span></div>
                    <ul className="offline-regions-list" aria-label="Areas">{unsplitAreas.map(partRow)}</ul>
                  </>
                )}
              </>
            ) : <p className="offline-regions-muted">No matching regions.</p>)}
          </>
        )}
      </div>
    </>
  )

  let detailView = null
  if (detail) {
    const { area: selected, record } = detail
    const target = downloads.target
    const regionID = selected?.regionId || record?.id || (target?.area.id === selected?.id ? target?.region : undefined)
    const regionInstalled = regionID ? installed.has(regionID) || record?.installed === true : false
    const job = regionID && routing?.job?.regionId === regionID ? routing.job : undefined
    const regionRunning = job?.state === 'running' || job?.state === 'queued'
    const matchingTarget = target?.area.id === selected?.id ? target : undefined
    // Rebuilt from the catalogue so a country knows its sub-regions.
    const detailArea = (record && areaForRegion(record, catalogue)) || selected
    const groups = Object.fromEntries(PACK_GROUPS.map(group => [group, areaDownloadState(downloads.packs, detailArea, group)])) as Record<ResourceGroup, AreaDownloadState>
    const maps = areaDownloadState(downloads.packs, detailArea)
    const hasMaps = maps.packs.length > 0
    const finishedParts = maps.packs.filter(pack => pack.status === 'complete').length
    const otherRegionPreparing = downloads.routingBusy && !regionRunning
    const missing: DownloadResource[] = [
      ...(regionInstalled || regionRunning || !routingAvailable ? [] : ['routing' as const]),
      ...PACK_GROUPS.filter(group => !groups[group].complete && !groups[group].active),
    ]
    const nothingYet = !regionInstalled && !regionRunning && PACK_GROUPS.every(group => !groups[group].packs.length)
    const failedPack = PACK_GROUPS.map(group => groups[group]).find(state => state.failed && !state.active)?.failed
    const children = record ? catalogue.filter(region => region.parent === record.id) : []
    // No catalogue regions to offer: a too-large area splits into a grid.
    const areaParts = detailArea && children.length === 0 ? gridParts(detailArea) : []
    detailView = (
      <div className="offline-regions-scroll">
        <div className="offline-regions-summary">
          <strong>{regionInstalled ? 'Routing downloaded' : selected?.kind === 'city' ? 'City maps & local routing' : 'Maps & routing'}</strong>
          <small>
            {regionInstalled
              ? routing?.regionId === regionID ? 'In use for routing' : 'Available on this server'
              : 'Download for offline use'}
          </small>
          {selected && <small>{coverageLabel(selected.kind)} · {boundsLabel(selected.bounds)}</small>}
        </div>
        {hasMaps && !maps.covers && <p className="offline-regions-muted">The saved map downloads cover only part of this area.</p>}
        {maps.packs.length > 1 && (
          <p className="offline-regions-muted">
            Maps are split into {maps.packs.length} downloads to stay within the per-download limit · {finishedParts} finished.
          </p>
        )}
        {selected?.kind === 'city' && <p className="offline-regions-muted">Maps cover the city. Routing uses the provider's regional extract that contains it.</p>}
        {(children.length > 0 || areaParts.length > 0) && !hasMaps && (
          <p className="offline-regions-muted">
            Too large for one download, so its maps download {children.length > 0 ? 'region by region' : `as ${areaParts.length} areas`}, queued one after another. Routing uses the single extract, so routes still cross between them. Download it all, or pick {children.length > 0 ? 'regions' : 'areas'} below.
          </p>
        )}
        {matchingTarget && downloads.preparing && (
          <p className="offline-regions-working" role="status"><span className="offline-regions-spinner" />{downloads.phase}</p>
        )}
        <ul className="offline-regions-resources" aria-label="Resources">
          <li>
            <span className="offline-regions-resource-name"><Icon name="route" />Routing</span>
            <strong className={regionInstalled && !regionRunning ? 'is-ready' : ''}>
              {regionRunning
                ? routingJobProgress(job).step ? `Step ${routingJobProgress(job).step}/${routingJobProgress(job).steps} · ${stageLabel(job)}` : stageLabel(job)
                : regionInstalled ? 'Downloaded' : regionID ? 'Not downloaded' : 'Not checked yet'}
            </strong>
            <span className="offline-regions-resource-action">
              {regionRunning ? (
                <button type="button" className="offline-regions-icon-button" aria-label="Stop routing download" title="Stop routing download" disabled={downloads.busyPack !== null} onClick={() => void downloads.stopRouting()}>
                  <Icon name="stop" />
                </button>
              ) : !regionInstalled && (
                <button
                  type="button"
                  className="offline-regions-icon-button"
                  aria-label="Download routing"
                  title={otherRegionPreparing ? 'Another region is preparing routing' : 'Download routing'}
                  disabled={!detailArea || downloads.preparing || offline || !routingAvailable || otherRegionPreparing}
                  onClick={() => detailArea && void downloads.start(detailArea, ['routing'])}
                >
                  <Icon name="download" />
                </button>
              )}
            </span>
            <small>Needed to plan routes, online or offline{regionInstalled && regionID !== routing?.regionId ? ' · downloaded, not in use' : ''}</small>
            {regionRunning && <progress max={100} value={routingJobProgress(job).overall === null ? undefined : Math.round(routingJobProgress(job).overall! * 100)} aria-label="Routing progress" />}
          </li>
          {PACK_GROUPS.map(group => {
            const state = groups[group]
            const { label } = RESOURCE_GROUPS[group]
            const progress = groupProgress(state, group)
            const done = state.complete
            return (
              <li key={group}>
                <span className="offline-regions-resource-name"><Icon name={done ? 'check' : 'map'} />{label}</span>
                <strong className={done ? 'is-ready' : progress.failed ? 'is-failed' : ''}>
                  {progress.allUnavailable
                    ? 'Provider limit'
                    : done
                      ? 'Downloaded'
                      : state.active
                        ? state.packs.every(pack => pack.status === 'queued' || !packIsActive(pack))
                          ? 'Queued'
                          : `${progress.done.toLocaleString()} / ${progress.total.toLocaleString()}`
                        : progress.failed
                          ? `${progress.failed} missing`
                          : state.packs.length && !state.covers ? 'Partial area' : state.packs.length ? 'Incomplete' : 'Not downloaded'}
                </strong>
                <span className="offline-regions-resource-action">
                  {!done && !state.active && (
                    <button
                      type="button"
                      className="offline-regions-icon-button"
                      aria-label={`Download ${label.toLowerCase()}`}
                      title={`Download ${label.toLowerCase()}`}
                      disabled={!detailArea || downloads.preparing || offline}
                      onClick={() => detailArea && void downloads.start(detailArea, [group])}
                    >
                      <Icon name="download" />
                    </button>
                  )}
                </span>
                <small>
                  {[
                    GROUP_HINTS[group],
                    progress.transfer,
                    progress.bytes > 0 ? `${formatBytes(progress.bytes)} stored` : '',
                    // Every part of a large area reports the same limit; say it once.
                    ...new Set(progress.unavailable.map(item => item.reason)),
                  ].filter(Boolean).join(' · ')}
                </small>
                {state.active && <progress max={progress.total || 1} value={progress.done} aria-label={`${label} progress`} />}
              </li>
            )
          })}
        </ul>
        {failedPack && <p className="offline-regions-error" role="alert">{maps.packs.length > 1 ? `${packAreaName(failedPack)}: ` : ''}{packFailure(failedPack)}</p>}
        {matchingTarget && downloads.error && <p className="offline-regions-error" role="alert">{downloads.error}</p>}
        {regionRunning && routing?.error && <p className="offline-regions-error" role="alert">{routing.error}</p>}
        <div className="offline-regions-detail-actions">
          <button
            type="button"
            className="btn btn-primary btn-sm"
            disabled={!detailArea || !missing.length || downloads.preparing || offline || (missing.includes('routing') && otherRegionPreparing)}
            onClick={() => detailArea && void downloads.start(detailArea, missing)}
          >
            <Icon name="download" />
            {downloads.preparing
              ? 'Preparing…'
              : !missing.length
                ? 'Everything downloaded'
                : nothingYet ? 'Download everything' : 'Download the rest'}
          </button>
          {downloads.downloading && (
            <button type="button" className="btn btn-ghost btn-sm" disabled={downloads.busyPack !== null} onClick={() => void downloads.stopAll()}>
              <Icon name="stop" />Stop downloads
            </button>
          )}
          {regionInstalled && regionID && regionID !== routing?.regionId && (
            <button type="button" className="btn btn-ghost btn-sm" disabled={downloads.preparing || downloads.routingBusy} onClick={() => void downloads.useRegion(regionID)}>
              Use for routing
            </button>
          )}
        </div>
        {offline && <p className="offline-regions-muted">Go online to download new resources. Downloaded regions keep working.</p>}
        {!selected && <p className="offline-regions-muted">Connect once to load this region's map boundaries.</p>}
        {children.length > 0 && (
          <>
            <div className="offline-regions-heading"><h3>Choose a smaller region</h3><span>{children.length}</span></div>
            <ul className="offline-regions-list">{children.map(regionRow)}</ul>
          </>
        )}
        {children.length === 0 && areaParts.length > 0 && (
          <>
            <div className="offline-regions-heading"><h3>Choose a smaller area</h3><span>{areaParts.length}</span></div>
            <ul className="offline-regions-list" aria-label="Areas">{areaParts.map(partRow)}</ul>
          </>
        )}
      </div>
    )
  }

  const title = detail ? detail.area?.name ?? detail.record?.name ?? 'Region' : 'Offline regions'
  return (
    <div className="offline-regions-backdrop" onMouseDown={event => { if (event.target === event.currentTarget) onClose() }}>
      <section className="offline-regions-dialog" role="dialog" aria-modal="true" aria-label="Offline regions" data-testid="offline-regions-dialog">
        <header className="offline-regions-header">
          {detail && (
            <button type="button" className="offline-regions-icon-button" aria-label="Back" title="Back" onClick={back}>
              <Icon name="back" />
            </button>
          )}
          <div>
            <small>{detail ? 'OFFLINE REGION' : 'MAPS · ROUTING · ELEVATION · POIS'}</small>
            <h2>{title}</h2>
          </div>
          <button type="button" className="offline-regions-icon-button" aria-label="Close offline regions" title="Close" onClick={onClose}>
            <Icon name="close" />
          </button>
        </header>
        {!detail && (
          <div className="offline-regions-tabs" role="tablist" aria-label="Offline regions views">
            <button type="button" role="tab" aria-selected={tab === 'downloads'} className={tab === 'downloads' ? 'is-selected' : ''} onClick={() => setTab('downloads')}>
              Downloads
              {downloads.downloading && <span className="offline-regions-spinner" aria-label="Downloading" />}
            </button>
            <button type="button" role="tab" aria-selected={tab === 'browse'} className={tab === 'browse' ? 'is-selected' : ''} onClick={() => setTab('browse')}>
              Add region
            </button>
          </div>
        )}
        {detail ? detailView : tab === 'downloads' ? downloadsView : browseView}
      </section>
    </div>
  )
}
