import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import type L from 'leaflet'
import { parseGPX, fromGpxFilename, toGpxFilename } from '../../../src/lib/gpx'
import {
  reverseTrack,
  trimTrack,
  splitTrack,
  splitIntoStages,
  simplifyToMaxPoints,
  withElevations,
} from '../../../src/lib/edit'
import { fetchElevationProfile } from '../../../src/lib/elevation'
import {
  bootstrapRuntimeConfig,
  decodeRuntimeConfig,
  fetchRoutingDataStatus,
  setRuntimeOfflineMode,
} from '../../../src/lib/offline'
import type { RoutingDataStatus } from '../../../src/lib/offline'
import { calculateRoute } from '../../../src/lib/routing'
import type { RouteResult } from '../../../src/lib/routing'
import { searchPlaces } from '../../../src/lib/geocoding'
import type { PlaceResult } from '../../../src/lib/geocoding'
import { fetchPoisForArea, POI_KINDS } from '../../../src/lib/poi'
import type { BoundingBox, Poi, PoiKind } from '../../../src/lib/poi'
import { runtimeTerrainLayers } from '../../../src/lib/terrain'
import type { Coordinate, Track } from '../../../src/lib/types'
import { decodeDraft, emptyPlan, jsonBody, request, trackGPX } from './model'
import type { Document, Draft, Plan, Tab } from './model'
import Icon from './Icon'
import MobileMap from './MobileMap'
import TrackSummary from './TrackSummary'
import RouteSurface from './RouteSurface'
import OfflineMapControl from './OfflineMapControl'
import PlanPanel, { PlanHandle } from './PlanPanel'
import { useIncomingGPX } from './useIncomingGPX'

const emptyCoordinates: Coordinate[] = []
type Dialog = {
  title: string
  message?: string
  initial?: string
  action: (value: string) => Promise<void> | void
  label?: string
}

export default function App() {
  const [runtime, setRuntime] = useState(() => bootstrapRuntimeConfig('', 'cache-only'))
  const [runtimeReady, setRuntimeReady] = useState(false)
  const [startupError, setStartupError] = useState('')
  const [startupAttempt, setStartupAttempt] = useState(0)
  const [native, setNative] = useState(false)
  const [incomingGPX, setIncomingGPX] = useState(false)
  const [tab, setTab] = useState<Tab>('explore')
  const [activeMap, setActiveMap] = useState<'plan' | 'track'>('plan')
  const [expanded, setExpanded] = useState(false)
  const [planPanelOpen, setPlanPanelOpen] = useState(false)
  const [routeInfoOpen, setRouteInfoOpen] = useState(false)
  const [plan, setPlan] = useState<Plan>(emptyPlan)
  const [document, setDocument] = useState<Document | null>(null)
  const [history, setHistory] = useState<Document[]>([])
  const [files, setFiles] = useState<string[]>([])
  const [filter, setFilter] = useState('')
  const [choices, setChoices] = useState<Document[]>([])
  const [hydrated, setHydrated] = useState(false)
  const [canPersist, setCanPersist] = useState(false)
  const [draftSaved, setDraftSaved] = useState(true)
  const [routeState, setRouteState] = useState<{
    key: string
    result?: RouteResult
    error?: string
    loading?: boolean
  }>({ key: '' })
  const [routing, setRouting] = useState<RoutingDataStatus | null>(null)
  const [revision, setRevision] = useState(0)
  const [center, setCenter] = useState<Coordinate>({ lat: 41.65, lon: -0.88 })
  const [bounds, setBounds] = useState<BoundingBox>({
    south: 41.5,
    west: -1,
    north: 41.8,
    east: -0.7,
  })
  const [layer, setLayer] = useState('openfreemap')
  const [relief, setRelief] = useState(false)
  const [layersOpen, setLayersOpen] = useState(false)
  const [toast, setToast] = useState('')
  const [busy, setBusy] = useState(false)
  const [dialog, setDialog] = useState<Dialog | null>(null)
  const [dialogValue, setDialogValue] = useState('')
  const [query, setQuery] = useState('')
  const [places, setPlaces] = useState<PlaceResult[]>([])
  const [pois, setPois] = useState<Poi[]>([])
  const [poiKind, setPoiKind] = useState<PoiKind | null>(null)
  const [tools, setTools] = useState(false)
  const [range, setRange] = useState<[number, number]>([0, 100])
  const map = useRef<L.Map | null>(null)
  const fileInput = useRef<HTMLInputElement>(null)
  const loaded = useRef(false)
  const draftQueue = useRef(Promise.resolve())
  const currentDocument = useRef(document)
  currentDocument.current = document
  const [restoreView, setRestoreView] = useState(false)

  const notify = useCallback((message: string) => setToast(message), [])
  const refresh = useCallback(() => setRevision((value) => value + 1), [])
  const openPlanPanel = useCallback((open: boolean) => {
    setPlanPanelOpen(open)
    if (open) setRouteInfoOpen(false)
  }, [])
  const onMap = useCallback((value: L.Map) => {
    map.current = value
    setRestoreView(true)
  }, [])
  const onView = useCallback((point: Coordinate, box: BoundingBox) => {
    setCenter(point)
    setBounds(box)
  }, [])
  const run = async (action: () => Promise<void>) => {
    setBusy(true)
    try {
      await action()
    } catch (error) {
      notify((error as Error).message)
    } finally {
      setBusy(false)
    }
  }
  const ask = (value: Dialog) => {
    setDialogValue(value.initial ?? '')
    setDialog(value)
  }
  const routeKey = JSON.stringify([plan.points, plan.profile, plan.permit, routing?.generationId])
  const route = routeState.key === routeKey && routing?.ready ? routeState.result : undefined
  const showingPlan = tab === 'plan' || (tab !== 'library' && activeMap === 'plan')
  const coordinates = showingPlan
    ? (route?.coordinates ?? emptyCoordinates)
    : (document?.track.coordinates ?? emptyCoordinates)
  const pins = showingPlan ? [] : (document?.track.waypoints ?? [])
  const fit = useCallback((points: Coordinate[]) => {
    if (points.length)
      map.current?.fitBounds(
        points.map((p) => [p.lat, p.lon] as [number, number]),
        { padding: [35, 75], maxZoom: 15, animate: true },
      )
  }, [])

  useEffect(() => {
    const controller = new AbortController()
    void request<unknown>('/config', { signal: controller.signal })
      .then((value) => {
        if (controller.signal.aborted) return
        setRuntime(decodeRuntimeConfig(value))
        setRuntimeReady(true)
        setStartupError('')
      })
      .catch((error) => {
        if (!controller.signal.aborted) setStartupError((error as Error).message)
      })
    return () => controller.abort()
  }, [startupAttempt])
  useEffect(() => {
    const controller = new AbortController()
    void request<{ native: boolean; incomingGPX?: boolean }>('/mobile/capabilities', { signal: controller.signal })
      .then((value) => {
        setNative(value.native)
        setIncomingGPX(value.incomingGPX === true)
      })
      .catch(() => {})
    void request<unknown>('/mobile/draft', { signal: controller.signal })
      .then((raw) => {
        const draft = decodeDraft(raw)
        if (draft) {
          setPlan(draft.plan)
          setDocument(draft.document)
          setTab(draft.tab)
          setActiveMap(draft.tab === 'plan' || !draft.document ? 'plan' : 'track')
        }
        setCanPersist(true)
      })
      .catch(() => {})
      .finally(() => {
        if (!controller.signal.aborted) setHydrated(true)
      })
    return () => controller.abort()
  }, [])
  useEffect(() => {
    if (tab === 'plan') setActiveMap('plan')
    else if (tab === 'library' && document) setActiveMap('track')
  }, [tab, document])
  useEffect(() => {
    if (!hydrated || !restoreView || loaded.current) return
    loaded.current = true
    fit(tab === 'plan' ? plan.points : (document?.track.coordinates ?? []))
  }, [hydrated, restoreView, fit, tab, plan.points, document])
  useEffect(() => {
    if (!hydrated || !canPersist) return
    setDraftSaved(false)
    const draft: Draft = { version: 1, plan, document, tab }
    const timer = setTimeout(() => {
      // Serialize writes so a slow earlier save never overwrites a newer edit.
      draftQueue.current = draftQueue.current
        .catch(() => {})
        .then(async () => {
          await request('/mobile/draft', jsonBody(draft, 'PUT'))
          setDraftSaved(true)
        })
        .catch(() => {
          notify('Could not save the draft. Save or share your track before closing.')
        })
    }, 250)
    return () => clearTimeout(timer)
  }, [plan, document, tab, hydrated, canPersist, notify])
  useEffect(() => {
    const controller = new AbortController()
    void request<{ files: string[] }>('/files', { signal: controller.signal })
      .then((value) => setFiles(value.files))
      .catch(() => {})
    return () => controller.abort()
  }, [revision])
  useEffect(() => {
    if (!runtime.offline?.routing) return
    const controller = new AbortController()
    const poll = () =>
      void fetchRoutingDataStatus(runtime, controller.signal)
        .then((value) => {
          if (!controller.signal.aborted) setRouting(value)
        })
        .catch(() => {})
    poll()
    const timer = setInterval(poll, 2500)
    return () => {
      controller.abort()
      clearInterval(timer)
    }
  }, [runtime, revision])
  useEffect(() => {
    if (!hydrated || plan.points.length < 2) {
      setRouteState({ key: routeKey })
      return
    }
    if (!routing?.ready) {
      setRouteState({
        key: routeKey,
        error: 'Download a routing region in Offline to plan this ride.',
      })
      return
    }
    const controller = new AbortController()
    setRouteState({ key: routeKey, loading: true })
    const timer = setTimeout(() => {
      void calculateRoute(plan.points, plan.profile, controller.signal, {
        runtime,
        accessPermit: plan.permit,
      })
        .then((result) => {
          if (!controller.signal.aborted) setRouteState({ key: routeKey, result })
        })
        .catch((error) => {
          if (!controller.signal.aborted)
            setRouteState({ key: routeKey, error: (error as Error).message })
        })
    }, 180)
    return () => {
      controller.abort()
      clearTimeout(timer)
    }
  }, [routeKey, plan.points, plan.profile, plan.permit, runtime, routing?.ready, hydrated])
  useEffect(() => {
    if (!toast) return
    const timer = setTimeout(() => setToast(''), 6500)
    return () => clearTimeout(timer)
  }, [toast])
  useEffect(() => {
    const back = (event: Event) => {
      if (dialog) setDialog(null)
      else if (routeInfoOpen) setRouteInfoOpen(false)
      else if (layersOpen) setLayersOpen(false)
      else if (choices.length) setChoices([])
      else if (tools) setTools(false)
      else if (planPanelOpen && tab === 'plan') setPlanPanelOpen(false)
      else if (expanded && tab === 'library') setExpanded(false)
      else if (tab !== 'explore') setTab('explore')
      else return
      event.preventDefault()
    }
    window.addEventListener('overland:back', back)
    return () => window.removeEventListener('overland:back', back)
  }, [dialog, routeInfoOpen, layersOpen, choices, tools, expanded, planPanelOpen, tab])
  useEffect(() => {
    if (tab === 'plan' && routeState.key === routeKey && routeState.error) notify(routeState.error)
  }, [tab, routeState.key, routeState.error, routeKey, notify])

  const movePoint = useCallback(
    (index: number, point: Coordinate) =>
      setPlan((current) => ({
        ...current,
        points: current.points.map((p, i) => (i === index ? point : p)),
      })),
    [],
  )
  const addPoint = () => {
    if (plan.points.length >= 100) {
      notify('A route can have up to 100 controls.')
      return
    }
    setPlan((current) => ({ ...current, points: [...current.points, center] }))
  }
  const openDocument = (value: Document) => {
    setDocument(value)
    setHistory([])
    setChoices([])
    setTools(false)
    setRange([0, 100])
    setTab('library')
    setExpanded(false)
    fit(value.track.coordinates)
  }
  const importContent = (text: string, filename: string, library = false) => {
    const tracks = parseGPX(text)
    if (!tracks.length) throw new Error('No usable track or route in this GPX.')
    const docs = tracks.map(
      (track, i): Document => ({
        track: {
          ...track,
          filename:
            tracks.length === 1 ? filename : toGpxFilename(`${fromGpxFilename(filename)} ${i + 1}`),
        },
        libraryFilename: library && tracks.length === 1 ? filename : null,
        dirty: !library || tracks.length > 1,
      }),
    )
    if (docs.length === 1) openDocument(docs[0])
    else setChoices(docs)
  }
  const confirmReplace = (action: () => Promise<void>) => {
    if (document?.dirty)
      ask({
        title: 'Open another track?',
        message:
          'Your current edits will be replaced. Save or share them first if you want to keep them.',
        action,
        label: 'Open track',
      })
    else void run(action)
  }
  const importFile = () =>
    confirmReplace(async () => {
      if (!native) {
        fileInput.current?.click()
        return
      }
      const result = await request<{ filename: string; content: string }>(
        '/mobile/import',
        jsonBody({}),
      )
      if (result.filename) importContent(result.content, result.filename)
    })
  const shared = useIncomingGPX({
    enabled: incomingGPX && hydrated,
    blocked: busy || !!dialog || choices.length > 0,
    dirty: document?.dirty === true,
    onImport: (content, filename) => {
      importContent(content, filename)
      setRouteInfoOpen(false)
      setLayersOpen(false)
    },
    onError: notify,
  })
  const saveTrack = async (track: Track, filename: string, replace: boolean) => {
    const content = trackGPX(track)
    if (replace)
      await request(`/gpx/${encodeURIComponent(filename)}`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/gpx+xml' },
        body: content,
      })
    else {
      const form = new FormData()
      form.append('file', new Blob([content], { type: 'application/gpx+xml' }), filename)
      await request('/upload', { method: 'POST', body: form })
    }
    refresh()
    notify('Saved to your library')
  }
  const saveDocument = () => {
    if (!document) return
    if (document.libraryFilename) {
      void run(async () => {
        await saveTrack(document.track, document.libraryFilename!, true)
        if (currentDocument.current === document) setDocument({ ...document, dirty: false })
      })
      return
    }
    ask({
      title: 'Save track',
      initial: fromGpxFilename(document.track.filename) || document.track.name,
      action: async (name) => {
        const filename = toGpxFilename(name)
        const track = { ...document.track, name, filename }
        await saveTrack(track, filename, false)
        setDocument({ track, libraryFilename: filename, dirty: false })
      },
      label: 'Save track',
    })
  }
  const deleteTrack = (filename: string) => {
    ask({
      title: 'Delete track?',
      message: `Delete “${filename}” from your library? This cannot be undone.`,
      label: 'Delete track',
      action: async () => {
        await request(`/gpx/${encodeURIComponent(filename)}`, { method: 'DELETE' })
        setFiles((current) => current.filter((file) => file !== filename))
        refresh()
        notify('Track deleted from your library')
      },
    })
  }
  const share = async (track: Track) => {
    const filename = track.filename || toGpxFilename(track.name),
      content = trackGPX(track)
    if (native) {
      await request('/mobile/share', jsonBody({ filename, content }))
      return
    }
    const url = URL.createObjectURL(new Blob([content], { type: 'application/gpx+xml' }))
    const link = window.document.createElement('a')
    link.href = url
    link.download = filename
    link.click()
    setTimeout(() => URL.revokeObjectURL(url), 1000)
  }
  const planTrack = (): Track | null =>
    route
      ? {
          name: plan.name,
          filename: toGpxFilename(plan.name),
          coordinates: route.coordinates,
          elevations: route.coordinates.map((p) => p.elevation ?? null),
          waypoints: [],
        }
      : null
  const edit = (track: Track) => {
    if (!document) return
    setHistory((previous) => [...previous.slice(-9), document])
    setDocument({ ...document, track, dirty: true })
    setRange([0, 100])
    fit(track.coordinates)
  }
  const split = (tracks: Track[]) => {
    setChoices(
      tracks.map((track, i) => ({
        track: {
          ...track,
          filename: toGpxFilename(`${fromGpxFilename(document!.track.filename)} part ${i + 1}`),
        },
        libraryFilename: null,
        dirty: true,
      })),
    )
  }
  const refreshElevation = () =>
    void run(async () => {
      if (!document) return
      const result = await fetchElevationProfile(document.track.coordinates, '', '', { runtime })
      if (currentDocument.current === document)
        edit(withElevations(document.track, result.elevations, result.interpolatedPoints))
    })
  const locate = () =>
    void run(async () => {
      const point = native
        ? await request<Coordinate>('/mobile/location', jsonBody({}))
        : await new Promise<Coordinate>((resolve, reject) =>
            navigator.geolocation.getCurrentPosition(
              (p) => resolve({ lat: p.coords.latitude, lon: p.coords.longitude }),
              reject,
              { enableHighAccuracy: true, timeout: 30000 },
            ),
          )
      map.current?.setView([point.lat, point.lon], 14)
    })
  const search = () =>
    void run(async () => {
      if (query.trim().length < 2) return
      setPlaces(await searchPlaces(query, undefined, runtime.nominatimUrl, { runtime }))
    })
  const loadPois = (kind: PoiKind) =>
    void run(async () => {
      if (poiKind === kind) {
        setPoiKind(null)
        setPois([])
        return
      }
      const result = await fetchPoisForArea(kind, bounds, undefined, { runtime })
      setPoiKind(kind)
      setPois(result.pois)
      if (!result.pois.length)
        notify('No matching places in this view. Try a smaller area or another location.')
    })
  const tabs: { id: Tab; label: string }[] = [
    { id: 'explore', label: 'Explore' },
    { id: 'plan', label: 'Plan' },
    { id: 'library', label: 'Library' },
    { id: 'offline', label: 'Offline' },
  ]
  const sheetTitle = document ? fromGpxFilename(document.track.filename) : 'Your tracks'
  const routeLoading = routeState.key === routeKey && routeState.loading
  const visibleFiles = files.filter((name) =>
    fromGpxFilename(name).toLowerCase().includes(filter.toLowerCase()),
  )
  const activeTrack = document?.track
  const startIndex = activeTrack
    ? Math.round((range[0] / 100) * (activeTrack.coordinates.length - 1))
    : 0
  const endIndex = activeTrack
    ? Math.round((range[1] / 100) * (activeTrack.coordinates.length - 1))
    : 0
  const terrainLayers = useMemo(() => runtimeTerrainLayers(runtime), [runtime])
  const detailCoordinates = tab === 'plan' ? route?.coordinates
    : tab === 'library' ? document?.track.coordinates : undefined

  const mapOnly = tab === 'explore' || tab === 'offline' || (tab === 'plan' && !planPanelOpen)
  return (
    <main
      className={`mobile-app ${mapOnly ? 'map-only' : tab === 'plan' ? 'plan-open' : expanded ? 'sheet-expanded' : ''}`}
    >
      <div className="map-stage">
        <MobileMap
          runtime={runtime}
          ready={runtimeReady}
          coordinates={coordinates}
          points={tab === 'plan' ? plan.points : []}
          pins={pins}
          pois={pois}
          surfaces={tab === 'plan' ? route?.surface.segments : undefined}
          layer={layer}
          relief={relief}
          onView={onView}
          onMap={onMap}
          onMovePoint={movePoint}
          onError={notify}
        />
        <header className="map-header">
          {tab === 'explore' ? (
            <div className="map-search">
              <form
                className="search-field"
                onSubmit={(event) => {
                  event.preventDefault()
                  ;(window.document.activeElement as HTMLElement)?.blur()
                  search()
                }}
              >
                <Icon name="search" size={19} />
                <input
                  aria-label="Search places"
                  placeholder="Search places"
                  value={query}
                  onChange={(e) => setQuery(e.target.value)}
                />
                <button type="submit" disabled={busy || !runtimeReady} aria-label="Search">
                  <Icon name="arrow" size={18} />
                </button>
              </form>
              {places.length > 0 && (
                <div className="search-results">
                  {places.map((place, i) => (
                    <button
                      key={i}
                      className="list-row"
                      onClick={() => {
                        map.current?.setView([place.lat, place.lon], 13)
                        setPlaces([])
                      }}
                    >
                      <Icon name="locate" size={18} />
                      <span>{place.display_name}</span>
                      <Icon name="arrow" size={16} />
                    </button>
                  ))}
                </div>
              )}
            </div>
          ) : (
            <>
              <div className="wordmark">
                <Icon name="mountain" />
                <span>OVERLAND</span>
              </div>
              <button
                className={`connection-pill ${runtime.offline?.mode === 'cache-only' ? 'offline' : ''}`}
                disabled={busy || !runtimeReady}
                onClick={() =>
                  void run(async () => {
                    const mode = await setRuntimeOfflineMode(
                      runtime,
                      runtime.offline?.mode === 'cache-only' ? 'auto' : 'cache-only',
                    )
                    setRuntime({ ...runtime, offline: { ...runtime.offline!, mode } })
                  })
                }
              >
                <i />
                {!runtimeReady ? 'Starting' : runtime.offline?.mode === 'cache-only' ? 'Offline' : 'Online'}
              </button>
            </>
          )}
        </header>
        <div className="map-actions">
          <button
            aria-label="Map layers"
            onClick={() => {
              setRouteInfoOpen(false)
              setLayersOpen((value) => !value)
            }}
          >
            <Icon name="layers" />
          </button>
          <button aria-label="My location" disabled={busy} onClick={locate}>
            <Icon name="locate" />
          </button>
          <button
            aria-label="Fit track"
            onClick={() => fit(coordinates.length ? coordinates : plan.points)}
          >
            <Icon name="fit" />
          </button>
        </div>
        {tab === 'plan' && (
          <>
            <div className="map-crosshair" aria-hidden="true">
              <span />
              <span />
            </div>
            <button
              className="add-route-point"
              aria-label={plan.points.length ? 'Add next point here' : 'Start route here'}
              onClick={addPoint}
            >
              <Icon name="plus" size={26} />
            </button>
            {!planPanelOpen && (
              <PlanHandle open={false} onOpen={openPlanPanel} count={plan.points.length} />
            )}
            {routeLoading && (
              <div className="route-map-status" role="status">
                <span className="spinner" />
                Calculating route…
              </div>
            )}
            {route && (
              <button
                className="route-info-button"
                aria-label="Route details"
                onClick={() => {
                  setLayersOpen(false)
                  setPlanPanelOpen(false)
                  setRouteInfoOpen((value) => !value)
                }}
              >
                <Icon name="info" size={24} />
              </button>
            )}
          </>
        )}
        {tab === 'explore' && (
          <div className="map-pois poi-chips">
            {POI_KINDS.map((kind) => (
              <button
                key={kind.id}
                className={poiKind === kind.id ? 'selected' : ''}
                disabled={busy}
                onClick={() => loadPois(kind.id)}
              >
                <span aria-hidden="true">{kind.glyph}</span>
                {kind.label}
              </button>
            ))}
          </div>
        )}
        {tab === 'offline' && runtimeReady && (
          <OfflineMapControl
            runtime={runtime}
            status={routing}
            refresh={refresh}
            bounds={bounds}
            notify={notify}
          />
        )}
        {layersOpen && (
          <section className="layers-popover">
            <div className="section-heading">
              <h3>Map style</h3>
              <button
                className="icon-button"
                aria-label="Close layers"
                onClick={() => setLayersOpen(false)}
              >
                <Icon name="close" size={18} />
              </button>
            </div>
            {terrainLayers.map((item) => (
              <button
                className={`layer-choice ${layer === item.id ? 'selected' : ''}`}
                key={item.id}
                onClick={() => {
                  setLayer(item.id)
                  setLayersOpen(false)
                }}
              >
                {item.label}
                {layer === item.id && <Icon name="check" size={18} />}
              </button>
            ))}
            <label className="setting-row">
              Relief shading
              <input
                type="checkbox"
                checked={relief}
                onChange={(e) => setRelief(e.target.checked)}
              />
            </label>
          </section>
        )}
      </div>

      {!runtimeReady && (
        <div className="startup-notice" role={startupError ? 'alert' : 'status'}>
          {!startupError && <span className="spinner" />}
          <span>{startupError || 'Opening saved maps and routing…'}</span>
          {startupError && <button onClick={() => { setStartupError(''); setStartupAttempt((value) => value + 1) }}>Retry</button>}
        </div>
      )}

      {tab === 'plan' && planPanelOpen && (
        <PlanPanel
          plan={plan}
          onChange={setPlan}
          onOpen={openPlanPanel}
          ready={Boolean(route)}
          busy={busy}
          onSave={() =>
            ask({
              title: 'Name your ride',
              initial: plan.name === 'New route' ? '' : plan.name,
              action: async (name) => {
                const track = planTrack()
                if (!track) return
                await saveTrack({ ...track, name }, toGpxFilename(name), false)
                setPlan((current) => ({ ...current, name }))
              },
              label: 'Save ride',
            })
          }
          onShare={() =>
            void run(async () => {
              const track = planTrack()
              if (track) await share(track)
            })
          }
          onNew={() =>
            ask({
              title: 'Start a new route?',
              message: 'This removes the current route controls.',
              action: () => setPlan(emptyPlan()),
              label: 'New route',
            })
          }
        />
      )}
      {detailCoordinates && routeInfoOpen && (
        <section className="route-info-overlay" role="dialog" aria-label="Route details">
          <div className="section-heading">
            <h2>Route details</h2>
            <button className="icon-button" aria-label="Close route details" onClick={() => setRouteInfoOpen(false)}>
              <Icon name="close" size={20} />
            </button>
          </div>
          <TrackSummary coordinates={detailCoordinates} duration={tab === 'plan' ? route?.durationSeconds : undefined} />
          <RouteSurface
            coordinates={detailCoordinates}
            surface={tab === 'plan' ? route?.surface : undefined}
            runtime={runtime}
            routing={routing}
          />
        </section>
      )}
      {tab === 'library' && (
        <section className="bottom-sheet" aria-label={`${tab} panel`}>
          <button
            className="sheet-handle"
            aria-label={expanded ? 'Collapse panel' : 'Expand panel'}
            onClick={() => setExpanded((value) => !value)}
          >
            <span />
          </button>
          <div className="sheet-heading">
            <div>
              <div className="eyebrow">
                {document
                  ? document.dirty
                    ? 'UNSAVED CHANGES'
                    : 'TRACK LIBRARY'
                  : 'TRACK LIBRARY'}
              </div>
              <h1>{sheetTitle}</h1>
            </div>
            {tab === 'library' && document ? (
              <button
                className="icon-button"
                aria-label="Back to library"
                onClick={() => {
                  if (document.dirty)
                    ask({
                      title: 'Close this track?',
                      message: 'Unsaved edits will be discarded.',
                      action: () => {
                        setDocument(null)
                        setTools(false)
                      },
                      label: 'Close track',
                    })
                  else {
                    setDocument(null)
                    setTools(false)
                  }
                }}
              >
                <Icon name="close" />
              </button>
            ) : (
              <span className="heading-mark">
                <Icon name={tab} size={24} />
              </span>
            )}
          </div>
          <div className="sheet-content">
            {tab === 'library' && !document && (
              <div className="panel-stack">
                <button className="primary" disabled={busy} onClick={importFile}>
                  <Icon name="plus" size={19} />
                  Import GPX
                </button>
                <label className="search-field">
                  <Icon name="search" size={18} />
                  <input
                    placeholder="Find a track"
                    aria-label="Filter tracks"
                    value={filter}
                    onChange={(event) => setFilter(event.target.value)}
                  />
                </label>
                {visibleFiles.map((filename) => (
                  <div className="track-card" key={filename}>
                    <button
                      className="track-open"
                      disabled={busy}
                      onClick={() =>
                        void run(async () => {
                          const response = await fetch(`/gpx/${encodeURIComponent(filename)}`)
                          if (!response.ok) throw new Error('Could not open track')
                          importContent(await response.text(), filename, true)
                        })
                      }
                    >
                      <span className="track-art">
                        <Icon name="mountain" size={30} />
                      </span>
                      <span>
                        <strong>{fromGpxFilename(filename)}</strong>
                        <small>GPX track · on this device</small>
                      </span>
                      <Icon name="arrow" size={18} />
                    </button>
                    <button
                      className="icon-button track-delete"
                      aria-label={`Delete ${filename}`}
                      disabled={busy}
                      onClick={() => deleteTrack(filename)}
                    >
                      <Icon name="trash" size={20} />
                    </button>
                  </div>
                ))}
                {!files.length && runtimeReady && (
                  <div className="empty-state">
                    <Icon name="mountain" size={42} />
                    <h3>A little inspiration?</h3>
                    <p>
                      Bring a GPX from a friend, or plan your first ride. Your tracks live right
                      here, ready for the next trip.
                    </p>
                  </div>
                )}
              </div>
            )}

            {tab === 'library' && document && (
              <div className="panel-stack">
                <TrackSummary coordinates={document.track.coordinates} />
                <button className="secondary" onClick={() => {
                  setLayersOpen(false)
                  setRouteInfoOpen(true)
                }}>
                  <Icon name="info" size={18} />
                  Route details
                </button>
                <div className="track-meta">
                  {document.track.coordinates.length.toLocaleString()} points ·{' '}
                  {document.track.waypoints.length} saved places
                  {document.track.coordinates.some((p) => p.elevationInterpolated) &&
                    ' · interpolated elevations'}
                </div>
                <div className="button-pair">
                  <button className="primary" disabled={busy} onClick={saveDocument}>
                    <Icon name="save" size={18} />
                    {document.libraryFilename ? 'Save edits' : 'Save to library'}
                  </button>
                  <button
                    className="secondary"
                    disabled={busy}
                    onClick={() => void run(() => share(document.track))}
                  >
                    <Icon name="share" size={18} />
                    Share GPX
                  </button>
                </div>
                <button
                  className="secondary"
                  onClick={() => {
                    setTools((value) => !value)
                    setExpanded(true)
                  }}
                >
                  <Icon name="edit" size={18} />
                  {tools ? 'Hide tools' : 'Edit track'}
                </button>
                {tools && (
                  <>
                    <div className="section-heading">
                      <h3>Make it yours</h3>
                      <button
                        className="text-button"
                        disabled={!history.length}
                        onClick={() => {
                          const previous = history[history.length - 1]
                          setDocument(previous)
                          setHistory(history.slice(0, -1))
                          fit(previous.track.coordinates)
                        }}
                      >
                        <Icon name="undo" size={16} />
                        Undo
                      </button>
                    </div>
                    <div className="tool-grid">
                      <button onClick={() => edit(reverseTrack(document.track))}>
                        <Icon name="reverse" />
                        <span>Reverse</span>
                      </button>
                      <button
                        onClick={() =>
                          ask({
                            title: 'Simplify track',
                            initial: '1000',
                            message:
                              'Maximum number of points. Geometry is simplified with the shared GPX editor.',
                            action: (value) => {
                              const count = Number(value)
                              if (!Number.isInteger(count) || count < 2)
                                throw new Error('Enter a whole number of at least 2')
                              edit(simplifyToMaxPoints(document.track, count))
                            },
                            label: 'Simplify',
                          })
                        }
                      >
                        <Icon name="plan" />
                        <span>Simplify</span>
                      </button>
                      <button onClick={refreshElevation} disabled={busy}>
                        <Icon name="mountain" />
                        <span>Refresh elevation</span>
                      </button>
                      <button
                        onClick={() =>
                          ask({
                            title: 'Split into days',
                            initial: '150',
                            message: 'Approximate distance per day, in kilometres.',
                            action: (value) => {
                              const km = Number(value)
                              if (!Number.isFinite(km) || km <= 0)
                                throw new Error('Enter a positive distance')
                              split(splitIntoStages(document.track, km))
                            },
                            label: 'Split',
                          })
                        }
                      >
                        <Icon name="library" />
                        <span>Day stages</span>
                      </button>
                    </div>
                    <div className="crop-card">
                      <h3>Keep the good part</h3>
                      <label>
                        Start · {range[0]}%
                        <input
                          aria-label="Crop start"
                          type="range"
                          min="0"
                          max={range[1] - 1}
                          value={range[0]}
                          onChange={(e) => setRange([Number(e.target.value), range[1]])}
                        />
                      </label>
                      <label>
                        End · {range[1]}%
                        <input
                          aria-label="Crop end"
                          type="range"
                          min={range[0] + 1}
                          max="100"
                          value={range[1]}
                          onChange={(e) => setRange([range[0], Number(e.target.value)])}
                        />
                      </label>
                      <div className="button-pair">
                        <button
                          className="secondary"
                          disabled={endIndex - startIndex < 1}
                          onClick={() => edit(trimTrack(document.track, startIndex, endIndex))}
                        >
                          <Icon name="scissors" size={17} />
                          Crop
                        </button>
                        <button
                          className="secondary"
                          disabled={document.track.coordinates.length < 3}
                          onClick={() =>
                            split(
                              splitTrack(
                                document.track,
                                Math.max(
                                  1,
                                  Math.min(
                                    document.track.coordinates.length - 2,
                                    startIndex || Math.floor(document.track.coordinates.length / 2),
                                  ),
                                ),
                              ),
                            )
                          }
                        >
                          Split at start
                        </button>
                      </div>
                    </div>
                    <button
                      className="secondary"
                      onClick={() =>
                        ask({
                          title: 'Mark this place',
                          initial: '',
                          message: 'Adds the map centre as a waypoint in this GPX.',
                          action: (name) =>
                            edit({
                              ...document.track,
                              waypoints: [
                                ...document.track.waypoints,
                                { ...center, name, sym: 'Flag, Blue' },
                              ],
                            }),
                          label: 'Add place',
                        })
                      }
                    >
                      <Icon name="plus" size={18} />
                      Add place at map centre
                    </button>
                  </>
                )}
                <button className="text-button" onClick={importFile}>
                  Open another GPX
                </button>
              </div>
            )}

            {canPersist && (
              <div className="draft-status">
                {draftSaved ? 'Draft saved on this device' : 'Saving draft…'}
              </div>
            )}
          </div>
        </section>
      )}
      <nav className="bottom-nav" aria-label="Main navigation">
        {tabs.map((item) => (
          <button
            key={item.id}
            className={tab === item.id ? 'active' : ''}
            onClick={() => {
              setTab(item.id)
              setExpanded(item.id === 'library' && !document)
              setPlanPanelOpen(false)
              setRouteInfoOpen(false)
              setLayersOpen(false)
            }}
          >
            <Icon name={item.id} size={22} />
            <span>{item.label}</span>
          </button>
        ))}
      </nav>
      {shared.pending && (
        <aside className="incoming-gpx" aria-label="Shared GPX">
          <div>
            <strong>{shared.working ? 'Opening shared GPX…' : 'Shared GPX received'}</strong>
            <span>{shared.pending.filename}</span>
            {shared.error && <small>{shared.error}</small>}
          </div>
          <button className="text-button" disabled={busy || shared.working || !!dialog || choices.length > 0}
            onClick={() => confirmReplace(shared.open)}>Open shared GPX</button>
          <button className="icon-button" aria-label="Dismiss shared GPX" disabled={shared.working}
            onClick={() => void shared.dismiss()}><Icon name="close" size={18} /></button>
        </aside>
      )}
      {toast && (
        <div className="toast" role="status">
          <span>{toast}</span>
          <button aria-label="Dismiss notification" onClick={() => setToast('')}>
            <Icon name="close" size={17} />
          </button>
        </div>
      )}
      {busy && (
        <div className="busy-indicator" role="status">
          <span className="spinner" />
          Working…
        </div>
      )}
      <input
        type="file"
        ref={fileInput}
        accept=".gpx,application/gpx+xml"
        hidden
        aria-label="Open GPX file"
        onChange={(event) => {
          const file = event.target.files?.[0]
          event.target.value = ''
          if (file)
            void run(async () => {
              if (file.size > 16 * 1024 * 1024) throw new Error('GPX exceeds 16 MiB')
              importContent(await file.text(), file.name)
            })
        }}
      />
      {choices.length > 0 && (
        <div className="modal-backdrop">
          <section className="modal" role="dialog" aria-modal="true" aria-label="Choose track">
            <div className="section-heading">
              <h2>Choose a track</h2>
              <button
                className="icon-button"
                aria-label="Close track chooser"
                onClick={() => setChoices([])}
              >
                <Icon name="close" />
              </button>
            </div>
            <p className="muted">Each part is saved as its own file.</p>
            {choices.map((choice, index) => (
              <div className="choice-row" key={index}>
                <button className="list-row" onClick={() => openDocument(choice)}>
                  <Icon name="mountain" />
                  <span>
                    {choice.track.name}
                    <small>{choice.track.coordinates.length} points</small>
                  </span>
                  <Icon name="arrow" />
                </button>
                <button
                  className="icon-button"
                  aria-label={`Save part ${index + 1}`}
                  disabled={busy}
                  onClick={() =>
                    void run(() => saveTrack(choice.track, choice.track.filename, false))
                  }
                >
                  <Icon name="save" size={18} />
                </button>
              </div>
            ))}
          </section>
        </div>
      )}
      {dialog && (
        <div className="modal-backdrop">
          <form
            className="modal"
            role="dialog"
            aria-modal="true"
            aria-label={dialog.title}
            onSubmit={(event) => {
              event.preventDefault()
              void run(async () => {
                if (dialog.initial !== undefined && !dialogValue.trim())
                  throw new Error('Enter a value')
                await dialog.action(dialogValue.trim())
                setDialog(null)
              })
            }}
          >
            <h2>{dialog.title}</h2>
            {dialog.message && <p className="muted">{dialog.message}</p>}
            {dialog.initial !== undefined && (
              <input
                className="text-input"
                autoFocus
                aria-label={dialog.title}
                value={dialogValue}
                onChange={(e) => setDialogValue(e.target.value)}
              />
            )}
            <div className="button-pair">
              <button type="button" className="secondary" onClick={() => setDialog(null)}>
                Cancel
              </button>
              <button className="primary" disabled={busy} type="submit">
                {dialog.label ?? 'Continue'}
              </button>
            </div>
          </form>
        </div>
      )}
    </main>
  )
}
