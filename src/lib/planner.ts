import type { CacheMetadata } from './offline'
import type { SurfaceClass } from './surface'
import type { Coordinate } from './types'

export interface ClearedRouteDerivedState {
  coordinates: Coordinate[]
  durationSeconds: null
  engine: null
  routeCache: CacheMetadata | undefined
  surfaceSegments: SurfaceClass[] | null
  surfaceApproximate: false
  surfaceCache: CacheMetadata | undefined
  surfaceError: null
  surfaceLoading: false
  elevationInterpolated: false
  elevationApiError: false
  routeStatus: ''
  routedLoading: false
}

/** Every value that becomes invalid when planner waypoints define a new route. */
export function clearedRouteDerivedState(): ClearedRouteDerivedState {
  return {
    coordinates: [],
    durationSeconds: null,
    engine: null,
    routeCache: undefined,
    surfaceSegments: null,
    surfaceApproximate: false,
    surfaceCache: undefined,
    surfaceError: null,
    surfaceLoading: false,
    elevationInterpolated: false,
    elevationApiError: false,
    routeStatus: '',
    routedLoading: false,
  }
}

export function routeSequenceIsCurrent(
  sequence: number,
  currentSequence: number,
  aborted: boolean,
): boolean {
  return sequence === currentSequence && !aborted
}
