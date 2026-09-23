import { useEffect, useMemo, useState } from 'react'
import type { RuntimeConfig, RoutingDataStatus, PackSummary } from '../../../src/lib/offline'
import { formatBytes } from '../../../src/lib/offline'
import { searchPlaces } from '../../../src/lib/geocoding'
import type { PlaceResult } from '../../../src/lib/geocoding'
import { request } from './model'
import Icon from './Icon'
import { coversBounds, RESOURCE_LABELS, packFailure } from './downloads'
import type { DownloadArea, DownloadRegion, Downloads } from './downloads'

function normalized(value: string) {
  return value
    .normalize('NFD')
    .replace(/\p{Diacritic}/gu, '')
    .toLowerCase()
}
function areaFor(region: DownloadRegion): DownloadArea | null {
  return region.bbox
    ? {
        id: region.id,
        name: region.name,
        kind: region.kind === 'country' ? 'country' : 'region',
        bounds: region.bbox,
        regionId: region.id,
      }
    : null
}
function fullPack(packs: PackSummary[], area: DownloadArea | null) {
  return area
    ? packs.find(
        (pack) =>
          (pack.status === 'complete' ||
            pack.detail === 'provider_limits' ||
            pack.detail === 'resource_failures') &&
          coversBounds(pack.bbox, area.bounds) &&
          pack.resources['vector-map']?.failed === 0 &&
          pack.resources['vector-map']?.total > 0 &&
          pack.resources['vector-map']?.done === pack.resources['vector-map']?.total,
      )
    : undefined
}

export default function RegionBrowser({
  runtime,
  status,
  downloads,
  onClose,
  notify,
}: {
  runtime: RuntimeConfig
  status: RoutingDataStatus | null
  downloads: Downloads
  onClose: () => void
  notify: (message: string) => void
}) {
  const [regions, setRegions] = useState<DownloadRegion[]>([])
  const [loading, setLoading] = useState(true)
  const [catalogError, setCatalogError] = useState('')
  const [mode, setMode] = useState<'cities' | 'regions' | 'countries'>('regions')
  const [country, setCountry] = useState('')
  const [query, setQuery] = useState('')
  const [places, setPlaces] = useState<PlaceResult[]>([])
  const [searching, setSearching] = useState(false)
  const [selected, setSelected] = useState<DownloadArea | null>(null)
  const [selectedRecord, setSelectedRecord] = useState<DownloadRegion | null>(null)
  const [trail, setTrail] = useState<DownloadRegion[]>([])
  const installed = useMemo(
    () => new Set(status?.cached.filter((r) => r.selected).map((r) => r.regionId) ?? []),
    [status?.cached],
  )
  const catalogue = useMemo(() => {
    const merged = new Map(regions.map((region) => [region.id, region]))
    for (const region of status?.cached ?? [])
      if (region.selected && !merged.has(region.regionId))
        merged.set(region.regionId, {
          id: region.regionId,
          name: region.name,
          kind: 'region',
          installed: true,
          active: region.regionId === status?.regionId,
        })
    return [...merged.values()].sort((a, b) => a.name.localeCompare(b.name))
  }, [regions, status?.cached, status?.regionId])
  const byID = useMemo(() => new Map(catalogue.map((region) => [region.id, region])), [catalogue])
  const countries = catalogue.filter((region) => region.kind === 'country')
  const cached = catalogue.filter((region) => installed.has(region.id) || region.installed)
  const countryOf = (region: DownloadRegion) => {
    let current: DownloadRegion | undefined = region
    const seen = new Set<string>()
    while (current && !seen.has(current.id)) {
      if (current.kind === 'country') return current.id
      seen.add(current.id)
      current = byID.get(current.parent || '')
    }
    return ''
  }

  useEffect(() => {
    const controller = new AbortController()
    void request<{ regions: DownloadRegion[]; cachedOnly: boolean }>(
      `${runtime.offline?.routing}/regions`,
      { signal: controller.signal },
    )
      .then((result) => {
        setRegions(result.regions)
        if (result.cachedOnly)
          setCatalogError('Showing downloaded regions. Go online once to load the full catalogue.')
        const active = result.regions.find((region) => region.id === status?.regionId)
        if (active?.parent)
          setCountry(
            result.regions.find(
              (region) => region.id === active.parent && region.kind === 'country',
            )?.id ?? '',
          )
      })
      .catch((error) => {
        if (!controller.signal.aborted) setCatalogError((error as Error).message)
      })
      .finally(() => {
        if (!controller.signal.aborted) setLoading(false)
      })
    return () => controller.abort()
  }, [runtime.offline?.routing])

  useEffect(() => {
    const back = (event: Event) => {
      event.preventDefault()
      event.stopImmediatePropagation()
      if (trail.length) {
        const previous = trail[trail.length - 1]
        setTrail(trail.slice(0, -1))
        setSelected(areaFor(previous))
        setSelectedRecord(previous)
      } else if (selected || selectedRecord) {
        setSelected(null)
        setSelectedRecord(null)
      } else onClose()
    }
    window.addEventListener('overland:back', back, true)
    return () => window.removeEventListener('overland:back', back, true)
  }, [selected, selectedRecord, trail, onClose])

  const choose = (region: DownloadRegion, download = false) => {
    const area = areaFor(region)
    if (selectedRecord) setTrail((previous) => [...previous, selectedRecord])
    setSelected(area)
    setSelectedRecord(region)
    if (download && area) void downloads.start(area)
  }
  const findCities = async () => {
    if (query.trim().length < 2) return
    setSearching(true)
    try {
      setPlaces(await searchPlaces(query, undefined, runtime.nominatimUrl, { runtime }))
    } catch (error) {
      notify((error as Error).message)
    } finally {
      setSearching(false)
    }
  }
  const selectCity = (place: PlaceResult) => {
    if (!place.bounds) {
      notify('This result has no area boundary. Choose a city or town result.')
      return
    }
    setSelectedRecord(null)
    setSelected({
      id: `city:${place.place_id}`,
      name: place.display_name.split(',')[0],
      kind: 'city',
      bounds: place.bounds,
    })
  }
  const begin = () => {
    if (selected) void downloads.start(selected)
  }
  const detailRegionID =
    selected?.regionId ||
    selectedRecord?.id ||
    (downloads.target?.area.id === selected?.id ? downloads.target?.region : undefined)
  const detailInstalled = detailRegionID
    ? installed.has(detailRegionID) || selectedRecord?.installed === true
    : false
  const relevantJob =
    detailRegionID && status?.job?.regionId === detailRegionID ? status?.job : undefined
  const regionRunning = relevantJob?.state === 'running' || relevantJob?.state === 'queued'
  const matchingTarget = downloads.target?.area.id === selected?.id ? downloads.target : undefined
  const matchingPack =
    downloads.packs.find(
      (pack) => pack.id === matchingTarget?.pack || `pack:${pack.id}` === selected?.id,
    ) ??
    fullPack(downloads.packs, selected) ??
    downloads.packs.find((pack) => pack.name === `Map: ${selected?.name}`)
  const coversSelection = selected ? coversBounds(matchingPack?.bbox, selected.bounds) : false
  const regionChildren = selectedRecord
    ? catalogue.filter((region) => region.parent === selectedRecord.id)
    : []
  const isOffline = runtime.offline?.mode === 'cache-only'
  const savedAreas = downloads.packs.filter(
    (pack, index, all) =>
      pack.bbox &&
      pack.name?.startsWith('Map:') &&
      (pack.status === 'complete' || pack.incomplete) &&
      all.findIndex(
        (p) => p.name === pack.name && JSON.stringify(p.bbox) === JSON.stringify(pack.bbox),
      ) === index,
  )

  const row = (region: DownloadRegion) => {
    const downloaded = installed.has(region.id) || region.installed
    const mapped = Boolean(fullPack(downloads.packs, areaFor(region)))
    const inUse = status?.ready && status.regionId === region.id
    const working = status?.job?.regionId === region.id && downloads.routingBusy
    const parentName = byID.get(region.parent || '')?.name
    return (
      <div className="region-row" key={region.id}>
        <button className="region-row-main" onClick={() => choose(region)}>
          <span className={`region-symbol ${downloaded ? 'downloaded' : ''}`}>
            <Icon name={downloaded ? 'check' : 'explore'} size={22} />
          </span>
          <span>
            <strong>{region.name}</strong>
            <small>
              {working
                ? 'Downloading routing…'
                : downloaded
                  ? mapped
                    ? 'Routing and map pack downloaded'
                    : 'Routing downloaded · maps not complete'
                  : parentName || 'Available to download'}
            </small>
          </span>
          {inUse && <span className="region-badge">In use</span>}
        </button>
        <button
          className="icon-button"
          aria-label={downloaded ? `Details for ${region.name}` : `Download ${region.name}`}
          disabled={downloads.preparing || downloads.downloading || (!downloaded && isOffline)}
          onClick={() => choose(region, !downloaded)}
        >
          <Icon name={downloaded ? 'arrow' : 'offline'} size={20} />
        </button>
      </div>
    )
  }
  const filtered = catalogue.filter(
    (region) =>
      region.kind === (mode === 'countries' ? 'country' : 'region') &&
      (!country || mode === 'countries' || countryOf(region) === country) &&
      normalized(region.name).includes(normalized(query)),
  )

  return (
    <section className="region-browser" aria-label="Download regions">
      <header className="region-browser-header">
        <button
          className="icon-button back-arrow"
          aria-label={selected || selectedRecord ? 'Back to regions' : 'Back to offline map'}
          onClick={() => {
            if (trail.length) {
              const previous = trail[trail.length - 1]
              setTrail(trail.slice(0, -1))
              setSelected(areaFor(previous))
              setSelectedRecord(previous)
            } else if (selected || selectedRecord) {
              setSelected(null)
              setSelectedRecord(null)
            } else onClose()
          }}
        >
          <Icon name="arrow" />
        </button>
        <div>
          <small>OFFLINE MAPS</small>
          <h1>{selected?.name || selectedRecord?.name || 'Download regions'}</h1>
        </div>
      </header>
      {selected || selectedRecord ? (
        <div className="region-browser-scroll panel-stack">
          <div className="region-detail-banner">
            <Icon name="explore" size={32} />
            <div>
              <strong>
                {detailInstalled
                  ? 'Routing downloaded'
                  : selected?.kind === 'city'
                    ? 'City maps & local routing'
                    : 'Maps & routing'}
              </strong>
              <small>
                {detailInstalled
                  ? status?.regionId === detailRegionID
                    ? 'Currently in use'
                    : 'Available on this device'
                  : 'Download for offline use'}
              </small>
            </div>
            {detailInstalled && <span className="region-badge">Downloaded</span>}
          </div>
          {selected?.kind === 'city' && (
            <p className="muted">
              Map downloads cover the city. Routing uses the provider's covering regional extract.
            </p>
          )}
          {selectedRecord?.kind === 'country' && regionChildren.length > 0 && (
            <p className="muted">
              For large countries, select a region below to keep detailed maps and place downloads
              within provider and storage limits.
            </p>
          )}
          {matchingTarget && downloads.preparing && (
            <p className="working" role="status">
              <span className="spinner" />
              {downloads.phase}
            </p>
          )}
          {!!matchingPack?.batchesTotal && matchingPack.batchesTotal > 1 && (
            <p className="muted" role="status">
              {matchingPack.total !== undefined && matchingPack.done === matchingPack.total && matchingPack.status !== 'running' && matchingPack.status !== 'queued'
                ? `${matchingPack.batchesTotal} batches finished`
                : `Batch ${Math.min((matchingPack.batchesDone ?? 0) + 1, matchingPack.batchesTotal)} of ${matchingPack.batchesTotal}`}
            </p>
          )}
          <div className="resource-downloads">
            <div className="resource-download">
              <div>
                <Icon name="plan" size={20} />
                <strong>Routing</strong>
                <span className={detailInstalled && !regionRunning ? 'ready-text' : ''}>
                  {regionRunning
                    ? relevantJob?.phase || 'Preparing'
                    : detailInstalled
                      ? 'Downloaded'
                      : detailRegionID
                        ? 'Not downloaded'
                        : 'Not checked yet'}
                </span>
              </div>
              {regionRunning && (
                <progress
                  max={relevantJob?.total || undefined}
                  value={relevantJob?.total ? (relevantJob.done ?? 0) : undefined}
                />
              )}
            </div>
            {Object.entries(RESOURCE_LABELS).map(([kind, label]) => {
              const progress = matchingPack?.resources[kind]
              const unavailable = matchingPack?.unavailable?.find(
                (item) =>
                  item.resource === kind || (kind === 'vector-map' && item.layer === 'openfreemap'),
              )
              const running =
                matchingPack?.status === 'running' || matchingPack?.status === 'queued'
              const done =
                progress &&
                progress.total > 0 &&
                progress.done === progress.total &&
                !progress.failed &&
                !unavailable &&
                (matchingPack?.status === 'complete' ||
                  matchingPack?.detail === 'provider_limits' ||
                  matchingPack?.detail === 'resource_failures') &&
                coversSelection
              return (
                <div className="resource-download" key={kind}>
                  <div>
                    <Icon
                      name={
                        done
                          ? 'check'
                          : kind === 'vector-map'
                            ? 'layers'
                            : kind === 'elevation'
                              ? 'mountain'
                              : 'locate'
                      }
                      size={20}
                    />
                    <strong>{label}</strong>
                    <span className={done ? 'ready-text' : progress?.failed ? 'missing-text' : ''}>
                      {unavailable
                        ? 'Provider limit'
                        : done
                          ? 'Downloaded'
                          : progress?.failed
                            ? `${progress.failed} missing`
                            : running
                              ? `${progress?.done ?? 0} / ${progress?.total ?? '…'}`
                              : progress && !coversSelection
                                ? 'Partial area'
                                : 'Not downloaded'}
                    </span>
                  </div>
                  {progress && running && (
                    <progress max={progress.total || 1} value={progress.done} />
                  )}
                  {progress && progress.bytes > 0 && <small>{formatBytes(progress.bytes)}</small>}
                  {unavailable && <small>{unavailable.reason}</small>}
                </div>
              )
            })}
          </div>
          {matchingPack?.incomplete && (
            <p className="inline-error" role="alert">
              {packFailure(matchingPack)}
            </p>
          )}
          {matchingTarget && downloads.error && (
            <p className="inline-error" role="alert">
              {downloads.error} Choose a city or smaller region if this area exceeds the download
              limits.
            </p>
          )}
          {regionRunning && status?.error && (
            <p className="inline-error" role="alert">
              {status.error}
            </p>
          )}
          {downloads.downloading ? (
            <button
              className="secondary"
              disabled={downloads.preparing}
              onClick={() => void downloads.cancel()}
            >
              Stop downloads
            </button>
          ) : (
            <button
              className="primary"
              disabled={!selected || downloads.preparing || isOffline}
              onClick={begin}
            >
              <Icon name="offline" size={20} />
              {downloads.preparing
                ? 'Preparing…'
                : detailInstalled
                  ? 'Download missing resources'
                  : `Download ${selected?.name || selectedRecord?.name}`}
            </button>
          )}
          {detailInstalled && detailRegionID !== status?.regionId && (
            <button
              className="secondary"
              disabled={downloads.preparing || downloads.downloading}
              onClick={() => void downloads.useRegion(detailRegionID!)}
            >
              Use downloaded routing
            </button>
          )}
          {!detailInstalled && selectedRecord && downloads.error && (
            <button
              className="secondary"
              disabled={downloads.preparing || downloads.downloading || isOffline}
              onClick={() => void downloads.useRegion(selectedRecord.id)}
            >
              Download routing only
            </button>
          )}
          {isOffline && (
            <p className="muted">
              Switch to Online on the map to download new resources. Downloaded routing is available
              now.
            </p>
          )}
          {regionChildren.length > 0 && (
            <>
              <div className="section-heading">
                <h3>Choose a smaller region</h3>
              </div>
              {regionChildren.map(row)}
            </>
          )}
          {!selected && <p className="muted">Connect once to load this region's map boundaries.</p>}
        </div>
      ) : (
        <>
          <div className="region-browser-controls">
            <div className="profile-selector" aria-label="Area type">
              {(['cities', 'regions', 'countries'] as const).map((item) => (
                <button
                  key={item}
                  className={mode === item ? 'selected' : ''}
                  onClick={() => {
                    setMode(item)
                    setQuery('')
                    setPlaces([])
                  }}
                >
                  {item === 'cities'
                    ? 'City'
                    : item === 'regions'
                      ? 'Region / comunidad'
                      : 'Country'}
                </button>
              ))}
            </div>
            <form
              className="search-field"
              onSubmit={(event) => {
                event.preventDefault()
                if (mode === 'cities') void findCities()
              }}
            >
              <Icon name="search" size={18} />
              <input
                aria-label={mode === 'cities' ? 'Search city' : 'Filter regions'}
                placeholder={mode === 'cities' ? 'Search a city or town' : 'Find a region'}
                value={query}
                onChange={(event) => setQuery(event.target.value)}
              />
              {mode === 'cities' && (
                <button type="submit" aria-label="Find city" disabled={searching}>
                  <Icon name="arrow" size={18} />
                </button>
              )}
            </form>
            {mode === 'regions' && countries.length > 0 && (
              <select
                aria-label="Country filter"
                value={country}
                onChange={(event) => setCountry(event.target.value)}
              >
                <option value="">All countries</option>
                {countries.map((item) => (
                  <option key={item.id} value={item.id}>
                    {item.name}
                  </option>
                ))}
              </select>
            )}
          </div>
          <div className="region-browser-scroll">
            {cached.length > 0 && !query && (
              <div className="downloaded-regions">
                <div className="section-heading">
                  <h3>Downloaded</h3>
                  <span>{cached.length}</span>
                </div>
                {cached.map(row)}
              </div>
            )}
            {savedAreas.length > 0 && !query && (
              <div className="downloaded-regions">
                <div className="section-heading">
                  <h3>Saved map areas</h3>
                </div>
                {savedAreas.map((pack) => (
                  <button
                    className="list-row"
                    key={pack.id}
                    onClick={() => {
                      setSelectedRecord(null)
                      setTrail([])
                      setSelected({
                        id: `pack:${pack.id}`,
                        name: pack.name!.replace(/^Map: /, ''),
                        kind: 'area',
                        bounds: pack.bbox!,
                      })
                    }}
                  >
                    <Icon name={pack.incomplete ? 'layers' : 'check'} />
                    <span>
                      {pack.name!.replace(/^Map: /, '')}
                      <small>
                        {pack.incomplete
                          ? 'Partial download · tap to finish'
                          : 'Map resources downloaded'}
                      </small>
                    </span>
                    <span className={`region-badge${pack.incomplete ? ' partial' : ''}`}>
                      {pack.incomplete ? 'Partial' : 'Downloaded'}
                    </span>
                  </button>
                ))}
              </div>
            )}
            {loading && (
              <p className="working">
                <span className="spinner" />
                Loading regions…
              </p>
            )}
            {catalogError && <p className="muted catalog-message">{catalogError}</p>}
            {mode === 'cities' ? (
              <>
                {searching && <p className="working">Finding cities…</p>}
                {places.map((place) => (
                  <button
                    className="list-row"
                    key={place.place_id}
                    onClick={() => selectCity(place)}
                  >
                    <Icon name="locate" />
                    <span>{place.display_name}</span>
                    <Icon name="arrow" size={18} />
                  </button>
                ))}
                {!places.length && !searching && (
                  <p className="muted catalog-message">
                    Search for a city to download its map area and the covering routing region.
                  </p>
                )}
              </>
            ) : (
              <>
                <div className="section-heading">
                  <h3>
                    {mode === 'countries'
                      ? 'Countries'
                      : country
                        ? `${byID.get(country)?.name} · regions`
                        : 'Regions & comunidades'}
                  </h3>
                </div>
                {filtered.map(row)}
                {!loading && !filtered.length && (
                  <p className="muted catalog-message">No matching regions.</p>
                )}
              </>
            )}
          </div>
        </>
      )}
    </section>
  )
}
