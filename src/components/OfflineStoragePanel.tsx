import { useEffect, useState } from 'react'
import type { CSSProperties } from 'react'
import type { Coordinate } from '../lib/types'
import {
  buildAutomaticPackRequest,
  estimatePack,
  fetchPacks,
  formatBytes,
  startPack,
} from '../lib/offline'
import type {
  PackEstimate,
  PackResourceProgress,
  PackSummary,
  RuntimeConfig,
} from '../lib/offline'
import { ESCAPE_PRIORITY, useEscapeDismiss } from './useEscapeDismiss'

const ACTIVE_STATES = new Set(['queued', 'running', 'cancelling'])

const RESOURCE_DEFINITIONS = [
  { id: 'vector-map', label: 'Vector map', hint: 'Route corridor and labels', tone: '#67e8f9' },
  { id: 'elevation', label: 'Terrain elevation', hint: 'Offline height model', tone: '#a3e635' },
  { id: 'fuel-stations', label: 'Fuel stations', hint: 'Stops near the route', tone: '#fb923c' },
  { id: 'water', label: 'Water points', hint: 'Drinking water nearby', tone: '#60a5fa' },
  { id: 'campsites', label: 'Campsites', hint: 'Camping along the way', tone: '#c084fc' },
  { id: 'fuel-prices', label: 'Fuel prices', hint: 'Latest station snapshot', tone: '#facc15' },
] as const

type ResourceID = typeof RESOURCE_DEFINITIONS[number]['id']
type ResourceState = 'scanning' | 'queued' | 'caching' | 'ready' | 'failed' | 'unavailable'

const EMPTY_PROGRESS: PackResourceProgress = { done: 0, total: 0, failed: 0, bytes: 0, items: 0 }

function estimateTotal(estimate: PackEstimate | null, resource: ResourceID): number {
  if (!estimate) return 0
  switch (resource) {
    case 'vector-map':
      return (estimate.counts['openfreemap-core'] ?? 0) +
        (estimate.counts['openfreemap-glyphs'] ?? 0) +
        (estimate.counts.openfreemap ?? 0) +
        (estimate.counts['openfreemap-raster'] ?? 0)
    case 'elevation': return estimate.counts.elevation ?? 0
    case 'fuel-stations': return estimate.counts['pois-fuel'] ?? 0
    case 'water': return estimate.counts['pois-water'] ?? 0
    case 'campsites': return estimate.counts['pois-camp'] ?? 0
    case 'fuel-prices': return estimate.counts.fuel ?? 0
  }
}

function blockedReason(estimate: PackEstimate | null, resource: ResourceID): string | undefined {
  return estimate?.blocked.find(blocked =>
    blocked.resource === resource || (resource === 'vector-map' && blocked.layer === 'openfreemap'),
  )?.reason
}

function resourceState(
  progress: PackResourceProgress,
  pack: PackSummary | null,
  estimate: PackEstimate | null,
  error: string,
  blocked: string | undefined,
): ResourceState {
  if (blocked) return 'unavailable'
  if (progress.failed > 0) return 'failed'
  if (progress.total > 0 && progress.done >= progress.total) return 'ready'
  if (error && !pack) return 'unavailable'
  if (!estimate && !pack) return 'scanning'
  if (!pack) return progress.total > 0 ? 'queued' : 'unavailable'
  if (pack.status === 'incomplete' || pack.status === 'cancelled') return 'failed'
  if (progress.done > 0) return 'caching'
  return ACTIVE_STATES.has(pack.status) ? 'queued' : 'unavailable'
}

function resourceDetail(
  resource: ResourceID,
  progress: PackResourceProgress,
  state: ResourceState,
  fallback: string,
  reason?: string,
): string {
  if (reason) return reason
  if (state === 'scanning') return 'Calculating route coverage'
  if (state === 'unavailable') return fallback
  if (state === 'failed') return progress.failed ? `${progress.failed} item${progress.failed === 1 ? '' : 's'} failed` : 'Caching interrupted'
  if (resource === 'fuel-stations' || resource === 'water' || resource === 'campsites') {
    if (state === 'ready') return progress.items > 0 ? `${progress.items} local records` : 'Search cached · none nearby'
    return state === 'queued' ? 'Waiting to search route' : 'Searching route area'
  }
  if (resource === 'fuel-prices' && state === 'ready' && progress.items > 0) {
    return `${progress.items.toLocaleString()} station prices`
  }
  const unit = resource === 'elevation' ? 'tiles' : resource === 'vector-map' ? 'files' : 'items'
  return `${progress.done.toLocaleString()} / ${progress.total.toLocaleString()} ${unit}`
}

function stateLabel(state: ResourceState): string {
  return {
    scanning: 'Scanning',
    queued: 'Queued',
    caching: 'Caching',
    ready: 'Ready',
    failed: 'Issue',
    unavailable: 'Unavailable',
  }[state]
}

function ResourceIcon({ id }: { id: ResourceID }) {
  const path = {
    'vector-map': 'M3 6.5 8 4l8 3 5-2.5v13L16 20l-8-3-5 2.5v-13ZM8 4v13m8-10v13',
    elevation: 'm3 18 5-10 4 6 3-5 6 9H3Z',
    'fuel-stations': 'M5 21V4h10v17M7 7h6v5H7V7Zm8 1h2l2 2v7a2 2 0 0 1-4 0v-3',
    water: 'M12 3s6 6.5 6 11a6 6 0 1 1-12 0c0-4.5 6-11 6-11Zm-3 12c.5 1.5 1.5 2.2 3 2.2',
    campsites: 'm4 20 8-16 8 16M7 14h10M9 20l3-6 3 6',
    'fuel-prices': 'M5 5h14v14H5V5Zm3 4h8m-8 4h5m-5 3h3',
  }[id]
  return (
    <svg viewBox="0 0 24 24" aria-hidden="true">
      <path d={path} fill="none" stroke="currentColor" strokeWidth="1.8" strokeLinecap="round" strokeLinejoin="round" />
    </svg>
  )
}

function ProgressRing({ progress, ready }: { progress: number; ready: boolean }) {
  return (
    <span className={`offline-progress-ring${ready ? ' is-ready' : ''}`} aria-hidden="true">
      <svg viewBox="0 0 36 36">
        <circle className="offline-progress-ring-track" cx="18" cy="18" r="15.5" pathLength="100" />
        <circle className="offline-progress-ring-value" cx="18" cy="18" r="15.5" pathLength="100" strokeDasharray={`${progress} 100`} />
      </svg>
      <span>{ready ? '✓' : `${progress}`}</span>
    </span>
  )
}

export function OfflineStoragePanel({
  runtime,
  routeKey,
  routeName,
  route,
}: {
  runtime: RuntimeConfig
  routeKey: number
  routeName: string
  route: Coordinate[]
}) {
  const [open, setOpen] = useState(false)
  const [estimate, setEstimate] = useState<PackEstimate | null>(null)
  const [pack, setPack] = useState<PackSummary | null>(null)
  const [error, setError] = useState('')
  const [attempt, setAttempt] = useState(0)
  const packID = pack?.id
  const packStatus = pack?.status
  useEscapeDismiss(open, () => setOpen(false), ESCAPE_PRIORITY.panel)

  useEffect(() => {
    const controller = new AbortController()
    let disposed = false
    setEstimate(null)
    setPack(null)
    setError('')

    const timer = window.setTimeout(() => {
      if (!runtime.offline) {
        setError('Offline service is not available')
        return
      }
      let request = buildAutomaticPackRequest(routeName, route)
      if (!request) {
        setError('This track has no route to cache')
        return
      }
      void (async () => {
        try {
          let elevationBlocked = ''
          let nextEstimate: PackEstimate | undefined
          while (!nextEstimate) {
            try {
              nextEstimate = await estimatePack(runtime, request, controller.signal)
            } catch (reason) {
              const message = (reason as Error).message
              if (request.scopes.includes('elevation') && /elevation pack|terrarium|tile cache/i.test(message)) {
                request = { ...request, scopes: request.scopes.filter(scope => scope !== 'elevation') }
                elevationBlocked = /require|configured/i.test(message)
                  ? 'Persistent terrain tiles are not configured'
                  : 'Route exceeds the offline elevation limit'
                continue
              }
              if (request.maxZoom > 10 && /pack exceeds \d+ resources/i.test(message)) {
                request = { ...request, maxZoom: request.maxZoom - 1 }
                continue
              }
              throw reason
            }
          }
          if (elevationBlocked) {
            nextEstimate = {
              ...nextEstimate,
              blocked: [...nextEstimate.blocked, { resource: 'elevation', reason: elevationBlocked }],
            }
          }
          if (disposed) return
          setEstimate(nextEstimate)
          const nextPack = await startPack(runtime, request, controller.signal)
          if (!disposed && nextPack) {
            setPack(nextPack)
          }
        } catch (reason) {
          if (!disposed && (reason as Error).name !== 'AbortError') setError((reason as Error).message)
        }
      })()
    }, 0)

    return () => {
      disposed = true
      controller.abort()
      window.clearTimeout(timer)
    }
  }, [attempt, route, routeKey, routeName, runtime])

  useEffect(() => {
    if (!runtime.offline || !packID || !packStatus || !ACTIVE_STATES.has(packStatus)) return
    const controller = new AbortController()
    let disposed = false
    const refresh = async () => {
      try {
        const current = (await fetchPacks(runtime, controller.signal)).find(candidate => candidate.id === packID)
        if (!disposed && current) setPack(current)
      } catch (reason) {
        if (!disposed && (reason as Error).name !== 'AbortError') setError((reason as Error).message)
      }
    }
    const timer = window.setInterval(() => void refresh(), 750)
    void refresh()
    return () => { disposed = true; controller.abort(); window.clearInterval(timer) }
  }, [packID, packStatus, runtime])

  const total = pack?.total ?? estimate?.resources ?? 0
  const done = pack?.done ?? 0
  const percent = total > 0 ? Math.min(100, Math.round(done / total * 100)) : 0
  const hasBlocked = Boolean(estimate?.blocked.length)
  const ready = pack?.status === 'complete' && (pack.failed ?? 0) === 0 && !hasBlocked
  const active = !ready && (!error && (!pack || ACTIVE_STATES.has(pack.status)))
  const partial = !ready && (Boolean(error) || pack?.status === 'incomplete' || hasBlocked)
  const buttonLabel = ready ? 'Offline ready' : partial ? 'Offline partial' : `Offline ${percent}%`
  const headline = ready
    ? 'Ready for dead zones'
    : partial
      ? 'Some resources need attention'
      : pack
        ? 'Caching this route'
        : 'Scanning this route'

  return (
    <div className={`offline-route-control${open ? ' is-open' : ''}`} data-testid="offline-route-control">
      <button
        className={`offline-route-fab${ready ? ' is-ready' : ''}${partial ? ' is-partial' : ''}`}
        onClick={() => setOpen(value => !value)}
        aria-expanded={open}
        aria-controls="offline-route-panel"
        title="Offline readiness for this route"
        data-testid="offline-route-button"
      >
        <ProgressRing progress={percent} ready={ready} />
        <span className="offline-route-fab-copy">
          <strong>{buttonLabel}</strong>
          <small>{ready ? 'Route prepared' : partial ? 'Open for details' : 'Preparing route'}</small>
        </span>
        <svg className="offline-route-chevron" viewBox="0 0 20 20" aria-hidden="true">
          <path d="m6 8 4 4 4-4" fill="none" stroke="currentColor" strokeWidth="1.8" strokeLinecap="round" strokeLinejoin="round" />
        </svg>
      </button>

      {open && (
        <section id="offline-route-panel" className="offline-route-panel" aria-label="Offline route readiness" data-testid="offline-route-panel">
          <header className="offline-route-header">
            <div>
              <span className="offline-route-kicker">Route readiness</span>
              <strong>{headline}</strong>
              <small title={routeName}>{routeName}</small>
            </div>
            <ProgressRing progress={percent} ready={ready} />
          </header>

          <div className="offline-route-overall">
            <span className={`offline-route-live${active ? ' is-active' : ''}`} />
            <span>{ready ? 'Everything essential is stored locally' : `${done.toLocaleString()} of ${total.toLocaleString()} resources cached`}</span>
            {pack?.bytes !== undefined && pack.bytes > 0 && <strong>{formatBytes(pack.bytes)}</strong>}
          </div>

          <ul className="offline-resource-list">
            {RESOURCE_DEFINITIONS.map(definition => {
              const estimated = estimateTotal(estimate, definition.id)
              const progress = pack?.resources[definition.id] ?? { ...EMPTY_PROGRESS, total: estimated }
              const reason = blockedReason(estimate, definition.id)
              const state = resourceState(progress, pack, estimate, error, reason)
              const rowPercent = progress.total > 0 ? Math.min(100, progress.done / progress.total * 100) : 0
              return (
                <li
                  key={definition.id}
                  className={`offline-resource is-${state}`}
                  style={{ '--resource-tone': definition.tone } as CSSProperties}
                  data-resource={definition.id}
                  data-state={state}
                >
                  <span className="offline-resource-icon"><ResourceIcon id={definition.id} /></span>
                  <span className="offline-resource-body">
                    <span className="offline-resource-title">
                      <strong>{definition.label}</strong>
                      <em>{stateLabel(state)}</em>
                    </span>
                    <span className="offline-resource-track"><span style={{ width: `${rowPercent}%` }} /></span>
                    <small>{resourceDetail(definition.id, progress, state, definition.hint, reason)}</small>
                  </span>
                </li>
              )
            })}
          </ul>

          <footer className="offline-route-footer">
            <span><i /> Automatic route caching</span>
            {partial && <button onClick={() => setAttempt(value => value + 1)}>Try again</button>}
          </footer>
        </section>
      )}
    </div>
  )
}
