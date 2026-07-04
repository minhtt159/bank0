// SPA fallback / static-asset half of the worker proxy contract.
// vitest-pool-workers >= 0.16 invokes the worker via `exports.default.fetch`,
// which bypasses the Workers assets router, so the ASSETS binding is exercised
// here instead through wrangler's test harness: the real worker + the real
// assets pipeline in workerd.
//
// Hermetic fixture assets — deliberately NOT the real `../web/app/dist` build
// output (that dir is gitignored and may be absent in CI). The inline config
// mirrors wrangler.toml ([assets] SPA fallback + [vars]) so the test stays
// self-contained and CI-friendly.
import { fileURLToPath } from "node:url";
import { afterAll, beforeAll, describe, expect, it } from "vitest";
import { createTestHarness } from "wrangler";

const workerDir = fileURLToPath(new URL("..", import.meta.url));

const harness = createTestHarness({
  root: workerDir,
  workers: [
    {
      config: {
        name: "bank0-webapp-test",
        main: "index.ts",
        compatibility_date: "2026-01-01",
        assets: {
          directory: "test/fixtures/assets",
          binding: "ASSETS",
          not_found_handling: "single-page-application",
        },
        vars: { API_ORIGIN: "https://upstream.test" },
      },
    },
  ],
});

beforeAll(async () => {
  await harness.listen();
});

afterAll(async () => {
  await harness.close();
});

describe("worker SPA fallback / static assets", () => {
  it("serves the SPA shell for a deep link and applies security headers", async () => {
    // /transfer/123 is a client-side route, not an asset -> SPA fallback
    // returns index.html (text/html), which the worker decorates.
    const res = await harness.fetch("/transfer/123");
    expect(res.status).toBe(200);
    expect(res.headers.get("content-type")).toContain("text/html");

    const body = await res.text();
    expect(body).toContain("spa-shell");

    // Security headers are added on HTML responses.
    expect(res.headers.get("content-security-policy")).toBeTruthy();
    expect(res.headers.get("x-content-type-options")).toBe("nosniff");
    expect(res.headers.get("referrer-policy")).toBe(
      "strict-origin-when-cross-origin",
    );
    expect(res.headers.get("strict-transport-security")).toContain("max-age=");
  });

  it("passes a non-HTML asset through without security headers", async () => {
    const res = await harness.fetch("/manifest.webmanifest");
    expect(res.status).toBe(200);
    expect(res.headers.get("content-type") ?? "").not.toContain("text/html");
    // No HTML decoration on non-HTML assets.
    expect(res.headers.get("content-security-policy")).toBeNull();

    const body = await res.text();
    expect(body).toContain('"bank0"');
  });
});
