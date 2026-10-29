import { useMemo } from 'react'
import L from 'leaflet'
import { Circle } from 'react-leaflet'
import { AROUND_CATEGORIES, AROUND_RADII_KM, aroundCategory, NEAR_ROUTE_M, ON_ROUTE_M } from '../lib/around'
import type { AroundCategory, AroundPoi, AroundSearchResult, RouteOffset } from '../lib/around'
import { MapIcon, mapIconMarkup } from './mapIcons'
import './insight.css'

export interface AroundState {
  /** Route point number shown in the header, 1-based. */
  pointNumber: number
  radiusKm: number
  loading: boolean
  /** The user stopped a search that had nothing earlier to fall back to. */
  cancelled?: boolean
  error?: string
  result?: AroundSearchResult
}

export type AroundSort = 'nearest' | 'route'

const LIST_LIMIT = 80

export function formatDistance(meters: number): string {
  return meters < 1000 ? `${Math.round(meters)} m` : `${(meters / 1000).toFixed(meters < 10_000 ? 1 : 0)} km`
}

/** OSM object page, so a result can be checked against the source. */
export function osmLink(poi: AroundPoi): string {
  const [type, id] = poi.id.split('/')
  return `https://www.openstreetmap.org/${type}/${id}`
}

const iconCache = new Map<string, L.DivIcon>()

/** Map pin for a nearby place: the category icon in a ring of its colour. */
export function aroundIcon(category: AroundCategory, onRoute = false): L.DivIcon {
  const key = `${category}:${onRoute}`
  let icon = iconCache.get(key)
  if (!icon) {
    const { color } = aroundCategory(category)
    icon = L.divIcon({
      className: 'around-marker',
      html: `<span class="around-marker-inner${onRoute ? ' on-route' : ''}" style="color:${color};border-color:${color}">${mapIconMarkup(category, 13)}</span>`,
      iconSize: [26, 26],
      iconAnchor: [13, 13],
    })
    iconCache.set(key, icon)
  }
  return icon
}

export function RouteBadge({ offset }: { offset?: RouteOffset }) {
  if (!offset || offset.offsetM > NEAR_ROUTE_M) return null
  return offset.offsetM <= ON_ROUTE_M ? (
    <span className="route-badge on-route">On route · km {offset.atKm.toFixed(1)}</span>
  ) : (
    <span className="route-badge">{formatDistance(offset.offsetM)} off route · km {offset.atKm.toFixed(0)}</span>
  )
}

export function AroundResults({
  state,
  hidden,
  offsets,
  sort,
  onSort,
  onToggleCategory,
  onRadius,
  onRetry,
  onFocus,
  onKeep,
  onCancel,
}: {
  state: AroundState
  hidden: ReadonlySet<AroundCategory>
  /** Present once there is a planned route to compare against. */
  offsets: Map<string, RouteOffset> | null
  sort: AroundSort
  onSort: (sort: AroundSort) => void
  onToggleCategory: (id: AroundCategory) => void
  onRadius: (km: number) => void
  onRetry: () => void
  onFocus: (poi: AroundPoi) => void
  /** Omitted where there is no document to keep a place in. */
  onKeep?: (poi: AroundPoi) => void
  onCancel: () => void
}) {
  const pois = useMemo(() => state.result?.pois ?? [], [state.result])
  const counts = useMemo(() => {
    const byCategory = new Map<AroundCategory, number>()
    for (const poi of pois) byCategory.set(poi.category, (byCategory.get(poi.category) ?? 0) + 1)
    return byCategory
  }, [pois])
  const byRoute = sort === 'route' && offsets !== null
  const visible = useMemo(() => {
    const shown = pois.filter(poi => !hidden.has(poi.category))
    if (!byRoute || !offsets) return shown
    // Along the route: places you can actually stop at, in riding order.
    return shown
      .filter(poi => (offsets.get(poi.id)?.offsetM ?? Infinity) <= NEAR_ROUTE_M)
      .sort((a, b) => offsets.get(a.id)!.atKm - offsets.get(b.id)!.atKm)
  }, [byRoute, hidden, offsets, pois])
  const onRouteCount = offsets ? pois.filter(poi => (offsets.get(poi.id)?.offsetM ?? Infinity) <= ON_ROUTE_M).length : 0

  return (
    <div className="insight">
      <div className="segmented" role="group" aria-label="Search radius">
        {AROUND_RADII_KM.map(km => (
          <button
            key={km}
            className={state.radiusKm === km ? 'active' : undefined}
            aria-pressed={state.radiusKm === km}
            onClick={() => onRadius(km)}
            disabled={state.loading}
          >
            {km} km
          </button>
        ))}
      </div>

      {state.loading ? (
        <div className="drawer-loading" role="status">
          <span className="drawer-spinner" aria-hidden="true" />
          <span>
            <strong>Asking OpenStreetMap</strong>
            <span>A busy server can take up to a minute.</span>
          </span>
          <button className="drawer-loading-cancel" onClick={onCancel}>Cancel</button>
        </div>
      ) : state.cancelled ? (
        <div className="drawer-callout tone-neutral" role="status">
          <MapIcon id="search" size={18} />
          <span>
            <strong>Search cancelled</strong>
            <span>The search was stopped before any results arrived.</span>
            <button className="insight-link" onClick={onRetry}>Search again</button>
          </span>
        </div>
      ) : state.error ? (
        <div className="drawer-callout tone-warn" role="status">
          <MapIcon id="search" size={18} />
          <span>
            <strong>Search failed</strong>
            <span>{state.error}</span>
            <button className="insight-link" onClick={onRetry}>Try again</button>
          </span>
        </div>
      ) : state.result && (
        <>
          <div className="chip-row">
            {AROUND_CATEGORIES.map(category => {
              const count = counts.get(category.id) ?? 0
              const on = !hidden.has(category.id) && count > 0
              return (
                <button
                  key={category.id}
                  className={`chip${on ? ' active' : ''}`}
                  style={on ? { color: category.color, borderColor: category.color } : undefined}
                  onClick={() => onToggleCategory(category.id)}
                  disabled={count === 0}
                  aria-pressed={on}
                >
                  <MapIcon id={category.id} size={14} />
                  <span className="chip-label">{category.label}</span>
                  <span className="chip-count">{count}</span>
                </button>
              )
            })}
          </div>

          {offsets && (
            <div className="list-toolbar">
              <span>{onRouteCount > 0 ? `${onRouteCount} on the route` : 'None directly on the route'}</span>
              <div className="segmented segmented--small" role="group" aria-label="Sort">
                <button className={sort === 'nearest' ? 'active' : undefined} onClick={() => onSort('nearest')}>Nearest</button>
                <button className={sort === 'route' ? 'active' : undefined} onClick={() => onSort('route')}>Along route</button>
              </div>
            </div>
          )}

          {visible.length === 0 ? (
            <p className="insight-empty">
              {byRoute
                ? `Nothing within ${formatDistance(NEAR_ROUTE_M)} of the route here.`
                : `Nothing mapped in OpenStreetMap within ${state.result.radiusKm} km.`}
            </p>
          ) : (
            <ul className="place-list">
              {visible.slice(0, LIST_LIMIT).map(poi => {
                const category = aroundCategory(poi.category)
                const facts = poi.facts.slice(0, 2).map(fact => `${fact.label}: ${fact.value}`).join(' · ')
                return (
                  <li key={poi.id}>
                    <button className="place-row" onClick={() => onFocus(poi)} title="Show on the map">
                      <span className="place-icon" style={{ color: category.color, background: `${category.color}1a` }}>
                        <MapIcon id={poi.category} size={16} />
                      </span>
                      <span className="place-text">
                        <span className="place-name">{poi.name ?? poi.kind}</span>
                        <span className="place-meta">{poi.name ? poi.kind : ''}{poi.name && facts ? ' · ' : ''}{facts}</span>
                        <RouteBadge offset={offsets?.get(poi.id)} />
                      </span>
                      <span className="place-distance">{formatDistance(poi.distanceM)}</span>
                    </button>
                    {onKeep && (
                      <button
                        className="place-keep"
                        onClick={() => onKeep(poi)}
                        title="Keep as a place in this GPX (not routed through)"
                        aria-label={`Keep ${poi.name ?? poi.kind} as a place`}
                      >
                        +
                      </button>
                    )}
                  </li>
                )
              })}
            </ul>
          )}
          {visible.length > LIST_LIMIT && (
            <p className="insight-note">Nearest {LIST_LIMIT} of {visible.length} listed; all are on the map.</p>
          )}
          {state.result.truncated && (
            <div className="drawer-callout tone-warn">
              <MapIcon id="search" size={18} />
              <span>
                <strong>Some places may be missing</strong>
                <span>OpenStreetMap returned its maximum for this area. Try a smaller radius.</span>
              </span>
            </div>
          )}
          <p className="insight-note">
            Distances are straight-line from point #{state.pointNumber}. Only what OpenStreetMap says: a spring is not
            drinkable unless tagged so, and huts may be locked or private.
            {state.result.reused ? ' Answered from this session, no new request.' : ''}
          </p>
        </>
      )}
    </div>
  )
}

export function AroundRadius({ result }: { result: AroundSearchResult }) {
  return (
    <Circle
      center={[result.center.lat, result.center.lon]}
      radius={result.radiusKm * 1000}
      pathOptions={{ color: '#2563eb', weight: 1.5, dashArray: '6 6', fillOpacity: 0.04, interactive: false }}
    />
  )
}
