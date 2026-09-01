import { useEffect, useMemo, useRef, useState } from 'react'
import { boundingBoxSpanKm } from '../lib/poi'
import {
  buildPackEstimateRequest,
  cancelPack,
  estimatePack,
  formatBytes,
  packRequestSignature,
  startPack,
} from '../lib/offline'
import type {
  OfflineStatus,
  PackBounds,
  PackEstimate,
  PackResourceProgress,
  PackSummary,
  RuntimeConfig,
} from '../lib/offline'
import { ESCAPE_PRIORITY, useEscapeDismiss } from './useEscapeDismiss'

const ACTIVE_STATES = new Set(['queued', 'running', 'cancelling'])
const EMPTY_PROGRESS: PackResourceProgress = { done: 0, total: 0, failed: 0, bytes: 0, items: 0 }

function estimatedMapResources(estimate: PackEstimate | null): number {
  if (!estimate) return 0
  return (estimate.counts['openfreemap-core'] ?? 0) +
    (estimate.counts['openfreemap-glyphs'] ?? 0) +
    (estimate.counts.openfreemap ?? 0) +
    (estimate.counts['openfreemap-raster'] ?? 0)
}

export function OfflineAreaPanel({
  runtime,
  bounds,
  selecting,
  currentZoom,
  suggestedName,
  status,
  packs,
  onToggleSelection,
  onClearSelection,
  onOpen,
  onPacksChanged,
}: {
  runtime: RuntimeConfig
  bounds: PackBounds | null
  selecting: boolean
  currentZoom: number
  suggestedName?: string
  status: OfflineStatus | null
  packs: PackSummary[]
  onToggleSelection: () => void
  onClearSelection: () => void
  onOpen: () => void
  onPacksChanged: (pack?: PackSummary) => Promise<void> | void
}) {
  const [open, setOpen] = useState(false)
  const [name, setName] = useState('Map area')
  const [minZoom, setMinZoom] = useState(7)
  const [maxZoom, setMaxZoom] = useState(11)
  const [includeElevation, setIncludeElevation] = useState(false)
  const [includePlaces, setIncludePlaces] = useState(true)
  const [estimate, setEstimate] = useState<PackEstimate | null>(null)
  const [estimateSignature, setEstimateSignature] = useState<string | null>(null)
  const [activePackID, setActivePackID] = useState<string | null>(null)
  const [busy, setBusy] = useState<'start' | 'cancel' | null>(null)
  const [estimating, setEstimating] = useState(false)
  const [error, setError] = useState('')
  const estimateGenerationRef = useRef(0)
  const latestZoomRef = useRef(currentZoom)
  const suggestedNameRef = useRef(suggestedName)
  latestZoomRef.current = currentZoom
  suggestedNameRef.current = suggestedName
  useEscapeDismiss(open, () => setOpen(false), ESCAPE_PRIORITY.panel)

  const activePack = packs.find(pack => pack.id === activePackID) ?? null
  const request = useMemo(() => buildPackEstimateRequest({
    name: `Map: ${(name.trim() || 'Selected area').replace(/^Map:\s*/i, '')}`.slice(0, 100),
    area: 'bbox',
    route: [],
    bbox: bounds,
    paddingKm: 0,
    minZoom,
    maxZoom,
    layers: ['openfreemap'],
    scopes: [
      ...(includeElevation ? ['elevation'] : []),
      ...(includePlaces ? ['places'] : []),
    ],
  }), [bounds, includeElevation, includePlaces, maxZoom, minZoom, name])
  const estimateRequest = useMemo(() => buildPackEstimateRequest({
    name: 'Map: Selected area',
    area: 'bbox',
    route: [],
    bbox: bounds,
    paddingKm: 0,
    minZoom,
    maxZoom,
    layers: ['openfreemap'],
    scopes: [
      ...(includeElevation ? ['elevation'] : []),
      ...(includePlaces ? ['places'] : []),
    ],
  }), [bounds, includeElevation, includePlaces, maxZoom, minZoom])
  const requestSignature = packRequestSignature(estimateRequest)
  const estimateCurrent = estimate && estimateSignature === requestSignature ? estimate : null
  const bulkAvailable = runtime.maps.openfreemap?.allowBulk === true
  const writable = status?.writable !== false
  const workingOffline = runtime.offline?.mode === 'cache-only'
  const boundsKey = bounds ? `${bounds.south}:${bounds.west}:${bounds.north}:${bounds.east}` : ''

  useEffect(() => {
    if (!boundsKey) return
    const zoom = Math.max(1, Math.min(14, Math.round(latestZoomRef.current)))
    setMinZoom(Math.max(0, zoom - 2))
    setMaxZoom(Math.min(14, zoom + 2))
    setEstimate(null)
    setEstimateSignature(null)
    setActivePackID(null)
    setName((suggestedNameRef.current?.split(',')[0] || 'Selected area').trim())
    setOpen(true)
    onOpen()
  }, [boundsKey, onOpen])

  useEffect(() => {
    const generation = ++estimateGenerationRef.current
    setEstimate(null)
    setEstimateSignature(null)
    if (!open || !estimateRequest || !runtime.offline || !bulkAvailable || !writable || workingOffline) {
      setEstimating(false)
      return
    }

    const controller = new AbortController()
    setEstimating(true)
    setError('')
    const timer = window.setTimeout(() => {
      void estimatePack(runtime, estimateRequest, controller.signal)
        .then(next => {
          if (generation !== estimateGenerationRef.current || controller.signal.aborted) return
          setEstimate(next)
          setEstimateSignature(packRequestSignature(estimateRequest))
        })
        .catch(reason => {
          if ((reason as Error).name !== 'AbortError' && generation === estimateGenerationRef.current) {
            setError((reason as Error).message)
          }
        })
        .finally(() => {
          if (generation === estimateGenerationRef.current) setEstimating(false)
        })
    }, 220)
    return () => {
      window.clearTimeout(timer)
      controller.abort()
    }
  }, [bulkAvailable, estimateRequest, open, runtime, workingOffline, writable])

  const handleStart = async () => {
    if (!request || !estimateCurrent || workingOffline) return
    setBusy('start')
    setError('')
    try {
      const pack = await startPack(runtime, request)
      if (pack) {
        setActivePackID(pack.id)
        await onPacksChanged(pack)
      }
    } catch (reason) {
      setError((reason as Error).message)
    } finally {
      setBusy(null)
    }
  }

  const handleCancel = async (pack: PackSummary) => {
    setBusy('cancel')
    setError('')
    try {
      await cancelPack(runtime, pack.id)
      await onPacksChanged()
    } catch (reason) {
      setError((reason as Error).message)
    } finally {
      setBusy(null)
    }
  }

  const handleToggleSelection = () => {
    if (!selecting) setOpen(false)
    onOpen()
    onToggleSelection()
  }

  const togglePanel = () => {
    const next = !open
    setOpen(next)
    if (next) onOpen()
  }

  const span = bounds ? boundingBoxSpanKm(bounds) : null
  const total = activePack?.total ?? estimateCurrent?.resources ?? 0
  const done = activePack?.done ?? 0
  const percent = total > 0 ? Math.min(100, Math.round(done / total * 100)) : 0
  const mapProgress = activePack?.resources['vector-map'] ?? { ...EMPTY_PROGRESS, total: estimatedMapResources(estimateCurrent) }
  const elevationProgress = activePack?.resources.elevation ?? { ...EMPTY_PROGRESS, total: estimateCurrent?.counts.elevation ?? 0 }
  const placesProgress = activePack?.resources.places ?? { ...EMPTY_PROGRESS, total: estimateCurrent?.counts.places ?? 0 }
  const levels = Math.abs(maxZoom - minZoom) + 1

  return (
    <div className={`offline-area-control${open ? ' is-open' : ''}`} data-testid="offline-area-control">
      <button
        className={`offline-storage-fab${activePack?.status === 'complete' ? ' is-ready' : ''}`}
        onClick={togglePanel}
        aria-expanded={open}
        aria-controls="offline-area-panel"
        aria-label="Offline maps"
        title="Download a map area for offline use"
        data-testid="offline-area-button"
      >
        <span className={`offline-health-dot${runtime.offline && bulkAvailable && !workingOffline ? '' : ' offline-health-dot--bad'}`} />
        <span>{activePack && ACTIVE_STATES.has(activePack.status) ? `Downloading ${percent}%` : 'Offline maps'}</span>
      </button>

      {open && (
        <section id="offline-area-panel" className="offline-panel offline-area-panel" aria-label="Offline map area" data-testid="offline-area-panel">
          <header className="offline-panel-header">
            <div><strong>Take this map offline</strong><span>OpenFreeMap</span></div>
            <button onClick={() => setOpen(false)} aria-label="Close offline map panel">&times;</button>
          </header>

          {!runtime.offline && <p className="offline-error">Connect to the overland server to create persistent map downloads.</p>}
          {runtime.offline && !bulkAvailable && <p className="offline-error">This server does not permit bulk OpenFreeMap downloads.</p>}
          {workingOffline && <p className="offline-error">Go online before starting a new area download. Saved areas remain available.</p>}
          {status && !writable && <p className="offline-error">Persistent offline storage is disabled on this server.</p>}
          {error && <p className="offline-error" role="alert">{error}</p>}

          <div className={`offline-area-selection${bounds ? ' has-area' : ''}`}>
            <div>
              <strong>{selecting ? 'Drag across the map' : bounds ? 'Area selected' : 'Choose an area'}</strong>
              <small>
                {selecting
                  ? 'Release to set the download boundary · Esc cancels'
                  : span
                    ? `${span.widthKm.toFixed(0)} × ${span.heightKm.toFixed(0)} km`
                    : 'Draw a rectangle around the region you need'}
              </small>
            </div>
            <button className="btn btn-ghost btn-sm" onClick={handleToggleSelection}>
              {selecting ? 'Cancel' : bounds ? 'Redraw' : 'Draw area'}
            </button>
            {bounds && !selecting && <button className="offline-area-clear" onClick={onClearSelection} aria-label="Clear selected area">&times;</button>}
          </div>

          <div className="offline-form-grid">
            <label className="offline-field offline-field--wide">
              Download name
              <input value={name} maxLength={95} onChange={event => setName(event.target.value)} />
            </label>
            <label className="offline-field">
              Minimum zoom
              <input type="number" min={0} max={14} value={minZoom} onChange={event => setMinZoom(Number(event.target.value))} />
            </label>
            <label className="offline-field">
              Maximum zoom
              <input type="number" min={0} max={14} value={maxZoom} onChange={event => setMaxZoom(Number(event.target.value))} />
            </label>
          </div>
          <p className="offline-help">Zoom {Math.min(minZoom, maxZoom)}–{Math.max(minZoom, maxZoom)} · {levels} level{levels === 1 ? '' : 's'}. More levels and larger areas need substantially more tiles.</p>

          <fieldset className="offline-checks">
            <legend>Include</legend>
            <label><input type="checkbox" checked readOnly /> Vector map</label>
            <label><input type="checkbox" checked={includeElevation} onChange={event => setIncludeElevation(event.target.checked)} /> Terrain elevation</label>
            <label title="Pins exact place searches already made; it never sweeps Nominatim"><input type="checkbox" checked={includePlaces} onChange={event => setIncludePlaces(event.target.checked)} /> Searched places</label>
          </fieldset>

          <div className="offline-pack-actions">
            {estimating && <span className="offline-estimating" role="status">Estimating download…</span>}
            <button
              className="btn btn-primary btn-sm"
              disabled={!request || !estimateCurrent || busy !== null || !writable || workingOffline}
              onClick={() => void handleStart()}
            >
              {busy === 'start' ? 'Starting…' : 'Download area'}
            </button>
          </div>

          {estimateCurrent && (
            <div className="offline-estimate" data-testid="offline-area-estimate" aria-live="polite">
              <div><span>Resources</span><strong>{estimateCurrent.resources?.toLocaleString() ?? '—'}</strong></div>
              <div><span>Download</span><strong>{formatBytes(estimateCurrent.bytes)}</strong></div>
              <div><span>Already local</span><strong>{formatBytes(estimateCurrent.reusedBytes)}</strong></div>
              <div><span>Free quota</span><strong>{formatBytes(estimateCurrent.quotaRemaining)}</strong></div>
              {estimateCurrent.blocked.map((blocked, index) => <p key={`${blocked.resource ?? blocked.layer}:${index}`}>{blocked.reason}</p>)}
            </div>
          )}

          {activePack && (
            <div className="offline-area-progress" data-testid="offline-area-progress" aria-live="polite">
              <div className="offline-usage-head"><strong>{activePack.status === 'complete' ? 'Available offline' : `Downloading ${percent}%`}</strong><span>{done.toLocaleString()} / {total.toLocaleString()}</span></div>
              <div className="offline-meter"><span style={{ width: `${percent}%` }} /></div>
              <ul>
                <li><span>Vector map</span><strong>{mapProgress.done}/{mapProgress.total}</strong></li>
                {includeElevation && <li><span>Terrain elevation</span><strong>{elevationProgress.done}/{elevationProgress.total}</strong></li>}
                {includePlaces && <li><span>Exact place searches</span><strong>{placesProgress.done}/{placesProgress.total}</strong></li>}
              </ul>
              {ACTIVE_STATES.has(activePack.status) && <button className="btn btn-ghost btn-xs" disabled={busy !== null} onClick={() => void handleCancel(activePack)}>Cancel download</button>}
            </div>
          )}

          <p className="offline-area-policy">Place lookup is cached only for searches you submit. Selecting an area never scans or autocompletes through Nominatim.</p>
        </section>
      )}
    </div>
  )
}
