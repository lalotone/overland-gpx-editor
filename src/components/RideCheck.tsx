import { useEffect, useMemo, useState } from 'react'
import { CircleMarker, Polyline, Tooltip } from 'react-leaflet'
import { ACCESS_CLASSES, accessDefinition } from '../lib/access'
import type { AccessReport, AccessStretch } from '../lib/access'
import type { FuelLeg, FuelRangePlan, FuelRangeSettings } from '../lib/fuelRange'
import type { RoutingDataStatus, RuntimeConfig } from '../lib/offline'
import { surfaceDefinition } from '../lib/surface'
import type { SurfaceSummary } from '../lib/surface'
import { annotateTrack } from '../lib/trackSurface'
import { Metric } from './MapDrawer'
import { MapIcon } from './mapIcons'
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
    <div className="insight">
      <div className="drawer-metrics">
        <Metric value={(summary.unpavedFraction * 100).toFixed(0)} unit="%" caption="unpaved" />
        <Metric value={summary.unpavedKm.toFixed(1)} unit="km" caption={`of ${summary.totalKm.toFixed(1)} km`} />
        {summary.unknownKm > 0.05 && (
          <Metric value={summary.unknownKm.toFixed(1)} unit="km" caption="unknown" />
        )}
      </div>

      <div className="insight-bar" aria-hidden="true">
        {summary.shares.map(share => (
          <span
            key={share.id}
            style={{ flexGrow: share.fraction, background: surfaceDefinition(share.id).color }}
            title={`${surfaceDefinition(share.id).label} — ${share.km.toFixed(1)} km`}
          />
        ))}
      </div>

      <ul className="insight-legend">
        {summary.shares.map(share => {
          const def = surfaceDefinition(share.id)
          return (
            <li key={share.id} title={def.hint}>
              <span className="insight-swatch" style={{ background: def.color }} />
              <span className="insight-legend-label">{def.label}</span>
              <span className="insight-legend-value">{share.km.toFixed(1)} km</span>
              <span className="insight-legend-share">{(share.fraction * 100).toFixed(0)}%</span>
            </li>
          )
        })}
      </ul>

      <label className="insight-switch">
        <input type="checkbox" role="switch" checked={colorBySurface} onChange={onToggleColor} />
        <span className="insight-switch-track" aria-hidden="true" />
        <span>Colour the route by surface</span>
      </label>

      <p className="insight-note">
        From OpenStreetMap surface tags.{' '}
        {summary.unknownKm > 0.05 && (
          <>Unknown distance {matched ? 'could not be matched to a road or ' : ''}has no supported tag; it is left
          out of the unpaved share rather than assumed sealed. </>
        )}
        {matched && 'Read by matching the track to the routing region; the GPX itself is unchanged.'}
      </p>
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

interface AccessGroup {
  key: string
  stretches: AccessStretch[]
  km: number
}

/** Same restriction on the same kind of way, so eight path stretches read as one line. */
function groupStretches(stretches: AccessStretch[]): AccessGroup[] {
  const groups = new Map<string, AccessGroup>()
  for (const stretch of stretches) {
    const key = `${stretch.id}|${stretch.tag ?? ''}|${stretch.highway ?? ''}`
    const group = groups.get(key) ?? { key, stretches: [], km: 0 }
    group.stretches.push(stretch)
    group.km += stretch.endKm - stretch.startKm
    groups.set(key, group)
  }
  const severity = ACCESS_CLASSES.map(def => def.id)
  return [...groups.values()].sort((a, b) =>
    severity.indexOf(a.stretches[0].id) - severity.indexOf(b.stretches[0].id) || b.km - a.km)
}

function AccessGroupRow({ group, onFocus }: { group: AccessGroup; onFocus?: (stretch: AccessStretch) => void }) {
  const [open, setOpen] = useState(false)
  const first = group.stretches[0]
  const def = accessDefinition(first.id)
  const detail = [first.tag, first.highway && `highway=${first.highway}`].filter(Boolean).join(' · ')
  return (
    <li className={`access-group${open ? ' open' : ''}`}>
      <button className="access-group-head" onClick={() => setOpen(v => !v)} aria-expanded={open} title={def.hint}>
        <span className="insight-swatch" style={{ background: def.color }} />
        <span className="access-group-text">
          <span className="access-group-label">{def.label}</span>
          <span className="access-group-detail">
            {detail}{detail ? ' · ' : ''}{group.stretches.length} {group.stretches.length === 1 ? 'stretch' : 'stretches'}
          </span>
        </span>
        <span className="access-group-km">{formatLength(group.km)}</span>
        <MapIcon id="chevron" size={14} className="access-group-chevron" />
      </button>
      {open && (
        <ol className="access-stretches">
          {group.stretches.map((stretch, i) => (
            <li key={i}>
              <button onClick={() => onFocus?.(stretch)} title="Show on the map">
                <span>km {stretch.startKm.toFixed(1)} – {stretch.endKm.toFixed(1)}</span>
                <span className="access-stretch-length">{formatLength(stretch.endKm - stretch.startKm)}</span>
              </button>
            </li>
          ))}
        </ol>
      )}
    </li>
  )
}

export function AccessWarnings({
  report,
  onFocus,
}: {
  report: AccessReport
  onFocus?: (stretch: AccessStretch) => void
}) {
  const breakdown = ACCESS_CLASSES.filter(def => (report.km[def.id] ?? 0) > 0.005)
  const groups = useMemo(() => groupStretches(report.stretches), [report.stretches])
  const untaggedKm = report.km.untagged ?? 0
  return (
    <div className="insight">
      {report.stretches.length > 0 ? (
        <div className="drawer-callout tone-danger">
          <MapIcon id="access" size={18} />
          <span>
            <strong>{report.stretches.length} {report.stretches.length === 1 ? 'stretch' : 'stretches'} may not be legal on a motorcycle</strong>
            <span>{formatLength(report.warnKm)} of {report.totalKm.toFixed(1)} km. Check them before you ride.</span>
          </span>
        </div>
      ) : (
        <div className="drawer-callout tone-ok">
          <MapIcon id="shield" size={18} />
          <span>
            <strong>No restrictions tagged</strong>
            <span>{untaggedKm > 0.05 ? `${untaggedKm.toFixed(1)} km has no access tag; local rules apply.` : 'Every way is tagged open.'}</span>
          </span>
        </div>
      )}

      <div className="insight-bar" aria-hidden="true">
        {breakdown.map(def => (
          <span
            key={def.id}
            style={{ flexGrow: (report.km[def.id] ?? 0) / (report.totalKm || 1), background: def.color }}
            title={`${def.label} — ${formatLength(report.km[def.id] ?? 0)}`}
          />
        ))}
      </div>

      {groups.length > 0 && <ul className="access-groups">{groups.map(group => (
        <AccessGroupRow key={group.key} group={group} onFocus={onFocus} />
      ))}</ul>}

      <ul className="insight-legend">
        {breakdown.filter(def => !def.warn).map(def => (
          <li key={def.id} title={def.hint}>
            <span className="insight-swatch" style={{ background: def.color }} />
            <span className="insight-legend-label">{def.label}</span>
            <span className="insight-legend-value">{formatLength(report.km[def.id] ?? 0)}</span>
          </li>
        ))}
      </ul>

      <p className="insight-note">
        From OpenStreetMap access tags for motorcycles. Untagged ways follow the national default for their road
        class, which varies by country and region. Gates and seasonal or conditional closures are not included.
      </p>
    </div>
  )
}

/* -- Fuel range ------------------------------------------------------- */

function legText(leg: FuelLeg): string {
  const from = leg.from ? `${leg.from.station.name ?? 'fuel'} at km ${leg.from.km.toFixed(0)}` : 'the start'
  const to = leg.to ? `next fuel at km ${leg.to.km.toFixed(0)}` : 'no fuel before the end'
  return `${(leg.toKm - leg.fromKm).toFixed(0)} km from ${from}, ${to}`
}

function NumberField({ label, unit, value, min, max, step, onChange }: {
  label: string
  unit: string
  value: number
  min: number
  max: number
  step: number
  onChange: (value: number) => void
}) {
  return (
    <label className="insight-field">
      <span className="insight-field-label">{label}</span>
      <span className="insight-field-input">
        <input type="number" min={min} max={max} step={step} value={value} onChange={e => onChange(Number(e.target.value))} />
        <span>{unit}</span>
      </span>
    </label>
  )
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
    <div className="insight">
      <div className="insight-fields">
        <NumberField label="Range on a full tank" unit="km" value={settings.rangeKm} min={10} max={5000} step={10}
          onChange={rangeKm => onSettings({ ...settings, rangeKm })} />
        <NumberField label="Keep in reserve" unit="%" value={settings.reservePct} min={0} max={90} step={5}
          onChange={reservePct => onSettings({ ...settings, reservePct })} />
      </div>

      {!fuelLoaded ? (
        <button className="insight-action" onClick={onLoadFuel} disabled={fuelLoading}>
          <MapIcon id="fuel" size={16} />
          {fuelLoading ? 'Finding fuel stations…' : 'Find fuel stations along the track'}
        </button>
      ) : plan && (
        <>
          <div className="drawer-metrics">
            <Metric value={plan.usableKm.toFixed(0)} unit="km" caption="usable range" />
            <Metric value={plan.longestLegKm.toFixed(0)} unit="km" caption="longest leg"
              tone={plan.longestLegKm > settings.rangeKm ? 'danger' : plan.longestLegKm > plan.usableKm ? 'warn' : 'ok'} />
            <Metric value={String(plan.stops.length)} caption={plan.stops.length === 1 ? 'station' : 'stations'} />
          </div>
          {plan.warnings.length === 0 ? (
            <div className="drawer-callout tone-ok">
              <MapIcon id="gauge" size={18} />
              <span>
                <strong>Every leg fits your range</strong>
                <span>You never need the reserve between stations.</span>
              </span>
            </div>
          ) : (
            <ul className="fuel-legs">
              {plan.warnings.map((leg, i) => (
                <li key={i} className={`drawer-callout tone-${leg.status === 'dry' ? 'danger' : 'warn'}`}>
                  <MapIcon id="fuel" size={18} />
                  <span>
                    <strong>
                      {leg.status === 'dry' ? `Tank empty at km ${leg.atKm?.toFixed(0)}` : `On reserve from km ${leg.atKm?.toFixed(0)}`}
                    </strong>
                    <span>{legText(leg)}</span>
                  </span>
                </li>
              ))}
            </ul>
          )}
        </>
      )}

      <p className="insight-note">
        Assumes a full tank at the start. Stations count within {corridorKm} km of the track, without the detour;
        opening hours and fuel types are not checked.
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

/* -- Lookup status ---------------------------------------------------- */

/** What the Surface and Access tabs show until the road match is ready. */
export function AnnotationStatus({ state }: { state: TrackAnnotationState | null }) {
  if (state?.status === 'error') {
    return (
      <div className="drawer-callout tone-warn" role="status">
        <MapIcon id="route" size={18} />
        <span>
          <strong>Could not match the track</strong>
          <span>{state.message}</span>
          <button className="insight-link" onClick={state.retry}>Try again</button>
        </span>
      </div>
    )
  }
  if (state?.status === 'unavailable') {
    return (
      <div className="drawer-callout tone-neutral">
        <MapIcon id="route" size={18} />
        <span>
          <strong>Needs local routing data</strong>
          <span>{state.message}</span>
        </span>
      </div>
    )
  }
  return (
    <div className="drawer-loading" role="status">
      <span className="drawer-spinner" aria-hidden="true" />
      <span>
        <strong>Matching the track to the road network</strong>
        <span>Usually a few seconds, longer for very long tracks.</span>
      </span>
    </div>
  )
}
