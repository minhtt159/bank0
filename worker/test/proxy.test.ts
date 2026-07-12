// Worker /api proxy-contract test (Tier B "cheaper intermediate" from
// docs/specs/spec-e2e-harness.md). Runs the REAL worker (../index.ts) in
// workerd via @cloudflare/vitest-pool-workers, stubs the upstream API by
// swapping globalThis.fetch (vitest-pool-workers v0.13+ removed the old
// cloudflare:test fetchMock), and asserts the four proxy invariants:
//   1. path rewrite (+ query preservation)
//   2. header forwarding (Authorization / Idempotency-Key survive; Host dropped)
//   3. method + body forwarding
//   4. SPA fallback / static asset handling (+ security headers on HTML)
// SELF (not exports.default) is kept deliberately: the SPA-fallback tests need
// the ASSETS binding, which exports.default.fetch does not expose.
import { SELF } from "cloudflare:test";
import { afterAll, afterEach, beforeAll, describe, expect, it } from "vitest";

// API_ORIGIN configured in vitest.config.ts.
const UPSTREAM = "https://upstream.test";

// Captured details of the request the worker made to the upstream API.
interface Captured {
  path: string; // pathname + search, as seen by the upstream
  method: string;
  headers: Record<string, string>;
  body?: string;
}

interface Interceptor {
  match: (pathWithSearch: string) => boolean;
  fired: boolean;
  captured?: Captured;
  fail?: boolean; // when set, the mock fetch throws to simulate an upstream error
}

// Registered per test, drained in afterEach. The worker forwards /api/* via the
// global fetch(); asset serving goes through env.ASSETS (a binding), so it is
// never intercepted here.
let interceptors: Interceptor[] = [];
const realFetch = globalThis.fetch;

beforeAll(() => {
  globalThis.fetch = (async (input: RequestInfo | URL, init?: RequestInit) => {
    const req = input instanceof Request ? input : new Request(input, init);
    const url = new URL(req.url);
    // Any outbound fetch we did NOT explicitly intercept is a bug we want to
    // see, not a silent pass-through to the real network.
    if (url.origin !== UPSTREAM) {
      throw new Error(`unexpected outbound fetch to ${url.href}`);
    }
    const pathWithSearch = url.pathname + url.search;
    const ic = interceptors.find((i) => !i.fired && i.match(pathWithSearch));
    if (!ic) {
      throw new Error(`no interceptor matched upstream ${pathWithSearch}`);
    }
    ic.fired = true;
    // Simulate the upstream being unreachable so the worker's try/catch -> 502
    // path fires instead of a normal upstream response.
    if (ic.fail) throw new Error("connection refused");
    const hasBody = req.method !== "GET" && req.method !== "HEAD";
    ic.captured = {
      path: pathWithSearch,
      method: req.method,
      headers: Object.fromEntries(req.headers.entries()),
      body: hasBody ? await req.text() : undefined,
    };
    return new Response("ok-from-upstream", { status: 200 });
  }) as typeof fetch;
});

afterEach(() => {
  // Surface unused interceptors so a rewrite that never fired fails loudly.
  const pending = interceptors.filter((i) => !i.fired).length;
  interceptors = [];
  if (pending) throw new Error(`${pending} upstream interceptor(s) never fired`);
});

afterAll(() => {
  globalThis.fetch = realFetch;
});

// Intercept exactly one upstream call, capture what the worker sent, and reply
// with a canned 200 so SELF.fetch resolves.
function captureUpstream(expectedPath: string | RegExp): { get(): Captured } {
  const ic: Interceptor = {
    match: (p) =>
      typeof expectedPath === "string" ? p === expectedPath : expectedPath.test(p),
    fired: false,
  };
  interceptors.push(ic);
  return {
    get() {
      if (!ic.captured) throw new Error("upstream interceptor never fired");
      return ic.captured;
    },
  };
}

// Register a one-shot interceptor whose upstream call throws, so we can assert
// the worker's controlled 502 (there is no reply-with-error primitive in the
// current harness).
function failUpstream(expectedPath: string | RegExp): void {
  interceptors.push({
    match: (p) =>
      typeof expectedPath === "string" ? p === expectedPath : expectedPath.test(p),
    fired: false,
    fail: true,
  });
}

describe("worker /api proxy contract", () => {
  describe("1. path rewrite", () => {
    it("rewrites /api/transfers -> /transfers", async () => {
      const up = captureUpstream("/transfers");
      const res = await SELF.fetch("https://bank0.test/api/transfers");
      expect(res.status).toBe(200);
      expect(up.get().path).toBe("/transfers");
    });

    it("rewrites bare /api -> /", async () => {
      const up = captureUpstream("/");
      const res = await SELF.fetch("https://bank0.test/api");
      expect(res.status).toBe(200);
      expect(up.get().path).toBe("/");
    });

    it("preserves the query string", async () => {
      const up = captureUpstream(/^\/transfers\?/);
      const res = await SELF.fetch(
        "https://bank0.test/api/transfers?limit=5&cursor=abc",
      );
      expect(res.status).toBe(200);
      expect(up.get().path).toBe("/transfers?limit=5&cursor=abc");
    });
  });

  describe("2. header forwarding", () => {
    it("forwards Authorization and Idempotency-Key unchanged; drops Host", async () => {
      const up = captureUpstream("/transfers");
      const res = await SELF.fetch("https://bank0.test/api/transfers", {
        headers: {
          authorization: "Bearer test-jwt-token",
          "idempotency-key": "idem-key-123",
          host: "bank0.test",
        },
      });
      expect(res.status).toBe(200);

      const h = up.get().headers;
      expect(h["authorization"]).toBe("Bearer test-jwt-token");
      expect(h["idempotency-key"]).toBe("idem-key-123");
      // The inbound Host must NOT be forwarded (the worker deletes it). It must
      // never leak the client-facing bank0.test to the upstream.
      expect(h["host"]).toBeUndefined();
    });
  });

  describe("3. method + body forwarding", () => {
    it("forwards POST with its JSON body intact", async () => {
      const up = captureUpstream("/transfers");
      const payload = JSON.stringify({
        to_iban: "NL00BANK0000000001",
        amount_minor: 1500,
      });
      const res = await SELF.fetch("https://bank0.test/api/transfers", {
        method: "POST",
        headers: { "content-type": "application/json" },
        body: payload,
      });
      expect(res.status).toBe(200);

      const c = up.get();
      expect(c.method).toBe("POST");
      expect(c.path).toBe("/transfers");
      expect(c.body).toBe(payload);
    });
  });

  describe("4. SPA fallback / static assets", () => {
    it("serves the SPA shell for a deep link and applies security headers", async () => {
      // /transfer/123 is a client-side route, not an asset -> SPA fallback
      // returns index.html (text/html), which the worker decorates.
      const res = await SELF.fetch("https://bank0.test/transfer/123");
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
      expect(res.headers.get("strict-transport-security")).toContain(
        "max-age=",
      );
    });

    it("passes a non-HTML asset through without security headers", async () => {
      const res = await SELF.fetch("https://bank0.test/manifest.webmanifest");
      expect(res.status).toBe(200);
      expect(res.headers.get("content-type") ?? "").not.toContain("text/html");
      // No HTML decoration on non-HTML assets.
      expect(res.headers.get("content-security-policy")).toBeNull();

      const body = await res.text();
      expect(body).toContain('"bank0"');
    });
  });

  describe("5. upstream failure", () => {
    it("returns a controlled JSON 502 when the upstream errors", async () => {
      failUpstream("/transfers");
      const res = await SELF.fetch("https://bank0.test/api/transfers");
      expect(res.status).toBe(502);
      expect(res.headers.get("content-type")).toContain("application/json");
      expect((await res.json()).error).toBe("bad_gateway");
    });
  });
});
