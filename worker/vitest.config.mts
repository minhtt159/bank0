import { defineConfig } from "vitest/config";
import { cloudflareTest } from "@cloudflare/vitest-pool-workers";

// Two projects because the two halves of the proxy contract need different
// harnesses (vitest-pool-workers >= 0.16 can no longer see the in-worker
// ASSETS binding):
//   - proxy: the real worker module in workerd; upstream stubbed by mocking
//     globalThis.fetch (fetchMock was removed from "cloudflare:test").
//   - assets: the real wrangler dev pipeline (worker + Workers assets router)
//     via wrangler's createTestHarness, for the SPA-fallback contract.
export default defineConfig({
  test: {
    projects: [
      {
        test: {
          name: "proxy",
          include: ["test/proxy.test.ts"],
        },
        plugins: [
          cloudflareTest({
            // Run the REAL worker module in workerd, not a re-implementation.
            main: "./index.ts",
            miniflare: {
              compatibilityDate: "2026-01-01",
              // API_ORIGIN mirrors wrangler.toml [vars]; the upstream host is
              // stubbed per-test by mocking globalThis.fetch.
              bindings: { API_ORIGIN: "https://upstream.test" },
            },
          }),
        ],
      },
      {
        test: {
          name: "assets",
          include: ["test/assets.test.ts"],
          environment: "node",
          // createTestHarness bundles the worker and boots workerd once.
          hookTimeout: 60_000,
          testTimeout: 30_000,
        },
      },
    ],
  },
});
