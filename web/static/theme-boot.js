// Catppuccin flavour bootstrap for the operator console. Stamps the stored
// flavour on <html data-theme=…> (no attribute = follow the OS) before first
// paint, so there is no flash of the wrong theme, and owns the picker.
//
// A separate file rather than an inline <script> in <head>: the console's CSP
// is `script-src 'self'` (issue #122), which blocks inline script. It is still
// render-blocking from <head>, which is what makes the pre-paint part work.
(function () {
  var flavours = ['latte', 'frappe', 'macchiato', 'mocha'], t = '';
  try { t = localStorage.getItem('bank0-theme') || ''; } catch (e) {}
  if (flavours.indexOf(t) < 0) t = '';
  if (t) document.documentElement.dataset.theme = t;
  var setTheme = function (n) {
    if (flavours.indexOf(n) < 0) n = '';
    if (n) document.documentElement.dataset.theme = n; else delete document.documentElement.dataset.theme;
    try { if (n) localStorage.setItem('bank0-theme', n); else localStorage.removeItem('bank0-theme'); } catch (e) {}
  };
  document.addEventListener('DOMContentLoaded', function () {
    var s = document.getElementById('theme-select');
    if (!s) return;
    s.value = t;
    // Was an onchange= attribute; inline handlers are blocked by script-src 'self'.
    s.addEventListener('change', function () { setTheme(this.value); });
  });
})();
