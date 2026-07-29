// Settings: instance-wide configuration, shown as a modal over whatever view
// the user was on. One file per view is this app's granularity (there is no
// bundler), so every pane lives here and PANES is the only thing to touch when
// adding one.
//
// The dialog is a native <dialog> opened with showModal(), which supplies the
// focus trap, Esc handling, backdrop, background inertness, and focus restore
// that would otherwise be hand-written. The hash stays the single source of
// truth: route() opens and closes it, and every exit path (× button, Esc,
// backdrop click) funnels through the same `close` event.
//
// Two hashes, two levels. `#/settings` is the section list, `#/settings/<id>`
// is one section. Wide viewports show both columns at once and so never rest
// on the list alone; narrow ones drill down, phone-settings style. Which
// column is visible is left entirely to CSS — no viewport branching here, so
// there is nothing to re-run on resize or rotate.

import { esc, getLastSessions } from './state.js';
import { bindPushToggle } from './push.js';

const dialog = document.getElementById('settings-dialog');
const paneHost = document.getElementById('settings-pane');
const navHost = document.getElementById('settings-nav');
const titlePane = document.getElementById('settings-title-pane');

const PANES = [
  {id: 'general', label: 'General', render: renderGeneralPane},
  {id: 'agents', label: 'Agents', render: renderAgentsPane},
  {id: 'scheduled', label: 'Scheduled', render: renderScheduledPane},
];

// Matches the app's other inline icons (16px box, 1.5 stroke) so the drill-in
// chevron and the back chevron in the header read as one weight.
const CHEVRON = `<svg class="settings-nav-chevron" width="16" height="16" viewBox="0 0 16 16"
  fill="none" stroke="currentColor" stroke-width="1.5" stroke-linecap="round"
  stroke-linejoin="round" aria-hidden="true"><path d="M6 3 L11 8 L6 13"/></svg>`;

let onRequestClose = null;
let renderedPane = '';

// ----- shell ---------------------------------------------------------------

export function showSettings(pane, requestClose) {
  onRequestClose = requestClose;
  const selected = PANES.find(p => p.id === pane);
  // '#/settings' shows the first section's content, so the nav marks it too —
  // wide layouts land on General rather than on nothing. The narrow list level
  // suppresses the highlight in CSS, since there nothing is open yet.
  const active = selected || PANES[0];
  dialog.classList.toggle('at-pane', Boolean(selected));
  navHost.innerHTML = PANES.map(p =>
    `<a class="settings-nav-item${p === active ? ' active' : ''}" href="#/settings/${esc(p.id)}">
      <span>${esc(p.label)}</span>${CHEVRON}
    </a>`
  ).join('');
  titlePane.textContent = active.label;
  if (!dialog.open) dialog.showModal();
  // The pane is rendered even while the narrow layout is showing the list, so
  // drilling in is instant and never refetches.
  if (renderedPane !== active.id) {
    renderedPane = active.id;
    active.render(paneHost);
  }
}

export function hideSettings() {
  if (!dialog.open) return false;
  onRequestClose = null; // closing to follow the hash; don't bounce it back
  dialog.close();
  return true;
}

dialog.addEventListener('close', () => {
  renderedPane = ''; // refetch on reopen; agents may have changed meanwhile
  const requestClose = onRequestClose;
  onRequestClose = null;
  requestClose?.();
});
document.getElementById('settings-close').addEventListener('click', () => dialog.close());
dialog.addEventListener('click', event => {
  if (event.target === dialog) dialog.close(); // backdrop only, never the panel
});

// Moving between levels replaces the hash instead of pushing it, so the whole
// settings visit stays one history entry and closing still lands on the view
// it was opened over. Left as real links for keyboard and copy-link; only the
// plain left-click is intercepted.
dialog.addEventListener('click', event => {
  const link = event.target.closest('a[href^="#/settings"]');
  if (!link || event.metaKey || event.ctrlKey || event.shiftKey || event.button !== 0) return;
  event.preventDefault();
  location.replace(link.getAttribute('href'));
});

// ----- general pane --------------------------------------------------------

function renderGeneralPane(host) {
  host.innerHTML = `
    <div class="settings-section">
      <h2>Notifications</h2>
      <div class="settings-row">
        <div class="settings-row-text">
          <span class="settings-row-label">Push notifications</span>
          <p class="settings-row-hint">Sent when a turn finishes or a session
            needs permission.</p>
        </div>
        <button id="general-notif" class="settings-row-action" type="button" hidden></button>
        <span class="settings-row-note muted">Not available in this browser.</span>
      </div>
    </div>`;
  // push.js owns the label, the disabled state, and the gesture-sensitive
  // click path; it keeps this button and the sidebar item in step.
  bindPushToggle(document.getElementById('general-notif'));
}

// ----- agents pane ---------------------------------------------------------

let selectedName = '';
let agents = [];
let catalogs = {};
let backends = [];
// Set by edits, cleared by save and by every renderForm. Anything that swaps
// the form out from under those edits checks it first.
let formDirty = false;

function confirmDiscard() {
  return !formDirty || confirm('Discard unsaved changes to this agent?');
}

// One scannable line per agent, so the list is useful without clicking.
function agentSummary(agent) {
  return [agent.backend, agent.model].filter(Boolean).join(' · ') || 'no defaults';
}

function renderList() {
  const list = document.getElementById('agent-list');
  if (!list) return;
  list.innerHTML = agents.map(agent => `
    <button type="button" class="cfg-row${agent.name === selectedName ? ' active' : ''}"
            data-agent-name="${esc(agent.name)}">
      <span class="cfg-row-name">${esc(agent.name)}</span>
      <small>${esc(agentSummary(agent))}</small>
    </button>`).join('');
  list.querySelectorAll('[data-agent-name]').forEach(button => {
    button.addEventListener('click', () => {
      if (button.dataset.agentName === selectedName || !confirmDiscard()) return;
      selectedName = button.dataset.agentName;
      renderList();
      renderForm(agents.find(agent => agent.name === selectedName));
    });
  });
}

function optionsHTML(rows, selected) {
  return rows.map(row =>
    `<option value="${esc(row.value)}"${row.value === selected ? ' selected' : ''}>${esc(row.label)}</option>`
  ).join('');
}

// No "unspecified" row, here or in the model list: the forms always write a
// concrete backend and model, as the composer does. An empty field is still
// legal over the API and in a hand-edited file, where it means "inherit", but
// offering that as a picker row invents a third state nothing else has.
function backendOptions(selected) {
  const rows = backends.map(name => ({value: name, label: name.charAt(0).toUpperCase() + name.slice(1)}));
  if (selected && !backends.includes(selected)) {
    rows.push({value: selected, label: selected + ' (unavailable)'});
  }
  return optionsHTML(rows, selected);
}

// A backend whose catalog usher can't read still needs one choice, and
// "default" is the sentinel that hands the pick back to the backend.
function modelOptions(backend, selected) {
  const models = catalogs[backend] || [];
  const options = models.length ? [...models] : [{id: 'default', display_name: 'Default'}];
  if (selected && !options.some(model => model.id === selected)) {
    options.push({id: selected, display_name: selected + ' (unavailable)'});
  }
  return options.map(model =>
    `<option value="${esc(model.id)}"${model.id === selected ? ' selected' : ''}>${esc(model.display_name || model.id)}</option>`
  ).join('');
}

function renderForm(agent, copy = false) {
  const host = document.getElementById('agent-editor');
  if (!host) return;
  const creating = !agent || copy;
  const value = agent || {name: '', cwd: '', backend: '', model: '', append_system_prompt: ''};
  const name = copy ? value.name + '-copy' : value.name;
  const selectedBackend = value.backend || backends[0] || '';
  host.innerHTML = `
    <form class="cfg-form">
      <label>
        <span>Name</span>
        <input name="name" class="mono" value="${esc(name)}" required autocapitalize="off" autocorrect="off" spellcheck="false">
        <small class="cfg-field-hint">No spaces or <code>/</code></small>
      </label>
      <label class="cfg-form-group"><span>cwd</span><input name="cwd" value="${esc(value.cwd || '')}"
        list="agent-cwd-list" autocomplete="off" placeholder="/absolute/path or ~"></label>
      <datalist id="agent-cwd-list">${[...new Set(getLastSessions().map(s => s.cwd).filter(Boolean))].sort()
        .map(cwd => `<option value="${esc(cwd)}"></option>`).join('')}</datalist>
      <div class="cfg-form-pair">
        <label><span>Backend</span><select name="backend">${backendOptions(selectedBackend)}</select></label>
        <label><span>Model</span><select name="model">${modelOptions(selectedBackend, value.model || '')}</select></label>
      </div>
      <label class="cfg-form-group"><span>Append system prompt</span>
        <textarea name="append_system_prompt" rows="6" placeholder="Additional instructions for sessions created with this agent…">${esc(value.append_system_prompt || '')}</textarea>
      </label>
      <div class="cfg-form-error err" hidden></div>
      <div class="cfg-form-actions">
        ${creating ? '' : '<button type="button" class="danger agent-delete">Delete</button><button type="button" class="agent-copy">Duplicate</button>'}
        <button type="submit" class="primary agent-save">${creating ? 'Create agent' : 'Save'}</button>
      </div>
    </form>`;

  formDirty = false; // fresh render reflects the stored agent (or a blank)
  const form = host.querySelector('form');
  const backendEl = form.elements.backend;
  const modelEl = form.elements.model;
  backendEl.addEventListener('change', () => {
    modelEl.innerHTML = modelOptions(backendEl.value, '');
  });
  const showError = message => {
    const error = form.querySelector('.cfg-form-error');
    error.textContent = message;
    error.hidden = false;
  };
  // Drop the confirmation as soon as the form is dirty again.
  form.addEventListener('input', () => {
    formDirty = true;
    const save = form.querySelector('.agent-save');
    if (save.textContent === 'Saved') save.textContent = 'Save';
  });
  form.addEventListener('submit', async event => {
    event.preventDefault();
    const payload = Object.fromEntries(new FormData(form));
    const response = await fetch(creating ? '/api/agents' : '/api/agents/' + encodeURIComponent(value.name), {
      method: creating ? 'POST' : 'PUT',
      headers: {'Content-Type': 'application/json'},
      body: JSON.stringify(payload),
    }).catch(error => ({ok: false, json: async () => ({error: error.message})}));
    const body = await response.json().catch(() => ({}));
    if (!response.ok) {
      showError(body.error || ('HTTP ' + response.status));
      return;
    }
    formDirty = false;
    // Take the name back from the server: it trims and lowercases, and may have
    // just been renamed, so echoing what was typed would lose the selection.
    selectedName = body.name || '';
    agents = creating
      ? [...agents, body]
      : agents.map(agent => (agent.name === value.name ? body : agent));
    // Refresh only the list. Re-rendering the form here would reset focus, the
    // caret, and any height the user dragged the textarea to — and would wipe
    // the confirmation below before it was ever painted. A fresh create is the
    // exception: the form has to switch into edit mode to grow its buttons and
    // start issuing PUTs.
    value.name = body.name;
    renderList();
    if (creating) {
      renderForm(body);
    }
    const save = document.querySelector('#agent-editor .agent-save');
    if (save) save.textContent = 'Saved';
  });
  host.querySelector('.agent-copy')?.addEventListener('click', () => {
    if (confirmDiscard()) renderForm(value, true);
  });
  host.querySelector('.agent-delete')?.addEventListener('click', async () => {
    if (!confirm(`Delete agent “${value.name}”? Existing sessions are not affected.`)) return;
    const response = await fetch('/api/agents/' + encodeURIComponent(value.name), {method: 'DELETE'});
    if (!response.ok) {
      const body = await response.json().catch(() => ({}));
      showError(body.error || ('HTTP ' + response.status));
      return;
    }
    selectedName = '';
    await loadAgents();
  });
}

// Both panes need the installed backends and their model catalogs; both fetch
// on open rather than caching, since a backend can be enabled under usher.
async function loadCatalogs() {
  const response = await fetch('/api/models');
  const data = response.ok ? await response.json() : {};
  backends = data.backends || [];
  catalogs = data.models || {};
}

async function loadAgentList() {
  const response = await fetch('/api/agents');
  const data = response.ok ? await response.json() : {agents: []};
  agents = data.agents || [];
}

async function loadAgents() {
  await loadAgentList();
  if (selectedName && !agents.some(agent => agent.name === selectedName)) selectedName = '';
  if (!selectedName && agents.length) selectedName = agents[0].name;
  renderList();
  renderForm(selectedName ? agents.find(agent => agent.name === selectedName) : null);
}

async function renderAgentsPane(host) {
  host.innerHTML = `
    <div class="cfg-pane">
      <div class="cfg-pane-list">
        <div class="cfg-pane-heading">
          <p class="cfg-intro">Reusable defaults for new sessions.</p>
          <button id="agent-new" type="button">+ new</button>
        </div>
        <div id="agent-list"></div>
      </div>
      <div id="agent-editor" class="cfg-pane-editor"></div>
    </div>`;
  document.getElementById('agent-new').addEventListener('click', () => {
    if (!confirmDiscard()) return;
    selectedName = '';
    renderList();
    renderForm(null);
    host.querySelector('.cfg-form input[name="name"]')?.focus();
  });
  try {
    await loadCatalogs();
    await loadAgents();
  } catch (error) {
    document.getElementById('agent-editor').innerHTML = `<p class="err">${esc(error.message || error)}</p>`;
  }
}

// ----- scheduled pane ------------------------------------------------------
// Same list-beside-editor shape as Agents, over /api/schedules. A task's
// fields are the composer's, plus when to fire.

let schedules = [];
let selectedScheduleID = '';
let scheduleDirty = false;
// The zone name the server reads cron expressions on, from /api/schedules.
let serverZone = '';

// Two clocks meet in this pane: the server reads cron expressions on its own,
// while timestamps render in the browser's. So the cron field is labelled with
// the server's zone, and each timestamp carries its own.
function fmtZoned(iso) {
  if (!iso) return '';
  const date = new Date(iso);
  if (isNaN(date)) return '';
  return date.toLocaleString(undefined, {
    year: 'numeric', month: 'numeric', day: 'numeric',
    hour: 'numeric', minute: '2-digit', timeZoneName: 'short',
  });
}

// Shortcuts, not a picker — the field still takes any cron expression. They
// are buttons rather than the input's datalist because nothing hints a
// datalist is there: an autocomplete list no one can see is a feature no one
// finds, and the people typing `0 9 * * 1-5` on a phone need it most.
const CRON_PRESETS = [
  ['0 9 * * mon-fri', 'Weekdays 9am'],
  ['0 3 * * *', 'Nightly 3am'],
  ['0 * * * *', 'Hourly'],
  ['0 */6 * * *', 'Every 6 hours'],
];

function confirmScheduleDiscard() {
  return !scheduleDirty || confirm('Discard unsaved changes to this schedule?');
}

// "None" is a real choice — a task may name no agent — unlike the backend and
// model lists, which never offer one.
function agentOptions(selected) {
  const rows = [{value: '', label: 'None'}, ...agents.map(a => ({value: a.name, label: a.name}))];
  if (selected && !agents.some(a => a.name === selected)) {
    rows.push({value: selected, label: selected + ' (unavailable)'});
  }
  return optionsHTML(rows, selected);
}

function renderScheduleList() {
  const list = document.getElementById('sched-list');
  if (!list) return;
  list.innerHTML = schedules.map(task => `
    <div class="sched-row">
      <button type="button" class="cfg-row${task.id === selectedScheduleID ? ' active' : ''}"
              data-sched-id="${esc(task.id)}">
        <span class="${task.enabled ? '' : 'sched-paused'}">${esc(task.name)}</span>
        <small>${esc(task.cron)}</small>
      </button>
      <label class="sched-toggle" title="Enabled">
        <input type="checkbox" data-toggle-id="${esc(task.id)}"${task.enabled ? ' checked' : ''}>
      </label>
    </div>`).join('');
  list.querySelectorAll('[data-sched-id]').forEach(button => {
    button.addEventListener('click', () => {
      if (button.dataset.schedId === selectedScheduleID || !confirmScheduleDiscard()) return;
      selectedScheduleID = button.dataset.schedId;
      renderScheduleList();
      renderScheduleForm(schedules.find(task => task.id === selectedScheduleID));
    });
  });
  list.querySelectorAll('[data-toggle-id]').forEach(box => {
    box.addEventListener('change', () => toggleSchedule(box.dataset.toggleId, box.checked));
  });
}

// The switch saves on the spot — being one click is the whole reason it is out
// here — and writes the answer back into the task object the editor is holding,
// so a form open on this task does not submit a stale enabled later.
async function toggleSchedule(id, enabled) {
  const task = schedules.find(entry => entry.id === id);
  if (!task) return;
  const response = await fetch('/api/schedules/' + encodeURIComponent(id), {
    method: 'PUT',
    headers: {'Content-Type': 'application/json'},
    body: JSON.stringify({...task, enabled}),
  }).catch(() => ({ok: false, json: async () => ({})}));
  if (!response.ok) {
    await loadSchedules(); // the server's answer, whatever it is
    return;
  }
  Object.assign(task, await response.json().catch(() => ({})));
  renderScheduleList();
  if (id === selectedScheduleID) {
    // Only the status block: the fields may hold edits in progress.
    const status = document.querySelector('#sched-editor .sched-status');
    if (status) status.innerHTML = scheduleStatusHTML(task);
  }
}

// When the runner will start this task next — its own cron reading, not a
// second implementation in the browser. Nothing here says what earlier runs
// did. "Overdue" is the one place a stopped runner becomes visible.
function scheduleStatusHTML(task) {
  if (!task) return '';
  let when = task.enabled ? 'never' : 'paused';
  if (task.next_run) {
    const overdue = new Date(task.next_run) < new Date();
    when = esc(fmtZoned(task.next_run) + (overdue ? ' (overdue, runs shortly)' : ''));
  }
  return `<span><span class="sched-status-label">Next run</span>${when}</span>`;
}

function renderScheduleForm(task) {
  const host = document.getElementById('sched-editor');
  if (!host) return;
  const creating = !task;
  const value = task || {
    name: '', enabled: true, cron: CRON_PRESETS[0][0],
    agent: '', cwd: '', backend: '', model: '', prompt: '',
  };
  const selectedBackend = value.backend || backends[0] || '';
  host.innerHTML = `
    <form class="cfg-form">
      <label><span>Name</span>
        <input name="name" value="${esc(value.name || '')}" required placeholder="Nightly test run"></label>
      <label class="cfg-form-group">
        <span>Cron${serverZone ? ` (${esc(serverZone)})` : ''}</span>
        <input name="cron" class="mono" value="${esc(value.cron || '')}"
               autocapitalize="off" autocorrect="off" spellcheck="false"></label>
      <div class="sched-presets">${CRON_PRESETS.map(
        ([expr, label]) => `<button type="button" class="sched-preset" data-cron="${esc(expr)}">${esc(label)}</button>`
      ).join('')}</div>
      <small class="cfg-field-hint"><code>minute hour day month weekday</code></small>
      <label class="cfg-form-group"><span>Agent</span><select name="agent">${
        agentOptions(value.agent || '')
      }</select></label>
      <label><span>cwd</span><input name="cwd" value="${esc(value.cwd || '')}"
        list="sched-cwd-list" autocomplete="off" placeholder="/absolute/path or ~"></label>
      <datalist id="sched-cwd-list">${[...new Set(getLastSessions().map(s => s.cwd).filter(Boolean))].sort()
        .map(cwd => `<option value="${esc(cwd)}"></option>`).join('')}</datalist>
      <div class="cfg-form-pair">
        <label><span>Backend</span><select name="backend">${backendOptions(selectedBackend)}</select></label>
        <label><span>Model</span><select name="model">${
          modelOptions(selectedBackend, value.model || '')
        }</select></label>
      </div>
      <label class="cfg-form-group"><span>Prompt</span>
        <textarea name="prompt" rows="6" required
          placeholder="The message each run starts its session with…">${esc(value.prompt || '')}</textarea>
      </label>
      ${task ? `<div class="sched-status">${scheduleStatusHTML(task)}</div>` : ''}
      <div class="cfg-form-error err" hidden></div>
      <div class="cfg-form-actions">
        ${creating ? '' : `<button type="button" class="danger sched-delete">Delete</button>
          <button type="button" class="sched-run">Run now</button>`}
        <button type="submit" class="primary sched-save">${creating ? 'Create schedule' : 'Save'}</button>
      </div>
    </form>`;

  scheduleDirty = false;
  const form = host.querySelector('form');
  const showError = message => {
    const error = form.querySelector('.cfg-form-error');
    error.textContent = message;
    error.hidden = false;
  };
  // Fill the field rather than submit: a preset is a starting point, and the
  // dispatched event runs the same dirty-tracking the keyboard path does.
  form.querySelectorAll('.sched-preset').forEach(button => {
    button.addEventListener('click', () => {
      form.elements.cron.value = button.dataset.cron;
      form.elements.cron.dispatchEvent(new Event('input', {bubbles: true}));
    });
  });
  const backendEl = form.elements.backend;
  backendEl.addEventListener('change', () => {
    form.elements.model.innerHTML = modelOptions(backendEl.value, '');
  });
  // Picking an agent fills the fields it has an opinion about and leaves them
  // editable, exactly as the composer does — so what the task stores is always
  // the concrete configuration it will run with.
  form.elements.agent.addEventListener('change', () => {
    const profile = agents.find(a => a.name === form.elements.agent.value);
    if (!profile) return;
    if (profile.cwd) form.elements.cwd.value = profile.cwd;
    if (profile.backend) backendEl.value = profile.backend;
    form.elements.model.innerHTML = modelOptions(backendEl.value, profile.model || '');
  });
  form.addEventListener('input', () => {
    scheduleDirty = true;
    const save = form.querySelector('.sched-save');
    if (save.textContent === 'Saved') save.textContent = 'Save';
  });

  form.addEventListener('submit', async event => {
    event.preventDefault();
    const payload = {
      name: form.elements.name.value,
      enabled: value.enabled, // owned by the list's switch, not by this form
      cron: form.elements.cron.value,
      agent: form.elements.agent.value,
      cwd: form.elements.cwd.value,
      backend: form.elements.backend.value,
      model: form.elements.model.value,
      prompt: form.elements.prompt.value,
    };
    const response = await fetch(creating ? '/api/schedules' : '/api/schedules/' + encodeURIComponent(value.id), {
      method: creating ? 'POST' : 'PUT',
      headers: {'Content-Type': 'application/json'},
      body: JSON.stringify(payload),
    }).catch(error => ({ok: false, json: async () => ({error: error.message})}));
    const body = await response.json().catch(() => ({}));
    if (!response.ok) {
      showError(body.error || ('HTTP ' + response.status));
      return;
    }
    scheduleDirty = false;
    selectedScheduleID = body.id;
    schedules = creating
      ? [...schedules, body]
      : schedules.map(task => (task.id === value.id ? body : task));
    renderScheduleList();
    if (creating) {
      // Switch into edit mode: grow the delete/run buttons and start issuing
      // PUTs against the id the server just assigned.
      renderScheduleForm(body);
    } else {
      // Keep the fields as typed (caret, scroll), but next_run has just moved.
      form.querySelector('.sched-status').innerHTML = scheduleStatusHTML(body);
    }
    const save = document.querySelector('#sched-editor .sched-save');
    if (save) save.textContent = 'Saved';
  });

  host.querySelector('.sched-run')?.addEventListener('click', async event => {
    if (scheduleDirty && !confirm('Run the saved version? Unsaved changes here are not used.')) return;
    const button = event.currentTarget;
    button.disabled = true;
    button.textContent = 'Starting…';
    const response = await fetch('/api/schedules/' + encodeURIComponent(value.id) + '/run', {method: 'POST'})
      .catch(error => ({ok: false, json: async () => ({error: error.message})}));
    const body = await response.json().catch(() => ({}));
    button.disabled = false;
    button.textContent = 'Run now';
    // Nothing on the task changes when it runs, so there is nothing to reload.
    if (!response.ok) showError(body.error || ('HTTP ' + response.status));
  });

  host.querySelector('.sched-delete')?.addEventListener('click', async () => {
    if (!confirm(`Delete schedule “${value.name}”? Sessions it already created are not affected.`)) return;
    const response = await fetch('/api/schedules/' + encodeURIComponent(value.id), {method: 'DELETE'});
    if (!response.ok) {
      const body = await response.json().catch(() => ({}));
      showError(body.error || ('HTTP ' + response.status));
      return;
    }
    scheduleDirty = false;
    selectedScheduleID = '';
    await loadSchedules();
  });
}

async function loadSchedules() {
  const response = await fetch('/api/schedules');
  const data = response.ok ? await response.json().catch(() => ({})) : {};
  schedules = data.schedules || [];
  serverZone = data.timezone || '';
  if (selectedScheduleID && !schedules.some(task => task.id === selectedScheduleID)) selectedScheduleID = '';
  if (!selectedScheduleID && schedules.length) selectedScheduleID = schedules[0].id;
  renderScheduleList();
  renderScheduleForm(schedules.find(task => task.id === selectedScheduleID));
}

async function renderScheduledPane(host) {
  host.innerHTML = `
    <div class="cfg-pane">
      <div class="cfg-pane-list">
        <div class="cfg-pane-heading">
          <p class="cfg-intro">Scheduled tasks.</p>
          <button id="sched-new" type="button">+ new</button>
        </div>
        <div id="sched-list"></div>
      </div>
      <div id="sched-editor" class="cfg-pane-editor"></div>
    </div>`;
  document.getElementById('sched-new').addEventListener('click', () => {
    if (!confirmScheduleDiscard()) return;
    selectedScheduleID = '';
    renderScheduleList();
    renderScheduleForm(null);
    host.querySelector('.cfg-form input[name="name"]')?.focus();
  });
  try {
    await loadCatalogs();
    await loadAgentList();
    await loadSchedules();
  } catch (error) {
    document.getElementById('sched-editor').innerHTML = `<p class="err">${esc(error.message || error)}</p>`;
  }
}
