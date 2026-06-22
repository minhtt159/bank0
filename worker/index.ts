// Cloudflare Worker for the bank0 customer app (docs/07).
//   - serves the built SPA from the ASSETS binding (SPA fallback in wrangler.toml)
//   - proxies /api/* to the client API so the browser is always same-origin
//     (no CORS); this is also the seam for a future token-holding BFF.

export interface Env {
  ASSETS: Fetcher;
  API_ORIGIN: string;
}

const SECURITY_HEADERS: Record<string, string> = {
  "Strict-Transport-Security": "max-age=31536000; includeSubDomains",
  "X-Content-Type-Options": "nosniff",
  "Referrer-Policy": "strict-origin-when-cross-origin",
  "Content-Security-Policy":
    "default-src 'self'; connect-src 'self'; img-src 'self' data:; " +
    "style-src 'self' 'unsafe-inline'; base-uri 'self'; frame-ancestors 'none'",
};

export default {
  async fetch(req: Request, env: Env): Promise<Response> {
    const url = new URL(req.url);

    // --- /api/* -> client API (strip the /api prefix, forward Bearer) ---
    if (url.pathname === "/api" || url.pathname.startsWith("/api/")) {
      const upstream = new URL(env.API_ORIGIN);
      upstream.pathname = url.pathname.replace(/^\/api/, "") || "/";
      upstream.search = url.search;

      const headers = new Headers(req.headers);
      headers.delete("host");

      const hasBody = req.method !== "GET" && req.method !== "HEAD";
      const init: RequestInit = {
        method: req.method,
        headers,
        redirect: "manual",
      };
      if (hasBody) {
        init.body = req.body;
        // streaming a request body requires half-duplex in Workers/fetch
        (init as RequestInit & { duplex: "half" }).duplex = "half";
      }
      // Time-box the upstream call and turn any failure (origin down, DNS, timeout)
      // into a controlled JSON 502 rather than a raw Cloudflare 1101 error page.
      const ac = new AbortController();
      const timer = setTimeout(() => ac.abort(), 15_000);
      try {
        return await fetch(upstream.toString(), { ...init, signal: ac.signal });
      } catch (e) {
        const reason = (e as Error)?.name === "AbortError" ? "upstream timed out" : "upstream unavailable";
        return new Response(JSON.stringify({ error: "bad_gateway", message: reason }), {
          status: 502,
          headers: { "content-type": "application/json" },
        });
      } finally {
        clearTimeout(timer);
      }
    }

    // --- static SPA assets ---
    const res = await env.ASSETS.fetch(req);
    const ct = res.headers.get("content-type") ?? "";
    if (ct.includes("text/html")) {
      const headers = new Headers(res.headers);
      for (const [k, v] of Object.entries(SECURITY_HEADERS)) headers.set(k, v);
      return new Response(res.body, { status: res.status, headers });
    }
    return res;
  },
};
