// usher SPA: hash-based routing between session list and detail view.
// Detail view streams subprocess events via SSE.

// Wrap fetch so any 401 from the API navigates to /login. SSE can't see
// HTTP status, so we rely on the polling fetches (session list, etc.) to
// trip this path within a few seconds of a cookie going bad.
const _origFetch = window.fetch.bind(window);
window.fetch = async (...args) => {
  const res = await _origFetch(...args);
  if (res.status === 401) {
    const next = encodeURIComponent(location.pathname + location.search + location.hash);
    location.href = '/login?next=' + next;
    // Return a never-resolving promise so callers don't proceed with the
    // 401 body. Navigation is about to take over the page anyway.
    return new Promise(() => {});
  }
  return res;
};

const root = document.getElementById('root');
const subtitle = document.getElementById('subtitle');
const renderModeBtn = document.getElementById('render-mode-btn');

let listInterval = null;
let currentES = null;
// The terminal-mirror SSE (the inline pane panel), tracked separately from
// currentES so the detail view's /events stream and the mirror's /screen
// stream can be open at once and both get torn down on navigation.
let currentScreenES = null;
// claude's bottom UI is a fixed 4-row block: input-box top border, input line,
// bottom border, hint line (verified by capture). The auto preview hides it as
// furniture; if claude's chrome ever changes, this is the number to revisit.
const TERM_FURNITURE_ROWS = 4;
const TERM_AUTO_ROWS = 14; // pane height captured for the auto inline preview
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
let currentDetailId = null;
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
// Auto-scroll to the bottom on new content only when the user is already near
// it, so scrolling up to read history isn't yanked back down.
const BOTTOM_THRESHOLD = 64;
function isNearBottom(el) { return el.scrollHeight - el.scrollTop - el.clientHeight < BOTTOM_THRESHOLD; }
// Set while a batch reconcile appends many turns so appendChatMessage skips its
// per-append scroll — reading scrollHeight each iteration forces a synchronous
// layout, making a big batch O(n²). The batch caller scrolls once at the end.
let suppressAppendScroll = false;
// Last sidebar HTML written to the DOM. The sidebar re-polls every 5s; skipping
// the innerHTML rewrite when nothing changed keeps the live-dot CSS animation
// from restarting (jumping back to its bright peak) on every poll.
let lastSidebarHtml = '';
// Last list-view rows HTML — same skip-when-unchanged trick as the sidebar,
// so the 5s poll doesn't restart status-dot animations or fight the filter.
let lastListRowsHtml = '';
let lastCwdSig = ''; // distinct-cwd set the list's cwd <select> was built from
let renderMode = localStorage.getItem('usher.renderMode') === 'raw' ? 'raw' : 'md';

// growPrompt sizes the textarea to its content (CSS min/max-height clamp it to
// 1–3 lines). The delegated input listener covers every view; call it directly
// after a programmatic clear, which fires no input event.
function growPrompt(el) {
  if (!el) return;
  el.style.height = 'auto';
  el.style.height = el.scrollHeight + 'px';
}

document.addEventListener('input', (e) => {
  if (e.target && e.target.id === 'prompt') growPrompt(e.target);
});
// Per-cwd archived-disclosure expansion state. Session-only — refresh
// collapses everything, matching the assumption that browsing archived
// sessions is a rare action.
const cwdExpanded = new Set();

function esc(s) {
  return String(s).replace(/[&<>"']/g, c => ({
    '&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'
  }[c]));
}

// renderMarkdown turns assistant/user content into safe HTML using the
// vendored marked (GFM: tables, task lists, nested lists, strikethrough,
// autolinks). marked deliberately ships without HTML sanitization, so two
// of our own layers keep things safe:
//
//   1. We let marked do all entity-escaping (it correctly escapes both text
//      and code content) and only neutralize the one thing it would pass
//      through verbatim — raw HTML — by escaping every block/inline `html`
//      token via the renderer hook. A raw `<script>` thus lands as literal
//      text. (The old approach pre-escaped the whole input instead, which
//      double-escaped entities inside code spans/fences because marked
//      re-encodes code content — so `don't` rendered as `don&#39;t`.)
//
//   2. We strip risky URL schemes (javascript:/data:/vbscript:) from any
//      <a>/<img> in marked's output before handing it to the DOM.
//
// breaks:true: a single \n becomes <br> so replies keep their line breaks
// (CommonMark would fold a lone \n to a space). Blank line = new paragraph.
window.marked.use({
  gfm: true,
  breaks: true,
  silent: true,
  renderer: {
    html(token) { return esc(typeof token === 'string' ? token : token.text); },
  },
});

function renderMarkdown(md) {
  if (renderMode === 'raw') {
    return '<pre class="raw-markdown">' + esc(md || '') + '</pre>';
  }
  let html = window.marked.parse(String(md || ''));
  html = html.replace(
    /(<(?:a|img)\b[^>]*?\b(?:href|src)=")\s*(?:javascript|data|vbscript)\s*:[^"]*"/gi,
    '$1#unsafe"',
  );
  html = html.replace(/<a /g, '<a target="_blank" rel="noopener" ');
  return html;
}

// Re-render every element that carries a data-raw markdown source. Called
// whenever the user toggles md/raw mode. Streaming response stays connected
// because we don't tear down the SSE — only the in-place HTML changes.
function rerenderAllContent() {
  document.querySelectorAll('[data-raw]').forEach(el => {
    el.innerHTML = renderMarkdown(el.dataset.raw);
  });
}

function setRenderMode(mode) {
  renderMode = mode === 'raw' ? 'raw' : 'md';
  localStorage.setItem('usher.renderMode', renderMode);
  updateRenderModeBtn();
  rerenderAllContent();
}

function updateRenderModeBtn() {
  if (!renderModeBtn) return;
  const val = document.getElementById('render-mode-val');
  if (val) val.textContent = renderMode;
  renderModeBtn.setAttribute('aria-pressed', renderMode === 'raw');
}

if (renderModeBtn) {
  renderModeBtn.addEventListener('click', () => {
    setRenderMode(renderMode === 'md' ? 'raw' : 'md');
  });
  updateRenderModeBtn();
}

function fmt(iso) {
  if (!iso) return '';
  const d = new Date(iso);
  if (isNaN(d)) return '';
  return d.toLocaleString();
}
function timeNow() {
  return new Date().toLocaleTimeString();
}

function closeES() {
  if (currentES) { currentES.close(); currentES = null; }
  closeScreenES();
}
function closeScreenES() {
  if (currentScreenES) { currentScreenES.close(); currentScreenES = null; }
}
function clearListInterval() {
  if (listInterval) { clearInterval(listInterval); listInterval = null; }
}

window.addEventListener('hashchange', route);

function route() {
  const hash = location.hash || '#/';
  if (hash === '#/' || hash === '') {
    showList();
  } else if (hash === '#/new') {
    showNewSession();
  } else if (hash === '#/chat' || hash.startsWith('#/chat/')) {
    const id = hash === '#/chat' ? 'default' : decodeURIComponent(hash.slice('#/chat/'.length));
    showMainChat(id);
  } else if (hash.startsWith('#/s/')) {
    showDetail(decodeURIComponent(hash.slice(4)));
  }
  updateSidebarActive();
}

// ---------- Sidebar ----------
//
// Polled every 5s independently of the active route. Renders Claude Code
// sessions grouped by cwd, recent activity first. The "main chat" entry is
// static markup in index.html — no fetch needed since we only ever route
// to a single mainchat (id=default).

async function loadSidebar() {
  try {
    // Always fetch include_archived=1 so the count and per-cwd disclosure
    // can show how many are archived even when collapsed. Payload size is
    // trivial at this scale.
    const res = await fetch('/api/sessions?include_archived=1');
    const sessions = res.ok ? (await res.json() || []) : [];
    renderSidebarSessions(sessions);
    updateSidebarActive();
  } catch {/* server may be down briefly */}
}

function renderSidebarSessions(allSessions) {
  const wrap = document.getElementById('sidebar-sessions');
  const count = document.getElementById('sidebar-session-count');
  const visible = allSessions.filter(s => !s.archived);
  if (count) {
    count.textContent = visible.length === allSessions.length
      ? '(' + allSessions.length + ')'
      : '(' + visible.length + '/' + allSessions.length + ')';
  }
  if (!wrap) return;
  if (!allSessions.length) {
    wrap.innerHTML = '<div class="sidebar-empty">no sessions found</div>';
    lastSidebarHtml = '';
    return;
  }
  // Group ALL sessions by cwd (incl. archived) so each group's tail can
  // offer a "└[ N archived ]" disclosure without a separate count lookup.
  const groups = new Map();
  for (const s of allSessions) {
    const cwd = s.cwd || '(unknown)';
    if (!groups.has(cwd)) groups.set(cwd, []);
    groups.get(cwd).push(s);
  }
  // Per design decision (a): cwds whose every session is archived simply
  // disappear from the sidebar. They can still be reached by URL; if a
  // user needs to recover one we may add a bottom "+ N from archived
  // cwds" affordance later, but not yet.
  const cwds = [...groups.keys()]
    .filter(cwd => groups.get(cwd).some(s => !s.archived));
  if (!cwds.length) {
    wrap.innerHTML = '<div class="sidebar-empty">no sessions found</div>';
    lastSidebarHtml = '';
    return;
  }
  const recencyOf = arr => Math.max(...arr.map(s => Date.parse(s.last_event_at) || 0));
  const byRecent = (a, b) => (Date.parse(b.last_event_at) || 0) - (Date.parse(a.last_event_at) || 0);
  // Sort cwd groups by their most-recent visible activity, not absolute
  // — stale cwds with one expanded archived row shouldn't jump to the top.
  cwds.sort((a, b) => {
    const av = groups.get(a).filter(s => !s.archived);
    const bv = groups.get(b).filter(s => !s.archived);
    return recencyOf(bv) - recencyOf(av);
  });

  const renderItem = s => {
    const href = '#/s/' + encodeURIComponent(s.id);
    const dot = statusDot(s.status);
    const auto = s.auto_approve
      ? '<span class="auto-dot" title="auto-approve enabled">ϟ</span>'
      : '';
    const title = s.title || '(untitled)';
    const liClass = s.archived ? 'sidebar-item archived-row' : 'sidebar-item';
    return `<li class="${liClass}">
      <a href="${esc(href)}" data-route="s:${esc(s.id)}" title="${esc(title)}">${dot}${auto}${esc(title)}</a>
      <button class="kebab-btn" type="button"
        data-id="${esc(s.id)}" data-archived="${s.archived ? '1' : '0'}"
        aria-label="session actions" title="more">⋮</button>
    </li>`;
  };

  const html = cwds.map(cwd => {
    const all = groups.get(cwd);
    const visibleItems = all.filter(s => !s.archived).sort(byRecent);
    const archivedItems = all.filter(s => s.archived).sort(byRecent);
    const expanded = cwdExpanded.has(cwd);
    let lis = visibleItems.map(renderItem).join('');
    if (expanded) lis += archivedItems.map(renderItem).join('');
    const toggleRow = archivedItems.length === 0
      ? ''
      : `<button class="cwd-toggle-archived" type="button" data-cwd="${esc(cwd)}">${
          expanded ? '└ [ collapse ]' : '└ [ ' + archivedItems.length + ' archived ]'
        }</button>`;
    return `<div class="cwd-group">
      <div class="cwd-label" title="${esc(cwd)}">${esc(cwd)}</div>
      <ul class="sidebar-list">${lis}</ul>
      ${toggleRow}
    </div>`;
  }).join('');
  // Only touch the DOM when the rendered markup actually changed, so a steady
  // session's status-dot animation keeps running smoothly across polls.
  if (html === lastSidebarHtml) return;
  lastSidebarHtml = html;
  wrap.innerHTML = html;
}

function updateSidebarActive() {
  const hash = location.hash || '#/';
  const inMainChat = hash === '#/chat' || hash.startsWith('#/chat/');
  document.querySelectorAll('.sidebar-mainchat').forEach(a => {
    a.classList.toggle('active', inMainChat);
  });
  document.querySelectorAll('.sidebar-new').forEach(a => {
    a.classList.toggle('active', hash === '#/new');
  });
  let sessionKey = '';
  if (hash.startsWith('#/s/')) {
    sessionKey = 's:' + decodeURIComponent(hash.slice(4));
  }
  document.querySelectorAll('#sidebar a[data-route]').forEach(a => {
    a.classList.toggle('active', a.dataset.route === sessionKey);
  });
}

// Mobile sidebar toggle. The sidebar is fixed-position with a slide-in
// transform under 720px wide; the hamburger button toggles .open.
const mobileToggle = document.getElementById('mobile-toggle');
const sidebarEl = document.getElementById('sidebar');
if (mobileToggle && sidebarEl) {
  mobileToggle.addEventListener('click', () => sidebarEl.classList.toggle('open'));
  window.addEventListener('hashchange', () => sidebarEl.classList.remove('open'));
}
const sidebarBackdrop = document.getElementById('sidebar-backdrop');
if (sidebarBackdrop && sidebarEl) {
  sidebarBackdrop.addEventListener('click', () => sidebarEl.classList.remove('open'));
}

// Kebab popover. A single floating element lives at document level and
// is repositioned on each open; closing on outside click, Esc, scroll,
// or resize keeps it tied to the source button without an observer.
const kebabPopover = document.getElementById('kebab-popover');
let kebabOpenFor = null; // session id currently anchored

let kebabOpenBtn = null; // the button element currently anchoring the popover

function closeKebabPopover() {
  if (!kebabPopover) return;
  kebabPopover.hidden = true;
  kebabPopover.innerHTML = '';
  if (kebabOpenBtn) kebabOpenBtn.classList.remove('open');
  kebabOpenBtn = null;
  kebabOpenFor = null;
}

function openKebabPopover(btn) {
  if (!kebabPopover) return;
  const id = btn.dataset.id;
  const archived = btn.dataset.archived === '1';
  const action = archived ? 'unarchive' : 'archive';
  const label = archived ? 'Unarchive' : 'Archive';
  kebabPopover.innerHTML =
    `<button type="button" class="kebab-item" data-action="${action}" data-id="${esc(id)}">${label}</button>` +
    `<button type="button" class="kebab-item kebab-danger" data-action="delete" data-id="${esc(id)}">Delete</button>`;
  kebabPopover.hidden = false;
  // Position below-right of the button; clamp to viewport edges so the
  // menu stays fully visible on narrow screens.
  const r = btn.getBoundingClientRect();
  const popW = kebabPopover.offsetWidth;
  const popH = kebabPopover.offsetHeight;
  let left = r.right - popW;
  let top = r.bottom + 4;
  if (left < 4) left = 4;
  if (top + popH > window.innerHeight - 4) {
    top = r.top - popH - 4;
  }
  kebabPopover.style.left = left + 'px';
  kebabPopover.style.top = top + 'px';
  kebabOpenFor = id;
  // .open keeps the kebab visible after the cursor leaves its row — the
  // user is now interacting with the popover, not the row.
  btn.classList.add('open');
  kebabOpenBtn = btn;
}

document.addEventListener('click', (e) => {
  const cwdToggle = e.target.closest('.cwd-toggle-archived');
  if (cwdToggle) {
    e.preventDefault();
    const cwd = cwdToggle.dataset.cwd;
    if (cwdExpanded.has(cwd)) cwdExpanded.delete(cwd);
    else cwdExpanded.add(cwd);
    loadSidebar();
    return;
  }
  const kebab = e.target.closest('.kebab-btn');
  if (kebab) {
    e.preventDefault();
    e.stopPropagation();
    if (kebabOpenFor === kebab.dataset.id) {
      closeKebabPopover();
    } else {
      openKebabPopover(kebab);
    }
    return;
  }
  const item = e.target.closest('.kebab-item');
  if (item) {
    e.preventDefault();
    e.stopPropagation();
    handleKebabAction(item.dataset.action, item.dataset.id);
    return;
  }
  if (kebabOpenFor && !e.target.closest('#kebab-popover')) {
    closeKebabPopover();
  }
});
document.addEventListener('keydown', (e) => {
  if (e.key === 'Escape' && kebabOpenFor) closeKebabPopover();
});
window.addEventListener('resize', closeKebabPopover);
// Listen on the sidebar's scroll container so a long sidebar scroll
// doesn't leave the popover floating mid-air.
if (sidebarEl) sidebarEl.addEventListener('scroll', closeKebabPopover, { passive: true });

async function handleKebabAction(action, id) {
  closeKebabPopover();
  if (action === 'delete') {
    deleteSession(id);
    return;
  }
  const method = action === 'archive' ? 'POST' : 'DELETE';
  try {
    const res = await fetch('/api/sessions/' + encodeURIComponent(id) + '/archive', { method });
    if (!res.ok) throw new Error('HTTP ' + res.status);
    loadSidebar();
    loadList(); // no-op unless the list view is open
  } catch (e) {
    console.warn('archive/unarchive failed', e);
  }
}

// deleteSession permanently removes a session (and its transcript) after a
// confirm — irreversible, unlike archive. If the deleted session is the one on
// screen, route home so the detail view doesn't sit on a now-404 id.
async function deleteSession(id) {
  if (!confirm('Delete this session permanently? Its conversation transcript will be removed and cannot be recovered.')) {
    return;
  }
  try {
    const res = await fetch('/api/sessions/' + encodeURIComponent(id), { method: 'DELETE' });
    if (!res.ok) throw new Error('HTTP ' + res.status);
    if (location.hash === '#/s/' + encodeURIComponent(id)) {
      location.hash = '#/';
    }
    loadSidebar();
    loadList(); // no-op unless the list view is open
  } catch (e) {
    console.warn('delete failed', e);
    alert('Failed to delete session.');
  }
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

async function showNewSession() {
  clearListInterval();
  currentDetailId = null;
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
        <div class="composer">
          <textarea id="prompt" rows="1" placeholder="message…"></textarea>
          <div class="composer-bar">
            <div class="composer-tools">
              <select id="new-model" class="composer-model" aria-label="model">
                <optgroup label="Claude">
                  <option value="opus" selected>Opus</option>
                  <option value="claude-opus-4-6">Opus 4.6</option>
                  <option value="sonnet">Sonnet</option>
                  <option value="sonnet[1m]">Sonnet [1m]</option>
                  <option value="haiku">Haiku</option>
                  <option value="fable">Fable</option>
                  <option value="opusplan">Opus Plan</option>
                </optgroup>
                <optgroup label="Codex" id="codex-modelgroup"></optgroup>
              </select>
            </div>
            <div class="composer-send"><button id="send">send</button></div>
          </div>
        </div>
        <div class="send-controls">
          <label class="new-cwd-field">
            <span class="muted">cwd</span>
            <input id="new-cwd" type="text" list="new-cwd-list" autocomplete="off"
                   placeholder="/absolute/path/to/project">
            <datalist id="new-cwd-list">${options}</datalist>
          </label>
        </div>
        <div id="new-session-err" class="err" style="display:none; margin-top:0.5rem"></div>
      </section>
    </div>
  `;

  const promptEl = document.getElementById('prompt');
  const sendBtn = document.getElementById('send');
  const cwdEl = document.getElementById('new-cwd');
  const modelEl = document.getElementById('new-model');
  const errEl = document.getElementById('new-session-err');
  cwdEl.focus();

  // Restore the last-picked model (the <select> defaults to Opus in markup; an
  // unknown stored value just leaves that default). Run AFTER codex models load
  // so a saved codex pick can be restored once its option exists.
  // Tint the picker by the selected model's backend (Claude coral / Codex green),
  // keyed off the chosen option's optgroup so it tracks whichever group it's in.
  const syncModelColor = () => {
    const og = modelEl.selectedOptions[0] && modelEl.selectedOptions[0].closest('optgroup');
    modelEl.classList.toggle('codex', !!og && og.label === 'Codex');
  };
  const applySavedModel = () => {
    try {
      const saved = localStorage.getItem('usher.newModel');
      if (saved && [...modelEl.options].some(o => o.value === saved)) modelEl.value = saved;
    } catch {/* private mode → keep default */}
    syncModelColor();
  };
  modelEl.addEventListener('change', () => {
    syncModelColor();
    try { localStorage.setItem('usher.newModel', modelEl.value); } catch {/* private mode */}
  });
  // Show only installed backends. Drop the Claude group if Claude isn't present
  // (so the default lands on an available model), and populate Codex's group from
  // its per-account catalog (or drop it if empty). The browser then selects the
  // first remaining option as the default.
  fetch('/api/models').then(r => r.ok ? r.json() : {}).then(data => {
    const backends = (data && data.backends) || ['claude'];
    if (!backends.includes('claude')) {
      const cg = modelEl.querySelector('optgroup[label="Claude"]');
      if (cg) cg.remove();
    }
    const grp = document.getElementById('codex-modelgroup');
    if (grp) {
      const models = (data && data.codex) || [];
      if (!models.length) {
        grp.remove();
      } else {
        for (const m of models) {
          const o = document.createElement('option');
          o.value = m.value;
          o.textContent = m.label;
          grp.appendChild(o);
        }
      }
    }
    applySavedModel();
  }).catch(applySavedModel);

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

// ---------- List view ----------

async function showList() {
  closeES();
  currentDetailId = null;
  subtitle.textContent = 'discovered Claude Code sessions';
  // Stable shell: pinned controls + a .list-scroll wrapper (the scroll
  // container — <main> is overflow:hidden). loadList only swaps the rows, so
  // the 5s poll doesn't disturb the controls.
  root.innerHTML = `
    <div class="list-controls">
      <select id="list-cwd"><option value="">all folders</option></select>
      <label class="archived-toggle"><input type="checkbox" id="list-archived"> show archived</label>
    </div>
    <div class="list-scroll">
      <table>
        <colgroup><col><col class="col-cwd"><col class="col-when"><col class="col-act"></colgroup>
        <thead><tr><th>title</th><th>cwd</th><th>last activity</th><th aria-label="actions"></th></tr></thead>
        <tbody id="list-rows"></tbody>
      </table>
    </div>`;
  lastListRowsHtml = '';
  lastCwdSig = '';
  const cEl = document.getElementById('list-cwd');
  if (cEl) cEl.addEventListener('change', applyListFilter);
  const aEl = document.getElementById('list-archived');
  if (aEl) aEl.addEventListener('change', applyListFilter);
  if (!listInterval) listInterval = setInterval(loadList, 5000);
  await loadList();
}

// applyListFilter hides rows that don't match the cwd dropdown and the
// archived toggle. All client-side — the rows always include archived sessions.
function applyListFilter() {
  const rowsEl = document.getElementById('list-rows');
  if (!rowsEl) return;
  const cEl = document.getElementById('list-cwd');
  const aEl = document.getElementById('list-archived');
  const cwd = cEl ? cEl.value : '';
  const showArchived = aEl ? aEl.checked : false;
  rowsEl.querySelectorAll('tr[data-id]').forEach(tr => {
    const okCwd = !cwd || tr.dataset.cwd === cwd;
    const okArchived = showArchived || tr.dataset.archived !== '1';
    tr.style.display = (okCwd && okArchived) ? '' : 'none';
  });
}

// updateCwdOptions rebuilds the cwd <select> from the distinct cwds, but only
// when the set changed, so the 5s poll doesn't reset the user's selection.
function updateCwdOptions(cwds) {
  const sel = document.getElementById('list-cwd');
  if (!sel) return;
  const sig = cwds.join('\n');
  if (sig === lastCwdSig) return;
  lastCwdSig = sig;
  const cur = sel.value;
  sel.innerHTML = '<option value="">all folders</option>' +
    cwds.map(c => `<option value="${esc(c)}">${esc(c)}</option>`).join('');
  if (cwds.includes(cur)) sel.value = cur;
}

async function loadList() {
  if (location.hash && location.hash !== '#/' && location.hash !== '') return;
  const rowsEl = document.getElementById('list-rows');
  if (!rowsEl) return; // shell not built (not on the list view)
  try {
    // Always fetch the full set (incl. archived); the controls narrow it
    // client-side, so toggling "show archived" needs no refetch.
    const res = await fetch('/api/sessions?include_archived=1');
    if (!res.ok) throw new Error('HTTP ' + res.status);
    const data = (await res.json()) || [];
    const html = data.length ? data.map(s => {
      const title = s.title || '(untitled)';
      const dot = statusDot(s.status);
      return `
      <tr data-id="${esc(s.id)}" data-cwd="${esc(s.cwd || '')}" data-archived="${s.archived ? '1' : ''}" class="${s.archived ? 'archived' : ''}">
        <td class="title" title="${esc(title)}">${dot ? dot + ' ' : ''}${esc(title)}</td>
        <td class="cwd" title="${esc(s.cwd || '')}">${esc(s.cwd || '')}</td>
        <td>${esc(fmt(s.last_event_at))}</td>
        <td class="act"><button class="kebab-btn" type="button" data-id="${esc(s.id)}" data-archived="${s.archived ? '1' : '0'}" aria-label="session actions" title="more">⋮</button></td>
      </tr>`;
    }).join('') : '<tr><td colspan="4" class="muted" style="padding:0.75rem">no sessions found</td></tr>';
    // Skip the rebuild when unchanged so status-dot animations don't restart
    // and the current filter view is left untouched.
    if (html === lastListRowsHtml) return;
    lastListRowsHtml = html;
    rowsEl.innerHTML = html;
    rowsEl.querySelectorAll('tr[data-id]').forEach(tr => {
      tr.addEventListener('click', (e) => {
        // Kebab clicks are taken by its own (document-level) popover handler;
        // the row listener runs first while bubbling, so skip them here.
        if (e.target.closest('.kebab-btn')) return;
        location.hash = '#/s/' + encodeURIComponent(tr.dataset.id);
      });
    });
    updateCwdOptions([...new Set(data.map(s => s.cwd).filter(Boolean))].sort());
    applyListFilter(); // keep the active filters applied across polls
  } catch (e) {
    rowsEl.innerHTML = '<tr><td colspan="4" class="err" style="padding:0.75rem">failed to load: ' + esc(String(e)) + '</td></tr>';
    lastListRowsHtml = '';
  }
}

// ---------- Detail view ----------

async function showDetail(id) {
  const epoch = ++detailEpoch;
  clearListInterval();
  closeES();
  // Fresh view: reset sync state so a prior session's signature/stream flag
  // can't suppress this one's first render.
  currentDetailId = id;
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

  root.innerHTML = `
    <div id="chat-scroll" class="chat-area">
      <div class="chat-loading muted" style="padding:0.5rem">loading…</div>
      <section class="send-anchor">
        <div id="term-panel" class="term-panel" hidden>
          <div class="term-screen"><pre id="term-grid" class="term-grid muted">connecting…</pre></div>
          <div class="term-keys" id="term-keys">
            <button type="button" data-key="escape">esc</button>
            <button type="button" data-key="tab">tab</button>
            <button type="button" data-key="up" aria-label="up">↑</button>
            <button type="button" data-key="down" aria-label="down">↓</button>
            <button type="button" data-key="left" aria-label="left">←</button>
            <button type="button" data-key="right" aria-label="right">→</button>
            <button type="button" data-key="enter">⏎</button>
          </div>
        </div>
        <div class="composer">
          <textarea id="prompt" rows="1" placeholder="message…"></textarea>
          <div class="composer-bar">
            <div class="composer-tools">
              <button id="auto-approve-toggle" class="auto-approve-toggle" type="button"
                aria-pressed="${sess.auto_approve ? 'true' : 'false'}"
                title="when on, every PreToolUse hook for this session is allowed without prompting">
                <span class="t-icon">ϟ</span><span class="t-full">approve:</span><span class="toggle-val">${sess.auto_approve ? 'auto' : 'ask'}</span>
              </button>
              <button id="term-toggle" class="term-toggle" type="button" aria-pressed="false"
                title="terminal mirror — click to cycle off → auto → on. auto previews live output inline in the turn bubble (before it lands); on docks an interactive pane.">
                <span class="t-icon">&gt;_</span><span class="t-full">terminal:</span><span class="toggle-val">off</span>
              </button>
            </div>
            <div class="composer-send">
              <button id="send">send</button>
              <button id="cancel" class="cancel" hidden>cancel</button>
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
  const cancelBtn = document.getElementById('cancel');

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

  // Terminal mirror: a collapsible live pane docked above the input, driven by
  // a 3-state toggle that cycles off → auto → on (persisted in localStorage):
  //   off  — hidden, no automatic behaviour (default)
  //   auto — reveals on send (the un-flushed turn is starting) and hides again
  //          after a deliberate scroll up into history; re-reveals next send
  //   on   — pinned open, never auto-hidden
  // The /screen stream runs only while actually shown, so background sessions
  // don't poll capture-pane. Soft keys wire once (the grid node is permanent).
  const termToggle = document.getElementById('term-toggle');
  const termPanel = document.getElementById('term-panel');
  let termMode = (() => { try { return localStorage.getItem('usher.term.mode') || 'auto'; } catch { return 'auto'; } })();
  let termShown = false;        // whether the docked panel is currently streaming/visible
  let termAppliedRows = 0;      // rows last applied — gates needless restreams
  let softKeysWired = false;
  const TERM_ROW_PX = 16.25; // .term-grid 13px × line-height 1.25 (keep in sync with CSS)
  // on fills half the chat area, bounded so the input/controls below stay
  // visible. The pane and the viewer box are both sized from this one number.
  const onRows = () => {
    const chatH = chatEl.clientHeight;
    const roomRows = Math.floor((chatH - 6 * 16) / TERM_ROW_PX); // leave ~6rem for input
    const halfRows = Math.floor((chatH / 2) / TERM_ROW_PX);
    return Math.max(1, Math.min(halfRows, roomRows));
  };
  // applyTermVisibility drives the docked panel, which now serves on only —
  // auto's mirror is the inline preview piped into the live turn bubble during
  // a turn, not this panel. The /screen restream re-applies pane size, so it's
  // skipped when the row count hasn't changed.
  const applyTermVisibility = () => {
    if (termMode !== 'on') {
      if (termShown) { termShown = false; if (termPanel) termPanel.hidden = true; closeScreenES(); }
      return;
    }
    const rows = onRows();
    if (termShown && rows === termAppliedRows) return; // already shown at this size
    termShown = true;
    termAppliedRows = rows;
    if (termPanel) termPanel.hidden = false;
    if (!softKeysWired) { wireSoftKeys(id); softKeysWired = true; }
    const gridEl = document.getElementById('term-grid');
    if (gridEl) {
      const box = gridEl.parentElement;
      // content-box: max-height is the content area, so rows×ROW_PX shows exactly
      // `rows` lines (+a few px slack to avoid a sub-pixel scrollbar).
      box.style.maxHeight = Math.ceil(rows * TERM_ROW_PX) + 4 + 'px';
      // on shows the whole pane (dropTail 0) plus the soft keys — it's the
      // interactive view for debugging / driving /rewind.
      openScreenStream(id, gridEl, measureCols(box), rows, 0);
    }
    // The panel grows the sticky anchor; keep the chat pinned if it was.
    if (isNearBottom(chatEl)) chatEl.scrollTop = chatEl.scrollHeight;
  };
  // evStream exposes syncInline so a mode toggle can start/stop auto's inline
  // preview against the in-flight turn (assigned once openEventStream runs below).
  let evStream = null;
  // applyTermMode reflects termMode onto the button, resolves the docked panel,
  // then reconciles auto's inline preview with the new mode.
  const applyTermMode = () => {
    if (termToggle) {
      termToggle.setAttribute('data-mode', termMode);
      termToggle.setAttribute('aria-pressed', termMode === 'off' ? 'false' : 'true');
      termToggle.querySelector('.toggle-val').textContent = termMode;
    }
    applyTermVisibility();
    if (evStream) evStream.syncInline();
  };
  if (termToggle && termPanel) {
    termToggle.addEventListener('click', () => {
      // off → auto → on → off. off: no mirror. auto: live output streams into
      // the live turn bubble during a turn (no docked panel). on: docked,
      // interactive panel. The mode is a persisted preference.
      termMode = termMode === 'off' ? 'auto' : termMode === 'auto' ? 'on' : 'off';
      try { localStorage.setItem('usher.term.mode', termMode); } catch { /* private mode */ }
      applyTermMode();
    });
    applyTermMode(); // honour the persisted mode on open ('on' reveals the panel)
  }

  cancelBtn.addEventListener('click', async () => {
    cancelBtn.disabled = true;
    try {
      await fetch('/api/sessions/' + encodeURIComponent(id) + '/send', { method: 'DELETE' });
    } catch (e) {
      appendChatMessage({ role: 'error', content: 'cancel failed: ' + String(e), ts: new Date().toISOString() });
    } finally {
      cancelBtn.disabled = false;
    }
  });

  evStream = openEventStream(id, chatEl, sendBtn, cancelBtn, () => termMode);

  const submit = async () => {
    const text = promptEl.value;
    if (!text.trim() || sendBtn.disabled) return;
    sendBtn.disabled = true;
    promptEl.value = '';
    growPrompt(promptEl); // shrink back; programmatic clear fires no input event
    // Optimistic: show the user message immediately. The live bubble is
    // created by openEventStream on subprocess.started. Marked .optimistic so
    // the turn.user event (or a truth-up fetch) replaces it with the
    // canonical turn — same text, server timestamp.
    const userNode = appendChatMessage({ role: 'user', content: text });
    if (userNode) userNode.classList.add('optimistic');
    try {
      const res = await fetch('/api/sessions/' + encodeURIComponent(id) + '/send', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ text }),
      });
      if (!res.ok) {
        const err = await res.json().catch(() => ({}));
        appendChatMessage({ role: 'error', content: 'send failed: ' + (err.error || ('HTTP ' + res.status)), ts: new Date().toISOString() });
        sendBtn.disabled = false;
        return;
      }
      // button stays disabled until subprocess.exit handler re-enables
    } catch (e) {
      appendChatMessage({ role: 'error', content: String(e), ts: new Date().toISOString() });
      sendBtn.disabled = false;
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

// statusDot renders the sidebar run-state indicator: a dim green dot when
// usher holds a warm-but-idle process ("live"), and a brighter pulsing dot
// while a turn is executing ("running"). Idle/undiscovered sessions get none.
function statusDot(status) {
  if (status === 'running') return '<span class="running-dot executing" title="executing">●</span>';
  if (status === 'live') return '<span class="running-dot" title="process live">●</span>';
  return '';
}

// openEventStream attaches SSE handlers to /api/sessions/{id}/events. The
// in-flight assistant turn renders inline at the bottom of the transcript:
// subprocess.started stands up the live bubble; each 'part' event (one
// server-grouped TurnPart — assistant text or a rendered tool result, the
// same shapes /transcript serves) appends into it as it happens; 'turn.user'
// adopts our optimistic echo as the canonical user turn; subprocess.exit
// promotes the live bubble in place (full fetch only when the turn is
// dirty — see finalizeTurn). Turn errors surface via the 'error' event.
function openEventStream(id, chatEl, sendBtn, cancelBtn, getTermMode) {
  const es = new EventSource('/api/sessions/' + encodeURIComponent(id) + '/events');
  currentES = es;

  let opened = false;
  // auto mode: while a turn is live, mirror the pane into a dedicated node at
  // the bottom of the live bubble, below the streamed parts. It runs the
  // WHOLE turn — covering every gap incl. tool execution — and is removed at
  // turn end. Streamed parts insert above it independently, so there's no
  // handoff and no contention over one element. Tear down only our own feed:
  // if the user switched to on mid-turn, the docked panel took over
  // currentScreenES.
  let inlineES = null;
  let inlineNode = null;
  const stopInlineMirror = () => {
    if (inlineES) {
      if (inlineES === currentScreenES) closeScreenES();
      inlineES = null;
    }
    if (inlineNode) { inlineNode.remove(); inlineNode = null; }
  };
  // syncInline reconciles the mirror with mode + turn state: it runs in auto
  // whenever a turn is live (live bubble present), torn down otherwise. Called
  // on turn start AND on every mode toggle, so switching to auto mid-turn
  // brings the live view back (e.g. on -> auto, where the docked panel just
  // closed). The node docks at the bottom of the live bubble; appendLivePart
  // inserts streamed parts above it.
  const syncInline = () => {
    if (getTermMode && getTermMode() === 'auto' && liveTurn) {
      if (!inlineES) {
        inlineNode = document.createElement('pre');
        inlineNode.className = 'term-inline';
        liveTurn.node.appendChild(inlineNode);
        inlineES = openScreenInline(id, inlineNode, chatEl);
      }
    } else {
      stopInlineMirror();
    }
  };
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
    sendBtn.disabled = false;
    if (cancelBtn) cancelBtn.hidden = true;
    setLoadEarlierDisabled(false);
  };
  const onRunning = () => {
    detailStreaming = true;
    sendBtn.disabled = true;
    if (cancelBtn) cancelBtn.hidden = false;
    setLoadEarlierDisabled(true);
  };
  // beginTurn stands up the live bubble + running-state UI + auto preview for
  // a turn. It's the single idempotent entry point for every way a turn
  // surfaces: subprocess.started (live), turn.active (server snapshot on a
  // mid-turn connect), and the lazy first-part fallback. The guard makes it
  // safe when two of those race (e.g. connecting in the window between the
  // session flipping to running and subprocess.started being published).
  const beginTurn = () => {
    if (liveTurn) return; // already tracking a turn
    ensureLiveTurn();
    syncInline();
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
    // a full fetch. The mount-time loadTranscript already shows the turn's
    // earlier parts as a committed partial turn; only the live bubble starts
    // from now.
    'turn.active': () => {
      beginTurn();
      liveTurnDirty = true;
    },
    // Counterpart to turn.active: the server says no turn is running. If we still
    // think one is — our subprocess.exit was dropped on a broken connection and
    // the turn ended before we reconnected — finalize now, else send stays
    // disabled and the preview streams on forever. No-op on a normal idle connect
    // (detailStreaming already false).
    'turn.idle': () => {
      if (!detailStreaming) return;
      stopInlineMirror();
      liveTurn = null;
      liveTurnDirty = false; // the full fetch below reconciles everything
      onIdle();
      loadTranscript(id);
    },
    'subprocess.started': () => beginTurn(),
    // One server-grouped TurnPart (assistant text or a rendered tool result)
    // appended to the in-progress turn. subprocess.started / turn.active
    // normally create the bubble, but if both were missed stand it up now —
    // via beginTurn so the auto preview is wired too, not just a bubble.
    'part': (d) => {
      if (!liveTurn) beginTurn();
      appendLivePart(d);
    },
    // The canonical user prompt hit the jsonl. Adopt our optimistic echo
    // (stamp the persisted ts, commit it). No echo means the prompt came
    // from elsewhere (mid-turn steering, another frontend) — server-side it
    // closed the in-progress assistant turn, which this client didn't
    // witness, so mark the turn dirty and let the turn-end full fetch
    // render everything in canonical order.
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
      liveTurnDirty = true;
    },
    'subprocess.exit': (d) => {
      stopInlineMirror();
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
      stopInlineMirror();
      const msg = d.message || JSON.stringify(d);
      if (liveTurn) {
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
  return { syncInline };
}

// ---------- Terminal mirror (read-only) ----------
//
// A1 escape hatch: an inline, collapsible panel above the chat input that
// mirrors the session's live tmux pane (claude's TUI) as a periodically
// re-captured frame over SSE, plus a row of soft keys so the user can drive
// menus the curated send path can't reach (arrow navigation, esc, ctrl-c).
// Read-only otherwise — no arbitrary typing; that's what chat is for.

// openScreenFeed is the shared /screen subscription: it invokes onFrame(text)
// for each changed (deduped) capture and onNopane when usher holds no live
// window. Tracked in currentScreenES so a new open or closeScreenES() tears it
// down. Both the docked mirror (on) and the inline auto preview build on it.
function openScreenFeed(id, cols, rows, onFrame, onNopane) {
  closeScreenES();
  const params = [];
  if (cols) params.push('cols=' + cols);
  if (rows) params.push('rows=' + rows);
  const q = params.length ? ('?' + params.join('&')) : '';
  const es = new EventSource('/api/sessions/' + encodeURIComponent(id) + '/screen' + q);
  currentScreenES = es;
  let lastRaw = null;
  es.addEventListener('screen', (ev) => {
    if (ev.data == null) return;
    let s;
    try { s = JSON.parse(ev.data); } catch { return; }
    if (s === lastRaw) return;
    lastRaw = s;
    onFrame(s);
  });
  if (onNopane) es.addEventListener('nopane', () => { lastRaw = null; onNopane(); });
  es.onerror = () => {/* SSE auto-reconnects; no user-visible noise */};
  return es;
}

// openScreenStream drives the docked on-mode panel: it mirrors the whole pane
// (keys + furniture) into screenEl, pinning to the bottom unless the reader has
// scrolled up.
function openScreenStream(id, screenEl, cols, rows, dropTail) {
  openScreenFeed(id, cols, rows, (s) => {
    const wrap = screenEl.parentElement;
    const atBottom = wrap ? (wrap.scrollHeight - wrap.scrollTop - wrap.clientHeight < 40) : true;
    screenEl.classList.remove('muted');
    screenEl.innerHTML = ansiToHtml(trimMirrorFrame(s, dropTail));
    if (atBottom && wrap) wrap.scrollTop = wrap.scrollHeight;
  }, () => {
    screenEl.classList.add('muted');
    screenEl.textContent =
      'no live process for this session — open chat and send a message to start one.';
  });
}

// openScreenInline (auto mode) streams the live pane into a dedicated <pre> node
// sitting below the assistant message text for the duration of the turn — a live
// peek at the terminal alongside the (possibly still-empty) jsonl text. Read-only
// — furniture rows trimmed, no keys.
function openScreenInline(id, node, chatEl) {
  return openScreenFeed(id, measureCols(node), TERM_AUTO_ROWS, (s) => {
    const stick = isNearBottom(chatEl);
    node.innerHTML = ansiToHtml(trimMirrorFrame(s, TERM_FURNITURE_ROWS));
    if (stick && chatEl) chatEl.scrollTop = chatEl.scrollHeight;
  });
}

// wireSoftKeys POSTs the tapped key to /keys. The server allow-lists key names;
// the screen stream reflects the result, so we don't render an ack — only a
// brief red flash if the key was rejected (e.g. the pane went away).
function wireSoftKeys(id) {
  document.querySelectorAll('.term-keys button[data-key]').forEach((btn) => {
    btn.addEventListener('click', async () => {
      try {
        const res = await fetch('/api/sessions/' + encodeURIComponent(id) + '/keys', {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ key: btn.dataset.key }),
        });
        if (!res.ok) {
          btn.classList.add('term-key-err');
          setTimeout(() => btn.classList.remove('term-key-err'), 500);
        }
      } catch {/* transient; the next tap or frame recovers */}
    });
  });
}

// measureCols turns the screen box's available width into terminal columns, so
// the tmux pane can be sized to fit the viewer (measured once, on expand). A
// hidden ruler in the grid's font gives an accurate per-char width; the server
// clamps the result to a sane range.
function measureCols(boxEl) {
  const ruler = document.createElement('span');
  ruler.textContent = '0'.repeat(100);
  ruler.style.cssText =
    'position:absolute;visibility:hidden;white-space:pre;' +
    'font-size:13px;font-family:var(--term-font)'; // match the grid's font, or cols mis-measure
  document.body.appendChild(ruler);
  const charPx = ruler.getBoundingClientRect().width / 100;
  ruler.remove();
  if (!charPx || !boxEl) return 80;
  // clientWidth includes the side padding; trim a little so a full-width line
  // doesn't trip horizontal scroll.
  const cols = Math.floor((boxEl.clientWidth - 24) / charPx);
  return cols > 0 ? cols : 80;
}

// trimMirrorFrame cleans a capture-pane frame for the read-only mirror: it drops
// the trailing blank pad, then the bottom `dropTail` rows — the input box + hint
// line, which are furniture, not output (auto hides them; on passes 0 to keep
// them for debugging / rewind). Blankness is tested after stripping SGR escapes
// so a colour-only-but-empty line still counts.
function trimMirrorFrame(s, dropTail) {
  const lines = String(s).split('\n');
  while (lines.length &&
         lines[lines.length - 1].replace(/\x1b\[[0-9;]*m/g, '').trim() === '') {
    lines.pop();
  }
  return (dropTail ? lines.slice(0, -dropTail) : lines).join('\n');
}

// Base 16-colour ANSI palette (30-37 normal, 90-97 bright), toned to read on
// the dark terminal background.
const ANSI_COLORS = [
  '#484f58', '#ff7b72', '#3fb950', '#d29922', '#58a6ff', '#bc8cff', '#39c5cf', '#b1bac4',
  '#6e7681', '#ffa198', '#56d364', '#e3b341', '#79c0ff', '#d2a8ff', '#56d4dd', '#f0f6fc',
];
const TERM_BG = '#0d1117';
const TERM_FG = '#c9d1d9';

// ansiToHtml converts a `capture-pane -e` frame (plain text + SGR colour
// escapes) into HTML spans. capture-pane -e emits only SGR (`ESC [ … m`), so
// that's all we parse: bold/dim/italic/underline/inverse, the 16 base colours,
// 256-colour, and truecolour. Inverse swaps fg/bg — that's how a TUI paints its
// selected menu row, the whole reason this mirror exists.
function ansiToHtml(s) {
  const str = String(s);
  let fg = null, bg = null, bold = false, dim = false, ital = false, ul = false, inv = false;
  let out = '', open = false;
  const color256 = (n) => {
    if (n < 16) return ANSI_COLORS[n];
    if (n >= 232) { const v = 8 + (n - 232) * 10; return `rgb(${v},${v},${v})`; }
    n -= 16;
    const r = Math.floor(n / 36), g = Math.floor((n % 36) / 6), b = n % 6;
    const c = (x) => (x === 0 ? 0 : 55 + x * 40);
    return `rgb(${c(r)},${c(g)},${c(b)})`;
  };
  const closeSpan = () => { if (open) { out += '</span>'; open = false; } };
  const openSpan = () => {
    let f = fg, b = bg;
    if (inv) { f = bg === null ? TERM_BG : bg; b = fg === null ? TERM_FG : fg; }
    const st = [];
    if (f !== null) st.push('color:' + f);
    if (b !== null) st.push('background:' + b);
    if (bold) st.push('font-weight:600');
    if (dim) st.push('opacity:0.6');
    if (ital) st.push('font-style:italic');
    if (ul) st.push('text-decoration:underline');
    if (!st.length) return;
    out += '<span style="' + st.join(';') + '">';
    open = true;
  };
  const esc1 = (c) => (c === '&' ? '&amp;' : c === '<' ? '&lt;' : c === '>' ? '&gt;' : c);
  let i = 0;
  while (i < str.length) {
    const ch = str[i];
    if (ch === '\x1b' && str[i + 1] === '[') {
      const m = /^\x1b\[([0-9;]*)m/.exec(str.slice(i));
      if (m) {
        closeSpan();
        const ps = m[1] === '' ? [0] : m[1].split(';').map(Number);
        for (let k = 0; k < ps.length; k++) {
          const p = ps[k];
          if (p === 0) { fg = bg = null; bold = dim = ital = ul = inv = false; }
          else if (p === 1) bold = true;
          else if (p === 2) dim = true;
          else if (p === 3) ital = true;
          else if (p === 4) ul = true;
          else if (p === 7) inv = true;
          else if (p === 22) bold = dim = false;
          else if (p === 23) ital = false;
          else if (p === 24) ul = false;
          else if (p === 27) inv = false;
          else if (p >= 30 && p <= 37) fg = ANSI_COLORS[p - 30];
          else if (p >= 40 && p <= 47) bg = ANSI_COLORS[p - 40];
          else if (p >= 90 && p <= 97) fg = ANSI_COLORS[p - 90 + 8];
          else if (p >= 100 && p <= 107) bg = ANSI_COLORS[p - 100 + 8];
          else if (p === 39) fg = null;
          else if (p === 49) bg = null;
          else if (p === 38 || p === 48) {
            const mode = ps[k + 1];
            if (mode === 5) { const col = color256(ps[k + 2]); if (p === 38) fg = col; else bg = col; k += 2; }
            else if (mode === 2) { const col = `rgb(${ps[k + 2] || 0},${ps[k + 3] || 0},${ps[k + 4] || 0})`; if (p === 38) fg = col; else bg = col; k += 4; }
          }
        }
        openSpan();
        i += m[0].length;
        continue;
      }
    }
    if (ch === '\x1b') { i++; continue; } // drop a stray/unrecognised escape
    out += esc1(ch);
    i++;
  }
  closeSpan();
  return out;
}

// ---------- Main chat view ----------

async function showMainChat(id) {
  clearListInterval();
  currentDetailId = null;
  closeES();
  subtitle.innerHTML = `<span class="subtitle-left"><strong class="session-title">main chat</strong></span>`;

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

  await loadMainChatInfo(id);
  await loadChatMessages(id);

  const submit = async () => {
    const text = promptEl.value;
    if (!text.trim() || sendBtn.disabled) return;
    sendBtn.disabled = true;
    promptEl.value = '';
    growPrompt(promptEl); // shrink back; programmatic clear fires no input event
    // Optimistic: show the user's message immediately and a "thinking" placeholder
    // since LLM agents may take 5–30s before any response comes back.
    const userNode = appendChatMessage({ role: 'user', content: text });
    const placeholder = appendChatMessage({ role: 'agent', content: 'thinking…', _placeholder: true });
    try {
      const res = await fetch('/api/mainchats/' + encodeURIComponent(id) + '/send', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ text }),
      });
      const data = await res.json();
      if (placeholder) placeholder.remove();
      if (!res.ok) {
        appendChatMessage({ role: 'error', content: data.error || 'send failed', ts: new Date().toISOString() });
      } else {
        // Server returns the persisted user+agent pair. Canonicalize the
        // optimistic user node's ts from the server, then render the agent
        // reply (which already carries its server ts via appendChatMessage).
        const msgs = data.messages || [];
        const serverUser = msgs.find(m => m.role === 'user');
        if (serverUser && serverUser.ts) updateMessageTs(userNode, serverUser.ts);
        for (const m of msgs.filter(m => m.role !== 'user')) appendChatMessage(m);
        // Focus may have shifted this turn — update the header.
        renderFocus(data.focus);
      }
    } catch (e) {
      if (placeholder) placeholder.remove();
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
  subtitle.innerHTML =
    `<span class="subtitle-left">` +
      `<strong class="session-title">${esc(sess.title || '(untitled)')}</strong>` +
    `</span>` +
    `<span class="session-id">${esc(sess.id.slice(0, 8))}</span>` +
    `<span class="session-cwd">${esc(sess.cwd || '')}</span>`;
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
  } catch {/* ignore */}
}

// ----- live turn (streamed parts) -----

// ensureLiveTurn stands up the optimistic assistant bubble the streamed parts
// render into. Parts-shaped from birth (empty parts array → role header only)
// so it matches the structure of a committed turn.
function ensureLiveTurn() {
  if (liveTurn) return liveTurn;
  const node = appendChatMessage({ role: 'assistant', parts: [] });
  if (!node) return null;
  node.classList.add('optimistic');
  liveTurn = { node, parts: [], ts: '' };
  return liveTurn;
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
  lt.parts.push(p);
  if (!lt.ts && d.ts) lt.ts = d.ts;
  const chat = document.getElementById('chat-scroll');
  const stick = chat && isNearBottom(chat);
  if (d.model) {
    const roleEl = lt.node.querySelector('.role');
    if (roleEl) roleEl.title = d.model;
  }
  const tmpl = document.createElement('template');
  tmpl.innerHTML = p.type === 'tool'
    ? renderToolPart(p)
    : `<div class="content" data-raw="${esc(p.content || '')}">${renderMarkdown(p.content || '')}</div>`;
  // The auto-mode inline mirror docks at the bottom of the bubble; parts stay
  // above it. insertBefore(_, null) is plain append when there's no mirror.
  lt.node.insertBefore(tmpl.content, lt.node.querySelector(':scope > .term-inline'));
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
    suppressAppendScroll = true;
    try {
      for (let i = lcp; i < turns.length; i++) {
        const node = appendChatMessage(turns[i]);
        if (node) renderedTurns.push({ key: newKeys[i], node });
      }
    } finally {
      suppressAppendScroll = false; // never leave it stuck, or future appends won't scroll
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

async function loadChatMessages(id) {
  try {
    const res = await fetch('/api/mainchats/' + encodeURIComponent(id) + '/messages');
    if (!res.ok) return;
    const data = (await res.json()) || [];
    const list = document.getElementById('chat-scroll');
    if (!list) return;
    list.querySelectorAll(':scope > .chat-loading, :scope > .chat-message').forEach(n => n.remove());
    suppressAppendScroll = true;
    for (const m of data) appendChatMessage(m);
    suppressAppendScroll = false;
    list.scrollTop = list.scrollHeight; // fresh main-chat load lands at the bottom
  } catch { suppressAppendScroll = false; }
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
  const left = `<span class="subtitle-left"><strong class="session-title">main chat</strong></span>`;
  if (!focus || !focus.session_id) {
    subtitle.innerHTML = left;
    return;
  }
  const sid = esc(focus.session_id.slice(0, 8));
  subtitle.innerHTML =
    left +
    `<a href="#/s/${esc(focus.session_id)}" class="subtitle-focus">focus: ${sid}</a>`;
}

// ---------- Rendering turns ----------

// renderToolPart renders a single tool part as a collapsible <details> element.
// Edit/Write expand by default; others collapse.
function renderToolPart(p) {
  const name = p.toolName || 'tool';
  const target = p.toolTarget || '';
  const expandByDefault = /^(Edit|Write)$/i.test(name);
  const openAttr = expandByDefault ? ' open' : '';
  const label = target ? esc(name) + ' <span class="tool-target">' + esc(target) + '</span>' : esc(name);
  return `<details class="tool-details"${openAttr}>` +
    `<summary>${label}</summary>` +
    `<div class="tool-body" data-raw="${esc(p.content || '')}">${renderMarkdown(p.content || '')}</div>` +
    `</details>`;
}

// forkBtnHTML is the fork control at the foot of a completed assistant turn —
// the fork keeps everything above the divider and drops everything below (on
// the last turn: fork the current state). data-uuid is the fork point the
// server expects; clicks are handled by a document-level delegate.
function forkBtnHTML(uuid) {
  return `<button class="turn-fork" type="button" data-uuid="${esc(uuid)}" title="Fork: branch a new session from this point">⑂ fork</button>`;
}

// renderAssistantParts renders the parts array of a grouped assistant turn.
function renderAssistantParts(parts) {
  return (parts || []).map(p => {
    if (p.type === 'tool') return renderToolPart(p);
    return `<div class="content" data-raw="${esc(p.content || '')}">${renderMarkdown(p.content || '')}</div>`;
  }).join('');
}

function appendChatMessage(m) {
  const list = document.getElementById('chat-scroll');
  if (!list) return null;
  // Decide stick-to-bottom BEFORE inserting — the insert changes scrollHeight.
  const stick = !suppressAppendScroll && isNearBottom(list);
  const div = document.createElement('div');
  const role = m.role || 'agent';
  div.className = 'chat-message ' + role + (m._placeholder ? ' placeholder' : '');
  const ts = m.ts ? `<span class="ts">${esc(new Date(m.ts).toLocaleString())}</span>` : '';
  const modelAttr = m.model ? ` title="${esc(m.model)}"` : '';

  if (role === 'assistant' && m.parts) {
    // Grouped assistant turn: role header + structured parts. An EMPTY parts
    // array is the live-turn shell — header only; streamed parts append into
    // it (a flat .content div here would misrender the first tool part).
    // Completed turns close with the fork control at the card's bottom edge.
    div.innerHTML =
      `<div class="role"${modelAttr}>${esc(role)}${ts}</div>` +
      renderAssistantParts(m.parts) +
      (m.uuid ? forkBtnHTML(m.uuid) : '');
  } else {
    // User, error, agent, or streaming placeholder (flat content).
    div.innerHTML =
      `<div class="role"${modelAttr}>${esc(role)}${ts}</div>` +
      `<div class="content" data-raw="${esc(m.content || '')}">${renderMarkdown(m.content || '')}</div>`;
  }
  // send-anchor lives inside chat-scroll (sticky at bottom). Insert
  // new messages before it so it stays the last child.
  const sendAnchor = list.querySelector(':scope > .send-anchor');
  if (sendAnchor) list.insertBefore(div, sendAnchor);
  else list.appendChild(div);
  if (stick) list.scrollTop = list.scrollHeight;
  return div;
}

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

// updateMessageTs sets (or inserts) the timestamp span on an existing
// chat-message node. Used to canonicalize optimistic messages once the
// server returns the persisted ts.
function updateMessageTs(node, ts) {
  if (!node || !ts) return;
  const roleEl = node.querySelector('.role');
  if (!roleEl) return;
  let span = roleEl.querySelector('.ts');
  if (!span) {
    span = document.createElement('span');
    span.className = 'ts';
    roleEl.appendChild(span);
  }
  span.textContent = new Date(ts).toLocaleString();
}

// ---------- Permission-request modal (global, runs in all views) ----------

let pendingInteractions = [];

async function pollInteractions() {
  try {
    const res = await fetch('/api/interactions');
    if (!res.ok) return;
    const list = (await res.json()) || [];
    if (!sameInteractions(pendingInteractions, list)) {
      pendingInteractions = list;
      renderInteractions();
    }
  } catch {/* server may be down briefly */}
}

function sameInteractions(a, b) {
  if (a.length !== b.length) return false;
  for (let i = 0; i < a.length; i++) if (a[i].id !== b[i].id) return false;
  return true;
}

// AskUserQuestion surfaces as a pending interaction whose tool_input carries
// the questions + options. We render it as a choice picker (single-select per
// question) instead of allow/deny; the picked labels go back as `answers`,
// which the server feeds into the tool's updatedInput so claude resolves it
// without ever rendering its pane TUI selector. (multiSelect / free-text are
// not handled yet — only the listed options.)
function isAskQuestion(p) {
  return p.tool_name === 'AskUserQuestion'
    && p.tool_input && Array.isArray(p.tool_input.questions) && p.tool_input.questions.length > 0;
}

// previewBlock renders an option's optional `preview` as raw monospace text;
// a tall preview just scrolls inside its box. Deliberately tiny — `preview` is
// a rare field and not worth more than this.
function previewBlock(preview) {
  return preview ? `<span class="qopt-preview">${esc(preview)}</span>` : '';
}

function renderAskQuestion(p, sid) {
  const blocks = p.tool_input.questions.map((q, qi) => {
    const opts = (q.options || []).map((o, oi) => `
      <button class="qopt" data-qi="${qi}" data-oi="${oi}">
        <span class="qopt-label">${esc(o.label || '')}</span>
        ${o.description ? `<span class="qopt-desc">${esc(o.description)}</span>` : ''}
        ${previewBlock(o.preview)}
      </button>`).join('');
    return `
      <div class="question">
        ${q.header ? `<div class="q-header">${esc(q.header)}</div>` : ''}
        <div class="q-text">${esc(q.question || '')}${q.multiSelect ? ' <span class="q-multi">(select all that apply)</span>' : ''}</div>
        <div class="q-options">${opts}</div>
        <input class="qother" data-qi="${qi}" type="text" placeholder="or type your own answer…">
      </div>`;
  }).join('');
  return `
    <div class="interaction ask" data-id="${esc(p.id)}">
      <div class="meta"><strong>question</strong><span class="muted">session ${esc(sid)}</span></div>
      ${blocks}
      <div class="actions">
        <button class="qignore">ignore</button>
        <button class="qsubmit" disabled>answer</button>
      </div>
    </div>`;
}

function renderPermission(p, sid) {
  let inputJSON = '';
  try { inputJSON = JSON.stringify(p.tool_input || {}, null, 2); }
  catch { inputJSON = String(p.tool_input || ''); }
  const matcher = deriveMatcherPreview(p.tool_name, p.tool_input);
  return `
    <div class="interaction" data-id="${esc(p.id)}">
      <div class="meta">
        <strong>${esc(p.tool_name || p.event)}</strong>
        <span class="muted">session ${esc(sid)}</span>
      </div>
      <pre class="tool-input">${esc(inputJSON)}</pre>
      <div class="actions">
        <button class="allow primary" data-scope="once">allow</button>
        <button class="allow secondary" data-scope="session" title="auto-allow ${esc(matcher)} for this session">allow always</button>
        <button class="deny secondary" data-scope="session" title="auto-deny ${esc(matcher)} for this session">deny always</button>
        <button class="deny primary" data-scope="once">deny</button>
      </div>
    </div>`;
}

// wireAskQuestion drives the choice picker. Each question takes an answer that
// is one of: a listed option, several options (when multiSelect), or free text
// (the always-available "Other", matching the native tool) — picking options
// clears the free text and vice-versa. multiSelect picks are joined with ", "
// (the format the native tool emits). The answer button enables once every
// question has an answer; "ignore" denies the tool, which claude treats as
// "skip the question, continue in chat". Answers go back as question -> string
// in one response.
function wireAskQuestion(node, id) {
  const p = pendingInteractions.find(x => x.id === id);
  if (!p) return;
  const qs = p.tool_input.questions;
  const submit = node.querySelector('.qsubmit');
  const otherOf = qi => node.querySelector(`.qother[data-qi="${qi}"]`);
  const answerOf = qi => {
    const typed = otherOf(qi).value.trim();
    if (typed) return typed;
    // Join all selected labels — a single-select question has at most one.
    return [...node.querySelectorAll(`.qopt.selected[data-qi="${qi}"]`)]
      .map(s => (qs[+qi].options[+s.dataset.oi] || {}).label || '').join(', ');
  };
  const recompute = () => {
    submit.disabled = qs.some((_, qi) => !answerOf(qi));
  };
  node.querySelectorAll('.qopt').forEach(btn => {
    btn.addEventListener('click', () => {
      const qi = btn.dataset.qi;
      if (qs[+qi].multiSelect) {
        btn.classList.toggle('selected'); // multi: toggle, leave others
      } else {
        node.querySelectorAll(`.qopt[data-qi="${qi}"]`).forEach(b => b.classList.remove('selected'));
        btn.classList.add('selected'); // single: radio
      }
      otherOf(qi).value = ''; // picking an option clears its free-text
      recompute();
    });
  });
  node.querySelectorAll('.qother').forEach(inp => {
    inp.addEventListener('input', () => {
      if (inp.value.trim()) { // typing clears the radio selection for that question
        node.querySelectorAll(`.qopt[data-qi="${inp.dataset.qi}"]`).forEach(b => b.classList.remove('selected'));
      }
      recompute();
    });
  });
  node.querySelector('.qignore').addEventListener('click',
    () => respond(id, 'deny', 'once', 'The user declined to answer; continue in the conversation.'));
  submit.addEventListener('click', () => {
    const answers = {};
    qs.forEach((q, qi) => { answers[q.question] = answerOf(qi); });
    respondAnswers(id, answers);
  });
}

function renderInteractions() {
  let modal = document.getElementById('modal');
  if (!pendingInteractions.length) {
    if (modal) modal.remove();
    return;
  }
  if (!modal) {
    modal = document.createElement('div');
    modal.id = 'modal';
    document.body.appendChild(modal);
  }
  const items = pendingInteractions.map(p => {
    const sid = (p.session_id || '').slice(0, 8) || '(unknown)';
    return isAskQuestion(p) ? renderAskQuestion(p, sid) : renderPermission(p, sid);
  }).join('');
  modal.innerHTML = `
    <div class="overlay"></div>
    <div class="dialog">
      <h3>pending requests (${pendingInteractions.length})</h3>
      ${items}
    </div>
  `;
  modal.querySelectorAll('.interaction').forEach(node => {
    const id = node.dataset.id;
    if (node.classList.contains('ask')) {
      wireAskQuestion(node, id);
      return;
    }
    node.querySelectorAll('button.allow,button.deny').forEach(btn => {
      btn.addEventListener('click', () => {
        const behavior = btn.classList.contains('allow') ? 'allow' : 'deny';
        const scope = btn.dataset.scope || 'once';
        respond(id, behavior, scope);
      });
    });
  });
}

async function respond(id, behavior, scope, reason) {
  try {
    await fetch('/api/interactions/' + encodeURIComponent(id) + '/respond', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ behavior, scope: scope || 'once', reason: reason || 'via usher web UI' }),
    });
  } catch (e) {
    console.error('respond', e);
  }
  pollInteractions();
}

// respondAnswers resolves an AskUserQuestion interaction: behavior "allow"
// plus the chosen labels (question → label), which the server merges into the
// tool's updatedInput.
async function respondAnswers(id, answers) {
  try {
    await fetch('/api/interactions/' + encodeURIComponent(id) + '/respond', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ behavior: 'allow', answers, reason: 'via usher web UI' }),
    });
  } catch (e) {
    console.error('respond answers', e);
  }
  pollInteractions();
}

// Mirror of internal/hook/hook.go deriveMatcher — used purely for the
// tooltip preview ("auto-allow Bash(git:*)" etc). Server is authoritative.
function deriveMatcherPreview(toolName, toolInput) {
  if (!toolName) return '(unknown)';
  if (toolName === 'Bash' && toolInput && typeof toolInput.command === 'string') {
    const cmd = toolInput.command.trim();
    if (cmd) return 'Bash(' + cmd.split(/\s+/, 1)[0] + ':*)';
  }
  return toolName;
}

setInterval(pollInteractions, 2000);
pollInteractions();

setInterval(loadSidebar, 5000);
loadSidebar();

route();

// PWA: register the service worker (installable + offline shell; caching
// strategy and the /api + SSE bypass live in sw.js). Best-effort — a failed
// registration just means no offline/install.
if ('serviceWorker' in navigator) {
  window.addEventListener('load', () => {
    navigator.serviceWorker.register('/sw.js').catch((err) =>
      console.warn('service worker registration failed', err));
  });
}
