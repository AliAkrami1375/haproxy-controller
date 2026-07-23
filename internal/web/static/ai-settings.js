// HAProxy Controller — assistant settings.
// Powered by Ebdaa.me — https://ebdaa.me
(function () {
  'use strict';

  var csrf = document.querySelector('input[name=csrf_token]');
  csrf = csrf ? csrf.value : '';

  var loadBtn = document.getElementById('load-models');
  var status = document.getElementById('models-status');
  var panel = document.getElementById('models-panel');
  var body = document.getElementById('models-body');
  var datalist = document.getElementById('model-list');
  var modelInput = document.getElementById('model');

  if (loadBtn) {
    loadBtn.addEventListener('click', function () {
      loadBtn.disabled = true;
      status.textContent = 'Fetching…';
      fetch('/settings/ai/models').then(function (r) { return r.json(); })
        .then(function (data) {
          if (data.error) { status.textContent = data.error; return; }
          var models = data.models || [];
          status.textContent = models.length + ' free, tool-capable model(s)';
          datalist.innerHTML = '';
          body.innerHTML = '';
          models.forEach(function (m) {
            var opt = document.createElement('option');
            opt.value = m.id;
            datalist.appendChild(opt);

            var tr = document.createElement('tr');
            var ctx = m.context ? (Math.round(m.context / 1000) + 'k') : '—';
            tr.innerHTML = '<td class="mono">' + m.id + '</td><td class="subtle">' + ctx + '</td>' +
              '<td class="right"><button type="button" class="btn btn-sm" data-pick="' + m.id + '">Use</button></td>';
            body.appendChild(tr);
          });
          panel.hidden = models.length === 0;
        })
        .catch(function () { status.textContent = 'Could not reach OpenRouter.'; })
        .finally(function () { loadBtn.disabled = false; });
    });
  }

  document.addEventListener('click', function (e) {
    var pick = e.target.closest('[data-pick]');
    if (pick) { modelInput.value = pick.getAttribute('data-pick'); }
  });

  var testBtn = document.getElementById('test-btn');
  var testStatus = document.getElementById('test-status');
  if (testBtn) {
    testBtn.addEventListener('click', function () {
      testBtn.disabled = true;
      testStatus.textContent = 'Testing…';
      testStatus.style.color = '';
      var b = new URLSearchParams(); b.set('csrf_token', csrf);
      fetch('/settings/ai/test', {
        method: 'POST',
        headers: { 'Content-Type': 'application/x-www-form-urlencoded', 'X-CSRF-Token': csrf },
        body: b.toString()
      }).then(function (r) { return r.json(); })
        .then(function (data) {
          if (data.ok) {
            testStatus.textContent = '✓ Connected — ' + data.model + ' replied.';
            testStatus.style.color = 'var(--success)';
          } else {
            testStatus.textContent = data.error || 'Test failed.';
            testStatus.style.color = 'var(--danger)';
          }
        })
        .catch(function () { testStatus.textContent = 'Test failed.'; testStatus.style.color = 'var(--danger)'; })
        .finally(function () { testBtn.disabled = false; });
    });
  }
})();
