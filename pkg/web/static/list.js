// usher SPA: list view.

import {
  esc, fmt, root, subtitle, closeES,
  setCurrentDetailId, setCurrentDraftKey,
  listInterval, clearListInterval, setListInterval, routerDir,
} from './state.js';
import { statusDot, backendMark } from './render.js';
import { icon } from './widgets.js';

// --- list-private state ---
let lastListRowsHtml = '';
let lastCwdSig = ''; // distinct-cwd set the list's cwd <select> was built from

// ---------- List view ----------

export async function showList() {
  closeES();
  setCurrentDetailId(null);
  setCurrentDraftKey(null);
  subtitle.textContent = 'discovered sessions';
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
  if (!listInterval) setListInterval(setInterval(loadList, 5000));
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

export async function loadList() {
  if (location.hash && location.hash !== '#/' && location.hash !== '') return;
  const rowsEl = document.getElementById('list-rows');
  if (!rowsEl) return; // shell not built (not on the list view)
  try {
    // Always fetch the full set (incl. archived); the controls narrow it
    // client-side, so toggling "show archived" needs no refetch.
    const res = await fetch('/api/v1/sessions?include_archived=1');
    if (!res.ok) throw new Error('HTTP ' + res.status);
    const data = (await res.json()).sessions || [];
    // Pinned first; the router's own directory last.
    const rank = s => (s.pinned ? 0 : 1) + (routerDir && s.cwd === routerDir ? 2 : 0);
    const sorted = data.slice().sort((a, b) => rank(a) - rank(b));
    const html = sorted.length ? sorted.map(s => {
      const title = s.title || '(untitled)';
      const dot = statusDot(s.status);
      return `
      <tr data-id="${esc(s.id)}" data-cwd="${esc(s.cwd || '')}" data-archived="${s.archived ? '1' : ''}" class="${s.archived ? 'archived' : ''}">
        <td class="title" title="${esc(title)}">${backendMark(s.backend)}${dot ? dot + ' ' : ''}${esc(title)}</td>
        <td class="cwd" title="${esc(s.cwd || '')}">${esc(s.cwd || '')}</td>
        <td>${esc(fmt(s.last_event_at))}</td>
        <td class="act"><button class="kebab-btn" type="button" data-id="${esc(s.id)}" data-archived="${s.archived ? '1' : '0'}" data-pinned="${s.pinned ? '1' : '0'}" data-status="${esc(s.status || '')}" aria-label="session actions" title="more">${icon('kebab')}</button></td>
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
