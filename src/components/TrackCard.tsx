import { useMemo } from 'react'
import { ALTITUDE_GRADIENT_CSS_VERTICAL } from '../lib/terrain'
import type { Coordinate, ElevationStats } from '../lib/types'

export interface TrackPreview {
  name: string
  filename: string
  distance: number
  elevStats: ElevationStats
  hasTime: boolean
  /** Decimated geometry, enough for a thumbnail. */
  shape: Coordinate[]
  /** Decimated elevation series for the sparkline. */
  profile: number[]
}

const THUMB_W = 220
const THUMB_H = 84
const PAD = 8

const TILE_SIZE = 256
const TILE_SUBDOMAINS = ['a', 'b', 'c']

interface ThumbTile {
  key: string
  url: string
  /** Position in thumbnail units — the same space as the SVG viewBox. */
  left: number
  top: number
}

interface ThumbLayout {
  path: string
  tiles: ThumbTile[]
}

/**
 * Web Mercator, in the pixel space the tile servers use: the whole world is
 * TILE_SIZE * 2^zoom pixels square. Returned at zoom 0, so callers scale by
 * 2^zoom rather than re-projecting.
 */
function projectZ0(lat: number, lon: number): { x: number; y: number } {
  // Clamped short of the poles, where the Mercator formula diverges.
  const s = Math.min(Math.max(Math.sin((lat * Math.PI) / 180), -0.9999), 0.9999)
  return {
    x: ((lon + 180) / 360) * TILE_SIZE,
    y: (0.5 - Math.log((1 + s) / (1 - s)) / (4 * Math.PI)) * TILE_SIZE,
  }
}

/**
 * Fit the track to the thumbnail box and work out which map tiles sit behind
 * it.
 *
 * Route and tiles are projected the same way, so the line lands on the roads
 * and valleys it actually follows — an approximate projection for the line
 * alone would drift visibly against the tiles at these zooms.
 */
function thumbLayout(coords: Coordinate[], tileUrl: string | null, maxZoom: number): ThumbLayout {
  if (coords.length < 2) return { path: '', tiles: [] }

  const points = coords.map(c => projectZ0(c.lat, c.lon))
  const minX = Math.min(...points.map(p => p.x))
  const maxX = Math.max(...points.map(p => p.x))
  const minY = Math.min(...points.map(p => p.y))
  const maxY = Math.max(...points.map(p => p.y))

  // Largest zoom at which the whole route still fits inside the padded box.
  const spanX = maxX - minX
  const spanY = maxY - minY
  const fitX = spanX > 1e-9 ? (THUMB_W - PAD * 2) / spanX : Infinity
  const fitY = spanY > 1e-9 ? (THUMB_H - PAD * 2) / spanY : Infinity
  const fit = Math.min(fitX, fitY)
  const zoom = Number.isFinite(fit)
    ? Math.max(0, Math.min(maxZoom, Math.floor(Math.log2(fit))))
    : maxZoom

  const scale = 2 ** zoom
  const originX = ((minX + maxX) / 2) * scale - THUMB_W / 2
  const originY = ((minY + maxY) / 2) * scale - THUMB_H / 2

  const path =
    'M' +
    points
      .map(p => `${(p.x * scale - originX).toFixed(1)},${(p.y * scale - originY).toFixed(1)}`)
      .join('L')

  const tiles: ThumbTile[] = []
  if (tileUrl) {
    const worldTiles = 2 ** zoom
    const firstX = Math.floor(originX / TILE_SIZE)
    const lastX = Math.floor((originX + THUMB_W) / TILE_SIZE)
    const firstY = Math.floor(originY / TILE_SIZE)
    const lastY = Math.floor((originY + THUMB_H) / TILE_SIZE)

    for (let ty = firstY; ty <= lastY; ty++) {
      // No tiles exist above the north edge or below the south edge.
      if (ty < 0 || ty >= worldTiles) continue
      for (let tx = firstX; tx <= lastX; tx++) {
        // X wraps at the antimeridian.
        const wrappedX = ((tx % worldTiles) + worldTiles) % worldTiles
        tiles.push({
          key: `${tx}/${ty}`,
          url: tileUrl
            .replace('{s}', TILE_SUBDOMAINS[(wrappedX + ty) % TILE_SUBDOMAINS.length])
            .replace('{z}', String(zoom))
            .replace('{x}', String(wrappedX))
            .replace('{y}', String(ty)),
          left: tx * TILE_SIZE - originX,
          top: ty * TILE_SIZE - originY,
        })
      }
    }
  }

  return { path, tiles }
}

function sparkPath(values: number[], width: number, height: number): string {
  if (values.length < 2) return ''
  const min = Math.min(...values)
  const max = Math.max(...values)
  const range = max - min || 1
  const step = width / (values.length - 1)
  const line = values
    .map((v, i) => `${(i * step).toFixed(1)},${(height - ((v - min) / range) * height).toFixed(1)}`)
    .join('L')
  return `M0,${height}L${line}L${width},${height}Z`
}

/** As a percentage of the thumbnail box, so tiles track the SVG at any size. */
const pct = (value: number, extent: number) => `${((value / extent) * 100).toFixed(4)}%`

export function TrackCard({
  track,
  tileUrl,
  tileMaxZoom,
  onOpen,
  onDelete,
}: {
  track: TrackPreview
  /** Base-map template for the thumbnail; null draws the plain backdrop. */
  tileUrl?: string | null
  tileMaxZoom?: number
  onOpen: () => void
  onDelete: () => void
}) {
  const { path, tiles } = useMemo(
    () => thumbLayout(track.shape, tileUrl ?? null, tileMaxZoom ?? 16),
    [track.shape, tileUrl, tileMaxZoom],
  )
  const spark = useMemo(() => sparkPath(track.profile, 100, 20), [track.profile])
  const hasProfile = track.profile.length > 1

  return (
    <li className="track-card">
      <button className="track-card-open" onClick={onOpen} title={`Open ${track.filename}`}>
        <span className="track-card-thumb">
          {tiles.length > 0 && (
            <span className="track-card-tiles" aria-hidden="true">
              {tiles.map(tile => (
                <img
                  key={tile.key}
                  src={tile.url}
                  alt=""
                  loading="lazy"
                  decoding="async"
                  className="track-card-tile"
                  style={{
                    left: pct(tile.left, THUMB_W),
                    top: pct(tile.top, THUMB_H),
                    width: pct(TILE_SIZE, THUMB_W),
                    height: pct(TILE_SIZE, THUMB_H),
                  }}
                  // Fade in per tile: they arrive at different times, and
                  // popping in at full opacity reads as flicker.
                  onLoad={e => { e.currentTarget.style.opacity = '1' }}
                  onError={e => { e.currentTarget.style.visibility = 'hidden' }}
                />
              ))}
            </span>
          )}

          <svg viewBox={`0 0 ${THUMB_W} ${THUMB_H}`} preserveAspectRatio="xMidYMid meet" aria-hidden="true">
            <path className="track-card-route-shadow" d={path} />
            <path className="track-card-route" d={path} />
          </svg>
          {track.hasTime && <span className="track-card-badge" title="Recorded track with timestamps">REC</span>}
        </span>

        <span className="track-card-body">
          <span className="track-card-name">{track.name}</span>
          <span className="track-card-stats">
            <span className="track-card-stat">
              <strong>{track.distance.toFixed(1)}</strong> km
            </span>
            <span className="track-card-stat track-card-gain">
              <strong>+{track.elevStats.gain.toFixed(0)}</strong> m
            </span>
            <span className="track-card-stat track-card-loss">
              <strong>-{track.elevStats.loss.toFixed(0)}</strong> m
            </span>
          </span>

          {hasProfile && (
            <span className="track-card-spark">
              <svg viewBox="0 0 100 20" preserveAspectRatio="none" aria-hidden="true">
                <path d={spark} />
              </svg>
              <span
                className="track-card-spark-scale"
                style={{ background: ALTITUDE_GRADIENT_CSS_VERTICAL }}
                title={`${track.elevStats.min.toFixed(0)} m to ${track.elevStats.max.toFixed(0)} m`}
              />
            </span>
          )}
        </span>
      </button>

      <button className="track-card-delete" onClick={onDelete} title={`Delete ${track.filename}`}>
        <svg viewBox="0 0 24 24" width="14" height="14" fill="currentColor">
          <path d="M6 19c0 1.1.9 2 2 2h8c1.1 0 2-.9 2-2V7H6v12zM19 4h-3.5l-1-1h-5l-1 1H5v2h14V4z" />
        </svg>
      </button>
    </li>
  )
}
