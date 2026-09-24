import { useCallback, useEffect, useRef, useState } from 'react'
import type {
  RuntimeConfig,
  RoutingDataStatus,
  PackSummary,
  PackEstimateRequest,
  PackBounds,
  PackResourceProgress,
} from '../../../src/lib/offline'
import {
  cancelRoutingData,
  prepareRoutingData,
  fetchPacks,
  estimatePack,
  startPack,
  cancelPack,
  normalizePackBounds,
} from '../../../src/lib/offline'
import type { BoundingBox } from '../../../src/lib/poi'
import { jsonBody, request } from './model'

export interface DownloadArea {
  id: string
  name: string
  kind: 'city' | 'region' | 'country' | 'area'
  bounds: BoundingBox
  regionId?: string
}
export interface DownloadRegion {
  id: string
  name: string
  parent?: string
  kind: 'continent' | 'country' | 'region'
  bbox?: BoundingBox
  installed: boolean
  active: boolean
}

export const RESOURCE_LABELS: Record<string, string> = {
  'vector-map': 'Vector maps',
  elevation: 'Elevation',
  'fuel-prices': 'Fuel prices',
  'fuel-stations': 'Fuel stations',
  water: 'Water',
  campsites: 'Campsites',
}

export function resourceTransferText(progress: PackResourceProgress | undefined): string | null {
  if (!progress || (progress.reused === undefined && progress.downloaded === undefined && progress.revalidated === undefined)) return null
  const parts = [`${(progress.reused ?? 0).toLocaleString()} cached`, `${(progress.downloaded ?? 0).toLocaleString()} downloaded`]
  if (progress.revalidated) parts.push(`${progress.revalidated.toLocaleString()} checked online`)
  return parts.join(' · ')
}

export function coverageLabel(kind: DownloadArea['kind'] | undefined): string {
  switch (kind) {
    case 'region': return 'Whole region'
    case 'country': return 'Whole country'
    case 'city': return 'City area'
    case 'area': return 'Selected map area'
    default: return 'Saved map extent'
  }
}

export function boundsLabel(bounds: BoundingBox): string {
  return `${bounds.south.toFixed(3)}, ${bounds.west.toFixed(3)} → ${bounds.north.toFixed(3)}, ${bounds.east.toFixed(3)}`
}

export function coversBounds(pack: PackBounds | undefined, area: BoundingBox): boolean {
  return Boolean(
    pack &&
      pack.south <= area.south + 1e-6 &&
      pack.west <= area.west + 1e-6 &&
      pack.north >= area.north - 1e-6 &&
      pack.east >= area.east - 1e-6,
  )
}

export function packFailure(pack: PackSummary): string {
  const failed = Object.entries(pack.resources)
    .filter(([, progress]) => progress.failed > 0)
    .map(([kind]) => RESOURCE_LABELS[kind] ?? kind)
  if (pack.detail === 'interrupted')
    return 'Download interrupted. Tap download to resume using the cached data.'
  if (pack.detail === 'resource_limit')
    return 'The download reached its storage budget. Choose a smaller area.'
  if (pack.detail === 'provider_limits')
    return 'Available resources downloaded. Place-provider limits require city-sized areas for fresh stops.'
  if (failed.length)
    return `Could not finish ${failed.join(', ').toLowerCase()}. Download again to retry the missing resources.`
  return 'Some resources are missing. Download again to finish the area.'
}

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
    region: string
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

  const start = async (area: DownloadArea) => {
    if (!runtime.offline?.routing) {
      notify('The local routing backend is unavailable.')
      return
    }
    setPreparing(true)
    setError('')
    try {
      const box = normalizePackBounds(
        { lat: area.bounds.south, lon: area.bounds.west },
        { lat: area.bounds.north, lon: area.bounds.east },
      )
      if (!box) throw new Error('Choose a smaller region before downloading.')
      let regionId = area.regionId,
        name = area.name
      setTarget(null)
      setPhase('Finding routing coverage…')
      if (!regionId) {
        const { region } = await request<{
          region: { regionId: string; name: string; coversView: boolean } | null
        }>(`${runtime.offline.routing}/suggest`, jsonBody({ bbox: box }))
        if (!region || !region.coversView)
          throw new Error(
            'This area is not covered by one local routing region. Choose a smaller area or a named region.',
          )
        regionId = region.regionId
        if (area.kind === 'area') name = `Visible area near ${region.name}`
      }
      const selected = { ...area, name, bounds: box, regionId }
      setTarget({ area: selected, region: regionId })
      const body: PackEstimateRequest = {
        regional: true,
        coverageKind: area.kind,
        name: `Map: ${name}`,
        bbox: [box.south, box.west, box.north, box.east],
        paddingKm: 0,
        minZoom: 5,
        maxZoom: 14,
        layers: ['openfreemap'],
        scopes: ['elevation', 'pois', 'fuel'],
      }
      setPhase('Checking maps and storage…')
      const estimate = await estimatePack(runtime, body)
      // Map/elevation batches can cover whole regions. Do not turn provider
      // limits on broad POI searches into tiled Overpass harvesting.
      const blockedMaps = estimate.blocked.filter((item) => item.layer)
      if (blockedMaps.length) throw new Error(blockedMaps.map((item) => item.reason).join(' · '))
      if (!estimate.resources) throw new Error('No map resources are available for this area.')
      setPhase(`Preparing ${name}…`)
      await prepareRoutingData(runtime, regionId)
      const pack = await startPack(runtime, body)
      if (pack) {
        // Retries now keep their pack ID. Replace its old failed snapshot before
        // allowing notifications for the new attempt.
        setPacks((previous) => [pack, ...previous.filter((item) => item.id !== pack.id)])
        notified.current.delete(pack.id)
      }
      setTarget({ area: selected, region: regionId, pack: pack?.id })
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
