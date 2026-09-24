import { useRef } from 'react'
import { ROUTING_PROFILES } from '../../../src/lib/routing'
import type { Plan } from './model'
import Icon from './Icon'

export function PlanHandle({
  open,
  onOpen,
  count,
}: {
  open: boolean
  onOpen: (open: boolean) => void
  count: number
}) {
  const startY = useRef<number | null>(null)
  return (
    <button
      className={open ? 'sheet-handle plan-handle' : 'plan-drawer-tab plan-handle'}
      aria-label={open ? 'Hide route points' : 'Show route points'}
      aria-expanded={open}
      onPointerDown={(event) => {
        startY.current = event.clientY
        event.currentTarget.setPointerCapture(event.pointerId)
      }}
      onPointerCancel={() => {
        startY.current = null
      }}
      onPointerUp={(event) => {
        if (startY.current === null) return
        const delta = event.clientY - startY.current
        startY.current = null
        onOpen(Math.abs(delta) > 24 ? delta < 0 : !open)
      }}
      onClick={(event) => {
        if (event.detail === 0) onOpen(!open)
      }}
    >
      <span className="drawer-grip" />
      {!open && <small>Route points · {count}</small>}
    </button>
  )
}

export default function PlanPanel({
  plan,
  onChange,
  onOpen,
  ready,
  busy,
  onSave,
  onShare,
  onNew,
}: {
  plan: Plan
  onChange: (plan: Plan) => void
  onOpen: (open: boolean) => void
  ready: boolean
  busy: boolean
  onSave: () => void
  onShare: () => void
  onNew: () => void
}) {
  return (
    <section className="bottom-sheet plan-panel" aria-label="Route points">
      <PlanHandle open onOpen={onOpen} count={plan.points.length} />
      <div className="plan-fixed-header">
        <div className="profile-selector" aria-label="Riding profile">
          {ROUTING_PROFILES.map((profile) => (
            <button
              key={profile.id}
              className={plan.profile === profile.id ? 'selected' : ''}
              onClick={() => onChange({ ...plan, profile: profile.id })}
            >
              {profile.label}
            </button>
          ))}
        </div>
        <label className="permit-check">
          <input
            type="checkbox"
            checked={plan.permit}
            onChange={(event) => onChange({ ...plan, permit: event.target.checked })}
          />
          Restricted access
        </label>
      </div>
      <div className="route-controls-list" aria-label="Route control points">
        {!plan.points.length && (
          <p className="muted plan-empty">
            Move the map to your start and tap +. Drag numbered points to adjust your route.
          </p>
        )}
        {plan.points.map((point, index) => (
          <div className="route-control" key={index}>
            <span className="point-number">{index + 1}</span>
            <span>
              {index === 0 ? 'Start' : index === plan.points.length - 1 ? 'Finish' : 'Via'}
              <small>
                {point.lat.toFixed(4)}, {point.lon.toFixed(4)}
              </small>
            </span>
            <button
              className="icon-button"
              aria-label={`Remove point ${index + 1}`}
              onClick={() =>
                onChange({ ...plan, points: plan.points.filter((_, i) => i !== index) })
              }
            >
              <Icon name="close" size={17} />
            </button>
          </div>
        ))}
      </div>
      <div className="plan-fixed-footer">
        <div className="button-pair">
          <button className="primary" disabled={!ready || busy} onClick={onSave}>
            <Icon name="save" size={18} />
            Save ride
          </button>
          <button className="secondary" disabled={!ready || busy} onClick={onShare}>
            <Icon name="share" size={18} />
            Share
          </button>
        </div>
        {!!plan.points.length && (
          <div className="button-pair">
            <button
              className="text-button"
              onClick={() => onChange({ ...plan, points: plan.points.slice(0, -1) })}
            >
              <Icon name="undo" size={16} />
              Undo point
            </button>
            <button className="text-button" onClick={onNew}>
              New route
            </button>
          </div>
        )}
      </div>
    </section>
  )
}
