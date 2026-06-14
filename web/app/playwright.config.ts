import { defineConfig, devices } from "@playwright/test";

// e2e runs the REAL stack: globalSetup stands up a throwaway Postgres + the api
// binary (migrated + seeded); the webServer runs the PWA (vite) proxying /api to it.
const VITE_PORT = 4318;
const API_PORT = 8099;

export default defineConfig({
  testDir: "./e2e",
  fullyParallel: false, // one shared seeded backend
  workers: 1,
  // list for the console; html (never auto-served) so CI can upload a report with
  // the retained traces as a failure artifact.
  reporter: [["list"], ["html", { open: "never" }]],
  timeout: 30_000,
  expect: { timeout: 7_000 },
  globalSetup: "./e2e/global-setup.ts",
  globalTeardown: "./e2e/global-teardown.ts",
  use: {
    baseURL: `http://localhost:${VITE_PORT}`,
    headless: true,
    trace: "retain-on-failure",
  },
  projects: [{ name: "chromium", use: { ...devices["Desktop Chrome"] } }],
  webServer: {
    command: `npm run dev -- --port ${VITE_PORT} --strictPort`,
    url: `http://localhost:${VITE_PORT}`,
    env: { VITE_API_PROXY: `http://localhost:${API_PORT}` },
    reuseExistingServer: false,
    timeout: 60_000,
    stdout: "ignore",
    stderr: "pipe",
  },
});
