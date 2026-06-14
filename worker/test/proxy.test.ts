// Worker /api proxy-contract test (Tier B "cheaper intermediate" from
// docs/specs/spec-e2e-harness.md). Runs the REAL worker (../index.ts) in
// workerd via @cloudflare/vitest-pool-workers, stubs the upstream API with
// fetchMock, and asserts the four proxy invariants:
//   1. path rewrite (+ query preservation)
//   2. header forwarding (Authorization / Idempotency-Key survive; Host dropped)
//   3. method + body forwarding
//   4. SPA fallback / static asset handling (+ security headers on HTML)
import { SELF, fetchMock } from "cloudflare:test";
import { afterEach, beforeAll, describe, expect, it } from "vitest";

// API_ORIGIN configured in vitest.config.ts.
const UPSTREAM = "https://upstream.test";

// Captured details of the request the worker made to the upstream API.
interface Captured {
  path: string; // pathname + search, as seen by the upstream
  method: string;
  headers: Record<string, string>;
  body?: string;
}

beforeAll(() => {
  // Any outbound fetch the worker makes that we did NOT explicitly intercept
  // is a bug we want to see, not a silent pass-through to the real network.
  fetchMock.activate();
  fetchMock.disableNetConnect();
});

afterEach(() => {
  // Surface unused interceptors so a rewrite that never fired fails loudly.
  fetchMock.assertNoPendingInterceptors();
});

// Intercept exactly one upstream call, capture what the worker sent, and reply
// with a canned 200 so SELF.fetch resolves.
function captureUpstream(expectedPath: string | RegExp): { get(): Captured } {
  let captured: Captured | undefined;
  fetchMock
    .get(UPSTREAM)
    .intercept({ path: expectedPath, method: () => true })
    .reply((opts) => {
      const headers =
        opts.headers instanceof Headers
          ? Object.fromEntries(opts.headers.entries())
          : ((opts.headers as Record<string, string>) ?? {});
      captured = {
        path: opts.path,
        method: opts.method,
        headers,
        body: typeof opts.body === "string" ? opts.body : undefined,
      };
      return { statusCode: 200, data: "ok-from-upstream" };
    });
  return {
    get() {
      if (!captured) throw new Error("upstream interceptor never fired");
      return captured;
    },
  };
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
});
