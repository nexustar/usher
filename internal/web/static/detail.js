// usher SPA: detail view + main chat + new session.

import {
  esc, root, subtitle, closeES, closeTerminalES, clearListInterval,
  currentDetailId, setCurrentDetailId, setCurrentDraftKey,
  setSuppressAppendScroll,
  isNearBottom, markViewing, setCurrentES,
  restoreDraft, clearDraft, growPrompt,
  registerRefreshSubtitle,
} from './state.js';
import {
  renderMarkdown, appendChatMessage, renderToolPart,
  forkBtnHTML, updateMessageTs,
  backendMark,
} from './render.js';
import { openTerminalScreen, wireTerminalControls, measureCols } from './terminal.js';
import { loadSidebar } from './sidebar.js';
import { loadList } from './list.js';

// --- detail-private state (not shared with other modules) ---

// Detail-view transcript sync. The live turn streams as server-grouped
// `part` SSE events (text and tool results alike, rendered into the live
// bubble as they happen). At turn end the bubble is promoted in place —
// its parts ARE the canonical server-rendered content, so no re-fetch is
// needed — unless the client knows it missed something (joined or
// reconnected mid-turn, steering prompt it didn't witness): then
// liveTurnDirty routes turn end through a full loadTranscript instead.
// Full fetches otherwise happen only on mount / reconnect-while-idle /
// load-earlier. Flags: detailStreaming (gate the reconnect re-fetch off a
// live turn), lastTranscriptSig (skip an unchanged rebuild), currentDetailId
// (ignore a re-fetch that resolves after the user navigated away).
let detailStreaming = false;
let lastTranscriptSig = '';

// Bumped on every showDetail entry. showDetail awaits (session fetch, transcript)
// before opening its /events stream; a newer mount started during those awaits
// bumps this, so the superseded run bails instead of opening — and orphaning — a
// second EventSource that would write into the live view.
let detailEpoch = 0;

// The committed transcript turns currently in the DOM, as {key, node} in order.
// Lets loadTranscript reconcile incrementally (append only what's new) instead
// of wiping and re-rendering all ~100 turns on every turn-end — the latter is
// O(n) per turn and visibly janks long, tool-heavy sessions. Tracking the nodes
// explicitly (rather than re-querying by position) keeps the diff correct even
// when untracked client-only bubbles — errors, optimistic placeholders — sit in
// the same list.
let renderedTurns = [];

// The in-flight assistant turn, shared between the SSE part handler and the
// turn-end finalizer (module level on purpose — a closure-held node was the
// old detached-DOM trap when a reconcile removed it). parts accumulates the
// streamed TurnParts so the promote path can compute the turn's key; ts is
// the first part's server timestamp (display fallback when the exit payload
// carries none).
let liveTurn = null; // { node, parts: [TurnPart], ts }

// Set when the live bubble is known to be incomplete — the client joined or
// reconnected mid-turn, or a prompt it didn't send slipped in (steering /
// another frontend). A dirty turn finalizes via full loadTranscript; a clean
// one is promoted in place with zero fetches.
let liveTurnDirty = false;

// Transcript window: render the most recent `transcriptLimit` turns; "load
// earlier" grows it by a page and re-fetches. transcriptTotal is the server's
// full turn count (X-Transcript-Total), used to show/hide the button.
const TRANSCRIPT_PAGE = 100;
let transcriptLimit = TRANSCRIPT_PAGE;
let transcriptTotal = 0;

// Attach a small command palette to a session composer. Command discovery is
// lazy because Claude only advertises its catalog after the live stream-json
// worker has emitted system/init; an empty response is retried on later `/`
// input rather than cached forever.
function wireCommandPreview(promptEl, sessionID) {
  const popup = promptEl.parentElement.querySelector('.command-preview');
  if (!popup) return;
  let commands = [];
  let source = '';
  let selected = 0;
  let loading = null;
  let lastAvailableLoad = 0;

  // Anchored with no leading slack: backends only honour a sigil in column 0,
  // so offering completion for " /compact" would promise a command the send
  // path then delivers as prose.
  const commandToken = () => {
    const match = promptEl.value.match(/^([/$][^\s]*)$/);
    return match ? match[1] : null;
  };
  // Matched case-sensitively for the same reason: both backends resolve names
  // exactly, so listing /Compact under "/C" would advertise a command that is
  // then sent as prose.
  const matches = () => {
    const token = commandToken();
    if (token == null) return [];
    if (token.startsWith('$')) {
      return commands.filter(command => source === 'codex' && command.kind === 'skill' &&
        ('$' + command.name).startsWith(token));
    }
    return commands.filter(command => ('/' + command.name).startsWith(token));
  };
  const displayName = (command) => commandToken()?.startsWith('$')
    ? '$' + command.name : '/' + command.name;
  const hide = () => {
    popup.hidden = true;
    popup.innerHTML = '';
    promptEl.removeAttribute('aria-activedescendant');
  };
  const render = () => {
    const found = matches();
    if (!found.length) { hide(); return; }
    selected = Math.min(selected, found.length - 1);
    popup.innerHTML = found.map((command, i) =>
      `<button type="button" role="option" id="command-option-${i}" class="command-option${i === selected ? ' selected' : ''}" aria-selected="${i === selected}">` +
        `<span class="command-option-main"><span>${esc(displayName(command))}</span>${command.description ? `<small>${esc(command.description)}</small>` : ''}</span>` +
        `<small>${esc(command.kind || '')}</small>` +
      `</button>`
    ).join('');
    popup.hidden = false;
    promptEl.setAttribute('aria-activedescendant', `command-option-${selected}`);
    popup.querySelector(`#command-option-${selected}`)?.scrollIntoView({ block: 'nearest' });
  };
  const load = async () => {
    if (loading || Date.now() - lastAvailableLoad < 30000) return loading;
    loading = fetch('/api/sessions/' + encodeURIComponent(sessionID) + '/commands')
      .then(res => res.ok ? res.json() : null)
      .then(data => {
        source = data && data.source || '';
        const available = !!(data && data.available);
        commands = available && Array.isArray(data.commands) ? data.commands : [];
        if (available) lastAvailableLoad = Date.now();
      })
      .catch(() => { commands = []; })
      .finally(() => { loading = null; render(); });
    return loading;
  };
  const complete = () => {
    const found = matches();
    const command = found[selected];
    if (!command) return false;
    const sigil = source === 'codex' && command.kind === 'skill' ? '$' : '/';
    // commandToken() matched the whole field, so the token is the whole value.
    promptEl.value = sigil + command.name + ' ';
    promptEl.dispatchEvent(new Event('input', { bubbles: true }));
    growPrompt(promptEl);
    hide();
    return true;
  };

  promptEl.setAttribute('aria-autocomplete', 'list');
  promptEl.setAttribute('aria-controls', 'command-preview');
  promptEl.addEventListener('input', () => {
    selected = 0;
    if (commandToken() == null) { hide(); return; }
    render();
    load();
  });
  promptEl.addEventListener('keydown', (e) => {
    if (popup.hidden || e.isComposing) return;
    const found = matches();
    if ((e.key === 'ArrowDown' || e.key === 'ArrowUp') && found.length) {
      e.preventDefault();
      selected = (selected + (e.key === 'ArrowDown' ? 1 : found.length - 1)) % found.length;
      render();
    } else if (e.key === 'Tab' || e.key === 'Enter') {
      if (complete()) e.preventDefault();
    } else if (e.key === 'Escape') {
      e.preventDefault();
      hide();
    }
  });
  popup.addEventListener('mousedown', (e) => {
    const option = e.target.closest('.command-option');
    if (!option) return;
    e.preventDefault(); // keep textarea focus
    selected = [...popup.children].indexOf(option);
    complete();
  });
  promptEl.addEventListener('blur', () => setTimeout(hide, 0));
}

// ---------- New session view ----------
//
// Mirrors the regular session detail layout so the page transition after
// creation is purely additive (empty placeholders fill in). The only
// pre-creation difference is the auto-approve toggle position — replaced
// with a cwd picker, since auto-approve can't be set without a session id.
// Submitting POSTs to /api/sessions (router.StartSession returns the new
// id immediately and streams to broker subscribers), then hash-routes to
// the freshly-created session's detail page.

export async function showNewSession(prefillCwd) {
  clearListInterval();
  setCurrentDetailId(null);
  setCurrentDraftKey(null); // not draft-managed; don't clobber a session draft
  closeES();
  subtitle.textContent = 'new session';

  let cwds = [];
  try {
    const res = await fetch('/api/sessions');
    if (res.ok) {
      const data = (await res.json()) || [];
      cwds = [...new Set(data.map(s => s.cwd).filter(Boolean))].sort();
    }
  } catch {/* offline → datalist just empty */}

  const options = cwds.map(c => `<option value="${esc(c)}"></option>`).join('');
  root.innerHTML = `
    <div id="chat-scroll" class="chat-area">
      <section class="send-anchor">
        <div class="send-controls">
          <label class="new-cwd-field">
            <span class="muted">cwd</span>
            <input id="new-cwd" type="text" list="new-cwd-list" autocomplete="off"
                   placeholder="/absolute/path/to/project">
            <datalist id="new-cwd-list">${options}</datalist>
          </label>
        </div>
        <div class="composer">
          <textarea id="prompt" rows="1" placeholder="message…"></textarea>
          <div class="composer-bar">
            <div class="composer-tools">
              <div class="composer-model-pickers">
                <select id="new-backend" class="composer-picker composer-backend" aria-label="backend"></select>
                <span class="picker-divider" aria-hidden="true"></span>
                <select id="new-model" class="composer-picker composer-model" aria-label="model"></select>
              </div>
            </div>
            <div class="composer-send"><button id="send">send</button></div>
          </div>
        </div>
        <div id="new-session-err" class="err" style="display:none; margin-top:0.5rem"></div>
      </section>
    </div>
  `;

  const promptEl = document.getElementById('prompt');
  const sendBtn = document.getElementById('send');
  const cwdEl = document.getElementById('new-cwd');
  const backendEl = document.getElementById('new-backend');
  const modelEl = document.getElementById('new-model');
  const errEl = document.getElementById('new-session-err');
  // Prefilled from a sidebar cwd "+": cwd is known, so drop the cursor in the
  // message box instead of the cwd field.
  if (prefillCwd) {
    cwdEl.value = prefillCwd;
    promptEl.focus();
  } else {
    cwdEl.focus();
  }

  let modelCatalogs = {};
  const renderModels = () => {
    const backend = backendEl.value;
    const models = modelCatalogs[backend] || [];
    const choices = models.length ? models : [{id: 'default', display_name: 'Default'}];
    modelEl.replaceChildren(...choices.map(m => {
      const o = document.createElement('option');
      o.value = m.id;
      o.textContent = m.display_name || m.id;
      return o;
    }));
    try {
      const key = 'usher.newModel.' + backend;
      const saved = localStorage.getItem(key);
      if (saved && [...modelEl.options].some(o => o.value === saved)) {
        modelEl.value = saved;
      }
    } catch {/* private mode → keep first model */}
    backendEl.parentElement.dataset.backend = backend || 'claude';
  };
  backendEl.addEventListener('change', () => {
    try { localStorage.setItem('usher.newBackend', backendEl.value); } catch {/* private mode */}
    renderModels();
  });
  modelEl.addEventListener('change', () => {
    try { localStorage.setItem('usher.newModel.' + backendEl.value, modelEl.value); } catch {/* private mode */}
  });

  // Build the backend picker once, then show only that backend's models.
  fetch('/api/models').then(r => r.ok ? r.json() : {}).then(data => {
    const backends = (data && data.backends) || ['claude'];
    modelCatalogs = (data && data.models) || {};
    backendEl.replaceChildren();
    for (const name of backends) {
      const o = document.createElement('option');
      o.value = name;
      o.textContent = name.charAt(0).toUpperCase() + name.slice(1);
      backendEl.appendChild(o);
    }
    try {
      const savedBackend = localStorage.getItem('usher.newBackend');
      if (savedBackend && backends.includes(savedBackend)) {
        backendEl.value = savedBackend;
      }
    } catch {/* private mode → keep first backend */}
    renderModels();
  }).catch(() => {
    modelCatalogs = {};
    const o = document.createElement('option');
    o.value = 'claude';
    o.textContent = 'Claude';
    backendEl.replaceChildren(o);
    renderModels();
  });

  const submit = async () => {
    if (sendBtn.disabled) return; // re-entrancy guard during in-flight submit
    errEl.style.display = 'none';
    errEl.textContent = '';
    sendBtn.disabled = true;
    sendBtn.textContent = 'creating…';
    try {
      const res = await fetch('/api/sessions', {
        method: 'POST',
        headers: {'Content-Type': 'application/json'},
        body: JSON.stringify({
          cwd: cwdEl.value.trim(),
          initial_message: promptEl.value,
          backend: backendEl.value,
          model: modelEl.value,
        }),
      });
      const body = await res.json().catch(() => ({}));
      if (!res.ok) throw new Error(body.error || ('HTTP ' + res.status));
      // Refresh sidebar so the new session shows up while its first stream
      // is still in flight; fsnotify normally surfaces it within ~1s but a
      // proactive reload avoids the awkward "where did it go?" gap.
      loadSidebar();
      location.hash = '#/s/' + encodeURIComponent(body.id);
    } catch (ex) {
      errEl.textContent = String(ex.message || ex);
      errEl.style.display = '';
      sendBtn.disabled = false;
      sendBtn.textContent = 'send';
    }
  };

  sendBtn.addEventListener('click', submit);
  promptEl.addEventListener('keydown', (e) => {
    if (e.key === 'Enter' && !e.shiftKey && !e.isComposing) {
      e.preventDefault();
      submit();
    }
  });
}

// ---------- Detail view ----------

export async function showDetail(id) {
  const epoch = ++detailEpoch;
  clearListInterval();
  closeES();
  // Fresh view: reset sync state so a prior session's signature/stream flag
  // can't suppress this one's first render.
  setCurrentDetailId(id);
  markViewing(id); // clear unread + exclude while open
  setCurrentDraftKey('s:' + id);
  lastTranscriptSig = '';
  renderedTurns = [];
  liveTurn = null;
  liveTurnDirty = false;
  transcriptLimit = TRANSCRIPT_PAGE;
  transcriptTotal = 0;
  detailStreaming = false;
  subtitle.textContent = 'session detail';

  let sess;
  try {
    const res = await fetch('/api/sessions/' + encodeURIComponent(id));
    if (!res.ok) {
      root.innerHTML = '<div class="err">session not found</div>';
      return;
    }
    sess = await res.json();
  } catch (e) {
    root.innerHTML = '<div class="err">' + esc(String(e)) + '</div>';
    return;
  }
  if (epoch !== detailEpoch) return; // a newer mount superseded us mid-fetch

  // Show title / cwd / short id in the page header subtitle so it stays
  // visible while transcript / response sections scroll. Mirrors how main
  // chat surfaces its focus block — the page header is the only sticky
  // band, no fragile second-tier sticky element.
  renderSessionSubtitle(sess);

  if (sess.is_subagent) {
    root.innerHTML = `<div id="chat-scroll" class="chat-area"></div>`;
    openSubagentEventStream(id);
    await loadTranscript(id);
    return;
  }

  root.innerHTML = `
    <div id="chat-scroll" class="chat-area">
      <section class="send-anchor">
        <div id="term-panel" class="term-panel" hidden>
          <div class="term-screen"><pre id="term-grid" class="term-grid muted">connecting…</pre></div>
          <div class="term-input-row">
            <span class="term-prompt" aria-hidden="true">$</span>
            <textarea id="term-input" rows="1" placeholder="command…" aria-label="terminal command"></textarea>
            <button id="term-send" type="button">run</button>
          </div>
          <div class="term-keys" id="term-keys">
            <button type="button" id="term-ctrl-toggle" aria-expanded="false" title="show ctrl-key row">ctl</button>
            <button type="button" data-control="escape">esc</button>
            <button type="button" data-control="tab">tab</button>
            <button type="button" data-control="up" aria-label="up">↑</button>
            <button type="button" data-control="down" aria-label="down">↓</button>
            <button type="button" data-control="left" aria-label="left">←</button>
            <button type="button" data-control="right" aria-label="right">→</button>
            <button type="button" data-control="enter">⏎</button>
          </div>
          <div class="term-ctrl-menu" id="term-ctrl-menu" hidden>
            <button type="button" data-control="ctrl-c">^C</button>
            <button type="button" data-control="ctrl-z">^Z</button>
            <button type="button" data-control="ctrl-d">^D</button>
            <button type="button" data-control="ctrl-x">^X</button>
            <button type="button" data-control="ctrl-o">^O</button>
            <button type="button" data-control="ctrl-w">^W</button>
            <button type="button" data-control="ctrl-k">^K</button>
            <button type="button" data-control="ctrl-u">^U</button>
          </div>
        </div>
        <div class="composer">
          <div id="command-preview" class="command-preview" role="listbox" hidden></div>
          <textarea id="prompt" rows="1" placeholder="message…"></textarea>
          <div class="composer-bar">
            <div class="composer-tools">
              <button id="upload-btn" class="upload-btn" type="button" title="upload file">
                <span class="t-icon">+</span><span class="t-full">upload</span>
              </button>
              <input id="upload-input" type="file" hidden>
              <button id="auto-approve-toggle" class="auto-approve-toggle" type="button"
                aria-pressed="${sess.auto_approve ? 'true' : 'false'}"
                title="ask: confirm each tool call · auto: run them automatically">
                <span class="t-icon">ϟ</span><span class="t-full">approve:</span><span class="toggle-val">${sess.auto_approve ? 'auto' : 'ask'}</span>
              </button>
              <button id="term-toggle" class="term-toggle" type="button" aria-pressed="${sess.terminal_open ? 'mixed' : 'false'}"
                ${sess.terminal_available ? '' : 'disabled'}
                title="${sess.terminal_available ? 'show or hide this session’s shell — hiding keeps it running; type exit in the shell to end it' : 'terminal unavailable: tmux is not installed'}">
                <span class="t-icon">&gt;_</span><span class="t-full">terminal:</span><span class="toggle-val">${sess.terminal_open ? 'bg' : 'off'}</span>
              </button>
            </div>
            <div class="composer-send">
              <div class="session-usage-wrap">
                <button id="session-usage" class="session-usage" type="button" hidden
                  aria-expanded="false" aria-controls="session-usage-detail"></button>
                <div id="session-usage-detail" class="session-usage-detail" hidden></div>
              </div>
              <button id="send">send</button>
            </div>
          </div>
        </div>
      </section>
    </div>
  `;

  await loadTranscript(id);
  if (epoch !== detailEpoch) return; // superseded before we wired the streams

  const chatEl = document.getElementById('chat-scroll');
  const promptEl = document.getElementById('prompt');
  const sendBtn = document.getElementById('send');
  const usageBtn = document.getElementById('session-usage');
  const usageDetail = document.getElementById('session-usage-detail');
  if (usageBtn && usageDetail) {
    usageBtn.addEventListener('click', () => {
      usageDetail.hidden = !usageDetail.hidden;
      usageBtn.setAttribute('aria-expanded', usageDetail.hidden ? 'false' : 'true');
    });
    usageBtn.addEventListener('blur', () => {
      usageDetail.hidden = true;
      usageBtn.setAttribute('aria-expanded', 'false');
    });
  }
  renderSessionRuntime(sess.runtime);
  restoreDraft(promptEl);
  wireCommandPreview(promptEl, id);

  const autoBtn = document.getElementById('auto-approve-toggle');
  if (autoBtn) {
    autoBtn.addEventListener('click', async () => {
      const next = autoBtn.getAttribute('aria-pressed') !== 'true';
      autoBtn.disabled = true;
      try {
        const res = await fetch('/api/sessions/' + encodeURIComponent(id) + '/auto-approve', {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ enabled: next }),
        });
        if (!res.ok) throw new Error('HTTP ' + res.status);
        autoBtn.setAttribute('aria-pressed', next ? 'true' : 'false');
        autoBtn.querySelector('.toggle-val').textContent = next ? 'auto' : 'ask';
        loadSidebar(); // refresh sidebar marker immediately
      } catch (e) {
        appendChatMessage({ role: 'error', content: 'auto-approve toggle failed: ' + String(e), ts: new Date().toISOString() });
      } finally {
        autoBtn.disabled = false;
      }
    });
  }

  // Terminal states: off, on, or bg (shell alive with the panel hidden).
  const termToggle = document.getElementById('term-toggle');
  const termPanel = document.getElementById('term-panel');
  const termInput = document.getElementById('term-input');
  const termSend = document.getElementById('term-send');
  let shellAlive = !!sess.terminal_open;
  let panelShown = false;
  let termStream = null;
  let terminalControlsWired = false;
  const TERM_ROW_PX = 16.25; // .term-grid 13px × line-height 1.25 (keep in sync with CSS)
  // The screen takes a little under half the chat viewport; the command and
  // key rows bring the complete terminal panel to roughly half-height.
  const onRows = () => Math.max(4, Math.floor(chatEl.clientHeight * 0.38 / TERM_ROW_PX));
  const paintTermToggle = () => {
    if (!termToggle) return;
    const state = panelShown ? 'on' : (shellAlive ? 'bg' : 'off');
    termToggle.setAttribute('aria-pressed', state === 'on' ? 'true' : (state === 'bg' ? 'mixed' : 'false'));
    termToggle.querySelector('.toggle-val').textContent = state;
  };
  const renderTerminal = () => {
    paintTermToggle();
    if (!panelShown) {
      if (termPanel) termPanel.hidden = true;
      termStream = null;
      closeTerminalES();
      return;
    }
    const rows = onRows();
    if (termPanel) termPanel.hidden = false;
    if (!terminalControlsWired) {
      wireTerminalControls(id);
      // Close the ctrl-key menu after a selection or outside click.
      const ctrlToggle = document.getElementById('term-ctrl-toggle');
      const ctrlMenu = document.getElementById('term-ctrl-menu');
      if (ctrlToggle && ctrlMenu) {
        const closeMenu = () => {
          ctrlMenu.hidden = true;
          ctrlToggle.setAttribute('aria-expanded', 'false');
          document.removeEventListener('click', onDocClick);
        };
        const onDocClick = (e) => {
          if (!ctrlMenu.contains(e.target) && !ctrlToggle.contains(e.target)) closeMenu();
        };
        ctrlToggle.addEventListener('click', () => {
          if (!ctrlMenu.hidden) { closeMenu(); return; }
          ctrlMenu.hidden = false;
          ctrlToggle.setAttribute('aria-expanded', 'true');
          document.addEventListener('click', onDocClick);
        });
        ctrlMenu.addEventListener('click', (e) => {
          if (e.target.closest('button[data-control]')) closeMenu();
        });
      }
      terminalControlsWired = true;
    }
    const gridEl = document.getElementById('term-grid');
    if (gridEl) {
      const box = gridEl.parentElement;
      // A few pixels of slack avoid a rounding scrollbar.
      box.style.height = Math.ceil(rows * TERM_ROW_PX) + 4 + 'px';
      const stream = openTerminalScreen(id, gridEl, measureCols(box), rows, () => {
        // Leave the closed message visible until the panel is collapsed.
        if (termStream !== stream) return;
        shellAlive = false;
        termStream = null;
        paintTermToggle();
        closeTerminalES();
      });
      termStream = stream;
    }
    if (isNearBottom(chatEl)) chatEl.scrollTop = chatEl.scrollHeight;
  };
  if (termToggle && termPanel) {
    termToggle.addEventListener('click', async () => {
      if (panelShown) {
        // Hide only — the shell keeps running (label flips to bg).
        panelShown = false;
        renderTerminal();
        return;
      }
      termToggle.disabled = true;
      try {
        // Reveal before measuring its width.
        termPanel.hidden = false;
        const box = document.getElementById('term-grid').parentElement;
        const res = await fetch('/api/sessions/' + encodeURIComponent(id) + '/terminal', {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ cols: measureCols(box), rows: onRows() }),
        });
        if (!res.ok) {
          const body = await res.json().catch(() => ({}));
          throw new Error(body.error || ('HTTP ' + res.status));
        }
        shellAlive = true;
        panelShown = true;
      } catch (e) {
        appendChatMessage({ role: 'error', content: 'terminal: ' + String(e.message || e), ts: new Date().toISOString() });
      } finally {
        termToggle.disabled = !sess.terminal_available;
        renderTerminal();
      }
    });
  }
  if (termInput && termSend) {
    let terminalRequestID = null;
    const submitTerminal = async () => {
      const text = termInput.value;
      if (!text || termSend.disabled || !shellAlive) return;
      termSend.disabled = true;
      termInput.disabled = true;
      try {
        terminalRequestID = terminalRequestID || (globalThis.crypto && globalThis.crypto.randomUUID
          ? globalThis.crypto.randomUUID()
          : Date.now() + '-' + Math.random().toString(16).slice(2));
        const res = await fetch('/api/sessions/' + encodeURIComponent(id) + '/terminal/input', {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({
            text,
            request_id: terminalRequestID,
          }),
        });
        if (!res.ok) {
          const body = await res.json().catch(() => ({}));
          throw new Error(body.error || ('HTTP ' + res.status));
        }
        termInput.value = '';
        terminalRequestID = null;
      } catch (e) {
        appendChatMessage({ role: 'error', content: 'terminal input: ' + String(e.message || e), ts: new Date().toISOString() });
      } finally {
        termSend.disabled = false;
        termInput.disabled = false;
        termInput.focus();
      }
    };
    termSend.addEventListener('click', submitTerminal);
    termInput.addEventListener('input', () => { terminalRequestID = null; });
    termInput.addEventListener('keydown', (e) => {
      if (e.key === 'Enter' && !e.shiftKey && !e.isComposing) {
        e.preventDefault();
        submitTerminal();
      }
    });
  }

  const uploadBtn = document.getElementById('upload-btn');
  const uploadInput = document.getElementById('upload-input');
  if (uploadBtn && uploadInput) {
    uploadBtn.addEventListener('click', () => uploadInput.click());
    uploadInput.addEventListener('change', async () => {
      const file = uploadInput.files[0];
      if (!file) return;
      uploadBtn.disabled = true;
      const form = new FormData();
      form.append('file', file);
      try {
        const res = await fetch('/api/sessions/' + encodeURIComponent(id) + '/upload', {
          method: 'POST', body: form,
        });
        if (!res.ok) {
          const err = await res.json().catch(() => ({}));
          appendChatMessage({ role: 'error', content: 'upload failed: ' + (err.error || 'HTTP ' + res.status), ts: new Date().toISOString() });
          return;
        }
        const { path } = await res.json();
        const prefix = promptEl.value && !promptEl.value.endsWith('\n') ? '\n' : '';
        promptEl.value += prefix + '[file: ' + path + '] ';
        promptEl.focus();
        growPrompt(promptEl);
        appendChatMessage({ role: 'system', content: 'uploaded ' + file.name, ts: new Date().toISOString() });
      } catch (e) {
        appendChatMessage({ role: 'error', content: 'upload failed: ' + String(e), ts: new Date().toISOString() });
      } finally {
        uploadBtn.disabled = false;
        uploadInput.value = '';
      }
    });
  }

  // Keep one send/cancel button stable while its request is in flight.
  let actionPending = null; // null | send | cancel
  let sendAccepted = false;
  let sendLifecycleObserved = false;
  const renderAction = () => {
    const action = actionPending || (detailStreaming ? 'cancel' : 'send');
    sendBtn.textContent = action;
    sendBtn.classList.toggle('cancel', action === 'cancel');
    sendBtn.disabled = actionPending !== null;
  };

  const observeSendLifecycle = () => {
    if (actionPending === 'send') {
      sendLifecycleObserved = true;
      if (sendAccepted) actionPending = null;
    }
    renderAction();
  };
  openEventStream(id, chatEl, renderAction, observeSendLifecycle);

  const submit = async () => {
    const text = promptEl.value;
    if (!text.trim() || detailStreaming || actionPending) return;
    actionPending = 'send';
    sendAccepted = false;
    sendLifecycleObserved = false;
    renderAction();
    promptEl.value = '';
    clearDraft();
    growPrompt(promptEl); // shrink back; programmatic clear fires no input event
    // Optimistic: show the user message immediately. The live bubble is
    // created by openEventStream on subprocess.started. Marked .optimistic so
    // the turn.user event (or a truth-up fetch) replaces it with the
    // canonical turn — same text, server timestamp.
    const el = document.getElementById('chat-scroll');
    if (el) el.querySelectorAll(':scope > .chat-loading').forEach(n => n.remove());
    const userNode = appendChatMessage({ role: 'user', content: text });
    if (userNode) userNode.classList.add('optimistic');
    // Codex slash input may become an RPC operation or normalize /skill to
    // $skill. Pi's local metadata/compaction commands also create no canonical
    // user turn. Reconcile at turn end so optimistic command text disappears
    // and newly persisted system records become visible.
    const piLocalCommand = sess.backend === 'pi' &&
      /^\/(?:name|compact)(?:\s|$)/.test(text);
    if ((sess.backend === 'codex' && text.startsWith('/')) || piLocalCommand) {
      liveTurnDirty = true;
    }
    try {
      const res = await fetch('/api/sessions/' + encodeURIComponent(id) + '/send', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ text }),
      });
      if (!res.ok) {
        const err = await res.json().catch(() => ({}));
        appendChatMessage({ role: 'error', content: 'send failed: ' + (err.error || ('HTTP ' + res.status)), ts: new Date().toISOString() });
        actionPending = null;
        renderAction();
        return;
      }
      sendAccepted = true;
      // Wait for the running lifecycle event; it may precede this response.
      if (sendLifecycleObserved) {
        actionPending = null;
        renderAction();
      }
    } catch (e) {
      appendChatMessage({ role: 'error', content: String(e), ts: new Date().toISOString() });
      actionPending = null;
      renderAction(); // an SSE start may already have made this cancel
    }
  };

  const cancel = async () => {
    if (!detailStreaming || actionPending) return;
    actionPending = 'cancel';
    renderAction();
    try {
      const res = await fetch('/api/sessions/' + encodeURIComponent(id) + '/send', { method: 'DELETE' });
      if (!res.ok) throw new Error('HTTP ' + res.status);
    } catch (e) {
      appendChatMessage({ role: 'error', content: 'cancel failed: ' + String(e), ts: new Date().toISOString() });
    } finally {
      actionPending = null;
      renderAction();
    }
  };

  sendBtn.addEventListener('click', () => detailStreaming ? cancel() : submit());
  promptEl.addEventListener('keydown', (e) => {
    if (e.defaultPrevented) return;
    if (e.key === 'Enter' && !e.shiftKey && !e.isComposing) {
      e.preventDefault();
      submit();
    }
  });

  promptEl.focus();
}

// Subagents are read-only and intentionally do not stream partial output.
// Their lightweight server-side watcher emits subprocess.exit when a child
// turn completes; refetch the transcript then. turn.idle is the server's
// snapshot-on-connect event, so it also closes the subscribe/fetch race and
// reconciles completions that happened while the SSE connection was down.
function openSubagentEventStream(id) {
  const es = new EventSource('/api/sessions/' + encodeURIComponent(id) + '/events');
  setCurrentES(es);
  es.addEventListener('turn.idle', () => loadTranscript(id));
  es.addEventListener('subprocess.exit', () => loadTranscript(id));
  es.onerror = () => {/* SSE auto-reconnects; no user-visible noise */};
}

// openEventStream attaches SSE handlers to /api/sessions/{id}/events. The
// in-flight assistant turn renders inline at the bottom of the transcript:
// subprocess.started stands up the live bubble; each 'part' event (one
// server-grouped TurnPart — assistant text or a rendered tool result, the
// same shapes /transcript serves) appends into it as it happens; 'turn.user'
// adopts our optimistic echo as the canonical user turn; subprocess.exit
// promotes the live bubble in place (full fetch only when the turn is
// dirty — see finalizeTurn). Turn errors surface via the 'error' event.
function openEventStream(id, chatEl, renderAction, observeSendLifecycle) {
  const es = new EventSource('/api/sessions/' + encodeURIComponent(id) + '/events');
  setCurrentES(es);

  let opened = false;
  // On reconnect (not the first connect) re-fetch to fill any gap. Mid-turn
  // the live stream owns the bubble, so just mark the turn dirty — events
  // during the outage are gone (the broker has no replay) and the turn-end
  // full fetch will reconcile.
  es.onopen = () => {
    if (opened) {
      if (detailStreaming) liveTurnDirty = true;
      else loadTranscript(id);
    }
    opened = true;
  };

  const setLoadEarlierDisabled = (v) => {
    const b = document.querySelector('#chat-scroll > .load-earlier');
    if (b) b.disabled = v;
  };
  const onIdle = () => {
    detailStreaming = false;
    observeSendLifecycle();
    setLoadEarlierDisabled(false);
  };
  const onRunning = () => {
    detailStreaming = true;
    // Confirm a successful send through the turn lifecycle.
    observeSendLifecycle();
    setLoadEarlierDisabled(true);
  };
  // beginTurn stands up the live bubble + running-state UI for
  // a turn. It's the single idempotent entry point for every way a turn
  // surfaces: subprocess.started (live), turn.active (server snapshot on a
  // mid-turn connect), and the lazy first-part fallback. The guard makes it
  // safe when two of those race (e.g. connecting in the window between the
  // session flipping to running and subprocess.started being published).
  const beginTurn = () => {
    if (liveTurn) return; // already tracking a turn
    const lt = ensureLiveTurn();
    if (lt) showLiveWorking(lt);
    onRunning();
  };

  const setRoleText = (el, text) => {
    const roleEl = el && el.querySelector('.role');
    if (roleEl && roleEl.firstChild) roleEl.firstChild.textContent = text;
  };

  const handlers = {
    // Sent by the server on connect when the session is already mid-turn (the
    // started event predates this subscribe). beginTurn is idempotent, so if a
    // real subprocess.started also lands (connect raced the turn starting) it
    // won't double the bubble. Parts that flowed before this subscribe are
    // gone (no broker replay) — mark the turn dirty so its end reconciles via
    // a full fetch. adoptLastTurnAsLive continues that same bubble instead of
    // starting a second one.
    'turn.active': () => {
      if (!liveTurn && adoptLastTurnAsLive()) {
        showLiveWorking(liveTurn);
        onRunning();
      }
      else beginTurn();
      liveTurnDirty = true;
    },
    // Counterpart to turn.active: the server says no turn is running. If we still
    // think one is — our subprocess.exit was dropped on a broken connection and
    // the turn ended before we reconnected — finalize now, else send stays
    // disabled and the preview streams on forever. No-op on a normal idle connect
    // (detailStreaming already false).
    'turn.idle': () => {
      if (!detailStreaming) return;
      clearLiveWorking(liveTurn);
      liveTurn = null;
      liveTurnDirty = false; // the full fetch below reconciles everything
      onIdle();
      loadTranscript(id);
      refreshSubtitle(id); // recover metadata when subprocess.exit was missed
    },
    'subprocess.started': () => beginTurn(),
    // One server-grouped TurnPart (assistant text or a rendered tool result)
    // appended to the in-progress turn. subprocess.started / turn.active
    // normally create the bubble, but if both were missed stand it up now —
    // via beginTurn so running-state UI is wired too, not just a bubble.
    'part': (d) => {
      if (!liveTurn) beginTurn();
      appendLivePart(d);
    },
    'part.delta': (d) => {
      if (!liveTurn) beginTurn();
      appendLiveDelta(d);
    },
    'turn.status': () => {
      if (!liveTurn) beginTurn();
    },
    // The canonical user prompt hit the jsonl. Adopt our optimistic echo
    // (stamp the persisted ts, commit it). No echo means the prompt came
    // from another frontend: render it immediately, ahead of the live
    // assistant bubble that subprocess.started may already have created.
    // Keep the turn dirty as well: for mid-turn steering the prompt closed
    // assistant output this client may not have witnessed, so the turn-end
    // fetch still reconciles the complete canonical order.
    'turn.user': (d) => {
      if (!d || !d.content) return;
      const el = document.getElementById('chat-scroll');
      if (!el) return;
      const t = { role: 'user', content: d.content, ts: d.ts };
      const echo = [...el.querySelectorAll(':scope > .chat-message.user.optimistic')]
        .reverse()
        .find(n => {
          const c = n.querySelector('.content');
          return c && c.dataset.raw === d.content;
        });
      if (echo) {
        updateMessageTs(echo, d.ts);
        echo.classList.remove('optimistic');
        renderedTurns.push({ key: turnKey(t), node: echo });
        transcriptTotal++;
        return;
      }
      const node = appendChatMessage(t);
      if (node) {
        if (liveTurn && liveTurn.node && liveTurn.node.parentNode === el) {
          el.insertBefore(node, liveTurn.node);
        }
        renderedTurns.push({ key: turnKey(t), node });
        transcriptTotal++;
      }
      liveTurnDirty = true;
    },
    'session.runtime': (d) => renderSessionRuntime(d),
    'subprocess.exit': (d) => {
      // Failed/unconfirmed exits follow an explicit error event. Keep that
      // error bubble visible instead of reconciling it away as a successful turn.
      if (d && d.reason && d.reason !== 'local_command') {
        onIdle();
        return;
      }
      // Promote the live bubble in place (its parts are already the
      // canonical server-rendered content), or — when this client missed
      // part of the turn — reconcile via a full fetch. See finalizeTurn.
      finalizeTurn(id, d);
      onIdle();
      // A just-created session opened as "(untitled)"; by now the server has
      // its real title/cwd, so refresh the header too.
      refreshSubtitle(id);
    },
    'error': (d) => {
      const msg = d.message || JSON.stringify(d);
      if (liveTurn) {
        clearLiveWorking(liveTurn);
        updateMessageTs(liveTurn.node, new Date().toISOString());
        // Keep .optimistic: an error isn't a canonical transcript turn, so a
        // full fetch should clear it. Streamed parts stay visible above the
        // error text until then (they're canonical in the jsonl anyway).
        liveTurn.node.className = 'chat-message error optimistic';
        setRoleText(liveTurn.node, 'error');
        const div = document.createElement('div');
        div.className = 'content';
        div.dataset.raw = msg;
        div.innerHTML = renderMarkdown(msg);
        liveTurn.node.appendChild(div);
        liveTurn = null;
      } else {
        const n = appendChatMessage({ role: 'error', content: msg, ts: new Date().toISOString() });
        if (n) n.classList.add('optimistic');
      }
      // The promote path never removes stale optimistic nodes; route the next
      // turn end through a full fetch so this error bubble gets cleaned up.
      liveTurnDirty = true;
      onIdle();
    },
  };

  Object.entries(handlers).forEach(([name, fn]) => {
    es.addEventListener(name, (ev) => {
      // addEventListener('error', …) also catches EventSource's native
      // connection-error event (a plain Event on every disconnect/reconnect),
      // which has no .data. Server-sent events always carry data:, so a
      // missing payload means a native error — ignore it (es.onerror handles
      // reconnect) instead of rendering a spurious "{}" error bubble.
      if (ev.data == null) return;
      let data;
      try { data = JSON.parse(ev.data); } catch { data = { raw: ev.data }; }
      try { fn(data); } catch (e) { console.error('handler error', name, e); }
    });
  });

  es.onerror = () => {/* SSE auto-reconnects; no user-visible noise */};
}

// ---------- Main chat view ----------

export async function showMainChat(id) {
  clearListInterval();
  setCurrentDetailId(null);
  setCurrentDraftKey('c:' + id);
  closeES();
  subtitle.innerHTML = `<span class="subtitle-left"><strong class="session-title">Router</strong></span>`;

  root.innerHTML = `
    <div id="chat-scroll" class="chat-area">
      <section class="send-anchor">
        <div class="composer">
          <textarea id="prompt" rows="1" placeholder="message… (try /help)"></textarea>
          <div class="composer-bar">
            <div class="composer-send"><button id="send">send</button></div>
          </div>
        </div>
      </section>
    </div>
  `;

  const promptEl = document.getElementById('prompt');
  const sendBtn = document.getElementById('send');
  restoreDraft(promptEl);

  // The "thinking…" placeholder lives until this turn's `turn.done` (or an
  // error); one placeholder even across queued sends.
  let placeholder = null;
  const clearPlaceholder = () => { if (placeholder) { placeholder.remove(); placeholder = null; } };

  // Delivery contract: no replay, but the server registers the subscription
  // before the SSE headers flush. So: open the stream FIRST, refetch history
  // on EVERY open — anything after the fetch snapshot is guaranteed to
  // arrive on the stream. Messages streaming in mid-refetch are queued and
  // re-applied after it renders; chatMsgKeys dedups the overlap.
  let chatLoading = false;
  const pendingWhileLoading = [];

  const applyChatMessage = (data) => {
    const m = data.message || {};
    if (chatSeenKey(m)) return; // already rendered (refetch overlap)
    if (m.role === 'user') {
      // Adopt our optimistic echo (canonical ts, drop the optimistic mark);
      // no matching echo means another client sent it — render it.
      const list = document.getElementById('chat-scroll');
      const echo = list && [...list.querySelectorAll(':scope > .chat-message.user.optimistic')]
        .find(n => { const c = n.querySelector('.content'); return c && c.dataset.raw === m.content; });
      if (echo) {
        updateMessageTs(echo, m.ts);
        echo.classList.remove('optimistic');
        markChatSeen(m);
      } else {
        appendChatMessage(m);
        markChatSeen(m);
      }
    } else {
      clearPlaceholder();
      appendChatMessage(m);
      markChatSeen(m);
    }
    if (data.focus) renderFocus(data.focus);
  };

  const refetchChat = async () => {
    chatLoading = true;
    try {
      await loadMainChatInfo(id);
      await loadChatMessages(id);
    } finally {
      chatLoading = false;
      // Re-apply anything that streamed in mid-fetch; chatSeenKey skips what
      // the fetched snapshot already contained.
      while (pendingWhileLoading.length) applyChatMessage(pendingWhileLoading.shift());
    }
  };

  const es = new EventSource('/api/mainchats/' + encodeURIComponent(id) + '/events');
  setCurrentES(es);
  es.onopen = () => { refetchChat(); };
  es.onerror = () => {/* auto-reconnect; onopen refetches */};
  es.addEventListener('message', (ev) => {
    if (ev.data == null) return;
    let data;
    try { data = JSON.parse(ev.data); } catch { return; }
    if (chatLoading) { pendingWhileLoading.push(data); return; }
    applyChatMessage(data);
  });
  es.addEventListener('turn.done', () => { clearPlaceholder(); });

  const submit = async () => {
    const text = promptEl.value;
    if (!text.trim() || sendBtn.disabled) return;
    sendBtn.disabled = true;
    promptEl.value = '';
    clearDraft();
    growPrompt(promptEl); // shrink back; programmatic clear fires no input event
    const userNode = appendChatMessage({ role: 'user', content: text });
    if (userNode) userNode.classList.add('optimistic');
    if (!placeholder) placeholder = appendChatMessage({ role: 'agent', content: 'thinking…', _placeholder: true });
    try {
      const res = await fetch('/api/mainchats/' + encodeURIComponent(id) + '/send', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ text }),
      });
      if (!res.ok) {
        const err = await res.json().catch(() => ({}));
        clearPlaceholder();
        // Rejected (429 queue full / 500 persist failure): the message was
        // NOT accepted — drop the echo and restore the draft for a retry.
        if (userNode) userNode.remove();
        promptEl.value = text;
        growPrompt(promptEl);
        appendChatMessage({ role: 'error', content: err.error || 'send failed', ts: new Date().toISOString() });
      }
      // 202 accepted: everything else arrives over SSE.
    } catch (e) {
      clearPlaceholder();
      if (userNode) userNode.remove();
      promptEl.value = text;
      growPrompt(promptEl);
      appendChatMessage({ role: 'error', content: String(e), ts: new Date().toISOString() });
    } finally {
      sendBtn.disabled = false;
      promptEl.focus();
    }
  };

  sendBtn.addEventListener('click', submit);
  promptEl.addEventListener('keydown', (e) => {
    if (e.key === 'Enter' && !e.shiftKey && !e.isComposing) {
      e.preventDefault();
      submit();
    }
  });
  promptEl.focus();
}

function renderSessionSubtitle(sess) {
  // The title is the session-actions menu trigger ("title as menu"): the
  // popover + handlers are the sidebar kebab's, reading everything off the
  // button's datasets. Both showDetail and refreshSubtitle come through
  // here, so the datasets track every action applied to the on-screen
  // session. The chevron sits OUTSIDE the truncating title span so a long
  // title can't ellipsize it away.
  if (sess.is_subagent) {
    subtitle.innerHTML =
      backendMark(sess.backend) +
      `<span class="subtitle-left">` +
        `<strong class="session-title">${esc(sess.title || sess.agent_name || '(subagent)')}</strong>` +
        `<span class="session-id">${esc(sess.id.slice(0, 8))}</span>` +
        `<span class="session-cwd">${esc(sess.cwd || '')}</span>` +
      `</span>`;
    return;
  }
  subtitle.innerHTML =
    backendMark(sess.backend) +
    `<span class="subtitle-left">` +
      `<button type="button" class="subtitle-menu"` +
        ` data-id="${esc(sess.id)}" data-archived="${sess.archived ? '1' : '0'}"` +
        ` data-pinned="${sess.pinned ? '1' : '0'}" data-status="${esc(sess.status || '')}"` +
        ` data-cwd="${esc(sess.cwd || '')}" aria-label="session actions" aria-haspopup="menu">` +
        `<strong class="session-title">${esc(sess.title || '(untitled)')}</strong>` +
        `<svg class="subtitle-chevron" viewBox="0 0 10 6" width="10" height="6" fill="none" stroke="currentColor" stroke-width="1.5" aria-hidden="true"><path d="M1 1l4 4 4-4"/></svg>` +
      `</button>` +
      `<span class="session-id">${esc(sess.id.slice(0, 8))}</span>` +
      `<span class="session-cwd">${esc(sess.cwd || '')}</span>` +
    `</span>`;
}

// refreshSubtitle re-reads the session so the header picks up a title/cwd that
// filled in after the view opened (a brand-new session opens as "(untitled)";
// the server has the real values once the first turn lands). Guarded by
// currentDetailId so a late fetch can't write into a view already left.
async function refreshSubtitle(id) {
  try {
    const res = await fetch('/api/sessions/' + encodeURIComponent(id));
    if (!res.ok) return;
    const sess = await res.json();
    if (id !== currentDetailId) return;
    renderSessionSubtitle(sess);
    renderSessionRuntime(sess.runtime);
  } catch {/* ignore */}
}
registerRefreshSubtitle(refreshSubtitle);

// ----- live turn (streamed parts) -----

// ensureLiveTurn stands up the optimistic assistant bubble the streamed parts
// render into. Parts-shaped from birth (empty parts array → role header only)
// so it matches the structure of a committed turn.
function ensureLiveTurn() {
  if (liveTurn) return liveTurn;
  // Also clear the empty-state placeholder: it only self-removes on this
  // client's own send or a full reconcile, not when another frontend starts
  // the turn.
  const list = document.getElementById('chat-scroll');
  if (list) list.querySelectorAll(':scope > .chat-loading').forEach(n => n.remove());
  const node = appendChatMessage({ role: 'assistant', parts: [] });
  if (!node) return null;
  // Until the first visible part arrives, show only the turn-level Working row
  // instead of an empty assistant header above it.
  node.classList.add('optimistic', 'awaiting-output');
  liveTurn = { node, parts: [], partNodes: [], ts: '', preview: null, previewText: '', previewRAF: 0, acceptDeltas: true, working: null };
  return liveTurn;
}

// adoptLastTurnAsLive continues the trailing bubble loadTranscript rendered
// on a mid-turn reconnect, instead of beginTurn starting a second one below
// it. Safe because ReadTurns always flushes the in-progress turn, so the
// last rendered turn IS the one turn.active refers to.
function adoptLastTurnAsLive() {
  if (liveTurn || !renderedTurns.length) return false;
  const last = renderedTurns[renderedTurns.length - 1];
  if (!last.node.classList.contains('assistant')) return false;
  const forkBtn = last.node.querySelector('.turn-fork');
  if (forkBtn) forkBtn.remove(); // stale fork point; finalize re-adds a current one
  const partNodes = [...last.node.children].filter(c => !c.classList.contains('role'));
  renderedTurns.pop();
  last.node.classList.add('optimistic');
  liveTurn = {
    node: last.node,
    parts: last.parts ? last.parts.slice() : [],
    partNodes,
    ts: last.ts || '',
    preview: null, previewText: '', previewRAF: 0, acceptDeltas: true, working: null,
  };
  return true;
}

// Working is one turn-level status row outside the canonical assistant node.
// As a sibling it stays below every streamed part without joining transcript
// content or requiring part handlers to move it.
function showLiveWorking(lt) {
  if (!lt) return;
  if (!lt.working) {
    lt.working = document.createElement('div');
    lt.working.className = 'live-working';
    lt.working.textContent = 'Working…';
  }
  lt.node.insertAdjacentElement('afterend', lt.working);
}

function clearLiveWorking(lt) {
  if (!lt || !lt.working) return;
  lt.working.remove();
  lt.working = null;
}

// appendLiveDelta grows one ephemeral text block from protocol deltas. It is
// deliberately not added to lt.parts: the complete JSONL-derived part remains
// canonical and replaces this preview when it arrives.
function appendLiveDelta(d) {
  if (!d || !d.delta) return;
  const lt = ensureLiveTurn();
  if (!lt) return;
  if (!lt.acceptDeltas) return;
  lt.node.classList.remove('awaiting-output');
  if (!lt.preview) {
    lt.preview = document.createElement('div');
    lt.preview.className = 'content stream-preview';
    lt.node.appendChild(lt.preview);
  }
  lt.previewText += d.delta;
  if (lt.previewRAF) return;
  lt.previewRAF = requestAnimationFrame(() => {
    lt.previewRAF = 0;
    if (!lt.preview) return;
    lt.preview.dataset.raw = lt.previewText;
    lt.preview.innerHTML = renderMarkdown(lt.previewText);
    const chat = document.getElementById('chat-scroll');
    if (chat && isNearBottom(chat)) chat.scrollTop = chat.scrollHeight;
  });
}

// appendLivePart renders one streamed "part" SSE event into the live bubble,
// using the same renderers committed turns use (renderToolPart / markdown) —
// the server grouped and rendered the content, so live and canonical views
// can't diverge. The part is also accumulated on liveTurn for the turn-end
// promote (finalizeTurn).
function appendLivePart(d) {
  const p = d && d.part;
  if (!p) return;
  const lt = ensureLiveTurn();
  if (!lt) return;
  lt.node.classList.remove('awaiting-output');
  // Pi emits pending tool cards before results; enrich the matching card even
  // when parallel calls finish out of order instead of rendering a duplicate.
  let enrichIndex = -1;
  if (p.type === 'tool' && p.content) {
    for (let i = lt.parts.length - 1; i >= 0; i--) {
      const prev = lt.parts[i];
      if (prev.type === 'tool' && !prev.content &&
          prev.toolName === p.toolName && prev.toolTarget === p.toolTarget) {
        enrichIndex = i;
        break;
      }
    }
  }
  const enrichTool = enrichIndex >= 0;
  if (enrichTool) lt.parts[enrichIndex] = p;
  else lt.parts.push(p);
  if (!lt.ts && d.ts) lt.ts = d.ts;
  const chat = document.getElementById('chat-scroll');
  const stick = chat && isNearBottom(chat);
  if (d.model) {
    const roleEl = lt.node.querySelector('.role');
    if (roleEl) roleEl.title = d.model;
  }
  if (p.type !== 'tool' && lt.preview) {
    if (lt.previewRAF) cancelAnimationFrame(lt.previewRAF);
    lt.previewRAF = 0;
    lt.preview.classList.remove('stream-preview');
    lt.preview.dataset.raw = p.content || '';
    lt.preview.innerHTML = renderMarkdown(p.content || '');
    lt.partNodes.push(lt.preview);
    lt.preview = null;
    lt.previewText = '';
    lt.acceptDeltas = false; // discard this item's protocol tail after JSONL lands
    return;
  }
  if (p.type === 'tool') {
    if (lt.preview) liveTurnDirty = true;
    lt.acceptDeltas = true; // a later assistant item starts a new preview phase
  } else {
    lt.acceptDeltas = false;
  }
  const tmpl = document.createElement('template');
  tmpl.innerHTML = p.type === 'tool'
    ? renderToolPart(p)
    : `<div class="content" data-raw="${esc(p.content || '')}">${renderMarkdown(p.content || '')}</div>`;
  const partNode = tmpl.content.firstElementChild;
  if (enrichTool && lt.partNodes.length) {
    const oldNode = lt.partNodes[enrichIndex];
    oldNode.replaceWith(partNode);
    lt.partNodes[enrichIndex] = partNode;
  } else {
    lt.node.appendChild(partNode);
    lt.partNodes.push(partNode);
  }
  if (stick && chat) chat.scrollTop = chat.scrollHeight;
}

// finalizeTurn settles the live bubble at turn end. Clean path (the common
// case): promote it in place — its parts are the canonical server-rendered
// content, so committing the node needs no fetch at all. Dirty path (the
// client knows it missed something: joined or reconnected mid-turn, an
// unwitnessed steering prompt): fall back to a full loadTranscript, which
// drops the optimistic nodes and renders canonical turns.
//
// The promoted key is computed from the streamed parts; if it ever diverges
// from the canonical key (e.g. a thinking-only first message shifts the
// turn's timestamp), the next full fetch's reconcile replaces that one node
// — promotion only has to be good enough to self-heal, not perfect.
function finalizeTurn(id, d) {
  if (liveTurn && liveTurn.preview) liveTurnDirty = true;
  clearLiveWorking(liveTurn);
  if (liveTurnDirty) {
    liveTurnDirty = false;
    liveTurn = null; // loadTranscript removes the optimistic node
    loadTranscript(id);
    return;
  }
  if (!liveTurn) return;
  const lt = liveTurn;
  liveTurn = null;
  if (!lt.parts.length) {
    // Nothing canonical to commit (interrupted before output, or an input
    // that produced no assistant turn) — drop the empty shell.
    lt.node.remove();
    return;
  }
  // Prefer the canonical turn timestamp from the exit payload (it is read
  // from the jsonl, so the key matches a later transcript fetch exactly);
  // fall back to the first part's ts.
  const ts = (d && d.assistant_ts) || lt.ts;
  updateMessageTs(lt.node, ts);
  // The promote path renders no fork control of its own; arm it from the
  // exit payload's fork point (a transcript refetch would deliver the same).
  if (d && d.assistant_uuid && !lt.node.querySelector('.turn-fork')) {
    lt.node.insertAdjacentHTML('beforeend', forkBtnHTML(d.assistant_uuid));
  }
  lt.node.classList.remove('optimistic');
  renderedTurns.push({ key: turnKey({ role: 'assistant', ts, parts: lt.parts }), node: lt.node });
  transcriptTotal++;
  updateLoadEarlier(id);
}

async function loadTranscript(id, opts) {
  opts = opts || {};
  try {
    const res = await fetch('/api/sessions/' + encodeURIComponent(id) + '/transcript?limit=' + transcriptLimit);
    if (!res.ok) return;
    const turns = (await res.json()) || [];
    // A late re-fetch must not render into a view the user already left.
    if (id !== currentDetailId) return;
    const total = parseInt(res.headers.get('X-Transcript-Total') || '', 10);
    transcriptTotal = Number.isFinite(total) ? total : turns.length;
    // Transcripts are append-only, so a change shows up as a longer list or a
    // mutated last turn. Skip the rebuild when nothing changed (no flicker /
    // scroll yank when there's nothing new).
    const last = turns[turns.length - 1];
    const sig = turns.length + ':' + (last ? JSON.stringify(last) : '');
    if (sig === lastTranscriptSig) { updateLoadEarlier(id); return; }
    const el = document.getElementById('chat-scroll');
    // Can't render now (view mid-transition): leave lastTranscriptSig untouched
    // so the next call retries this state instead of skipping it forever.
    if (!el) return;
    // Capture stick-to-bottom intent before any mutation changes the geometry.
    const wasAtBottom = isNearBottom(el);
    // Drop the loading stub and any optimistic bubbles (the in-flight turn's
    // user/assistant placeholders) — they're about to be represented by their
    // canonical turns from this fetch.
    const hadOptimistic = !!el.querySelector(':scope > .chat-message.optimistic');
    el.querySelectorAll(':scope > .chat-loading, :scope > .chat-message.optimistic').forEach(n => n.remove());
    const committed = () => el.querySelectorAll(':scope > .chat-message:not(.optimistic)');
    // Self-heal: if our tracked turns drifted from what's actually in the DOM
    // (an earlier early-return, a caught exception, or a race), rebuild from
    // scratch. The old loadTranscript was stateless and so always matched the
    // server; this keeps the incremental path from silently losing an update.
    if (renderedTurns.length !== committed().length) {
      committed().forEach(n => n.remove());
      renderedTurns = [];
    }
    if (!turns.length) {
      if (hadOptimistic) return;
      renderedTurns.forEach(r => r.node.remove());
      renderedTurns = [];
      const empty = document.createElement('div');
      empty.className = 'chat-loading muted';
      empty.style.padding = '0.5rem';
      empty.textContent = 'no past turns yet';
      const sendAnchor = el.querySelector(':scope > .send-anchor');
      if (sendAnchor) el.insertBefore(empty, sendAnchor);
      else el.appendChild(empty);
      lastTranscriptSig = sig;
      updateLoadEarlier(id);
      return;
    }
    // Reconcile against what's already rendered. Transcripts are append-only,
    // so the new list shares a prefix with the old; keep that prefix's DOM
    // untouched and only append (or, if the tail diverged, replace the tail).
    const newKeys = turns.map(turnKey);
    let lcp = 0;
    while (lcp < renderedTurns.length && lcp < newKeys.length && renderedTurns[lcp].key === newKeys[lcp]) lcp++;
    // Drop the diverged tail (last turn finalized, or the window slid past the
    // front), then append everything past the common prefix.
    for (let i = lcp; i < renderedTurns.length; i++) renderedTurns[i].node.remove();
    renderedTurns.length = lcp;
    setSuppressAppendScroll(true);
    try {
      for (let i = lcp; i < turns.length; i++) {
        const node = appendChatMessage(turns[i]);
        if (!node) continue;
        // parts/ts ride along for adoptLastTurnAsLive on a later reconnect
        const entry = { key: newKeys[i], node };
        if (turns[i].role === 'assistant') { entry.parts = turns[i].parts; entry.ts = turns[i].ts; }
        renderedTurns.push(entry);
      }
    } finally {
      setSuppressAppendScroll(false); // never leave it stuck, or future appends won't scroll
    }
    if (opts.anchorHeight != null) {
      // "Load earlier" prepended older turns above the viewport: restore the
      // prior position by the height the prepended content added, so the reader
      // stays on what they were reading.
      el.scrollTop = opts.anchorTop + (el.scrollHeight - opts.anchorHeight);
    } else if (wasAtBottom) {
      // Only follow new turns to the bottom if the reader was already there.
      el.scrollTop = el.scrollHeight;
    }
    // Mark this state rendered only now — a successful render — so any earlier
    // bail-out leaves the signature stale and the next call retries.
    lastTranscriptSig = sig;
    updateLoadEarlier(id);
  } catch {/* ignore — lastTranscriptSig stays put, so the next call retries */}
}

function renderSessionRuntime(u) {
  const el = document.getElementById('session-usage');
  const detail = document.getElementById('session-usage-detail');
  if (!el || !detail) return;
  const context = Number(u && u.context_tokens) || 0;
  el.hidden = context <= 0;
  if (context <= 0) return;
  const max = Number(u.context_window) || 0;
  const pct = max > 0 ? Math.min(100, Math.max(0, context / max * 100)) : 0;
  el.classList.toggle('unknown', max <= 0);
  el.style.setProperty('--usage-pct', pct.toFixed(1) + '%');
  el.setAttribute('aria-label', max > 0
    ? 'Context ' + Math.round(pct) + '% used; show usage details'
    : 'Context limit unavailable; show usage details');
  el.title = max > 0 ? 'Context ' + Math.round(pct) + '% used' : 'Context limit unavailable';

  const rows = [];
  if (u.model) rows.push(`<div><strong>Model</strong><span>${esc(u.model)}</span></div>`);
  if (u.effort) rows.push(`<div><strong>Effort</strong><span>${esc(u.effort)}</span></div>`);
  rows.push(`<div><strong>Context</strong><span>${max > 0 ? esc(formatTokenCount(context) + ' / ' + formatTokenCount(max) + ' · ' + Math.round(pct) + '%') : esc(formatTokenCount(context) + ' / unavailable')}</span></div>`);
  detail.innerHTML = rows.join('');
}

function formatTokenCount(n) {
  n = Number(n) || 0;
  if (n < 1000) return String(n);
  if (n < 1000000) return (n / 1000).toFixed(n < 10000 ? 1 : 0).replace(/\.0$/, '') + 'k';
  return (n / 1000000).toFixed(n < 10000000 ? 1 : 0).replace(/\.0$/, '') + 'm';
}

// updateLoadEarlier shows a "load earlier" control at the top of the transcript
// when the server holds older turns beyond the current window, and removes it
// once the whole history is loaded. Disabled mid-turn (the window is shifting).
function updateLoadEarlier(id) {
  const el = document.getElementById('chat-scroll');
  if (!el) return;
  let btn = el.querySelector(':scope > .load-earlier');
  const more = transcriptTotal > renderedTurns.length;
  if (!more) { if (btn) btn.remove(); return; }
  if (!btn) {
    btn = document.createElement('button');
    btn.className = 'load-earlier';
    btn.type = 'button';
    btn.addEventListener('click', () => loadEarlier(id));
    el.insertBefore(btn, el.firstChild);
  } else if (el.firstChild !== btn) {
    el.insertBefore(btn, el.firstChild);
  }
  btn.disabled = detailStreaming;
  btn.textContent = '↑ load earlier (' + renderedTurns.length + '/' + transcriptTotal + ')';
}

// loadEarlier grows the window by a page and re-fetches, anchoring the scroll so
// the prepended history doesn't yank the reader. No-op while a turn streams.
async function loadEarlier(id) {
  if (detailStreaming) return;
  const el = document.getElementById('chat-scroll');
  if (!el) return;
  transcriptLimit += TRANSCRIPT_PAGE;
  const anchorTop = el.scrollTop;
  const anchorHeight = el.scrollHeight;
  lastTranscriptSig = ''; // window changed — force the reconcile past the gate
  await loadTranscript(id, { anchorTop, anchorHeight });
}

// turnKey identifies a transcript turn for incremental reconcile. For user turns
// it uses content; for assistant turns it fingerprints the parts array.
function turnKey(t) {
  if (t.parts && t.parts.length) {
    const fp = t.parts.map(p => (p.type || '') + (p.toolName || '') + (p.content || '').length).join('|');
    return (t.role || '') + '\x00' + (t.ts || '') + '\x00' + fp;
  }
  const c = t.content || '';
  return (t.role || '') + '\x00' + (t.ts || '') + '\x00' + c.length + '\x00' + c.slice(0, 48);
}

// chatMsgKeys tracks which persisted messages are rendered, across the two
// arrival paths (history refetch + SSE stream), so their overlap renders once.
// Reset by loadChatMessages, whose snapshot becomes the new ground truth.
let chatMsgKeys = new Set();
function chatKey(m) { return (m.role || '') + '\x00' + (m.ts || '') + '\x00' + (m.content || ''); }
function chatSeenKey(m) { return chatMsgKeys.has(chatKey(m)); }
function markChatSeen(m) { chatMsgKeys.add(chatKey(m)); }

async function loadChatMessages(id) {
  try {
    const res = await fetch('/api/mainchats/' + encodeURIComponent(id) + '/messages');
    if (!res.ok) return;
    const data = (await res.json()) || [];
    const list = document.getElementById('chat-scroll');
    if (!list) return;
    list.querySelectorAll(':scope > .chat-loading, :scope > .chat-message').forEach(n => n.remove());
    chatMsgKeys = new Set();
    setSuppressAppendScroll(true);
    try {
      for (const m of data) { appendChatMessage(m); markChatSeen(m); }
    } finally {
      setSuppressAppendScroll(false);
    }
    list.scrollTop = list.scrollHeight; // fresh main-chat load lands at the bottom
  } catch { /* ignore */ }
}

async function loadMainChatInfo(id) {
  try {
    const res = await fetch('/api/mainchats/' + encodeURIComponent(id));
    if (!res.ok) return;
    const info = await res.json();
    renderFocus(info.focus);
  } catch {}
}

function renderFocus(focus) {
  const left = `<span class="subtitle-left"><strong class="session-title">Router</strong></span>`;
  if (!focus || !focus.session_id) {
    subtitle.innerHTML = left;
    return;
  }
  const sid = esc(focus.session_id.slice(0, 8));
  subtitle.innerHTML =
    left +
    `<a href="#/s/${esc(focus.session_id)}" class="subtitle-focus">focus: ${sid}</a>`;
}

// Preserve the reader's place across resize / orientation change.
let _anchorEl = null;
let _anchorOff = 0;
document.addEventListener('scroll', (e) => {
  if (e.target.id !== 'chat-scroll') return;
  const r = e.target.getBoundingClientRect();
  const hit = document.elementFromPoint(r.left + r.width / 2, r.top + 1);
  const msg = hit && hit.closest('#chat-scroll > .chat-message');
  if (msg) { _anchorEl = msg; _anchorOff = msg.offsetTop - e.target.scrollTop; }
}, true);
window.addEventListener('resize', () => {
  const el = document.getElementById('chat-scroll');
  if (!el) return;
  if (_anchorEl && _anchorEl.isConnected) el.scrollTop = _anchorEl.offsetTop - _anchorOff;
});

// Fork click delegate. No confirmation: the fork is a pure server-side file
// copy (the source session is untouched) — success navigates into the branch.
document.addEventListener('click', async (e) => {
  const btn = e.target.closest('.turn-fork');
  if (!btn || !currentDetailId) return;
  btn.disabled = true;
  try {
    const res = await fetch('/api/sessions/' + encodeURIComponent(currentDetailId) + '/fork', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ after_uuid: btn.dataset.uuid }),
    });
    if (!res.ok) {
      const err = await res.json().catch(() => ({}));
      appendChatMessage({ role: 'error', content: 'fork failed: ' + (err.error || ('HTTP ' + res.status)), ts: new Date().toISOString() });
      return;
    }
    const data = await res.json();
    location.hash = '#/s/' + data.id;
  } catch (e2) {
    appendChatMessage({ role: 'error', content: 'fork failed: ' + String(e2), ts: new Date().toISOString() });
  } finally {
    btn.disabled = false;
  }
});

// Copy button on tool block headers: copies the .tool-target text (path,
// command, or pattern) verbatim. Document-level delegate for the same reason
// as .turn-fork: transcript nodes get re-rendered.
document.addEventListener('click', (e) => {
  const btn = e.target.closest('.tool-copy');
  if (!btn) return;
  e.preventDefault(); // a click inside <summary> would otherwise toggle it
  const t = btn.closest('summary')?.querySelector('.tool-target');
  if (!t) return;
  copyText(t.textContent).then((ok) => {
    if (!ok) return;
    btn.classList.add('copied');
    setTimeout(() => btn.classList.remove('copied'), 1200);
  });
});

// navigator.clipboard needs a secure context; plain-HTTP deployments (e.g. a
// raw tailnet IP) fall back to execCommand. Both require a user gesture.
function copyText(s) {
  if (navigator.clipboard && window.isSecureContext) {
    return navigator.clipboard.writeText(s).then(() => true, () => legacyCopy(s));
  }
  return Promise.resolve(legacyCopy(s));
}

function legacyCopy(s) {
  const ta = document.createElement('textarea');
  ta.value = s;
  ta.style.position = 'fixed';
  ta.style.opacity = '0';
  document.body.appendChild(ta);
  ta.select();
  let ok = false;
  try { ok = document.execCommand('copy'); } catch (_) {}
  ta.remove();
  return ok;
}
