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

// --editor-url template from GET /api/config ("" = no editor configured).
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
let lastSessions = [];         // latest /api/sessions payload — written by sidebar
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

// --- utility functions ---

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
