// Session details: what a session is and what it is spawned with, as a modal
// opened from the session menu. Only the extra CLI flags are editable; the
// system prompt addition is shown but not editable, since Claude and Codex keep
// the one a session started with across resumes. Flags apply from the next
// spawn, so the save button names what saving does to the process.

import { esc, fmt, formatArgs, parseArgs, patchSession, refreshSubtitle, currentDetailId } from './state.js';
import { loadSidebar } from './sidebar.js';
import { loadList } from './list.js';

const dialog = document.getElementById('details-dialog');
const body = document.getElementById('details-body');
let dirty = false;

function confirmDiscard() {
  return !dirty || confirm('Discard unsaved changes?');
}

dialog.addEventListener('cancel', event => {
  if (!confirmDiscard()) event.preventDefault();
});
document.getElementById('details-close').addEventListener('click', () => {
  if (confirmDiscard()) dialog.close();
});
dialog.addEventListener('click', event => {
  if (event.target === dialog && confirmDiscard()) dialog.close();
});

async function fetchSession(id) {
  const res = await fetch('/api/v1/sessions/' + encodeURIComponent(id));
  if (!res.ok) throw new Error('HTTP ' + res.status);
  return res.json();
}

export async function showSessionDetails(id) {
  let sess;
  try {
    sess = await fetchSession(id);
  } catch (e) {
    console.warn('details failed', e);
    alert('Failed to load session details.');
    return;
  }
  render(sess);
  if (!dialog.open) dialog.showModal();
}

const busy = status => status === 'running' || status === 'awaiting_permission';

// saveLabel is what saving does: an idle session spawns with the new flags on
// its next send, a live one has to be restarted for them.
function saveLabel(status) {
  return status === 'idle' ? 'Save' : 'Save & restart';
}

function render(sess) {
  const rt = sess.runtime || {};
  // A zero time.Time arrives as year 1.
  const when = iso => (iso && !iso.startsWith('0001-') ? fmt(iso) : '');
  const facts = [
    ['Session', sess.id],
    ...(sess.subagent ? [['Parent', sess.parent_id], ['Agent', sess.agent_name]] : []),
    ['Status', (sess.status || '').replace(/_/g, ' ')],
    ['Backend', sess.backend],
    ['Model', rt.model],
    ['Effort', rt.effort],
    ['cwd', sess.cwd],
    ['Created', when(sess.started_at)],
    ['Updated', when(sess.last_event_at)],
  ];
  const editable = !sess.router && !sess.subagent;
  const args = formatArgs(sess.extra_args);
  const prompt = sess.append_system_prompt || '';
  body.innerHTML = `
    <form class="cfg-form details-form">
      <div class="details-scroll">
        <dl class="details-facts">
          ${facts.map(([k, v]) => `
            <dt>${esc(k)}</dt>
            ${v ? `<dd>${esc(v)}</dd>` : '<dd class="details-none">Unavailable</dd>'}`).join('')}
          <dt>Prompt</dt>
          ${prompt ? `<dd class="details-prompt">${esc(prompt)}</dd>` : '<dd class="details-none">None</dd>'}
        </dl>
        <label><span>Extra CLI flags</span>
          <input name="extra_args" class="mono" value="${esc(args)}"${editable ? '' : ' readonly'}
                 autocapitalize="off" autocorrect="off" spellcheck="false"
                 placeholder="${editable ? 'Flags added when spawning the backend, quoted as in a shell' : ''}">
        </label>
        ${editable ? '<div class="cfg-form-error err" hidden></div>' : `<p class="cfg-field-hint">${sess.router
          ? 'The router session’s flags are set by usher.'
          : 'Subagent transcripts are read-only.'}</p>`}
      </div>
      ${editable ? `<div class="cfg-form-actions">
        <button type="submit" class="primary details-save"></button>
      </div>` : ''}
    </form>`;
  dirty = false;
  if (!editable) return;

  const form = body.querySelector('form');
  const save = form.querySelector('.details-save');
  const error = form.querySelector('.cfg-form-error');
  const showState = status => {
    save.textContent = saveLabel(status);
    save.disabled = busy(status);
  };
  showState(sess.status);
  form.addEventListener('input', () => { dirty = true; });
  form.addEventListener('submit', async event => {
    event.preventDefault();
    error.hidden = true;
    save.disabled = true;
    try {
      const extraArgs = parseArgs(form.elements.extra_args.value);
      // Re-read: the session may have started a turn since the dialog opened.
      const now = await fetchSession(sess.id);
      if (busy(now.status)) {
        showState(now.status);
        return;
      }
      const restart = now.status !== 'idle';
      if (restart && ((now.activity?.loops || []).length || now.activity?.agents) &&
          !confirm('Restarting stops this session’s loops and background agents. Restart now?')) {
        save.disabled = false;
        return;
      }
      const res = await patchSession(sess.id, { extra_args: extraArgs });
      const out = await res.json().catch(() => ({}));
      if (!res.ok) throw new Error(out.error?.message || ('HTTP ' + res.status));
      if (restart) {
        for (const step of ['pause', 'resume']) {
          const r = await fetch('/api/v1/sessions/' + encodeURIComponent(sess.id) + '/' + step, { method: 'POST' });
          if (!r.ok) throw new Error(step + ' failed: HTTP ' + r.status);
        }
      }
      dirty = false;
      dialog.close();
      loadSidebar();
      loadList();
      if (sess.id === currentDetailId) refreshSubtitle(sess.id);
    } catch (e) {
      error.textContent = e.message;
      error.hidden = false;
      save.disabled = false;
    }
  });
}
