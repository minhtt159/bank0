// bank0 operator console — client touches: active left-nav highlighting, a top
// progress bar during HTMX requests (skipping the 15s auto-refresh polls so it
// doesn't flicker), the collapsible right-rail behaviour (opens when content is
// swapped into #rail, closes via its × button), and toast dismissal. Errors are
// rendered by the server as Toast partials (Server.consoleFail); the client only
// toasts non-HTML failures and network errors. Loaded at the end of <body>.
//
// htmx 4 events carry the request context in e.detail.ctx (sourceElement,
// target, response, ...) rather than htmx 2's flat detail.elt / .target / .xhr.
(function () {
  if (window.bank0Console) return; // guard against a second run if the body is ever re-swapped
  window.bank0Console = true;
  window.openRail = function () { var l = document.getElementById('layout'); if (l) l.classList.add('rail-open'); };
  window.closeRail = function () { var l = document.getElementById('layout'); if (l) l.classList.remove('rail-open'); };
  var setActive = function (item) {
    if (!item) return;
    document.querySelectorAll('.leftnav .navitem').forEach(function (n) { n.classList.remove('active'); });
    item.classList.add('active');
  };
  document.addEventListener('click', function (e) { setActive(e.target.closest('.leftnav .navitem')); });
  // deep link / F5 / history restore: highlight the panel the URL names (the shell loads it)
  var here = location.pathname === '/' ? '/console/dashboard' : location.pathname;
  setActive(document.querySelector('.leftnav .navitem[hx-get="' + here + '"]'));
  var dismiss = function (t) {
    if (t.dataset.armed) return;
    t.dataset.armed = '1';
    setTimeout(function () { t.classList.add('out'); }, 3800);
    setTimeout(function () { t.remove(); }, 4200);
  };
  window.toast = function (msg, kind) {
    var wrap = document.getElementById('toasts');
    if (!wrap) return;
    var t = document.createElement('div');
    t.className = 'toast' + (kind ? ' ' + kind : '');
    t.textContent = msg;
    wrap.appendChild(t);
    dismiss(t);
  };
  if (window.htmx) {
    var bar = function () { return document.getElementById('progress'); };
    var ctx = function (evt) { return (evt.detail && evt.detail.ctx) || {}; };
    htmx.on('htmx:before:request', function (evt) {
      var el = ctx(evt).sourceElement;
      var trg = el && el.getAttribute && el.getAttribute('hx-trigger');
      if (trg && trg.indexOf('every') !== -1) return; // skip auto-refresh polling
      bar().classList.add('on');
    });
    htmx.on('htmx:after:request', function () { bar().classList.remove('on'); });
    htmx.on('htmx:after:swap', function (evt) {
      var target = ctx(evt).target;
      if (!target) return;
      if (target.id === 'toasts') { target.querySelectorAll('.toast').forEach(dismiss); return; } // server-rendered Toast partials
      if (target.id !== 'rail') return;
      window.openRail();
      var h = target.querySelector('h2'); // keyboard users land on the detail, not where they were
      if (h) { h.tabIndex = -1; h.focus({ preventScroll: true }); }
    });
    htmx.on('htmx:response:error', function (evt) {
      var res = ctx(evt).response;
      var status = res && res.status;
      if (status === 401) { location.href = '/login'; return; }
      // The server answers console errors with an HTML Toast partial that htmx swaps into #toasts;
      // only a non-HTML error body (a JSON error, a proxy page) still needs a client-side toast.
      if (res && (res.headers.get('content-type') || '').indexOf('text/html') === 0) return;
      window.toast('Request failed' + (status ? ' (' + status + ')' : ''), 'bad');
    });
    htmx.on('htmx:error', function () { window.toast('Network error — is the server up?', 'bad'); });
  }
})();
