import { useEffect, useRef, useState } from 'react'
import { useMapEvents } from 'react-leaflet'
import L from 'leaflet'
import { cancelRoutingData, prepareRoutingData, fetchRoutingDataStatus, responseError, normalizePackBounds, formatBytes } from '../lib/offline'
import type { RuntimeConfig, RoutingDataStatus } from '../lib/offline'
import './RoutingDownloadControl.css'
import { useEscapeDismiss, ESCAPE_PRIORITY } from './useEscapeDismiss'

interface Suggestion { regionId: string; name: string; installed: boolean; active: boolean; coversView?: boolean }
interface AcquisitionPlan { pbfBytes: number | null; estimatedBytes: number | null; tilesKnown: boolean; tilesTotal: number; tilesCached: number; tilesMissing: number }

const stages = [
  { id: 'pbf', label: 'Road data' },
  { id: 'planning', label: 'Select terrain tiles' },
  { id: 'elevation', label: 'Terrain tiles' },
  { id: 'build', label: 'Build routing graph' },
  { id: 'warmup', label: 'Prepare riding profiles' },
]

export default function RoutingDownloadControl({ runtime, status: suppliedStatus, onStatus }: {
  runtime: RuntimeConfig
  status?: RoutingDataStatus | null
  onStatus?: (status: RoutingDataStatus) => void
}) {
  const [localStatus, setLocalStatus] = useState<RoutingDataStatus | null>(null)
  const status = suppliedStatus === undefined ? localStatus : suppliedStatus
  const publish = onStatus ?? setLocalStatus
  const suggestionRequest = useRef<AbortController | null>(null)
  const [revision, setRevision] = useState(0)
  const [region, setRegion] = useState<Suggestion | null>(null)
  const [open, setOpen] = useState(false)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')
  const [checking, setChecking] = useState(true)
  const [plan, setPlan] = useState<AcquisitionPlan | null>(null)
  const [planning, setPlanning] = useState(false)
  useEscapeDismiss(open, () => setOpen(false), ESCAPE_PRIORITY.panel)
  const element = useRef<HTMLDivElement>(null)
  const map = useMapEvents({
    movestart: () => { suggestionRequest.current?.abort(); setRegion(null); setChecking(true) },
    moveend: () => setRevision(value => value + 1),
    resize: () => setRevision(value => value + 1),
  })
  const running = status?.job?.state === 'queued' || status?.job?.state === 'running'
  useEffect(() => {
    setPlan(null)
    setPlanning(false)
    if (!open || !region || running || !runtime.offline?.routing) return
    const controller = new AbortController()
    setPlanning(true)
    void fetch(`${runtime.offline.routing}/plan`, {
      method: 'POST', signal: controller.signal,
      headers: { 'Content-Type': 'application/json', 'X-GPX-Editor': '1' },
      body: JSON.stringify({ regionId: region.regionId }),
    }).then(async response => {
      if (!response.ok) throw await responseError(response, 'Could not estimate routing download')
      const value = await response.json() as AcquisitionPlan
      if (!controller.signal.aborted) setPlan(value)
    }).catch(() => { /* Estimates are advisory; preparation still reports failures. */ })
      .finally(() => { if (!controller.signal.aborted) setPlanning(false) })
    return () => controller.abort()
  }, [open, region, running, runtime])
  useEffect(() => {
    if (!runtime.offline?.routing) return
    const controller = new AbortController()
    let timer: number | undefined
    const poll = async () => {
      try {
        const next = await fetchRoutingDataStatus(runtime, controller.signal)
        if (next && !controller.signal.aborted) {
          setLocalStatus(next)
          onStatus?.(next)
        }
      } catch { /* Keep the last known progress during a connection interruption. */ }
      if (!controller.signal.aborted) timer = window.setTimeout(() => void poll(), 1000)
    }
    void poll()
    return () => { controller.abort(); window.clearTimeout(timer) }
  }, [onStatus, runtime])
  useEffect(() => {
    if (element.current) {
      L.DomEvent.disableClickPropagation(element.current)
      L.DomEvent.disableScrollPropagation(element.current)
    }
  }, [])
  useEffect(() => {
    const endpoint = runtime.offline?.routing
    if (!endpoint) return
    const controller = new AbortController()
    suggestionRequest.current = controller
    setRegion(null)
    setChecking(true)
    const timer = window.setTimeout(async () => {
      const bounds = map.getBounds()
      const bbox = normalizePackBounds(
        { lat: bounds.getSouth(), lon: bounds.getWest() },
        { lat: bounds.getNorth(), lon: bounds.getEast() },
      )
      if (!bbox) { setChecking(false); return }
      try {
        const response = await fetch(`${endpoint}/suggest`, {
          method: 'POST', signal: controller.signal,
          headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ bbox }),
        })
        if (!response.ok) throw await responseError(response, 'Routing downloads are unavailable')
        const data = await response.json() as { region: Suggestion | null }
        if (data.region !== null && (!data.region || typeof data.region.regionId !== 'string' || typeof data.region.name !== 'string')) {
          throw new Error('Invalid routing region suggestion')
        }
        if (!controller.signal.aborted) { setRegion(data.region); setError('') }
      } catch (reason) {
        if (!controller.signal.aborted) setError((reason as Error).message)
      } finally {
        if (!controller.signal.aborted) setChecking(false)
      }
    }, 500)
    return () => { controller.abort(); window.clearTimeout(timer) }
  }, [map, revision, runtime, status?.generationId])

  const act = async (cancel: boolean) => {
    if (busy || (!cancel && !region)) return
    setBusy(true)
    setError('')
    try {
      publish(cancel ? await cancelRoutingData(runtime) : await prepareRoutingData(runtime, region!.regionId))
    } catch (reason) { setError((reason as Error).message) }
    finally { setBusy(false) }
  }
  const active = region?.active || (status?.ready && region?.regionId === status.regionId)
  const job = status?.job
  const percent = job?.total ? Math.min(100, Math.round((job.done ?? 0) / job.total * 100)) : null
  const stageIndex = stages.findIndex(stage => stage.id === job?.phase)
  const phaseLabel = stages[stageIndex]?.label ?? 'Checking routing data'
  const tiles = job?.completedItems ?? 0
  if (!runtime.offline?.routing) return null
  return <div className="routing-download" ref={element}>
    {open && <section className="routing-download-panel" aria-label="Offline routing download">
      <header><div><small>OFFLINE ROUTING</small><strong>{running ? (region?.regionId === job?.regionId ? region?.name : job?.regionId) : region?.name ?? 'Routing data'}</strong></div><button className="routing-download-close" aria-label="Close routing download" onClick={() => setOpen(false)}>×</button></header>
      {running ? <>
        <ol className="routing-download-stages">{stages.map((stage, i) => <li key={stage.id} className={i < stageIndex ? 'complete' : i === stageIndex ? 'current' : ''}><span>{i < stageIndex ? '✓' : i + 1}</span>{stage.label}</li>)}</ol>
        <div className="routing-download-transfer" role="status">
          <strong>{job?.retrying ? `Retrying · attempt ${job.attempt ?? 1}` : phaseLabel}</strong>
          {job?.phase === 'elevation' && <>
            <span>{tiles}{job.itemsTotal ? ` of ${job.itemsTotal}` : ''} terrain tiles ready</span>
            <small>{job.itemsDownloaded ?? 0} downloaded · {job.itemsReused ?? 0} reused</small>
          </>}
          <span className="routing-download-item" title={job?.item}>{job?.item}</span>
          {!!job?.elapsedSeconds && <small>{Math.floor(job.elapsedSeconds / 60)}m {Math.floor(job.elapsedSeconds % 60)}s in this step{job.retrying && job.retrySeconds ? ` · retry in ${Math.ceil(job.retrySeconds)}s` : ''}</small>}
          <progress max={100} value={percent ?? undefined} aria-label="Routing download progress" />
          <small>{job?.phase === 'pbf' || job?.phase === 'elevation' ? `${formatBytes(job?.done ?? 0)}${job?.total ? ` of ${formatBytes(job.total)}` : ''} · current file` : percent !== null ? `${percent}% of current step` : 'Working…'}</small>
        </div>
        <button className="routing-download-secondary" disabled={busy} onClick={() => void act(true)}>Cancel download</button>
      </> : <>
        <p>{checking ? 'Finding routing data for this view…' : active ? 'Downloaded and ready to ride offline.' : region ? region.coversView === false ? 'Local extract for the map centre. It does not cover every edge of this view; neighbouring areas may need another download.' : 'Smallest available extract covering this view.' : 'No local routing extract found. Zoom in or move towards the area you want to ride.'}</p>
        {!active && <p className="routing-download-estimate">{planning ? 'Checking cached routing data…' : plan?.tilesKnown ? `${plan.tilesTotal} terrain tiles · ${plan.tilesCached} cached · ${plan.tilesMissing} to fetch` : 'Terrain requirements will be known after the road data is downloaded.'}
          {plan?.estimatedBytes != null && <><br />{formatBytes(plan.estimatedBytes)} remaining source downloads</>}
        </p>}
        {region && !active && <button className="routing-download-primary" disabled={busy || checking || (!region.installed && runtime.offline.mode === 'cache-only')} onClick={() => void act(false)}>
          {region.installed ? 'Use downloaded region' : `Download ${region.name}`}
        </button>}
        {runtime.offline.mode === 'cache-only' && !region?.installed && <p>Go online to download routing data.</p>}
      </>}
      {(error || status?.error || job?.state === 'failed') && <p role="alert">{error || status?.error || job?.detail}</p>}
      {!!status?.cacheBytes && <small>{formatBytes(status.cacheBytes)} cached for routing</small>}
    </section>}
    <button className="routing-download-pill" aria-expanded={open} onClick={() => setOpen(value => !value)}>
      {running ? `${phaseLabel}${job?.phase === 'elevation' ? ` · ${tiles}${job.itemsTotal ? `/${job.itemsTotal}` : ''} tiles` : '…'}` : job?.state === 'failed' ? 'Routing download failed' : active ? 'Routing ready' : 'Offline routing'}
      {!running && region && !active && <span> · {region.name} ↓</span>}
    </button>
  </div>
}
