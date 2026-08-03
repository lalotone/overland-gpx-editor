import { useState, useRef, useEffect, useCallback, useMemo } from 'react'
import { MapContainer, Marker, Polyline, Popup, useMap } from 'react-leaflet'
import L from 'leaflet'

import { ColoredTrack } from './components/ColoredTrack'
import { ElevationProfile } from './components/ElevationProfile'
import type { ProfileBar, TrimSelection } from './components/ElevationProfile'
import { MapTiles, TerrainControls } from './components/MapLayers'
import { SplashScreen } from './components/SplashScreen'
import { TrackCard } from './components/TrackCard'
import type { TrackPreview } from './components/TrackCard'

import {
  attachElevations,
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
import { fetchRouteSurface, summarizeSurface, surfaceDefinition } from './lib/surface'
import type { SurfaceClass } from './lib/surface'
import { altitudeColor, getBaseLayer, getSegmentColor } from './lib/terrain'
import type { ColorMode } from './lib/terrain'
import type { Coordinate, GpxWaypoint, Track } from './lib/types'

import './App.css'

/* ------------------------------------------------------------------ */
/*  Constants                                                          */
/* ------------------------------------------------------------------ */

// Empty means "same origin": the Go binary serves this bundle and the API
// together. In `npm run dev` the Vite proxy forwards those paths to the
// backend on :8000. Set VITE_API_BASE to point at a backend somewhere else.
const API_BASE = import.meta.env.VITE_API_BASE ?? ''
// Optional direct-to-DEM address, used only if the backend proxy fails. Empty
// by default: the backend serves this page, so if it cannot be reached there
// is nothing left to fall back from.
const ELEVATION_API = import.meta.env.VITE_ELEVATION_API ?? ''
const ELEVATION_DATASET = import.meta.env.VITE_ELEVATION_DATASET ?? 'srtm30m'

const MAX_PROFILE_BARS = 200
const FLY_TO_DURATION = 1.5
const BAR_ZOOM_LEVEL = 16
/** Wait this long after the last waypoint edit before routing. */
const ROUTE_DEBOUNCE_MS = 350
/** Point budget for a track destined for a GPS unit. */
const DEFAULT_SIMPLIFY_TARGET = 500
/** How far off the route a fuel station still counts as reachable. */
const FUEL_CORRIDOR_M = 3000

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

function waypointIcon(label: string): L.DivIcon {
  const key = `wpt:${label}`
  let icon = iconCache.get(key)
  if (!icon) {
    icon = L.divIcon({
      className: 'gpx-waypoint-marker',
      html: `<span class="gpx-waypoint-inner">${label}</span>`,
      iconSize: [20, 20],
      iconAnchor: [10, 10],
    })
    iconCache.set(key, icon)
  }
  return icon
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

function NotificationBar({
  notifications,
  onDismiss,
}: {
  notifications: Notification[]
  onDismiss: (id: number) => void
}) {
  return (
    <div className="notification-container">
      {notifications.map(n => (
        <div key={n.id} className={`notification notification-${n.type}`}>
          <span>{n.message}</span>
          <button onClick={() => onDismiss(n.id)} className="notification-close">&times;</button>
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

/* ------------------------------------------------------------------ */
/*  Main App                                                           */
/* ------------------------------------------------------------------ */

function App() {
  const [viewMode, setViewMode] = useState<'welcome' | 'upload' | 'creation' | 'view'>('welcome')
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

  /* -- Terrain / map presentation ----------------------------------- */

  const [baseLayer, setBaseLayer] = useState(() => localStorage.getItem('gpx-base-layer') ?? 'topo')
  const [hillshade, setHillshade] = useState(() => localStorage.getItem('gpx-hillshade') !== 'off')
  const [hillshadeOpacity, setHillshadeOpacity] = useState(
    () => parseFloat(localStorage.getItem('gpx-hillshade-opacity') ?? '0.45'),
  )
  const [colorMode, setColorMode] = useState<ColorMode>(
    () => (localStorage.getItem('gpx-color-mode') as ColorMode) ?? 'slope',
  )

  /** Library thumbnails use whichever base map the user reads the map in. */
  const thumbnailLayer = useMemo(() => getBaseLayer(baseLayer), [baseLayer])

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
  const [poiLoading, setPoiLoading] = useState<PoiKind | null>(null)

  /* -- Creation ----------------------------------------------------- */

  const [creationWaypoints, setCreationWaypoints] = useState<Waypoint[]>([])
  const [routedCoordinates, setRoutedCoordinates] = useState<Coordinate[]>([])
  const [routedDuration, setRoutedDuration] = useState<number | null>(null)
  const [routedEngine, setRoutedEngine] = useState<string | null>(null)
  const [routedLoading, setRoutedLoading] = useState(false)
  const [routeStatus, setRouteStatus] = useState('')
  const [elevationApiError, setElevationApiError] = useState(false)
  const [elevationInterpolated, setElevationInterpolated] = useState(false)
  const [routingProfile, setRoutingProfile] = useState<RoutingProfile>('mixed')
  /**
   * POIs found while planning. Kept apart from the view screen's `activePois`,
   * which are tied to a loaded track: these are tied to a map view instead,
   * and switching screens should not carry one set into the other.
   */
  const [creationPois, setCreationPois] = useState<Record<PoiKind, Poi[]>>({ fuel: [], water: [], camp: [] })
  const [creationPoiLoading, setCreationPoiLoading] = useState<PoiKind | null>(null)
  /** Full-map mode: sidebar and header hidden, map overlays kept. */
  const [mapOnly, setMapOnly] = useState(false)
  /** Fuel the price colouring ranks on, shared by both screens. */
  const [fuelReference, setFuelReference] = useState(DEFAULT_FUEL_REFERENCE)
  const [surfaceSegments, setSurfaceSegments] = useState<SurfaceClass[] | null>(null)
  const [surfaceApproximate, setSurfaceApproximate] = useState(false)
  const [surfaceLoading, setSurfaceLoading] = useState(false)
  const [surfaceError, setSurfaceError] = useState<string | null>(null)
  const [exportWaypoints, setExportWaypoints] = useState(false)
  const [placeSearch, setPlaceSearch] = useState('')
  const [placeResults, setPlaceResults] = useState<{ display_name: string; lat: string; lon: string }[]>([])
  const [placeSearching, setPlaceSearching] = useState(false)

  const mapRef = useRef<L.Map | null>(null)
  const notifIdRef = useRef(0)
  const routeSeqRef = useRef(0)

  /* -- Notifications ------------------------------------------------ */

  const notify = useCallback((message: string, type: NotificationType = 'info') => {
    const id = ++notifIdRef.current
    setNotifications(prev => [...prev, { id, message, type }])
    setTimeout(() => setNotifications(prev => prev.filter(n => n.id !== id)), 5000)
  }, [])

  const dismissNotification = useCallback((id: number) => {
    setNotifications(prev => prev.filter(n => n.id !== id))
  }, [])

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

  const openTracks = useCallback((parsed: Track[], filename: string) => {
    setTracks(parsed.map(t => ({ ...t, filename })))
    setSelectedTrackIndex(0)
    setEditHistory([])
    setDirty(false)
    setSelection(null)
    setSelectionAnchor(null)
    setSelectionMode(false)
    setActivePois({ fuel: [], water: [], camp: [] })
    setViewMode('view')
  }, [])

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
          await fetch(`${API_BASE}/upload`, { method: 'POST', body: formData })
          loadSavedFiles()
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
        return
      }
      if (!currentTrack) return
      const bbox = boundsAround(currentTrack.coordinates, 5)
      if (!bbox) return

      setPoiLoading(kind)
      try {
        const { pois, source } = await fetchPoisForArea(kind, bbox)
        setActivePois(prev => ({ ...prev, [kind]: pois }))
        if (pois.length === 0) notify(`No ${kind} points found near this route`, 'info')
        else if (source) notify(`${pois.length} fuel stations with ${source}`, 'info')
      } catch (err) {
        notify(`Could not load ${kind} points: ${(err as Error).message}`, 'error')
      } finally {
        setPoiLoading(null)
      }
    },
    [activePois, currentTrack, notify],
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

  useEffect(() => {
    if (!mapOnly) return
    // Escape is the way out that needs no on-screen furniture.
    const onKey = (e: KeyboardEvent) => { if (e.key === 'Escape') setMapOnly(false) }
    window.addEventListener('keydown', onKey)
    return () => window.removeEventListener('keydown', onKey)
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
      if (!bbox) return

      const { widthKm, heightKm } = boundingBoxSpanKm(bbox)
      const span = Math.max(widthKm, heightKm)
      if (span > MAX_SEARCH_SPAN_KM) {
        notify(
          `This view spans about ${span.toFixed(0)} km — zoom in to under ${MAX_SEARCH_SPAN_KM} km to search it`,
          'info',
        )
        return
      }

      // One kind at a time: a refresh asks for three layers at once, and
      // Overpass is a shared free service.
      for (const kind of kinds) {
        setCreationPoiLoading(kind)
        try {
          const { pois, source, truncated } = await fetchPoisForArea(kind, bbox)
          setCreationPois(prev => ({ ...prev, [kind]: pois }))
          const label = POI_KINDS.find(k => k.id === kind)?.label.toLowerCase() ?? kind
          if (pois.length === 0) {
            notify(`No ${label} points in this view`, 'info')
          } else if (truncated) {
            notify(`Showing the ${pois.length} ${label} points nearest the centre of this view`, 'info')
          } else if (source) {
            notify(`${pois.length} fuel stations with ${source}`, 'info')
          }
        } catch (err) {
          notify(`Could not load ${kind} points: ${(err as Error).message}`, 'error')
        } finally {
          setCreationPoiLoading(null)
        }
      }
    },
    [viewportBounds, notify],
  )

  const toggleCreationPoiLayer = useCallback(
    (kind: PoiKind) => {
      if (creationPois[kind].length > 0) {
        setCreationPois(prev => ({ ...prev, [kind]: [] }))
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
    setSelectedTrackIndex(0)
    setEditHistory([])
    setDirty(false)
    setSelection(null)
    setSelectionAnchor(null)
    setSelectionMode(false)
    setWaypointMode(false)
    setActivePois({ fuel: [], water: [], camp: [] })
    setSurfaceSegments(null)
    setSurfaceError(null)
    setSurfaceApproximate(false)
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
      applyEdit('Waypoint added', t => ({
        ...t,
        waypoints: [...t.waypoints, { lat, lon, name: `Waypoint ${t.waypoints.length + 1}` }],
      }))
    },
    [applyEdit],
  )

  const renameWaypoint = useCallback(
    (index: number) => {
      const current = tracks[selectedTrackIndex]?.waypoints[index]
      const name = prompt('Waypoint name', current?.name ?? '')?.trim()
      if (!name) return
      applyEdit('Waypoint renamed', t => ({
        ...t,
        waypoints: t.waypoints.map((w, i) => (i === index ? { ...w, name } : w)),
      }))
    },
    [applyEdit, tracks, selectedTrackIndex],
  )

  const removeWaypoint = useCallback(
    (index: number) => {
      applyEdit('Waypoint removed', t => ({
        ...t,
        waypoints: t.waypoints.filter((_, i) => i !== index),
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
      const { elevations } = await fetchElevationProfile(
        currentTrack.coordinates,
        API_BASE,
        ELEVATION_API,
        {
          dataset: ELEVATION_DATASET,
          onProgress: (done, total) =>
            setLoadingMessage(`Reading elevation… ${done}/${total} points`),
        },
      )
      applyEdit('Elevation refreshed from terrain model', track => withElevations(track, elevations))
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
  }, [currentTrack, applyEdit, notify])

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
      )
      if (elevation === null) { setElevationApiError(true); return }
      setElevationApiError(false)
      setCreationWaypoints(prev =>
        prev.map(w => (w.id === waypoint.id ? { ...w, elevation } : w)),
      )
    } catch {
      setElevationApiError(true)
    }
  }, [])

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
          { lat: e.latlng.lat, lon: e.latlng.lng, name: `Point of interest ${prev.length + 1}` },
        ])
        return
      }

      const waypoint: Waypoint = { id: Date.now(), lat: e.latlng.lat, lon: e.latlng.lng }
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
    if (!query.trim()) { setPlaceResults([]); return }
    setPlaceSearching(true)
    try {
      const res = await fetch(
        `https://nominatim.openstreetmap.org/search?q=${encodeURIComponent(query)}&format=json&limit=5`,
        { headers: { 'Accept-Language': 'en' } },
      )
      setPlaceResults(await res.json())
    } catch {
      setPlaceResults([])
    }
    setPlaceSearching(false)
  }, [])

  const flyToPlace = useCallback((lat: string, lon: string) => {
    mapRef.current?.flyTo([parseFloat(lat), parseFloat(lon)], 13, { duration: FLY_TO_DURATION })
    setPlaceResults([])
    setPlaceSearch('')
  }, [])

  /* -- Creation: routing -------------------------------------------- */

  useEffect(() => {
    if (viewMode !== 'creation') return

    if (creationWaypoints.length < 2) {
      routeSeqRef.current++
      setRoutedCoordinates([])
      setRoutedDuration(null)
      setRoutedEngine(null)
      setElevationInterpolated(false)
      setRoutedLoading(false)
      setSurfaceSegments(null)
      setSurfaceError(null)
      setSurfaceLoading(false)
      return
    }

    const controller = new AbortController()
    const seq = ++routeSeqRef.current
    // Only the newest request may write state; a slow earlier response
    // must never overwrite a newer route.
    const isCurrent = () => seq === routeSeqRef.current && !controller.signal.aborted

    const timer = setTimeout(async () => {
      setRoutedLoading(true)
      setRouteStatus('Calculating route…')
      try {
        const result = await calculateRoute(
          creationWaypoints.map(w => ({ lat: w.lat, lon: w.lon })),
          routingProfile,
          controller.signal,
        )
        if (!isCurrent()) return

        setRoutedCoordinates(result.coordinates)
        setRoutedDuration(result.durationSeconds)
        setRoutedEngine(result.engine)
        if (result.warning) notify(result.warning, 'info')

        // Surface is an overlay on top of a route that already works, so it
        // runs alongside elevation instead of in front of it: a slow or
        // missing trace service must never hold up the numbers that matter.
        setSurfaceSegments(null)
        setSurfaceError(null)
        setSurfaceLoading(true)
        void fetchRouteSurface(result.coordinates, routingProfile, controller.signal)
          .then(surface => {
            if (!isCurrent()) return
            setSurfaceSegments(surface.segments)
            setSurfaceApproximate(surface.approximate)
          })
          .catch(err => {
            if ((err as Error).name === 'AbortError' || !isCurrent()) return
            setSurfaceSegments(null)
            setSurfaceError((err as Error).message || 'Surface data unavailable')
          })
          .finally(() => { if (isCurrent()) setSurfaceLoading(false) })

        setRouteStatus('Reading elevation…')
        const { coordinates, interpolated } = await attachElevations(
          result.coordinates,
          API_BASE,
          ELEVATION_API,
          {
            signal: controller.signal,
            dataset: ELEVATION_DATASET,
            onProgress: (done, total) => {
              if (isCurrent()) setRouteStatus(`Reading elevation… ${done}/${total}`)
            },
          },
        )
        if (!isCurrent()) return

        setRoutedCoordinates(coordinates)
        setElevationInterpolated(interpolated)
        setElevationApiError(false)
      } catch (err) {
        if ((err as Error).name === 'AbortError' || !isCurrent()) return
        if (err instanceof ElevationUnavailableError) {
          setElevationApiError(true)
        } else {
          notify((err as Error).message || 'Could not calculate route', 'error')
        }
      } finally {
        if (isCurrent()) { setRoutedLoading(false); setRouteStatus('') }
      }
    }, ROUTE_DEBOUNCE_MS)

    return () => { clearTimeout(timer); controller.abort() }
  }, [creationWaypoints, routingProfile, viewMode, notify])

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
        // Places of interest always travel with the file; the routing points
        // only when asked for, since they are scaffolding rather than content.
        waypoints: [
          ...creationPins,
          ...(exportWaypoints
            ? creationWaypoints.map((w, i) => ({
                lat: w.lat,
                lon: w.lon,
                elevation: w.elevation,
                name: `WP ${i + 1}`,
              }))
            : []),
        ],
      }),
    [creationCoordinates, creationWaypoints, creationPins, exportWaypoints],
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

  const resetCreation = useCallback(() => {
    routeSeqRef.current++
    setCreationWaypoints([])
    setCreationPins([])
    setWaypointMode(false)
    setRoutedCoordinates([])
    setRoutedDuration(null)
    setRoutedEngine(null)
    setElevationApiError(false)
    setElevationInterpolated(false)
    setSurfaceSegments(null)
    setSurfaceError(null)
    setCreationPois({ fuel: [], water: [], camp: [] })
    // Otherwise the next track starts with the panels hidden and no hint why.
    setMapOnly(false)
    loadSavedFiles()
    setViewMode('welcome')
  }, [loadSavedFiles])

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
        )
        setCursorElevation(value)
      } catch {
        setCursorElevation(null)
      }
    }, 220)
    return () => { clearTimeout(timer); controller.abort() }
  }, [cursorPos])

  /* -- Render ------------------------------------------------------- */

  /** Surface colouring is only offered on a screen that has surface data. */
  const renderTerrainControls = (mode: ColorMode, surfaceAvailable: boolean) => (
    <TerrainControls
      baseLayerId={baseLayer}
      onBaseLayer={setBaseLayer}
      hillshade={hillshade}
      onHillshade={setHillshade}
      hillshadeOpacity={hillshadeOpacity}
      onHillshadeOpacity={setHillshadeOpacity}
      colorMode={mode}
      onColorMode={setColorMode}
      surfaceAvailable={surfaceAvailable}
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

  return (
    <div className="app">
      {showSplash && <SplashScreen onDone={() => setShowSplash(false)} />}
      <NotificationBar notifications={notifications} onDismiss={dismissNotification} />

      {(viewMode === 'welcome' || viewMode === 'upload') && (
        <ThemeToggle theme={theme} onToggle={() => setTheme(t => (t === 'light' ? 'dark' : 'light'))} />
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
                Plan and edit offroad routes — terrain, gradient and elevation at a glance
              </p>
              <div className="welcome-buttons">
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
                  <p>The elevation service is not reachable. The track will be saved without elevation data.</p>
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
                    onKeyDown={e => e.key === 'Enter' && searchPlace(placeSearch)}
                  />
                  {placeSearching ? (
                    <span className="place-search-spinner" />
                  ) : (
                    <button className="place-search-btn" onClick={() => searchPlace(placeSearch)} title="Search">
                      Go
                    </button>
                  )}
                </div>
                {placeResults.length > 0 && (
                  <ul className="place-results">
                    {placeResults.map((r, i) => (
                      <li key={i} onClick={() => flyToPlace(r.lat, r.lon)} title={r.display_name}>
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
                      Long route — elevation sampled and interpolated between measured points.
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
                {surfaceLoading && <span className="surface-spinner" title="Reading surface…" />}
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
                  {surfaceApproximate && (
                    <p className="route-stat-note">
                      This route had to be matched back onto the road graph, so surface
                      boundaries are approximate.
                    </p>
                  )}
                </div>
              ) : surfaceLoading ? (
                <p className="sidebar-empty">Reading surface…</p>
              ) : surfaceError ? (
                <p className="sidebar-empty">{surfaceError}</p>
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
                <label className="inline-check" title="Also write your clicked waypoints into the file as POIs">
                  <input
                    type="checkbox"
                    checked={exportWaypoints}
                    onChange={e => setExportWaypoints(e.target.checked)}
                  />
                  POIs
                </label>
                <button className="btn btn-danger" onClick={resetCreation}>
                  <svg viewBox="0 0 24 24" width="16" height="16" fill="currentColor">
                    <path d="M19 6.41L17.59 5 12 10.59 6.41 5 5 6.41 10.59 12 5 17.59 6.41 19 12 13.41 17.59 19 19 17.59 13.41 12z" />
                  </svg>
                  Discard
                </button>
                <ThemeToggle theme={theme} onToggle={() => setTheme(t => (t === 'light' ? 'dark' : 'light'))} inline />
              </div>
            </div>

            <div className={`map-container${waypointMode ? ' placing-waypoint' : ''}`}>
              <MapContainer
                center={[41.65, -0.88]}
                zoom={9}
                style={{ width: '100%', height: '100%' }}
                scrollWheelZoom
                ref={map => { if (map) mapRef.current = map }}
              >
                <MapTiles baseLayerId={baseLayer} hillshade={hillshade} hillshadeOpacity={hillshadeOpacity} />
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
                  <Marker key={`pin-${i}`} position={[pin.lat, pin.lon]} icon={waypointIcon('★')}>
                    <Popup>
                      <strong>{pin.name}</strong>
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

              {renderTerrainControls(colorMode, surfaceReady)}
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
                    onChange={e => setSelectedTrackIndex(parseInt(e.target.value))}
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
                  title={kind.title}
                >
                  {poiLoading === kind.id ? '…' : `${kind.glyph} ${kind.label}`}
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

          <div className={`map-wrapper-with-elevation${waypointMode ? ' placing-waypoint' : ''}`}>
            <MapContainer
              center={[currentTrack.coordinates[0]?.lat ?? 0, currentTrack.coordinates[0]?.lon ?? 0]}
              zoom={13}
              style={{ width: '100%', flex: 1 }}
              ref={map => { if (map) mapRef.current = map }}
            >
              <MapTiles baseLayerId={baseLayer} hillshade={hillshade} hillshadeOpacity={hillshadeOpacity} />
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
                  icon={waypointIcon(String(i + 1))}
                >
                  <Popup>
                    <strong>{w.name ?? `Waypoint ${i + 1}`}</strong>
                    {w.desc && <><br />{w.desc}</>}
                    {w.elevation !== undefined && <><br />{w.elevation.toFixed(0)} m</>}
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

            {renderTerrainControls(viewColorMode, false)}
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
