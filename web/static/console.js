// bank0 operator console — client touches: active left-nav highlighting, a top
// progress bar during HTMX requests (skipping the 15s auto-refresh polls so it
// doesn't flicker), the collapsible right-rail behaviour (opens when content is
// swapped into #rail, closes via its × button), and toast notifications for
// failed/aborted HTMX requests. Loaded at the end of <body>.
//
// htmx 4 events carry the request context in e.detail.ctx (sourceElement,
// target, response, ...) rather than htmx 2's flat detail.elt / .target / .xhr.
(function () {
  window.openRail = function () { var l = document.getElementById('layout'); if (l) l.classList.add('rail-open'); };
  window.closeRail = function () { var l = document.getElementById('layout'); if (l) l.classList.remove('rail-open'); };
  document.addEventListener('click', function (e) {
    var item = e.target.closest('.leftnav .navitem');
    if (!item) return;
    document.querySelectorAll('.leftnav .navitem').forEach(function (n) { n.classList.remove('active'); });
    item.classList.add('active');
  });
  window.toast = function (msg, kind) {
    var wrap = document.getElementById('toasts');
    if (!wrap) return;
    var t = document.createElement('div');
    t.className = 'toast' + (kind ? ' ' + kind : '');
    t.textContent = msg;
    wrap.appendChild(t);
    setTimeout(function () { t.classList.add('out'); }, 3800);
    setTimeout(function () { t.remove(); }, 4200);
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
      if (!target || target.id !== 'rail') return;
      window.openRail();
      var h = target.querySelector('h2'); // keyboard users land on the detail, not where they were
      if (h) { h.tabIndex = -1; h.focus({ preventScroll: true }); }
    });
    htmx.on('htmx:response:error', function (evt) {
      var res = ctx(evt).response;
      var status = res && res.status;
      var msg = 'Request failed' + (status ? ' (' + status + ')' : '');
      if (status === 403) msg = 'Not allowed — your role can’t do that.';
      if (status === 401) { location.href = '/login'; return; }
      window.toast(msg, 'bad');
    });
    htmx.on('htmx:error', function () { window.toast('Network error — is the server up?', 'bad'); });
  }
})();
