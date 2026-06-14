import { fileURLToPath } from "node:url";
import { defineWorkersConfig } from "@cloudflare/vitest-pool-workers/config";

// Hermetic fixture assets — deliberately NOT the real `../web/app/dist` build
// output (that dir is gitignored and may be absent in CI). Mirrors the
// wrangler.toml [assets] binding (SPA fallback) so the proxy contract test
// stays self-contained and CI-friendly.
const assetsDir = fileURLToPath(
  new URL("./test/fixtures/assets", import.meta.url),
);

export default defineWorkersConfig({
  test: {
    poolOptions: {
      workers: {
        // Run the REAL worker module in workerd (faithful to the spec's
        // Miniflare/workerd intent), not a re-implementation.
        main: "./index.ts",
        miniflare: {
          compatibilityDate: "2026-01-01",
          // API_ORIGIN mirrors wrangler.toml [vars]; the upstream host is
          // stubbed per-test via fetchMock from "cloudflare:test".
          bindings: { API_ORIGIN: "https://upstream.test" },
          // ASSETS binding mirrors wrangler.toml [assets].
          assets: {
            directory: assetsDir,
            binding: "ASSETS",
            assetConfig: {
              not_found_handling: "single-page-application",
              html_handling: "none",
            },
          },
        },
      },
    },
  },
});
