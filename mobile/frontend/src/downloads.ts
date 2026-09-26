import { useCallback, useEffect, useRef, useState } from 'react'
import type { RuntimeConfig, RoutingDataStatus, PackSummary } from '../../../src/lib/offline'
import { cancelRoutingData, prepareRoutingData, fetchPacks, cancelPack } from '../../../src/lib/offline'
import { packFailure, startRegionDownload } from '../../../src/lib/offlineRegions'
import type { DownloadArea, DownloadResource } from '../../../src/lib/offlineRegions'

export {
  RESOURCE_LABELS,
  resourceTransferText,
  coverageLabel,
  boundsLabel,
  coversBounds,
  packFailure,
} from '../../../src/lib/offlineRegions'
export type { DownloadArea, DownloadRegion } from '../../../src/lib/offlineRegions'

export function useDownloads(
  runtime: RuntimeConfig,
  status: RoutingDataStatus | null,
  refresh: () => void,
  notify: (message: string) => void,
) {
  const [packs, setPacks] = useState<PackSummary[]>([])
  const [preparing, setPreparing] = useState(false)
  const [phase, setPhase] = useState('')
  const [error, setError] = useState('')
  const [target, setTarget] = useState<{
    area: DownloadArea
    region?: string
    pack?: string
  } | null>(null)
  const notified = useRef(new Set<string>())
  const active = packs.filter((pack) => pack.status === 'running' || pack.status === 'queued')
  const routingBusy = status?.job?.state === 'queued' || status?.job?.state === 'running'
  const downloading = routingBusy || active.length > 0

  useEffect(() => {
    if (!runtime.offline) return
    const controller = new AbortController()
    const poll = () =>
      void fetchPacks(runtime, controller.signal)
        .then((value) => {
          if (!controller.signal.aborted) setPacks(value)
        })
        .catch(() => {})
    poll()
    const timer = setInterval(poll, 2000)
    return () => {
      controller.abort()
      clearInterval(timer)
    }
  }, [runtime])
  useEffect(() => {
    // Report the current/latest pack, not every historical failed attempt.
    const pack = target?.pack ? packs.find((p) => p.id === target.pack) : packs[0]
    if (pack && (pack.status === 'failed' || pack.incomplete) && !notified.current.has(pack.id)) {
      notified.current.add(pack.id)
      notify(packFailure(pack))
    }
    if (status?.job?.state === 'failed' && !notified.current.has(status.job.id)) {
      notified.current.add(status.job.id)
      notify(status.error || status.job.detail || 'Routing download failed. Try again when online.')
    }
  }, [packs, status, target, notify])

  /** Download some or all of an area's resources; everything by default. */
  const start = async (area: DownloadArea, resources?: DownloadResource[]) => {
    if (!runtime.offline?.routing && (!resources || resources.includes('routing'))) {
      notify('The local routing backend is unavailable.')
      return
    }
    setPreparing(true)
    setError('')
    try {
      setTarget(null)
      const { area: selected, regionId, pack, skipped, warning } = await startRegionDownload(runtime, area, {
        phase: setPhase,
        resolved: (resolved, region) => setTarget({ area: resolved, region }),
      }, resources)
      if (pack) {
        // Retries now keep their pack ID. Replace its old failed snapshot before
        // allowing notifications for the new attempt.
        setPacks((previous) => [pack, ...previous.filter((item) => item.id !== pack.id)])
        notified.current.delete(pack.id)
      }
      setTarget({ area: selected, region: regionId, pack: pack?.id })
      if (skipped.length) notify(`Some parts could not be downloaded: ${skipped.join(' · ')}`)
      if (warning) notify(warning)
      setPacks(await fetchPacks(runtime))
    } catch (reason) {
      const message = (reason as Error).message
      setError(message)
      notify(message)
    } finally {
      setPreparing(false)
      setPhase('')
      refresh()
    }
  }
  const cancel = async () => {
    setPreparing(true)
    setPhase('Stopping downloads…')
    try {
      await Promise.all([
        ...(routingBusy ? [cancelRoutingData(runtime)] : []),
        ...active.map((pack) => cancelPack(runtime, pack.id)),
      ])
    } catch (reason) {
      notify((reason as Error).message)
    } finally {
      setPreparing(false)
      setPhase('')
      refresh()
    }
  }
  const useRegion = useCallback(
    async (id: string) => {
      try {
        await prepareRoutingData(runtime, id)
        refresh()
      } catch (reason) {
        notify((reason as Error).message)
      }
    },
    [runtime, refresh, notify],
  )
  return {
    packs,
    active,
    preparing,
    phase,
    error,
    target,
    routingBusy,
    downloading,
    start,
    cancel,
    useRegion,
  }
}

export type Downloads = ReturnType<typeof useDownloads>
