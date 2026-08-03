import { useState } from 'react'
import { Pane, TileLayer } from 'react-leaflet'
import { BASE_LAYERS, HILLSHADE_LAYER, getBaseLayer } from '../lib/terrain'
import type { ColorMode } from '../lib/terrain'

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
}: {
  baseLayerId: string
  hillshade: boolean
  hillshadeOpacity: number
}) {
  const base = getBaseLayer(baseLayerId)

  return (
    <>
      <TileLayer
        key={base.id}
        url={base.url}
        attribution={base.attribution}
        maxZoom={base.maxZoom}
        maxNativeZoom={base.maxZoom}
      />
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
        <span className="terrain-fab-label">{base.label}</span>
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
