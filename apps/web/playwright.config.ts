import { defineConfig } from '@playwright/test';

// Tests run against the production build (vite build + server.mjs), not the dev server.
// Set BASE_URL to test a server that is already running (CI's compose web, or scripts/e2e-auth.sh).
const port = Number(process.env.WEB_PORT ?? 3000);
const baseURL = process.env.BASE_URL ?? `http://127.0.0.1:${port}`;

export default defineConfig({
  testDir: './tests',
  use: { browserName: 'chromium', baseURL },
  webServer: process.env.BASE_URL
    ? undefined
    : {
        command: 'npm run build && npm start',
        url: baseURL,
        reuseExistingServer: true,
        env: { PORT: String(port), HOST: '127.0.0.1', API_ORIGIN: process.env.API_ORIGIN ?? 'http://127.0.0.1:8080' },
        timeout: 180_000,
      },
});
