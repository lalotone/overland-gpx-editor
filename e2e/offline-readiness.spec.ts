import { expect, test } from '@playwright/test'
import type { Page, Route } from '@playwright/test'

const STYLE = {
  version: 8,
  sources: {
    transient: {
      type: 'raster',
      tiles: ['/e2e/transient/{z}/{x}/{y}.png'],
      tileSize: 256,
    },
  },
  layers: [
    { id: 'background', type: 'background', paint: { 'background-color': '#dce8df' } },
    { id: 'transient', type: 'raster', source: 'transient' },
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

const GPX = `<?xml version="1.0" encoding="UTF-8"?>
<gpx version="1.1" creator="browser-test" xmlns="http://www.topografix.com/GPX/1/1">
  <trk><name>Browser route</name><trkseg>
    <trkpt lat="42.8500" lon="-2.6800"><ele>540</ele></trkpt>
    <trkpt lat="42.9000" lon="-2.5900"><ele>680</ele></trkpt>
    <trkpt lat="42.9600" lon="-2.5100"><ele>620</ele></trkpt>
  </trkseg></trk>
</gpx>`

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

async function mockRuntime(page: Page, options: { style?: object; elevationUnavailable?: boolean } = {}) {
  let started = false
  let packRequest: Record<string, unknown> | undefined
  const activeResources = options.elevationUnavailable
    ? Object.fromEntries(Object.entries(resourceProgress).filter(([key]) => key !== 'elevation'))
    : resourceProgress
  const total = Object.values(activeResources).reduce((sum, progress) => sum + progress.total, 0)
  const pack = (state: 'queued' | 'complete') => ({
    id: '0123456789abcdef0123456789abcdef',
    name: 'Route: Browser trip',
    state,
    done: state === 'complete' ? total : 0,
    total,
    failed: 0,
    bytes: state === 'complete' ? 24000 : 0,
    resources: state === 'complete'
      ? activeResources
      : Object.fromEntries(Object.entries(activeResources).map(([key, value]) => [key, { ...value, done: 0, bytes: 0, items: 0 }])),
    createdAt: '2026-08-31T12:00:00Z',
    updatedAt: '2026-08-31T12:00:01Z',
  })

  await page.addInitScript(() => {
    localStorage.setItem('gpx-hillshade', 'off')
    localStorage.setItem('gpx-base-layer', 'openfreemap')
  })
  await page.route(/\/config$/, route => json(route, {
    offline: { enabled: true, mode: 'auto', status: '/offline/status', packs: '/offline/packs' },
    services: {},
    maps: { raster: { osm: '/e2e/osm/{z}/{x}/{y}.png' }, openfreemap: { style: '/map/openfreemap/style.json', allowBulk: true } },
  }))
  await page.route(/\/files$/, route => json(route, { files: [] }))
  await page.route(/\/upload$/, route => json(route, { filename: 'browser-trip.gpx' }, 201))
  await page.route(/\/elevation\/prefetch$/, route => json(route, { done: 0, total: 0, state: 'complete' }))
  await page.route(/\/map\/openfreemap\/style\.json$/, route => json(route, options.style ?? STYLE))
  await page.route(/\/e2e\/transient\//, route => route.fulfill({ status: 503, body: 'temporary tile failure' }))
  await page.route(/\/e2e\/source\.json$/, route => route.fulfill({ status: 503, body: 'source unavailable' }))
  await page.route(/\/e2e\/osm\//, route => route.fulfill({
    status: 200,
    contentType: 'image/png',
    body: Buffer.from('iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+A8AAQUBAScY42YAAAAASUVORK5CYII=', 'base64'),
  }))
  await page.route(/\/offline\/packs\/estimate$/, async route => {
    packRequest = route.request().postDataJSON() as Record<string, unknown>
    if (options.elevationUnavailable && (packRequest.scopes as string[]).includes('elevation')) {
      await json(route, { detail: 'elevation packs require Terrarium tile mode with a persistent tile cache' }, 400)
      return
    }
    await json(route, {
      resources: total,
      estimatedBytes: 24000,
      remainingQuota: 1000000,
      counts: {
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
  await page.route(/\/offline\/packs$/, async route => {
    if (route.request().method() === 'POST') {
      started = true
      await json(route, pack('queued'), 202)
      return
    }
    await json(route, started ? [pack('complete')] : [])
  })
  await page.route(/\/offline\/packs\/[a-f0-9]{32}$/, route => route.fulfill({ status: 204 }))

  return {
    getPackRequest: () => packRequest,
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

test('creation mode renders the selected OFM vector layer', async ({ page }) => {
  await mockRuntime(page)
  await page.goto('/')
  await page.getByRole('button', { name: 'Plan a route' }).click()
  await expectVectorMap(page)
  await expect(page.locator('.terrain-fab-label')).toHaveText('OFM')
})

test('a critical OFM source failure reports and renders the raster fallback', async ({ page }) => {
  await mockRuntime(page, { style: BROKEN_SOURCE_STYLE })
  await page.goto('/')
  await page.getByRole('button', { name: 'Plan a route' }).click()
  await expect(page.locator('.terrain-fab-label')).toHaveText('OSM')
  await expect(page.getByText(/vector map style is unavailable/i)).toBeVisible()
  await expect(page.locator('.maplibregl-canvas')).toHaveCount(0)
  await expect(page.locator('.leaflet-tile-pane img[src*="/e2e/osm/"]').first()).toBeVisible()
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
})
