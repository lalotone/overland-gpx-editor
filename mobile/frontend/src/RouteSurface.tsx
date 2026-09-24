import { useEffect, useMemo, useState } from 'react'
import { cumulativeDistanceKm } from '../../../src/lib/geo'
import type { RuntimeConfig, RoutingDataStatus } from '../../../src/lib/offline'
import { summarizeSurface, surfaceDefinition } from '../../../src/lib/surface'
import type { SurfaceResult, SurfaceSummary } from '../../../src/lib/surface'
import { annotateTrackSurface } from '../../../src/lib/trackSurface'
import type { Coordinate } from '../../../src/lib/types'

export default function RouteSurface({ coordinates, surface, runtime, routing }: {
  coordinates: Coordinate[]
  surface?: SurfaceResult
  runtime: RuntimeConfig
  routing: RoutingDataStatus | null
}) {
  const [attempt, setAttempt] = useState(0)
  const [lookup, setLookup] = useState<{
    coordinates: Coordinate[]
    graph: string
    summary?: SurfaceSummary
    error?: string
  } | null>(null)
  const graph = JSON.stringify([routing?.regionId, routing?.generationId])
  const available = routing?.ready && Boolean(runtime.services.broomAnnotate)
  const inline = useMemo(() => surface
    ? summarizeSurface(surface.segments, cumulativeDistanceKm(coordinates))
    : undefined, [surface, coordinates])
  useEffect(() => {
    if (surface || !available) return
    const controller = new AbortController()
    setLookup(null)
    void annotateTrackSurface(coordinates, controller.signal, { runtime })
      .then(summary => {
        if (!controller.signal.aborted) setLookup({ coordinates, graph, summary })
      })
      .catch(error => {
        if (!controller.signal.aborted) setLookup({ coordinates, graph, error: (error as Error).message })
      })
    return () => controller.abort()
  }, [coordinates, graph, surface, available, runtime, attempt])
  const current = available && lookup?.coordinates === coordinates && lookup.graph === graph ? lookup : null
  const summary = inline ?? current?.summary

  return (
    <section className="route-surface" aria-label="Surface types">
      <h3>Surface types</h3>
      {summary ? <>
        {summary.totalKm > 0 ? <>
          <div className="surface-bar" aria-hidden="true">
            {summary.shares.map(share => <span key={share.id} style={{ flex: share.fraction, background: surfaceDefinition(share.id).color }} />)}
          </div>
          <ul className="surface-shares">
            {summary.shares.map(share => {
              const definition = surfaceDefinition(share.id)
              const percent = share.fraction * 100
              return <li key={share.id}>
                <i aria-hidden="true" style={{ background: definition.color }} />
                <span>{definition.label}</span>
                <strong>{percent < 0.1 ? '<0.1' : Number(percent.toFixed(1))}%</strong>
              </li>
            })}
          </ul>
        </> : <p>No measurable track distance.</p>}
        <p>Share of total distance · OSM surface tags. Unknown includes untagged or uncertain sections.</p>
        {!surface && <p>Approximate matching to the open routing region; GPX geometry is unchanged.</p>}
      </> : !available ? (
        <p>{!runtime.services.broomAnnotate
          ? 'Track surface lookup requires a backend with Broom 0.9 or newer.'
          : 'Open or download a routing region in Offline to look up track surfaces.'}</p>
      ) : current?.error ? <>
        <p role="status">Surface lookup unavailable: {current.error}</p>
        <button className="text-button" onClick={() => setAttempt(value => value + 1)}>Retry surface lookup</button>
      </> : <p role="status">Looking up track surfaces…</p>}
    </section>
  )
}
