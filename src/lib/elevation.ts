/*
 * Elevation lookups.
 *
 * Requests are batched and run with bounded concurrency. The previous
 * one-request-per-sample loop meant a 200 km route made dozens of sequential
 * round trips, which is why elevation used to be sampled every 20th point and
 * linearly interpolated in between — that interpolation was then written into
 * the saved GPX as if it were measured. We fetch every point instead.
 *
 * Calls go through the backend proxy, which picks the DEM source: the public
 * Open-Meteo API by default, or a self-hosted opentopodata instance when one
 * is configured. A direct browser call is the fallback, and only exists for
 * installs that set VITE_ELEVATION_API.
 */

import type { Coordinate } from './types'

const CHUNK_SIZE = 100
const CONCURRENCY = 5

/** Above this many points we thin the request set and interpolate the rest. */
export const MAX_ELEVATION_LOOKUPS = 6000

export interface ElevationOptions {
  signal?: AbortSignal
  onProgress?: (done: number, total: number) => void
  dataset?: string
}

export class ElevationUnavailableError extends Error {
  constructor(message = 'Elevation service unavailable') {
    super(message)
    this.name = 'ElevationUnavailableError'
  }
}

/* -- Single-point lookup, memoised (used by the cursor readout) -------- */

const pointCache = new Map<string, number | null>()

export async function fetchGroundElevation(
  lat: number,
  lon: number,
  apiBase: string,
  elevationApi: string,
  dataset: string,
  signal?: AbortSignal,
): Promise<number | null> {
  // ~11 m of precision, comfortably finer than any DEM we query.
  const key = `${dataset}:${lat.toFixed(4)},${lon.toFixed(4)}`
  const cached = pointCache.get(key)
  if (cached !== undefined) return cached

  // A failed lookup is not cached — the service may just be briefly away.
  const values = await fetchChunk([{ lat, lon }], apiBase, elevationApi, dataset, signal)
  const value = values[0] ?? null

  if (pointCache.size > 4000) pointCache.clear()
  pointCache.set(key, value)
  return value
}

/* -- Batch lookup ----------------------------------------------------- */

function chunk<T>(items: T[], size: number): T[][] {
  const out: T[][] = []
  for (let i = 0; i < items.length; i += size) out.push(items.slice(i, i + size))
  return out
}

function encodeLocations(coords: Coordinate[]): string {
  return coords.map(c => `${c.lat.toFixed(6)},${c.lon.toFixed(6)}`).join('|')
}

function readResults(data: unknown, expected: number): (number | null)[] {
  const results = (data as { results?: { elevation?: number | null }[] })?.results
  if (!Array.isArray(results)) throw new ElevationUnavailableError('Malformed elevation response')
  const out: (number | null)[] = new Array(expected).fill(null)
  for (let i = 0; i < Math.min(results.length, expected); i++) {
    const e = results[i]?.elevation
    out[i] = typeof e === 'number' && Number.isFinite(e) ? e : null
  }
  return out
}

async function fetchChunk(
  coords: Coordinate[],
  apiBase: string,
  elevationApi: string,
  dataset: string,
  signal?: AbortSignal,
): Promise<(number | null)[]> {
  const locations = encodeLocations(coords)

  // Preferred path: backend proxy (reachable from anywhere the app is served).
  try {
    const res = await fetch(`${apiBase}/elevation/batch`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ locations, dataset }),
      signal,
    })
    if (res.ok) return readResults(await res.json(), coords.length)
  } catch (err) {
    if ((err as Error)?.name === 'AbortError') throw err
  }

  // Fallback: talk to an opentopodata-style service directly. Only configured
  // installs have one; without it the proxy failure is the answer.
  if (!elevationApi) {
    throw new ElevationUnavailableError('Elevation service unreachable')
  }
  const res = await fetch(
    `${elevationApi}/v1/${dataset}?locations=${encodeURIComponent(locations)}`,
    { signal },
  )
  if (!res.ok) throw new ElevationUnavailableError(`Elevation service returned ${res.status}`)
  return readResults(await res.json(), coords.length)
}

/** Run `tasks` with at most `limit` in flight, preserving result order. */
async function pooled<T>(tasks: (() => Promise<T>)[], limit: number): Promise<T[]> {
  const results = new Array<T>(tasks.length)
  let next = 0
  const workers = Array.from({ length: Math.min(limit, tasks.length) }, async () => {
    while (true) {
      const i = next++
      if (i >= tasks.length) return
      results[i] = await tasks[i]()
    }
  })
  await Promise.all(workers)
  return results
}

/**
 * Elevation for every coordinate.
 *
 * Beyond MAX_ELEVATION_LOOKUPS points we query an evenly spaced subset and
 * interpolate between them — but only then, and the caller is told via
 * `interpolated` so it can be surfaced rather than silently written to file.
 */
export async function fetchElevationProfile(
  coords: Coordinate[],
  apiBase: string,
  elevationApi: string,
  opts: ElevationOptions = {},
): Promise<{ elevations: (number | null)[]; interpolated: boolean }> {
  if (coords.length === 0) return { elevations: [], interpolated: false }

  const dataset = opts.dataset ?? 'srtm30m'
  const stride = Math.max(1, Math.ceil(coords.length / MAX_ELEVATION_LOOKUPS))
  const interpolated = stride > 1

  const sampleIndices: number[] = []
  for (let i = 0; i < coords.length; i += stride) sampleIndices.push(i)
  if (sampleIndices[sampleIndices.length - 1] !== coords.length - 1) {
    sampleIndices.push(coords.length - 1)
  }

  const samples = sampleIndices.map(i => coords[i])
  const groups = chunk(samples, CHUNK_SIZE)

  let done = 0
  const tasks = groups.map(group => async () => {
    const values = await fetchChunk(group, apiBase, elevationApi, dataset, opts.signal)
    done += group.length
    opts.onProgress?.(done, samples.length)
    return values
  })

  const sampled = (await pooled(tasks, CONCURRENCY)).flat()
  if (sampled.every(v => v === null)) {
    throw new ElevationUnavailableError()
  }

  if (!interpolated) return { elevations: sampled, interpolated: false }

  // Fill the gaps between measured samples.
  const elevations: (number | null)[] = new Array(coords.length).fill(null)
  for (let s = 0; s < sampleIndices.length; s++) elevations[sampleIndices[s]] = sampled[s]
  for (let s = 0; s < sampleIndices.length - 1; s++) {
    const i0 = sampleIndices[s]
    const i1 = sampleIndices[s + 1]
    const e0 = sampled[s]
    const e1 = sampled[s + 1]
    if (e0 === null || e1 === null) continue
    for (let i = i0 + 1; i < i1; i++) {
      elevations[i] = e0 + ((i - i0) / (i1 - i0)) * (e1 - e0)
    }
  }
  return { elevations, interpolated: true }
}

/** Fetch elevations and attach them to a copy of the coordinates. */
export async function attachElevations(
  coords: Coordinate[],
  apiBase: string,
  elevationApi: string,
  opts: ElevationOptions = {},
): Promise<{ coordinates: Coordinate[]; interpolated: boolean }> {
  const { elevations, interpolated } = await fetchElevationProfile(coords, apiBase, elevationApi, opts)
  return {
    coordinates: coords.map((c, i) => ({
      ...c,
      elevation: elevations[i] ?? undefined,
    })),
    interpolated,
  }
}
