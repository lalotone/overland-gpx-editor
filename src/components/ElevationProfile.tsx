import {
  ALTITUDE_GRADIENT_CSS,
  SLOPE_COLORS,
  SLOPE_LABELS,
} from '../lib/terrain'
import type { ColorMode } from '../lib/terrain'
import type { ElevationStats } from '../lib/types'

export interface ProfileBar {
  /** Index into the full-resolution coordinate array. */
  coordIndex: number
  elevation: number
  elevDelta: number
  slope: number
  distKm: number
  /** 0-100, relative to the track's own elevation range. */
  barHeight: number
  color: string
}

export interface TrimSelection {
  startIdx: number
  endIdx: number
}

export function ElevationProfile({
  bars,
  stats,
  yAxisTicks,
  colorMode,
  collapsed,
  onToggleCollapsed,
  hoveredBar,
  onHoverBar,
  activeCoordIndex,
  onSelectBar,
  selectionMode,
  selection,
  interpolatedElevation,
}: {
  bars: ProfileBar[]
  stats: ElevationStats
  yAxisTicks: number[]
  colorMode: ColorMode
  collapsed: boolean
  onToggleCollapsed: () => void
  hoveredBar: number | null
  onHoverBar: (index: number | null) => void
  activeCoordIndex: number | null
  onSelectBar: (bar: ProfileBar) => void
  selectionMode: boolean
  selection: TrimSelection | null
  interpolatedElevation?: boolean
}) {
  // Selection is stored as coordinate indices; find where those fall among bars.
  const selectionRange = (() => {
    if (!selection || bars.length === 0) return null
    const lo = Math.min(selection.startIdx, selection.endIdx)
    const hi = Math.max(selection.startIdx, selection.endIdx)
    let start = bars.findIndex(b => b.coordIndex >= lo)
    let end = bars.findIndex(b => b.coordIndex > hi)
    if (start < 0) start = bars.length - 1
    if (end < 0) end = bars.length
    return { leftPct: (start / bars.length) * 100, widthPct: ((end - start) / bars.length) * 100 }
  })()

  return (
    <div className={`elevation-profile ${collapsed ? 'collapsed' : ''}`}>
      <div className="elevation-profile-header" onClick={onToggleCollapsed}>
        <div className="header-content">
          <svg className="header-icon" viewBox="0 0 24 24" width="16" height="16" fill="currentColor">
            <path d="M16 6l2.29 2.29-4.88 4.88-4-4L2 16.59 3.41 18l6-6 4 4 6.3-6.29L22 12V6z" />
          </svg>
          <span className="header-title">Elevation Profile</span>
          {collapsed && <span className="expand-hint">— click to expand</span>}
          <div className="elevation-profile-stats">
            <span className="ep-stat">
              <span className="ep-stat-label">Min</span>
              <span className="ep-stat-value">{stats.min.toFixed(0)}m</span>
            </span>
            <span className="ep-stat-divider" />
            <span className="ep-stat">
              <span className="ep-stat-label">Max</span>
              <span className="ep-stat-value">{stats.max.toFixed(0)}m</span>
            </span>
            <span className="ep-stat-divider" />
            <span className="ep-stat">
              <span className="ep-stat-label">Gain</span>
              <span className="ep-stat-value ep-gain">+{stats.gain.toFixed(0)}m</span>
            </span>
            <span className="ep-stat-divider" />
            <span className="ep-stat">
              <span className="ep-stat-label">Loss</span>
              <span className="ep-stat-value ep-loss">-{stats.loss.toFixed(0)}m</span>
            </span>
          </div>
        </div>
        <span className="collapse-icon">
          <svg
            viewBox="0 0 24 24" width="16" height="16" fill="none"
            stroke="currentColor" strokeWidth="2.5" strokeLinecap="round" strokeLinejoin="round"
            style={{
              transform: collapsed ? 'rotate(0deg)' : 'rotate(180deg)',
              transition: 'transform 0.32s cubic-bezier(0.4,0,0.2,1)',
            }}
          >
            <polyline points="6 9 12 15 18 9" />
          </svg>
        </span>
      </div>

      {!collapsed && (
        <>
          {interpolatedElevation && (
            <div className="elevation-note">
              Track is long enough that elevation was sampled and interpolated between measured points.
            </div>
          )}

          <div className="elevation-chart-area">
            <div className="elevation-y-axis">
              {yAxisTicks.slice().reverse().map((tick, i) => (
                <span key={i} className="y-tick">{tick}</span>
              ))}
            </div>

            <div
              className={`elevation-bars-wrapper${selectionMode ? ' selecting' : ''}`}
              onMouseLeave={() => onHoverBar(null)}
            >
              <div className="elevation-grid-lines">
                {yAxisTicks.map((_, i) => <div key={i} className="grid-line" />)}
              </div>

              {selectionRange && (
                <div
                  className="elevation-selection"
                  style={{ left: `${selectionRange.leftPct}%`, width: `${selectionRange.widthPct}%` }}
                />
              )}

              <div className="elevation-bars">
                {bars.map((bar, barIdx) => (
                  <div
                    key={bar.coordIndex}
                    className={`elevation-bar${activeCoordIndex === bar.coordIndex ? ' active' : ''}${
                      hoveredBar === barIdx ? ' hovered' : ''
                    }`}
                    style={{
                      flex: `0 0 ${100 / bars.length}%`,
                      height: `${bar.barHeight}%`,
                      '--bar-color': bar.color,
                    } as React.CSSProperties}
                    onClick={() => onSelectBar(bar)}
                    onMouseEnter={() => onHoverBar(barIdx)}
                  />
                ))}
              </div>

              {hoveredBar !== null && bars[hoveredBar] && (
                <div
                  className="elevation-tooltip"
                  style={{ left: `${((hoveredBar + 0.5) / bars.length) * 100}%` }}
                >
                  <span className="tooltip-elevation">{bars[hoveredBar].elevation.toFixed(0)} m</span>
                  <span className="tooltip-distance">{bars[hoveredBar].distKm.toFixed(2)} km</span>
                  {hoveredBar > 0 && (
                    <span
                      className="tooltip-delta"
                      style={{ color: bars[hoveredBar].elevDelta >= 0 ? '#16a34a' : '#ef4444' }}
                    >
                      {bars[hoveredBar].elevDelta >= 0 ? '+' : ''}
                      {bars[hoveredBar].elevDelta.toFixed(1)} m
                    </span>
                  )}
                  <span className="tooltip-slope">
                    {bars[hoveredBar].slope > 0 ? '+' : ''}{bars[hoveredBar].slope.toFixed(1)}%
                  </span>
                </div>
              )}
            </div>
          </div>

          <div className="elevation-x-axis">
            {bars.length > 0 && [0, 0.25, 0.5, 0.75, 1].map((pct, i) => {
              const idx = Math.min(Math.floor(pct * (bars.length - 1)), bars.length - 1)
              return <span key={i} className="x-tick">{bars[idx].distKm.toFixed(1)} km</span>
            })}
          </div>

          {colorMode === 'slope' ? (
            <div className="elevation-legend">
              {SLOPE_LABELS.map((label, i) => (
                <div key={i} className="legend-item">
                  <span className="legend-color" style={{ backgroundColor: SLOPE_COLORS[i] }} />
                  <span className="legend-text">{label}</span>
                </div>
              ))}
            </div>
          ) : (
            <div className="elevation-legend elevation-legend--altitude">
              <span className="legend-text">{stats.min.toFixed(0)} m</span>
              <span className="altitude-scale" style={{ background: ALTITUDE_GRADIENT_CSS }} />
              <span className="legend-text">{stats.max.toFixed(0)} m</span>
            </div>
          )}
        </>
      )}
    </div>
  )
}
