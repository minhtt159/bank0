import { readFileSync } from "node:fs";
import { describe, expect, it } from "vitest";

// The Worker serves HTML under `default-src 'self'` with no script-src (worker/index.ts),
// so an inline <script> is blocked in production while working fine in `vite dev` —
// a break that only shows up after deploy. Keep every script in index.html external.
describe("index.html", () => {
  const html = readFileSync(new URL("../index.html", import.meta.url), "utf8");

  it("has no inline script bodies", () => {
    const inline = [...html.matchAll(/<script(?![^>]*\bsrc=)[^>]*>([\s\S]*?)<\/script>/g)]
      .map((m) => m[1].trim())
      .filter(Boolean);
    expect(inline).toEqual([]);
  });

  it("boots the theme before the stylesheet can paint", () => {
    expect(html).toContain('<script src="/theme-boot.js"></script>');
  });
});
