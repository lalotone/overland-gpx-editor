import { useCallback, useEffect, useRef, useState } from 'react'
import { cancelPack, cancelRoutingData, deletePack, fetchPacks, prepareRoutingData } from '../lib/offline'
import type { PackSummary, RoutingDataStatus, RuntimeConfig } from '../lib/offline'
import { packAreaName, packFailure, packIsActive, routingJobActive, startRegionDownload } from '../lib/offlineRegions'
import type { DownloadArea } from '../lib/offlineRegions'

type Notify = (message: string, type?: 'info' | 'success' | 'error') => void

export interface DownloadTarget {
  area: DownloadArea
  region: string
  /** Every map pack this download started; large areas take several. */
  packs?: string[]
}

/**
 * Region download state for the web manager. Routing status is owned by the
 * caller (the planner needs it too); this hook follows the map packs and runs
 * the actions. Packs are polled only while the manager is open or something
 * is downloading.
 */
export function useRegionDownloads({
  runtime,
  routing,
  refreshRouting,
  watching,
  notify,
}: {
  runtime: RuntimeConfig
  routing: RoutingDataStatus | null
  refreshRouting: () => void
  watching: boolean
  notify: Notify
}) {
  const [packs, setPacks] = useState<PackSummary[]>([])
  const [packsError, setPacksError] = useState('')
  const [preparing, setPreparing] = useState(false)
  const [phase, setPhase] = useState('')
  const [error, setError] = useState('')
  const [busyPack, setBusyPack] = useState<string | null>(null)
  const [target, setTarget] = useState<DownloadTarget | null>(null)
  const notified = useRef(new Set<string>())
  const activePacks = packs.filter(packIsActive)
  const routingBusy = routingJobActive(routing)
  const downloading = routingBusy || activePacks.length > 0
  const available = Boolean(runtime.offline?.routing)

  const refreshPacks = useCallback(async (signal?: AbortSignal) => {
    if (!runtime.offline) {
      setPacks([])
      return
    }
    try {
      const next = await fetchPacks(runtime, signal)
      if (signal?.aborted) return
      setPacks(next)
      setPacksError('')
    } catch (reason) {
      if ((reason as Error).name !== 'AbortError') setPacksError((reason as Error).message)
    }
  }, [runtime])

  const polling = watching || activePacks.length > 0
  useEffect(() => {
    const controller = new AbortController()
    void refreshPacks(controller.signal)
    if (!polling) return () => controller.abort()
    const timer = window.setInterval(() => void refreshPacks(controller.signal), 2000)
    return () => {
      controller.abort()
      window.clearInterval(timer)
    }
  }, [polling, refreshPacks])

  useEffect(() => {
    // Report this download's packs, not every historical failed attempt.
    for (const id of target?.packs ?? []) {
      const pack = packs.find(p => p.id === id)
      if (pack && (pack.status === 'failed' || pack.incomplete) && !notified.current.has(pack.id)) {
        notified.current.add(pack.id)
        notify(`${packAreaName(pack)}: ${packFailure(pack)}`, 'error')
      }
    }
    if (routing?.job?.state === 'failed' && !notified.current.has(routing.job.id)) {
      notified.current.add(routing.job.id)
      notify(routing.error || routing.job.detail || 'Routing download failed. Try again when online.', 'error')
    }
  }, [packs, routing, target, notify])

  const start = async (area: DownloadArea) => {
    if (!available) {
      notify('The local routing backend is unavailable.', 'error')
      return
    }
    setPreparing(true)
    setError('')
    setTarget(null)
    try {
      const { area: selected, regionId, packs: started, skipped, warning } = await startRegionDownload(runtime, area, {
        phase: setPhase,
        resolved: (resolved, region) => setTarget({ area: resolved, region }),
      })
      // Retries keep their pack ID. Replace old failed snapshots before
      // allowing notifications for the new attempt.
      const ids = new Set(started.map(item => item.id))
      setPacks(previous => [...started, ...previous.filter(item => !ids.has(item.id))])
      for (const id of ids) notified.current.delete(id)
      setTarget({ area: selected, region: regionId, packs: [...ids] })
      if (skipped.length) notify(`Some parts could not be downloaded: ${skipped.join(' · ')}`, 'error')
      if (warning) notify(warning, 'info')
      await refreshPacks()
    } catch (reason) {
      const message = (reason as Error).message
      setError(message)
      notify(message, 'error')
    } finally {
      setPreparing(false)
      setPhase('')
      refreshRouting()
    }
  }

  const run = async (id: string, action: () => Promise<unknown>) => {
    setBusyPack(id)
    try {
      await action()
    } catch (reason) {
      notify((reason as Error).message, 'error')
    } finally {
      setBusyPack(null)
      refreshRouting()
      await refreshPacks()
    }
  }

  return {
    available,
    packs,
    packsError,
    activePacks,
    preparing,
    phase,
    error,
    target,
    busyPack,
    routingBusy,
    downloading,
    start,
    stopAll: () => run('all', () => Promise.all([
      ...(routingBusy ? [cancelRoutingData(runtime)] : []),
      ...activePacks.map(pack => cancelPack(runtime, pack.id)),
    ])),
    stopRouting: () => run('routing', () => cancelRoutingData(runtime)),
    stopPack: (id: string) => run(id, () => cancelPack(runtime, id)),
    removePack: (id: string) => run(id, () => deletePack(runtime, id)),
    /** Switch routing to a downloaded region, or resume/finish one. */
    useRegion: (id: string) => run('routing', () => prepareRoutingData(runtime, id)),
  }
}

export type RegionDownloads = ReturnType<typeof useRegionDownloads>
