import { useEffect, useMemo, useState } from 'react'
import type { ReactNode } from 'react'
import { AccessWarnings, FuelRangeView, SurfaceBreakdown } from '../../../src/components/RideCheck'
import { MapIcon } from '../../../src/components/mapIcons'
import type { MapIconId } from '../../../src/components/mapIcons'
import { accessReport, locateSegmentRoads } from '../../../src/lib/access'
import type { AccessReport, AccessStretch } from '../../../src/lib/access'
import { DEFAULT_FUEL_RANGE, fuelStopsAlong, normalizeFuelRange, planFuelRange } from '../../../src/lib/fuelRange'
import type { FuelRangePlan, FuelRangeSettings } from '../../../src/lib/fuelRange'
import { cumulativeDistanceKm } from '../../../src/lib/geo'
import type { RuntimeConfig, RoutingDataStatus } from '../../../src/lib/offline'
import { boundsAround, fetchPoisForArea } from '../../../src/lib/poi'
import type { Poi } from '../../../src/lib/poi'
import type { RouteResult } from '../../../src/lib/routing'
import { summarizeSurface } from '../../../src/lib/surface'
import { annotateTrack } from '../../../src/lib/trackSurface'
import type { TrackAnnotationResult } from '../../../src/lib/trackSurface'
import type { Coordinate } from '../../../src/lib/types'

/** Same corridor as the desktop fuel gap: a station within 3 km is on the way. */
const FUEL_CORRIDOR_M = 3000

export interface RideOverlay {
  access: AccessStretch[]
  fuel: FuelRangePlan | null
}

function Section({ icon, title, children }: { icon: MapIconId; title: string; children: ReactNode }) {
  return (
    <section className="ride-section">
      <h3>
        <span className="ride-section-icon"><MapIcon id={icon} size={16} /></span>
        {title}
      </h3>
      {children}
    </section>
  )
}

function readFuelRange(): FuelRangeSettings {
  try {
    return normalizeFuelRange(JSON.parse(localStorage.getItem('gpx-fuel-range') ?? '{}') as Partial<FuelRangeSettings>)
  } catch {
    return DEFAULT_FUEL_RANGE
  }
}

/**
 * Surface, access and fuel range for a planned route or an opened track.
 * A planned route carries its own surface and road tags; an opened track is
 * matched to the open routing region on demand, never changing the GPX.
 */
export default function RideDetails({
  coordinates,
  route,
  runtime,
  routing,
  onOverlay,
}: {
  coordinates: Coordinate[]
  /** Present on the planner, where Broom returned the geometry inline. */
  route?: RouteResult
  runtime: RuntimeConfig
  routing: RoutingDataStatus | null
  onOverlay: (overlay: RideOverlay) => void
}) {
  const cumKm = useMemo(() => cumulativeDistanceKm(coordinates), [coordinates])

  /* -- Opened track: match it to the routing region ------------------- */
  const [attempt, setAttempt] = useState(0)
  const [lookup, setLookup] = useState<{
    coordinates: Coordinate[]
    graph: string
    result?: TrackAnnotationResult
    error?: string
  } | null>(null)
  const graph = JSON.stringify([routing?.regionId, routing?.generationId])
  const available = Boolean(routing?.ready && runtime.services.broomAnnotate)
  useEffect(() => {
    if (route || !available) return
    const controller = new AbortController()
    setLookup(null)
    void annotateTrack(coordinates, controller.signal, { runtime })
      .then(result => {
        if (!controller.signal.aborted) setLookup({ coordinates, graph, result })
      })
      .catch(error => {
        if (!controller.signal.aborted) setLookup({ coordinates, graph, error: (error as Error).message })
      })
    return () => controller.abort()
  }, [coordinates, graph, route, available, runtime, attempt])
  const current = available && lookup?.coordinates === coordinates && lookup.graph === graph ? lookup : null

  const surface = useMemo(
    () => route ? summarizeSurface(route.surface.segments, cumKm) : current?.result?.summary,
    [route, cumKm, current?.result],
  )
  const access: AccessReport | null = useMemo(() => {
    if (route) return route.roads.length === cumKm.length - 1 ? accessReport(locateSegmentRoads(route.roads), cumKm) : null
    const spans = current?.result?.spans
    return spans && spans.length > 0 ? accessReport(spans, cumKm) : null
  }, [route, cumKm, current?.result])

  /* -- Fuel range ------------------------------------------------------ */
  const [fuelRange, setFuelRange] = useState(readFuelRange)
  useEffect(() => { localStorage.setItem('gpx-fuel-range', JSON.stringify(fuelRange)) }, [fuelRange])
  const [fuel, setFuel] = useState<{ coordinates: Coordinate[]; stations: Poi[] } | null>(null)
  const [fuelLoading, setFuelLoading] = useState(false)
  const [fuelError, setFuelError] = useState<string | null>(null)
  const stations = fuel?.coordinates === coordinates ? fuel.stations : null
  const fuelPlan = useMemo(() => {
    if (!stations || cumKm.length === 0) return null
    return planFuelRange(cumKm[cumKm.length - 1], fuelStopsAlong(coordinates, cumKm, stations, FUEL_CORRIDOR_M), fuelRange)
  }, [stations, coordinates, cumKm, fuelRange])
  const loadFuel = async () => {
    const bbox = boundsAround(coordinates, 5)
    if (!bbox) return
    setFuelLoading(true)
    setFuelError(null)
    try {
      const { pois } = await fetchPoisForArea('fuel', bbox, undefined, { runtime })
      setFuel({ coordinates, stations: pois })
    } catch (error) {
      setFuelError((error as Error).message)
    } finally {
      setFuelLoading(false)
    }
  }

  useEffect(() => {
    onOverlay({ access: access?.stretches ?? [], fuel: fuelPlan })
  }, [access, fuelPlan, onOverlay])
  useEffect(() => () => onOverlay({ access: [], fuel: null }), [onOverlay])

  return (
    <>
      <Section icon="surface" title="Surface">
        {surface ? (
          surface.totalKm > 0
            ? <SurfaceBreakdown summary={surface} matched={!route} />
            : <p className="ride-note">No measurable track distance.</p>
        ) : !available ? (
          <p className="ride-note">{!runtime.services.broomAnnotate
            ? 'Track surface lookup requires a backend with Broom 0.9 or newer.'
            : 'Open or download a routing region in Offline to look up track surfaces.'}</p>
        ) : current?.error ? (
          <div className="ride-status ride-status--error">
            <p role="status">Surface lookup unavailable: {current.error}</p>
            <button className="text-button" onClick={() => setAttempt(value => value + 1)}>Retry surface lookup</button>
          </div>
        ) : (
          <div className="ride-status">
            <span className="drawer-spinner" aria-hidden="true" />
            <p role="status">Looking up track surfaces…</p>
          </div>
        )}
      </Section>

      <Section icon="access" title="Access">
        {access ? (
          <AccessWarnings report={access} />
        ) : (
          <p className="ride-note">
            {route || current?.result
              ? 'This backend reports surface totals only; update it to check access.'
              : 'Read from the same road match as the surface.'}
          </p>
        )}
      </Section>

      <Section icon="fuel" title="Fuel range">
        <FuelRangeView
          settings={fuelRange}
          onSettings={next => setFuelRange(normalizeFuelRange(next))}
          plan={fuelPlan}
          fuelLoaded={stations !== null}
          fuelLoading={fuelLoading}
          onLoadFuel={() => void loadFuel()}
          corridorKm={FUEL_CORRIDOR_M / 1000}
        />
        {fuelError && <p className="ride-note ride-note--error">Could not load fuel stations: {fuelError}</p>}
      </Section>
    </>
  )
}
