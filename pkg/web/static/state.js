// usher SPA: shared mutable state, tiny utilities, and cross-module helpers.
//
// Only state that is read or written by MORE THAN ONE module lives here.
// Module-private state stays in its own file (detail.js, sidebar.js, etc.).

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

// --- DOM anchors (used by multiple views) ---
export const root = document.getElementById('root');
export const subtitle = document.getElementById('subtitle');
export const renderPillMd = document.getElementById('render-pill-md');
export const renderPillRaw = document.getElementById('render-pill-raw');

// --- cross-module mutable state ---

export let listInterval = null;
export function setListInterval(v) { listInterval = v; }

export let currentES = null;
export function setCurrentES(v) { currentES = v; }

// Conversation and terminal streams can be open together.
export let currentTerminalES = null;
export function setCurrentTerminalES(v) { currentTerminalES = v; }

// detail.js owns all transcript/live-turn state but needs to expose
// currentDetailId for the fork delegate and refreshSubtitle guard, and
// detailStreaming / suppressAppendScroll which render.js reads.
export let currentDetailId = null;
export function setCurrentDetailId(v) { currentDetailId = v; }

export let suppressAppendScroll = false;
export function setSuppressAppendScroll(v) { suppressAppendScroll = v; }

// --editor-url template from GET /internal/config ("" = no editor configured).
// Fetched once at boot by app.js; read by the kebab popover.
export let editorUrl = '';
export function setEditorUrl(v) { editorUrl = v; }

// --- constants used by multiple modules ---

// Auto-scroll to the bottom on new content only when the user is already near
// it, so scrolling up to read history isn't yanked back down.
export const BOTTOM_THRESHOLD = 64;
export function isNearBottom(el) { return el.scrollHeight - el.scrollTop - el.clientHeight < BOTTOM_THRESHOLD; }

// --- unread tracking ------------------------------------------------------
// A session goes "unread" on a running -> settled transition (a turn finished)
// while unviewed — keyed off the status change, not last_event_at, so a long
// multi-round turn never flickers mid-flight. In-memory, clean on every load;
// the app-away case is push's job.
let viewingId = null;          // session whose detail is open
// Router sessions all run in one directory; lists show it last.
export let routerDir = '';
export function setRouterDir(v) { routerDir = v || ''; }

// Sessions the open conversation routed work to. Their permission requests
// and questions are answered from here too.
export let routedTargets = new Set();
export function setRoutedTargets(ids) { routedTargets = new Set(ids); }

let lastSessions = [];         // latest /api/v1/sessions payload — written by sidebar
export function getLastSessions() { return lastSessions; }
export function setLastSessions(v) { lastSessions = v; }
let pendingInteractionCounts = new Map(); // session id -> unresolved permission/question count
export function pendingInteractionCount(id) { return pendingInteractionCounts.get(id) || 0; }
export function setPendingInteractionCounts(v) {
  pendingInteractionCounts = v;
  if (lastSessions.length && _renderSidebarSessionsFn) _renderSidebarSessionsFn(lastSessions);
  updateTabBadge();
}
const prevStatus = {};         // id -> status at the previous poll
const unreadIds = new Set();   // sessions with an unseen finished turn

export function isUnread(s) {
  return unreadIds.has(s.id) && s.id !== viewingId && !s.archived;
}

// Mark unread on running -> settled; viewing/running/archived clears it. First
// poll has no prior status, so nothing is marked (clean slate on load).
export function reconcileUnread(allSessions) {
  for (const s of allSessions) {
    const cur = s.status, prev = prevStatus[s.id];
    if (s.id === viewingId || cur === 'running' || s.archived) {
      unreadIds.delete(s.id);
    } else if (prev === 'running') {
      unreadIds.add(s.id);
    }
    prevStatus[s.id] = cur;
  }
}

// Opening a session clears its unread and excludes it until the user leaves.
// NOTE: markViewing references renderSidebarSessions which lives in sidebar.js.
// To avoid a circular import, we use a late-bound callback that sidebar.js registers.
let _renderSidebarSessionsFn = null;
export function registerRenderSidebarSessions(fn) { _renderSidebarSessionsFn = fn; }

let _refreshSubtitleFn = null;
export function registerRefreshSubtitle(fn) { _refreshSubtitleFn = fn; }
export function refreshSubtitle(id) { if (_refreshSubtitleFn) _refreshSubtitleFn(id); }

export function markViewing(id) {
  viewingId = id;
  unreadIds.delete(id);
  if (lastSessions.length && _renderSidebarSessionsFn) _renderSidebarSessionsFn(lastSessions);
  updateTabBadge();
}
export function clearViewing() {
  viewingId = null;
  updateTabBadge();
}

// Count unique sessions that are unread or awaiting a permission/question.
export function updateTabBadge() {
  const attention = new Set(lastSessions.filter(isUnread).map(s => s.id));
  for (const id of pendingInteractionCounts.keys()) attention.add(id);
  const n = attention.size;
  document.title = n > 0 ? `(${n}) usher` : 'usher';
  if ('setAppBadge' in navigator) {
    if (n > 0) navigator.setAppBadge(n).catch(() => {});
    else if ('clearAppBadge' in navigator) navigator.clearAppBadge().catch(() => {});
  }
}

// --- render mode (shared between render.js writer and state reader) ---
export let renderMode = localStorage.getItem('usher.renderMode') === 'raw' ? 'raw' : 'md';
export function setRenderModeValue(v) { renderMode = v; }

// --- drafts (shared between detail.js writer and state's input listener) ---

// growPrompt sizes the textarea to its content (CSS min/max-height clamp it to
// 1–3 lines). The delegated input listener covers every view; call it directly
// after a programmatic clear, which fires no input event.
export function growPrompt(el) {
  if (!el) return;
  el.style.height = 'auto';
  el.style.height = el.scrollHeight + 'px';
}

// Per-session prompt drafts. Routing swaps innerHTML, so unsent text in #prompt
// would vanish on switch — stash it here keyed by the mounted view, restore on
// re-mount. In-memory only (survives switching, not refresh). currentDraftKey is
// null on views with no managed draft (list/new) so their #prompt can't clobber.
const promptDrafts = new Map();
export let currentDraftKey = null;
export function setCurrentDraftKey(v) { currentDraftKey = v; }

export function restoreDraft(promptEl) {
  if (!promptEl || !currentDraftKey) return;
  const d = promptDrafts.get(currentDraftKey);
  if (d) { promptEl.value = d; growPrompt(promptEl); }
}

export function clearDraft() {
  if (currentDraftKey) promptDrafts.delete(currentDraftKey);
}

document.addEventListener('input', (e) => {
  if (e.target && e.target.id === 'prompt') {
    growPrompt(e.target);
    if (currentDraftKey) promptDrafts.set(currentDraftKey, e.target.value);
  }
});

// Per-cwd archived-disclosure expansion state. Session-only — refresh
// collapses everything, matching the assumption that browsing archived
// sessions is a rare action.
export const cwdExpanded = new Set();
export const cwdArchivedVisible = new Map();

// --- utility functions ---

// fetchBackends returns {backends: [name], models: {name: [model]},
// approvals: {name: [approval]}}, or {}.
export async function fetchBackends() {
  const res = await fetch('/api/v1/backends');
  if (!res.ok) return {};
  const list = (await res.json()).backends || [];
  return {
    backends: list.map(b => b.name),
    models: Object.fromEntries(list.map(b => [b.name, b.models])),
    approvals: Object.fromEntries(list.map(b => [b.name, b.approvals || []])),
  };
}

// Who answers the agent's approval requests under each approval, in menu
// order.
export const APPROVALS = {
  ask: 'you decide each request',
  auto: 'a reviewer model decides most requests',
  full: 'every request is allowed',
};

// approvalToggleHTML is the composer's approval control: a button that opens
// its menu (see wireApprovalToggle).
export function approvalToggleHTML(approval, hidden) {
  return `<div class="approval-wrap"${hidden ? ' hidden' : ''}>
    <button class="approval-toggle" type="button" aria-haspopup="menu" aria-expanded="false"
      data-approval="${esc(approval)}" title="who answers the agent’s approval requests">
      <span class="t-icon">ϟ</span><span class="t-full">approve:</span><span class="toggle-val">${esc(approval)}</span>
    </button>
    <div class="composer-config-menu approval-menu" role="menu" hidden></div>
  </div>`;
}

export function setApprovalToggle(wrap, approval) {
  const btn = wrap.querySelector('.approval-toggle');
  btn.dataset.approval = approval;
  btn.querySelector('.toggle-val').textContent = approval;
}

// wireApprovalToggle lists choices() in the menu; pick(approval) resolves true
// once the approval is taken, which the toggle then shows.
export function wireApprovalToggle(wrap, choices, pick) {
  const btn = wrap.querySelector('.approval-toggle');
  const menu = wrap.querySelector('.approval-menu');
  const close = () => {
    menu.hidden = true;
    btn.setAttribute('aria-expanded', 'false');
    document.removeEventListener('click', onDoc);
  };
  const onDoc = e => { if (!wrap.contains(e.target)) close(); };
  btn.addEventListener('click', () => {
    if (!menu.hidden) { close(); return; }
    const current = btn.dataset.approval;
    const list = choices();
    menu.innerHTML = '<div class="config-menu-body">' +
      Object.keys(APPROVALS).filter(a => list.includes(a) || a === current).map(a =>
        `<button type="button" class="config-row" data-approval="${a}">` +
        `<span><span class="approval-name">${a}</span><span class="approval-desc">${esc(APPROVALS[a])}</span></span>` +
        `<span class="config-check">${a === current ? '✓' : ''}</span></button>`).join('') + '</div>';
    menu.hidden = false;
    btn.setAttribute('aria-expanded', 'true');
    document.addEventListener('click', onDoc);
  });
  menu.addEventListener('click', async e => {
    const row = e.target.closest('button[data-approval]');
    if (!row) return;
    close();
    const approval = row.dataset.approval;
    if (approval === btn.dataset.approval) return;
    btn.disabled = true;
    try {
      if (await pick(approval)) setApprovalToggle(wrap, approval);
    } finally {
      btn.disabled = false;
    }
  });
}

// patchSession sets the fields of PATCH /api/v1/sessions/{id}.
export function patchSession(id, fields) {
  return fetch('/api/v1/sessions/' + encodeURIComponent(id), {
    method: 'PATCH',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(fields),
  });
}

// navigator.clipboard needs a secure context; plain-HTTP deployments (e.g. a
// raw tailnet IP) fall back to execCommand. Both require a user gesture.
export function copyText(s) {
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

// formatArgs and parseArgs move CLI flags between an argv array and one line
// of shell-style text, so empty arguments and ones with spaces survive the
// round trip: ['--tools', ''] is `--tools ''`.
export function formatArgs(args) {
  return (args || []).map(a =>
    /^[\w@%+=:,./-]+$/.test(a) ? a : `'${a.replace(/'/g, `'\\''`)}'`
  ).join(' ');
}

// parseArgs takes single quotes literally; in double quotes and bare text a
// backslash escapes the next character. It throws on an unclosed quote.
export function parseArgs(text) {
  const args = [];
  let cur = '', inArg = false, quote = '';
  for (let i = 0; i < text.length; i++) {
    const c = text[i];
    if (quote === "'") {
      if (c === "'") quote = ''; else cur += c;
    } else if (c === '\\' && i + 1 < text.length && (!quote || '"\\'.includes(text[i + 1]))) {
      cur += text[++i];
      inArg = true;
    } else if (quote === '"') {
      if (c === '"') quote = ''; else cur += c;
    } else if (c === "'" || c === '"') {
      quote = c;
      inArg = true;
    } else if (/\s/.test(c)) {
      if (inArg) args.push(cur);
      cur = '';
      inArg = false;
    } else {
      cur += c;
      inArg = true;
    }
  }
  if (quote) throw new Error(`Unclosed ${quote} in flags`);
  if (inArg) args.push(cur);
  return args;
}

export function esc(s) {
  return String(s).replace(/[&<>"']/g, c => ({
    '&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'
  }[c]));
}

export function fmt(iso) {
  if (!iso) return '';
  const d = new Date(iso);
  if (isNaN(d)) return '';
  return d.toLocaleString(undefined, {
    year: 'numeric', month: 'numeric', day: 'numeric',
    hour: 'numeric', minute: '2-digit',
  });
}

// --- teardown helpers (used by multiple views on route change) ---

export function closeES() {
  if (currentES) { currentES.close(); currentES = null; }
  closeTerminalES();
  clearViewing();
}
export function closeTerminalES() {
  if (currentTerminalES) { currentTerminalES.close(); currentTerminalES = null; }
}
export function clearListInterval() {
  if (listInterval) { clearInterval(listInterval); listInterval = null; }
}
