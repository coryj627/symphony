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
    command: '/Users/coryj/.local/share/mise/installs/go/1.26.5/bin/go run -tags=e2e ./cmd/symphony configure ./testdata/manual/WORKFLOW.md --port 43127',
    url: 'http://127.0.0.1:43127/',
    reuseExistingServer: false,
    timeout: 120000,
    env: {
      ...process.env,
      SYMPHONY_E2E_BOOTSTRAP_TOKEN: bootstrapToken,
    },
  },
});
