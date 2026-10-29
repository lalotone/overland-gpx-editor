import type { Coordinate } from './types'
import { fetchRuntimeService, responseError } from './offline'
import type { RuntimeRequestContext } from './offline'
import { decodeRoadTags } from './access'
import type { LocatedRoad } from './access'
import { classifySurface, summarizeSurfaceDistances } from './surface'
import type { SurfaceClass, SurfaceSummary } from './surface'

/** One matched or uncertain stretch of the original track. */
export interface TrackAnnotationSpan extends LocatedRoad {
  surface: SurfaceClass
  matched: boolean
}

export interface TrackAnnotationResult {
  summary: SurfaceSummary
  /** Empty from a backend that predates per-span output. */
  spans: TrackAnnotationSpan[]
}

function decodePosition(raw: unknown, points: number): { index: number; fraction: number } {
  const value = raw !== null && typeof raw === 'object' ? raw as Record<string, unknown> : null
  const index = value?.index
  const fraction = value?.fraction ?? 0
  if (typeof index !== 'number' || !Number.isInteger(index) || index < 0 || index >= points ||
      typeof fraction !== 'number' || !(fraction >= 0 && fraction < 1)) {
    throw new Error('Malformed track annotation position')
  }
  return { index, fraction }
}

/** Advisory graph matching; never changes or replaces the supplied GPX geometry. */
export async function annotateTrack(
  coordinates: Coordinate[],
  signal?: AbortSignal,
  context: RuntimeRequestContext = {},
): Promise<TrackAnnotationResult> {
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
  const spans: TrackAnnotationSpan[] = Array.isArray(data.spans)
    ? data.spans.map((raw: unknown): TrackAnnotationSpan => {
      const item = raw !== null && typeof raw === 'object' ? raw as Record<string, unknown> : {}
      const start = decodePosition(item.start, coordinates.length)
      const end = decodePosition(item.end, coordinates.length)
      if (end.index + end.fraction < start.index + start.fraction) {
        throw new Error('Track annotation spans run backwards')
      }
      const matched = item.matched === true
      return {
        startIndex: start.index,
        startFraction: start.fraction,
        endIndex: end.index,
        endFraction: end.fraction,
        matched,
        surface: matched ? classifySurface(item.surface) : 'unknown',
        road: matched ? decodeRoadTags(item.road) : null,
      }
    })
    : []
  return { summary, spans }
}

/** Surface totals only, as the mobile route details show them. */
export async function annotateTrackSurface(
  coordinates: Coordinate[],
  signal?: AbortSignal,
  context: RuntimeRequestContext = {},
): Promise<SurfaceSummary> {
  return (await annotateTrack(coordinates, signal, context)).summary
}

/**
 * Per-segment surface classes for colouring the original track: each segment
 * takes the class of whichever span covers most of it, and stays `unknown`
 * where no span reaches.
 */
export function segmentSurfaces(spans: TrackAnnotationSpan[], points: number): SurfaceClass[] {
  const segments: SurfaceClass[] = new Array(Math.max(0, points - 1)).fill('unknown')
  const cover = new Array<number>(segments.length).fill(0)
  for (const span of spans) {
    const start = span.startIndex + span.startFraction
    const end = span.endIndex + span.endFraction
    for (let i = span.startIndex; i < segments.length && i < end; i++) {
      const overlap = Math.min(end, i + 1) - Math.max(start, i)
      if (overlap > cover[i]) {
        cover[i] = overlap
        segments[i] = span.surface
      }
    }
  }
  return segments
}
