import type { Coordinate } from './types'
import { fetchRuntimeService, responseError } from './offline'
import type { RuntimeRequestContext } from './offline'
import { classifySurface, summarizeSurfaceDistances } from './surface'
import type { SurfaceSummary } from './surface'

/** Advisory graph matching; never changes or replaces the supplied GPX geometry. */
export async function annotateTrackSurface(
  coordinates: Coordinate[],
  signal?: AbortSignal,
  context: RuntimeRequestContext = {},
): Promise<SurfaceSummary> {
  if (coordinates.length < 2 || coordinates.length > 50001) {
    throw new Error('Surface lookup needs 2–50,001 points. Split larger tracks first.')
  }
  const { response } = await fetchRuntimeService({
    ...context,
    service: 'broomAnnotate',
    directUrl: '',
    backendInit: {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ coordinates: coordinates.map(({ lat, lon }) => ({ lat, lon })) }),
    },
    signal,
  })
  if (!response.ok) throw await responseError(response, 'Could not look up track surfaces')
  const data = await response.json()
  if (!data || data.schemaVersion !== 1 || !Array.isArray(data.surfaces) ||
      !Number.isFinite(data.distanceMeters) || data.distanceMeters < 0) {
    throw new Error('Malformed track surface response')
  }
  const summary = summarizeSurfaceDistances(data.surfaces.map((raw: unknown) => {
    const item = raw !== null && typeof raw === 'object' ? raw as Record<string, unknown> : null
    if (!item || typeof item.surface !== 'string' || typeof item.distanceMeters !== 'number' ||
        !Number.isFinite(item.distanceMeters) || item.distanceMeters < 0) {
      throw new Error('Malformed track surface distances')
    }
    return { id: classifySurface(item.surface), km: item.distanceMeters / 1000 }
  }))
  if (Math.abs(summary.totalKm * 1000 - data.distanceMeters) > Math.max(0.01, data.distanceMeters * 1e-6)) {
    throw new Error('Track surface distances do not cover the original track')
  }
  return summary
}
