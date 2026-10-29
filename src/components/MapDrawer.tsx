import type { ReactNode } from 'react'
import { MapIcon } from './mapIcons'
import type { MapIconId } from './mapIcons'
import { ESCAPE_PRIORITY, useEscapeDismiss } from './useEscapeDismiss'

export interface DrawerTab<T extends string> {
  id: T
  label: string
  /** Short status beside the label: a count, a share, a warning. */
  badge?: string
  tone?: 'neutral' | 'warn' | 'danger' | 'ok'
}

/**
 * A panel docked to the right edge of the map, beside it rather than on top
 * of the route. The map's own right-hand controls move aside while it is open
 * (see `.has-map-drawer` in App.css); on narrow screens it becomes a bottom
 * sheet.
 */
export function MapDrawer<T extends string>({
  icon,
  title,
  subtitle,
  tabs,
  activeTab,
  onTab,
  onClose,
  children,
  label,
}: {
  icon: MapIconId
  title: string
  subtitle?: ReactNode
  tabs?: DrawerTab<T>[]
  activeTab?: T
  onTab?: (id: T) => void
  onClose: () => void
  children: ReactNode
  label: string
}) {
  useEscapeDismiss(true, onClose, ESCAPE_PRIORITY.panel)
  return (
    <aside className="map-drawer" aria-label={label}>
      <header className="map-drawer-header">
        <span className="map-drawer-icon"><MapIcon id={icon} size={18} /></span>
        <span className="map-drawer-titles">
          <span className="map-drawer-title">{title}</span>
          {subtitle && <span className="map-drawer-subtitle">{subtitle}</span>}
        </span>
        <button className="map-drawer-close" onClick={onClose} aria-label={`Close ${label}`} title="Close (Esc)">
          <MapIcon id="close" size={16} />
        </button>
      </header>
      {tabs && tabs.length > 0 && (
        <nav className="map-drawer-tabs" role="tablist">
          {tabs.map(tab => (
            <button
              key={tab.id}
              role="tab"
              aria-selected={activeTab === tab.id}
              className={`map-drawer-tab${activeTab === tab.id ? ' active' : ''}`}
              onClick={() => onTab?.(tab.id)}
            >
              <span>{tab.label}</span>
              {tab.badge && <span className={`map-drawer-badge tone-${tab.tone ?? 'neutral'}`}>{tab.badge}</span>}
            </button>
          ))}
        </nav>
      )}
      <div className="map-drawer-body" role={tabs ? 'tabpanel' : undefined}>
        {children}
      </div>
    </aside>
  )
}

/** A titled block inside a drawer or sidebar. */
export function DrawerSection({ title, aside, children }: { title?: string; aside?: ReactNode; children: ReactNode }) {
  return (
    <section className="drawer-section">
      {(title || aside) && (
        <div className="drawer-section-head">
          {title && <h4>{title}</h4>}
          {aside}
        </div>
      )}
      {children}
    </section>
  )
}

/** A big number with its unit and a caption, the way a dashboard tile reads. */
export function Metric({ value, unit, caption, tone }: { value: string; unit?: string; caption: string; tone?: 'warn' | 'danger' | 'ok' }) {
  return (
    <div className={`drawer-metric${tone ? ` tone-${tone}` : ''}`}>
      <span className="drawer-metric-value">{value}{unit && <small>{unit}</small>}</span>
      <span className="drawer-metric-caption">{caption}</span>
    </div>
  )
}
