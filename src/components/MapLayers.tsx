import { useCallback, useEffect, useRef, useState } from 'react'
import { maplibreGL } from '@maplibre/maplibre-gl-leaflet'
import type { StyleSpecification } from 'maplibre-gl'
import { Pane, TileLayer, useMap } from 'react-leaflet'
import { resolveMapStyleResources } from '../lib/mapStyle'
import { SERVICE_ATTRIBUTIONS, getBaseLayerFrom } from '../lib/terrain'
import type { BaseLayerDefinition, ColorMode, ThumbnailLayerDefinition } from '../lib/terrain'
import { ESCAPE_PRIORITY, useEscapeDismiss } from './useEscapeDismiss'

let webGL2Available: boolean | undefined

export type VectorMapPhase = 'webgl' | 'style' | 'initialization' | 'source' | 'tile' | 'glyph' | 'sprite'

export interface VectorMapIssue {
  phase: VectorMapPhase
  message: string
  status?: number
  source?: string
  resource?: string
  code?: string
}

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

function safeText(value: unknown): string | undefined {
  if (typeof value !== 'string') return undefined
  const cleaned = value.replace(/[\u0000-\u001f\u007f]+/g, ' ').replace(/\s+/g, ' ').trim()
  return cleaned ? cleaned.slice(0, 240) : undefined
}

function safeResource(value: unknown): string | undefined {
  if (typeof value !== 'string' || !value) return undefined
  try {
    const url = new URL(value, window.location.href)
    return url.origin === window.location.origin ? url.pathname : `${url.host}${url.pathname}`
  } catch {
    return undefined
  }
}

function phaseFrom(value: unknown, fallback: VectorMapPhase): VectorMapPhase {
  return value === 'style' || value === 'source' || value === 'tile' || value === 'glyph' || value === 'sprite'
    ? value
    : fallback
}

async function styleResponseIssue(response: Response, styleUrl: string): Promise<VectorMapIssue> {
  const payload = await response.clone().json().catch(() => null) as Record<string, unknown> | null
  const phase = phaseFrom(payload?.stage, 'style')
  const upstreamStatus = typeof payload?.upstreamStatus === 'number' ? payload.upstreamStatus : response.status
  const detail = safeText(payload?.detail)
  return {
    phase,
    status: upstreamStatus,
    code: safeText(payload?.code),
    resource: safeResource(styleUrl),
    message: detail
      ? `${detail}${detail.includes(`HTTP ${upstreamStatus}`) ? '' : ` (HTTP ${upstreamStatus})`}`
      : `OpenFreeMap ${phase} failed with HTTP ${upstreamStatus}`,
  }
}

function mapLibreIssue(event: unknown): VectorMapIssue {
  const raw = typeof event === 'object' && event !== null ? event as Record<string, unknown> : {}
  const error = typeof raw.error === 'object' && raw.error !== null ? raw.error as Record<string, unknown> : {}
  const status = typeof error.status === 'number' ? error.status : undefined
  const source = safeText(raw.sourceId)
  const resource = safeResource(error.url)
  const rawMessage = safeText(error.message)
  const lowerMessage = rawMessage?.toLowerCase() ?? ''
  const phase: VectorMapPhase = raw.tile
    ? 'tile'
    : resource?.includes('/glyph') || resource?.includes('/font')
      ? 'glyph'
      : resource?.includes('/sprite')
        ? 'sprite'
        : lowerMessage.includes('glyph') || lowerMessage.includes('font')
          ? 'glyph'
          : lowerMessage.includes('sprite')
            ? 'sprite'
        : source
          ? 'source'
          : 'style'
  const label = source ? `OpenFreeMap ${phase} "${source}"` : `OpenFreeMap ${phase}`
  const cause = status ? `HTTP ${status}` : rawMessage?.replace(/https?:\/\/\S+/g, 'remote resource') ?? 'unknown error'
  return { phase, status, source, resource, message: `${label} failed with ${cause}` }
}

function VectorBaseLayer({
  styleUrl,
  attribution,
  onStatus,
}: {
  styleUrl: string
  attribution: string
  onStatus?: (issue: VectorMapIssue | null) => void
}) {
  const map = useMap()
  const webGL2 = hasWebGL2()
  const [style, setStyle] = useState<StyleSpecification | null>(null)
  const lastLoggedRef = useRef('')

  const reportIssue = useCallback((issue: VectorMapIssue | null) => {
    if (!issue) {
      lastLoggedRef.current = ''
      onStatus?.(null)
      return
    }
    const fingerprint = `${issue.phase}:${issue.status ?? ''}:${issue.source ?? ''}:${issue.message}`
    if (lastLoggedRef.current === fingerprint) return
    lastLoggedRef.current = fingerprint
    onStatus?.(issue)
    console.error(`[vector-map] ${issue.message}${issue.resource ? ` (${issue.resource})` : ''}`)
  }, [onStatus])

  useEffect(() => {
    const controller = new AbortController()
    setStyle(null)
    reportIssue(null)
    if (!webGL2) return () => controller.abort()
    void (async () => {
      try {
        const response = await fetch(styleUrl, { signal: controller.signal })
        if (!response.ok) {
          reportIssue(await styleResponseIssue(response, styleUrl))
          return
        }
        const value = await response.json() as StyleSpecification
        if (value.version !== 8 || !value.sources || !Array.isArray(value.layers)) {
          reportIssue({
            phase: 'style',
            message: 'OpenFreeMap returned an invalid style document',
            resource: safeResource(styleUrl),
            code: 'invalid_style',
          })
          return
        }
        const absoluteStyleUrl = new URL(styleUrl, window.location.href).toString()
        setStyle(resolveMapStyleResources(value, absoluteStyleUrl))
      } catch (reason) {
        if ((reason as Error).name === 'AbortError') return
        reportIssue({
          phase: 'style',
          message: `OpenFreeMap style request failed: ${safeText((reason as Error).message) ?? 'network error'}`,
          resource: safeResource(styleUrl),
          code: 'style_request_failed',
        })
      }
    })()
    return () => controller.abort()
  }, [reportIssue, styleUrl, webGL2])

  useEffect(() => {
    if (!webGL2 || !style) return

    let layer: ReturnType<typeof maplibreGL>
    try {
      layer = maplibreGL({ style, attributionControl: false }).addTo(map)
    } catch (reason) {
      reportIssue({
        phase: 'initialization',
        message: `OpenFreeMap could not start: ${safeText((reason as Error).message) ?? 'MapLibre initialization failed'}`,
        code: 'map_initialization_failed',
      })
      return
    }
    const libreMap = layer.getMaplibreMap()
    const reportResourceFailure = (event: unknown) => reportIssue(mapLibreIssue(event))
    libreMap.on('error', reportResourceFailure)
    map.attributionControl.addAttribution(attribution)

    return () => {
      libreMap.off('error', reportResourceFailure)
      map.attributionControl.removeAttribution(attribution)
      if (map.hasLayer(layer)) map.removeLayer(layer)
    }
  }, [attribution, map, reportIssue, style, webGL2])

  useEffect(() => {
    if (!webGL2) {
      reportIssue({
        phase: 'webgl',
        message: 'OpenFreeMap requires WebGL2, which is unavailable in this browser',
        code: 'webgl2_unavailable',
      })
    }
  }, [reportIssue, webGL2])

  return null
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
  onVectorStatus,
  layers,
  hillshadeLayer,
}: {
  baseLayerId: string
  hillshade: boolean
  hillshadeOpacity: number
  onVectorStatus?: (issue: VectorMapIssue | null) => void
  layers: BaseLayerDefinition[]
  hillshadeLayer?: ThumbnailLayerDefinition
}) {
  const base = getBaseLayerFrom(layers, baseLayerId)

  return (
    <>
      <ServiceAttributions />
      {base.kind === 'vector' ? (
        <VectorBaseLayer
          key={base.id}
          styleUrl={base.styleUrl}
          attribution={base.attribution}
          onStatus={onVectorStatus}
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
      {hillshade && hillshadeLayer && (
        <Pane name="hillshade-pane" style={{ zIndex: 250 }}>
          <TileLayer
            url={hillshadeLayer.url}
            attribution={hillshadeLayer.attribution}
            maxZoom={19}
            maxNativeZoom={hillshadeLayer.maxZoom}
            opacity={hillshadeOpacity}
          />
        </Pane>
      )}
    </>
  )
}

export function VectorMapDiagnostic({
  issue,
  onDismiss,
}: {
  issue: VectorMapIssue | null
  onDismiss?: () => void
}) {
  useEscapeDismiss(Boolean(issue && onDismiss), () => onDismiss?.(), ESCAPE_PRIORITY.passive)
  if (!issue) return null
  const degraded = issue.phase === 'source' || issue.phase === 'tile' || issue.phase === 'glyph' || issue.phase === 'sprite'
  return (
    <div className="vector-map-diagnostic" role="alert" data-testid="vector-map-diagnostic">
      {onDismiss && <button type="button" onClick={onDismiss} aria-label="Dismiss vector map warning">&times;</button>}
      <strong>{degraded ? 'OpenFreeMap is incomplete' : 'OpenFreeMap could not load'}</strong>
      <span>{issue.message}</span>
      {issue.resource && <code>{issue.resource}</code>}
      <small>Vector remains selected. Choose another map layer manually.</small>
    </div>
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
  layers,
  hillshadeAvailable = true,
  vectorIssue = null,
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
  layers: BaseLayerDefinition[]
  hillshadeAvailable?: boolean
  vectorIssue?: VectorMapIssue | null
}) {
  const [open, setOpen] = useState(false)
  const base = getBaseLayerFrom(layers, baseLayerId)
  const activeVectorIssue = base.kind === 'vector' ? vectorIssue : null
  useEscapeDismiss(open, () => setOpen(false), ESCAPE_PRIORITY.panel)

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
        {hillshade && hillshadeAvailable && <span className="terrain-fab-dot" title="Relief on" />}
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
        {layers.map(layer => (
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
          <input
            type="checkbox"
            checked={hillshade && hillshadeAvailable}
            disabled={!hillshadeAvailable}
            onChange={e => onHillshade(e.target.checked)}
          />
          <span>Relief</span>
        </label>
        <input
          className="terrain-slider"
          type="range"
          min={0.15}
          max={1}
          step={0.05}
          value={hillshadeOpacity}
          disabled={!hillshade || !hillshadeAvailable}
          onChange={e => onHillshadeOpacity(parseFloat(e.target.value))}
          title="Relief strength"
          aria-label="Relief strength"
        />
      </div>

      {!hillshadeAvailable && <div className="terrain-row terrain-note">Relief is not cached</div>}

      {base.hasContours && <div className="terrain-row terrain-note">Contour lines included</div>}
      {base.kind === 'vector' && (
        <div className="terrain-row terrain-note">
          {activeVectorIssue ? activeVectorIssue.message : 'OpenFreeMap vector'}
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
