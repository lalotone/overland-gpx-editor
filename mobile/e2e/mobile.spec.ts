import { expect, test } from '@playwright/test'
import type { Page } from '@playwright/test'

const fixture = `<?xml version="1.0"?><gpx version="1.1"><wpt lat="42.01" lon="1.01"><name>Water</name><sym>Drinking Water</sym></wpt><trk><name>Created Track</name><trkseg>${Array.from({ length: 10 }, (_, i) => `<trkpt lat="${42 + i / 100}" lon="${1 + i / 100}"><ele>${400 + i * 10}</ele><time>2026-09-23T08:00:0${i}Z</time></trkpt>`).join('')}</trkseg></trk></gpx>`

async function setup(page: Page) {
  let draft: unknown = null
  const writes: { url: string; body: string | null }[] = []
  const routes: { profile: string; accessPermit: boolean }[] = []
  await page.route('**/*', async (route) => {
    const url = new URL(route.request().url())
    const json = (body: unknown, status = 200) =>
      route.fulfill({ status, contentType: 'application/json', body: JSON.stringify(body) })
    if (url.pathname === '/config')
      return json({
        offline: {
          enabled: true,
          mode: 'auto',
          routing: '/offline/routing',
          status: '/offline/status',
          packs: '/offline/packs',
          modeControl: '/offline/mode',
        },
        services: { broomRoute: '/routing/broom/route', places: '/places/search' },
        maps: { openfreemap: { style: '/test/style.json' } },
      })
    if (url.pathname === '/test/style.json')
      return json({
        version: 8,
        sources: {},
        layers: [
          { id: 'background', type: 'background', paint: { 'background-color': '#dce4d4' } },
        ],
      })
    if (url.pathname === '/mobile/capabilities') return json({ native: false })
    if (url.pathname === '/mobile/draft') {
      if (route.request().method() === 'PUT') {
        draft = route.request().postDataJSON()
        return json({ ok: true })
      }
      return json(draft)
    }
    if (url.pathname === '/files') return json({ files: ['original-filename.gpx'] })
    if (url.pathname.startsWith('/gpx/') && route.request().method() === 'GET')
      return route.fulfill({ contentType: 'application/gpx+xml', body: fixture })
    if (url.pathname.startsWith('/gpx/') || url.pathname === '/upload') {
      writes.push({ url: url.pathname, body: route.request().postData() })
      return json({ ok: true })
    }
    if (url.pathname === '/offline/routing')
      return json({
        enabled: true,
        ready: true,
        regionId: 'test',
        name: 'Test region',
        generationId: '1',
        cached: [],
        cacheBytes: 0,
      })
    if (url.pathname === '/offline/packs') return json([])
    if (url.pathname === '/offline/mode') return json({ mode: route.request().postDataJSON().mode })
    if (url.pathname === '/routing/broom/route') {
      routes.push(route.request().postDataJSON())
      return json({
        schemaVersion: 1,
        coordinates: [
          { lat: 42, lon: 1 },
          { lat: 42.1, lon: 1.1 },
        ],
        elevations: [
          { meters: 300, interpolated: false },
          { meters: 350, interpolated: true },
        ],
        segments: [],
        distanceMeters: 13000,
        durationSeconds: 900,
      })
    }
    if (url.host !== '127.0.0.1:4174') return route.abort()
    return route.continue()
  })
  return { writes, routes, draft: () => draft }
}

test('library deletes only the confirmed filename and retains tracks after a failure', async ({ page }) => {
  await setup(page)
  const filename = 'Ruta #1 & río.gpx'
  let files = [filename, 'another-track.gpx']
  let fail = true
  const deleted: string[] = []
  await page.route('**/files', route => route.fulfill({ json: { files } }))
  await page.route('**/gpx/**', async route => {
    expect(route.request().method()).toBe('DELETE')
    const path = new URL(route.request().url()).pathname
    deleted.push(decodeURIComponent(path.slice('/gpx/'.length)))
    if (fail) return route.fulfill({ status: 500, json: { error: 'Could not delete track' } })
    files = files.filter(file => file !== deleted.at(-1))
    return route.fulfill({ json: { message: 'File deleted successfully' } })
  })
  await page.goto('/')
  await page.getByRole('button', { name: 'Library', exact: true }).click()
  const remove = page.getByRole('button', { name: `Delete ${filename}`, exact: true })
  await remove.click()
  const dialog = page.getByRole('dialog', { name: 'Delete track?', exact: true })
  await expect(dialog).toContainText(filename)
  await dialog.getByRole('button', { name: 'Cancel', exact: true }).click()
  expect(deleted).toEqual([])
  await expect(remove).toBeVisible()
  await remove.click()
  await dialog.getByRole('button', { name: 'Delete track', exact: true }).click()
  await expect(page.locator('.toast')).toContainText('Could not delete track')
  await expect(remove).toBeVisible()
  await expect(dialog).toBeVisible()
  fail = false
  await dialog.getByRole('button', { name: 'Delete track', exact: true }).click()
  await expect(dialog).toHaveCount(0)
  await expect(remove).toHaveCount(0)
  expect(deleted).toEqual([filename, filename])
  await expect(page.getByRole('button', { name: 'Delete another-track.gpx', exact: true })).toBeVisible()
  await page.reload()
  await page.getByRole('button', { name: 'Library', exact: true }).click()
  await expect(remove).toHaveCount(0)
  await page.getByRole('button', { name: 'Delete another-track.gpx', exact: true }).click()
  await dialog.getByRole('button', { name: 'Delete track', exact: true }).click()
  await expect(page.getByRole('heading', { name: 'A little inspiration?' })).toBeVisible()
})

test('Explore is a full map and one offline button downloads the complete area', async ({
  page,
}) => {
  await setup(page)
  const preparations: unknown[] = []
  const packRequests: Record<string, unknown>[] = []
  await page.route('**/offline/routing/suggest', (route) =>
    route.fulfill({
      json: { region: { regionId: 'test', name: 'Test region', coversView: true } },
    }),
  )
  await page.route('**/offline/routing/prepare', (route) => {
    preparations.push(route.request().postDataJSON())
    return route.fulfill({ json: { enabled: true, ready: true, regionId: 'test', cached: [] } })
  })
  await page.route('**/offline/packs/estimate', (route) =>
    route.fulfill({ json: { resources: 100, bytes: 4096, blocked: [] } }),
  )
  await page.route('**/offline/packs', (route) => {
    if (route.request().method() === 'POST') {
      packRequests.push(route.request().postDataJSON())
      return route.fulfill({ json: { id: 'area-1', name: 'Map: Test region', state: 'queued' } })
    }
    return route.fulfill({
      json: packRequests.length
        ? [{ id: 'area-1', name: 'Map: Test region', state: 'running', done: 20, total: 100 }]
        : [],
    })
  })
  await page.goto('/')
  await expect(page.getByRole('textbox', { name: 'Search places' })).toBeVisible()
  await expect(page.getByRole('button', { name: 'Fuel', exact: true })).toBeVisible()
  await expect(page.locator('.bottom-sheet')).toHaveCount(0)
  await expect(page.getByText('Find your own way')).toHaveCount(0)
  await page.getByRole('button', { name: 'Offline', exact: true }).click()
  await expect(page.locator('.bottom-sheet')).toHaveCount(0)
  await page.getByRole('button', { name: /^Download this area/ }).click()
  await expect.poll(() => packRequests.length).toBe(1)
  expect(preparations).toEqual([{ regionId: 'test', update: false }])
  expect(packRequests[0]).toMatchObject({
    regional: true,
    coverageKind: 'area',
    name: 'Map: Visible area near Test region',
    layers: ['openfreemap'],
    scopes: ['elevation', 'pois', 'fuel'],
    minZoom: 5,
    maxZoom: 14,
  })
  expect(packRequests[0].bbox).toHaveLength(4)
  await expect(page.getByRole('button', { name: /^Stop downloads/ })).toBeVisible()
})

test('area download refuses incomplete routing coverage before starting jobs', async ({ page }) => {
  await setup(page)
  let started = false
  await page.route('**/offline/routing/suggest', (route) =>
    route.fulfill({ json: { region: { regionId: 'test', name: 'Test', coversView: false } } }),
  )
  await page.route('**/offline/routing/prepare', (route) => {
    started = true
    return route.fulfill({ json: {} })
  })
  await page.goto('/')
  await page.getByRole('button', { name: 'Offline', exact: true }).click()
  await page.getByRole('button', { name: /^Download this area/ }).click()
  await expect(page.getByRole('status')).toContainText('smaller area')
  expect(started).toBe(false)
})

test('region browser clearly lists downloads, browses countries and shows every resource', async ({
  page,
}) => {
  await setup(page)
  const bounds = { south: 40, west: -2, north: 43, east: 1 }
  let pack: Record<string, unknown> | null = null
  await page.route('**/offline/routing', (route) =>
    route.fulfill({
      json: {
        enabled: true,
        ready: true,
        regionId: 'aragon',
        generationId: 'one',
        cached: [{ regionId: 'aragon', generationId: 'one', name: 'Aragón', selected: true }],
        cacheBytes: 1024,
      },
    }),
  )
  await page.route('**/offline/routing/regions', (route) =>
    route.fulfill({
      json: {
        regions: [
          {
            id: 'spain',
            name: 'Spain',
            parent: 'europe',
            kind: 'country',
            installed: false,
            bbox: { south: 36, west: -9, north: 44, east: 4 },
          },
          {
            id: 'aragon',
            name: 'Aragón',
            parent: 'spain',
            kind: 'region',
            installed: true,
            active: true,
            bbox: bounds,
          },
        ],
      },
    }),
  )
  await page.route('**/offline/routing/prepare', (route) =>
    route.fulfill({ json: { enabled: true, ready: true, regionId: 'aragon', cached: [] } }),
  )
  await page.route('**/offline/packs/estimate', (route) =>
    route.fulfill({
      json: {
        resources: 22000,
        bytes: 4096,
        blocked: [{ resource: 'water', reason: 'City-sized searches are required for water' }],
      },
    }),
  )
  await page.route('**/offline/packs', (route) => {
    if (route.request().method() === 'POST')
      pack = {
        id: 'region-pack',
        name: 'Map: Aragón',
        coverageKind: 'region',
        state: 'running',
        batchesTotal: 172,
        batchesDone: 1,
        unavailable: [{ resource: 'water', reason: 'City-sized searches are required for water' }],
        bbox: bounds,
        resources: Object.fromEntries(
          ['vector-map', 'elevation', 'fuel-prices', 'fuel-stations', 'water', 'campsites'].map(
            (key) => [key, { done: key === 'elevation' || key === 'fuel-prices' ? 10 : 3, total: 10, failed: 0, bytes: 1024, ...(key === 'vector-map' ? { reused: 3, downloaded: 0 } : {}) }],
          ),
        ),
      }
    return route.fulfill({ json: route.request().method() === 'POST' ? pack : pack ? [pack] : [] })
  })
  await page.goto('/')
  await page.getByRole('button', { name: 'Offline', exact: true }).click()
  await page.getByRole('button', { name: 'Download region', exact: true }).click()
  const screen = page.getByRole('region', { name: 'Download regions', exact: true })
  await expect(screen.getByRole('heading', { name: 'Downloaded', exact: true })).toBeVisible()
  await expect(screen.getByText('In use').first()).toBeVisible()
  await expect(screen.getByText('Routing downloaded · maps not complete').first()).toBeVisible()
  await screen.getByRole('button', { name: 'Country', exact: true }).click()
  await screen.locator('.region-row-main').filter({ hasText: 'Spain' }).click()
  await expect(screen.getByRole('heading', { name: 'Spain', exact: true })).toBeVisible()
  await screen.locator('.region-row-main').filter({ hasText: 'Aragón' }).click()
  await expect(screen.getByRole('heading', { name: 'Aragón', exact: true })).toBeVisible()
  await expect(screen.locator('.map-coverage')).toContainText('Whole region')
  await expect(screen.locator('.map-coverage')).toContainText('40.000, -2.000 → 43.000, 1.000')
  await screen.getByRole('button', { name: 'Download missing resources' }).click()
  await expect(
    screen.locator('.resource-download').filter({ hasText: 'Vector maps' }),
  ).toContainText('3 / 10')
  const vectors = screen.locator('.resource-download').filter({ hasText: 'Vector maps' })
  await expect(vectors).toContainText('3 cached · 0 downloaded')
  const resources = pack!.resources as Record<string, Record<string, number>>
  resources['vector-map'] = { done: 5, total: 10, failed: 0, bytes: 1024, reused: 3, downloaded: 1, revalidated: 1 }
  await expect(vectors).toContainText('3 cached · 1 downloaded · 1 checked online')
  await expect(vectors).toContainText('added to cache')
  await expect(screen.locator('.resource-download').filter({ hasText: 'Water' })).toContainText(
    'Provider limit',
  )
  await expect(screen.getByText('Batch 2 of 172')).toBeVisible()
  for (const label of ['Elevation', 'Fuel prices']) {
    const resource = screen.locator('.resource-download').filter({ hasText: label })
    await expect(resource).toContainText('Downloaded')
    await expect(resource.getByRole('progressbar')).toHaveCount(0)
  }
  for (const label of [
    'Routing',
    'Vector maps',
    'Elevation',
    'Fuel prices',
    'Fuel stations',
    'Water',
    'Campsites',
  ])
    await expect(screen.getByText(label, { exact: true })).toBeVisible()
  await screen.getByRole('button', { name: 'Back to regions' }).click()
  await expect(screen.getByRole('heading', { name: 'Spain', exact: true })).toBeVisible()
})

test('saved map areas distinguish legacy extents from whole-region coverage', async ({ page }) => {
  await setup(page)
  await page.route('**/offline/routing/regions', route => route.fulfill({ json: { regions: [] } }))
  await page.route('**/offline/packs', route => route.fulfill({ json: [
    { id: 'small', name: 'Map: Cataluña', state: 'complete', bbox: { south: 41.4, west: 2.2, north: 41.6, east: 2.4 }, resources: { 'vector-map': { done: 10, total: 10, failed: 0, bytes: 0 } } },
    { id: 'whole', name: 'Map: Cataluña', state: 'complete', coverageKind: 'region', bbox: { south: 40.2, west: 0.1, north: 42.8, east: 4.1 }, resources: { 'vector-map': { done: 100, total: 100, failed: 0, bytes: 0, reused: 100 } } },
  ] }))
  await page.goto('/')
  await page.getByRole('button', { name: 'Offline', exact: true }).click()
  await page.getByRole('button', { name: 'Download region', exact: true }).click()
  const screen = page.getByRole('region', { name: 'Download regions', exact: true })
  const legacy = screen.getByRole('button').filter({ hasText: 'Saved map extent' })
  await expect(legacy).toContainText('41.400, 2.200 → 41.600, 2.400')
  const whole = screen.getByRole('button').filter({ hasText: 'Whole region' })
  await expect(whole).toContainText('40.200, 0.100 → 42.800, 4.100')
  await legacy.click()
  await expect(screen.locator('.map-coverage')).toContainText('Selected map area')
  await expect(screen.locator('.map-coverage')).not.toContainText('Whole region')
  await screen.getByRole('button', { name: 'Back to regions' }).click()
  await whole.click()
  await expect(screen.locator('.map-coverage')).toContainText('Whole region')
  await expect(screen.locator('.resource-download').filter({ hasText: 'Vector maps' })).toContainText('100 cached · 0 downloaded')
})

test('region browser can select a city boundary from place search', async ({ page }) => {
  await setup(page)
  await page.route('**/offline/routing/regions', (route) =>
    route.fulfill({ json: { regions: [] } }),
  )
  await page.route('**/places/search?*', (route) =>
    route.fulfill({
      json: [
        {
          place_id: 123,
          display_name: 'Zaragoza, Aragón, Spain',
          lat: '41.65',
          lon: '-0.88',
          boundingbox: ['41.5', '41.8', '-1', '-0.7'],
        },
      ],
    }),
  )
  await page.goto('/')
  await page.getByRole('button', { name: 'Offline', exact: true }).click()
  await page.getByRole('button', { name: 'Download region', exact: true }).click()
  await page.getByRole('button', { name: 'City', exact: true }).click()
  await page.getByRole('textbox', { name: 'Search city' }).fill('Zaragoza')
  await page.getByRole('button', { name: 'Find city' }).click()
  await page.getByRole('button', { name: 'Zaragoza, Aragón, Spain' }).click()
  await expect(page.getByRole('heading', { name: 'Zaragoza', exact: true })).toBeVisible()
  await expect(page.getByRole('button', { name: 'Download Zaragoza', exact: true })).toBeVisible()
})

test('phone editor preserves filename, supports undo, and restores its draft', async ({ page }) => {
  const state = await setup(page)
  await page.goto('/')
  await page.getByRole('button', { name: 'Library', exact: true }).click()
  await page.getByRole('button', { name: /Original filename/ }).click()
  await page.getByRole('button', { name: 'Edit track', exact: true }).click()
  await page.getByRole('button', { name: 'Reverse', exact: true }).click()
  await expect(page.getByText('UNSAVED CHANGES')).toBeVisible()
  await page.getByRole('button', { name: 'Undo', exact: true }).click()
  await page.getByRole('button', { name: 'Reverse', exact: true }).click()
  await page.getByRole('button', { name: 'Save edits', exact: true }).click()
  await expect.poll(() => state.writes.length).toBe(1)
  expect(state.writes[0].url).toBe('/gpx/original-filename.gpx')
  expect(state.writes[0].body).toContain('<name>Water</name>')
  await expect
    .poll(() => state.draft())
    .toMatchObject({
      tab: 'library',
      document: { libraryFilename: 'original-filename.gpx', dirty: false },
    })
  await page.reload()
  await expect(page.getByRole('heading', { name: 'Original filename' })).toBeVisible()
  expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true)
})

test('planner uses centre controls, riding profiles and explicit permits', async ({ page }) => {
  const state = await setup(page)
  await page.goto('/')
  await page.getByRole('button', { name: 'Plan', exact: true }).click()
  await expect(page.getByRole('region', { name: 'Route points', exact: true })).toHaveCount(0)
  await page.getByRole('button', { name: 'Start route here' }).click()
  await page.getByRole('button', { name: 'Add next point here' }).click()
  await expect.poll(() => state.routes.length).toBeGreaterThan(0)
  expect(state.routes[0]).toMatchObject({ profile: 'mixed', accessPermit: false })
  await expect(page.getByText('Distance', { exact: true })).toHaveCount(0)
  await page.getByRole('button', { name: 'Route details', exact: true }).click()
  await expect(page.getByRole('dialog', { name: 'Route details' })).toBeVisible()
  await expect(page.getByText('Distance', { exact: true })).toBeVisible()
  await page.getByRole('button', { name: 'Close route details' }).click()
  await page.getByRole('button', { name: 'Show route points' }).click()
  await page.getByRole('checkbox', { name: 'Restricted access' }).check()
  await page.getByRole('button', { name: 'Enduro', exact: true }).click()
  await expect
    .poll(() => state.routes[state.routes.length - 1])
    .toMatchObject({ profile: 'enduro', accessPermit: true })
  await page.getByRole('button', { name: 'Save ride', exact: true }).click()
  await page.getByRole('textbox', { name: 'Name your ride' }).fill('Weekend ride')
  await page.getByRole('dialog').getByRole('button', { name: 'Save ride' }).click()
  await expect.poll(() => state.writes.length).toBe(1)
  expect(state.writes[0].url).toBe('/upload')
  expect(state.writes[0].body).not.toContain('<ele>350.00</ele>')
})

test('Plan pins its profiles and actions while points scroll, and supports pulling up', async ({
  page,
}) => {
  await setup(page)
  await page.goto('/')
  await page.getByRole('button', { name: 'Plan', exact: true }).click()
  await page.getByRole('button', { name: 'Start route here' }).click()
  for (let i = 0; i < 11; i++)
    await page.getByRole('button', { name: 'Add next point here' }).click()
  const handle = page.getByRole('button', { name: 'Show route points' })
  const box = await handle.boundingBox()
  await page.mouse.move(box!.x + box!.width / 2, box!.y + box!.height / 2)
  await page.mouse.down()
  await page.mouse.move(box!.x + box!.width / 2, box!.y - 80, { steps: 8 })
  await page.mouse.up()
  const profiles = page.locator('.profile-selector')
  await expect(profiles).toBeVisible()
  const before = await profiles.boundingBox()
  const save = page.getByRole('button', { name: 'Save ride', exact: true })
  const saveBefore = await save.boundingBox()
  await page.locator('.route-controls-list').evaluate((element) => {
    element.scrollTop = element.scrollHeight
  })
  await expect(page.getByRole('button', { name: 'Remove point 12' })).toBeVisible()
  expect((await profiles.boundingBox())!.y).toBeCloseTo(before!.y, 0)
  expect((await save.boundingBox())!.y).toBeCloseTo(saveBefore!.y, 0)
  await page.getByRole('button', { name: 'Hide route points' }).click()
  await expect(page.getByRole('region', { name: 'Route points', exact: true })).toHaveCount(0)
  await expect(handle).toBeVisible()
})

test('local import remains create-only and offline mode is accessible', async ({ page }) => {
  const state = await setup(page)
  await page.goto('/')
  await page.getByLabel('Open GPX file').setInputFiles({
    name: 'local.gpx',
    mimeType: 'application/gpx+xml',
    buffer: Buffer.from(fixture),
  })
  await page.getByRole('button', { name: 'Save to library' }).click()
  await page.getByRole('textbox', { name: 'Save track' }).fill('Local adventure')
  await page.getByRole('dialog').getByRole('button', { name: 'Save track' }).click()
  await expect.poll(() => state.writes.length).toBe(1)
  expect(state.writes[0].url).toBe('/upload')
  await page.getByRole('button', { name: 'Offline', exact: true }).click()
  await page.getByRole('button', { name: 'Online', exact: true }).click()
  await expect(page.locator('.connection-pill')).toHaveText('Offline')
})
