import { expect, test } from '@playwright/test'
import type { Page, Route } from '@playwright/test'

const STYLE = {
  version: 8,
  sources: {
    fixture: {
      type: 'geojson',
      data: { type: 'FeatureCollection', features: [] },
    },
  },
  layers: [
    { id: 'background', type: 'background', paint: { 'background-color': '#dce8df' } },
  ],
}

const BROKEN_SOURCE_STYLE = {
  version: 8,
  sources: { roads: { type: 'vector', url: '/e2e/source.json' } },
  layers: [
    { id: 'background', type: 'background', paint: { 'background-color': '#dce8df' } },
    { id: 'roads', type: 'line', source: 'roads', 'source-layer': 'roads' },
  ],
}

const BROKEN_TILE_STYLE = {
  version: 8,
  sources: {
    roads: {
      type: 'vector',
      tiles: ['/e2e/vector/{z}/{x}/{y}.pbf'],
      minzoom: 0,
      maxzoom: 14,
    },
  },
  layers: [
    { id: 'background', type: 'background', paint: { 'background-color': '#dce8df' } },
    { id: 'roads', type: 'line', source: 'roads', 'source-layer': 'roads' },
  ],
}

const PROXIED_STYLE = {
  version: 8,
  sprite: '/map/openfreemap/sprite',
  glyphs: '/map/openfreemap/glyphs/{fontstack}/{range}.pbf',
  sources: {},
  layers: [
    { id: 'background', type: 'background', paint: { 'background-color': '#dce8df' } },
  ],
}

const GPX = `<?xml version="1.0" encoding="UTF-8"?>
<gpx version="1.1" creator="browser-test" xmlns="http://www.topografix.com/GPX/1/1">
  <trk><name>Browser route</name><trkseg>
    <trkpt lat="42.8500" lon="-2.6800"><ele>540</ele></trkpt>
    <trkpt lat="42.9000" lon="-2.5900"><ele>680</ele></trkpt>
    <trkpt lat="42.9600" lon="-2.5100"><ele>620</ele></trkpt>
  </trkseg></trk>
</gpx>`

const GPX_WITH_WAYPOINT = GPX.replace(
  '  <trk>',
  '  <wpt lat="42.8700" lon="-2.6300"><name>Fuel stop</name><sym>Gas Station</sym></wpt>\n  <trk>',
)

const resourceProgress = {
  'vector-map': { done: 3, total: 3, failed: 0, bytes: 12000, items: 0 },
  elevation: { done: 2, total: 2, failed: 0, bytes: 8000, items: 0 },
  'fuel-stations': { done: 1, total: 1, failed: 0, bytes: 1000, items: 4 },
  water: { done: 1, total: 1, failed: 0, bytes: 1000, items: 7 },
  campsites: { done: 1, total: 1, failed: 0, bytes: 1000, items: 2 },
  'fuel-prices': { done: 1, total: 1, failed: 0, bytes: 1000, items: 8500 },
}

async function json(route: Route, body: unknown, status = 200) {
  await route.fulfill({ status, contentType: 'application/json', body: JSON.stringify(body) })
}

/**
 * Serve the oldest unacknowledged command as an SSE frame, the way the broker
 * does. The handler always fulfils immediately — awaiting inside a route
 * handler queues every other intercepted request behind it — and asks for a
 * slow reconnect so the mock's short-lived stream does not spin.
 */
async function fulfillCommandStream(route: Route, command?: Record<string, unknown>) {
  const frames = ['retry: 1000\n\n']
  if (command) frames.push(`id: ${String(command.id)}\ndata: ${JSON.stringify(command)}\n\n`)
  await route.fulfill({
    status: 200,
    headers: { 'Content-Type': 'text/event-stream', 'Cache-Control': 'no-store' },
    body: frames.join(''),
  })
}

async function mockRuntime(page: Page, options: {
  style?: object
  styleFailure?: { status: number; body: Record<string, unknown> }
  elevationUnavailable?: boolean
  webGL2Unavailable?: boolean
  startupCacheOnly?: boolean
  vectorMissDuringOfflineTransition?: boolean
} = {}) {
  let started = false
  let offlineMode: 'auto' | 'cache-only' = options.startupCacheOnly ? 'cache-only' : 'auto'
  let osmRequests = 0
  let vectorRequests = 0
  let placeRequests = 0
  let packRequest: Record<string, unknown> | undefined
  const pendingVectorRoutes: Route[] = []
  const activeResources = options.elevationUnavailable
    ? Object.fromEntries(Object.entries(resourceProgress).filter(([key]) => key !== 'elevation'))
    : resourceProgress
  const mapResources = {
    'vector-map': resourceProgress['vector-map'],
    places: { done: 1, total: 1, failed: 0, bytes: 900, items: 1 },
  }
  const isMapPack = () => String(packRequest?.name ?? '').startsWith('Map: ')
  const pack = (state: 'queued' | 'complete') => {
    const resources = isMapPack() ? mapResources : activeResources
    const total = Object.values(resources).reduce((sum, progress) => sum + progress.total, 0)
    const requestedBounds = Array.isArray(packRequest?.bbox) ? packRequest.bbox as number[] : null
    return {
      id: '0123456789abcdef0123456789abcdef',
      name: String(packRequest?.name ?? 'Route: Browser trip'),
      state,
      done: state === 'complete' ? total : 0,
      total,
      failed: 0,
      bytes: state === 'complete' ? 24000 : 0,
      resources: state === 'complete'
        ? resources
        : Object.fromEntries(Object.entries(resources).map(([key, value]) => [key, { ...value, done: 0, bytes: 0, items: 0 }])),
      ...(requestedBounds?.length === 4 ? { bbox: {
        south: requestedBounds[0], west: requestedBounds[1], north: requestedBounds[2], east: requestedBounds[3],
      } } : {}),
      createdAt: '2026-08-31T12:00:00Z',
      updatedAt: '2026-08-31T12:00:01Z',
    }
  }

  await page.addInitScript(({ webGL2Unavailable }) => {
    localStorage.setItem('gpx-hillshade', 'off')
    if (!localStorage.getItem('gpx-base-layer')) localStorage.setItem('gpx-base-layer', 'openfreemap')
    if (webGL2Unavailable) {
      const original = HTMLCanvasElement.prototype.getContext
      HTMLCanvasElement.prototype.getContext = function (type: string, ...args: unknown[]) {
        if (type === 'webgl2') return null
        return original.call(this, type, ...args as [])
      } as typeof HTMLCanvasElement.prototype.getContext
    }
  }, { webGL2Unavailable: options.webGL2Unavailable === true })
  await page.route(/\/config$/, route => json(route, {
    offline: {
      enabled: true,
      mode: offlineMode,
      status: '/offline/status',
      packs: '/offline/packs',
      ...(options.startupCacheOnly ? {} : { modeControl: '/offline/mode' }),
    },
    services: { places: '/places/search' },
    maps: { raster: { osm: '/e2e/osm/{z}/{x}/{y}.png' }, openfreemap: { style: '/map/openfreemap/style.json', allowBulk: true } },
  }))
  await page.route(/\/files$/, route => json(route, { files: [] }))
  await page.route(/\/upload$/, route => json(route, { filename: 'browser-trip.gpx' }, 201))
  await page.route(/\/elevation\/prefetch$/, route => json(route, { done: 0, total: 0, state: 'complete' }))
  await page.route(/\/map\/openfreemap\/style\.json$/, route => options.styleFailure
    ? json(route, options.styleFailure.body, options.styleFailure.status)
    : json(route, options.style ?? STYLE))
  await page.route(/\/map\/openfreemap\/sprite(?:@2x)?\.json$/, route => json(route, {}))
  await page.route(/\/map\/openfreemap\/sprite(?:@2x)?\.png$/, route => route.fulfill({
    status: 200,
    contentType: 'image/png',
    body: Buffer.from('iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+A8AAQUBAScY42YAAAAASUVORK5CYII=', 'base64'),
  }))
  await page.route(/\/e2e\/transient\//, route => route.fulfill({ status: 503, body: 'temporary tile failure' }))
  await page.route(/\/e2e\/source\.json$/, route => route.fulfill({ status: 503, body: 'source unavailable' }))
  await page.route(/\/e2e\/vector\//, route => {
    vectorRequests++
    if (options.vectorMissDuringOfflineTransition && offlineMode === 'auto') {
      pendingVectorRoutes.push(route)
      return
    }
    return route.fulfill({
      status: offlineMode === 'cache-only' ? 504 : 503,
      contentType: 'application/json',
      body: offlineMode === 'cache-only'
        ? JSON.stringify({ code: 'offline_cache_miss', detail: 'resource is not available in the offline cache' })
        : JSON.stringify({ detail: 'tile unavailable' }),
    })
  })
  await page.route(/\/e2e\/osm\//, route => {
    osmRequests++
    return route.fulfill({
      status: 200,
      contentType: 'image/png',
      body: Buffer.from('iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+A8AAQUBAScY42YAAAAASUVORK5CYII=', 'base64'),
    })
  })
  await page.route(/\/places\/search/, route => {
    placeRequests++
    return route.fulfill({
      status: 200,
      contentType: 'application/json',
      headers: { 'X-GPX-Cache': 'hit', Age: '120' },
      body: JSON.stringify([{
        place_id: 123456,
        display_name: 'Vitoria-Gasteiz, Araba, Euskadi, España',
        lat: '42.8467',
        lon: '-2.6726',
        boundingbox: ['42.80', '42.90', '-2.75', '-2.58'],
        class: 'place',
        type: 'city',
      }]),
    })
  })
  await page.route(/\/offline\/packs\/estimate$/, async route => {
    packRequest = route.request().postDataJSON() as Record<string, unknown>
    if (options.elevationUnavailable && (packRequest.scopes as string[]).includes('elevation')) {
      await json(route, { detail: 'elevation packs require Terrarium tile mode with a persistent tile cache' }, 400)
      return
    }
    const estimatedResources = isMapPack() ? mapResources : activeResources
    const total = Object.values(estimatedResources).reduce((sum, progress) => sum + progress.total, 0)
    await json(route, {
      resources: total,
      estimatedBytes: 24000,
      remainingQuota: 1000000,
      counts: isMapPack()
        ? {
            'openfreemap-core': 1,
            openfreemap: 2,
            ...((packRequest.scopes as string[]).includes('places') ? { places: 1 } : {}),
            ...((packRequest.scopes as string[]).includes('elevation') ? { elevation: 2 } : {}),
          }
        : {
            'openfreemap-core': 1,
            openfreemap: 2,
            ...(options.elevationUnavailable ? {} : { elevation: 2 }),
            'pois-fuel': 1,
            'pois-water': 1,
            'pois-camp': 1,
            fuel: 1,
          },
      blocked: [],
      scopes: {},
    })
  })
  await page.route(/\/offline\/mode$/, async route => {
    const request = route.request().postDataJSON() as { mode?: string }
    offlineMode = request.mode === 'cache-only' ? 'cache-only' : 'auto'
    if (offlineMode === 'cache-only') {
      const pending = pendingVectorRoutes.splice(0)
      await Promise.all(pending.map(vectorRoute => vectorRoute.fulfill({
        status: 504,
        contentType: 'application/json',
        body: JSON.stringify({ code: 'offline_cache_miss', detail: 'resource is not available in the offline cache' }),
      })))
    }
    await json(route, { mode: offlineMode, changed: true })
  })
  await page.route(/\/offline\/status$/, route => json(route, {
    mode: offlineMode,
    writable: true,
    bytes: started ? 24000 : 0,
    maxBytes: 1000000,
    entries: started ? 4 : 0,
    scopes: {},
    providers: {},
    jobs: [],
  }))
  await page.route(/\/offline\/packs$/, async route => {
    if (route.request().method() === 'POST') {
      packRequest = route.request().postDataJSON() as Record<string, unknown>
      started = true
      await json(route, pack('queued'), 202)
      return
    }
    await json(route, started ? [pack('complete')] : [])
  })
  await page.route(/\/offline\/packs\/[a-f0-9]{32}$/, route => {
    started = false
    return route.fulfill({ status: 204 })
  })

  return {
    getOsmRequests: () => osmRequests,
    getPackRequest: () => packRequest,
    getPlaceRequests: () => placeRequests,
    getOfflineMode: () => offlineMode,
    getVectorRequests: () => vectorRequests,
  }
}

async function expectVectorMap(page: Page) {
  await expect(page.locator('.maplibregl-canvas')).toBeVisible()
  await expect(page.getByText(/vector map style is unavailable/i)).toHaveCount(0)
}

async function expectStackedBelow(upper: ReturnType<Page['locator']>, lower: ReturnType<Page['locator']>) {
  const upperBox = await upper.boundingBox()
  const lowerBox = await lower.boundingBox()
  expect(upperBox).not.toBeNull()
  expect(lowerBox).not.toBeNull()
  expect(lowerBox!.y).toBeGreaterThanOrEqual(upperBox!.y + upperBox!.height + 4)
  expect(Math.abs((upperBox!.x + upperBox!.width) - (lowerBox!.x + lowerBox!.width))).toBeLessThan(4)
}

async function expectFloatingAbove(upper: ReturnType<Page['locator']>, lower: ReturnType<Page['locator']>) {
  await expect.poll(async () => {
    const upperBox = await upper.boundingBox()
    const lowerBox = await lower.boundingBox()
    if (!upperBox || !lowerBox) return -1
    return lowerBox.y - upperBox.y - upperBox.height
  }).toBeGreaterThanOrEqual(8)
}

test('creation mode renders the selected OFM vector layer', async ({ page }) => {
  await mockRuntime(page)
  await page.goto('/')
  await page.getByRole('button', { name: 'Plan a route' }).click()
  await expectVectorMap(page)
  await expect(page.locator('.terrain-fab-label')).toHaveText('OFM')
})

test('Enduro and session BRF profiles are selectable without persisting the upload', async ({ page }) => {
  await mockRuntime(page)
  let released = false
  await page.route(/\/config$/, route => json(route, {
    offline: { enabled: true, mode: 'auto', routing: '/offline/routing' },
    services: { broomRoute: '/routing/broom/route' },
    maps: { openfreemap: { style: '/map/openfreemap/style.json', allowBulk: true } },
  }))
  await page.route(/\/offline\/routing$/, route => json(route, { enabled: true, ready: true, regionId: 'aragon', generationId: 'one', cached: [], cacheBytes: 0 }))
  await page.route(/\/offline\/routing\/suggest$/, route => json(route, { region: { regionId: 'aragon', name: 'Aragón', installed: true, active: true } }))
  await page.route(/\/offline\/routing\/profile$/, async route => {
    expect(route.request().postDataJSON()).toMatchObject({ source: 'session BRF test' })
    await json(route, { id: 'session-test-token', warnings: [] }, 201)
  })
  await page.route(/\/offline\/routing\/profile\/release$/, async route => {
    expect(route.request().postDataJSON()).toEqual({ id: 'session-test-token' })
    released = true
    await route.fulfill({ status: 204 })
  })
  await page.goto('/')
  await page.getByRole('button', { name: 'Plan a route' }).click()
  await page.getByRole('button', { name: 'Enduro', exact: true }).click()
  await expect(page.getByRole('button', { name: 'Enduro', exact: true })).toHaveClass(/active/)
  await expect(page.getByRole('checkbox', { name: 'Restricted access' })).toBeEnabled()
  await expect(page.getByRole('button', { name: 'Upload session BRF' })).toBeEnabled()
  await page.getByLabel('Session BRF profile').setInputFiles({ name: 'Weekend.brf', mimeType: 'text/plain', buffer: Buffer.from('session BRF test') })
  await expect(page.getByRole('button', { name: 'Weekend', exact: true })).toHaveClass(/active/)
  await expect(page.getByRole('checkbox', { name: 'Restricted access' })).toBeDisabled()
  expect(await page.evaluate(() => JSON.stringify(localStorage))).not.toContain('session-test-token')
  await page.getByRole('button', { name: 'Remove session profile' }).click()
  await expect.poll(() => released).toBe(true)
  await expect(page.getByRole('button', { name: 'Weekend', exact: true })).toHaveCount(0)
  await page.reload()
  await page.getByRole('button', { name: 'Plan a route' }).click()
  await expect(page.getByRole('button', { name: 'Weekend', exact: true })).toHaveCount(0)
})

test('permit access reroutes and routing upgrades keep the planner usable', async ({ page }) => {
  await mockRuntime(page)
  let upgrading = true
  let paused = false
  const requests: { profile: string; accessPermit: boolean }[] = []
  const status = () => ({
    enabled: true, ready: true, regionId: 'aragon', generationId: upgrading ? 'old' : 'new',
    upgradePending: upgrading, cached: [], cacheBytes: 4096,
    job: { id: 'upgrade', regionId: 'aragon', upgrading: true, state: upgrading ? paused ? 'cancelled' : 'running' : 'complete', phase: 'build' },
  })
  await page.route(/\/config$/, route => json(route, {
    offline: { enabled: true, mode: 'auto', routing: '/offline/routing' },
    services: { broomRoute: '/routing/broom/route' },
    maps: { openfreemap: { style: '/map/openfreemap/style.json', allowBulk: true } },
  }))
  await page.route(/\/offline\/routing$/, route => json(route, status()))
  await page.route(/\/offline\/routing\/suggest$/, route => json(route, { region: { regionId: 'aragon', name: 'Aragón', installed: true, active: true } }))
  await page.route(/\/offline\/routing\/cancel$/, route => { paused = true; return json(route, status()) })
  await page.route(/\/offline\/routing\/prepare$/, route => {
    expect(route.request().postDataJSON()).toMatchObject({ regionId: 'aragon', update: false })
    paused = false
    return json(route, status(), 202)
  })
  await page.route(/\/routing\/broom\/route$/, route => {
    requests.push(route.request().postDataJSON())
    return json(route, {
      schemaVersion: 1, engine: 'Broom', engineVersion: '0.6.0',
      coordinates: [{ lat: 41.6, lon: -0.9 }, { lat: 41.7, lon: -0.8 }],
      elevations: [null, null], segments: [], distanceMeters: 15000, durationSeconds: 900,
    })
  })
  await page.goto('/')
  await page.getByRole('button', { name: 'Plan a route' }).click()
  const permit = page.getByRole('checkbox', { name: 'Restricted access' })
  await expect(permit).not.toBeChecked()
  const pill = page.locator('.routing-download-pill')
  await expect(pill).toContainText('Updating routing')
  await pill.click()
  await expect(page.getByText('Your downloaded region remains available', { exact: false })).toBeVisible()
  await page.getByRole('button', { name: 'Pause routing update' }).click()
  await page.getByRole('button', { name: 'Resume routing update' }).click()
  await pill.click()
  await page.locator('.leaflet-container').click({ position: { x: 170, y: 150 } })
  await page.locator('.leaflet-container').click({ position: { x: 240, y: 210 } })
  await expect.poll(() => requests.at(-1)).toMatchObject({ profile: 'mixed', accessPermit: false })
  await permit.check()
  await expect.poll(() => requests.at(-1)).toMatchObject({ profile: 'mixed', accessPermit: true })
  await page.getByRole('button', { name: 'Enduro', exact: true }).click()
  await expect.poll(() => requests.at(-1)).toMatchObject({ profile: 'enduro', accessPermit: true })
  await permit.uncheck()
  await expect.poll(() => requests.at(-1)).toMatchObject({ profile: 'enduro', accessPermit: false })
  const beforeUpgrade = requests.length
  upgrading = false
  await expect(pill).toHaveText('Routing ready')
  await expect.poll(() => requests.length).toBeGreaterThan(beforeUpgrade)
  await permit.check()
  await page.getByRole('button', { name: 'Clear', exact: true }).click()
  await expect(permit).not.toBeChecked()
})

test('planner suggests viewport routing downloads and reveals progress', async ({ page }, testInfo) => {
  await mockRuntime(page)
  let ready = false
  let running = false
  let complete = false
  let suggestedBounds: unknown
  let requestedRegion: string | undefined
  let routeCalls = 0
  const status = () => ({
    enabled: true,
    ready,
    ...(ready ? { regionId: 'spain/aragon', generationId: 'generation-1', name: 'Aragon' } : {}),
    ...(running ? { job: { id: 'routing-1', regionId: 'spain/aragon', state: 'running', phase: 'elevation', item: 'N41E002', done: 1, total: 2, completedItems: 12, itemsTotal: 55, itemsDownloaded: 8, itemsReused: 4 } } : {}),
    cached: ready ? [{ regionId: 'spain/aragon', generationId: 'generation-1', name: 'Aragon', selected: true, pinned: false }] : [],
    cacheBytes: ready ? 4096 : 0,
    pinnedBytes: 0,
    inUseBytes: ready ? 2048 : 0,
    reclaimableBytes: 0,
  })
  await page.route(/\/config$/, route => json(route, {
    offline: {
      enabled: true,
      mode: 'auto',
      status: '/offline/status',
      packs: '/offline/packs',
      modeControl: '/offline/mode',
      routing: '/offline/routing',
    },
    services: { places: '/places/search', broomRoute: '/routing/broom/route' },
    maps: { raster: { osm: '/e2e/osm/{z}/{x}/{y}.png' }, openfreemap: { style: '/map/openfreemap/style.json', allowBulk: true } },
  }))
  await page.route(/\/offline\/routing\/prepare$/, async route => {
    requestedRegion = (route.request().postDataJSON() as { regionId: string }).regionId
    running = true
    await json(route, status(), 202)
  })
  await page.route(/\/offline\/routing\/plan$/, route => json(route, {
    pbfBytes: 1000, estimatedBytes: null, tilesKnown: true, tilesTotal: 55, tilesCached: 4, tilesMissing: 51,
  }))
  await page.route(/\/offline\/routing\/suggest$/, async route => {
    suggestedBounds = (route.request().postDataJSON() as { bbox: unknown }).bbox
    await json(route, { region: { regionId: 'spain/aragon', name: 'Aragon', installed: ready, active: ready } })
  })
  await page.route(/\/offline\/routing$/, async route => {
    if (running && complete) {
      running = false
      ready = true
    }
    await json(route, status())
  })
  await page.route(/\/routing\/broom\/route$/, async route => {
    routeCalls++
    expect(ready).toBe(true)
    await json(route, {
      schemaVersion: 1, engine: 'Broom', engineVersion: '0.4.0',
      coordinates: [{ lat: 41.6, lon: -0.9 }, { lat: 41.7, lon: -0.8 }],
      elevations: [null, null], segments: [], distanceMeters: 15000, durationSeconds: 900,
    })
  })

  await page.goto('/')
  await page.getByRole('button', { name: 'Plan a route' }).click()
  await expect(page.getByRole('combobox', { name: 'Broom routing region' })).toHaveCount(0)
  const pill = page.locator('.routing-download-pill')
  await page.locator('.leaflet-container').scrollIntoViewIfNeeded()
  await expect(pill).toContainText('Aragon')
  await expectFloatingAbove(page.locator('.leaflet-control-zoom'), page.locator('.creation-poi-strip'))
  await page.locator('.leaflet-container').hover({ position: { x: 100, y: 100 } })
  const readout = page.locator('.map-cursor-readout')
  if (await readout.isVisible()) await expectFloatingAbove(pill, readout)
  const mapBounds = await page.locator('.leaflet-container').boundingBox()
  const fullMap = await page.getByRole('button', { name: 'Full map — hide the panels' }).boundingBox()
  expect(fullMap!.y - mapBounds!.y).toBeLessThanOrEqual(12)
  expect(fullMap!.x - mapBounds!.x).toBeLessThanOrEqual(12)
  expect(suggestedBounds).toMatchObject({ south: expect.any(Number), west: expect.any(Number), north: expect.any(Number), east: expect.any(Number) })
  expect(requestedRegion).toBeUndefined()
  await expect(page.getByRole('region', { name: 'Offline routing download' })).toHaveCount(0)
  await pill.click()
  await expect(page.locator('.routing-download-estimate')).toContainText('55 terrain tiles · 4 cached · 51 to fetch')
  await page.getByRole('button', { name: 'Download Aragon' }).click()
  expect(requestedRegion).toBe('spain/aragon')
  await expect(page.getByRole('progressbar', { name: 'Routing download progress' })).toHaveAttribute('value', '50')
  await pill.click()
  await expect(page.getByRole('progressbar')).toHaveCount(0)
  await expect(pill).toContainText('12/55 tiles')
  await pill.click()
  await expect(page.getByRole('button', { name: 'Cancel download' })).toBeVisible()
  await expect(page.locator('.routing-download-transfer')).toContainText('8 downloaded · 4 reused')
  await page.screenshot({ path: testInfo.outputPath('routing-progress.png') })
  await pill.click()
  await page.locator('.leaflet-container').click({ position: { x: 170, y: 150 } })
  await page.locator('.leaflet-container').click({ position: { x: 240, y: 210 } })
  expect(routeCalls).toBe(0)
  complete = true
  await expect(pill).toHaveText('Routing ready')
  await expect.poll(() => routeCalls).toBe(1)
})

test('MCP bridge draws planner routes and reports the active view', async ({ page }) => {
  await mockRuntime(page)
  const commands = [
    { id: 'command-mode', name: 'switch_mode', arguments: { mode: 'planner' } },
    {
      id: 'command-1',
      name: 'plan_route',
      arguments: {
        points: [{ lat: 42.82, lon: -2.75 }, { lat: 42.9, lon: -2.58 }],
        mode: 'append',
        profile: 'mixed',
        fitView: true,
      },
    },
  ]
  const results: Record<string, unknown>[] = []
  let commandIndex = 0
  let resultAttempts = 0
  let publishedPlannerRoute: { screen?: string; points?: number } | undefined

  await page.route(/\/mcp\/browser\/session$/, route => json(route, { enabled: true }))
  await page.route(/\/mcp\/browser\/view$/, async route => {
    const candidate = route.request().postDataJSON() as Record<string, unknown>
    const snapshot = candidate.snapshot as { screen?: string; planner?: { routePoints?: unknown[] } }
    if (snapshot?.planner?.routePoints?.length === 2) {
      publishedPlannerRoute = { screen: snapshot.screen, points: snapshot.planner.routePoints.length }
    }
    await route.fulfill({ status: 204 })
  })
  await page.route(/\/mcp\/browser\/events/, route =>
    fulfillCommandStream(route, commands[commandIndex]))
  await page.route(/\/mcp\/browser\/result$/, async route => {
    resultAttempts++
    if (resultAttempts === 1) {
      await route.fulfill({ status: 503 })
      return
    }
    results.push(route.request().postDataJSON() as Record<string, unknown>)
    commandIndex++
    await route.fulfill({ status: 204 })
  })

  await page.goto('/')
  await expect(page.locator('.creation-screen')).toBeVisible()
  await expect.poll(() => results.length).toBe(2)
  expect(resultAttempts).toBe(3)
  expect(results[0]).toMatchObject({
    id: 'command-mode',
    result: { ok: true, mode: 'planner', screen: 'creation' },
  })
  expect(results[1]).toMatchObject({
    id: 'command-1',
    result: { ok: true, screen: 'creation', routePointCount: 2 },
  })
  // The bridge guarantee is that the snapshot produced by a command is
  // published before that command is acknowledged, so assert on what was
  // published rather than on whichever heartbeat happens to land last.
  await expect.poll(() => publishedPlannerRoute).toEqual({ screen: 'creation', points: 2 })
})

test('MCP session overlays survive map modes and semantic tools stay mode-specific', async ({ page }) => {
  await mockRuntime(page)
  const commands = [
    { id: 'mode-explore', name: 'switch_mode', arguments: { mode: 'explore' } },
    {
      id: 'wrong-mode-route',
      name: 'plan_route',
      arguments: { points: [{ lat: 42.82, lon: -2.75 }, { lat: 42.9, lon: -2.58 }] },
    },
    {
      id: 'map-track',
      name: 'draw_map_track',
      arguments: { points: [{ lat: 42.82, lon: -2.75 }, { lat: 42.9, lon: -2.58 }], fitView: false },
    },
    {
      id: 'map-markers',
      name: 'set_map_markers',
      arguments: { markers: [{ lat: 42.85, lon: -2.67, marker: 'repair', name: 'Workshop' }], fitView: false },
    },
  ]
  const results: Record<string, unknown>[] = []
  let commandIndex = 0
  let latestView: Record<string, unknown> | undefined
  let latestViewSequence = 0

  await page.route(/\/mcp\/browser\/session$/, route => json(route, { enabled: true }))
  await page.route(/\/mcp\/browser\/view$/, async route => {
    const candidate = route.request().postDataJSON() as Record<string, unknown>
    const sequence = typeof candidate.sequence === 'number' ? candidate.sequence : 0
    if (sequence > latestViewSequence) {
      latestView = candidate
      latestViewSequence = sequence
    }
    await route.fulfill({ status: 204 })
  })
  await page.route(/\/mcp\/browser\/events/, route =>
    fulfillCommandStream(route, commands[commandIndex]))
  await page.route(/\/mcp\/browser\/result$/, async route => {
    results.push(route.request().postDataJSON() as Record<string, unknown>)
    commandIndex++
    await route.fulfill({ status: 204 })
  })

  await page.goto('/')
  await expect.poll(() => results.length).toBe(commands.length)
  expect(results[1]).toMatchObject({
    id: 'wrong-mode-route',
    error: 'plan_route requires planner mode; use switch_mode first',
  })
  await expect(page.getByTestId('explore-screen')).toBeVisible()
  await expect(page.locator('.session-map-track')).toHaveCount(1)
  await expect(page.locator('.waypoint-marker-repair')).toHaveCount(1)
  await expect.poll(() => {
    const state = latestView?.snapshot as {
      screen?: string
      markerCatalog?: unknown[]
      mapOverlays?: { persisted?: boolean; track?: unknown[]; markers?: unknown[] }
    } | undefined
    return {
      screen: state?.screen,
      catalog: state?.markerCatalog?.length,
      persisted: state?.mapOverlays?.persisted,
      track: state?.mapOverlays?.track?.length,
      markers: state?.mapOverlays?.markers?.length,
    }
  }).toEqual({ screen: 'explore', catalog: 17, persisted: false, track: 2, markers: 1 })

  await page.getByRole('button', { name: 'Back to home' }).click()
  await page.getByRole('button', { name: 'Plan a route' }).click()
  await expect(page.locator('.session-map-track')).toHaveCount(1)
  await expect(page.locator('.waypoint-marker-repair')).toHaveCount(1)
})

test('MCP reports and moves the Explore map', async ({ page }) => {
  await mockRuntime(page)
  const command = {
    id: 'command-explore-map',
    name: 'set_map_view',
    arguments: { lat: 42.85, lon: -2.67, zoom: 12 },
  }
  let deliverCommand = false
  let commandAcknowledged = false
  let latestView: Record<string, unknown> | undefined
  let latestViewSequence = 0
  let result: Record<string, unknown> | undefined

  await page.route(/\/mcp\/browser\/session$/, route => json(route, { enabled: true }))
  await page.route(/\/mcp\/browser\/view$/, async route => {
    const candidate = route.request().postDataJSON() as Record<string, unknown>
    const sequence = typeof candidate.sequence === 'number' ? candidate.sequence : 0
    if (sequence > latestViewSequence) {
      latestView = candidate
      latestViewSequence = sequence
    }
    await route.fulfill({ status: 204 })
  })
  await page.route(/\/mcp\/browser\/events/, route =>
    fulfillCommandStream(route, deliverCommand && !commandAcknowledged ? command : undefined))
  await page.route(/\/mcp\/browser\/result$/, async route => {
    result = route.request().postDataJSON() as Record<string, unknown>
    commandAcknowledged = true
    await route.fulfill({ status: 204 })
  })

  await page.goto('/')
  await page.getByRole('button', { name: 'Explore map' }).click()
  await expect(page.getByTestId('explore-map')).toBeVisible()
  await expect.poll(() => {
    const state = latestView?.snapshot as { screen?: string; map?: unknown } | undefined
    return { screen: state?.screen, hasMap: state?.map != null }
  }).toEqual({ screen: 'explore', hasMap: true })

  deliverCommand = true
  await expect.poll(() => commandAcknowledged).toBe(true)
  expect(result).toMatchObject({
    id: command.id,
    result: { ok: true, center: { lat: 42.85, lon: -2.67 }, zoom: 12 },
  })
  await expect.poll(() => {
    const state = latestView?.snapshot as {
      screen?: string
      map?: { center?: { lat?: number; lon?: number }; zoom?: number; bounds?: unknown }
    } | undefined
    return {
      screen: state?.screen,
      lat: state?.map?.center?.lat,
      lon: state?.map?.center?.lon,
      zoom: state?.map?.zoom,
      hasBounds: state?.map?.bounds != null,
    }
  }).toEqual({ screen: 'explore', lat: 42.85, lon: -2.67, zoom: 12, hasBounds: true })
  await expect.poll(() => page.evaluate(() => localStorage.getItem('gpx-explore-view'))).toContain('42.85')
})

test('manual offline mode can be toggled while the server is connected', async ({ page }) => {
  await page.addInitScript(() => localStorage.setItem('gpx-base-layer', 'satellite'))
  const runtime = await mockRuntime(page)
  await page.goto('/')

  const toggle = page.getByTestId('offline-mode-toggle')
  await expect(toggle).toContainText('Work offline')
  const externalRequests: string[] = []
  const appOrigin = new URL(page.url()).origin
  page.on('request', request => {
    const url = new URL(request.url())
    if (url.origin !== appOrigin && url.protocol !== 'data:' && url.protocol !== 'blob:') {
      externalRequests.push(request.url())
    }
  })
  await toggle.click()
  await expect(toggle).toContainText('Go online')
  await expect(toggle).toHaveAttribute('aria-pressed', 'true')
  expect(runtime.getOfflineMode()).toBe('cache-only')

  await page.getByRole('button', { name: 'Explore map' }).click()
  await expectVectorMap(page)
  expect(externalRequests).toEqual([])

  await toggle.click()
  await expect(toggle).toContainText('Work offline')
  await expect(toggle).toHaveAttribute('aria-pressed', 'false')
  expect(runtime.getOfflineMode()).toBe('auto')
})

test('notifications stack at the bottom center', async ({ page }) => {
  const runtime = await mockRuntime(page)
  await page.goto('/')

  const toggle = page.getByTestId('offline-mode-toggle')
  await toggle.click()
  await expect.poll(runtime.getOfflineMode).toBe('cache-only')
  await toggle.click()
  await expect.poll(runtime.getOfflineMode).toBe('auto')

  const container = page.locator('.notification-container')
  const notifications = container.locator('.notification')
  await expect(notifications).toHaveCount(2)
  const boxes = await notifications.evaluateAll(elements => elements.map(element => {
    const box = element.getBoundingClientRect()
    return { top: box.top, bottom: box.bottom }
  }))
  expect(boxes[1].top).toBeGreaterThanOrEqual(boxes[0].bottom + 9)

  const containerBox = await container.boundingBox()
  const viewport = page.viewportSize()
  expect(containerBox).not.toBeNull()
  expect(viewport).not.toBeNull()
  expect(Math.abs(containerBox!.x + containerBox!.width / 2 - viewport!.width / 2)).toBeLessThan(2)
  const bottomGap = viewport!.height - containerBox!.y - containerBox!.height
  expect(bottomGap).toBeGreaterThanOrEqual(18)
  expect(bottomGap).toBeLessThanOrEqual(32)
})

test('startup cache-only replaces a persisted online-only map without external traffic', async ({ page }) => {
  await page.addInitScript(() => localStorage.setItem('gpx-base-layer', 'satellite'))
  await mockRuntime(page, { startupCacheOnly: true })
  const requests: string[] = []
  page.on('request', request => requests.push(request.url()))
  await page.goto('/')

  const toggle = page.getByTestId('offline-mode-toggle')
  await expect(toggle).toContainText('Offline')
  await expect(toggle).toBeDisabled()
  await page.getByRole('button', { name: 'Explore map' }).click()
  await expectVectorMap(page)

  const appOrigin = new URL(page.url()).origin
  expect(requests.filter(raw => {
    const url = new URL(raw)
    return url.origin !== appOrigin && url.protocol !== 'data:' && url.protocol !== 'blob:'
  })).toEqual([])
})

test('cache-only vector tile misses do not show an error diagnostic', async ({ page }) => {
  const runtime = await mockRuntime(page, {
    style: BROKEN_TILE_STYLE,
    vectorMissDuringOfflineTransition: true,
  })
  await page.goto('/')
  await page.getByRole('button', { name: 'Plan a route' }).click()

  await expectVectorMap(page)
  await expect.poll(runtime.getVectorRequests).toBeGreaterThan(0)
  await page.getByTestId('offline-mode-toggle').click()
  await expect.poll(runtime.getOfflineMode).toBe('cache-only')
  await expect(page.getByTestId('vector-map-diagnostic')).toHaveCount(0)
  await expect(page.locator('.notification-error')).toHaveCount(0)
})

test('server-relative OFM resources do not produce a false style failure', async ({ page }) => {
  const vectorLogs: string[] = []
  page.on('console', message => {
    if (message.type() === 'error' && message.text().startsWith('[vector-map]')) vectorLogs.push(message.text())
  })
  await mockRuntime(page, { style: PROXIED_STYLE })
  await page.goto('/')
  await page.getByRole('button', { name: 'Plan a route' }).click()

  await expectVectorMap(page)
  await page.waitForTimeout(500)
  await expect(page.getByTestId('vector-map-diagnostic')).toHaveCount(0)
  expect(vectorLogs).toEqual([])
})

test('Escape dismisses planner overlays', async ({ page }) => {
  await mockRuntime(page)
  await page.goto('/')
  await page.getByRole('button', { name: 'Plan a route' }).click()
  await expectVectorMap(page)

  const search = page.getByPlaceholder('Search village or place…')
  await search.fill('Vitoria-Gasteiz')
  await search.press('Enter')
  await expect(page.locator('.place-results')).toBeVisible()
  await page.keyboard.press('Escape')
  await expect(page.locator('.place-results')).toHaveCount(0)

  const addPlace = page.getByRole('button', { name: 'Add place' })
  await addPlace.click()
  await expect(page.getByRole('button', { name: 'Click the map…' })).toBeVisible()
  await page.keyboard.press('Escape')
  await expect(addPlace).toBeVisible()

  await page.locator('.terrain-fab').click()
  await expect(page.locator('.terrain-controls')).toBeVisible()
  await page.keyboard.press('Escape')
  await expect(page.locator('.terrain-controls')).toHaveCount(0)

  const fullMap = page.locator('.map-full-toggle')
  await fullMap.click()
  await expect(page.locator('.creation-screen')).toHaveClass(/creation-screen--full/)
  await page.keyboard.press('Escape')
  await expect(page.locator('.creation-screen')).not.toHaveClass(/creation-screen--full/)
})

test('a critical OFM source failure stays vector until raster is manually selected', async ({ page }) => {
  const vectorLogs: string[] = []
  page.on('console', message => {
    if (message.type() === 'error' && message.text().startsWith('[vector-map]')) vectorLogs.push(message.text())
  })
  const runtime = await mockRuntime(page, { style: BROKEN_SOURCE_STYLE })
  await page.goto('/')
  await page.getByRole('button', { name: 'Plan a route' }).click()

  await expect(page.locator('.terrain-fab-label')).toHaveText('OFM')
  await expect(page.locator('.maplibregl-canvas')).toBeVisible()
  const diagnostic = page.getByTestId('vector-map-diagnostic')
  await expect(diagnostic).toBeVisible()
  await expect(diagnostic).toContainText('OpenFreeMap is incomplete')
  await expect(diagnostic).toContainText('HTTP 503')
  await expect(diagnostic).toContainText('roads')
  await expect(diagnostic).toContainText(/choose another map layer manually/i)
  await expect(page.locator('.leaflet-tile-pane img[src*="/e2e/osm/"]')).toHaveCount(0)
  expect(runtime.getOsmRequests()).toBe(0)
  await expect.poll(() => vectorLogs.some(message => message.includes('HTTP 503') && message.includes('roads'))).toBe(true)

  await page.keyboard.press('Escape')
  await expect(diagnostic).toHaveCount(0)
  await page.waitForTimeout(500)
  await expect(page.getByTestId('vector-map-diagnostic')).toHaveCount(0)

  await page.locator('.terrain-fab').click()
  await page.getByRole('button', { name: 'OSM', exact: true }).click()
  await expect(page.locator('.leaflet-tile-pane img[src*="/e2e/osm/"]').first()).toBeVisible()
  expect(runtime.getOsmRequests()).toBeGreaterThan(0)
  expect(await page.evaluate(() => localStorage.getItem('gpx-base-layer'))).toBe('osm')
  await expect(diagnostic).toHaveCount(0)

  await page.reload()
  await page.getByRole('button', { name: 'Plan a route' }).click()
  await expect(page.locator('.terrain-fab-label')).toHaveText('OSM')
  await expect(page.locator('.maplibregl-canvas')).toHaveCount(0)
  await expect(page.locator('.leaflet-tile-pane img[src*="/e2e/osm/"]').first()).toBeVisible()
})

test('an OFM style HTTP failure reports the backend reason without selecting raster', async ({ page }) => {
  const vectorLogs: string[] = []
  page.on('console', message => {
    if (message.type() === 'error' && message.text().startsWith('[vector-map]')) vectorLogs.push(message.text())
  })
  const runtime = await mockRuntime(page, {
    styleFailure: {
      status: 503,
      body: {
        detail: 'Map proxy is unavailable',
        code: 'upstream_http_status',
        scope: 'maps-openfreemap',
        stage: 'style',
        upstreamStatus: 503,
      },
    },
  })
  await page.goto('/')
  await page.getByRole('button', { name: 'Plan a route' }).click()

  const diagnostic = page.getByTestId('vector-map-diagnostic')
  await expect(page.locator('.terrain-fab-label')).toHaveText('OFM')
  await expect(page.locator('.maplibregl-canvas')).toHaveCount(0)
  await expect(diagnostic).toContainText('Map proxy is unavailable')
  await expect(diagnostic).toContainText('OpenFreeMap could not load')
  await expect(diagnostic).toContainText('HTTP 503')
  await expect(diagnostic.locator('code')).toHaveText('/map/openfreemap/style.json')
  expect(runtime.getOsmRequests()).toBe(0)
  await expect.poll(() => vectorLogs.some(message => message.includes('Map proxy is unavailable') && message.includes('HTTP 503'))).toBe(true)
})

test('failed OFM vector tiles are diagnosed without replacing the vector canvas', async ({ page }) => {
  const runtime = await mockRuntime(page, { style: BROKEN_TILE_STYLE })
  await page.goto('/')
  await page.getByRole('button', { name: 'Plan a route' }).click()

  const diagnostic = page.getByTestId('vector-map-diagnostic')
  await expect(page.locator('.maplibregl-canvas')).toBeVisible()
  await expect(page.locator('.terrain-fab-label')).toHaveText('OFM')
  await expect(diagnostic).toContainText('OpenFreeMap tile "roads"')
  await expect(diagnostic).toContainText('OpenFreeMap is incomplete')
  await expect(diagnostic).toContainText('HTTP 503')
  expect(runtime.getOsmRequests()).toBe(0)
})

test('missing WebGL2 leaves OFM selected and never starts raster tiles', async ({ page }) => {
  const runtime = await mockRuntime(page, { webGL2Unavailable: true })
  await page.goto('/')
  await page.getByRole('button', { name: 'Plan a route' }).click()

  await expect(page.locator('.terrain-fab-label')).toHaveText('OFM')
  await expect(page.locator('.maplibregl-canvas')).toHaveCount(0)
  await expect(page.getByTestId('vector-map-diagnostic')).toContainText('WebGL2')
  expect(runtime.getOsmRequests()).toBe(0)
})

test('unavailable elevation leaves other route resources prepared', async ({ page }) => {
  const runtime = await mockRuntime(page, { elevationUnavailable: true })
  await page.goto('/')
  await page.getByRole('button', { name: 'Open a GPX file' }).click()
  await page.locator('input[type="file"]').setInputFiles({
    name: 'browser-trip.gpx',
    mimeType: 'application/gpx+xml',
    buffer: Buffer.from(GPX),
  })

  const offlineButton = page.getByTestId('offline-route-button')
  await expect(offlineButton).toContainText('Offline partial')
  await offlineButton.click()
  const panel = page.getByTestId('offline-route-panel')
  await expect(panel.locator('.offline-resource[data-state="ready"]')).toHaveCount(5)
  await expect(panel.locator('[data-resource="elevation"]')).toContainText('Persistent terrain tiles are not configured')
  expect(runtime.getPackRequest()?.scopes).toEqual(['pois', 'fuel'])
})

test('GPX waypoint marker types render and remain editable', async ({ page }) => {
  await mockRuntime(page)
  await page.goto('/')
  await page.getByRole('button', { name: 'Open a GPX file' }).click()
  await page.locator('input[type="file"]').setInputFiles({
    name: 'typed-waypoint.gpx',
    mimeType: 'application/gpx+xml',
    buffer: Buffer.from(GPX_WITH_WAYPOINT),
  })

  const fuelMarker = page.locator('.waypoint-marker-fuel')
  await expect(fuelMarker).toHaveCount(1)
  await fuelMarker.click()
  const markerType = page.getByLabel('Waypoint marker type')
  await expect(markerType).toHaveValue('fuel')
  await markerType.selectOption('camp')
  await expect(page.locator('.waypoint-marker-camp')).toHaveCount(1)
  await expect(page.locator('.waypoint-marker-fuel')).toHaveCount(0)
})

test('loading a GPX automatically prepares and reports route resources', async ({ page }) => {
  const runtime = await mockRuntime(page)
  await page.goto('/')
  await page.getByRole('button', { name: 'Open a GPX file' }).click()
  await page.locator('input[type="file"]').setInputFiles({
    name: 'browser-trip.gpx',
    mimeType: 'application/gpx+xml',
    buffer: Buffer.from(GPX),
  })

  await expectVectorMap(page)
  const zoomControl = page.locator('.map-wrapper-with-elevation .leaflet-control-zoom')
  const zoomIn = zoomControl.locator('.leaflet-control-zoom-in')
  const profile = page.locator('.elevation-profile')
  await expect(profile).toHaveClass(/collapsed/)
  await expectFloatingAbove(zoomControl, profile)
  const zoomBeforeHover = await zoomIn.boundingBox()
  await zoomIn.hover()
  const zoomAfterHover = await zoomIn.boundingBox()
  expect(zoomBeforeHover).not.toBeNull()
  expect(zoomAfterHover).not.toBeNull()
  expect(Math.abs(zoomAfterHover!.width - zoomBeforeHover!.width)).toBeLessThan(1)
  expect(Math.abs(zoomAfterHover!.height - zoomBeforeHover!.height)).toBeLessThan(1)

  await profile.locator('.elevation-profile-header').click()
  await expect(profile).not.toHaveClass(/collapsed/)
  await expectFloatingAbove(zoomControl, profile)
  await page.keyboard.press('Escape')
  await expect(profile).toHaveClass(/collapsed/)
  await expectFloatingAbove(zoomControl, profile)

  const terrainButton = page.locator('.terrain-fab')
  const offlineButton = page.getByTestId('offline-route-button')
  await expect(offlineButton).toContainText('Offline ready')
  await expectStackedBelow(terrainButton, offlineButton)

  expect(runtime.getPackRequest()).toMatchObject({
    name: 'Route: Browser trip',
    automatic: true,
    layers: ['openfreemap'],
    scopes: ['elevation', 'pois', 'fuel'],
  })
  expect(runtime.getPackRequest()?.route).toHaveLength(3)

  await terrainButton.click()
  await expectStackedBelow(page.locator('.terrain-controls'), offlineButton)
  await offlineButton.click()
  const panel = page.getByTestId('offline-route-panel')
  await expect(panel).toBeVisible()
  await expectStackedBelow(offlineButton, panel)
  await expect(panel.locator('.offline-resource[data-state="ready"]')).toHaveCount(6)
  await expect(panel.locator('[data-resource="water"]')).toContainText('7 local records')

  const readyRing = panel.locator('.offline-progress-ring.is-ready')
  await expect(readyRing).toHaveCSS('border-radius', '50%')
  await expect(readyRing.locator('.offline-progress-ring-value')).toHaveCSS('filter', 'none')

  await page.keyboard.press('Escape')
  await expect(panel).toHaveCount(0)
  await expect(page.locator('.terrain-controls')).toBeVisible()
  await page.keyboard.press('Escape')
  await expect(page.locator('.terrain-controls')).toHaveCount(0)

  const tools = page.getByRole('button', { name: 'Tools' })
  await tools.click()
  const editTools = page.locator('.edit-toolbar')
  await expect(editTools).toBeVisible()
  await editTools.getByRole('button', { name: 'Select range' }).click()
  await expect(profile).not.toHaveClass(/collapsed/)
  await page.keyboard.press('Escape')
  await expect(editTools.getByRole('button', { name: 'Select range' })).toBeVisible()
  await expect(editTools).toBeVisible()
  await page.keyboard.press('Escape')
  await expect(profile).toHaveClass(/collapsed/)
  await expect(editTools).toBeVisible()
  await page.keyboard.press('Escape')
  await expect(editTools).toHaveCount(0)

  const waypoint = page.getByRole('button', { name: 'Waypoint' })
  await waypoint.click()
  await expect(page.getByRole('button', { name: 'Click the map…' })).toBeVisible()
  await page.keyboard.press('Escape')
  await expect(waypoint).toBeVisible()
})

test('explore caches deliberate place searches and downloads a drawn map area', async ({ page }) => {
  const runtime = await mockRuntime(page)
  await page.goto('/')
  await page.getByRole('button', { name: 'Explore map' }).click()

  await expect(page.getByTestId('explore-screen')).toBeVisible()
  await expectVectorMap(page)
  expect(runtime.getPlaceRequests()).toBe(0)

  const actions = page.locator('.explore-actions')
  const topButtons = [
    page.locator('.explore-home'),
    actions.getByTestId('offline-area-button'),
    actions.getByTestId('offline-areas-button'),
    actions.locator('.offline-coverage-toggle'),
    actions.getByTestId('offline-mode-toggle'),
    actions.locator('.theme-toggle'),
  ]
  for (const button of topButtons) {
    await expect(button).toBeVisible()
    await expect(button).toHaveCSS('border-radius', '12px')
  }
  const topButtonHeights = await Promise.all(topButtons.map(button => button.evaluate(element => element.getBoundingClientRect().height)))
  expect(new Set(topButtonHeights).size).toBe(1)

  await page.locator('.terrain-fab').click()
  await expect(page.locator('.terrain-controls')).toBeVisible()
  await page.keyboard.press('Escape')
  await expect(page.locator('.terrain-controls')).toHaveCount(0)

  await page.getByTestId('offline-area-button').click()
  await expect(page.getByTestId('offline-area-panel')).toBeVisible()
  await page.keyboard.press('Escape')
  await expect(page.getByTestId('offline-area-panel')).toHaveCount(0)

  const search = page.getByRole('searchbox', { name: 'Search OpenStreetMap places' })
  await search.fill('Vitoria-Gasteiz')
  await page.getByRole('button', { name: 'Search', exact: true }).click()
  const result = page.getByRole('option', { name: /Vitoria-Gasteiz/ })
  await expect(result).toBeVisible()
  await expect(page.locator('.explore-search-status em')).toHaveText('cached')
  expect(runtime.getPlaceRequests()).toBe(1)

  await page.keyboard.press('Escape')
  await expect(page.getByRole('listbox', { name: 'Place results' })).toHaveCount(0)

  await search.fill('  vitoria-gasteiz  ')
  await page.getByRole('button', { name: 'Search', exact: true }).click()
  await expect(result).toBeVisible()
  expect(runtime.getPlaceRequests()).toBe(1)
  await result.click()
  await expect(page.locator('.explore-place-card')).toContainText('Vitoria-Gasteiz')
  expect(parseFloat(await page.locator('.explore-place-card strong').evaluate(element => getComputedStyle(element).fontSize))).toBeGreaterThanOrEqual(16)
  await expect(page.getByRole('listbox', { name: 'Place results' })).toHaveCount(0)
  await expect(page.locator('.leaflet-bottom.leaflet-right .leaflet-control-zoom')).toBeVisible()

  await page.locator('.leaflet-marker-icon').click()
  await expect(page.locator('.leaflet-popup')).toBeVisible()
  await page.getByTestId('offline-area-button').click()
  await expect(page.getByTestId('offline-area-panel')).toBeVisible()
  await page.keyboard.press('Escape')
  await expect(page.locator('.leaflet-popup')).toHaveCount(0)
  await expect(page.getByTestId('offline-area-panel')).toBeVisible()
  await page.keyboard.press('Escape')
  await expect(page.getByTestId('offline-area-panel')).toHaveCount(0)

  await page.getByTestId('offline-area-button').click()
  const panel = page.getByTestId('offline-area-panel')
  await expect(panel).toBeVisible()
  await panel.getByRole('button', { name: 'Draw area' }).click()
  await expect(panel).toHaveCount(0)

  await expect(page.locator('.explore-selection-hint')).toBeVisible()
  await page.keyboard.press('Escape')
  await expect(page.locator('.explore-selection-hint')).toHaveCount(0)
  await page.getByTestId('offline-area-button').click()
  await page.getByTestId('offline-area-panel').getByRole('button', { name: 'Draw area' }).click()

  const mapBox = await page.getByTestId('explore-map').boundingBox()
  expect(mapBox).not.toBeNull()
  await page.mouse.move(mapBox!.x + mapBox!.width * 0.18, mapBox!.y + mapBox!.height * 0.28)
  await page.mouse.down()
  await page.mouse.move(mapBox!.x + mapBox!.width * 0.54, mapBox!.y + mapBox!.height * 0.55, { steps: 8 })
  await page.mouse.up()

  const selectedPanel = page.getByTestId('offline-area-panel')
  await expect(selectedPanel).toContainText('Area selected')
  await expect(page.getByTestId('offline-area-estimate')).toBeVisible()
  await expect(selectedPanel.getByRole('button', { name: /estimate/i })).toHaveCount(0)

  const estimatedRequest = runtime.getPackRequest()
  const bbox = estimatedRequest?.bbox as number[]
  expect(bbox).toHaveLength(4)
  expect(bbox.every(Number.isFinite)).toBe(true)
  expect(bbox[0]).toBeLessThan(bbox[2])

  await selectedPanel.getByRole('button', { name: 'Download area' }).click()
  expect(runtime.getPackRequest()).toMatchObject({
    name: 'Map: Vitoria-Gasteiz',
    paddingKm: 0,
    layers: ['openfreemap'],
    scopes: ['places'],
  })
  await expect(page.getByTestId('offline-area-progress')).toContainText('Available offline')
  await expect(page.locator('.leaflet-offline-coverage-pane path')).toHaveCount(1)
  await expect(selectedPanel).toContainText('Exact place searches')
  expect(runtime.getPlaceRequests()).toBe(1)

  await page.keyboard.press('Escape')
  await expect(selectedPanel).toHaveCount(0)
  await expect(page.locator('.explore-place-card')).toBeVisible()
  await page.keyboard.press('Escape')
  await expect(page.locator('.explore-place-card')).toHaveCount(0)

  await page.getByRole('button', { name: 'Hide downloaded areas' }).click()
  await expect(page.locator('.leaflet-offline-coverage-pane path')).toHaveCount(0)
  await page.getByTestId('offline-area-button').click()
  await page.getByTestId('offline-area-button').click()
  await expect(page.locator('.leaflet-offline-coverage-pane path')).toHaveCount(1)

  await page.evaluate(() => localStorage.setItem('gpx-explore-view', JSON.stringify({ lat: 0, lon: 0, zoom: 3 })))
  await page.reload()
  await page.getByRole('button', { name: 'Explore map' }).click()
  await expect(page.locator('.leaflet-offline-coverage-pane path')).toHaveCount(1)

  const currentView = () => page.evaluate(() => JSON.parse(localStorage.getItem('gpx-explore-view') ?? '{}') as { lat?: number; lon?: number })
  await expect.poll(async () => (await currentView()).lat).toBeCloseTo(0, 1)

  await page.getByRole('button', { name: /Downloaded areas/ }).click()
  const manager = page.getByRole('region', { name: 'Downloaded areas' })
  await expect(manager).toContainText('Vitoria-Gasteiz')
  await manager.getByRole('button', { name: 'View Vitoria-Gasteiz on map' }).click()
  await expect(manager).toHaveCount(0)
  await expect.poll(async () => (await currentView()).lat).toBeCloseTo((bbox[0] + bbox[2]) / 2, 1)
  await expect.poll(async () => (await currentView()).lon).toBeCloseTo((bbox[1] + bbox[3]) / 2, 1)

  await page.getByRole('button', { name: /Downloaded areas/ }).click()
  await page.keyboard.press('Escape')
  await expect(manager).toHaveCount(0)
  await page.getByRole('button', { name: /Downloaded areas/ }).click()
  const removeArea = manager.getByRole('button', { name: 'Remove Vitoria-Gasteiz' })
  await expect(removeArea.locator('svg')).toBeVisible()
  await expect(removeArea).toHaveText('')
  await removeArea.click()
  await expect(manager.getByRole('button', { name: 'Confirm removal' })).toBeVisible()
  await page.keyboard.press('Escape')
  await expect(manager).toBeVisible()
  await expect(manager.getByRole('button', { name: 'Confirm removal' })).toHaveCount(0)
  await removeArea.click()
  await manager.getByRole('button', { name: 'Confirm removal' }).click()
  await expect(page.locator('.leaflet-offline-coverage-pane path')).toHaveCount(0)
})

test('MCP agents can see errors caused by their own commands', async ({ page }) => {
  await mockRuntime(page)
  // Routing fails after plan_route has already returned, so the only way an
  // agent can learn about it is through the snapshot.
  const commands = [
    { id: 'command-mode', name: 'switch_mode', arguments: { mode: 'planner' } },
    {
      id: 'command-route',
      name: 'plan_route',
      arguments: { points: [{ lat: 42.82, lon: -2.75 }, { lat: 42.9, lon: -2.58 }], fitView: false },
    },
  ]
  const results: Record<string, unknown>[] = []
  let commandIndex = 0
  let latestView: Record<string, unknown> | undefined

  await page.route(/\/mcp\/browser\/session$/, route => json(route, { enabled: true }))
  await page.route(/\/mcp\/browser\/view$/, async route => {
    latestView = route.request().postDataJSON() as Record<string, unknown>
    await route.fulfill({ status: 204 })
  })
  await page.route(/\/mcp\/browser\/events/, route =>
    fulfillCommandStream(route, commands[commandIndex]))
  await page.route(/\/mcp\/browser\/result$/, async route => {
    results.push(route.request().postDataJSON() as Record<string, unknown>)
    commandIndex++
    await route.fulfill({ status: 204 })
  })

  await page.goto('/')
  await expect.poll(() => results.length).toBe(2)
  // The command itself reported success: the failure had not happened yet.
  expect(results[1]).toMatchObject({ id: 'command-route', result: { ok: true, routePointCount: 2 } })

  await expect.poll(() => {
    const state = latestView?.snapshot as {
      planner?: { routeError?: string | null; loading?: boolean }
      notifications?: { type?: string; message?: string }[]
    } | undefined
    return {
      routeFailed: typeof state?.planner?.routeError === 'string' && state.planner.routeError.length > 0,
      reportedError: (state?.notifications ?? []).some(entry => entry.type === 'error'),
    }
  }, { timeout: 25000 }).toEqual({ routeFailed: true, reportedError: true })
})
