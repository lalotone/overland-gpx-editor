import { useMemo } from 'react'
import { Circle } from 'react-leaflet'
import { AROUND_CATEGORIES, AROUND_RADII_KM, aroundCategory } from '../lib/around'
import type { AroundCategory, AroundPoi, AroundSearchResult } from '../lib/around'
import { useEscapeDismiss, ESCAPE_PRIORITY } from './useEscapeDismiss'

export interface AroundState {
  /** Route point number shown in the header, 1-based. */
  pointNumber: number
  radiusKm: number
  loading: boolean
  error?: string
  result?: AroundSearchResult
}

const LIST_LIMIT = 80

export function formatDistance(meters: number): string {
  return meters < 1000 ? `${Math.round(meters)} m` : `${(meters / 1000).toFixed(meters < 10_000 ? 1 : 0)} km`
}

/** OSM object page, so a result can be checked against the source. */
export function osmLink(poi: AroundPoi): string {
  const [type, id] = poi.id.split('/')
  return `https://www.openstreetmap.org/${type}/${id}`
}

export function AroundPanel({
  state,
  hidden,
  onToggleCategory,
  onRadius,
  onRetry,
  onFocus,
  onKeep,
  onClose,
}: {
  state: AroundState
  hidden: ReadonlySet<AroundCategory>
  onToggleCategory: (id: AroundCategory) => void
  onRadius: (km: number) => void
  onRetry: () => void
  onFocus: (poi: AroundPoi) => void
  onKeep: (poi: AroundPoi) => void
  onClose: () => void
}) {
  useEscapeDismiss(true, onClose, ESCAPE_PRIORITY.panel)
  const pois = state.result?.pois ?? []
  const counts = useMemo(() => {
    const byCategory = new Map<AroundCategory, number>()
    for (const poi of pois) byCategory.set(poi.category, (byCategory.get(poi.category) ?? 0) + 1)
    return byCategory
  }, [pois])
  const visible = pois.filter(poi => !hidden.has(poi.category))

  return (
    <div className="around-panel" role="dialog" aria-label={`What's around route point ${state.pointNumber}`}>
      <div className="around-header">
        <strong>Around point #{state.pointNumber}</strong>
        <button className="terrain-close" onClick={onClose} aria-label="Close nearby results" title="Close (Esc)">
          &times;
        </button>
      </div>

      <div className="around-radius" role="group" aria-label="Search radius">
        {AROUND_RADII_KM.map(km => (
          <button
            key={km}
            className={`creation-poi-btn${state.radiusKm === km ? ' active' : ''}`}
            onClick={() => onRadius(km)}
            disabled={state.loading}
          >
            {km} km
          </button>
        ))}
      </div>

      {state.loading ? (
        <p className="around-note" role="status">Asking OpenStreetMap… a busy Overpass server can take a minute.</p>
      ) : state.error ? (
        <p className="around-note around-error" role="status">
          {state.error}{' '}
          <button className="btn btn-ghost btn-xs" onClick={onRetry}>Retry</button>
        </p>
      ) : state.result && (
        <>
          <div className="around-categories">
            {AROUND_CATEGORIES.map(category => {
              const count = counts.get(category.id) ?? 0
              return (
                <button
                  key={category.id}
                  className={`creation-poi-btn${!hidden.has(category.id) && count > 0 ? ' active' : ''}`}
                  onClick={() => onToggleCategory(category.id)}
                  disabled={count === 0}
                  aria-pressed={!hidden.has(category.id)}
                  title={category.label}
                >
                  {category.glyph} {category.label}
                  <span className="creation-poi-count">{count}</span>
                </button>
              )
            })}
          </div>

          {visible.length === 0 ? (
            <p className="around-note">Nothing mapped in OpenStreetMap within {state.result.radiusKm} km.</p>
          ) : (
            <ul className="around-list">
              {visible.slice(0, LIST_LIMIT).map(poi => {
                const category = aroundCategory(poi.category)
                return (
                  <li key={poi.id}>
                    <button className="around-item" onClick={() => onFocus(poi)} title="Show on the map">
                      <span className="around-glyph" style={{ borderColor: category.color }}>{category.glyph}</span>
                      <span className="around-item-text">
                        <span className="around-item-name">{poi.name ?? poi.kind}</span>
                        <span className="around-item-facts">
                          {[poi.name ? poi.kind : undefined, ...poi.facts.slice(0, 2).map(fact => `${fact.label}: ${fact.value}`)]
                            .filter(Boolean).join(' · ')}
                        </span>
                      </span>
                      <span className="around-distance">{formatDistance(poi.distanceM)}</span>
                    </button>
                    <button
                      className="around-keep"
                      onClick={() => onKeep(poi)}
                      title="Keep as a place in this GPX (not routed through)"
                      aria-label={`Keep ${poi.name ?? poi.kind} as a place`}
                    >
                      +
                    </button>
                  </li>
                )
              })}
            </ul>
          )}
          {visible.length > LIST_LIMIT && (
            <p className="around-note">Nearest {LIST_LIMIT} of {visible.length} listed; all are on the map.</p>
          )}
          {state.result.truncated && (
            <p className="around-note around-error">
              OpenStreetMap returned its maximum number of places for this area, so some may be missing. Try a smaller radius.
            </p>
          )}
          <p className="around-note">
            Straight-line distances. From OpenStreetMap tags only: a spring is not drinkable unless tagged so,
            and huts may be locked or private.{state.result.reused ? ' Answered from this session — no new request.' : ''}
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
