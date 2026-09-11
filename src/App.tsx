import { useState, useRef, useEffect, useCallback, useMemo } from 'react'
import { MapContainer, Marker, Polyline, Popup, ZoomControl, useMap } from 'react-leaflet'
import L from 'leaflet'

import { ColoredTrack } from './components/ColoredTrack'
import { ElevationProfile } from './components/ElevationProfile'
import type { ProfileBar, TrimSelection } from './components/ElevationProfile'
import { ExploreScreen } from './components/ExploreScreen'
import { MapTiles, TerrainControls, VectorMapDiagnostic } from './components/MapLayers'
import type { VectorMapIssue } from './components/MapLayers'
import { OfflineStoragePanel } from './components/OfflineStoragePanel'
import { SplashScreen } from './components/SplashScreen'
import { TrackCard } from './components/TrackCard'
import type { TrackPreview } from './components/TrackCard'
import { ESCAPE_PRIORITY, useEscapeDismiss } from './components/useEscapeDismiss'
import { useMcpBridge } from './useMcpBridge'

import {
  ElevationUnavailableError,
  fetchElevationProfile,
  fetchGroundElevation,
} from './lib/elevation'
import {
  reverseTrack,
  simplifyToMaxPoints,
  smoothTrackElevation,
  splitIntoStages,
  splitTrack,
  trimTrack,
  withElevations,
} from './lib/edit'
import {
  calculateDistance,
  calculateElevationStats,
  calculateTimeStats,
  cumulativeDistanceKm,
  longestGapKm,
  slopePercent,
  smoothElevations,
} from './lib/geo'
import { DEFAULT_NOMINATIM_API, searchPlaces } from './lib/geocoding'
import type { PlaceResult } from './lib/geocoding'
import { buildGPX, fromGpxFilename, parseGPX, toGpxFilename } from './lib/gpx'
import {
  boundingBoxSpanKm,
  boundsAround,
  fetchPoisForArea,
  MAX_SEARCH_SPAN_KM,
  POI_KINDS,
} from './lib/poi'
import type { BoundingBox, Poi, PoiKind } from './lib/poi'
import { calculateRoute, formatDuration, ROUTING_PROFILES } from './lib/routing'
import type { RoutingProfile } from './lib/routing'
import { availableFuels, DEFAULT_FUEL_REFERENCE, fuelBandColors, FUEL_PRICE_BANDS } from './lib/fuel'
import { summarizeSurface, surfaceDefinition } from './lib/surface'
import type { SurfaceClass } from './lib/surface'
import { clearedRouteDerivedState, routeSequenceIsCurrent } from './lib/planner'
import type { McpCommand, McpMapMarker } from './lib/mcp'
import {
  altitudeColor,
  getSegmentColor,
  getThumbnailLayer,
  runtimeHillshadeLayer,
  runtimeTerrainLayers,
} from './lib/terrain'
import type { ColorMode } from './lib/terrain'
import type { Coordinate, GpxWaypoint, Track } from './lib/types'
import {
  WAYPOINT_MARKERS,
  waypointMarkerDefinition,
  waypointMarkerIdFromSymbol,
} from './lib/waypointMarkers'
import type { WaypointMarkerId } from './lib/waypointMarkers'
import {
  bootstrapRuntimeConfig,
  fetchRoutingDataStatus,
  formatCacheContext,
  formatCacheDate,
  loadRuntimeConfig,
  setRuntimeOfflineMode,
} from './lib/offline'
import type { CacheMetadata, OfflineMode, RoutingDataStatus } from './lib/offline'

import './App.css'
import RoutingDownloadControl from './components/RoutingDownloadControl'
import MapControlLayout from './components/MapControlLayout'

/* ------------------------------------------------------------------ */
/*  Constants                                                          */
/* ------------------------------------------------------------------ */

// Empty means "same origin": the Go binary serves this bundle and the API
// together. In `npm run dev` the Vite proxy forwards those paths to the
// backend on :8000. Set VITE_API_BASE to point at a backend somewhere else.
const API_BASE = import.meta.env.VITE_API_BASE ?? ''
// Optional direct-to-DEM address for standalone frontend deployments. Once a
// runtime backend is advertised it remains authoritative, including failures.
const ELEVATION_API = import.meta.env.VITE_ELEVATION_API ?? ''
const ELEVATION_DATASET = import.meta.env.VITE_ELEVATION_DATASET ?? 'srtm30m'

function initialRuntimeConfig() {
  const embeddedMode = document.querySelector<HTMLMetaElement>('meta[name="gpx-editor-offline-mode"]')?.content
  return bootstrapRuntimeConfig(API_BASE, embeddedMode)
}

const MAX_PROFILE_BARS = 200
const FLY_TO_DURATION = 1.5
const BAR_ZOOM_LEVEL = 16
/** Wait this long after the last waypoint edit before routing. */
const ROUTE_DEBOUNCE_MS = 350
/** Point budget for a track destined for a GPS unit. */
const DEFAULT_SIMPLIFY_TARGET = 500
/** How far off the route a fuel station still counts as reachable. */
const FUEL_CORRIDOR_M = 3000

function expectedOfflineVectorMiss(issue: VectorMapIssue, mode?: OfflineMode): boolean {
  return mode === 'cache-only' &&
    (issue.code === 'offline_cache_miss' || issue.status === 504) &&
    issue.phase !== 'webgl' && issue.phase !== 'initialization'
}

type ViewMode = 'welcome' | 'upload' | 'creation' | 'explore' | 'view'

function hasInteractiveMap(viewMode: ViewMode): boolean {
  return viewMode === 'creation' || viewMode === 'explore' || viewMode === 'view'
}

function readMapViewport(map: L.Map) {
  const center = map.getCenter()
  const bounds = map.getBounds()
  return {
    center: { lat: center.lat, lon: center.lng },
    zoom: map.getZoom(),
    bounds: {
      south: bounds.getSouth(),
      west: bounds.getWest(),
      north: bounds.getNorth(),
      east: bounds.getEast(),
    },
  }
}

interface TilePrefetch {
  running: boolean
  done: number
  total: number
  skipped?: boolean
  /** The view was wider than the cache budget; only its middle is covered. */
  clamped?: boolean
  reason?: string
}

interface Waypoint {
  id: number
  lat: number
  lon: number
  elevation?: number
}

function CacheContext({
  metadata,
  compact = false,
}: {
  metadata?: CacheMetadata
  compact?: boolean
}) {
  if (!metadata) return null
  return (
    <span
      className={`cache-context${metadata.stale ? ' cache-context--stale' : ''}`}
      title={formatCacheContext(metadata)}
    >
      {formatCacheContext(metadata, compact)}
    </span>
  )
}

/** Points kept for a card thumbnail — plenty of shape, negligible cost. */
const THUMBNAIL_POINTS = 140

/* ------------------------------------------------------------------ */
/*  Map helper components                                              */
/* ------------------------------------------------------------------ */

/**
 * Reports the visible bounds once the map stops moving, so the backend can
 * pull elevation tiles for where you are working before you need them.
 * Debounced: panning across a region should warm the place you land, not
 * every viewport you crossed on the way.
 */
function ViewportReporter({ onSettle }: { onSettle: (b: L.LatLngBounds) => void }) {
  const map = useMap()
  useEffect(() => {
    let timer: number | undefined
    const report = () => {
      window.clearTimeout(timer)
      timer = window.setTimeout(() => onSettle(map.getBounds()), 700)
    }
    report()
    map.on('moveend', report)
    map.on('zoomend', report)
    return () => {
      window.clearTimeout(timer)
      map.off('moveend', report)
      map.off('zoomend', report)
    }
  }, [map, onSettle])
  return null
}

function MapClickHandler({ onClick }: { onClick: (e: L.LeafletMouseEvent) => void }) {
  const map = useMap()
  useEffect(() => {
    map.on('click', onClick)
    return () => { map.off('click', onClick) }
  }, [map, onClick])
  return null
}

/** Throttle interval for the cursor readout, in ms. */
const CURSOR_THROTTLE_MS = 120

function MapMouseTracker({ onMove }: { onMove: (pos: { lat: number; lon: number } | null) => void }) {
  const map = useMap()
  const lastRef = useRef(0)

  useEffect(() => {
    // Untrottled, this sets state on every mousemove and re-renders a view
    // holding hundreds of track polylines.
    const move = (e: L.LeafletMouseEvent) => {
      const now = Date.now()
      if (now - lastRef.current < CURSOR_THROTTLE_MS) return
      lastRef.current = now
      onMove({ lat: e.latlng.lat, lon: e.latlng.lng })
    }
    const out = () => onMove(null)
    map.on('mousemove', move)
    map.on('mouseout', out)
    return () => { map.off('mousemove', move); map.off('mouseout', out) }
  }, [map, onMove])
  return null
}

function MapJump({ target }: { target: Coordinate | null }) {
  const map = useMap()
  useEffect(() => {
    if (target) {
      map.flyTo([target.lat, target.lon], BAR_ZOOM_LEVEL, {
        duration: FLY_TO_DURATION,
        easeLinearity: 0.1,
      })
    }
  }, [target, map])
  return null
}

function MapRestoreView({
  restore,
  coordinates,
  onRestored,
}: {
  restore: boolean
  coordinates: Coordinate[]
  onRestored: () => void
}) {
  const map = useMap()
  useEffect(() => {
    if (restore && coordinates.length > 0) {
      const bounds = L.latLngBounds(coordinates.map(c => [c.lat, c.lon] as [number, number]))
      map.flyToBounds(bounds, { padding: [40, 40], duration: FLY_TO_DURATION })
      onRestored()
    }
  }, [restore, coordinates, map, onRestored])
  return null
}

function MapFitBounds({ coordinates }: { coordinates: Coordinate[] }) {
  const map = useMap()
  useEffect(() => {
    if (coordinates.length > 0) {
      const bounds = L.latLngBounds(coordinates.map(c => [c.lat, c.lon] as [number, number]))
      map.fitBounds(bounds, { padding: [40, 40] })
    }
  }, [coordinates, map])
  return null
}

// Markers are built from inline HTML — no image assets to resolve at build
// time, and colour-coding costs nothing. Cached so re-renders reuse instances
// rather than handing Leaflet a fresh icon object every time.
const iconCache = new Map<string, L.DivIcon>()

function poiIcon(glyph: string, color: string): L.DivIcon {
  const key = `poi:${glyph}:${color}`
  let icon = iconCache.get(key)
  if (!icon) {
    icon = L.divIcon({
      className: 'poi-marker',
      html: `<span class="poi-marker-inner" style="border-color:${color}">${glyph}</span>`,
      iconSize: [24, 24],
      iconAnchor: [12, 12],
    })
    iconCache.set(key, icon)
  }
  return icon
}

/**
 * What the fuel marker colours mean, and which fuel they are ranked on.
 *
 * The reference matters: an overlander running diesel gets the wrong answer
 * from petrol prices, so the fuel being compared is switchable and always
 * stated rather than assumed.
 */
function FuelPriceLegend({
  pois,
  reference,
  onReference,
}: {
  pois: Poi[]
  reference: string
  onReference: (fuel: string) => void
}) {
  const fuels = useMemo(() => availableFuels(pois), [pois])
  const rankedCount = useMemo(
    () => pois.filter(p => typeof p.prices?.[reference] === 'number').length,
    [pois, reference],
  )

  if (fuels.length === 0) return null

  return (
    <div className="fuel-legend">
      <div className="fuel-legend-head">
        <select
          className="fuel-legend-select"
          value={reference}
          onChange={e => onReference(e.target.value)}
          title="Which fuel the colours rank stations on"
        >
          {fuels.map(fuel => (
            <option key={fuel} value={fuel}>{fuel}</option>
          ))}
        </select>
        <span className="fuel-legend-count">{rankedCount} in view</span>
      </div>
      <div className="fuel-legend-scale">
        {FUEL_PRICE_BANDS.map(band => (
          <span
            key={band.label}
            className="fuel-legend-swatch"
            style={{ background: band.color }}
            title={band.label}
          />
        ))}
      </div>
      <div className="fuel-legend-ends">
        <span>Cheapest</span>
        <span>Dearest</span>
      </div>
    </div>
  )
}

/** POI popup: a name, plus whatever extra a richer source supplied. */
function PoiPopupBody({ poi, fallbackLabel }: { poi: Poi; fallbackLabel: string }) {
  return (
    <div className="poi-popup">
      <strong className="poi-popup-name">{poi.name ?? fallbackLabel}</strong>
      {poi.detail && (
        <>
          {poi.detail.lines.length > 0 && (
            <ul className="poi-popup-prices">
              {poi.detail.lines.map(line => (
                <li key={line.label}>
                  <span>{line.label}</span>
                  <span className="poi-popup-value">{line.value}</span>
                </li>
              ))}
            </ul>
          )}
          {poi.detail.note && <p className="poi-popup-note">{poi.detail.note}</p>}
          {poi.detail.source && <p className="poi-popup-source">{poi.detail.source}</p>}
        </>
      )}
    </div>
  )
}

function waypointIcon(markerId: WaypointMarkerId): L.DivIcon {
  const key = `wpt:${markerId}`
  let icon = iconCache.get(key)
  if (!icon) {
    const marker = waypointMarkerDefinition(markerId)
    icon = L.divIcon({
      className: `gpx-waypoint-marker waypoint-marker-${marker.id}`,
      // Glyph and colour come only from the fixed catalog, never GPX or MCP text.
      html: `<span class="gpx-waypoint-inner" style="background:${marker.color}">${marker.glyph}</span>`,
      iconSize: [24, 24],
      iconAnchor: [12, 12],
    })
    iconCache.set(key, icon)
  }
  return icon
}

function WaypointMarkerSelect({
  symbol,
  onChange,
}: {
  symbol?: string
  onChange: (marker: WaypointMarkerId) => void
}) {
  const markerId = waypointMarkerIdFromSymbol(symbol)
  const imported = markerId === undefined
  return (
    <label className="wpt-marker-select">
      <span>Marker</span>
      <select
        value={markerId ?? '__imported__'}
        onChange={event => {
          if (event.target.value !== '__imported__') onChange(event.target.value as WaypointMarkerId)
        }}
        aria-label="Waypoint marker type"
      >
        {imported && <option value="__imported__" disabled>Imported: {symbol}</option>}
        {WAYPOINT_MARKERS.map(marker => (
          <option key={marker.id} value={marker.id}>{marker.glyph} {marker.label}</option>
        ))}
      </select>
    </label>
  )
}

function SessionMapOverlays({
  track,
  markers,
}: {
  track: Coordinate[]
  markers: McpMapMarker[]
}) {
  return (
    <>
      {track.length > 1 && (
        <Polyline
          positions={track.map(point => [point.lat, point.lon] as [number, number])}
          pathOptions={{ color: '#d946ef', weight: 5, opacity: 0.9, dashArray: '10 7', className: 'session-map-track' }}
          interactive={false}
        />
      )}
      {markers.map((marker, index) => {
        const definition = waypointMarkerDefinition(marker.marker)
        return (
          <Marker
            key={`${marker.lat}:${marker.lon}:${marker.marker}:${index}`}
            position={[marker.lat, marker.lon]}
            icon={waypointIcon(marker.marker)}
          >
            <Popup>
              <strong>{marker.name ?? definition.label}</strong>
              {marker.desc && <><br />{marker.desc}</>}
              <span className="session-overlay-note">Session map marker</span>
            </Popup>
          </Marker>
        )
      })}
    </>
  )
}

/* ------------------------------------------------------------------ */
/*  Notifications                                                      */
/* ------------------------------------------------------------------ */

type NotificationType = 'success' | 'error' | 'info'

interface Notification {
  id: number
  message: string
  type: NotificationType
}

/** How many recent messages the snapshot carries for MCP clients. */
const MAX_RETAINED_MESSAGES = 20

interface RetainedMessage {
  seq: number
  at: string
  type: NotificationType
  message: string
}

function NotificationBar({
  notifications,
  onDismiss,
}: {
  notifications: Notification[]
  onDismiss: (id: number) => void
}) {
  return (
    <div className="notification-container" aria-live="polite" aria-label="Notifications">
      {notifications.map(n => (
        <div key={n.id} className={`notification notification-${n.type}`}>
          <span>{n.message}</span>
          <button onClick={() => onDismiss(n.id)} className="notification-close" aria-label="Dismiss notification">&times;</button>
        </div>
      ))}
    </div>
  )
}

function ThemeIcon({ theme }: { theme: 'light' | 'dark' }) {
  return theme === 'light' ? (
    <svg viewBox="0 0 24 24" width="14" height="14" fill="currentColor">
      <path d="M12 3a9 9 0 1 0 9 9c0-.46-.04-.92-.1-1.36a5.389 5.389 0 0 1-4.4 2.26 5.403 5.403 0 0 1-3.14-9.8c-.44-.06-.9-.1-1.36-.1z" />
    </svg>
  ) : (
    <svg viewBox="0 0 24 24" width="14" height="14" fill="currentColor">
      <path d="M12 7c-2.76 0-5 2.24-5 5s2.24 5 5 5 5-2.24 5-5-2.24-5-5-5zM2 13h2c.55 0 1-.45 1-1s-.45-1-1-1H2c-.55 0-1 .45-1 1s.45 1 1 1zm18 0h2c.55 0 1-.45 1-1s-.45-1-1-1h-2c-.55 0-1 .45-1 1s.45 1 1 1zM11 2v2c0 .55.45 1 1 1s1-.45 1-1V2c0-.55-.45-1-1-1s-1 .45-1 1zm0 18v2c0 .55.45 1 1 1s1-.45 1-1v-2c0-.55-.45-1-1-1s-1 .45-1 1zM5.99 4.58a.996.996 0 0 0-1.41 0 .996.996 0 0 0 0 1.41l1.06 1.06c.39.39 1.03.39 1.41 0s.39-1.03 0-1.41L5.99 4.58zm12.37 12.37a.996.996 0 0 0-1.41 0 .996.996 0 0 0 0 1.41l1.06 1.06c.39.39 1.03.39 1.41 0a.996.996 0 0 0 0-1.41l-1.06-1.06zm1.06-10.96a.996.996 0 0 0 0-1.41.996.996 0 0 0-1.41 0l-1.06 1.06c-.39.39-.39 1.03 0 1.41s1.03.39 1.41 0l1.06-1.06zM7.05 18.36a.996.996 0 0 0 0-1.41.996.996 0 0 0-1.41 0l-1.06 1.06c-.39.39-.39 1.03 0 1.41s1.03.39 1.41 0l1.06-1.06z" />
    </svg>
  )
}

function ThemeToggle({
  theme,
  onToggle,
  inline,
}: {
  theme: 'light' | 'dark'
  onToggle: () => void
  inline?: boolean
}) {
  const label = theme === 'light' ? 'Switch to dark mode' : 'Switch to light mode'
  return (
    <button
      className={`theme-toggle${inline ? ' theme-toggle--inline' : ''}`}
      onClick={onToggle}
      title={label}
      aria-label={label}
    >
      <ThemeIcon theme={theme} />
      {theme === 'light' ? 'Dark' : 'Light'}
    </button>
  )
}

function OfflineModeToggle({
  mode,
  available,
  busy,
  onToggle,
}: {
  mode: OfflineMode
  available: boolean
  busy: boolean
  onToggle: () => void
}) {
  const offline = mode === 'cache-only'
  const label = busy ? 'Changing mode…' : offline && available ? 'Go online' : offline ? 'Offline' : 'Work offline'
  return (
    <button
      type="button"
      className={`offline-mode-toggle${offline ? ' is-offline' : ''}`}
      onClick={onToggle}
      disabled={!available || busy}
      aria-pressed={offline}
      title={!available && offline ? 'Offline mode was fixed when the server started' : label}
      data-testid="offline-mode-toggle"
    >
      <span className="offline-mode-icon" aria-hidden="true"><i /></span>
      <span>{label}</span>
    </button>
  )
}

/* ------------------------------------------------------------------ */
/*  Main App                                                           */
/* ------------------------------------------------------------------ */

function App() {
  const [viewMode, setViewMode] = useState<ViewMode>('welcome')
  const [showSplash, setShowSplash] = useState(true)
  const [theme, setTheme] = useState<'light' | 'dark'>(
    () => (localStorage.getItem('gpx-theme') as 'light' | 'dark') ?? 'light',
  )

  useEffect(() => {
    document.documentElement.classList.toggle('dark', theme === 'dark')
    localStorage.setItem('gpx-theme', theme)
  }, [theme])

  /* -- Tracks ------------------------------------------------------- */

  const [tracks, setTracks] = useState<Track[]>([])
  const [selectedTrackIndex, setSelectedTrackIndex] = useState(0)
  const [editHistory, setEditHistory] = useState<Track[][]>([])
  const [dirty, setDirty] = useState(false)
  const [offlineRoute, setOfflineRoute] = useState<{
    key: number
    name: string
    coordinates: Coordinate[]
  } | null>(null)

  /* -- Terrain / map presentation ----------------------------------- */

  const [runtime, setRuntime] = useState(initialRuntimeConfig)
  const initialRuntimeRef = useRef(runtime)
  const runtimeModeRef = useRef(runtime.offline?.mode)
  useEffect(() => { runtimeModeRef.current = runtime.offline?.mode }, [runtime.offline?.mode])

  const [baseLayer, setBaseLayer] = useState(() => {
    const stored = localStorage.getItem('gpx-base-layer')
    return stored ?? 'openfreemap'
  })
  const [hillshade, setHillshade] = useState(() => localStorage.getItem('gpx-hillshade') !== 'off')
  const [hillshadeOpacity, setHillshadeOpacity] = useState(
    () => parseFloat(localStorage.getItem('gpx-hillshade-opacity') ?? '0.45'),
  )
  const [colorMode, setColorMode] = useState<ColorMode>(
    () => (localStorage.getItem('gpx-color-mode') as ColorMode) ?? 'slope',
  )
  const [sessionMapTrack, setSessionMapTrack] = useState<Coordinate[]>([])
  const [sessionMapMarkers, setSessionMapMarkers] = useState<McpMapMarker[]>([])
  const [vectorMapIssue, setVectorMapIssue] = useState<VectorMapIssue | null>(null)
  const [offlineModeBusy, setOfflineModeBusy] = useState(false)

  const terrainLayers = useMemo(() => runtimeTerrainLayers(runtime), [runtime])
  const hillshadeLayer = useMemo(() => runtimeHillshadeLayer(runtime), [runtime])
  const activeBaseLayer = useMemo(() => terrainLayers.some(layer => layer.id === baseLayer)
    ? baseLayer
    : (terrainLayers.find(layer => layer.id === 'openfreemap') ?? terrainLayers[0])?.id ?? baseLayer,
  [baseLayer, terrainLayers])
  /** Vector maps use an image-tile fallback so each card stays lightweight. */
  const thumbnailLayer = useMemo(
    () => getThumbnailLayer(activeBaseLayer, terrainLayers),
    [activeBaseLayer, terrainLayers],
  )

  useEffect(() => { localStorage.setItem('gpx-base-layer', baseLayer) }, [baseLayer])
  useEffect(() => { localStorage.setItem('gpx-hillshade', hillshade ? 'on' : 'off') }, [hillshade])
  useEffect(() => { localStorage.setItem('gpx-hillshade-opacity', String(hillshadeOpacity)) }, [hillshadeOpacity])
  useEffect(() => { localStorage.setItem('gpx-color-mode', colorMode) }, [colorMode])

  /**
   * Surface colouring only exists where a route has been traced, which is the
   * creation screen. Everywhere else the choice falls back to gradient rather
   * than drawing a track with no colour data at all.
   */
  const viewColorMode: ColorMode = colorMode === 'surface' ? 'slope' : colorMode

  /* -- Misc UI state ------------------------------------------------ */

  const [loading, setLoading] = useState(false)
  const [loadingMessage, setLoadingMessage] = useState('')
  const [notifications, setNotifications] = useState<Notification[]>([])
  /** Retained copy of every notification, for agents reading the snapshot. */
  const [recentMessages, setRecentMessages] = useState<RetainedMessage[]>([])
  const [dragOver, setDragOver] = useState(false)
  const [savedFiles, setSavedFiles] = useState<TrackPreview[]>([])
  const [trackFilter, setTrackFilter] = useState('')
  const [saveFileName, setSaveFileName] = useState('')
  const [elevationCollapsed, setElevationCollapsed] = useState(true)
  const [hoveredBar, setHoveredBar] = useState<number | null>(null)
  const [activeCoordIndex, setActiveCoordIndex] = useState<number | null>(null)
  const [jumpTarget, setJumpTarget] = useState<Coordinate | null>(null)
  const [restoreView, setRestoreView] = useState(false)
  const [isZoomedToBar, setIsZoomedToBar] = useState(false)
  const [cursorPos, setCursorPos] = useState<{ lat: number; lon: number } | null>(null)
  const [cursorElevation, setCursorElevation] = useState<number | null>(null)

  /* -- Editing ------------------------------------------------------ */

  const [showTools, setShowTools] = useState(false)
  /** Clicking the map drops a place of interest instead of a route point. */
  const [waypointMode, setWaypointMode] = useState(false)
  /** Places of interest placed while planning — not part of the routed line. */
  const [creationPins, setCreationPins] = useState<GpxWaypoint[]>([])
  /** Background elevation-tile download for the visible area, when the
      backend is serving elevation from tiles. */
  const [tilePrefetch, setTilePrefetch] = useState<TilePrefetch | null>(null)
  const [selectionMode, setSelectionMode] = useState(false)
  const [selection, setSelection] = useState<TrimSelection | null>(null)
  const [selectionAnchor, setSelectionAnchor] = useState<number | null>(null)
  const [stageKm, setStageKm] = useState(150)
  const [simplifyTarget, setSimplifyTarget] = useState(DEFAULT_SIMPLIFY_TARGET)

  /* -- POIs --------------------------------------------------------- */

  const [activePois, setActivePois] = useState<Record<PoiKind, Poi[]>>({ fuel: [], water: [], camp: [] })
  const [activePoiCache, setActivePoiCache] = useState<Partial<Record<PoiKind, CacheMetadata>>>({})
  const [poiLoading, setPoiLoading] = useState<PoiKind | null>(null)

  /* -- Creation ----------------------------------------------------- */

  const [creationWaypoints, setCreationWaypoints] = useState<Waypoint[]>([])
  const [routedCoordinates, setRoutedCoordinates] = useState<Coordinate[]>([])
  const [routedDuration, setRoutedDuration] = useState<number | null>(null)
  const [routedEngine, setRoutedEngine] = useState<string | null>(null)
  const [routedLoading, setRoutedLoading] = useState(false)
  const [routeStatus, setRouteStatus] = useState('')
  const [routeError, setRouteError] = useState<string | null>(null)
  const [elevationApiError, setElevationApiError] = useState(false)
  const [elevationInterpolated, setElevationInterpolated] = useState(false)
  const [routingProfile, setRoutingProfile] = useState<RoutingProfile>('mixed')
  const [routingData, setRoutingData] = useState<RoutingDataStatus | null>(null)
  /**
   * POIs found while planning. Kept apart from the view screen's `activePois`,
   * which are tied to a loaded track: these are tied to a map view instead,
   * and switching screens should not carry one set into the other.
   */
  const [creationPois, setCreationPois] = useState<Record<PoiKind, Poi[]>>({ fuel: [], water: [], camp: [] })
  const [creationPoiCache, setCreationPoiCache] = useState<Partial<Record<PoiKind, CacheMetadata>>>({})
  const [creationPoiLoading, setCreationPoiLoading] = useState<PoiKind | null>(null)
  /** Full-map mode: sidebar and header hidden, map overlays kept. */
  const [mapOnly, setMapOnly] = useState(false)
  /** Fuel the price colouring ranks on, shared by both screens. */
  const [fuelReference, setFuelReference] = useState(DEFAULT_FUEL_REFERENCE)
  const [surfaceSegments, setSurfaceSegments] = useState<SurfaceClass[] | null>(null)
  const [placeSearch, setPlaceSearch] = useState('')
  const [placeResults, setPlaceResults] = useState<PlaceResult[]>([])
  const [placeCache, setPlaceCache] = useState<CacheMetadata | undefined>()
  const [placeSearching, setPlaceSearching] = useState(false)
  const [nominatimApi, setNominatimApi] = useState(DEFAULT_NOMINATIM_API)

  const mapRef = useRef<L.Map | null>(null)
  const registerActiveMap = useCallback((map: L.Map | null) => {
    mapRef.current = map
  }, [])
  const notifIdRef = useRef(0)
  const vectorIssueNotifiedRef = useRef('')
  const staleNotifiedRef = useRef(false)
  const routeSeqRef = useRef(0)
  const routePointIDRef = useRef(Date.now())
  const placeSearchSeqRef = useRef(0)
  const placeSearchAbortRef = useRef<AbortController | null>(null)
  const offlineRouteKeyRef = useRef(0)

  const clearRouteDerived = useCallback(() => {
    const cleared = clearedRouteDerivedState()
    setRoutedCoordinates(cleared.coordinates)
    setRoutedDuration(cleared.durationSeconds)
    setRoutedEngine(cleared.engine)
    setSurfaceSegments(cleared.surfaceSegments)
    setElevationInterpolated(cleared.elevationInterpolated)
    setElevationApiError(cleared.elevationApiError)
    setRouteStatus(cleared.routeStatus)
    setRoutedLoading(cleared.routedLoading)
    setRouteError(null)
  }, [])

  /* -- Notifications ------------------------------------------------ */

  const notify = useCallback((message: string, type: NotificationType = 'info') => {
    const id = ++notifIdRef.current
    const at = new Date().toISOString()
    setNotifications(prev => [...prev, { id, message, type }])
    // Toasts vanish after five seconds, which is far too short for an agent
    // polling the snapshot. Anything the user was told is retained here so an
    // MCP client can see the failure its own command caused.
    setRecentMessages(prev => [...prev, { seq: id, at, type, message }].slice(-MAX_RETAINED_MESSAGES))
    setTimeout(() => setNotifications(prev => prev.filter(n => n.id !== id)), 5000)
  }, [])

  const dismissNotification = useCallback((id: number) => {
    setNotifications(prev => prev.filter(n => n.id !== id))
  }, [])

  useEffect(() => {
    const controller = new AbortController()
    void loadRuntimeConfig(API_BASE, controller.signal, fetch, initialRuntimeRef.current)
      .then(config => {
        setRuntime(config)
        if (config.nominatimUrl) setNominatimApi(config.nominatimUrl)
      })
      .catch(() => {})
    return () => controller.abort()
  }, [])

  useEffect(() => {
    if (!runtime.offline?.routing) {
      setRoutingData(null)
      return
    }
    const controller = new AbortController()
    const load = async () => {
      try {
        const status = await fetchRoutingDataStatus(runtime, controller.signal)
        if (!status || controller.signal.aborted) return
        setRoutingData(status)
      } catch (error) {
        if ((error as Error).name !== 'AbortError') setRoutingData(null)
      }
    }
    void load()
    return () => controller.abort()
  }, [runtime])

  const handleCacheMetadata = useCallback((metadata: CacheMetadata) => {
    if (!metadata.stale || staleNotifiedRef.current) return
    staleNotifiedRef.current = true
    const cachedAt = formatCacheDate(metadata.cachedAt)
    notify(`Using stale cached data${cachedAt ? ` from ${cachedAt}` : ''}`, 'info')
  }, [notify])

  const handleVectorStatus = useCallback((issue: VectorMapIssue | null) => {
    if (issue && expectedOfflineVectorMiss(issue, runtimeModeRef.current)) {
      setVectorMapIssue(null)
      return
    }
    setVectorMapIssue(issue)
    if (!issue) return
    const fingerprint = `${issue.phase}:${issue.status ?? ''}:${issue.source ?? ''}:${issue.message}`
    if (vectorIssueNotifiedRef.current === fingerprint) return
    vectorIssueNotifiedRef.current = fingerprint
    notify(issue.message, 'error')
  }, [notify])

  const selectBaseLayer = useCallback((id: string) => {
    setVectorMapIssue(null)
    setBaseLayer(id)
  }, [])

  const toggleOfflineMode = useCallback(async () => {
    if (!runtime.offline?.modeControl || offlineModeBusy) return
    const previousMode = runtime.offline.mode
    const requested: OfflineMode = runtime.offline.mode === 'cache-only' ? 'auto' : 'cache-only'
    runtimeModeRef.current = requested === 'cache-only' ? requested : previousMode
    setOfflineModeBusy(true)
    try {
      const mode = await setRuntimeOfflineMode(runtime, requested)
      const nextRuntime = runtime.offline
        ? { ...runtime, offline: { ...runtime.offline, mode } }
        : runtime
      runtimeModeRef.current = mode
      setRuntime(nextRuntime)
      setVectorMapIssue(null)
      notify(mode === 'cache-only' ? 'Working offline. Only cached resources will be used.' : 'Online access restored.', 'success')
    } catch (reason) {
      runtimeModeRef.current = previousMode
      notify((reason as Error).message || 'Could not change offline mode', 'error')
    } finally {
      setOfflineModeBusy(false)
    }
  }, [offlineModeBusy, notify, runtime])

  /* -- Saved files -------------------------------------------------- */

  const loadSavedFiles = useCallback(async () => {
    try {
      const response = await fetch(`${API_BASE}/files`)
      if (!response.ok) return
      const data = await response.json()
      const fileNames: string[] = data.files || []

      const files = await Promise.all(
        fileNames.map(async (filename: string): Promise<TrackPreview | null> => {
          try {
            const res = await fetch(`${API_BASE}/gpx/${encodeURIComponent(filename)}`)
            const parsed = parseGPX(await res.text())
            if (parsed.length === 0) return null

            const track = parsed[0]
            const cum = cumulativeDistanceKm(track.coordinates)
            const smoothed = smoothElevations(track.elevations, cum)
            // Douglas-Peucker keeps the recognisable shape of the route,
            // which plain index-striding does not.
            const shape = simplifyToMaxPoints(track, THUMBNAIL_POINTS).coordinates
            const stride = Math.max(1, Math.ceil(smoothed.length / 60))
            const profile = smoothed
              .filter((_, i) => i % stride === 0)
              .filter((e): e is number => e !== null)

            return {
              // Titled off the filename, not the GPX <name>: the file is the
              // identity here, and <name> is often missing or shared between
              // unrelated tracks.
              name: fromGpxFilename(filename),
              filename,
              distance: calculateDistance(track.coordinates),
              elevStats: calculateElevationStats(smoothed),
              hasTime: track.coordinates.some(c => c.time),
              shape,
              profile,
            }
          } catch {
            return null
          }
        }),
      )
      setSavedFiles(files.filter((f): f is TrackPreview => f !== null))
    } catch {
      // Backend unavailable — the app still works with local files.
    }
  }, [])

  useEffect(() => { loadSavedFiles() }, [loadSavedFiles])

  const filteredTracks = useMemo(() => {
    const q = trackFilter.trim().toLowerCase()
    if (!q) return savedFiles
    return savedFiles.filter(
      f => f.name.toLowerCase().includes(q) || f.filename.toLowerCase().includes(q),
    )
  }, [savedFiles, trackFilter])

  /* -- Loading tracks ----------------------------------------------- */

  const activateOfflineRoute = useCallback((track: Track | undefined) => {
    if (!track) {
      setOfflineRoute(null)
      return
    }
    setOfflineRoute({
      key: ++offlineRouteKeyRef.current,
      name: fromGpxFilename(track.filename) || track.name,
      // This snapshot deliberately does not follow edits. Caching is tied to
      // opening/selecting a track, not every immutable edit-state replacement.
      coordinates: track.coordinates,
    })
  }, [])

  const openTracks = useCallback((parsed: Track[], filename: string) => {
    const loaded = parsed.map(t => ({ ...t, filename }))
    setTracks(loaded)
    setSelectedTrackIndex(0)
    setEditHistory([])
    setDirty(false)
    setSelection(null)
    setSelectionAnchor(null)
    setSelectionMode(false)
    setActivePois({ fuel: [], water: [], camp: [] })
    setActivePoiCache({})
    activateOfflineRoute(loaded[0])
    setViewMode('view')
  }, [activateOfflineRoute])

  const selectTrack = useCallback((index: number) => {
    setSelectedTrackIndex(index)
    activateOfflineRoute(tracks[index])
  }, [activateOfflineRoute, tracks])

  const processGPXFile = useCallback(
    async (gpxFile: File) => {
      setLoading(true)
      setLoadingMessage('Parsing GPX file…')
      try {
        const content = await gpxFile.text()
        const parsed = parseGPX(content)
        if (parsed.length === 0) {
          notify('No tracks or routes found in this file', 'error')
          return
        }

        // Upload is best-effort: parsing already succeeded locally.
        try {
          const formData = new FormData()
          formData.append('file', gpxFile)
          const response = await fetch(`${API_BASE}/upload`, { method: 'POST', body: formData })
          if (response.ok) {
            loadSavedFiles()
          } else if (response.status === 409) {
            loadSavedFiles()
            notify(`${gpxFile.name} is already in the library. Opened the local copy without replacing it.`, 'info')
          }
        } catch {
          // Backend unavailable.
        }

        openTracks(parsed, gpxFile.name)
      } catch (err) {
        notify(`Could not read this file: ${(err as Error).message}`, 'error')
      } finally {
        setLoading(false)
      }
    },
    [notify, openTracks, loadSavedFiles],
  )

  const handleFileUpload = useCallback(
    (e: React.ChangeEvent<HTMLInputElement>) => {
      const uploaded = e.target.files?.[0]
      if (uploaded) processGPXFile(uploaded)
    },
    [processGPXFile],
  )

  const handleDragOver = useCallback((e: React.DragEvent) => { e.preventDefault(); setDragOver(true) }, [])
  const handleDragLeave = useCallback((e: React.DragEvent) => { e.preventDefault(); setDragOver(false) }, [])

  const handleDrop = useCallback(
    (e: React.DragEvent) => {
      e.preventDefault()
      setDragOver(false)
      const dropped = e.dataTransfer.files[0]
      if (dropped && dropped.name.toLowerCase().endsWith('.gpx')) processGPXFile(dropped)
      else notify('Please drop a .gpx file', 'error')
    },
    [processGPXFile, notify],
  )

  const loadTrackFromFile = useCallback(
    async (filename: string) => {
      setLoading(true)
      setLoadingMessage('Loading track…')
      try {
        const response = await fetch(`${API_BASE}/gpx/${encodeURIComponent(filename)}`)
        if (!response.ok) throw new Error(`Server returned ${response.status}`)
        const parsed = parseGPX(await response.text())
        if (parsed.length === 0) {
          notify('No tracks or routes found in this file', 'error')
          return
        }
        openTracks(parsed, filename)
      } catch (err) {
        notify(`Error loading track: ${(err as Error).message}`, 'error')
      } finally {
        setLoading(false)
      }
    },
    [notify, openTracks],
  )

  const deleteFile = useCallback(
    async (filename: string) => {
      if (!confirm(`Delete "${filename}"?`)) return
      try {
        const response = await fetch(`${API_BASE}/gpx/${encodeURIComponent(filename)}`, { method: 'DELETE' })
        // 404 means it is already gone — the card was stale, so refreshing the
        // library is the right answer rather than an error nobody can act on.
        if (!response.ok && response.status !== 404) {
          const detail = await response
            .json()
            .then((body: { detail?: string }) => body?.detail)
            .catch(() => null)
          throw new Error(detail ?? `server returned ${response.status}`)
        }
        // Drop the card now rather than waiting on the refresh. loadSavedFiles
        // keeps the previous list when the backend is unreachable, which is
        // right on first load but here would leave a deleted track on screen
        // looking as though nothing happened.
        setSavedFiles(prev => prev.filter(f => f.filename !== filename))
        loadSavedFiles()
        notify(`Deleted ${filename}`, 'success')
      } catch (err) {
        // Say which file and why: "Error deleting file" on a library holding
        // two identically named tracks is impossible to act on.
        notify(`Could not delete ${filename}: ${(err as Error).message}`, 'error')
      }
    },
    [loadSavedFiles, notify],
  )

  /* -- Current track derivations ------------------------------------ */

  const currentTrack = tracks[selectedTrackIndex] ?? tracks[0]

  const cumKm = useMemo(
    () => (currentTrack ? cumulativeDistanceKm(currentTrack.coordinates) : []),
    [currentTrack],
  )

  // Smoothed once, then reused for stats, colouring and the profile so all
  // three agree with each other.
  const smoothedElevations = useMemo(
    () => (currentTrack ? smoothElevations(currentTrack.elevations, cumKm) : []),
    [currentTrack, cumKm],
  )

  const trackDistance = cumKm.length > 0 ? cumKm[cumKm.length - 1] : 0
  const elevationStats = useMemo(
    () => calculateElevationStats(smoothedElevations),
    [smoothedElevations],
  )
  const timeStats = useMemo(
    () => (currentTrack ? calculateTimeStats(currentTrack.coordinates) : null),
    [currentTrack],
  )

  const hasElevationData = useMemo(
    () => smoothedElevations.some(e => e !== null),
    [smoothedElevations],
  )

  const cancelTrackSelection = useCallback(() => {
    setSelectionMode(false)
    setSelectionAnchor(null)
    setSelection(null)
  }, [])
  useEscapeDismiss(viewMode === 'view' && selectionMode, cancelTrackSelection, ESCAPE_PRIORITY.nested)
  useEscapeDismiss(viewMode === 'view' && showTools, () => setShowTools(false), ESCAPE_PRIORITY.panel)
  useEscapeDismiss(viewMode === 'view' && !elevationCollapsed && hasElevationData, () => setElevationCollapsed(true), ESCAPE_PRIORITY.panel)
  useEscapeDismiss((viewMode === 'view' || viewMode === 'creation') && waypointMode, () => setWaypointMode(false), ESCAPE_PRIORITY.mode)
  useEscapeDismiss(viewMode === 'creation' && mapOnly, () => setMapOnly(false), ESCAPE_PRIORITY.mode)
  useEscapeDismiss(viewMode === 'view' && isZoomedToBar, () => setRestoreView(true), ESCAPE_PRIORITY.passive)

  /**
   * Profile samples, spaced by real distance rather than by array index so
   * the slope denominator is always meaningful regardless of how densely the
   * source recorded.
   */
  const profilePoints = useMemo(() => {
    if (!currentTrack) return []
    const totalKm = cumKm[cumKm.length - 1] ?? 0
    const minSegmentKm = Math.max(0.02, totalKm / MAX_PROFILE_BARS)

    const pts: { coordIndex: number; elevation: number; distKm: number }[] = []
    let distAtLastSample = -minSegmentKm
    for (let i = 0; i < currentTrack.coordinates.length; i++) {
      const e = smoothedElevations[i]
      if (e === null || e === undefined) continue
      if (pts.length > 0 && cumKm[i] - distAtLastSample < minSegmentKm) continue
      pts.push({ coordIndex: i, elevation: e, distKm: cumKm[i] })
      distAtLastSample = cumKm[i]
    }
    return pts
  }, [currentTrack, cumKm, smoothedElevations])

  const profileElevMin = profilePoints.length
    ? Math.min(...profilePoints.map(p => p.elevation))
    : 0
  const profileElevMax = profilePoints.length
    ? Math.max(...profilePoints.map(p => p.elevation))
    : 1000
  const profileElevRange = profileElevMax - profileElevMin || 1

  const profileBars = useMemo<ProfileBar[]>(
    () =>
      profilePoints.map((pt, idx) => {
        let slope = 0
        let elevDelta = 0
        if (idx > 0) {
          const prev = profilePoints[idx - 1]
          elevDelta = pt.elevation - prev.elevation
          slope = slopePercent(elevDelta, (pt.distKm - prev.distKm) * 1000)
        }
        return {
          coordIndex: pt.coordIndex,
          elevation: pt.elevation,
          elevDelta,
          slope,
          distKm: pt.distKm,
          barHeight: ((pt.elevation - profileElevMin) / profileElevRange) * 100,
          color:
            viewColorMode === 'slope'
              ? getSegmentColor(slope)
              : altitudeColor(pt.elevation, profileElevMin, profileElevMax),
        }
      }),
    [profilePoints, profileElevMin, profileElevMax, profileElevRange, viewColorMode],
  )

  const yAxisTicks = useMemo(() => {
    const ticks: number[] = []
    for (let t = 0; t <= 4; t++) ticks.push(Math.round(profileElevMin + (profileElevRange * t) / 4))
    return ticks
  }, [profileElevMin, profileElevRange])

  /* -- Fuel gap ----------------------------------------------------- */

  const fuelGap = useMemo(() => {
    const fuel = activePois.fuel
    if (!currentTrack || fuel.length === 0 || cumKm.length === 0) return null
    return longestGapKm(currentTrack.coordinates, cumKm, fuel, FUEL_CORRIDOR_M).gapKm
  }, [activePois.fuel, currentTrack, cumKm])

  const togglePoiLayer = useCallback(
    async (kind: PoiKind) => {
      if (activePois[kind].length > 0) {
        setActivePois(prev => ({ ...prev, [kind]: [] }))
        setActivePoiCache(prev => ({ ...prev, [kind]: undefined }))
        return
      }
      if (!currentTrack) return
      const bbox = boundsAround(currentTrack.coordinates, 5)
      if (!bbox) return

      setPoiLoading(kind)
      try {
        const { pois, source, cache } = await fetchPoisForArea(kind, bbox, undefined, {
          runtime,
          onCacheMetadata: handleCacheMetadata,
        })
        setActivePois(prev => ({ ...prev, [kind]: pois }))
        setActivePoiCache(prev => ({ ...prev, [kind]: cache }))
        if (pois.length === 0) notify(`No ${kind} points found near this route`, 'info')
        else if (source) notify(`${pois.length} fuel stations with ${source}`, 'info')
      } catch (err) {
        notify(`Could not load ${kind} points: ${(err as Error).message}`, 'error')
      } finally {
        setPoiLoading(null)
      }
    },
    [activePois, currentTrack, handleCacheMetadata, notify, runtime],
  )

  /* -- Creation: full-map mode --------------------------------------- */

  useEffect(() => {
    // Leaflet caches its container size, so after the panels collapse it keeps
    // drawing at the old width and leaves grey where the sidebar used to be.
    // Told on the next frame, once the new layout has actually been applied.
    const map = mapRef.current
    if (!map) return
    const frame = requestAnimationFrame(() => map.invalidateSize())
    return () => cancelAnimationFrame(frame)
  }, [mapOnly])

  /* -- Creation: POIs in the current view ---------------------------- */

  /**
   * While planning there is often no route yet to search around, so the search
   * area is whatever the map is showing at the moment the button is pressed.
   */
  const viewportBounds = useCallback((): BoundingBox | null => {
    const map = mapRef.current
    if (!map) return null
    const bounds = map.getBounds()
    return {
      south: bounds.getSouth(),
      west: bounds.getWest(),
      north: bounds.getNorth(),
      east: bounds.getEast(),
    }
  }, [])

  const loadCreationPois = useCallback(
    async (kinds: PoiKind[]) => {
      const bbox = viewportBounds()
      if (!bbox) return { loaded: 0, errors: ['The planning map is not available'] }

      const { widthKm, heightKm } = boundingBoxSpanKm(bbox)
      const span = Math.max(widthKm, heightKm)
      if (span > MAX_SEARCH_SPAN_KM) {
        const message = `This view spans about ${span.toFixed(0)} km — zoom in to under ${MAX_SEARCH_SPAN_KM} km to search it`
        notify(message, 'info')
        return { loaded: 0, errors: [message] }
      }

      let loaded = 0
      const errors: string[] = []
      // One kind at a time: a refresh asks for three layers at once, and
      // Overpass is a shared free service.
      for (const kind of kinds) {
        setCreationPoiLoading(kind)
        setCreationPoiCache(prev => ({ ...prev, [kind]: undefined }))
        try {
          const { pois, source, truncated, cache } = await fetchPoisForArea(kind, bbox, undefined, {
            runtime,
            onCacheMetadata: handleCacheMetadata,
          })
          setCreationPois(prev => ({ ...prev, [kind]: pois }))
          setCreationPoiCache(prev => ({ ...prev, [kind]: cache }))
          loaded += pois.length
          const label = POI_KINDS.find(k => k.id === kind)?.label.toLowerCase() ?? kind
          if (pois.length === 0) {
            notify(`No ${label} points in this view`, 'info')
          } else if (truncated) {
            notify(`Showing the ${pois.length} ${label} points nearest the centre of this view`, 'info')
          } else if (source) {
            notify(`${pois.length} fuel stations with ${source}`, 'info')
          }
        } catch (err) {
          const message = `Could not load ${kind} points: ${(err as Error).message}`
          errors.push(message)
          notify(message, 'error')
        } finally {
          setCreationPoiLoading(null)
        }
      }
      return { loaded, errors }
    },
    [handleCacheMetadata, notify, runtime, viewportBounds],
  )

  const toggleCreationPoiLayer = useCallback(
    (kind: PoiKind) => {
      if (creationPois[kind].length > 0) {
        setCreationPois(prev => ({ ...prev, [kind]: [] }))
        setCreationPoiCache(prev => ({ ...prev, [kind]: undefined }))
        return
      }
      void loadCreationPois([kind])
    },
    [creationPois, loadCreationPois],
  )

  /** Marker colour per fuel station, ranked among those in the same set. */
  const creationFuelColors = useMemo(
    () => fuelBandColors(creationPois.fuel, fuelReference),
    [creationPois.fuel, fuelReference],
  )
  const viewFuelColors = useMemo(
    () => fuelBandColors(activePois.fuel, fuelReference),
    [activePois.fuel, fuelReference],
  )

  /** Kinds currently drawn, so a pan can re-run exactly those. */
  const activeCreationPoiKinds = useMemo(
    () => POI_KINDS.filter(k => creationPois[k.id].length > 0).map(k => k.id),
    [creationPois],
  )

  /* -- Track editing ------------------------------------------------ */

  // The new state is computed up front rather than inside a setState updater:
  // updaters must stay pure, and StrictMode runs them twice.
  const applyEdit = useCallback(
    (label: string, transform: (track: Track) => Track | Track[]) => {
      const track = tracks[selectedTrackIndex]
      if (!track) return
      const result = transform(track)
      const replacements = Array.isArray(result) ? result : [result]
      const next = [...tracks]
      next.splice(selectedTrackIndex, 1, ...replacements)

      setEditHistory(hist => [...hist, tracks])
      setTracks(next)
      setDirty(true)
      setSelection(null)
      setSelectionAnchor(null)
      notify(label, 'success')
    },
    [tracks, selectedTrackIndex, notify],
  )

  /** Drop the loaded track and everything derived from it, back to a blank slate. */
  const clearTrack = useCallback(() => {
    if (dirty && !confirm('This track has unsaved edits. Clear it anyway?')) return
    setTracks([])
    setOfflineRoute(null)
    setSelectedTrackIndex(0)
    setEditHistory([])
    setDirty(false)
    setSelection(null)
    setSelectionAnchor(null)
    setSelectionMode(false)
    setWaypointMode(false)
    setActivePois({ fuel: [], water: [], camp: [] })
    setSurfaceSegments(null)
    setElevationInterpolated(false)
    setElevationApiError(false)
    setShowTools(false)
    setViewMode('welcome')
    loadSavedFiles()
  }, [dirty, loadSavedFiles])

  /* -- Custom waypoints --------------------------------------------- */

  /*
   * Waypoints are independent of the track's own points: you drop them where
   * something is, not where the route happens to have a sample. They ride
   * along in the file as <wpt> elements, which buildGPX already writes.
   */
  const addWaypoint = useCallback(
    (lat: number, lon: number) => {
      applyEdit('Waypoint added', track => ({
        ...track,
        waypoints: [
          ...track.waypoints,
          {
            lat,
            lon,
            name: `Waypoint ${track.waypoints.length + 1}`,
            sym: waypointMarkerDefinition('generic').gpxSymbol,
          },
        ],
      }))
    },
    [applyEdit],
  )

  const renameWaypoint = useCallback(
    (index: number) => {
      const current = tracks[selectedTrackIndex]?.waypoints[index]
      const name = prompt('Waypoint name', current?.name ?? '')?.trim()
      if (!name) return
      applyEdit('Waypoint renamed', track => ({
        ...track,
        waypoints: track.waypoints.map((waypoint, i) => (i === index ? { ...waypoint, name } : waypoint)),
      }))
    },
    [applyEdit, tracks, selectedTrackIndex],
  )

  const removeWaypoint = useCallback(
    (index: number) => {
      applyEdit('Waypoint removed', track => ({
        ...track,
        waypoints: track.waypoints.filter((_, i) => i !== index),
      }))
    },
    [applyEdit],
  )

  const changeWaypointMarker = useCallback(
    (index: number, markerId: WaypointMarkerId) => {
      applyEdit('Waypoint marker changed', track => ({
        ...track,
        waypoints: track.waypoints.map((waypoint, i) => i === index
          ? { ...waypoint, sym: waypointMarkerDefinition(markerId).gpxSymbol }
          : waypoint),
      }))
    },
    [applyEdit],
  )

  const handleViewMapClick = useCallback(
    (e: L.LeafletMouseEvent) => {
      if (!waypointMode) return
      addWaypoint(e.latlng.lat, e.latlng.lng)
    },
    [waypointMode, addWaypoint],
  )

  const undoEdit = useCallback(() => {
    if (editHistory.length === 0) return
    const previous = editHistory[editHistory.length - 1]
    setEditHistory(editHistory.slice(0, -1))
    setTracks(previous)
    setSelectedTrackIndex(i => Math.min(i, previous.length - 1))
    setSelection(null)
    setSelectionAnchor(null)
  }, [editHistory])

  const handleSelectBar = useCallback(
    (bar: ProfileBar) => {
      if (selectionMode) {
        if (selectionAnchor === null) {
          setSelectionAnchor(bar.coordIndex)
          setSelection({ startIdx: bar.coordIndex, endIdx: bar.coordIndex })
        } else {
          setSelection({ startIdx: selectionAnchor, endIdx: bar.coordIndex })
          setSelectionAnchor(null)
        }
        return
      }
      const coord = currentTrack?.coordinates[bar.coordIndex]
      if (!coord) return
      setActiveCoordIndex(bar.coordIndex)
      setJumpTarget({ lat: coord.lat, lon: coord.lon })
      setIsZoomedToBar(true)
      setTimeout(() => setActiveCoordIndex(null), 2500)
    },
    [selectionMode, selectionAnchor, currentTrack],
  )

  const refetchElevation = useCallback(async () => {
    if (!currentTrack) return
    setLoading(true)
    setLoadingMessage('Reading elevation from the terrain model…')
    try {
      const { elevations, interpolatedPoints } = await fetchElevationProfile(
        currentTrack.coordinates,
        API_BASE,
        ELEVATION_API,
        {
          dataset: ELEVATION_DATASET,
          runtime,
          onCacheMetadata: handleCacheMetadata,
          onProgress: (done, total) =>
            setLoadingMessage(`Reading elevation… ${done}/${total} points`),
        },
      )
      applyEdit('Elevation refreshed from terrain model', track => withElevations(track, elevations, interpolatedPoints))
    } catch (err) {
      notify(
        err instanceof ElevationUnavailableError
          ? 'Elevation service is not reachable'
          : `Could not read elevation: ${(err as Error).message}`,
        'error',
      )
    } finally {
      setLoading(false)
    }
  }, [currentTrack, applyEdit, handleCacheMetadata, notify, runtime])

  /* -- Downloads and saving ----------------------------------------- */

  const downloadGpx = useCallback((content: string, filename: string) => {
    const blob = new Blob([content], { type: 'application/gpx+xml' })
    const url = URL.createObjectURL(blob)
    const a = document.createElement('a')
    a.href = url
    a.download = filename
    a.click()
    // Revoke on the next tick so the click has definitely been dispatched.
    setTimeout(() => URL.revokeObjectURL(url), 0)
  }, [])

  const downloadCurrentTrack = useCallback(() => {
    if (!currentTrack) return
    downloadGpx(
      buildGPX({
        name: currentTrack.name,
        coordinates: currentTrack.coordinates,
        waypoints: currentTrack.waypoints,
        time: currentTrack.time,
      }),
      currentTrack.filename || toGpxFilename(currentTrack.name),
    )
  }, [currentTrack, downloadGpx])

  const saveCurrentTrack = useCallback(async () => {
    if (!currentTrack) return
    // Save back over the file this track came from. Deriving the name from the
    // track's <name> instead writes a second file every time — open
    // "2026-03-08_Sun.gpx", save, and the library grows a "morning-ride.gpx"
    // twin holding the same route.
    const filename = currentTrack.filename || toGpxFilename(currentTrack.name)
    const content = buildGPX({
      name: currentTrack.name,
      coordinates: currentTrack.coordinates,
      waypoints: currentTrack.waypoints,
      time: currentTrack.time,
    })
    try {
      const res = await fetch(`${API_BASE}/gpx/${encodeURIComponent(filename)}`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/gpx+xml' },
        body: content,
      })
      if (!res.ok) throw new Error(`Server returned ${res.status}`)
      setDirty(false)
      loadSavedFiles()
      notify(`Saved to ${filename}`, 'success')
    } catch (err) {
      notify(`Could not save: ${(err as Error).message}`, 'error')
    }
  }, [currentTrack, loadSavedFiles, notify])

  /* -- Creation: waypoints ------------------------------------------ */

  const fetchWaypointElevation = useCallback(async (waypoint: Waypoint) => {
    try {
      const elevation = await fetchGroundElevation(
        waypoint.lat,
        waypoint.lon,
        API_BASE,
        ELEVATION_API,
        ELEVATION_DATASET,
        undefined,
        { runtime, onCacheMetadata: handleCacheMetadata },
      )
      if (elevation === null) { setElevationApiError(true); return }
      setElevationApiError(false)
      setCreationWaypoints(prev =>
        prev.map(w => (w.id === waypoint.id ? { ...w, elevation } : w)),
      )
    } catch {
      setElevationApiError(true)
    }
  }, [handleCacheMetadata, runtime])

  /*
   * Elevation tiles for the area on screen.
   *
   * The backend answers `enabled: false` when it is not in tile mode, and we
   * stop asking for the rest of the session — there is nothing to download
   * and nothing to show.
   */
  const prefetchDisabledRef = useRef(false)
  const pollRef = useRef<number | undefined>(undefined)

  const pollPrefetch = useCallback(() => {
    window.clearTimeout(pollRef.current)
    pollRef.current = window.setTimeout(async () => {
      try {
        const res = await fetch(`${API_BASE}/elevation/prefetch`)
        if (!res.ok) return
        const p: TilePrefetch & { enabled: boolean } = await res.json()
        if (!p.enabled) { prefetchDisabledRef.current = true; setTilePrefetch(null); return }
        setTilePrefetch(p.running ? p : null)
        if (p.running) pollPrefetch()
      } catch {
        // The download is best-effort; on-demand lookup still covers the route.
        setTilePrefetch(null)
      }
    }, 400)
  }, [])

  const handleViewportSettle = useCallback(
    async (bounds: L.LatLngBounds) => {
      if (prefetchDisabledRef.current) return
      try {
        const res = await fetch(`${API_BASE}/elevation/prefetch`, {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({
            bbox: [bounds.getSouth(), bounds.getWest(), bounds.getNorth(), bounds.getEast()],
          }),
        })
        if (!res.ok) return
        const p: TilePrefetch & { enabled: boolean } = await res.json()
        if (!p.enabled) { prefetchDisabledRef.current = true; return }
        setTilePrefetch(p.running ? p : null)
        if (p.running) pollPrefetch()
      } catch {
        // Backend unreachable — the app keeps working without it.
      }
    },
    [pollPrefetch],
  )

  useEffect(() => () => window.clearTimeout(pollRef.current), [])

  const handleMapClick = useCallback(
    (e: L.LeafletMouseEvent) => {
      if (viewMode !== 'creation') return

      // Places of interest are not route points: the router must not detour
      // through a monument you only want marked on the map.
      if (waypointMode) {
        setCreationPins(prev => [
          ...prev,
          {
            lat: e.latlng.lat,
            lon: e.latlng.lng,
            name: `Point of interest ${prev.length + 1}`,
            sym: waypointMarkerDefinition('generic').gpxSymbol,
          },
        ])
        return
      }

      const waypoint: Waypoint = { id: ++routePointIDRef.current, lat: e.latlng.lat, lon: e.latlng.lng }
      setCreationWaypoints(prev => [...prev, waypoint])
      fetchWaypointElevation(waypoint)
    },
    [viewMode, waypointMode, fetchWaypointElevation],
  )

  // prompt() stays outside the updater: updaters must be pure, and StrictMode
  // runs them twice.
  const renameCreationPin = useCallback(
    (index: number) => {
      const name = prompt('Name this place', creationPins[index]?.name ?? '')?.trim()
      if (!name) return
      setCreationPins(prev => prev.map((p, i) => (i === index ? { ...p, name } : p)))
    },
    [creationPins],
  )

  const removeCreationPin = useCallback((index: number) => {
    setCreationPins(prev => prev.filter((_, i) => i !== index))
  }, [])

  const changeCreationPinMarker = useCallback((index: number, markerId: WaypointMarkerId) => {
    setCreationPins(prev => prev.map((pin, i) => i === index
      ? { ...pin, sym: waypointMarkerDefinition(markerId).gpxSymbol }
      : pin))
  }, [])

  const deleteWaypoint = useCallback((id: number) => {
    setCreationWaypoints(prev => prev.filter(w => w.id !== id))
  }, [])

  const undoLastWaypoint = useCallback(() => {
    setCreationWaypoints(prev => prev.slice(0, -1))
  }, [])

  const reverseWaypoints = useCallback(() => {
    setCreationWaypoints(prev => [...prev].reverse())
  }, [])

  const handleWaypointDrag = useCallback(
    (id: number, lat: number, lon: number) => {
      setCreationWaypoints(prev =>
        prev.map(w => (w.id === id ? { ...w, lat, lon, elevation: undefined } : w)),
      )
      fetchWaypointElevation({ id, lat, lon })
    },
    [fetchWaypointElevation],
  )

  useEffect(() => {
    if (viewMode !== 'creation') return
    const handler = (e: KeyboardEvent) => {
      if ((e.ctrlKey || e.metaKey) && e.key === 'z') {
        e.preventDefault()
        setCreationWaypoints(prev => prev.slice(0, -1))
      }
    }
    window.addEventListener('keydown', handler)
    return () => window.removeEventListener('keydown', handler)
  }, [viewMode])

  const searchPlace = useCallback(async (query: string) => {
    const seq = ++placeSearchSeqRef.current
    placeSearchAbortRef.current?.abort()
    if (!query.trim()) {
      setPlaceResults([])
      setPlaceCache(undefined)
      setPlaceSearching(false)
      placeSearchAbortRef.current = null
      return
    }

    const controller = new AbortController()
    placeSearchAbortRef.current = controller
    setPlaceSearching(true)
    setPlaceCache(undefined)
    try {
      const results = await searchPlaces(query, controller.signal, nominatimApi, {
        runtime,
        onCacheMetadata: metadata => {
          handleCacheMetadata(metadata)
          if (seq === placeSearchSeqRef.current) setPlaceCache(metadata)
        },
      })
      if (seq === placeSearchSeqRef.current) setPlaceResults(results)
    } catch (err) {
      if ((err as Error).name !== 'AbortError' && seq === placeSearchSeqRef.current) {
        setPlaceResults([])
        setPlaceCache(undefined)
        notify((err as Error).message || 'Place search unavailable', 'error')
      }
    } finally {
      if (seq === placeSearchSeqRef.current) {
        setPlaceSearching(false)
        placeSearchAbortRef.current = null
      }
    }
  }, [handleCacheMetadata, nominatimApi, notify, runtime])

  const flyToPlace = useCallback((lat: number, lon: number) => {
    mapRef.current?.flyTo([lat, lon], 13, { duration: FLY_TO_DURATION })
    setPlaceResults([])
    setPlaceCache(undefined)
    setPlaceSearch('')
  }, [])

  const dismissPlaceResults = useCallback(() => {
    placeSearchSeqRef.current++
    placeSearchAbortRef.current?.abort()
    placeSearchAbortRef.current = null
    setPlaceResults([])
    setPlaceCache(undefined)
    setPlaceSearching(false)
  }, [])
  useEscapeDismiss(
    viewMode === 'creation' && !mapOnly && (placeResults.length > 0 || placeSearching),
    dismissPlaceResults,
    ESCAPE_PRIORITY.popover,
  )

  /* -- Creation: routing -------------------------------------------- */

  useEffect(() => {
    if (viewMode !== 'creation') return

    if (creationWaypoints.length < 2) {
      routeSeqRef.current++
      clearRouteDerived()
      return
    }

    if (runtime.services.broomRoute && !routingData?.ready) {
      routeSeqRef.current++
      clearRouteDerived()
      setRouteError(routingData?.error || 'Routing data is not ready yet. Open Offline routing on the map to follow preparation or download this area.')
      return
    }

    const controller = new AbortController()
    const seq = ++routeSeqRef.current
    clearRouteDerived()
    // Only the newest request may write state; a slow earlier response
    // must never overwrite a newer route.
    const isCurrent = () => routeSequenceIsCurrent(
      seq,
      routeSeqRef.current,
      controller.signal.aborted,
    )

    const timer = setTimeout(async () => {
      setRoutedLoading(true)
      setRouteStatus('Calculating route…')
      try {
        const result = await calculateRoute(
          creationWaypoints.map(w => ({ lat: w.lat, lon: w.lon })),
          routingProfile,
          controller.signal,
          { runtime },
        )
        if (!isCurrent()) return
        setRoutedCoordinates(result.coordinates)
        setRoutedDuration(result.durationSeconds)
        setRoutedEngine(result.engine)
        setSurfaceSegments(result.surface.segments)
        setElevationInterpolated(result.coordinates.some(point => point.elevationInterpolated === true))
        setElevationApiError(!result.coordinates.some(point => point.elevation !== undefined))
      } catch (err) {
        if ((err as Error).name === 'AbortError' || !isCurrent()) return
        clearRouteDerived()
        const message = (err as Error).message || 'Could not calculate route'
        setRouteError(message)
        notify(message, 'error')
      } finally {
        if (isCurrent()) { setRoutedLoading(false); setRouteStatus('') }
      }
    }, ROUTE_DEBOUNCE_MS)

    return () => { clearTimeout(timer); controller.abort() }
  }, [clearRouteDerived, creationWaypoints, routingData?.generationId, routingData?.ready, routingData?.error, routingProfile, runtime, viewMode, notify])

  const creationCoordinates = useMemo<Coordinate[]>(
    () =>
      routedCoordinates.length > 0
        ? routedCoordinates
        : creationWaypoints.map(w => ({ lat: w.lat, lon: w.lon, elevation: w.elevation })),
    [routedCoordinates, creationWaypoints],
  )

  const creationDistance = useMemo(() => calculateDistance(creationCoordinates), [creationCoordinates])
  const creationElevations = useMemo(
    () => creationCoordinates.map(c => (c.elevation === undefined ? null : c.elevation)),
    [creationCoordinates],
  )
  const creationCumKm = useMemo(() => cumulativeDistanceKm(creationCoordinates), [creationCoordinates])
  const creationSmoothed = useMemo(
    () => smoothElevations(creationElevations, creationCumKm),
    [creationElevations, creationCumKm],
  )
  const creationElevStats = useMemo(
    () => calculateElevationStats(creationSmoothed),
    [creationSmoothed],
  )
  const creationHasElevation = creationSmoothed.some(e => e !== null)

  /**
   * Only summarise while the traced segments still describe the geometry on
   * screen — a stale breakdown from the previous route is worse than none.
   */
  const surfaceSummary = useMemo(
    () =>
      surfaceSegments && surfaceSegments.length === creationCumKm.length - 1
        ? summarizeSurface(surfaceSegments, creationCumKm)
        : null,
    [surfaceSegments, creationCumKm],
  )

  const surfaceReady = surfaceSummary !== null && surfaceSummary.shares.length > 0

  /** Sidebar sparkline path, sampled down to a manageable number of points. */
  const creationSparkPath = useMemo(() => {
    const values = creationSmoothed.filter((e): e is number => e !== null)
    if (values.length < 2) return ''
    const W = 260
    const H = 52
    const PAD = 2
    const MAX_POINTS = 160
    const stride = Math.max(1, Math.ceil(values.length / MAX_POINTS))
    const sampled = values.filter((_, i) => i % stride === 0)
    const range = creationElevStats.max - creationElevStats.min || 1
    const step = (W - PAD * 2) / (sampled.length - 1)
    return (
      'M' +
      sampled
        .map((e, i) => {
          const x = PAD + i * step
          const y = H - PAD - ((e - creationElevStats.min) / range) * (H - PAD * 2)
          return `${x.toFixed(1)},${y.toFixed(1)}`
        })
        .join('L')
    )
  }, [creationSmoothed, creationElevStats])

  const creationGpx = useCallback(
    (name: string) =>
      buildGPX({
        name,
        coordinates: creationCoordinates,
        // Only places of interest. The route points are scaffolding for the
        // router — writing them as waypoints too just litters the file with
        // markers nobody asked for.
        waypoints: creationPins,
      }),
    [creationCoordinates, creationPins],
  )

  const handleSaveCreation = useCallback(async () => {
    const name = saveFileName.trim()
    if (!name) { notify('Please enter a track name', 'error'); return }

    const filename = toGpxFilename(name)
    if (savedFiles.some(f => f.filename.toLowerCase() === filename)) {
      notify('A track with this name already exists', 'error')
      return
    }

    try {
      const res = await fetch(`${API_BASE}/gpx/${encodeURIComponent(filename)}`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/gpx+xml' },
        body: creationGpx(name),
      })
      if (!res.ok) throw new Error(`Server returned ${res.status}`)
      notify(`Saved as ${filename}`, 'success')
      setSaveFileName('')
      loadSavedFiles()
      setViewMode('welcome')
    } catch (err) {
      notify(`Error saving track: ${(err as Error).message}`, 'error')
    }
  }, [saveFileName, savedFiles, creationGpx, loadSavedFiles, notify])

  const downloadCreation = useCallback(() => {
    const name = saveFileName.trim() || 'Created Track'
    downloadGpx(creationGpx(name), toGpxFilename(name))
  }, [saveFileName, creationGpx, downloadGpx])

  /** Wipe the route back to an empty map, staying on the planner. */
  const clearCreation = useCallback(() => {
    routeSeqRef.current++
    setCreationWaypoints([])
    setCreationPins([])
    setWaypointMode(false)
    clearRouteDerived()
    setSaveFileName('')
  }, [clearRouteDerived])

  const resetCreation = useCallback(() => {
    routeSeqRef.current++
    setCreationWaypoints([])
    setCreationPins([])
    setWaypointMode(false)
    clearRouteDerived()
    setCreationPois({ fuel: [], water: [], camp: [] })
    setCreationPoiCache({})
    setPlaceCache(undefined)
    // Otherwise the next track starts with the panels hidden and no hint why.
    setMapOnly(false)
    loadSavedFiles()
    setViewMode('welcome')
  }, [clearRouteDerived, loadSavedFiles])

  const fitMcpPoints = (points: Coordinate[]) => {
    if (points.length === 0) return
    window.setTimeout(() => {
      const map = mapRef.current
      if (!map) return
      if (points.length === 1) {
        map.flyTo([points[0].lat, points[0].lon], Math.max(map.getZoom(), 13), { duration: 0.4 })
        return
      }
      map.fitBounds(L.latLngBounds(points.map(point => [point.lat, point.lon])), { padding: [40, 40] })
    }, 100)
  }

  async function handleMcpCommand(command: McpCommand): Promise<unknown> {
    switch (command.name) {
      case 'plan_route': {
        if (viewMode !== 'creation') throw new Error('plan_route requires planner mode; use switch_mode first')
        const controls = command.arguments.points.map(point => ({
          id: ++routePointIDRef.current,
          lat: point.lat,
          lon: point.lon,
          elevation: point.elevation,
        }))
        const next = command.arguments.mode === 'append'
          ? [...creationWaypoints, ...controls]
          : controls
        if (next.length > 100) throw new Error('The planner cannot contain more than 100 route controls')
        routeSeqRef.current++
        clearRouteDerived()
        if (command.arguments.profile) setRoutingProfile(command.arguments.profile)
        setWaypointMode(false)
        setCreationWaypoints(next)
        if (command.arguments.fitView) fitMcpPoints(next)
        return { ok: true, screen: 'creation', routePointCount: next.length }
      }
      case 'draw_track': {
        if (viewMode !== 'view') throw new Error('draw_track requires editor mode; use open_track or switch_mode first')
        if (!currentTrack) throw new Error('Open a track before editing its geometry')
        const nextCoordinates = command.arguments.mode === 'append'
          ? [...currentTrack.coordinates, ...command.arguments.points]
          : command.arguments.points
        if (nextCoordinates.length > 6000) throw new Error('The track cannot contain more than 6000 points')
        applyEdit('Track geometry updated by MCP', track => ({
          ...track,
          coordinates: nextCoordinates,
          elevations: command.arguments.mode === 'append'
            ? [...track.elevations, ...command.arguments.points.map(point => point.elevation ?? null)]
            : nextCoordinates.map(point => point.elevation ?? null),
        }))
        if (command.arguments.fitView) fitMcpPoints(nextCoordinates)
        return { ok: true, screen: 'view', pointCount: nextCoordinates.length, dirty: true }
      }
      case 'set_waypoints': {
        if (command.arguments.target === 'planner') {
          if (viewMode !== 'creation') throw new Error('Planner waypoints require planner mode; use switch_mode first')
          const next = command.arguments.mode === 'append'
            ? [...creationPins, ...command.arguments.waypoints]
            : command.arguments.waypoints
          if (next.length > 1000) throw new Error('The planner cannot contain more than 1000 waypoints')
          setCreationPins(next)
          return { ok: true, target: 'planner', waypointCount: next.length }
        }
        if (viewMode !== 'view') throw new Error('Track waypoints require editor mode; use open_track or switch_mode first')
        if (!currentTrack) throw new Error('Open a track before editing its waypoints')
        const next = command.arguments.mode === 'append'
          ? [...currentTrack.waypoints, ...command.arguments.waypoints]
          : command.arguments.waypoints
        if (next.length > 1000) throw new Error('The track cannot contain more than 1000 waypoints')
        applyEdit('Waypoints updated by MCP', track => ({ ...track, waypoints: next }))
        return { ok: true, target: 'track', waypointCount: next.length, dirty: true }
      }
      case 'draw_map_track': {
        if (!hasInteractiveMap(viewMode)) {
          throw new Error('Open the planner, a track, or Explore before drawing a session map track')
        }
        const next = command.arguments.mode === 'append'
          ? [...sessionMapTrack, ...command.arguments.points]
          : command.arguments.points
        if (next.length > 6000) throw new Error('The session map track cannot contain more than 6000 points')
        setSessionMapTrack(next)
        if (command.arguments.fitView) fitMcpPoints(next)
        return { ok: true, screen: viewMode, pointCount: next.length, persisted: false }
      }
      case 'set_map_markers': {
        if (!hasInteractiveMap(viewMode)) {
          throw new Error('Open the planner, a track, or Explore before adding session map markers')
        }
        const next = command.arguments.mode === 'append'
          ? [...sessionMapMarkers, ...command.arguments.markers]
          : command.arguments.markers
        if (next.length > 1000) throw new Error('The session map cannot contain more than 1000 markers')
        setSessionMapMarkers(next)
        if (command.arguments.fitView) fitMcpPoints(next)
        return { ok: true, screen: viewMode, markerCount: next.length, persisted: false }
      }
      case 'switch_mode': {
        if (command.arguments.mode === 'editor' && !currentTrack) {
          throw new Error('Open a track before switching to editor mode')
        }
        const screen: ViewMode = command.arguments.mode === 'planner'
          ? 'creation'
          : command.arguments.mode === 'editor'
            ? 'view'
            : 'explore'
        setWaypointMode(false)
        setSelectionMode(false)
        setCursorPos(null)
        setViewMode(screen)
        return { ok: true, mode: command.arguments.mode, screen }
      }
      case 'set_map_view': {
        if (!hasInteractiveMap(viewMode)) {
          throw new Error('Open the planner, a track, or Explore before moving the map')
        }
        const map = mapRef.current
        if (!map) throw new Error('The active map is not ready')
        map.setView(
          [command.arguments.lat, command.arguments.lon],
          command.arguments.zoom ?? map.getZoom(),
          { animate: false },
        )
        return { ok: true, ...readMapViewport(map) }
      }
      case 'open_track': {
        if (dirty) {
          throw new Error('The current track has unsaved edits; save or discard them in Overland before opening another track')
        }
        const response = await fetch(`${API_BASE}/gpx/${encodeURIComponent(command.arguments.filename)}`)
        if (!response.ok) throw new Error(`Could not open ${command.arguments.filename}: server returned ${response.status}`)
        const parsed = parseGPX(await response.text())
        if (parsed.length === 0) throw new Error(`${command.arguments.filename} contains no tracks or routes`)
        openTracks(parsed, command.arguments.filename)
        return { ok: true, filename: command.arguments.filename, trackCount: parsed.length, selectedTrackIndex: 0 }
      }
      case 'select_track': {
        if (viewMode !== 'view') throw new Error('select_track requires editor mode; use open_track or switch_mode first')
        if (command.arguments.index >= tracks.length) {
          throw new Error(`Track index ${command.arguments.index} is out of range`)
        }
        selectTrack(command.arguments.index)
        return { ok: true, selectedTrackIndex: command.arguments.index }
      }
      case 'load_pois': {
        if (command.arguments.scope === 'current_view') {
          if (viewMode !== 'creation') throw new Error('Current-view POIs require the route planner')
          const outcome = await loadCreationPois(command.arguments.kinds)
          if (outcome.errors.length > 0) throw new Error(outcome.errors.join('; '))
          return { ok: true, scope: command.arguments.scope, loaded: outcome.loaded }
        }
        if (viewMode !== 'view') throw new Error('Current-track POIs require editor mode; use open_track or switch_mode first')
        if (!currentTrack) throw new Error('Current-track POIs require an open track')
        const bbox = boundsAround(currentTrack.coordinates, 5)
        if (!bbox) throw new Error('The current track has no searchable bounds')
        let loaded = 0
        for (const kind of command.arguments.kinds) {
          setPoiLoading(kind)
          try {
            const result = await fetchPoisForArea(kind, bbox, undefined, {
              runtime,
              onCacheMetadata: handleCacheMetadata,
            })
            setActivePois(previous => ({ ...previous, [kind]: result.pois }))
            setActivePoiCache(previous => ({ ...previous, [kind]: result.cache }))
            loaded += result.pois.length
          } finally {
            setPoiLoading(null)
          }
        }
        return { ok: true, scope: command.arguments.scope, loaded }
      }
    }
  }

  function getMcpSnapshot() {
    const map = hasInteractiveMap(viewMode) ? mapRef.current : null
    const bounds = map?.getBounds()
    const displayedPois = viewMode === 'creation'
      ? creationPois
      : viewMode === 'view'
        ? activePois
        : { fuel: [], water: [], camp: [] }
    const loadedPois = POI_KINDS.flatMap(kind =>
      displayedPois[kind.id].map(poi => ({ ...poi, kind: kind.id })),
    )
    const visiblePois = bounds
      ? loadedPois.filter(poi => bounds.contains([poi.lat, poi.lon]))
      : []
    const displayedWaypoints = viewMode === 'creation'
      ? creationPins
      : viewMode === 'view'
        ? currentTrack?.waypoints ?? []
        : []
    const visibleWaypoints = bounds
      ? displayedWaypoints.filter(waypoint => bounds.contains([waypoint.lat, waypoint.lon]))
      : []
    const visibleSessionMarkers = bounds
      ? sessionMapMarkers.filter(marker => bounds.contains([marker.lat, marker.lon]))
      : []

    return {
      version: 1,
      screen: viewMode,
      splashVisible: showSplash,
      theme,
      mapOnly,
      map: map ? readMapViewport(map) : null,
      mapPresentation: {
        baseLayer: activeBaseLayer,
        availableLayers: terrainLayers.map(layer => ({ id: layer.id, label: layer.label, title: layer.title, kind: layer.kind })),
        hillshade,
        hillshadeOpacity,
        colorMode,
        vectorIssue: vectorMapIssue,
        tilePrefetch,
        offlineMode: runtime.offline?.mode,
      },
      markerCatalog: WAYPOINT_MARKERS.map(({ id, label, glyph, color, gpxSymbol }) => ({
        id,
        label,
        glyph,
        color,
        gpxSymbol,
      })),
      mapOverlays: {
        persisted: false,
        track: sessionMapTrack,
        markers: sessionMapMarkers,
      },
      library: savedFiles.map(file => ({
        filename: file.filename,
        name: file.name,
        distanceKm: file.distance,
        elevation: file.elevStats,
        hasTime: file.hasTime,
      })),
      planner: {
        routePoints: creationWaypoints,
        waypoints: creationPins,
        profile: routingProfile,
        displayCoordinates: creationCoordinates,
        routedCoordinates,
        distanceKm: creationDistance,
        durationSeconds: routedDuration,
        engine: routedEngine,
        loading: routedLoading,
        status: routeStatus,
        routeError,
        routingData,
        elevationInterpolated,
        elevationError: elevationApiError,
        surface: surfaceSummary,
        pois: POI_KINDS.flatMap(kind => creationPois[kind.id].map(poi => ({ ...poi, kind: kind.id }))),
        poiCache: creationPoiCache,
      },
      editor: {
        tracks: tracks.map((track, index) => ({
          index,
          name: track.name,
          filename: track.filename,
          pointCount: track.coordinates.length,
          waypointCount: track.waypoints.length,
        })),
        selectedTrackIndex: currentTrack ? selectedTrackIndex : null,
        currentTrack: currentTrack ?? null,
        dirty,
        undoDepth: editHistory.length,
        toolsVisible: showTools,
        selectionMode,
        selection,
        distanceKm: trackDistance,
        elevation: elevationStats,
        time: timeStats,
        pois: POI_KINDS.flatMap(kind => activePois[kind.id].map(poi => ({ ...poi, kind: kind.id }))),
        poiCache: activePoiCache,
      },
      visibleFeatures: {
        loadedPois,
        visiblePois,
        loadedWaypoints: displayedWaypoints,
        visibleWaypoints,
        loadedMapMarkers: sessionMapMarkers,
        visibleMapMarkers: visibleSessionMarkers,
      },
      placeSearch: {
        query: placeSearch,
        results: placeResults,
        searching: placeSearching,
        cache: placeCache,
      },
      cursor: cursorPos ? { ...cursorPos, groundElevation: cursorElevation } : null,
      // Everything the user was told, newest last. Agents should check this
      // after a command: work started by a command can fail after it returns.
      notifications: recentMessages,
    }
  }

  /* -- Ground elevation under the cursor ---------------------------- */

  useEffect(() => {
    if (!cursorPos) { setCursorElevation(null); return }
    const controller = new AbortController()
    const timer = setTimeout(async () => {
      try {
        const value = await fetchGroundElevation(
          cursorPos.lat,
          cursorPos.lon,
          API_BASE,
          ELEVATION_API,
          ELEVATION_DATASET,
          controller.signal,
          { runtime, onCacheMetadata: handleCacheMetadata },
        )
        setCursorElevation(value)
      } catch {
        setCursorElevation(null)
      }
    }, 220)
    return () => { clearTimeout(timer); controller.abort() }
  }, [cursorPos, handleCacheMetadata, runtime])

  useMcpBridge(API_BASE, getMcpSnapshot, handleMcpCommand)

  /* -- Render ------------------------------------------------------- */

  /** Surface colouring is only offered on a screen that has surface data. */
  const renderTerrainControls = (mode?: ColorMode, surfaceAvailable = false) => (
    <TerrainControls
      baseLayerId={activeBaseLayer}
      onBaseLayer={selectBaseLayer}
      hillshade={hillshade}
      onHillshade={setHillshade}
      hillshadeOpacity={hillshadeOpacity}
      onHillshadeOpacity={setHillshadeOpacity}
      colorMode={mode}
      onColorMode={mode ? setColorMode : undefined}
      surfaceAvailable={surfaceAvailable}
      layers={terrainLayers}
      hillshadeAvailable={Boolean(hillshadeLayer)}
      vectorIssue={vectorMapIssue}
    />
  )

  const cursorReadout = cursorPos && (
    <div className="map-cursor-readout">
      {cursorPos.lat.toFixed(5)}, {cursorPos.lon.toFixed(5)}
      {cursorElevation !== null && (
        <span className="cursor-elevation">▲ {cursorElevation.toFixed(0)} m</span>
      )}
    </div>
  )
  const offlineModeAction = runtime.offline && (
    <OfflineModeToggle
      mode={runtime.offline.mode}
      available={Boolean(runtime.offline.modeControl)}
      busy={offlineModeBusy}
      onToggle={() => void toggleOfflineMode()}
    />
  )

  return (
    <div className="app">
      {showSplash && <SplashScreen onDone={() => setShowSplash(false)} />}
      <NotificationBar notifications={notifications} onDismiss={dismissNotification} />

      {(viewMode === 'welcome' || viewMode === 'upload') && (
        <div className="corner-actions">
          {offlineModeAction}
          <ThemeToggle theme={theme} onToggle={() => setTheme(t => (t === 'light' ? 'dark' : 'light'))} inline />
        </div>
      )}

      {/* ===================== WELCOME ============================= */}
      {viewMode === 'welcome' && (
        <div className="welcome-screen">
          <div className="welcome-bg-decoration" aria-hidden="true" />
          <div className="welcome-content">
            <div className="welcome-hero">
              <div className="welcome-logo">
                <svg viewBox="0 0 24 24" width="34" height="34" fill="none" stroke="currentColor" strokeWidth="1.7" strokeLinecap="round" strokeLinejoin="round">
                  <path d="M3 18l5-11 4 7 3-5 6 9z" />
                  <circle cx="17.5" cy="5.5" r="2" />
                </svg>
              </div>
              <h1>
                <span className="welcome-title-mark">GPX</span> Editor
              </h1>
              <p className="welcome-subtitle">
                Explore, plan and edit offroad routes — terrain, gradient and elevation at a glance
              </p>
              <div className="welcome-buttons">
                <button className="btn btn-explore" onClick={() => setViewMode('explore')}>
                  <svg viewBox="0 0 24 24" width="18" height="18" fill="none" stroke="currentColor" strokeWidth="1.8" strokeLinecap="round" strokeLinejoin="round">
                    <circle cx="12" cy="12" r="9" />
                    <path d="m15.8 8.2-2.1 5.5-5.5 2.1 2.1-5.5 5.5-2.1Z" />
                  </svg>
                  Explore map
                </button>
                <button className="btn btn-primary" onClick={() => setViewMode('creation')}>
                  <svg viewBox="0 0 24 24" width="18" height="18" fill="currentColor">
                    <path d="M19 13h-6v6h-2v-6H5v-2h6V5h2v6h6v2z" />
                  </svg>
                  Plan a route
                </button>
                <button className="btn btn-ghost" onClick={() => setViewMode('upload')}>
                  <svg viewBox="0 0 24 24" width="18" height="18" fill="currentColor">
                    <path d="M9 16h6v-6h4l-7-7-7 7h4v6zm-4 2h14v2H5v-2z" />
                  </svg>
                  Open a GPX file
                </button>
              </div>
            </div>

            {savedFiles.length > 0 && (
              <div className="saved-files-section">
                <div className="saved-files-header">
                  <h3>
                    Library
                    <span className="library-summary">
                      {savedFiles.length} track{savedFiles.length === 1 ? '' : 's'} ·{' '}
                      {savedFiles.reduce((sum, f) => sum + f.distance, 0).toFixed(0)} km
                    </span>
                  </h3>
                  {savedFiles.length > 4 && (
                    <input
                      type="search"
                      className="library-filter"
                      placeholder="Filter…"
                      value={trackFilter}
                      onChange={e => setTrackFilter(e.target.value)}
                    />
                  )}
                </div>

                <ul className="track-card-grid">
                  {filteredTracks.map(file => (
                    <TrackCard
                      key={file.filename}
                      track={file}
                      tileUrl={thumbnailLayer.url}
                      tileMaxZoom={thumbnailLayer.maxZoom}
                      onOpen={() => loadTrackFromFile(file.filename)}
                      onDelete={() => deleteFile(file.filename)}
                    />
                  ))}
                </ul>

                {filteredTracks.length === 0 && (
                  <p className="library-empty">No tracks match “{trackFilter}”</p>
                )}

                {filteredTracks.length > 0 && (
                  <p
                    className="track-card-attribution"
                    dangerouslySetInnerHTML={{ __html: `Thumbnail maps: ${thumbnailLayer.attribution}` }}
                  />
                )}
              </div>
            )}
          </div>
        </div>
      )}

      {/* ===================== UPLOAD ============================== */}
      {viewMode === 'upload' && (
        <div className="upload-screen">
          <div className="upload-header">
            <button className="btn btn-ghost" onClick={() => setViewMode('welcome')}>
              <svg viewBox="0 0 24 24" width="18" height="18" fill="currentColor">
                <path d="M20 11H7.83l5.59-5.59L12 4l-8 8 8 8 1.41-1.41L7.83 13H20v-2z" />
              </svg>
              Back
            </button>
            <h2>Upload GPX File</h2>
          </div>
          <div
            className={`drop-zone ${dragOver ? 'drag-over' : ''}`}
            onDragOver={handleDragOver}
            onDragLeave={handleDragLeave}
            onDrop={handleDrop}
          >
            <div className="drop-zone-icon">
              <svg viewBox="0 0 24 24" width="48" height="48" fill="none" stroke="currentColor" strokeWidth="1.5">
                <path d="M21 15v4a2 2 0 01-2 2H5a2 2 0 01-2-2v-4M17 8l-5-5-5 5M12 3v12" />
              </svg>
            </div>
            <p className="drop-zone-text">Drag and drop a GPX file here</p>
            <p className="drop-zone-divider">or</p>
            <label className="file-input-label">
              <span className="btn btn-primary">Browse Files</span>
              <input type="file" accept=".gpx" onChange={handleFileUpload} />
            </label>
          </div>
        </div>
      )}

      {/* ===================== EXPLORE ============================= */}
      {viewMode === 'explore' && (
        <ExploreScreen
          runtime={runtime}
          nominatimApi={nominatimApi}
          baseLayerId={activeBaseLayer}
          hillshade={hillshade}
          hillshadeOpacity={hillshadeOpacity}
          layers={terrainLayers}
          hillshadeLayer={hillshadeLayer}
          terrainControls={renderTerrainControls()}
          modeAction={offlineModeAction}
          themeAction={<ThemeToggle theme={theme} onToggle={() => setTheme(t => (t === 'light' ? 'dark' : 'light'))} inline />}
          vectorIssue={vectorMapIssue}
          onVectorStatus={handleVectorStatus}
          onDismissVectorIssue={() => setVectorMapIssue(null)}
          onHome={() => setViewMode('welcome')}
          onCacheMetadata={handleCacheMetadata}
          onNotify={notify}
          onRoutingStatus={setRoutingData}
          onMapInstance={registerActiveMap}
          mapOverlays={<SessionMapOverlays track={sessionMapTrack} markers={sessionMapMarkers} />}
        />
      )}

      {/* ===================== CREATION ============================ */}
      {viewMode === 'creation' && (
        <div className={`creation-screen${mapOnly ? ' creation-screen--full' : ''}`}>
          <div className="creation-sidebar">
            {elevationApiError && (
              <div className="elev-api-banner">
                <svg viewBox="0 0 24 24" width="15" height="15" fill="currentColor" style={{ flexShrink: 0, marginTop: 1 }}>
                  <path d="M12 2C6.48 2 2 6.48 2 12s4.48 10 10 10 10-4.48 10-10S17.52 2 12 2zm1 15h-2v-2h2v2zm0-4h-2V7h2v6z" />
                </svg>
                <div>
                  <strong>Elevation unavailable</strong>
                  <p>No elevation is available for this route. The track will be saved without invented heights.</p>
                </div>
              </div>
            )}

            <div className="sidebar-section">
              <div className="place-search-box">
                <div className="place-search-input-row">
                  <svg viewBox="0 0 24 24" width="15" height="15" fill="currentColor" className="place-search-icon">
                    <path d="M15.5 14h-.79l-.28-.27A6.471 6.471 0 0 0 16 9.5 6.5 6.5 0 1 0 9.5 16c1.61 0 3.09-.59 4.23-1.57l.27.28v.79l5 4.99L20.49 19l-4.99-5zm-6 0C7.01 14 5 11.99 5 9.5S7.01 5 9.5 5 14 7.01 14 9.5 11.99 14 9.5 14z" />
                  </svg>
                  <input
                    type="text"
                    className="place-search-input"
                    placeholder="Search village or place…"
                    value={placeSearch}
                    onChange={e => setPlaceSearch(e.target.value)}
                    onKeyDown={e => { if (e.key === 'Enter') void searchPlace(placeSearch) }}
                  />
                  {placeSearching ? (
                    <span className="place-search-spinner" />
                  ) : (
                    <button className="place-search-btn" onClick={() => void searchPlace(placeSearch)} title="Search">
                      Go
                    </button>
                  )}
                </div>
                {placeResults.length > 0 && (
                  <ul className="place-results">
                    {placeCache && (
                      <li className="place-cache-context">
                        <CacheContext metadata={placeCache} />
                      </li>
                    )}
                    {placeResults.map(r => (
                      <li key={r.place_id} onClick={() => flyToPlace(r.lat, r.lon)} title={r.display_name}>
                        {r.display_name}
                      </li>
                    ))}
                  </ul>
                )}
              </div>
            </div>

            <div className="sidebar-section">
              <h3>
                <svg viewBox="0 0 24 24" width="18" height="18" fill="currentColor">
                  <path d="M12 2C8.13 2 5 5.13 5 9c0 5.25 7 13 7 13s7-7.75 7-13c0-3.87-3.13-7-7-7zm0 9.5c-1.38 0-2.5-1.12-2.5-2.5s1.12-2.5 2.5-2.5 2.5 1.12 2.5 2.5-1.12 2.5-2.5 2.5z" />
                </svg>
                Route Information
              </h3>
              {creationCoordinates.length > 1 ? (
                <div className="route-stats">
                  <div className="route-stat-row">
                    <span className="route-stat-label">Distance</span>
                    <span className="route-stat-value">{creationDistance.toFixed(2)} km</span>
                  </div>
                  {routedDuration !== null && (
                    <div className="route-stat-row">
                      <span className="route-stat-label">Est. time</span>
                      <span className="route-stat-value">{formatDuration(routedDuration)}</span>
                    </div>
                  )}
                  <div className="route-stat-row">
                    <span className="route-stat-label">Total elevation</span>
                    <span className="route-stat-value">
                      {creationHasElevation ? (
                        <>
                          <span className="elev-gain">+{creationElevStats.gain.toFixed(0)}m</span>
                          {' / '}
                          <span className="elev-loss">-{creationElevStats.loss.toFixed(0)}m</span>
                        </>
                      ) : (
                        <span className="route-stat-muted">unavailable</span>
                      )}
                    </span>
                  </div>
                  {routedEngine && (
                    <div className="route-stat-row">
                      <span className="route-stat-label">Engine</span>
                      <span className="route-stat-value route-stat-muted">{routedEngine}</span>
                    </div>
                  )}
                  {creationHasElevation && creationSparkPath && (
                    <div className="route-elev-sparkline">
                      <div className="sparkline-labels">
                        <span>{creationElevStats.max.toFixed(0)}m</span>
                        <span>{creationElevStats.min.toFixed(0)}m</span>
                      </div>
                      <svg
                        viewBox="0 0 260 52"
                        preserveAspectRatio="none"
                        className="sparkline-svg"
                        aria-label="Elevation profile"
                      >
                        <path d={`${creationSparkPath}L258,52L2,52Z`} fill="var(--accent-light)" stroke="none" />
                        <path
                          d={creationSparkPath}
                          fill="none"
                          stroke="var(--gradient-start)"
                          strokeWidth="1.5"
                          strokeLinejoin="round"
                          strokeLinecap="round"
                        />
                      </svg>
                    </div>
                  )}
                  {elevationInterpolated && (
                    <p className="route-stat-note">
                      Some clipped route points use interpolated elevation. Those heights are shown here but omitted from GPX export.
                    </p>
                  )}
                </div>
              ) : (
                <p className="sidebar-empty">Add waypoints to see route info</p>
              )}
            </div>

            <div className="sidebar-section">
              <h3>
                <svg viewBox="0 0 24 24" width="18" height="18" fill="currentColor">
                  <path d="M14 6l-3.75 5 2.85 3.8-1.6 1.2C9.81 13.75 7 10 7 10l-6 8h22L14 6z" />
                </svg>
                Surface
              </h3>

              {surfaceReady && surfaceSummary ? (
                <div className="surface-panel">
                  <div className="surface-headline">
                    <span>
                      <strong>{(surfaceSummary.unpavedFraction * 100).toFixed(0)}%</strong> unpaved
                    </span>
                    <span className="surface-headline-sub">
                      {surfaceSummary.unpavedKm.toFixed(1)} of {surfaceSummary.totalKm.toFixed(1)} km
                    </span>
                  </div>

                  <div className="surface-bar" aria-hidden="true">
                    {surfaceSummary.shares.map(share => (
                      <span
                        key={share.id}
                        className="surface-bar-part"
                        style={{
                          width: `${(share.fraction * 100).toFixed(2)}%`,
                          background: surfaceDefinition(share.id).color,
                        }}
                        title={`${surfaceDefinition(share.id).label} — ${share.km.toFixed(1)} km`}
                      />
                    ))}
                  </div>

                  <ul className="surface-legend">
                    {surfaceSummary.shares.map(share => {
                      const def = surfaceDefinition(share.id)
                      return (
                        <li key={share.id} title={def.hint}>
                          <span className="surface-swatch" style={{ background: def.color }} />
                          <span className="surface-legend-label">{def.label}</span>
                          <span className="surface-legend-value">
                            {share.km.toFixed(1)} km · {(share.fraction * 100).toFixed(0)}%
                          </span>
                        </li>
                      )
                    })}
                  </ul>

                  <button
                    className="surface-mode-btn"
                    onClick={() => setColorMode(colorMode === 'surface' ? 'slope' : 'surface')}
                    title="Paint the route on the map with these colours"
                  >
                    {colorMode === 'surface' ? 'Back to gradient colours' : 'Colour route by surface'}
                  </button>

                  {surfaceSummary.unknownKm > 0.05 && (
                    <p className="route-stat-note">
                      {surfaceSummary.unknownKm.toFixed(1)} km carries no surface tag in
                      OpenStreetMap. It is shown as Unknown and left out of the unpaved share
                      rather than assumed sealed.
                    </p>
                  )}
                </div>
              ) : (
                <p className="sidebar-empty">Add waypoints to see what the route is made of</p>
              )}
            </div>

            <div className="sidebar-section sidebar-section--grow">
              <h3>
                <svg viewBox="0 0 24 24" width="18" height="18" fill="currentColor">
                  <path d="M12 2C8.13 2 5 5.13 5 9c0 5.25 7 13 7 13s7-7.75 7-13c0-3.87-3.13-7-7-7zm0 9.5c-1.38 0-2.5-1.12-2.5-2.5s1.12-2.5 2.5-2.5 2.5 1.12 2.5 2.5-1.12 2.5-2.5 2.5z" />
                </svg>
                Route points
                <span className="badge">{creationWaypoints.length}</span>
                <span className="waypoint-actions">
                  <button
                    className="wpt-action-btn"
                    onClick={undoLastWaypoint}
                    disabled={creationWaypoints.length === 0}
                    title="Undo last waypoint (Ctrl+Z)"
                  >
                    <svg viewBox="0 0 24 24" width="13" height="13" fill="currentColor">
                      <path d="M12.5 8c-2.65 0-5.05.99-6.9 2.6L2 7v9h9l-3.62-3.62c1.39-1.16 3.16-1.88 5.12-1.88 3.54 0 6.55 2.31 7.6 5.5l2.37-.78C21.08 11.03 17.15 8 12.5 8z" />
                    </svg>
                  </button>
                  <button
                    className="wpt-action-btn"
                    onClick={reverseWaypoints}
                    disabled={creationWaypoints.length < 2}
                    title="Reverse track direction"
                  >
                    <svg viewBox="0 0 24 24" width="13" height="13" fill="currentColor">
                      <path d="M6.99 11L3 15l3.99 4v-3H14v-2H6.99v-3zM21 9l-3.99-4v3H10v2h7.01v3L21 9z" />
                    </svg>
                  </button>
                </span>
              </h3>
              {creationWaypoints.length === 0 ? (
                <p className="sidebar-empty">Click on the map to add points</p>
              ) : (
                <ul className="waypoint-list">
                  {creationWaypoints.map((w, index) => (
                    <li key={w.id} style={{ animationDelay: `${index * 0.05}s` }}>
                      <span className="waypoint-info">
                        <span className="waypoint-number">#{index + 1}</span>
                        <span className="waypoint-coords">{w.lat.toFixed(4)}, {w.lon.toFixed(4)}</span>
                        {w.elevation !== undefined && <span className="waypoint-elev">{w.elevation.toFixed(0)}m</span>}
                      </span>
                      <button className="waypoint-delete" onClick={() => deleteWaypoint(w.id)} title="Remove waypoint">
                        <svg viewBox="0 0 24 24" width="12" height="12" fill="currentColor">
                          <path d="M19 6.41L17.59 5 12 10.59 6.41 5 5 6.41 10.59 12 5 17.59 6.41 19 12 13.41 17.59 19 19 17.59 13.41 12z" />
                        </svg>
                      </button>
                    </li>
                  ))}
                </ul>
              )}
            </div>
          </div>

          <div className="creation-main">
            <div className="creation-header">
              <div className="creation-title-row">
                <h2>Create New Track</h2>
                <p>Click on the map to add waypoints. Add at least 2 points to generate a route.</p>
                <div className="routing-profile-toggle">
                  {ROUTING_PROFILES.map(p => (
                    <button
                      key={p.id}
                      className={`profile-btn${routingProfile === p.id ? ' active' : ''}`}
                      onClick={() => setRoutingProfile(p.id)}
                      title={p.hint}
                    >
                      {p.label}
                    </button>
                  ))}
                </div>
              </div>
              <div className="creation-controls">
                <button
                  className={`btn btn-ghost btn-sm${waypointMode ? ' active' : ''}`}
                  onClick={() => setWaypointMode(v => !v)}
                  title="Mark a place worth stopping at — a monument, a viewpoint, a spring. It is saved with the file but the route does not detour through it."
                >
                  <svg viewBox="0 0 24 24" width="15" height="15" fill="currentColor">
                    <path d="M12 2C8.13 2 5 5.13 5 9c0 5.25 7 13 7 13s7-7.75 7-13c0-3.87-3.13-7-7-7zm0 9.5a2.5 2.5 0 010-5 2.5 2.5 0 010 5z" />
                  </svg>
                  {waypointMode ? 'Click the map…' : 'Add place'}
                  {creationPins.length > 0 && <span className="badge">{creationPins.length}</span>}
                </button>
                <button className="btn btn-primary" onClick={downloadCreation} disabled={creationCoordinates.length < 2}>
                  <svg viewBox="0 0 24 24" width="16" height="16" fill="currentColor">
                    <path d="M5 20h14v-2H5v2zM19 9h-4V3H9v6H5l7 7 7-7z" />
                  </svg>
                  Download
                </button>
                <div className="save-input-group">
                  <input
                    type="text"
                    placeholder="Track name"
                    value={saveFileName}
                    onChange={e => setSaveFileName(e.target.value)}
                    disabled={creationCoordinates.length < 2}
                  />
                  <button
                    className="btn btn-success"
                    onClick={handleSaveCreation}
                    disabled={creationCoordinates.length < 2 || !saveFileName.trim()}
                  >
                    Save
                  </button>
                </div>
                <button
                  className="btn btn-ghost btn-sm"
                  onClick={clearCreation}
                  disabled={creationWaypoints.length === 0 && creationPins.length === 0}
                  title="Remove every point and start this track again"
                >
                  <svg viewBox="0 0 24 24" width="15" height="15" fill="currentColor">
                    <path d="M15.14 3a2 2 0 00-1.41.59L3 14.32a2 2 0 000 2.83L6.85 21H12l9-9a2 2 0 000-2.83l-4.45-4.58A2 2 0 0015.14 3zM7.5 19.1L4.9 16.5l6-6 2.6 2.6-6 6z" />
                  </svg>
                  Clear
                </button>
                <button className="btn btn-danger btn-sm" onClick={resetCreation} title="Leave the planner">
                  <svg viewBox="0 0 24 24" width="15" height="15" fill="currentColor">
                    <path d="M19 6.41L17.59 5 12 10.59 6.41 5 5 6.41 10.59 12 5 17.59 6.41 19 12 13.41 17.59 19 19 17.59 13.41 12z" />
                  </svg>
                  Discard
                </button>
                {offlineModeAction}
                <ThemeToggle theme={theme} onToggle={() => setTheme(t => (t === 'light' ? 'dark' : 'light'))} inline />
              </div>
            </div>

            <div className={`map-container${waypointMode ? ' placing-waypoint' : ''}`}>
              <MapContainer
                center={[41.65, -0.88]}
                zoom={9}
                minZoom={1}
                style={{ width: '100%', height: '100%' }}
                scrollWheelZoom
                zoomControl={false}
                ref={registerActiveMap}
              >
                <RoutingDownloadControl runtime={runtime} status={routingData} onStatus={setRoutingData} />
                <MapControlLayout />
                <MapTiles
                  baseLayerId={activeBaseLayer}
                  hillshade={hillshade}
                  hillshadeOpacity={hillshadeOpacity}
                  onVectorStatus={handleVectorStatus}
                  layers={terrainLayers}
                  hillshadeLayer={hillshadeLayer}
                />
                <ZoomControl position="bottomright" />
                <ViewportReporter onSettle={handleViewportSettle} />
                {creationWaypoints.map(w => (
                  <Marker
                    key={w.id}
                    position={[w.lat, w.lon]}
                    draggable
                    eventHandlers={{
                      dragend: e => {
                        const { lat, lng } = (e.target as L.Marker).getLatLng()
                        handleWaypointDrag(w.id, lat, lng)
                      },
                    }}
                  >
                    <Popup>Route point</Popup>
                  </Marker>
                ))}

                {/* Places of interest: marked, saved, but never routed through. */}
                {creationPins.map((pin, i) => (
                  <Marker
                    key={`pin-${i}`}
                    position={[pin.lat, pin.lon]}
                    icon={waypointIcon(waypointMarkerIdFromSymbol(pin.sym) ?? 'generic')}
                  >
                    <Popup>
                      <strong>{pin.name}</strong>
                      <WaypointMarkerSelect symbol={pin.sym} onChange={marker => changeCreationPinMarker(i, marker)} />
                      <span className="wpt-popup-actions">
                        <button className="btn btn-ghost btn-xs" onClick={() => renameCreationPin(i)}>Rename</button>
                        <button className="btn btn-danger btn-xs" onClick={() => removeCreationPin(i)}>Delete</button>
                      </span>
                    </Popup>
                  </Marker>
                ))}
                {routedCoordinates.length > 1 && (
                  // Surface colouring needs no elevation, so it can draw a
                  // meaningful line even where the DEM is unreachable.
                  creationHasElevation || (colorMode === 'surface' && surfaceReady) ? (
                    <ColoredTrack
                      coordinates={routedCoordinates}
                      elevations={creationSmoothed}
                      cumKm={creationCumKm}
                      colorMode={colorMode}
                      elevMin={creationElevStats.min}
                      elevMax={creationElevStats.max}
                      surfaces={surfaceReady ? surfaceSegments! : undefined}
                    />
                  ) : (
                    <Polyline
                      positions={routedCoordinates.map(c => [c.lat, c.lon] as [number, number])}
                      color="#508DCC"
                      weight={4}
                    />
                  )
                )}
                {POI_KINDS.flatMap(kind =>
                  creationPois[kind.id].map(poi => (
                    <Marker
                      key={`${kind.id}:${poi.id}`}
                      position={[poi.lat, poi.lon]}
                      icon={poiIcon(kind.glyph, creationFuelColors.get(poi.id) ?? kind.color)}
                    >
                      <Popup>
                        <PoiPopupBody poi={poi} fallbackLabel={kind.label} />
                      </Popup>
                    </Marker>
                  )),
                )}
                <SessionMapOverlays track={sessionMapTrack} markers={sessionMapMarkers} />

                <MapClickHandler onClick={handleMapClick} />
                <MapMouseTracker onMove={setCursorPos} />
              </MapContainer>

              <div className="creation-poi-strip">
                <span className="creation-poi-label">In view</span>
                {POI_KINDS.map(kind => (
                  <button
                    key={kind.id}
                    className={`creation-poi-btn${creationPois[kind.id].length > 0 ? ' active' : ''}`}
                    onClick={() => toggleCreationPoiLayer(kind.id)}
                    disabled={creationPoiLoading !== null}
                    title={`${kind.title.replace('near the route', 'in the area you are looking at')} — searches the current map view`}
                  >
                    {creationPoiLoading === kind.id
                      ? '…'
                      : `${kind.glyph} ${kind.label}`}
                    {creationPois[kind.id].length > 0 && (
                      <span className="creation-poi-count">{creationPois[kind.id].length}</span>
                    )}
                    <CacheContext metadata={creationPoiCache[kind.id]} compact />
                  </button>
                ))}
                {activeCreationPoiKinds.length > 0 && (
                  <button
                    className="creation-poi-btn creation-poi-refresh"
                    onClick={() => void loadCreationPois(activeCreationPoiKinds)}
                    disabled={creationPoiLoading !== null}
                    title="Search again where the map is now"
                  >
                    <svg viewBox="0 0 24 24" width="13" height="13" fill="currentColor">
                      <path d="M17.65 6.35A7.958 7.958 0 0 0 12 4a8 8 0 1 0 7.73 10h-2.08A6 6 0 1 1 12 6c1.66 0 3.14.69 4.22 1.78L13 11h7V4l-2.35 2.35z" />
                    </svg>
                    Search here
                  </button>
                )}
              </div>

              <button
                className={`map-full-toggle${mapOnly ? ' active' : ''}`}
                onClick={() => setMapOnly(v => !v)}
                title={mapOnly ? 'Show the panels again (Esc)' : 'Full map — hide the panels'}
                aria-pressed={mapOnly}
              >
                {mapOnly ? (
                  <svg viewBox="0 0 24 24" width="14" height="14" fill="currentColor">
                    <path d="M5 16h3v3h2v-5H5v2zm3-8H5v2h5V5H8v3zm6 11h2v-3h3v-2h-5v5zm2-11V5h-2v5h5V8h-3z" />
                  </svg>
                ) : (
                  <svg viewBox="0 0 24 24" width="14" height="14" fill="currentColor">
                    <path d="M7 14H5v5h5v-2H7v-3zm-2-4h2V7h3V5H5v5zm12 7h-3v2h5v-5h-2v3zM14 5v2h3v3h2V5h-5z" />
                  </svg>
                )}
                {mapOnly && <span className="map-full-toggle-label">Exit full map · Esc</span>}
              </button>

              <FuelPriceLegend
                pois={creationPois.fuel}
                reference={fuelReference}
                onReference={setFuelReference}
              />

              <div className="map-control-stack" data-testid="map-control-stack">
                {renderTerrainControls(colorMode, surfaceReady)}
              </div>
              <VectorMapDiagnostic
                issue={activeBaseLayer === 'openfreemap' ? vectorMapIssue : null}
                onDismiss={() => setVectorMapIssue(null)}
              />
              {cursorReadout}

              {creationWaypoints.length === 0 && (
                <div className="map-overlay-hint">Click on the map to add waypoints</div>
              )}

              {tilePrefetch && (
                <div className="tile-progress">
                  <span className="tile-progress-label">
                    Downloading elevation tiles… {tilePrefetch.done}/{tilePrefetch.total}
                  </span>
                  <span className="tile-progress-track">
                    <span
                      className="tile-progress-bar"
                      style={{
                        width: `${tilePrefetch.total > 0
                          ? Math.round((tilePrefetch.done / tilePrefetch.total) * 100)
                          : 0}%`,
                      }}
                    />
                  </span>
                  {/* Zoomed out, only the middle of the view is worth caching. */}
                  {tilePrefetch.clamped && tilePrefetch.reason && (
                    <span className="tile-progress-note">{tilePrefetch.reason}</span>
                  )}
                </div>
              )}
            </div>
          </div>
        </div>
      )}

      {/* ===================== VIEW ================================ */}
      {viewMode === 'view' && currentTrack && (
        <div className="view-screen">
          <div className="stats-panel">
            <div className="stats-header">
              <h3 title={currentTrack.filename}>
                {/* Titled off the file, like the library card, so the header
                    matches the card you clicked. The GPX <name> is left
                    untouched and still written back on save. */}
                {fromGpxFilename(currentTrack.filename) || currentTrack.name}
                {dirty && <span className="dirty-dot" title="Unsaved edits">•</span>}
              </h3>
              <div className="stats-actions">
                {tracks.length > 1 && (
                  <select
                    className="track-select"
                    value={selectedTrackIndex}
                    onChange={e => selectTrack(parseInt(e.target.value))}
                    title="Select track"
                  >
                    {tracks.map((track, index) => (
                      <option key={index} value={index}>{track.name}</option>
                    ))}
                  </select>
                )}
                <button className="btn btn-ghost btn-sm" onClick={() => setViewMode('welcome')}>
                  <svg viewBox="0 0 24 24" width="16" height="16" fill="currentColor">
                    <path d="M20 11H7.83l5.59-5.59L12 4l-8 8 8 8 1.41-1.41L7.83 13H20v-2z" />
                  </svg>
                  Home
                </button>
                <button className="btn btn-primary btn-sm" onClick={downloadCurrentTrack}>
                  <svg viewBox="0 0 24 24" width="16" height="16" fill="currentColor">
                    <path d="M5 20h14v-2H5v2zM19 9h-4V3H9v6H5l7 7 7-7z" />
                  </svg>
                  Download
                </button>
                <button className="btn btn-success btn-sm" onClick={saveCurrentTrack} title="Save back to the library">
                  Save
                </button>
                <button
                  className="btn btn-danger btn-sm"
                  onClick={clearTrack}
                  title="Clear the map and start again"
                >
                  Clear
                </button>
                {offlineModeAction}
                <ThemeToggle theme={theme} onToggle={() => setTheme(t => (t === 'light' ? 'dark' : 'light'))} inline />
              </div>
            </div>

            <div className="stats-grid">
              <div className="stat-item">
                <span className="stat-label">Distance</span>
                <span className="stat-value">{trackDistance.toFixed(2)} <small>km</small></span>
              </div>
              <div className="stat-item">
                <span className="stat-label">Min Alt</span>
                <span className="stat-value">{elevationStats.min.toFixed(0)} <small>m</small></span>
              </div>
              <div className="stat-item">
                <span className="stat-label">Max Alt</span>
                <span className="stat-value">{elevationStats.max.toFixed(0)} <small>m</small></span>
              </div>
              <div className="stat-item stat-gain">
                <span className="stat-label">Gain</span>
                <span className="stat-value">+{elevationStats.gain.toFixed(0)} <small>m</small></span>
              </div>
              <div className="stat-item stat-loss">
                <span className="stat-label">Loss</span>
                <span className="stat-value">-{elevationStats.loss.toFixed(0)} <small>m</small></span>
              </div>
              <div className="stat-item">
                <span className="stat-label">Points</span>
                <span className="stat-value">{currentTrack.coordinates.length}</span>
              </div>
              {timeStats && (
                <div className="stat-item">
                  <span className="stat-label">Moving</span>
                  <span className="stat-value">
                    {Math.floor(timeStats.movingSeconds / 3600)}h{' '}
                    {Math.round((timeStats.movingSeconds % 3600) / 60)}
                    <small>min</small>
                  </span>
                </div>
              )}
              {timeStats && (
                <div className="stat-item">
                  <span className="stat-label">Avg moving</span>
                  <span className="stat-value">{timeStats.movingSpeedKmh.toFixed(1)} <small>km/h</small></span>
                </div>
              )}
              {/*
                * Always rendered, even before the fuel layer is on. Adding a
                * tile on toggle used to wrap the grid onto a second row, which
                * grew the panel and shoved the map and profile down the page.
                */}
              <div
                className={`stat-item${fuelGap !== null && fuelGap > 200 ? ' stat-warn' : ''}`}
                title={fuelGap === null ? 'Turn on the Fuel layer to measure this' : undefined}
              >
                <span className="stat-label">Longest fuel gap</span>
                <span className="stat-value">
                  {fuelGap === null
                    ? <span className="stat-pending">—</span>
                    : <>{fuelGap.toFixed(0)} <small>km</small></>}
                </span>
              </div>
            </div>

            <div className="toolbar-strip">
              <button
                className={`btn btn-ghost btn-xs toolbar-toggle${showTools ? ' active' : ''}`}
                onClick={() => setShowTools(v => !v)}
                title="Show track editing tools"
              >
                <svg viewBox="0 0 24 24" width="13" height="13" fill="currentColor">
                  <path d="M22.7 19l-9.1-9.1c.9-2.3.4-5-1.5-6.9-2-2-5-2.4-7.4-1.3L9 6 6 9 1.6 4.7C.4 7.1.9 10.1 2.9 12.1c1.9 1.9 4.6 2.4 6.9 1.5l9.1 9.1c.4.4 1 .4 1.4 0l2.3-2.3c.5-.4.5-1.1.1-1.4z" />
                </svg>
                Tools
              </button>

              <button
                className={`btn btn-ghost btn-xs${waypointMode ? ' active' : ''}`}
                onClick={() => setWaypointMode(v => !v)}
                title="Click anywhere on the map to drop a waypoint — it does not have to sit on the track"
              >
                <svg viewBox="0 0 24 24" width="13" height="13" fill="currentColor">
                  <path d="M12 2C8.13 2 5 5.13 5 9c0 5.25 7 13 7 13s7-7.75 7-13c0-3.87-3.13-7-7-7zm0 9.5a2.5 2.5 0 010-5 2.5 2.5 0 010 5z" />
                </svg>
                {waypointMode ? 'Click the map…' : 'Waypoint'}
              </button>

              <span className="toolbar-divider" />

              <span className="edit-toolbar-label">Nearby</span>
              {POI_KINDS.map(kind => (
                <button
                  key={kind.id}
                  className={`btn btn-ghost btn-xs${activePois[kind.id].length > 0 ? ' active' : ''}`}
                  onClick={() => togglePoiLayer(kind.id)}
                  disabled={poiLoading !== null}
                  title={activePoiCache[kind.id]
                    ? `${kind.title} · ${formatCacheContext(activePoiCache[kind.id]!)}`
                    : kind.title}
                >
                  {poiLoading === kind.id ? '…' : `${kind.glyph} ${kind.label}`}
                  <CacheContext metadata={activePoiCache[kind.id]} compact />
                </button>
              ))}

              {editHistory.length > 0 && (
                <>
                  <span className="toolbar-divider" />
                  <button className="btn btn-ghost btn-xs" onClick={undoEdit} title="Undo the last edit">
                    <svg viewBox="0 0 24 24" width="13" height="13" fill="currentColor">
                      <path d="M12.5 8c-2.65 0-5.05.99-6.9 2.6L2 7v9h9l-3.62-3.62c1.39-1.16 3.16-1.88 5.12-1.88 3.54 0 6.55 2.31 7.6 5.5l2.37-.78C21.08 11.03 17.15 8 12.5 8z" />
                    </svg>
                    Undo ({editHistory.length})
                  </button>
                </>
              )}
            </div>

            {showTools && (
              <div className="edit-toolbar">
                <div className="edit-cluster">
                  <span className="edit-cluster-label">Section</span>
                  <div className="edit-cluster-body">
                    <button
                      className={`btn btn-ghost btn-xs${selectionMode ? ' active' : ''}`}
                      onClick={() => {
                        setSelectionMode(m => !m)
                        setSelectionAnchor(null)
                        setSelection(null)
                        if (!selectionMode) setElevationCollapsed(false)
                      }}
                      title="Pick two points on the elevation profile to mark a section"
                    >
                      {selectionMode
                        ? selectionAnchor === null ? 'Pick start…' : 'Pick end…'
                        : 'Select range'}
                    </button>
                    <button
                      className="btn btn-ghost btn-xs"
                      disabled={!selection || selection.startIdx === selection.endIdx}
                      onClick={() => applyEdit('Trimmed to selection', t =>
                        trimTrack(t, selection!.startIdx, selection!.endIdx),
                      )}
                      title="Keep only the selected section"
                    >
                      Crop
                    </button>
                    <button
                      className="btn btn-ghost btn-xs"
                      disabled={!selection}
                      onClick={() => applyEdit('Split into two tracks', t => splitTrack(t, selection!.startIdx))}
                      title="Split the track at the start of the selection"
                    >
                      Split
                    </button>
                    <button
                      className="btn btn-ghost btn-xs"
                      onClick={() => applyEdit('Track reversed', reverseTrack)}
                      title="Reverse the direction of travel"
                    >
                      Reverse
                    </button>
                  </div>
                </div>

                <div className="edit-cluster">
                  <span className="edit-cluster-label">Break up</span>
                  <div className="edit-cluster-body">
                    <button
                      className="btn btn-ghost btn-xs"
                      disabled={trackDistance <= stageKm}
                      onClick={() => applyEdit('Split into day stages', t => splitIntoStages(t, stageKm))}
                      title="Cut into roughly equal daily stages"
                    >
                      Day stages
                    </button>
                    <span className="edit-group">
                      <input
                        type="number"
                        className="edit-number"
                        value={stageKm}
                        min={5}
                        step={5}
                        onChange={e => setStageKm(Math.max(5, parseInt(e.target.value) || 5))}
                        title="Kilometres per stage"
                      />
                      <span className="edit-unit">km</span>
                    </span>
                  </div>
                </div>

                <div className="edit-cluster">
                  <span className="edit-cluster-label">Point budget</span>
                  <div className="edit-cluster-body">
                    <button
                      className="btn btn-ghost btn-xs"
                      disabled={currentTrack.coordinates.length <= simplifyTarget}
                      onClick={() => {
                        const simplified = simplifyToMaxPoints(currentTrack, simplifyTarget)
                        applyEdit(
                          `Simplified ${currentTrack.coordinates.length} → ${simplified.coordinates.length} points`,
                          () => simplified,
                        )
                      }}
                      title="Thin the track down for GPS units that cap track points"
                    >
                      Simplify
                    </button>
                    <span className="edit-group">
                      <input
                        type="number"
                        className="edit-number"
                        value={simplifyTarget}
                        min={50}
                        step={50}
                        onChange={e => setSimplifyTarget(Math.max(50, parseInt(e.target.value) || 50))}
                        title="Maximum track points"
                      />
                      <span className="edit-unit">pts</span>
                    </span>
                  </div>
                </div>

                <div className="edit-cluster">
                  <span className="edit-cluster-label">Elevation</span>
                  <div className="edit-cluster-body">
                    <button
                      className="btn btn-ghost btn-xs"
                      disabled={!hasElevationData}
                      onClick={() => applyEdit('Elevation smoothed', t => smoothTrackElevation(t, 60))}
                      title="Bake noise-filtering into the stored elevation"
                    >
                      Smooth
                    </button>
                    <button
                      className="btn btn-ghost btn-xs"
                      onClick={refetchElevation}
                      title="Re-read every point's elevation from the terrain model"
                    >
                      Refetch from DEM
                    </button>
                  </div>
                </div>
              </div>
            )}
          </div>

          <div className={`map-wrapper-with-elevation${hasElevationData && !elevationCollapsed ? ' elevation-expanded' : ''}${waypointMode ? ' placing-waypoint' : ''}`}>
            <MapContainer
              center={[currentTrack.coordinates[0]?.lat ?? 0, currentTrack.coordinates[0]?.lon ?? 0]}
              zoom={13}
              minZoom={1}
              style={{ width: '100%', flex: 1 }}
              zoomControl={false}
              ref={registerActiveMap}
            >
              <MapTiles
                baseLayerId={activeBaseLayer}
                hillshade={hillshade}
                hillshadeOpacity={hillshadeOpacity}
                onVectorStatus={handleVectorStatus}
                layers={terrainLayers}
                hillshadeLayer={hillshadeLayer}
              />
              <ZoomControl position="bottomright" />
              <MapClickHandler onClick={handleViewMapClick} />

              <ColoredTrack
                coordinates={currentTrack.coordinates}
                elevations={smoothedElevations}
                cumKm={cumKm}
                colorMode={viewColorMode}
                elevMin={elevationStats.min}
                elevMax={elevationStats.max}
              />

              {selection && currentTrack.coordinates.length > 0 && (
                <Polyline
                  positions={currentTrack.coordinates
                    .slice(
                      Math.min(selection.startIdx, selection.endIdx),
                      Math.max(selection.startIdx, selection.endIdx) + 1,
                    )
                    .map(c => [c.lat, c.lon] as [number, number])}
                  color="#111827"
                  weight={9}
                  opacity={0.28}
                />
              )}

              {currentTrack.waypoints.map((w, i) => (
                <Marker
                  key={`wpt-${i}`}
                  position={[w.lat, w.lon]}
                  icon={waypointIcon(waypointMarkerIdFromSymbol(w.sym) ?? 'generic')}
                >
                  <Popup>
                    <strong>{w.name ?? `Waypoint ${i + 1}`}</strong>
                    {w.desc && <><br />{w.desc}</>}
                    {w.elevation !== undefined && <><br />{w.elevation.toFixed(0)} m</>}
                    <WaypointMarkerSelect symbol={w.sym} onChange={marker => changeWaypointMarker(i, marker)} />
                    <span className="wpt-popup-actions">
                      <button className="btn btn-ghost btn-xs" onClick={() => renameWaypoint(i)}>Rename</button>
                      <button className="btn btn-danger btn-xs" onClick={() => removeWaypoint(i)}>Delete</button>
                    </span>
                  </Popup>
                </Marker>
              ))}

              {POI_KINDS.flatMap(kind =>
                activePois[kind.id].map(poi => (
                  <Marker
                    key={poi.id}
                    position={[poi.lat, poi.lon]}
                    icon={poiIcon(kind.glyph, viewFuelColors.get(poi.id) ?? kind.color)}
                  >
                    <Popup>
                      <PoiPopupBody poi={poi} fallbackLabel={kind.label} />
                    </Popup>
                  </Marker>
                )),
              )}
              <SessionMapOverlays track={sessionMapTrack} markers={sessionMapMarkers} />

              <MapJump target={jumpTarget} />
              <MapFitBounds coordinates={currentTrack.coordinates} />
              <MapRestoreView
                restore={restoreView}
                coordinates={currentTrack.coordinates}
                onRestored={() => { setRestoreView(false); setIsZoomedToBar(false) }}
              />
              <MapMouseTracker onMove={setCursorPos} />
            </MapContainer>

            <FuelPriceLegend
              pois={activePois.fuel}
              reference={fuelReference}
              onReference={setFuelReference}
            />

            <div className="map-control-stack" data-testid="map-control-stack">
              {renderTerrainControls(viewColorMode, false)}
              {offlineRoute && (
                <OfflineStoragePanel
                  runtime={runtime}
                  routeKey={offlineRoute.key}
                  routeName={offlineRoute.name}
                  route={offlineRoute.coordinates}
                />
              )}
            </div>
            <VectorMapDiagnostic
              issue={activeBaseLayer === 'openfreemap' ? vectorMapIssue : null}
              onDismiss={() => setVectorMapIssue(null)}
            />
            {cursorReadout}

            {isZoomedToBar && (
              <button className="restore-view-btn" onClick={() => setRestoreView(true)}>
                <svg viewBox="0 0 24 24" width="16" height="16" fill="currentColor">
                  <path d="M15 3l2.3 2.3-2.89 2.87 1.42 1.42L18.7 6.7 21 9V3h-6zM3 9l2.3-2.3 2.87 2.89 1.42-1.42L6.7 5.3 9 3H3v6zm6 12l-2.3-2.3 2.89-2.87-1.42-1.42L5.3 17.3 3 15v6h6zm12-6l-2.3 2.3-2.87-2.89-1.42 1.42 2.89 2.87L15 21h6v-6z" />
                </svg>
                Restore view
              </button>
            )}

            {hasElevationData ? (
              <ElevationProfile
                bars={profileBars}
                stats={elevationStats}
                yAxisTicks={yAxisTicks}
                colorMode={viewColorMode}
                collapsed={elevationCollapsed}
                onToggleCollapsed={() => setElevationCollapsed(c => !c)}
                hoveredBar={hoveredBar}
                onHoverBar={setHoveredBar}
                activeCoordIndex={activeCoordIndex}
                onSelectBar={handleSelectBar}
                selectionMode={selectionMode}
                selection={selection}
              />
            ) : (
              <div className="elevation-profile collapsed">
                <div className="elevation-profile-header">
                  <div className="header-content">
                    <span className="header-title">No elevation data in this track</span>
                    <span className="expand-hint">— use “Refetch ele” to read it from the terrain model</span>
                  </div>
                </div>
              </div>
            )}
          </div>
        </div>
      )}

      {/* ===================== LOADING ============================= */}
      {(loading || routedLoading) && (
        <div className="loading-overlay">
          <div className="loading-content">
            <div className="loading-spinner" />
            <p>{loadingMessage || routeStatus || 'Working…'}</p>
          </div>
        </div>
      )}
    </div>
  )
}

export default App
