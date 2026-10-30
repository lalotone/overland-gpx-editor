// device-smoke.mjs over raw DevTools, for WebViews Playwright cannot attach to
// (connectOverCDP needs Browser.setDownloadBehavior, which older Android
// System WebViews such as emulator images reject). Downloads Monaco, then
// routes all four riding profiles in strict cache-only mode and restores the
// original offline mode.
import { attach } from './cdp.mjs'

const app = await attach(Number(process.env.CDP_PORT || 9222))
const call = (path, body, method = 'POST') => app.evaluate(`fetch(${JSON.stringify(path)}, {
  method: ${JSON.stringify(method)},
  headers: { 'Content-Type': 'application/json', 'X-GPX-Editor': '1' },
  body: ${JSON.stringify(JSON.stringify(body))},
}).then(async r => ({ status: r.status, text: await r.text() }))`)
const status = () => app.evaluate(`fetch('/offline/routing').then(r => r.json())`)
try {
  let s = await status()
  if (process.argv.includes('--prepare') && !(s.ready && s.regionId === 'monaco')) {
    const prepared = await call('/offline/routing/prepare', { regionId: 'monaco' })
    if (prepared.status >= 300) throw new Error(prepared.text)
    let previous = ''
    for (let i = 0; i < 300; i++) {
      s = await status()
      const progress = `${s.job?.state}/${s.job?.phase ?? ''}/${s.job?.item ?? ''}`
      if (progress !== previous) console.log(progress)
      previous = progress
      if (s.job?.state === 'failed') throw new Error(s.error || s.job.detail)
      if (s.ready && s.regionId === 'monaco') break
      await app.wait(1000)
    }
  }
  if (!(s.ready && s.regionId === 'monaco')) throw new Error('Monaco is not ready; run with --prepare')
  const original = await app.evaluate(`fetch('/config').then(r => r.json()).then(c => c.offline.mode)`)
  const offline = await call('/offline/mode', { mode: 'cache-only' }, 'PUT')
  if (offline.status !== 200) throw new Error(offline.text)
  try {
    for (const profile of ['road', 'mixed', 'trail', 'enduro']) {
      const started = Date.now()
      const route = await call('/routing/broom/route', {
        profile, waypoints: [{ lat: 43.7311, lon: 7.4197 }, { lat: 43.7484, lon: 7.4371 }],
      })
      if (route.status !== 200) throw new Error(`${profile}: HTTP ${route.status} ${route.text}`)
      const body = JSON.parse(route.text)
      console.log(`${profile}: ${(body.distanceMeters / 1000).toFixed(2)} km, ${body.coordinates.length} points (${Date.now() - started} ms)`)
    }
  } finally {
    await call('/offline/mode', { mode: original }, 'PUT')
  }
  if (app.errors.length) throw new Error('Page errors: ' + app.errors.join('; '))
  console.log('Device smoke passed')
} finally {
  app.close()
}
