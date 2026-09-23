import { defineConfig, devices } from '@playwright/test'

export default defineConfig({
  testDir: './e2e',
  testMatch: '**/*.spec.ts',
  workers: 2,
  outputDir: '../test-results/mobile',
  use: {
    baseURL: 'http://127.0.0.1:4174',
    trace: 'retain-on-failure',
    launchOptions: { args: ['--enable-unsafe-swiftshader', '--use-angle=swiftshader'] },
  },
  projects: [
    { name: 'phone', use: { ...devices['Pixel 7'] } },
    { name: 'small-phone', use: { ...devices['iPhone SE'], defaultBrowserType: 'chromium' } },
  ],
  webServer: {
    command: 'npm run dev:mobile -- --port 4174',
    cwd: '..',
    url: 'http://127.0.0.1:4174',
    reuseExistingServer: !process.env.CI,
  },
})
