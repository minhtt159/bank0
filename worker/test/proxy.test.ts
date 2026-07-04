// Worker /api proxy-contract test. Runs the REAL worker (../index.ts) in
// workerd via @cloudflare/vitest-pool-workers, stubs the upstream API by
// mocking globalThis.fetch (the worker runs in the same isolate as the tests,
// so the mock applies to it), and asserts the proxy invariants:
//   1. path rewrite (+ query preservation)
//   2. header forwarding (Authorization / Idempotency-Key survive; Host dropped)
//   3. method + body forwarding
// The SPA-fallback / static-asset invariants live in test/assets.test.ts —
// vitest-pool-workers >= 0.16 can't route through the ASSETS binding.
import { exports } from "cloudflare:workers";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

// API_ORIGIN configured in vitest.config.ts.
const UPSTREAM = "https://upstream.test";

// Captured details of the request the worker made to the upstream API.
interface Captured {
  path: string; // pathname + search, as seen by the upstream
  method: string;
  headers: Headers;
  body?: string;
}

let captured: Captured | undefined;
let upstreamCalls: number;

beforeEach(() => {
  captured = undefined;
  upstreamCalls = 0;
  // Any outbound fetch the worker makes to a host other than the stubbed
  // upstream is a bug we want to see, not a silent pass-through to the real
  // network (the old fetchMock.disableNetConnect()).
  vi.stubGlobal(
    "fetch",
    async (input: RequestInfo | URL, init?: RequestInit) => {
      const req = new Request(input, init);
      const url = new URL(req.url);
      if (url.origin !== UPSTREAM) {
        throw new Error(`unexpected outbound fetch: ${req.url}`);
      }
      upstreamCalls++;
      captured = {
        path: url.pathname + url.search,
        method: req.method,
        headers: req.headers,
        body:
          req.method === "GET" || req.method === "HEAD"
            ? undefined
            : await req.text(),
      };
      return new Response("ok-from-upstream");
    },
  );
});

afterEach(() => {
  vi.unstubAllGlobals();
});

// The one upstream call the worker must have made (fails loudly if the proxy
// never fired, or fired more than once).
function upstream(): Captured {
  expect(upstreamCalls).toBe(1);
  if (!captured) throw new Error("upstream fetch never fired");
  return captured;
}

describe("worker /api proxy contract", () => {
  describe("1. path rewrite", () => {
    it("rewrites /api/transfers -> /transfers", async () => {
      const res = await exports.default.fetch(
        "https://bank0.test/api/transfers",
      );
      expect(res.status).toBe(200);
      expect(upstream().path).toBe("/transfers");
    });

    it("rewrites bare /api -> /", async () => {
      const res = await exports.default.fetch("https://bank0.test/api");
      expect(res.status).toBe(200);
      expect(upstream().path).toBe("/");
    });

    it("preserves the query string", async () => {
      const res = await exports.default.fetch(
        "https://bank0.test/api/transfers?limit=5&cursor=abc",
      );
      expect(res.status).toBe(200);
      expect(upstream().path).toBe("/transfers?limit=5&cursor=abc");
    });
  });

  describe("2. header forwarding", () => {
    it("forwards Authorization and Idempotency-Key unchanged; drops Host", async () => {
      const res = await exports.default.fetch(
        "https://bank0.test/api/transfers",
        {
          headers: {
            authorization: "Bearer test-jwt-token",
            "idempotency-key": "idem-key-123",
            host: "bank0.test",
          },
        },
      );
      expect(res.status).toBe(200);

      const h = upstream().headers;
      expect(h.get("authorization")).toBe("Bearer test-jwt-token");
      expect(h.get("idempotency-key")).toBe("idem-key-123");
      // The inbound Host must NOT be forwarded (the worker deletes it). It must
      // never leak the client-facing bank0.test to the upstream.
      expect(h.get("host")).toBeNull();
    });
  });

  describe("3. method + body forwarding", () => {
    it("forwards POST with its JSON body intact", async () => {
      const payload = JSON.stringify({
        to_iban: "NL00BANK0000000001",
        amount_minor: 1500,
      });
      const res = await exports.default.fetch(
        "https://bank0.test/api/transfers",
        {
          method: "POST",
          headers: { "content-type": "application/json" },
          body: payload,
        },
      );
      expect(res.status).toBe(200);

      const c = upstream();
      expect(c.method).toBe("POST");
      expect(c.path).toBe("/transfers");
      expect(c.body).toBe(payload);
    });
  });
});
