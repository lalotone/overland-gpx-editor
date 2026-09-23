import { useState } from 'react'
import type { RuntimeConfig, RoutingDataStatus } from '../../../src/lib/offline'
import type { BoundingBox } from '../../../src/lib/poi'
import Icon from './Icon'
import { useDownloads, resourceTransferText } from './downloads'
import RegionBrowser from './RegionBrowser'

export default function OfflineMapControl({
  runtime,
  status,
  refresh,
  bounds,
  notify,
}: {
  runtime: RuntimeConfig
  status: RoutingDataStatus | null
  refresh: () => void
  bounds: BoundingBox
  notify: (message: string) => void
}) {
  const [browserOpen, setBrowserOpen] = useState(false)
  const downloads = useDownloads(runtime, status, refresh, notify)
  const { preparing, phase, downloading, routingBusy, active } = downloads
  const offline = runtime.offline?.mode === 'cache-only'
  const total = active.reduce((sum, pack) => sum + (pack.total || 0), 0)
  const detail = preparing
    ? phase
    : routingBusy
      ? `Routing · ${status?.job?.phase || 'starting'}`
      : active.length
        ? total
          ? `Preparing resources · ${active.reduce((sum, pack) => sum + (pack.done || 0), 0)} / ${total}`
          : 'Preparing maps…'
        : offline
          ? 'Go online to download'
          : 'All resources for this view'
  return (
    <>
      <div className="offline-map-control download-split">
        <button
          className="download-area"
          disabled={preparing || (!downloading && offline)}
          onClick={() =>
            void (downloading
              ? downloads.cancel()
              : downloads.start({ id: 'visible-area', name: 'Visible area', kind: 'area', bounds }))
          }
        >
          {preparing || downloading ? (
            <span className="spinner" />
          ) : (
            <Icon name="offline" size={22} />
          )}
          <span>
            <strong>
              {downloading
                ? 'Stop downloads'
                : preparing
                  ? 'Preparing download'
                  : 'Download this area'}
            </strong>
            <small>{detail}</small>
            {!preparing && active.length === 1 && resourceTransferText(active[0].resources['vector-map']) && (
              <small>Maps · {resourceTransferText(active[0].resources['vector-map'])}</small>
            )}
          </span>
        </button>
        <button
          className="download-region-link"
          aria-label="Download region"
          title="Download region"
          onClick={() => setBrowserOpen(true)}
        >
          <Icon name="explore" size={24} />
          <Icon name="arrow" size={20} />
        </button>
      </div>
      {browserOpen && (
        <RegionBrowser
          runtime={runtime}
          status={status}
          downloads={downloads}
          notify={notify}
          onClose={() => setBrowserOpen(false)}
        />
      )}
    </>
  )
}
