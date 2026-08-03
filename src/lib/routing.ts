/*
 * Route calculation.
 *
 * Profiles are motorbike-shaped: the old "offroad" mode used Valhalla's
 * bicycle/Mountain costing, which routes onto MTB singletrack a loaded bike
 * cannot ride, avoids legal forest roads it can, and reports a cyclist's
 * travel time. Valhalla's `motorcycle` costing models the right vehicle;
 * `use_trails` is what moves it between tarmac and dirt.
 */

import type { Coordinate } from './types'

export const VALHALLA_API = 'https://valhalla1.openstreetmap.de'
const OSRM_API = 'https://router.project-osrm.org'

export type RoutingProfile = 'road' | 'mixed' | 'trail'

export interface ProfileDefinition {
  id: RoutingProfile
  label: string
  hint: string
}

export const ROUTING_PROFILES: ProfileDefinition[] = [
  { id: 'road', label: 'Road', hint: 'Sealed roads — fastest sensible tarmac route' },
  { id: 'mixed', label: 'Dirt', hint: 'Prefers unsealed roads and forest tracks over tarmac' },
  { id: 'trail', label: 'Trail', hint: 'Maximum offroad — narrow tracks and paths where legal' },
]

export interface RouteResult {
  coordinates: Coordinate[]
  durationSeconds: number | null
  distanceKm: number | null
  /** Human-readable description of what actually produced this route. */
  engine: string
  /** Set when we had to fall back from the preferred costing model. */
  warning?: string
}

export class RoutingError extends Error {
  constructor(message: string) {
    super(message)
    this.name = 'RoutingError'
  }
}

/** Valhalla returns shapes as polylines with 6 decimal places of precision. */
export function decodePolyline6(encoded: string): Coordinate[] {
  const coords: Coordinate[] = []
  let index = 0
  let lat = 0
  let lon = 0
  while (index < encoded.length) {
    let shift = 0
    let result = 0
    let b: number
    do { b = encoded.charCodeAt(index++) - 63; result |= (b & 0x1f) << shift; shift += 5 } while (b >= 0x20)
    lat += (result & 1) ? ~(result >> 1) : (result >> 1)
    shift = 0
    result = 0
    do { b = encoded.charCodeAt(index++) - 63; result |= (b & 0x1f) << shift; shift += 5 } while (b >= 0x20)
    lon += (result & 1) ? ~(result >> 1) : (result >> 1)
    coords.push({ lat: lat / 1e6, lon: lon / 1e6 })
  }
  return coords
}

export interface ValhallaCosting {
  costing: string
  costing_options: Record<string, Record<string, number | string>>
}

/** Exported so surface tracing can walk the route under the same costing. */
export function motorcycleCosting(profile: RoutingProfile): ValhallaCosting {
  const options: Record<RoutingProfile, Record<string, number>> = {
    // use_trails is Valhalla's "how willing are you to leave the tarmac" dial.
    road:  { use_highways: 0.6, use_tolls: 0.5, use_trails: 0.0 },
    mixed: { use_highways: 0.1, use_tolls: 0.0, use_trails: 0.6 },
    trail: { use_highways: 0.0, use_tolls: 0.0, use_trails: 1.0 },
  }
  return { costing: 'motorcycle', costing_options: { motorcycle: options[profile] } }
}

/** Used when a Valhalla build has motorcycle costing disabled. */
function fallbackCosting(profile: RoutingProfile): ValhallaCosting {
  if (profile === 'trail') {
    return {
      costing: 'bicycle',
      costing_options: {
        bicycle: { bicycle_type: 'Mountain', use_roads: 0.1, use_hills: 1.0, use_trails: 1.0 },
      },
    }
  }
  return {
    costing: 'auto',
    costing_options: { auto: { use_highways: profile === 'road' ? 0.6 : 0.1, use_tolls: 0.0 } },
  }
}

function shapeToCoordinates(legs: { shape: string }[]): Coordinate[] {
  const out: Coordinate[] = []
  for (const leg of legs) {
    const decoded = decodePolyline6(leg.shape)
    // Drop each leg's first point after the first leg — it duplicates the
    // previous leg's last point at the junction.
    out.push(...decoded.slice(out.length > 0 ? 1 : 0))
  }
  return out
}

async function valhallaRoute(
  waypoints: Coordinate[],
  costing: ValhallaCosting,
  signal?: AbortSignal,
): Promise<RouteResult | null> {
  const res = await fetch(`${VALHALLA_API}/route`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({
      locations: waypoints.map(w => ({ lat: w.lat, lon: w.lon })),
      directions_options: { units: 'kilometers' },
      ...costing,
    }),
    signal,
  })

  if (!res.ok) {
    // 400 usually means "costing model not supported by this instance",
    // which the caller can retry around. Anything else is a hard failure.
    if (res.status === 400) return null
    throw new RoutingError(`Routing service returned ${res.status}`)
  }

  const data = await res.json()
  const coordinates = shapeToCoordinates(data.trip?.legs ?? [])
  if (coordinates.length === 0) return null

  return {
    coordinates,
    durationSeconds: data.trip?.summary?.time ?? null,
    distanceKm: data.trip?.summary?.length ?? null,
    engine: `Valhalla ${costing.costing}`,
  }
}

async function osrmRoute(waypoints: Coordinate[], signal?: AbortSignal): Promise<RouteResult | null> {
  const path = waypoints.map(w => `${w.lon.toFixed(6)},${w.lat.toFixed(6)}`).join(';')
  const res = await fetch(
    `${OSRM_API}/route/v1/driving/${path}?overview=full&geometries=geojson`,
    { signal },
  )
  if (!res.ok) return null
  const data = await res.json()
  const route = data.routes?.[0]
  if (!route) return null
  return {
    coordinates: route.geometry.coordinates.map((c: number[]) => ({ lat: c[1], lon: c[0] })),
    durationSeconds: route.duration ?? null,
    distanceKm: route.distance != null ? route.distance / 1000 : null,
    engine: 'OSRM driving',
  }
}

/**
 * Route through the given waypoints.
 *
 * Tries motorcycle costing, falls back to auto/bicycle if the Valhalla
 * instance does not offer it, and finally to OSRM for road routing. The
 * returned `warning` says when a fallback was used so the UI can tell the
 * user their travel time is modelled on a different vehicle.
 */
export async function calculateRoute(
  waypoints: Coordinate[],
  profile: RoutingProfile,
  signal?: AbortSignal,
): Promise<RouteResult> {
  if (waypoints.length < 2) throw new RoutingError('Need at least two waypoints')

  const primary = await valhallaRoute(waypoints, motorcycleCosting(profile), signal)
  if (primary) return primary

  const alt = fallbackCosting(profile)
  const secondary = await valhallaRoute(waypoints, alt, signal)
  if (secondary) {
    return {
      ...secondary,
      warning:
        `Motorcycle routing unavailable on this server — used ${alt.costing} instead. ` +
        'Travel time is modelled on a different vehicle.',
    }
  }

  if (profile === 'road') {
    const osrm = await osrmRoute(waypoints, signal)
    if (osrm) {
      return { ...osrm, warning: 'Fell back to OSRM car routing.' }
    }
  }

  throw new RoutingError('No route found between these points')
}

export function formatDuration(seconds: number): string {
  const h = Math.floor(seconds / 3600)
  const m = Math.round((seconds % 3600) / 60)
  if (h === 0) return `~${m}min`
  if (m === 0) return `~${h}h`
  return `~${h}h ${m}min`
}
