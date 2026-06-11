// bank0 operator console — client touches: active left-nav highlighting, a top
// progress bar during HTMX requests (skipping the 15s auto-refresh polls so it
// doesn't flicker), the collapsible right-rail behaviour (opens when content is
// swapped into #rail, closes via its × button), and toast notifications for
// failed/aborted HTMX requests. Loaded at the end of <body>.
(function () {
  window.openRail = function () { var l = document.getElementById('layout'); if (l) l.classList.add('rail-open'); };
  window.closeRail = function () { var l = document.getElementById('layout'); if (l) l.classList.remove('rail-open'); };
  document.addEventListener('click', function (e) {
    var item = e.target.closest('.leftnav .navitem');
    if (!item) return;
    document.querySelectorAll('.leftnav .navitem').forEach(function (n) { n.classList.remove('active'); });
    item.classList.add('active');
  });
  document.body.addEventListener('htmx:afterSwap', function (e) {
    if (e.detail && e.detail.target && e.detail.target.id === 'rail') window.openRail();
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
    htmx.on('htmx:beforeRequest', function (evt) {
      var trg = evt.detail && evt.detail.elt && evt.detail.elt.getAttribute('hx-trigger');
      if (trg && trg.indexOf('every') !== -1) return; // skip auto-refresh polling
      bar().classList.add('on');
    });
    htmx.on('htmx:afterRequest', function () { bar().classList.remove('on'); });
    htmx.on('htmx:responseError', function (evt) {
      var x = evt.detail && evt.detail.xhr;
      var msg = 'Request failed' + (x && x.status ? ' (' + x.status + ')' : '');
      if (x && x.status === 403) msg = 'Not allowed — your role can’t do that.';
      if (x && x.status === 401) { location.href = '/login'; return; }
      window.toast(msg, 'bad');
    });
    htmx.on('htmx:sendError', function () { window.toast('Network error — is the server up?', 'bad'); });
  }
})();
