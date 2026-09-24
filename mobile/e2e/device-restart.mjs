// Requires a running debug app and its WebView forwarded to localhost:9222.
// Restarts only Overland, then verifies that its draft and authenticated local
// backend are usable again. Does not edit the user's draft or library.
import { execFileSync } from 'node:child_process'
import assert from 'node:assert/strict'
import { chromium, expect } from '@playwright/test'

const adb = (...args) =>
  execFileSync(
    'adb',
    [...(process.env.ANDROID_SERIAL ? ['-s', process.env.ANDROID_SERIAL] : []), ...args],
    { encoding: 'utf8' },
  ).trim()
let browser = await chromium.connectOverCDP('http://127.0.0.1:9222')
let page = browser
  .contexts()[0]
  .pages()
  .find((p) => p.url().startsWith('http://127.0.0.1:'))
const before = await page.evaluate(async () => (await fetch('/mobile/draft')).json())
await browser.close()
adb('shell', 'am', 'force-stop', 'co.overland.mobile')
adb('shell', 'am', 'start', '-W', '-n', 'co.overland.mobile/com.wails.app.MainActivity')
const pid = adb('shell', 'pidof', 'co.overland.mobile')
adb('forward', 'tcp:9222', `localabstract:webview_devtools_remote_${pid}`)
for (let attempt = 0; attempt < 30; attempt++) {
  try {
    browser = await chromium.connectOverCDP('http://127.0.0.1:9222')
    break
  } catch (error) {
    if (attempt === 29) throw error
    await new Promise((resolve) => setTimeout(resolve, 500))
  }
}
try {
  page = browser.contexts()[0].pages()[0]
  await expect(page.getByRole('navigation', { name: 'Main navigation' })).toBeVisible({
    timeout: 60000,
  })
  if (before)
    await expect(page.locator('.bottom-nav button.active')).toHaveText(
      before.tab[0].toUpperCase() + before.tab.slice(1),
    )
  const after = await page.evaluate(async () => (await fetch('/mobile/draft')).json())
  assert.deepEqual(after?.plan, before?.plan)
  assert.deepEqual(after?.document, before?.document)
  console.log('PASS: draft, navigation and authenticated backend recovered after process restart')
} finally {
  await browser.close()
}
