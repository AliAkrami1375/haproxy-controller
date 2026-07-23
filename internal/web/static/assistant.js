// HAProxy Controller — assistant chat.
// Powered by Ebdaa.me — https://ebdaa.me
(function () {
  'use strict';

  var chat = document.getElementById('chat');
  if (!chat) return;

  var conversationID = chat.getAttribute('data-conversation');
  var csrf = chat.getAttribute('data-csrf');
  var stream = document.getElementById('chat-stream');
  var scroll = document.getElementById('chat-scroll');
  var form = document.getElementById('composer-form');
  var input = document.getElementById('composer-input');

  // --- icons reused for dynamically appended messages
  var ICON = {
    user: '<svg viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.4" stroke-linecap="round" stroke-linejoin="round"><circle cx="8" cy="5.5" r="2.6"/><path d="M3.2 13c.6-2 2.4-3 4.8-3s4.2 1 4.8 3"/></svg>',
    bot: '<svg viewBox="0 0 16 16" fill="none" stroke="white" stroke-width="1.4" stroke-linecap="round" stroke-linejoin="round"><path d="M3 3.5h10v6.5H6l-3 2.2V10H3z"/></svg>',
    ok: '<svg viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round"><circle cx="8" cy="8" r="6.5"/><path d="m5.5 8 1.8 1.8L11 6"/></svg>',
    bad: '<svg viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round"><circle cx="8" cy="8" r="6.5"/><path d="M8 5v3.5M8 11h.01"/></svg>',
    list: '<svg viewBox="0 0 16 16" width="14" height="14" fill="none" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" stroke-linejoin="round"><path d="M2 4h12M2 8h12M2 12h8"/></svg>',
    chev: '<svg class="chev" viewBox="0 0 16 16" width="13" height="13" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round"><path d="M6 4l4 4-4 4"/></svg>'
  };

  function esc(s) {
    return String(s).replace(/[&<>"]/g, function (c) {
      return { '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;' }[c];
    });
  }

  // Minimal, safe inline markdown: escape first, then `code` and **bold**.
  function md(s) {
    return esc(s)
      .replace(/`([^`]+)`/g, '<code>$1</code>')
      .replace(/\*\*([^*]+)\*\*/g, '<strong>$1</strong>');
  }

  function toBottom() { scroll.scrollTop = scroll.scrollHeight; }

  function clearEmpty() {
    var empty = stream.querySelector('.chat-empty');
    if (empty) empty.remove();
  }

  function addUser(text) {
    clearEmpty();
    var el = document.createElement('div');
    el.className = 'msg user';
    el.innerHTML = '<div class="msg-avatar">' + ICON.user + '</div>' +
      '<div class="msg-body"><div class="bubble">' + esc(text) + '</div></div>';
    stream.appendChild(el);
    toBottom();
  }

  var thinkingEl = null;
  function addThinking() {
    thinkingEl = document.createElement('div');
    thinkingEl.className = 'msg assistant';
    thinkingEl.innerHTML = '<div class="msg-avatar">' + ICON.bot + '</div>' +
      '<div class="msg-body"><div class="bubble"><div class="thinking">' +
      '<span class="dots"><span></span><span></span><span></span></span>' +
      '<span id="thinking-label">Working through it…</span></div></div></div>';
    stream.appendChild(thinkingEl);
    toBottom();
  }
  function removeThinking() { if (thinkingEl) { thinkingEl.remove(); thinkingEl = null; } }

  function stepHTML(s) {
    var icon = s.ok ? ICON.ok : ICON.bad;
    var detail = s.detail ? '<div class="step-detail">' + esc(s.detail) + '</div>' : '';
    return '<div class="step ' + (s.ok ? 'ok' : 'bad') + '">' +
      '<span class="step-icon">' + icon + '</span>' +
      '<div class="step-main"><span class="step-kind">' + esc(s.kind || '') + '</span>' +
      esc(s.summary || '') + detail + '</div></div>';
  }

  function addAssistant(data) {
    var el = document.createElement('div');
    el.className = 'msg assistant';

    var stepsHTML = '';
    if (data.steps && data.steps.length) {
      var inner = data.steps.map(stepHTML).join('');
      var n = data.steps.length;
      stepsHTML = '<details class="steps"><summary class="steps-head">' + ICON.list +
        ' ' + n + ' step' + (n === 1 ? '' : 's') + ICON.chev + '</summary>' +
        '<div class="steps-body">' + inner + '</div></details>';
    }

    var actions = '';
    if (data.changed) {
      actions = '<div class="outcome-actions"><a href="/config" class="btn btn-sm btn-primary">Review &amp; apply</a>' +
        (data.valid ? '' : '<span class="badge bad">needs attention</span>') + '</div>';
    }

    el.innerHTML = '<div class="msg-avatar">' + ICON.bot + '</div>' +
      '<div class="msg-body">' + stepsHTML +
      '<div class="bubble">' + md(data.reply || '') + '</div>' + actions + '</div>';
    stream.appendChild(el);
    toBottom();
  }

  function addError(message) {
    var el = document.createElement('div');
    el.className = 'msg assistant';
    el.innerHTML = '<div class="msg-avatar">' + ICON.bot + '</div>' +
      '<div class="msg-body"><div class="bubble" style="border-color:var(--danger);color:var(--danger)">' +
      esc(message) + '</div></div>';
    stream.appendChild(el);
    toBottom();
  }

  function send(text) {
    if (!text.trim()) return;
    input.disabled = true;
    form.querySelector('.send').disabled = true;

    addUser(text);
    input.value = '';
    autoGrow();
    addThinking();

    var body = new URLSearchParams();
    body.set('csrf_token', csrf);
    body.set('message', text);

    fetch('/assistant/' + conversationID + '/message', {
      method: 'POST',
      headers: { 'Content-Type': 'application/x-www-form-urlencoded', 'X-CSRF-Token': csrf },
      body: body.toString()
    }).then(function (r) { return r.json(); })
      .then(function (data) {
        removeThinking();
        if (data.error) { addError(data.error); return; }
        addAssistant(data);
        // Refresh the thread list title on the first exchange.
        if (document.querySelectorAll('.msg.user').length === 1) {
          var active = document.querySelector('.thread.active');
          if (active && active.childNodes.length) active.childNodes[0].textContent = text.slice(0, 60);
        }
      })
      .catch(function () { removeThinking(); addError('The request failed. Check your connection and try again.'); })
      .finally(function () {
        input.disabled = false;
        form.querySelector('.send').disabled = false;
        input.focus();
      });
  }

  function autoGrow() {
    if (!input) return;
    input.style.height = 'auto';
    input.style.height = Math.min(input.scrollHeight, 200) + 'px';
  }

  if (form) {
    form.addEventListener('submit', function (e) {
      e.preventDefault();
      send(input.value);
    });
    input.addEventListener('input', autoGrow);
    // Enter sends; Shift+Enter inserts a newline.
    input.addEventListener('keydown', function (e) {
      if (e.key === 'Enter' && !e.shiftKey) {
        e.preventDefault();
        send(input.value);
      }
    });
  }

  // Suggestion cards on an empty conversation.
  document.querySelectorAll('.suggestion').forEach(function (card) {
    card.addEventListener('click', function () {
      var prompt = card.getAttribute('data-prompt');
      if (input) { send(prompt); }
    });
  });

  toBottom();
})();
