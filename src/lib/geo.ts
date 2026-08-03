/* Geospatial and elevation maths. Pure functions — no DOM, no network. */

import type { Coordinate, ElevationStats, TimeStats } from './types'

const EARTH_RADIUS_M = 6371008.8
const RAD = Math.PI / 180

/**
 * Elevation changes smaller than this are treated as noise and not counted
 * towards gain/loss. Barometric drift and DEM quantisation otherwise inflate
 * the totals by 50-100% on a densely recorded track.
 */
export const ELEVATION_NOISE_FLOOR_M = 3

/** Distance window used to smooth elevation before it is charted or summed. */
export const ELEVATION_SMOOTHING_WINDOW_M = 60

export function haversineDistance(a: Coordinate, b: Coordinate): number {
  const dLat = (b.lat - a.lat) * RAD
  const dLon = (b.lon - a.lon) * RAD
  const h =
    Math.sin(dLat / 2) ** 2 +
    Math.cos(a.lat * RAD) * Math.cos(b.lat * RAD) * Math.sin(dLon / 2) ** 2
  return EARTH_RADIUS_M * 2 * Math.atan2(Math.sqrt(h), Math.sqrt(1 - h))
}

/** Cumulative along-path distance in km, one entry per coordinate. */
export function cumulativeDistanceKm(coords: Coordinate[]): number[] {
  const out = new Array<number>(coords.length)
  let total = 0
  for (let i = 0; i < coords.length; i++) {
    if (i > 0) total += haversineDistance(coords[i - 1], coords[i]) / 1000
    out[i] = total
  }
  return out
}

/** Total path length in km. */
export function calculateDistance(coords: Coordinate[]): number {
  if (coords.length < 2) return 0
  let total = 0
  for (let i = 1; i < coords.length; i++) total += haversineDistance(coords[i - 1], coords[i])
  return total / 1000
}

/**
 * Moving average of elevation over a fixed *distance* window rather than a
 * fixed number of samples, so the result does not depend on how densely the
 * source device recorded. Nulls are preserved.
 */
export function smoothElevations(
  elevations: (number | null)[],
  cumKm: number[],
  windowM = ELEVATION_SMOOTHING_WINDOW_M,
): (number | null)[] {
  const halfKm = windowM / 2000
  const out: (number | null)[] = new Array(elevations.length).fill(null)
  let lo = 0
  let hi = 0
  let sum = 0
  let count = 0

  for (let i = 0; i < elevations.length; i++) {
    while (hi < elevations.length && cumKm[hi] <= cumKm[i] + halfKm) {
      const e = elevations[hi]
      if (e !== null && e !== undefined && Number.isFinite(e)) { sum += e; count++ }
      hi++
    }
    while (lo < hi && cumKm[lo] < cumKm[i] - halfKm) {
      const e = elevations[lo]
      if (e !== null && e !== undefined && Number.isFinite(e)) { sum -= e; count-- }
      lo++
    }
    const own = elevations[i]
    if (own === null || own === undefined || !Number.isFinite(own)) continue
    out[i] = count > 0 ? sum / count : own
  }
  return out
}

/**
 * Min/max/gain/loss with a noise floor, using direction hysteresis.
 *
 * Only a *reversal* has to clear `noiseFloorM`; once a direction is
 * established, further movement the same way is counted immediately. That
 * keeps jitter around a level from accumulating while still counting a long
 * steady climb in full — a plain threshold would silently discard whatever
 * part of the climb sits below the floor at the end.
 */
export function calculateElevationStats(
  elevations: (number | null)[],
  noiseFloorM = ELEVATION_NOISE_FLOOR_M,
): ElevationStats {
  let min = Infinity
  let max = -Infinity
  let gain = 0
  let loss = 0
  let ref: number | null = null
  let direction: 0 | 1 | -1 = 0

  for (const e of elevations) {
    if (e === null || e === undefined || !Number.isFinite(e)) continue
    if (e < min) min = e
    if (e > max) max = e
    if (ref === null) { ref = e; continue }

    const d = e - ref
    if (direction > 0) {
      if (d > 0) { gain += d; ref = e }
      else if (d <= -noiseFloorM) { loss -= d; ref = e; direction = -1 }
    } else if (direction < 0) {
      if (d < 0) { loss -= d; ref = e }
      else if (d >= noiseFloorM) { gain += d; ref = e; direction = 1 }
    } else if (d >= noiseFloorM) {
      gain += d; ref = e; direction = 1
    } else if (d <= -noiseFloorM) {
      loss -= d; ref = e; direction = -1
    }
  }

  if (min === Infinity) return { min: 0, max: 0, gain: 0, loss: 0 }
  return { min, max, gain, loss }
}

export function slopePercent(riseM: number, runM: number): number {
  if (!Number.isFinite(runM) || runM <= 0) return 0
  return (riseM / runM) * 100
}

/**
 * Speed and moving-time stats from recorded timestamps. Returns null when the
 * track has no usable time data (a planned route, for instance).
 */
export function calculateTimeStats(coords: Coordinate[]): TimeStats | null {
  const stamped = coords
    .map(c => ({ c, t: c.time ? Date.parse(c.time) : NaN }))
    .filter(p => Number.isFinite(p.t))
  if (stamped.length < 2) return null

  const MOVING_THRESHOLD_KMH = 1
  const totalSeconds = (stamped[stamped.length - 1].t - stamped[0].t) / 1000
  let movingSeconds = 0
  let movingMetres = 0
  let totalMetres = 0
  let maxSpeedKmh = 0

  for (let i = 1; i < stamped.length; i++) {
    const dt = (stamped[i].t - stamped[i - 1].t) / 1000
    const dm = haversineDistance(stamped[i - 1].c, stamped[i].c)
    totalMetres += dm
    if (dt <= 0) continue
    const kmh = (dm / dt) * 3.6
    // Reject physically implausible jumps from GPS glitches.
    if (kmh > 0 && kmh < 400 && kmh > maxSpeedKmh) maxSpeedKmh = kmh
    if (kmh >= MOVING_THRESHOLD_KMH) {
      movingSeconds += dt
      movingMetres += dm
    }
  }

  return {
    totalSeconds,
    movingSeconds,
    avgSpeedKmh: totalSeconds > 0 ? (totalMetres / totalSeconds) * 3.6 : 0,
    movingSpeedKmh: movingSeconds > 0 ? (movingMetres / movingSeconds) * 3.6 : 0,
    maxSpeedKmh,
  }
}

/** Longest stretch, in km, containing none of the supplied points of interest. */
export function longestGapKm(
  coords: Coordinate[],
  cumKm: number[],
  pois: { lat: number; lon: number }[],
  corridorM = 2000,
): { gapKm: number; atKm: number } {
  if (coords.length === 0) return { gapKm: 0, atKm: 0 }
  const hits: number[] = [0]
  for (let i = 0; i < coords.length; i++) {
    for (const p of pois) {
      if (haversineDistance(coords[i], p as Coordinate) <= corridorM) { hits.push(cumKm[i]); break }
    }
  }
  hits.push(cumKm[cumKm.length - 1])
  hits.sort((a, b) => a - b)

  let gapKm = 0
  let atKm = 0
  for (let i = 1; i < hits.length; i++) {
    const g = hits[i] - hits[i - 1]
    if (g > gapKm) { gapKm = g; atKm = hits[i - 1] }
  }
  return { gapKm, atKm }
}
