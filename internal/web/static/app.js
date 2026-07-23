// HAProxy Controller - control panel behaviour.
// Powered by Ebdaa.me - https://ebdaa.me
(function () {
  'use strict';

  // Confirmation for destructive actions, driven by a data attribute so no
  // inline handlers are needed (the CSP forbids them).
  document.addEventListener('submit', function (e) {
    var form = e.target;
    var msg = form.getAttribute('data-confirm');
    if (msg && !window.confirm(msg)) {
      e.preventDefault();
      return;
    }
    // Guard against double submission of long-running actions such as Apply.
    var btn = form.querySelector('[data-busy]');
    if (btn) {
      var label = btn.getAttribute('data-busy');
      window.setTimeout(function () {
        btn.disabled = true;
        btn.textContent = label;
      }, 0);
    }
  });

  // Show or hide dependent fieldsets, e.g. TLS options only when SSL is on.
  function syncToggles() {
    document.querySelectorAll('[data-toggle-target]').forEach(function (input) {
      var sel = input.getAttribute('data-toggle-target');
      var on = input.type === 'checkbox' ? input.checked : !!input.value;
      var invert = input.hasAttribute('data-toggle-invert');
      document.querySelectorAll(sel).forEach(function (el) {
        el.hidden = invert ? on : !on;
      });
    });
  }
  document.addEventListener('change', function (e) {
    if (e.target.hasAttribute && e.target.hasAttribute('data-toggle-target')) syncToggles();
  });
  syncToggles();

  // Reveal an inline editor row (add server, add bind, edit rule).
  document.addEventListener('click', function (e) {
    var t = e.target.closest('[data-reveal]');
    if (!t) return;
    e.preventDefault();
    var el = document.querySelector(t.getAttribute('data-reveal'));
    if (!el) return;
    el.hidden = !el.hidden;
    if (!el.hidden) {
      var first = el.querySelector('input:not([type=hidden]), select, textarea');
      if (first) first.focus();
    }
  });

  // Copy a code block to the clipboard.
  document.addEventListener('click', function (e) {
    var btn = e.target.closest('[data-copy]');
    if (!btn) return;
    e.preventDefault();
    var src = document.querySelector(btn.getAttribute('data-copy'));
    if (!src || !navigator.clipboard) return;
    navigator.clipboard.writeText(src.textContent).then(function () {
      var original = btn.textContent;
      btn.textContent = 'Copied';
      window.setTimeout(function () { btn.textContent = original; }, 1600);
    });
  });

  // Live status refresh on pages that opt in.
  var live = document.querySelector('[data-live-status]');
  if (live) {
    var refresh = function () {
      fetch('/api/status', { headers: { 'Accept': 'application/json' } })
        .then(function (r) { return r.ok ? r.json() : null; })
        .then(function (data) {
          if (!data) return;
          document.querySelectorAll('[data-stat]').forEach(function (el) {
            var key = el.getAttribute('data-stat');
            if (data.info && data.info[key] !== undefined) {
              el.textContent = data.info[key];
            }
          });
        })
        .catch(function () { /* the panel stays usable if HAProxy is down */ });
    };
    window.setInterval(refresh, 10000);
  }
})();
