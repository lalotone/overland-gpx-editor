/*
 * Non-destructive track editing. Every function returns a new Track.
 *
 * These cover the operations that actually come up when adapting somebody
 * else's GPX for a ride: cutting the boring approach off the front, splitting
 * a long route into day stages, and thinning a dense recording down to
 * something a GPS unit will accept.
 */

import { cumulativeDistanceKm, haversineDistance, smoothElevations } from './geo'
import type { Coordinate, GpxWaypoint, Track } from './types'

const EARTH_RADIUS_M = 6371008.8
const RAD = Math.PI / 180

function rebuild(track: Track, coordinates: Coordinate[], nameSuffix?: string): Track {
  return {
    ...track,
    name: nameSuffix ? `${track.name} ${nameSuffix}` : track.name,
    coordinates,
    elevations: coordinates.map(c => (c.elevation === undefined ? null : c.elevation)),
  }
}

/** Keep only waypoints within `corridorM` of the retained geometry. */
function waypointsNear(waypoints: GpxWaypoint[], coords: Coordinate[], corridorM = 1500): GpxWaypoint[] {
  if (coords.length === 0) return []
  return waypoints.filter(w =>
    coords.some(c => haversineDistance(c, { lat: w.lat, lon: w.lon }) <= corridorM),
  )
}

export function reverseTrack(track: Track): Track {
  return rebuild(track, [...track.coordinates].reverse())
}

/** Keep points `startIdx..endIdx` inclusive. */
export function trimTrack(track: Track, startIdx: number, endIdx: number): Track {
  const lo = Math.max(0, Math.min(startIdx, endIdx))
  const hi = Math.min(track.coordinates.length - 1, Math.max(startIdx, endIdx))
  const coords = track.coordinates.slice(lo, hi + 1)
  return { ...rebuild(track, coords), waypoints: waypointsNear(track.waypoints, coords) }
}

/** Split into two tracks at `idx`; the point itself ends one and starts the other. */
export function splitTrack(track: Track, idx: number): [Track, Track] {
  const cut = Math.max(1, Math.min(track.coordinates.length - 2, idx))
  const first = track.coordinates.slice(0, cut + 1)
  const second = track.coordinates.slice(cut)
  return [
    { ...rebuild(track, first, '(1)'), waypoints: waypointsNear(track.waypoints, first) },
    { ...rebuild(track, second, '(2)'), waypoints: waypointsNear(track.waypoints, second) },
  ]
}

/** Split into roughly equal-distance stages — one per riding day. */
export function splitIntoStages(track: Track, stageKm: number): Track[] {
  const cum = cumulativeDistanceKm(track.coordinates)
  const total = cum[cum.length - 1] ?? 0
  if (stageKm <= 0 || total <= stageKm) return [track]

  const stages: Track[] = []
  let start = 0
  let target = stageKm
  for (let i = 1; i < cum.length; i++) {
    if (cum[i] >= target || i === cum.length - 1) {
      const coords = track.coordinates.slice(start, i + 1)
      stages.push({
        ...rebuild(track, coords, `— day ${stages.length + 1}`),
        waypoints: waypointsNear(track.waypoints, coords),
      })
      start = i
      target = cum[i] + stageKm
    }
  }
  return stages
}

export function joinTracks(a: Track, b: Track): Track {
  return {
    ...a,
    name: `${a.name} + ${b.name}`,
    coordinates: [...a.coordinates, ...b.coordinates],
    elevations: [...a.elevations, ...b.elevations],
    waypoints: [...a.waypoints, ...b.waypoints],
  }
}

/* -- Simplification --------------------------------------------------- */

/**
 * Ramer-Douglas-Peucker, iterative so a 20k-point recording cannot blow the
 * call stack. Distances are metres via a local equirectangular projection,
 * which is accurate well past the size of any single track.
 */
export function simplifyCoordinates(coords: Coordinate[], toleranceM: number): Coordinate[] {
  if (coords.length < 3 || toleranceM <= 0) return coords

  const lat0 = coords[Math.floor(coords.length / 2)].lat * RAD
  const cosLat0 = Math.cos(lat0)
  const xs = coords.map(c => c.lon * RAD * cosLat0 * EARTH_RADIUS_M)
  const ys = coords.map(c => c.lat * RAD * EARTH_RADIUS_M)

  const keep = new Array<boolean>(coords.length).fill(false)
  keep[0] = true
  keep[coords.length - 1] = true

  const stack: [number, number][] = [[0, coords.length - 1]]
  while (stack.length > 0) {
    const [first, last] = stack.pop()!
    if (last - first < 2) continue

    const x0 = xs[first]
    const y0 = ys[first]
    const dx = xs[last] - x0
    const dy = ys[last] - y0
    const segLenSq = dx * dx + dy * dy

    let maxDist = -1
    let maxIdx = -1
    for (let i = first + 1; i < last; i++) {
      const px = xs[i] - x0
      const py = ys[i] - y0
      let dist: number
      if (segLenSq === 0) {
        dist = Math.sqrt(px * px + py * py)
      } else {
        const t = Math.max(0, Math.min(1, (px * dx + py * dy) / segLenSq))
        const ex = px - t * dx
        const ey = py - t * dy
        dist = Math.sqrt(ex * ex + ey * ey)
      }
      if (dist > maxDist) { maxDist = dist; maxIdx = i }
    }

    if (maxDist > toleranceM && maxIdx > 0) {
      keep[maxIdx] = true
      stack.push([first, maxIdx], [maxIdx, last])
    }
  }

  return coords.filter((_, i) => keep[i])
}

/**
 * Thin down to at most `maxPoints`, searching for the tolerance that gets
 * there. Garmin units cap track points, and going over silently truncates.
 */
export function simplifyToMaxPoints(track: Track, maxPoints: number): Track {
  if (track.coordinates.length <= maxPoints) return track
  let lo = 0
  let hi = 500
  let best = simplifyCoordinates(track.coordinates, hi)
  for (let iter = 0; iter < 24 && hi - lo > 0.5; iter++) {
    const mid = (lo + hi) / 2
    const candidate = simplifyCoordinates(track.coordinates, mid)
    if (candidate.length > maxPoints) {
      lo = mid
    } else {
      hi = mid
      best = candidate
    }
  }
  return rebuild(track, best)
}

/* -- Elevation repair -------------------------------------------------- */

/** Bake distance-windowed smoothing into the track's stored elevations. */
export function smoothTrackElevation(track: Track, windowM: number): Track {
  const cum = cumulativeDistanceKm(track.coordinates)
  const smoothed = smoothElevations(track.elevations, cum, windowM)
  return {
    ...track,
    coordinates: track.coordinates.map((c, i) => ({
      ...c,
      elevation: smoothed[i] ?? undefined,
    })),
    elevations: smoothed,
  }
}

/** Replace elevations wholesale, e.g. after re-querying the DEM. */
export function withElevations(track: Track, elevations: (number | null)[]): Track {
  return {
    ...track,
    coordinates: track.coordinates.map((c, i) => ({
      ...c,
      elevation: elevations[i] ?? undefined,
    })),
    elevations: [...elevations],
  }
}
