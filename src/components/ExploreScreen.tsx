import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import type { FormEvent, KeyboardEvent as ReactKeyboardEvent, ReactNode } from 'react'
import { MapContainer, Marker, Pane, Popup, Rectangle, ZoomControl, useMap } from 'react-leaflet'
import L from 'leaflet'
import { MapTiles, VectorMapDiagnostic } from './MapLayers'
import type { VectorMapIssue } from './MapLayers'
import { OfflineAreaPanel } from './OfflineAreaPanel'
import { OfflineAreasPanel } from './OfflineAreasPanel'
import { ESCAPE_PRIORITY, useEscapeDismiss } from './useEscapeDismiss'
import { searchPlaces } from '../lib/geocoding'
import type { PlaceResult } from '../lib/geocoding'
import { fetchOfflineStatus, fetchPacks, formatCacheContext, normalizePackBounds } from '../lib/offline'
import type { CacheMetadata, OfflineStatus, PackBounds, PackSummary, RuntimeConfig } from '../lib/offline'
import type { BaseLayerDefinition, ThumbnailLayerDefinition } from '../lib/terrain'

const DEFAULT_VIEW = { center: [41.65, -0.88] as [number, number], zoom: 7 }
const ACTIVE_PACK_STATES = new Set(['queued', 'running', 'cancelling'])

function initialExploreView(): typeof DEFAULT_VIEW {
  try {
    const stored = JSON.parse(localStorage.getItem('gpx-explore-view') ?? 'null') as unknown
    if (stored && typeof stored === 'object') {
      const value = stored as { lat?: unknown; lon?: unknown; zoom?: unknown }
      if (typeof value.lat === 'number' && Number.isFinite(value.lat) &&
          typeof value.lon === 'number' && Number.isFinite(value.lon) &&
          typeof value.zoom === 'number' && Number.isFinite(value.zoom)) {
        return { center: [value.lat, value.lon], zoom: Math.max(1, Math.min(19, value.zoom)) }
      }
    }
  } catch {
    // A malformed local preference should never keep the map from opening.
  }
  return DEFAULT_VIEW
}

function MapStateReporter({
  onZoom,
  onCursor,
}: {
  onZoom: (zoom: number) => void
  onCursor: (position: { lat: number; lon: number } | null) => void
}) {
  const map = useMap()
  useEffect(() => {
    const persist = () => {
      const center = map.getCenter()
      const zoom = map.getZoom()
      onZoom(zoom)
      localStorage.setItem('gpx-explore-view', JSON.stringify({ lat: center.lat, lon: center.lng, zoom }))
    }
    const move = (event: L.LeafletMouseEvent) => onCursor({ lat: event.latlng.lat, lon: event.latlng.lng })
    const leave = () => onCursor(null)
    persist()
    map.on('moveend zoomend', persist)
    map.on('mousemove', move)
    map.getContainer().addEventListener('mouseleave', leave)
    return () => {
      map.off('moveend zoomend', persist)
      map.off('mousemove', move)
      map.getContainer().removeEventListener('mouseleave', leave)
    }
  }, [map, onCursor, onZoom])
  return null
}

function AreaSelector({
  active,
  onSelected,
  onCancel,
  onInvalid,
}: {
  active: boolean
  onSelected: (bounds: PackBounds) => void
  onCancel: () => void
  onInvalid: () => void
}) {
  const map = useMap()
  useEscapeDismiss(active, onCancel, ESCAPE_PRIORITY.nested)
  useEffect(() => {
    if (!active) return
    const container = map.getContainer()
    const previousTouchAction = container.style.touchAction
    const draggingEnabled = map.dragging.enabled()
    const boxZoomEnabled = map.boxZoom.enabled()
    let pointerID: number | null = null
    let startPoint: L.Point | null = null
    let startLatLng: L.LatLng | null = null
    let draft: L.Rectangle | null = null

    const mapPoint = (event: PointerEvent) => {
      const rect = container.getBoundingClientRect()
      return L.point(event.clientX - rect.left, event.clientY - rect.top)
    }
    const finish = (event: PointerEvent) => {
      if (pointerID !== event.pointerId || !startPoint || !startLatLng) return
      event.preventDefault()
      const endPoint = mapPoint(event)
      const endLatLng = map.containerPointToLatLng(endPoint)
      try { container.releasePointerCapture(event.pointerId) } catch { /* already released */ }
      pointerID = null
      draft?.remove()
      draft = null
      if (startPoint.distanceTo(endPoint) < 12) {
        onInvalid()
        return
      }
      const selected = normalizePackBounds(
        { lat: startLatLng.lat, lon: startLatLng.lng },
        { lat: endLatLng.lat, lon: endLatLng.lng },
      )
      if (selected) onSelected(selected)
      else onInvalid()
    }
    const pointerCancel = (event: PointerEvent) => {
      if (pointerID !== event.pointerId) return
      try { container.releasePointerCapture(event.pointerId) } catch { /* already released */ }
      pointerID = null
      startPoint = null
      startLatLng = null
      draft?.remove()
      draft = null
      onCancel()
    }
    const pointerDown = (event: PointerEvent) => {
      if (!event.isPrimary || event.button !== 0) return
      event.preventDefault()
      pointerID = event.pointerId
      startPoint = mapPoint(event)
      startLatLng = map.containerPointToLatLng(startPoint)
      container.setPointerCapture(event.pointerId)
      draft = L.rectangle(L.latLngBounds(startLatLng, startLatLng), {
        color: '#38bdf8',
        weight: 2,
        dashArray: '8 6',
        fillColor: '#0ea5e9',
        fillOpacity: 0.16,
        interactive: false,
      }).addTo(map)
    }
    const pointerMove = (event: PointerEvent) => {
      if (pointerID !== event.pointerId || !startLatLng || !draft) return
      event.preventDefault()
      draft.setBounds(L.latLngBounds(startLatLng, map.containerPointToLatLng(mapPoint(event))))
    }
    container.classList.add('selecting-offline-area')
    container.style.touchAction = 'none'
    map.dragging.disable()
    map.boxZoom.disable()
    container.addEventListener('pointerdown', pointerDown)
    container.addEventListener('pointermove', pointerMove)
    container.addEventListener('pointerup', finish)
    container.addEventListener('pointercancel', pointerCancel)
    return () => {
      draft?.remove()
      container.classList.remove('selecting-offline-area')
      container.style.touchAction = previousTouchAction
      if (draggingEnabled) map.dragging.enable()
      if (boxZoomEnabled) map.boxZoom.enable()
      container.removeEventListener('pointerdown', pointerDown)
      container.removeEventListener('pointermove', pointerMove)
      container.removeEventListener('pointerup', finish)
      container.removeEventListener('pointercancel', pointerCancel)
    }
  }, [active, map, onCancel, onInvalid, onSelected])
  return null
}

function PlaceSearch({
  runtime,
  api,
  selected,
  onSelect,
  onCacheMetadata,
}: {
  runtime: RuntimeConfig
  api: string
  selected: PlaceResult | null
  onSelect: (place: PlaceResult) => void
  onCacheMetadata: (metadata: CacheMetadata) => void
}) {
  const [query, setQuery] = useState('')
  const [results, setResults] = useState<PlaceResult[]>([])
  const [cache, setCache] = useState<CacheMetadata | undefined>()
  const [loading, setLoading] = useState(false)
  const [searched, setSearched] = useState(false)
  const [error, setError] = useState('')
  const [activeIndex, setActiveIndex] = useState(-1)
  const sequenceRef = useRef(0)
  const abortRef = useRef<AbortController | null>(null)
  const dismissResults = useCallback(() => {
    sequenceRef.current++
    abortRef.current?.abort()
    abortRef.current = null
    setResults([])
    setCache(undefined)
    setLoading(false)
    setSearched(false)
    setError('')
    setActiveIndex(-1)
  }, [])
  const resultsOpen = results.length > 0 || Boolean(error) || (searched && !loading)
  useEscapeDismiss(resultsOpen, dismissResults, ESCAPE_PRIORITY.popover)

  useEffect(() => () => abortRef.current?.abort(), [])

  const submit = async (event?: FormEvent) => {
    event?.preventDefault()
    if (!query.trim()) return
    const sequence = ++sequenceRef.current
    abortRef.current?.abort()
    const controller = new AbortController()
    abortRef.current = controller
    setLoading(true)
    setSearched(true)
    setError('')
    setCache(undefined)
    setActiveIndex(-1)
    try {
      const next = await searchPlaces(query, controller.signal, api, {
        runtime,
        onCacheMetadata: metadata => {
          if (sequence !== sequenceRef.current) return
          setCache(metadata)
          onCacheMetadata(metadata)
        },
      })
      if (sequence === sequenceRef.current) setResults(next)
    } catch (reason) {
      if ((reason as Error).name !== 'AbortError' && sequence === sequenceRef.current) {
        setResults([])
        setError((reason as Error).message || 'Place search is unavailable')
      }
    } finally {
      if (sequence === sequenceRef.current) {
        setLoading(false)
        abortRef.current = null
      }
    }
  }

  const choose = (place: PlaceResult) => {
    setResults([])
    setSearched(false)
    setActiveIndex(-1)
    onSelect(place)
  }

  const handleKeyDown = (event: ReactKeyboardEvent<HTMLInputElement>) => {
    if (event.key === 'ArrowDown' && results.length > 0) {
      event.preventDefault()
      setActiveIndex(index => Math.min(results.length - 1, index + 1))
    } else if (event.key === 'ArrowUp' && results.length > 0) {
      event.preventDefault()
      setActiveIndex(index => Math.max(0, index - 1))
    } else if (event.key === 'Enter' && activeIndex >= 0 && results[activeIndex]) {
      event.preventDefault()
      choose(results[activeIndex])
    } else if (event.key === 'Escape') {
      event.preventDefault()
      dismissResults()
    }
  }

  return (
    <div className="explore-search-shell">
      <form className="explore-search" onSubmit={event => void submit(event)} role="search">
        <svg viewBox="0 0 24 24" aria-hidden="true"><path d="m21 21-4.35-4.35m2.1-5.4a7.5 7.5 0 1 1-15 0 7.5 7.5 0 0 1 15 0Z" /></svg>
        <input
          type="search"
          value={query}
          onChange={event => setQuery(event.target.value)}
          onKeyDown={handleKeyDown}
          placeholder="Search towns, roads and places"
          aria-label="Search OpenStreetMap places"
          aria-expanded={results.length > 0}
          aria-controls="explore-place-results"
          aria-activedescendant={activeIndex >= 0 ? `explore-place-${results[activeIndex]?.place_id}` : undefined}
        />
        {loading ? <span className="place-search-spinner" aria-label="Searching" /> : <button type="submit" disabled={!query.trim()}>Search</button>}
      </form>

      {(results.length > 0 || error || (searched && !loading)) && (
        <div className="explore-search-results" id="explore-place-results" role="listbox" aria-label="Place results">
          <div className="explore-search-status" aria-live="polite">
            <span>{error || (results.length > 0 ? `${results.length} result${results.length === 1 ? '' : 's'}` : 'No places found')}</span>
            {cache && <em title={formatCacheContext(cache)}>{formatCacheContext(cache, true)}</em>}
          </div>
          {results.map((place, index) => (
            <button
              id={`explore-place-${place.place_id}`}
              type="button"
              role="option"
              aria-selected={selected?.place_id === place.place_id}
              className={selected?.place_id === place.place_id || activeIndex === index ? 'is-selected' : ''}
              key={place.place_id}
              onMouseEnter={() => setActiveIndex(index)}
              onClick={() => choose(place)}
            >
              <span className="explore-result-pin">●</span>
              <span><strong>{place.display_name.split(',')[0]}</strong><small>{place.display_name}</small></span>
            </button>
          ))}
        </div>
      )}
    </div>
  )
}

function selectionRectangles(bounds: PackBounds | null): L.LatLngBoundsExpression[] {
  if (!bounds) return []
  if (bounds.west < bounds.east) {
    return [[
      [bounds.south, bounds.west],
      [bounds.north, bounds.east],
    ]]
  }
  return [
    [[bounds.south, bounds.west], [bounds.north, 180]],
    [[bounds.south, -180], [bounds.north, bounds.east]],
  ]
}

export function ExploreScreen({
  runtime,
  nominatimApi,
  baseLayerId,
  hillshade,
  hillshadeOpacity,
  layers,
  hillshadeLayer,
  terrainControls,
  modeAction,
  themeAction,
  vectorIssue,
  onVectorStatus,
  onDismissVectorIssue,
  onHome,
  onCacheMetadata,
  onNotify,
}: {
  runtime: RuntimeConfig
  nominatimApi: string
  baseLayerId: string
  hillshade: boolean
  hillshadeOpacity: number
  layers: BaseLayerDefinition[]
  hillshadeLayer?: ThumbnailLayerDefinition
  terrainControls: ReactNode
  modeAction: ReactNode
  themeAction: ReactNode
  vectorIssue: VectorMapIssue | null
  onVectorStatus: (issue: VectorMapIssue | null) => void
  onDismissVectorIssue: () => void
  onHome: () => void
  onCacheMetadata: (metadata: CacheMetadata) => void
  onNotify: (message: string, type?: 'info' | 'success' | 'error') => void
}) {
  const initial = useRef(initialExploreView()).current
  const [map, setMap] = useState<L.Map | null>(null)
  const [zoom, setZoom] = useState(initial.zoom)
  const [cursor, setCursor] = useState<{ lat: number; lon: number } | null>(null)
  const [selectedPlace, setSelectedPlace] = useState<PlaceResult | null>(null)
  const [area, setArea] = useState<PackBounds | null>(null)
  const [selectingArea, setSelectingArea] = useState(false)
  const [offlineStatus, setOfflineStatus] = useState<OfflineStatus | null>(null)
  const [packs, setPacks] = useState<PackSummary[]>([])
  const [offlineError, setOfflineError] = useState('')
  const [coverageVisible, setCoverageVisible] = useState(true)
  useEscapeDismiss(selectedPlace !== null, () => setSelectedPlace(null), ESCAPE_PRIORITY.passive)

  const downloadedAreas = useMemo(
    () => packs.filter(pack => pack.bbox),
    [packs],
  )
  const completedAreas = useMemo(
    () => downloadedAreas.filter(pack => pack.status === 'complete' && pack.bbox),
    [downloadedAreas],
  )

  const refreshOffline = useCallback(async (signal?: AbortSignal) => {
    if (!runtime.offline) {
      setOfflineStatus(null)
      setPacks([])
      return
    }
    const [nextStatus, nextPacks] = await Promise.all([
      fetchOfflineStatus(runtime, signal),
      fetchPacks(runtime, signal),
    ])
    setOfflineStatus(nextStatus)
    setPacks(nextPacks)
    setOfflineError('')
  }, [runtime])

  useEffect(() => {
    const controller = new AbortController()
    void refreshOffline(controller.signal).catch(reason => {
      if ((reason as Error).name !== 'AbortError') setOfflineError((reason as Error).message)
    })
    return () => controller.abort()
  }, [refreshOffline])

  const hasActiveAreaPack = downloadedAreas.some(pack => ACTIVE_PACK_STATES.has(pack.status))
  useEffect(() => {
    if (!hasActiveAreaPack) return
    const controller = new AbortController()
    let timer = 0
    const poll = async () => {
      try {
        await refreshOffline(controller.signal)
      } catch (reason) {
        if ((reason as Error).name !== 'AbortError') setOfflineError((reason as Error).message)
      } finally {
        if (!controller.signal.aborted) timer = window.setTimeout(() => void poll(), 750)
      }
    }
    timer = window.setTimeout(() => void poll(), 750)
    return () => {
      controller.abort()
      window.clearTimeout(timer)
    }
  }, [hasActiveAreaPack, refreshOffline])

  const handlePacksChanged = useCallback(async (pack?: PackSummary) => {
    if (pack) setPacks(current => [pack, ...current.filter(candidate => candidate.id !== pack.id)])
    try {
      await refreshOffline()
    } catch (reason) {
      setOfflineError((reason as Error).message)
    }
  }, [refreshOffline])

  const showDownloadedAreas = useCallback(() => setCoverageVisible(true), [])

  const viewDownloadedArea = useCallback((pack: PackSummary) => {
    if (!map || !pack.bbox) return
    const { south, west, north, east } = pack.bbox
    const continuousEast = east < west ? east + 360 : east
    setCoverageVisible(true)
    setSelectingArea(false)
    setArea(null)
    setSelectedPlace(null)
    map.flyToBounds(
      [[south, west], [north, continuousEast]],
      { padding: [54, 54], maxZoom: 15, duration: 1.2 },
    )
  }, [map])

  const selectPlace = useCallback((place: PlaceResult) => {
    setSelectedPlace(place)
    if (!map) return
    if (place.bounds) {
      map.flyToBounds(
        [[place.bounds.south, place.bounds.west], [place.bounds.north, place.bounds.east]],
        { padding: [54, 54], maxZoom: 15, duration: 1.2 },
      )
    } else {
      map.flyTo([place.lat, place.lon], 13, { duration: 1.2 })
    }
  }, [map])

  const completeArea = useCallback((bounds: PackBounds) => {
    setArea(bounds)
    setSelectingArea(false)
  }, [])
  const cancelArea = useCallback(() => setSelectingArea(false), [])
  const invalidArea = useCallback(() => {
    onNotify('Drag a larger rectangle to select an offline area', 'info')
  }, [onNotify])

  return (
    <div className="explore-screen" data-testid="explore-screen">
      <div className="explore-map" data-testid="explore-map">
        <MapContainer
          center={initial.center}
          zoom={initial.zoom}
          minZoom={1}
          maxZoom={19}
          scrollWheelZoom
          zoomControl={false}
          style={{ width: '100%', height: '100%' }}
          ref={instance => { if (instance) setMap(instance) }}
        >
          <MapTiles
            baseLayerId={baseLayerId}
            hillshade={hillshade}
            hillshadeOpacity={hillshadeOpacity}
            onVectorStatus={onVectorStatus}
            layers={layers}
            hillshadeLayer={hillshadeLayer}
          />
          <MapStateReporter onZoom={setZoom} onCursor={setCursor} />
          <ZoomControl position="bottomright" />
          <AreaSelector active={selectingArea} onSelected={completeArea} onCancel={cancelArea} onInvalid={invalidArea} />
          {coverageVisible && completedAreas.length > 0 && (
            <Pane name="offline-coverage" style={{ zIndex: 245 }}>
              {completedAreas.flatMap(pack => selectionRectangles(pack.bbox ?? null).map((bounds, index) => (
                <Rectangle
                  key={`${pack.id}:${index}`}
                  bounds={bounds}
                  pathOptions={{ color: '#047857', weight: 1.5, fillColor: '#10b981', fillOpacity: 0.12 }}
                  interactive={false}
                />
              )))}
            </Pane>
          )}
          {selectionRectangles(area).map((bounds, index) => (
            <Rectangle
              key={`${area?.west}:${area?.east}:${index}`}
              bounds={bounds}
              pathOptions={{ color: '#0284c7', weight: 2, dashArray: '8 5', fillColor: '#38bdf8', fillOpacity: 0.13 }}
              interactive={false}
            />
          ))}
          {selectedPlace && (
            <Marker position={[selectedPlace.lat, selectedPlace.lon]}>
              <Popup>
                <strong>{selectedPlace.display_name.split(',')[0]}</strong>
                <span className="explore-popup-detail">{selectedPlace.display_name}</span>
              </Popup>
            </Marker>
          )}
        </MapContainer>

        <div className="explore-toolbar">
          <button className="explore-home" onClick={onHome} aria-label="Back to home" title="Back to home">
            <svg viewBox="0 0 24 24" aria-hidden="true"><path d="M20 11H7.83l5.59-5.59L12 4l-8 8 8 8 1.41-1.41L7.83 13H20v-2z" /></svg>
          </button>
          <div className="explore-title"><strong>Explore</strong><span>OpenStreetMap</span></div>
          <PlaceSearch runtime={runtime} api={nominatimApi} selected={selectedPlace} onSelect={selectPlace} onCacheMetadata={onCacheMetadata} />
          <div className="explore-theme explore-actions">
            <OfflineAreaPanel
              runtime={runtime}
              bounds={area}
              selecting={selectingArea}
              currentZoom={zoom}
              suggestedName={selectedPlace?.display_name}
              status={offlineStatus}
              packs={packs}
              onToggleSelection={() => { setCoverageVisible(true); setSelectingArea(value => !value) }}
              onClearSelection={() => { setArea(null); setSelectingArea(false) }}
              onOpen={showDownloadedAreas}
              onPacksChanged={handlePacksChanged}
            />
            <OfflineAreasPanel
              runtime={runtime}
              packs={packs}
              coverageVisible={coverageVisible}
              onCoverageVisible={setCoverageVisible}
              onViewArea={viewDownloadedArea}
              onPacksChanged={handlePacksChanged}
              loadError={offlineError}
            />
            {modeAction}
            {themeAction}
          </div>
        </div>

        <div className="map-control-stack explore-control-stack" data-testid="map-control-stack">
          {terrainControls}
        </div>

        {selectingArea && <div className="explore-selection-hint">Drag to frame the area you want offline <span>Esc to cancel</span></div>}
        {cursor && <div className="map-cursor-readout">{cursor.lat.toFixed(5)}, {cursor.lon.toFixed(5)}</div>}
        <VectorMapDiagnostic
          issue={baseLayerId === 'openfreemap' ? vectorIssue : null}
          onDismiss={onDismissVectorIssue}
        />
        {selectedPlace && (
          <div className="explore-place-card">
            <span>Selected place</span>
            <strong>{selectedPlace.display_name.split(',')[0]}</strong>
            <small>{selectedPlace.display_name}</small>
          </div>
        )}
      </div>
    </div>
  )
}
