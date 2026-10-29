import { useEffect, useState } from 'react'
import { CircleMarker, Polyline, Tooltip } from 'react-leaflet'
import { ACCESS_CLASSES, accessDefinition } from '../lib/access'
import type { AccessReport, AccessStretch } from '../lib/access'
import type { FuelLeg, FuelRangePlan, FuelRangeSettings } from '../lib/fuelRange'
import type { RoutingDataStatus, RuntimeConfig } from '../lib/offline'
import { surfaceDefinition } from '../lib/surface'
import type { SurfaceSummary } from '../lib/surface'
import { annotateTrack } from '../lib/trackSurface'
import type { TrackAnnotationResult } from '../lib/trackSurface'
import type { Coordinate } from '../lib/types'

/* -- Track annotation lookup ----------------------------------------- */

export type TrackAnnotationState =
  | { status: 'unavailable'; message: string }
  | { status: 'loading' }
  | { status: 'error'; message: string; retry: () => void }
  | { status: 'ready'; result: TrackAnnotationResult }

/**
 * Matches a loaded track to the open routing region while `enabled`. The
 * result belongs to one geometry and one routing generation; an edit or a
 * region switch makes it stale and it is read again.
 */
export function useTrackAnnotation(
  coordinates: Coordinate[] | undefined,
  runtime: RuntimeConfig,
  routing: RoutingDataStatus | null,
  enabled: boolean,
): TrackAnnotationState | null {
  const [attempt, setAttempt] = useState(0)
  const [lookup, setLookup] = useState<{
    coordinates: Coordinate[]
    graph: string
    result?: TrackAnnotationResult
    error?: string
  } | null>(null)
  const graph = JSON.stringify([routing?.regionId, routing?.generationId])
  const available = Boolean(routing?.ready && runtime.services.broomAnnotate)
  const current = lookup && lookup.coordinates === coordinates && lookup.graph === graph ? lookup : null
  const needed = enabled && available && Boolean(coordinates && coordinates.length >= 2) && !current?.result

  useEffect(() => {
    if (!needed || !coordinates) return
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
  }, [coordinates, graph, needed, runtime, attempt])

  if (!enabled || !coordinates) return null
  if (!runtime.services.broomAnnotate) {
    return { status: 'unavailable', message: 'Surface and access lookup needs the editor backend with local routing.' }
  }
  if (!routing?.ready) {
    return { status: 'unavailable', message: 'Download or open a routing region that covers this track (Offline regions).' }
  }
  if (current?.result) return { status: 'ready', result: current.result }
  if (current?.error) {
    return { status: 'error', message: current.error, retry: () => setAttempt(value => value + 1) }
  }
  return { status: 'loading' }
}

/* -- Surface ---------------------------------------------------------- */

export function SurfaceBreakdown({
  summary,
  colorBySurface,
  onToggleColor,
  matched = false,
}: {
  summary: SurfaceSummary
  colorBySurface: boolean
  onToggleColor: () => void
  /** The track was matched to the road network rather than routed on it. */
  matched?: boolean
}) {
  return (
    <div className="surface-panel">
      <div className="surface-headline">
        <span>
          <strong>{(summary.unpavedFraction * 100).toFixed(0)}%</strong> unpaved
        </span>
        <span className="surface-headline-sub">
          {summary.unpavedKm.toFixed(1)} of {summary.totalKm.toFixed(1)} km
        </span>
      </div>

      <div className="surface-bar" aria-hidden="true">
        {summary.shares.map(share => (
          <span
            key={share.id}
            className="surface-bar-part"
            style={{
              width: `${(share.fraction * 100).toFixed(2)}%`,
              background: surfaceDefinition(share.id).color,
            }}
            title={`${surfaceDefinition(share.id).label} — ${share.km.toFixed(1)} km`}
          />
        ))}
      </div>

      <ul className="surface-legend">
        {summary.shares.map(share => {
          const def = surfaceDefinition(share.id)
          return (
            <li key={share.id} title={def.hint}>
              <span className="surface-swatch" style={{ background: def.color }} />
              <span className="surface-legend-label">{def.label}</span>
              <span className="surface-legend-value">
                {share.km.toFixed(1)} km · {(share.fraction * 100).toFixed(0)}%
              </span>
            </li>
          )
        })}
      </ul>

      <button
        className="surface-mode-btn"
        onClick={onToggleColor}
        title="Paint the route on the map with these colours"
      >
        {colorBySurface ? 'Back to gradient colours' : 'Colour route by surface'}
      </button>

      {summary.unknownKm > 0.05 && (
        <p className="route-stat-note">
          {summary.unknownKm.toFixed(1)} km {matched ? 'could not be matched to a road or ' : ''}carries
          no surface tag in OpenStreetMap. It is shown as Unknown and left out of the unpaved share
          rather than assumed sealed.
        </p>
      )}
      {matched && (
        <p className="route-stat-note">
          Read by matching the track to the routing region; the GPX itself is unchanged.
        </p>
      )}
    </div>
  )
}

/* -- Access ----------------------------------------------------------- */

/** Lengths under a kilometre in metres, so a short stretch never reads 0.0 km. */
function formatLength(km: number): string {
  return km < 1 ? `${Math.max(1, Math.round(km * 1000))} m` : `${km.toFixed(1)} km`
}

function kmRange(stretch: AccessStretch): string {
  return `km ${stretch.startKm.toFixed(1)} · ${formatLength(stretch.endKm - stretch.startKm)}`
}

export function AccessWarnings({
  report,
  onFocus,
}: {
  report: AccessReport
  onFocus?: (stretch: AccessStretch) => void
}) {
  const breakdown = ACCESS_CLASSES.filter(def => (report.km[def.id] ?? 0) > 0.005)
  return (
    <div className="surface-panel access-panel">
      <div className="surface-headline">
        {report.stretches.length > 0 ? (
          <span>
            <strong>{report.stretches.length}</strong> access {report.stretches.length === 1 ? 'warning' : 'warnings'}
          </span>
        ) : (
          <span>No access restrictions tagged</span>
        )}
        <span className="surface-headline-sub">
          {report.warnKm.toFixed(1)} of {report.totalKm.toFixed(1)} km
        </span>
      </div>

      <div className="surface-bar" aria-hidden="true">
        {breakdown.map(def => (
          <span
            key={def.id}
            className="surface-bar-part"
            style={{
              width: `${(((report.km[def.id] ?? 0) / (report.totalKm || 1)) * 100).toFixed(2)}%`,
              background: def.color,
            }}
            title={`${def.label} — ${(report.km[def.id] ?? 0).toFixed(1)} km`}
          />
        ))}
      </div>

      {report.stretches.length > 0 && (
        <ul className="access-list">
          {report.stretches.map((stretch, i) => {
            const def = accessDefinition(stretch.id)
            return (
              <li key={i}>
                <button
                  type="button"
                  className="access-item"
                  onClick={() => onFocus?.(stretch)}
                  title={`${def.hint}. Click to show it on the map.`}
                >
                  <span className="surface-swatch" style={{ background: def.color }} />
                  <span className="access-item-text">
                    <span className="access-item-label">{def.label}</span>
                    <span className="access-item-tag">
                      {[stretch.tag, stretch.highway && `highway=${stretch.highway}`].filter(Boolean).join(' · ')}
                    </span>
                  </span>
                  <span className="surface-legend-value">{kmRange(stretch)}</span>
                </button>
              </li>
            )
          })}
        </ul>
      )}

      <ul className="surface-legend">
        {breakdown.filter(def => !def.warn).map(def => (
          <li key={def.id} title={def.hint}>
            <span className="surface-swatch" style={{ background: def.color }} />
            <span className="surface-legend-label">{def.label}</span>
            <span className="surface-legend-value">{formatLength(report.km[def.id] ?? 0)}</span>
          </li>
        ))}
      </ul>

      <p className="route-stat-note">
        From OpenStreetMap access tags for motorcycles. Untagged ways follow the national default
        for their road class, which varies by country and region — check local rules. Gates and
        seasonal or conditional closures are not included.
      </p>
    </div>
  )
}

/* -- Fuel range ------------------------------------------------------- */

function legText(leg: FuelLeg): string {
  const from = leg.from ? `${leg.from.station.name ?? 'fuel'} at km ${leg.from.km.toFixed(0)}` : 'the start'
  const to = leg.to ? `next fuel at km ${leg.to.km.toFixed(0)}` : 'no fuel before the end'
  return `${(leg.toKm - leg.fromKm).toFixed(0)} km from ${from} — ${to}`
}

export function FuelRangeView({
  settings,
  onSettings,
  plan,
  fuelLoaded,
  fuelLoading,
  onLoadFuel,
  corridorKm,
}: {
  settings: FuelRangeSettings
  onSettings: (settings: FuelRangeSettings) => void
  plan: FuelRangePlan | null
  fuelLoaded: boolean
  fuelLoading: boolean
  onLoadFuel: () => void
  corridorKm: number
}) {
  return (
    <div className="surface-panel fuel-range-panel">
      <div className="fuel-range-inputs">
        <label>
          <span>Full tank</span>
          <input
            className="edit-number"
            type="number"
            min={10}
            max={5000}
            step={10}
            value={settings.rangeKm}
            onChange={e => onSettings({ ...settings, rangeKm: Number(e.target.value) })}
            aria-label="Range on a full tank in km"
          />
          <span className="edit-unit">km</span>
        </label>
        <label>
          <span>Reserve</span>
          <input
            className="edit-number"
            type="number"
            min={0}
            max={90}
            step={5}
            value={settings.reservePct}
            onChange={e => onSettings({ ...settings, reservePct: Number(e.target.value) })}
            aria-label="Reserve to keep, percent of the tank"
          />
          <span className="edit-unit">%</span>
        </label>
      </div>

      {!fuelLoaded ? (
        <button className="surface-mode-btn" onClick={onLoadFuel} disabled={fuelLoading}>
          {fuelLoading ? 'Loading fuel stations…' : 'Load fuel stations along the track'}
        </button>
      ) : plan && (
        <>
          <div className="surface-headline">
            {plan.warnings.length === 0 ? (
              <span>Every leg fits in <strong>{plan.usableKm.toFixed(0)}</strong> km</span>
            ) : (
              <span>
                <strong>{plan.warnings.length}</strong> {plan.warnings.length === 1 ? 'leg' : 'legs'} beyond{' '}
                {plan.usableKm.toFixed(0)} km
              </span>
            )}
            <span className="surface-headline-sub">
              {plan.stops.length} {plan.stops.length === 1 ? 'stop' : 'stops'} · longest {plan.longestLegKm.toFixed(0)} km
            </span>
          </div>
          {plan.warnings.length > 0 && (
            <ul className="access-list">
              {plan.warnings.map((leg, i) => (
                <li key={i} className={`fuel-leg fuel-leg--${leg.status}`}>
                  <span className="fuel-leg-status">
                    {leg.status === 'dry'
                      ? `Runs dry at km ${leg.atKm?.toFixed(0)}`
                      : `On reserve from km ${leg.atKm?.toFixed(0)}`}
                  </span>
                  <span className="access-item-tag">{legText(leg)}</span>
                </li>
              ))}
            </ul>
          )}
        </>
      )}

      <p className="route-stat-note">
        Assumes a full tank at the start. Stations count within {corridorKm} km of the track, without
        the detour; opening hours and fuel types are not checked.
      </p>
    </div>
  )
}

/* -- Map overlays ----------------------------------------------------- */

export function AccessOverlay({
  coordinates,
  stretches,
}: {
  coordinates: Coordinate[]
  stretches: AccessStretch[]
}) {
  return (
    <>
      {stretches.map((stretch, i) => {
        const def = accessDefinition(stretch.id)
        const positions = coordinates
          .slice(stretch.startIndex, stretch.endIndex + 1)
          .map(point => [point.lat, point.lon] as [number, number])
        if (positions.length < 2) return null
        return (
          <Polyline
            key={i}
            positions={positions}
            pathOptions={{ color: def.color, weight: 11, opacity: 0.55, dashArray: '2 10', lineCap: 'round' }}
          >
            <Tooltip sticky>
              {def.label}{stretch.tag ? ` (${stretch.tag})` : ''} · {kmRange(stretch)}
            </Tooltip>
          </Polyline>
        )
      })}
    </>
  )
}

/** Where along the track `km` falls, interpolated between points. */
export function pointAtKm(coordinates: Coordinate[], cumKm: number[], km: number): Coordinate | null {
  if (coordinates.length === 0) return null
  let lo = 0
  let hi = cumKm.length - 1
  if (km <= 0) return coordinates[0]
  if (km >= cumKm[hi]) return coordinates[hi]
  while (hi - lo > 1) {
    const mid = (lo + hi) >> 1
    if (cumKm[mid] <= km) lo = mid
    else hi = mid
  }
  const span = cumKm[hi] - cumKm[lo]
  const t = span > 0 ? (km - cumKm[lo]) / span : 0
  return {
    lat: coordinates[lo].lat + (coordinates[hi].lat - coordinates[lo].lat) * t,
    lon: coordinates[lo].lon + (coordinates[hi].lon - coordinates[lo].lon) * t,
  }
}

export function FuelRangeMarkers({
  coordinates,
  cumKm,
  plan,
}: {
  coordinates: Coordinate[]
  cumKm: number[]
  plan: FuelRangePlan
}) {
  return (
    <>
      {plan.warnings.map((leg, i) => {
        if (leg.atKm === undefined) return null
        const point = pointAtKm(coordinates, cumKm, leg.atKm)
        if (!point) return null
        const dry = leg.status === 'dry'
        return (
          <CircleMarker
            key={i}
            center={[point.lat, point.lon]}
            radius={8}
            pathOptions={{ color: '#fff', weight: 2, fillColor: dry ? '#c0392b' : '#e67e22', fillOpacity: 1 }}
          >
            <Tooltip>
              {dry ? 'Tank empty' : 'Reserve starts'} at km {leg.atKm.toFixed(0)} — {legText(leg)}
            </Tooltip>
          </CircleMarker>
        )
      })}
    </>
  )
}
