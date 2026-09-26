import { defineConfig, devices } from '@playwright/test'
import { mkdtempSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'

// Passkey sign-in runs in the Go server, not the Vite dev server, so this
// suite builds and serves the real binary with --auth. Workers inherit these
// variables from the main process, so every test shares one account database.
const port = 18093
process.env.APP_ORIGIN ??= `http://localhost:${port}`
process.env.APP_NAME ??= 'Overland'
const work = (process.env.APP_WORK ??= mkdtempSync(join(tmpdir(), 'overland-auth-e2e-')))
process.env.APP_BIN ??= join(work, 'overland')
process.env.APP_DB ??= join(work, 'accounts.db')

export default defineConfig({
  testDir: './e2e/auth',
  workers: 1,
  forbidOnly: Boolean(process.env.CI),
  reporter: 'line',
  expect: { timeout: 10_000 },
  use: { reducedMotion: 'reduce', trace: 'retain-on-failure' },
  projects: [{ name: 'desktop-chromium', use: { ...devices['Desktop Chrome'] } }],
  webServer: {
    command: [
      'npm run build',
      `go build -o ${process.env.APP_BIN} ./cmd/overland`,
      `${process.env.APP_BIN} serve --auth --addr 127.0.0.1:${port} --auth-db ${process.env.APP_DB}` +
        ` --gpx-dir ${join(work, 'gpx')} --routing-cache-dir '' --offline-cache-dir '' --elevation-tile-cache ''` +
        ' --stats-log-interval 0',
    ].join(' && '),
    url: `http://127.0.0.1:${port}/healthz`,
    reuseExistingServer: false,
    timeout: 300_000,
  },
})
