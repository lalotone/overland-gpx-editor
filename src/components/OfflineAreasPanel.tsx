import { useCallback, useMemo, useState } from 'react'
import { boundingBoxSpanKm } from '../lib/poi'
import { cancelPack, deletePack, formatBytes } from '../lib/offline'
import type { PackSummary, RuntimeConfig } from '../lib/offline'
import { ESCAPE_PRIORITY, useEscapeDismiss } from './useEscapeDismiss'

const ACTIVE_STATES = new Set(['queued', 'running', 'cancelling'])

function areaName(pack: PackSummary): string {
  return pack.name?.replace(/^Map:\s*/i, '').trim() || 'Downloaded area'
}

function areaDate(pack: PackSummary): string {
  const date = new Date(pack.updatedAt ?? pack.createdAt ?? '')
  return Number.isFinite(date.getTime()) ? date.toLocaleDateString() : 'Date unavailable'
}

export function OfflineAreasPanel({
  runtime,
  packs,
  coverageVisible,
  onCoverageVisible,
  onViewArea,
  onPacksChanged,
  loadError,
}: {
  runtime: RuntimeConfig
  packs: PackSummary[]
  coverageVisible: boolean
  onCoverageVisible: (visible: boolean) => void
  onViewArea: (pack: PackSummary) => void
  onPacksChanged: () => Promise<void> | void
  loadError?: string
}) {
  const [open, setOpen] = useState(false)
  const [busyID, setBusyID] = useState<string | null>(null)
  const [confirmID, setConfirmID] = useState<string | null>(null)
  const [error, setError] = useState('')
  const areas = useMemo(() => packs.filter(pack => pack.bbox), [packs])
  const completed = areas.filter(pack => pack.status === 'complete').length
  const closePanel = useCallback(() => {
    setConfirmID(null)
    setOpen(false)
  }, [])
  useEscapeDismiss(open, closePanel, ESCAPE_PRIORITY.panel)
  useEscapeDismiss(confirmID !== null, () => setConfirmID(null), ESCAPE_PRIORITY.nested)

  const handleCancel = async (pack: PackSummary) => {
    setBusyID(pack.id)
    setError('')
    try {
      await cancelPack(runtime, pack.id)
      await onPacksChanged()
    } catch (reason) {
      setError((reason as Error).message)
    } finally {
      setBusyID(null)
    }
  }

  const handleDelete = async (pack: PackSummary) => {
    setBusyID(pack.id)
    setError('')
    try {
      await deletePack(runtime, pack.id)
      setConfirmID(null)
      await onPacksChanged()
    } catch (reason) {
      setError((reason as Error).message)
    } finally {
      setBusyID(null)
    }
  }

  return (
    <div className="offline-areas-control">
      <div className="offline-areas-actions">
        <button
          className="offline-storage-fab offline-areas-trigger"
          type="button"
          onClick={() => { if (open) closePanel(); else setOpen(true) }}
          aria-expanded={open}
          aria-controls="offline-areas-panel"
          aria-label={`Downloaded areas (${areas.length})`}
          title="Manage downloaded map areas"
          data-testid="offline-areas-button"
        >
          <span className="offline-area-swatch" aria-hidden="true" />
          <span>Downloaded areas ({areas.length})</span>
        </button>
        <button
          className="offline-coverage-toggle"
          type="button"
          disabled={completed === 0}
          onClick={() => onCoverageVisible(!coverageVisible)}
          aria-pressed={coverageVisible}
          aria-label={coverageVisible ? 'Hide downloaded areas' : 'Show downloaded areas'}
          title={coverageVisible ? 'Hide downloaded areas' : 'Show downloaded areas'}
        >
          <svg viewBox="0 0 24 24" aria-hidden="true">
            {coverageVisible
              ? <><path d="M2 12s3.5-6 10-6 10 6 10 6-3.5 6-10 6S2 12 2 12Z" /><circle cx="12" cy="12" r="2.8" /></>
              : <><path d="m3 3 18 18M10.6 6.1A11 11 0 0 1 12 6c6.5 0 10 6 10 6a14.8 14.8 0 0 1-3 3.6M6.2 7.6C3.4 9.5 2 12 2 12s3.5 6 10 6a10.8 10.8 0 0 0 3-.4" /></>}
          </svg>
        </button>
      </div>

      {open && (
        <aside id="offline-areas-panel" className="offline-areas-panel" role="region" aria-label="Downloaded areas">
          <header className="offline-panel-header">
            <div><strong>Downloaded areas</strong><span>{completed} ready</span></div>
            <button type="button" onClick={closePanel} aria-label="Close downloaded areas">&times;</button>
          </header>

          <label className="offline-coverage-switch">
            <span><strong>Show coverage</strong><small>Lightly tint areas already stored</small></span>
            <input
              type="checkbox"
              checked={coverageVisible}
              disabled={completed === 0}
              onChange={event => onCoverageVisible(event.target.checked)}
            />
          </label>

          {(error || loadError) && <p className="offline-error" role="alert">{error || loadError}</p>}

          <div className="offline-areas-list">
            {areas.length === 0 && (
              <div className="offline-areas-empty">
                <strong>No downloaded areas yet</strong>
                <p>Draw an area from Offline maps to keep it available without a connection.</p>
              </div>
            )}
            {areas.map(pack => {
              const name = areaName(pack)
              const span = pack.bbox ? boundingBoxSpanKm(pack.bbox) : null
              const active = ACTIVE_STATES.has(pack.status)
              const percent = pack.total ? Math.min(100, Math.round((pack.done ?? 0) / pack.total * 100)) : 0
              return (
                <article key={pack.id} className={`offline-area-entry is-${pack.status}`}>
                  <button
                    className="offline-area-entry-map"
                    type="button"
                    aria-label={`View ${name} on map`}
                    onClick={() => { onViewArea(pack); closePanel() }}
                  >
                    <span className="offline-area-entry-head">
                      <span className="offline-area-swatch" aria-hidden="true" />
                      <span><strong>{name}</strong><small>{span ? `${span.widthKm.toFixed(0)} × ${span.heightKm.toFixed(0)} km` : 'Area bounds unavailable'}</small></span>
                      <em>{active ? `${percent}%` : pack.status}</em>
                    </span>
                    <span className="offline-area-entry-meta">
                      <span>{formatBytes(pack.bytes)}</span>
                      <span>{(pack.done ?? 0).toLocaleString()} / {(pack.total ?? 0).toLocaleString()} resources</span>
                      <span>{areaDate(pack)}</span>
                    </span>
                    {active && <span className="offline-meter"><span style={{ width: `${percent}%` }} /></span>}
                  </button>
                  <div className="offline-area-entry-actions">
                    {active ? (
                      <button className="btn btn-ghost btn-xs" disabled={busyID !== null} onClick={() => void handleCancel(pack)}>
                        {busyID === pack.id ? 'Cancelling…' : `Cancel ${name}`}
                      </button>
                    ) : confirmID === pack.id ? (
                      <>
                        <button className="btn btn-danger btn-xs" disabled={busyID !== null} onClick={() => void handleDelete(pack)}>Confirm removal</button>
                        <button className="btn btn-ghost btn-xs" disabled={busyID !== null} onClick={() => setConfirmID(null)}>Keep area</button>
                      </>
                    ) : (
                      <button
                        className="offline-area-delete-button"
                        type="button"
                        disabled={busyID !== null}
                        aria-label={`Remove ${name}`}
                        title={`Remove ${name}`}
                        onClick={() => setConfirmID(pack.id)}
                      >
                        <svg viewBox="0 0 24 24" aria-hidden="true">
                          <path d="M4 7h16M9 7V4h6v3m3 0-1 13H7L6 7m4 4v5m4-5v5" />
                        </svg>
                      </button>
                    )}
                  </div>
                </article>
              )
            })}
          </div>
        </aside>
      )}
    </div>
  )
}
