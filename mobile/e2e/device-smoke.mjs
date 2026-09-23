// Attach to the app's debug WebView after forwarding its devtools socket.
// This intentionally downloads Monaco into the test app when --prepare is set.
import { chromium, expect } from '@playwright/test'

const browser = await chromium.connectOverCDP(process.env.CDP_URL || 'http://127.0.0.1:9222')
let page, original
try {
  page = browser
    .contexts()[0]
    .pages()
    .find((p) => p.url().startsWith('http://127.0.0.1:'))
  if (!page) throw new Error('Overland WebView not found')
  page.on('pageerror', (error) => console.error('WebView error:', error.message))
  await expect(page.getByRole('navigation', { name: 'Main navigation' })).toBeVisible()
  console.log('WebView and mobile UI ready')
  original = await page.evaluate(async () => {
    const routing = await (await fetch('/offline/routing')).json()
    const config = await (await fetch('/config')).json()
    return { region: routing.ready ? routing.regionId : null, mode: config.offline.mode }
  })
  if (process.argv.includes('--prepare')) {
    console.log(
      await page.evaluate(async () => {
        const response = await fetch('/offline/routing/prepare', {
          method: 'POST',
          headers: { 'Content-Type': 'application/json', 'X-GPX-Editor': '1' },
          body: JSON.stringify({ regionId: 'monaco' }),
        })
        if (!response.ok) throw new Error(await response.text())
        return { preparation: response.status }
      }),
    )
    let previous = ''
    for (let i = 0; i < 180; i++) {
      const status = await page.evaluate(async () => (await fetch('/offline/routing')).json())
      const progress = `${status.job?.state}/${status.job?.phase}/${status.job?.item}`
      if (progress !== previous) console.log(progress)
      previous = progress
      if (status.job?.state === 'failed') throw new Error(status.error || status.job.detail)
      if (status.ready && status.regionId === 'monaco') break
      await new Promise((resolve) => setTimeout(resolve, 1000))
    }
  }
  console.log(
    await page.evaluate(async (originalMode) => {
      const status = await (await fetch('/offline/routing')).json()
      if (!status.ready || status.regionId !== 'monaco')
        throw new Error('Monaco is not ready; run with --prepare')
      const mode = await fetch('/offline/mode', {
        method: 'PUT',
        headers: { 'Content-Type': 'application/json', 'X-GPX-Editor': '1' },
        body: JSON.stringify({ mode: 'cache-only' }),
      })
      if (!mode.ok) throw new Error(await mode.text())
      const results = []
      try {
        for (const profile of ['road', 'mixed', 'trail', 'enduro']) {
          const start = performance.now()
          const response = await fetch('/routing/broom/route', {
            method: 'POST',
            headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify({
              profile,
              waypoints: [
                { lon: 7.4197, lat: 43.7384 },
                { lon: 7.4276, lat: 43.7392 },
              ],
            }),
          })
          const route = await response.json()
          if (!response.ok) throw new Error(JSON.stringify(route))
          results.push({
            profile,
            ms: Math.round(performance.now() - start),
            distanceMeters: Math.round(route.distanceMeters),
            coordinates: route.coordinates.length,
          })
        }
      } finally {
        await fetch('/offline/mode', {
          method: 'PUT',
          headers: { 'Content-Type': 'application/json', 'X-GPX-Editor': '1' },
          body: JSON.stringify({ mode: originalMode }),
        })
      }
      return { offlineRoutes: results }
    }, original.mode),
  )
} finally {
  if (page && original?.region && original.region !== 'monaco') {
    await page.evaluate(async (regionId) => {
      const status = await (await fetch('/offline/routing')).json()
      if (status.regionId === 'monaco') {
        const response = await fetch('/offline/routing/prepare', {
          method: 'POST',
          headers: { 'Content-Type': 'application/json', 'X-GPX-Editor': '1' },
          body: JSON.stringify({ regionId }),
        })
        if (!response.ok) throw new Error('Could not restore the original routing region')
      }
    }, original.region)
    await expect
      .poll(
        async () =>
          page.evaluate(async () => (await (await fetch('/offline/routing')).json()).regionId),
        { timeout: 60000 },
      )
      .toBe(original.region)
  }
  await browser.close()
}
