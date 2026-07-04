// Types the `env` / `exports` values from "cloudflare:workers" for the proxy
// tests (what `wrangler types` would generate; hand-kept since the test env is
// tiny — API_ORIGIN from vitest.config.ts, no ASSETS in the workers pool).
declare namespace Cloudflare {
  interface Env {
    API_ORIGIN: string;
  }
  interface GlobalProps {
    mainModule: typeof import("../index");
  }
}
