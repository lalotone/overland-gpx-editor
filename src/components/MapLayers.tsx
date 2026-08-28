import { useEffect, useState } from 'react'
import { maplibreGL } from '@maplibre/maplibre-gl-leaflet'
import { Pane, TileLayer, useMap } from 'react-leaflet'
import { BASE_LAYERS, HILLSHADE_LAYER, SERVICE_ATTRIBUTIONS, getBaseLayer } from '../lib/terrain'
import type { ColorMode, ThumbnailLayerDefinition } from '../lib/terrain'

let webGL2Available: boolean | undefined

function ServiceAttributions() {
  const map = useMap()

  useEffect(() => {
    SERVICE_ATTRIBUTIONS.forEach(credit => map.attributionControl.addAttribution(credit))
    return () => {
      SERVICE_ATTRIBUTIONS.forEach(credit => map.attributionControl.removeAttribution(credit))
    }
  }, [map])

  return null
}

function hasWebGL2(): boolean {
  if (webGL2Available !== undefined) return webGL2Available
  if (typeof document === 'undefined') return false

  try {
    const canvas = document.createElement('canvas')
    const context = canvas.getContext('webgl2')
    context?.getExtension('WEBGL_lose_context')?.loseContext()
    webGL2Available = context !== null
  } catch {
    webGL2Available = false
  }

  return webGL2Available
}

function VectorBaseLayer({
  styleUrl,
  attribution,
  fallback,
  onFallback,
}: {
  styleUrl: string
  attribution: string
  fallback: ThumbnailLayerDefinition
  onFallback?: () => void
}) {
  const map = useMap()
  const webGL2 = hasWebGL2()

  useEffect(() => {
    if (!webGL2) return

    const layer = maplibreGL({
      style: styleUrl,
      attributionControl: false,
    }).addTo(map)
    map.attributionControl.addAttribution(attribution)

    return () => {
      map.attributionControl.removeAttribution(attribution)
      if (map.hasLayer(layer)) map.removeLayer(layer)
    }
  }, [attribution, map, styleUrl, webGL2])

  useEffect(() => {
    if (!webGL2) onFallback?.()
  }, [onFallback, webGL2])

  if (webGL2) return null

  return (
    <TileLayer
      url={fallback.url}
      attribution={fallback.attribution}
      maxZoom={fallback.maxZoom}
      maxNativeZoom={fallback.maxZoom}
    />
  )
}

/**
 * Base tiles plus an optional hillshade relief overlay.
 *
 * The hillshade sits in its own pane above the tile pane (z-index 200) but
 * below the overlay pane (400), so relief shades the map without ever
 * covering the track.
 */
export function MapTiles({
  baseLayerId,
  hillshade,
  hillshadeOpacity,
  onVectorFallback,
}: {
  baseLayerId: string
  hillshade: boolean
  hillshadeOpacity: number
  onVectorFallback?: () => void
}) {
  const base = getBaseLayer(baseLayerId)

  return (
    <>
      <ServiceAttributions />
      {base.kind === 'vector' ? (
        <VectorBaseLayer
          key={base.id}
          styleUrl={base.styleUrl}
          attribution={base.attribution}
          fallback={base.fallback}
          onFallback={onVectorFallback}
        />
      ) : (
        <TileLayer
          key={base.id}
          url={base.url}
          attribution={base.attribution}
          maxZoom={base.maxZoom}
          maxNativeZoom={base.maxZoom}
        />
      )}
      {hillshade && (
        <Pane name="hillshade-pane" style={{ zIndex: 250 }}>
          <TileLayer
            url={HILLSHADE_LAYER.url}
            attribution={HILLSHADE_LAYER.attribution}
            maxZoom={19}
            maxNativeZoom={HILLSHADE_LAYER.maxZoom}
            opacity={hillshadeOpacity}
          />
        </Pane>
      )}
    </>
  )
}

/** Base-map picker, relief toggle and track colouring mode. */
export function TerrainControls({
  baseLayerId,
  onBaseLayer,
  hillshade,
  onHillshade,
  hillshadeOpacity,
  onHillshadeOpacity,
  colorMode,
  onColorMode,
  surfaceAvailable = false,
}: {
  baseLayerId: string
  onBaseLayer: (id: string) => void
  hillshade: boolean
  onHillshade: (on: boolean) => void
  hillshadeOpacity: number
  onHillshadeOpacity: (value: number) => void
  colorMode?: ColorMode
  onColorMode?: (mode: ColorMode) => void
  /** Surface data has been read for this track, so the mode is offerable. */
  surfaceAvailable?: boolean
}) {
  const [open, setOpen] = useState(false)
  const base = getBaseLayer(baseLayerId)
  const vectorFallback = base.kind === 'vector' && !hasWebGL2()

  // Collapsed by default: the expanded panel is useful but covers a corner of
  // the map, which matters when you are reading terrain under it.
  if (!open) {
    return (
      <button
        className="terrain-fab"
        onClick={() => setOpen(true)}
        title="Map layers and terrain"
        aria-label="Map layers and terrain"
      >
        <svg viewBox="0 0 24 24" width="15" height="15" fill="currentColor">
          <path d="M11.99 18.54l-7.37-5.73L3 14.07l9 7 9-7-1.63-1.27-7.38 5.74zM12 16l7.36-5.73L21 9l-9-7-9 7 1.63 1.27L12 16z" />
        </svg>
        <span className="terrain-fab-label">{vectorFallback ? 'OSM' : base.label}</span>
        {hillshade && <span className="terrain-fab-dot" title="Relief on" />}
      </button>
    )
  }

  return (
    <div className="terrain-controls">
      <div className="terrain-header">
        <span className="terrain-label">Terrain</span>
        <button
          className="terrain-close"
          onClick={() => setOpen(false)}
          title="Close"
          aria-label="Close terrain panel"
        >
          &times;
        </button>
      </div>

      <div className="terrain-row terrain-row--layers">
        {BASE_LAYERS.map(layer => (
          <button
            key={layer.id}
            className={`tile-btn${baseLayerId === layer.id ? ' active' : ''}`}
            onClick={() => onBaseLayer(layer.id)}
            title={layer.title}
          >
            {layer.label}
          </button>
        ))}
      </div>

      <div className="terrain-row">
        <label
          className="terrain-toggle"
          title="Shaded relief overlay — reveals ridges and gullies, especially over satellite imagery"
        >
          <input type="checkbox" checked={hillshade} onChange={e => onHillshade(e.target.checked)} />
          <span>Relief</span>
        </label>
        <input
          className="terrain-slider"
          type="range"
          min={0.15}
          max={1}
          step={0.05}
          value={hillshadeOpacity}
          disabled={!hillshade}
          onChange={e => onHillshadeOpacity(parseFloat(e.target.value))}
          title="Relief strength"
          aria-label="Relief strength"
        />
      </div>

      {base.hasContours && <div className="terrain-row terrain-note">Contour lines included</div>}
      {base.kind === 'vector' && (
        <div className="terrain-row terrain-note">
          {vectorFallback ? 'OSM raster fallback — WebGL2 unavailable' : 'OpenFreeMap vector'}
        </div>
      )}

      {colorMode && onColorMode && (
        <div className="terrain-row terrain-row--colormode">
          <span className="terrain-label">Track</span>
          <button
            className={`tile-btn${colorMode === 'slope' ? ' active' : ''}`}
            onClick={() => onColorMode('slope')}
            title="Colour the track by gradient"
          >
            Gradient
          </button>
          <button
            className={`tile-btn${colorMode === 'altitude' ? ' active' : ''}`}
            onClick={() => onColorMode('altitude')}
            title="Colour the track by altitude above sea level"
          >
            Altitude
          </button>
          {surfaceAvailable && (
            <button
              className={`tile-btn${colorMode === 'surface' ? ' active' : ''}`}
              onClick={() => onColorMode('surface')}
              title="Colour the track by ground surface — tarmac, gravel, dirt"
            >
              Surface
            </button>
          )}
        </div>
      )}
    </div>
  )
}
