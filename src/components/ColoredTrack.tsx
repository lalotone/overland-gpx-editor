import { memo, useMemo } from 'react'
import { Polyline } from 'react-leaflet'
import { slopePercent } from '../lib/geo'
import { altitudeColor, getSegmentColor } from '../lib/terrain'
import type { ColorMode } from '../lib/terrain'
import { surfaceColor } from '../lib/surface'
import type { SurfaceClass } from '../lib/surface'
import type { Coordinate } from '../lib/types'

/**
 * Distance over which gradient is measured for colouring. Taking the slope
 * between two consecutive points of a dense recording divides by a run of a
 * couple of metres, which makes the percentage meaningless.
 */
const SLOPE_WINDOW_KM = 0.04

/** Altitude bands. Enough to read the terrain, few enough to batch well. */
const ALTITUDE_BANDS = 24

const NO_DATA_COLOR = '#94a3b8'

interface Run {
  color: string
  positions: [number, number][]
}

/**
 * The full-resolution track, drawn as one polyline per run of same-coloured
 * segments.
 *
 * Colouring per segment would mean one Leaflet layer per point — unusable at
 * 10k points — and drawing a decimated line instead (what this used to do)
 * cuts every switchback off a twisty trail. Run-length grouping keeps the
 * exact geometry while collapsing to a few hundred layers.
 */
function ColoredTrackImpl({
  coordinates,
  elevations,
  cumKm,
  colorMode,
  elevMin,
  elevMax,
  surfaces,
  weight = 4,
}: {
  coordinates: Coordinate[]
  elevations: (number | null)[]
  cumKm: number[]
  colorMode: ColorMode
  elevMin: number
  elevMax: number
  /** Per-segment surface classes; required for `colorMode: 'surface'`. */
  surfaces?: SurfaceClass[]
  weight?: number
}) {
  const runs = useMemo<Run[]>(() => {
    const out: Run[] = []
    if (coordinates.length < 2) return out

    // Surface mode without data would paint the whole track "no data" grey,
    // which reads as a broken track rather than a missing overlay.
    const mode: ColorMode = colorMode === 'surface' && !surfaces ? 'slope' : colorMode

    const colorAt = (i: number): string => {
      // Segment i runs from point i-1 to point i.
      if (mode === 'surface') return surfaceColor(surfaces![i - 1] ?? 'unknown')

      const e = elevations[i]
      if (e === null || e === undefined) return NO_DATA_COLOR

      if (mode === 'altitude') {
        const range = elevMax - elevMin
        const band = range > 0
          ? Math.min(ALTITUDE_BANDS - 1, Math.floor(((e - elevMin) / range) * ALTITUDE_BANDS))
          : 0
        const bandCenter = elevMin + ((band + 0.5) / ALTITUDE_BANDS) * range
        return altitudeColor(bandCenter, elevMin, elevMax)
      }

      // Walk back far enough that the run is a meaningful distance.
      let j = i - 1
      while (j > 0 && cumKm[i] - cumKm[j] < SLOPE_WINDOW_KM) j--
      const prev = elevations[j]
      if (prev === null || prev === undefined) return NO_DATA_COLOR
      return getSegmentColor(slopePercent(e - prev, (cumKm[i] - cumKm[j]) * 1000))
    }

    let current: Run | null = null
    for (let i = 1; i < coordinates.length; i++) {
      const color = colorAt(i)
      if (!current || current.color !== color) {
        if (current) out.push(current)
        // Start the new run at the previous point so runs stay connected.
        current = { color, positions: [[coordinates[i - 1].lat, coordinates[i - 1].lon]] }
      }
      current.positions.push([coordinates[i].lat, coordinates[i].lon])
    }
    if (current) out.push(current)
    return out
  }, [coordinates, elevations, cumKm, colorMode, elevMin, elevMax, surfaces])

  return (
    <>
      {runs.map((run, i) => (
        <Polyline
          key={i}
          positions={run.positions}
          color={run.color}
          weight={weight}
          // Round joins stop hairline gaps showing where runs meet.
          lineCap="round"
          lineJoin="round"
        />
      ))}
    </>
  )
}

/**
 * Memoised: a long track expands to hundreds of polylines, and unrelated
 * state changes (cursor position, hover) would otherwise re-render them all.
 */
export const ColoredTrack = memo(ColoredTrackImpl)
