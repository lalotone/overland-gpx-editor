import { useMemo } from 'react'
import {
  calculateElevationStats,
  cumulativeDistanceKm,
  smoothElevations,
} from '../../../src/lib/geo'
import { formatDuration } from '../../../src/lib/routing'
import type { Coordinate } from '../../../src/lib/types'

export function useTrackStats(coordinates: Coordinate[]) {
  return useMemo(() => {
    const cumKm = cumulativeDistanceKm(coordinates)
    const elevations = smoothElevations(
      coordinates.map((p) => p.elevation ?? null),
      cumKm,
    )
    return {
      cumKm,
      elevations,
      distance: cumKm[cumKm.length - 1] ?? 0,
      stats: calculateElevationStats(elevations),
      hasElevation: elevations.some((e) => e !== null),
      partial: elevations.some((e) => e === null),
    }
  }, [coordinates])
}

export default function TrackSummary({
  coordinates,
  duration,
  chart = true,
}: {
  coordinates: Coordinate[]
  duration?: number | null
  chart?: boolean
}) {
  const { distance, stats, hasElevation, partial, elevations, cumKm } = useTrackStats(coordinates)
  // Distance is the x axis; source gaps break the line instead of inventing data.
  let path = '',
    drawing = false,
    lastX = -1
  elevations.forEach((e, i) => {
    if (e === null) {
      drawing = false
      return
    }
    const x = distance > 0 ? (cumKm[i] / distance) * 320 : 0
    if (drawing && x - lastX < 1 && i !== elevations.length - 1) return
    const y = 62 - ((e - stats.min) / Math.max(1, stats.max - stats.min)) * 52
    path += `${drawing ? 'L' : 'M'}${x.toFixed(1)},${y.toFixed(1)} `
    drawing = true
    lastX = x
  })
  return (
    <>
      <div className="stats">
        <div>
          <strong>
            {distance.toFixed(1)}
            <small> km</small>
          </strong>
          <span>Distance</span>
        </div>
        <div>
          <strong>
            {hasElevation ? Math.round(stats.gain).toLocaleString() : '—'}
            <small> m</small>
          </strong>
          <span>Ascent{partial && hasElevation ? ' · partial' : ''}</span>
        </div>
        <div>
          <strong>
            {duration != null
              ? formatDuration(duration)
              : hasElevation
                ? `${Math.round(stats.max).toLocaleString()}`
                : '—'}
            <small>{duration == null && hasElevation ? ' m' : ''}</small>
          </strong>
          <span>{duration != null ? 'Moving time' : 'Highest point'}</span>
        </div>
      </div>
      {chart && hasElevation && (
        <div className="profile-chart">
          <svg viewBox="0 0 320 76" role="img" aria-label="Elevation profile">
            <path d="M0 66H320" stroke="#d9dfd5" />
            <path d={path} fill="none" stroke="#65856b" strokeWidth="2" />
          </svg>
          <div>
            <span>{Math.round(stats.min)} m</span>
            <span>{distance.toFixed(1)} km</span>
          </div>
        </div>
      )}
    </>
  )
}
