// Catppuccin flavour bootstrap. Runs render-blocking from <head> so a dark-mode
// phone never flashes Latte on a cold start, and lives in public/ rather than
// inline because the Worker's CSP is `default-src 'self'` with no script-src —
// an inline <script> would be blocked in production but not in `vite dev`.
// Mirrors src/lib/theme.ts (same key, same crust colours); keep the two in step.
(function () {
  try {
    var f = localStorage.getItem("bank0-theme");
    if (["latte", "frappe", "macchiato", "mocha"].indexOf(f) >= 0) {
      document.documentElement.setAttribute("data-theme", f);
    } else {
      f = matchMedia("(prefers-color-scheme: dark)").matches ? "mocha" : "latte";
    }
    var crust = { latte: "#dce0e8", frappe: "#232634", macchiato: "#181926", mocha: "#11111b" };
    document.querySelector('meta[name="theme-color"]').setAttribute("content", crust[f]);
  } catch (e) {
    /* private mode / storage blocked: fall through to the prefers-color-scheme default */
  }
})();
