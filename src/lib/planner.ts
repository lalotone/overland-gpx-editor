import type { SurfaceClass } from './surface'
import type { Coordinate } from './types'

export interface ClearedRouteDerivedState {
  coordinates: Coordinate[]
  durationSeconds: null
  engine: null
  surfaceSegments: SurfaceClass[] | null
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
    surfaceSegments: null,
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
