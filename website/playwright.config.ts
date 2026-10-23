import { defineConfig, devices } from '@playwright/test';

const BASE_PATH = process.env.BASE_PATH ?? '/TeleMeetBot-TeleMeetBot-implementation';
const PORT = Number(process.env.PREVIEW_PORT ?? 4321);
const origin = `http://127.0.0.1:${PORT}`;

export default defineConfig({
  testDir: './tests/e2e',
  timeout: 30_000,
  expect: { timeout: 7_000 },
  fullyParallel: true,
  forbidOnly: !!process.env.CI,
  retries: process.env.CI ? 1 : 0,
  workers: process.env.CI ? 2 : undefined,
  reporter: process.env.CI ? [['list'], ['github']] : [['list']],
  use: {
    baseURL: `${origin}${BASE_PATH}/`,
    permissions: ['clipboard-read', 'clipboard-write'],
    trace: 'retain-on-failure',
    video: 'off',
  },
  projects: [
    {
      name: 'desktop',
      use: { ...devices['Desktop Chrome'], viewport: { width: 1440, height: 900 } },
    },
    {
      name: 'mobile',
      use: { ...devices['Pixel 7'] },
    },
  ],
  webServer: {
    command: `npm run build && npm run preview -- --port ${PORT} --host 127.0.0.1`,
    url: `${origin}${BASE_PATH}/`,
    reuseExistingServer: !process.env.CI,
    timeout: 120_000,
    env: {
      SITE_URL: 'https://opselon.github.io',
      BASE_PATH,
    },
  },
});
