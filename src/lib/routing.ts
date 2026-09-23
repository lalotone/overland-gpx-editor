/* Local, offline-first route calculation through the embedded Broom backend. */

import type { Coordinate } from './types'
import { fetchRuntimeService, responseError } from './offline'
import type { RuntimeRequestContext } from './offline'
import { classifySurface } from './surface'
import type { SurfaceClass, SurfaceResult } from './surface'

export type RoutingProfile = 'road' | 'mixed' | 'trail' | 'enduro' | 'custom'

export interface ProfileDefinition {
  id: RoutingProfile
  label: string
  hint: string
}

export const ROUTING_PROFILES: ProfileDefinition[] = [
  { id: 'road', label: 'Road', hint: 'Sealed roads — fastest sensible tarmac route' },
  { id: 'mixed', label: 'Dirt', hint: 'Prefers unsealed roads and forest tracks over tarmac' },
  { id: 'trail', label: 'Trail', hint: 'Maximum offroad — narrow tracks and paths where legal' },
  { id: 'enduro', label: 'Enduro', hint: 'Road-registered enduro motorcycle — prefers unsealed tracks; paths require explicit motor permission' },
]

export interface RouteResult {
  coordinates: Coordinate[]
  durationSeconds: number | null
  distanceKm: number | null
  engine: string
  surface: SurfaceResult
  inlineElevations: true
  regionId?: string
  generationId?: string
}

export class RoutingError extends Error {
  constructor(message: string) {
    super(message)
    this.name = 'RoutingError'
  }
}

interface BroomRoutePayload {
  schemaVersion?: unknown
  engine?: unknown
  engineVersion?: unknown
  regionId?: unknown
  generationId?: unknown
  coordinates?: unknown
  elevations?: unknown
  segments?: unknown
  distanceMeters?: unknown
  durationSeconds?: unknown
}

interface BroomSegmentPayload {
  geometryStart?: unknown
  geometryEnd?: unknown
  surface?: unknown
  beeline?: unknown
}

function finiteNumber(value: unknown): value is number {
  return typeof value === 'number' && Number.isFinite(value)
}

function decodeBroomRoute(data: BroomRoutePayload): RouteResult {
  if (data.schemaVersion !== 1 || !Array.isArray(data.coordinates) || data.coordinates.length < 2 ||
      !Array.isArray(data.elevations) || data.elevations.length !== data.coordinates.length ||
      !Array.isArray(data.segments)) {
    throw new RoutingError('Malformed local routing response')
  }
  const elevations = data.elevations as unknown[]
  const coordinates = data.coordinates.map((raw, index): Coordinate => {
    const point = raw !== null && typeof raw === 'object' ? raw as Record<string, unknown> : null
    if (!point || !finiteNumber(point.lat) || !finiteNumber(point.lon) ||
        point.lat < -90 || point.lat > 90 || point.lon < -180 || point.lon > 180) {
      throw new RoutingError('Malformed local route geometry')
    }
    const elevation = elevations[index]
    if (elevation === null) return { lat: point.lat, lon: point.lon }
    const sample = typeof elevation === 'object' ? elevation as Record<string, unknown> : null
    if (!sample || !finiteNumber(sample.meters) || typeof sample.interpolated !== 'boolean') {
      throw new RoutingError('Malformed local route elevation')
    }
    return {
      lat: point.lat,
      lon: point.lon,
      elevation: sample.meters,
      elevationInterpolated: sample.interpolated,
    }
  })
  const surfaceSegments: SurfaceClass[] = new Array(coordinates.length - 1).fill('unknown')
  for (const raw of data.segments as BroomSegmentPayload[]) {
    if (!Number.isInteger(raw.geometryStart) || !Number.isInteger(raw.geometryEnd)) {
      throw new RoutingError('Malformed local route annotations')
    }
    const start = raw.geometryStart as number
    const end = raw.geometryEnd as number
    if (start < 0 || end < start || end >= coordinates.length) {
      throw new RoutingError('Local route annotations do not align with geometry')
    }
    const surface = raw.beeline === true ? 'unknown' : classifySurface(raw.surface)
    for (let i = start; i < end; i++) surfaceSegments[i] = surface
  }
  return {
    coordinates,
    durationSeconds: finiteNumber(data.durationSeconds) ? data.durationSeconds : null,
    distanceKm: finiteNumber(data.distanceMeters) ? data.distanceMeters / 1000 : null,
    engine: `${typeof data.engine === 'string' ? data.engine : 'Broom'}${typeof data.engineVersion === 'string' ? ` ${data.engineVersion}` : ''}`,
    surface: { segments: surfaceSegments },
    inlineElevations: true,
    regionId: typeof data.regionId === 'string' ? data.regionId : undefined,
    generationId: typeof data.generationId === 'string' ? data.generationId : undefined,
  }
}

export async function calculateRoute(
  waypoints: Coordinate[],
  profile: RoutingProfile,
  signal?: AbortSignal,
  context: RuntimeRequestContext & { sessionProfile?: string; accessPermit?: boolean } = {},
): Promise<RouteResult> {
  if (waypoints.length < 2) throw new RoutingError('Need at least two waypoints')
  if (!context.runtime?.services.broomRoute) {
    throw new RoutingError('Local Broom routing is unavailable. Start the editor backend and prepare routing data.')
  }
  const { response } = await fetchRuntimeService({
    ...context,
    service: 'broomRoute',
    directUrl: '',
    backendInit: {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ profile, sessionProfile: context.sessionProfile, accessPermit: context.accessPermit === true, waypoints: waypoints.map(({ lat, lon }) => ({ lat, lon })) }),
    },
    signal,
  })
  if (!response.ok) throw await responseError(response, `Local routing returned ${response.status}`)
  return decodeBroomRoute(await response.json() as BroomRoutePayload)
}

export function formatDuration(seconds: number): string {
  const h = Math.floor(seconds / 3600)
  const m = Math.round((seconds % 3600) / 60)
  if (h === 0) return `~${m}min`
  if (m === 0) return `~${h}h`
  return `~${h}h ${m}min`
}
