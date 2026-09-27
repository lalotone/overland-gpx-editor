// Adapted from the go-passkey-auth skill's browser test. Chrome's virtual
// WebAuthn authenticator stands in for a real passkey; enrollment links come
// from the real `overland user` CLI, so the server needs no test endpoints.
import { expect, test } from '@playwright/test'
import type { CDPSession, Page } from '@playwright/test'
import { execFileSync } from 'node:child_process'
import { readdirSync } from 'node:fs'
import { join } from 'node:path'

const origin = process.env.APP_ORIGIN!
const appName = process.env.APP_NAME!
const bin = process.env.APP_BIN!
const db = process.env.APP_DB!

function enrollmentLink(name: string): string {
  const out = execFileSync(bin, ['user', 'add', name, '--db', db, '--origin', origin]).toString()
  return out.match(/https?:\/\/\S+\/auth\/enroll#token=\S+/)![0]
}

async function virtualAuthenticator(page: Page): Promise<{ cdp: CDPSession; authenticatorId: string }> {
  const cdp = await page.context().newCDPSession(page)
  await cdp.send('WebAuthn.enable')
  const { authenticatorId } = await cdp.send('WebAuthn.addVirtualAuthenticator', {
    options: {
      protocol: 'ctap2',
      transport: 'internal',
      hasResidentKey: true,
      hasUserVerification: true,
      isUserVerified: true,
      automaticPresenceSimulation: true,
    },
  })
  return { cdp, authenticatorId }
}

async function expectSignedOut(page: Page) {
  await expect(page).toHaveTitle('404 Not Found')
  await expect(page.getByRole('button', { name: 'Sign in' })).toBeVisible()
  await expect(page.locator('body')).not.toContainText(appName)
  await expect(page.locator('body')).not.toContainText('GPX Editor')
}

async function enroll(page: Page, name: string) {
  await page.goto(enrollmentLink(name))
  await expect(page.getByRole('heading', { name: 'Create your passkey' })).toBeVisible()
  await page.getByRole('button', { name: 'Create passkey' }).click()
  await expect(page.getByRole('heading', { name: "You're all set" })).toBeVisible()
}

const unique = (prefix: string) => `${prefix}${Date.now().toString(36)}`

test('enroll, sign out, sign back in; links are single use', async ({ page }) => {
  const { cdp, authenticatorId } = await virtualAuthenticator(page)
  const response = await page.goto(`${origin}/`)
  expect(response?.status()).toBe(404)
  await expectSignedOut(page)

  const name = unique('alice')
  const link = enrollmentLink(name)
  await page.goto(link)
  await expect(page.locator('#enroll-app')).toHaveText(appName)
  await page.getByText('Label it in your passkey manager').click()
  await page.getByLabel('Passkey label').fill('alice@example.org')
  await page.getByRole('button', { name: 'Create passkey' }).click()
  await expect(page.locator('#done-lead')).toHaveText(`Signed in as ${name}.`)
  await page.getByRole('link', { name: `Open ${appName}` }).click()
  const signOut = page.getByRole('button', { name: 'Sign out' })
  await expect(signOut).toBeVisible()
  await expect(signOut).toHaveAttribute('title', `Signed in as ${name}`)

  // The label lives on the authenticator only.
  const { credentials } = await cdp.send('WebAuthn.getCredentials', { authenticatorId })
  expect(credentials).toHaveLength(1)
  if ('userName' in credentials[0]) expect(credentials[0].userName).toBe('alice@example.org')

  await signOut.click()
  await expectSignedOut(page)
  await page.getByRole('button', { name: 'Sign in' }).click()
  // Sign-in reloads into the app itself.
  await expect(signOut).toBeVisible()

  // A used link reveals nothing. Same-URL hash changes don't reload, so
  // navigate away first.
  await page.goto('about:blank')
  await page.goto(link)
  await expect(page.getByRole('heading', { name: "This link doesn't work" })).toBeVisible()
  await expect(page.locator('body')).not.toContainText(appName)
})

test('the library API needs a session', async ({ page }) => {
  await virtualAuthenticator(page)
  await page.goto(`${origin}/`)
  await expectSignedOut(page)
  expect(await page.evaluate(() => fetch('/files').then(r => r.status))).toBe(401)

  await enroll(page, unique('dave'))
  await page.goto(`${origin}/`)
  await expect(page.getByRole('button', { name: 'Sign out' })).toBeVisible()
  expect(await page.evaluate(() => fetch('/files').then(r => r.status))).toBe(200)
})

test('an expired session returns to the sign-in page', async ({ page }) => {
  await virtualAuthenticator(page)
  await enroll(page, unique('bob'))
  await page.goto(`${origin}/`)
  await expect(page.getByRole('button', { name: 'Sign out' })).toBeVisible()
  await page.context().clearCookies()
  await page.evaluate(() => fetch('/files'))
  await expectSignedOut(page)
})

test("a deleted account's passkey is refused", async ({ page }) => {
  await virtualAuthenticator(page)
  const name = unique('carol')
  await enroll(page, name)
  await page.goto(`${origin}/`)
  await page.getByRole('button', { name: 'Sign out' }).click()
  execFileSync(bin, ['user', 'delete', name, '--yes', '--db', db])
  await page.getByRole('button', { name: 'Sign in' }).click()
  await expect(page.locator('#auth-status')).toHaveText('passkey sign-in failed')
})

// Each account has its own library and its own say over server-wide state.
test('accounts see only their own tracks, and only operators manage the server', async ({ page }) => {
  await virtualAuthenticator(page)
  const alice = unique('alice')
  await enroll(page, alice)
  await page.goto(`${origin}/`)
  await expect(page.getByRole('button', { name: 'Sign out' })).toBeVisible()
  const gpx = '<gpx version="1.1" creator="e2e"><trk><name>Private</name></trk></gpx>'
  expect(await page.evaluate(body => fetch('/gpx/private.gpx', { method: 'PUT', body, headers: { 'Content-Type': 'application/xml' } }).then(r => r.status), gpx)).toBe(200)
  expect(await page.evaluate(() => fetch('/files').then(r => r.json()))).toEqual({ files: ['private.gpx'] })
  // A signed-in user is not an operator unless named with --auth-operator.
  const config = await page.evaluate(() => fetch('/config').then(r => r.json())) as { offline: { operator: boolean; modeControl?: string } }
  expect(config.offline.operator).toBe(false)
  expect(config.offline.modeControl).toBeUndefined()
  expect(await page.evaluate(() => fetch('/offline/mode', { method: 'PUT', body: '{"mode":"cache-only"}', headers: { 'Content-Type': 'application/json', 'X-GPX-Editor': '1' } }).then(r => r.status))).toBe(403)
  await page.getByRole('button', { name: 'Sign out' }).click()
  await expectSignedOut(page)

  await enroll(page, unique('bob'))
  await page.goto(`${origin}/`)
  await expect(page.getByRole('button', { name: 'Sign out' })).toBeVisible()
  expect(await page.evaluate(() => fetch('/files').then(r => r.json()))).toEqual({ files: [] })
  expect(await page.evaluate(() => fetch('/gpx/private.gpx').then(r => r.status))).toBe(404)
  expect(await page.evaluate(() => fetch('/gpx/private.gpx', { method: 'DELETE' }).then(r => r.status))).toBe(404)
  // Same name, different library: no conflict with alice's file.
  expect(await page.evaluate(body => fetch('/gpx/private.gpx', { method: 'PUT', body, headers: { 'Content-Type': 'application/xml' } }).then(r => r.status), gpx)).toBe(200)
  await page.getByRole('button', { name: 'Sign out' }).click()
  await expectSignedOut(page)

  // The operator named on the command line may change the offline mode.
  await enroll(page, 'operator')
  await page.goto(`${origin}/`)
  await expect(page.getByRole('button', { name: 'Sign out' })).toBeVisible()
  const operatorConfig = await page.evaluate(() => fetch('/config').then(r => r.json())) as { offline: { operator: boolean; modeControl?: string } }
  expect(operatorConfig.offline.operator).toBe(true)
  expect(operatorConfig.offline.modeControl).toBe('/offline/mode')
  expect(await page.evaluate(() => fetch('/files').then(r => r.json()))).toEqual({ files: [] })

  // Deleting an account removes its directory and nobody else's. Other
  // tests' accounts share this server, so count rather than enumerate.
  const before = readdirSync(join(process.env.APP_DATA!, 'owners'))
  execFileSync(bin, ['user', 'delete', alice, '--yes', '--db', db, '--data-dir', process.env.APP_DATA!])
  const after = readdirSync(join(process.env.APP_DATA!, 'owners'))
  expect(after.length).toBe(before.length - 1)
  for (const owner of after) expect(owner).toMatch(/^[a-z2-7]{52}$/)
  expect(before.filter(owner => !after.includes(owner)).length).toBe(1)
})
