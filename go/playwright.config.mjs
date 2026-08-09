import {defineConfig} from '@playwright/test';

const bootstrapToken = '0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef';

export default defineConfig({
  testDir: './tests/accessibility',
  globalSetup: './tests/accessibility/fixtures.mjs',
  fullyParallel: false,
  workers: 1,
  reporter: 'line',
  use: {
    baseURL: 'http://127.0.0.1:43127',
    trace: 'off',
  },
  projects: [
    {name: 'chromium', use: {browserName: 'chromium'}},
    {name: 'webkit', use: {browserName: 'webkit'}},
  ],
  webServer: {
    command: 'npm run start:e2e',
    url: 'http://127.0.0.1:43127/',
    reuseExistingServer: false,
    timeout: 120000,
    env: {
      ...process.env,
      TZ: 'America/New_York',
      SYMPHONY_E2E_BOOTSTRAP_TOKEN: bootstrapToken,
      SYMPHONY_E2E_FAIL_DELETE_TRACKER: 'linear',
    },
  },
});
