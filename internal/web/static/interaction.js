// usher SPA: in-session permission + AskUserQuestion interactions.

import {
  esc, currentDetailId, setPendingInteractionCounts,
} from './state.js';

// --- interaction-private state ---
let pendingInteractions = [];

export async function pollInteractions() {
  try {
    const res = await fetch('/api/interactions');
    if (!res.ok) return;
    const list = (await res.json()) || [];
    if (!sameInteractions(pendingInteractions, list)) {
      pendingInteractions = list;
    }
    // Render even when only the active route changed.
    renderInteractions();
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
      <div class="question" data-qi="${qi}"${qi ? ' hidden' : ''}>
        ${q.header ? `<div class="q-header">${esc(q.header)}</div>` : ''}
        <div class="q-text">${esc(q.question || '')}${q.multiSelect ? ' <span class="q-multi">(select all that apply)</span>' : ''}</div>
        <div class="q-options">${opts}</div>
        <input class="qother" data-qi="${qi}" type="text" placeholder="or type your own answer…">
      </div>`;
  }).join('');
  return `
    <div class="interaction ask" data-id="${esc(p.id)}">
      <div class="meta">
        <strong>question</strong>
        <span class="muted">session ${esc(sid)}</span>
        <span class="muted q-position">${p.tool_input.questions.length > 1 ? `1 of ${p.tool_input.questions.length}` : ''}</span>
      </div>
      ${blocks}
      <div class="actions">
        <button class="qback" hidden>back</button>
        <button class="qignore">ignore</button>
        <button class="qsubmit" disabled>${p.tool_input.questions.length > 1 ? 'next' : 'answer'}</button>
      </div>
    </div>`;
}

function renderPermission(p, position, total) {
  let inputJSON = '';
  try { inputJSON = JSON.stringify(p.tool_input || {}, null, 2); }
  catch { inputJSON = String(p.tool_input || ''); }
  const always = p.allow_always
    ? '<button class="allow secondary" data-scope="session">allow always</button>'
    : '';
  return `
    <div class="interaction" data-id="${esc(p.id)}">
      <div class="meta">
        <strong>${esc(p.tool_name || p.event)}</strong>
        ${total > 1 ? `<span class="muted">${position} of ${total}</span>` : ''}
      </div>
      <pre class="tool-input">${esc(inputJSON)}</pre>
      <div class="actions">
        <button class="allow primary" data-scope="once">allow</button>
        ${always}
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
  const back = node.querySelector('.qback');
  const position = node.querySelector('.q-position');
  let current = 0;
  const otherOf = qi => node.querySelector(`.qother[data-qi="${qi}"]`);
  const answerOf = qi => {
    const typed = otherOf(qi).value.trim();
    if (typed) return typed;
    // Join all selected labels — a single-select question has at most one.
    return [...node.querySelectorAll(`.qopt.selected[data-qi="${qi}"]`)]
      .map(s => (qs[+qi].options[+s.dataset.oi] || {}).label || '').join(', ');
  };
  const recompute = () => {
    submit.disabled = !answerOf(current);
  };
  const showQuestion = qi => {
    current = qi;
    node.querySelectorAll('.question').forEach((q, i) => { q.hidden = i !== current; });
    position.textContent = `${current + 1} of ${qs.length}`;
    back.hidden = current === 0;
    submit.textContent = current === qs.length - 1 ? 'answer' : 'next';
    recompute();
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
  back.addEventListener('click', () => showQuestion(current - 1));
  submit.addEventListener('click', () => {
    if (current < qs.length - 1) {
      showQuestion(current + 1);
      return;
    }
    const answers = {};
    qs.forEach((q, qi) => { answers[q.question] = answerOf(qi); });
    respondAnswers(id, answers);
  });
}

function renderInteractions() {
  const permissions = pendingInteractions.filter(p => !isAskQuestion(p));
  const counts = new Map();
  for (const p of pendingInteractions) {
    if (p.session_id) counts.set(p.session_id, (counts.get(p.session_id) || 0) + 1);
  }
  setPendingInteractionCounts(counts);

  // Preserve an unchanged node so polling does not reset its scroll position.
  const here = permissions.filter(p => p.session_id === currentDetailId);
  const composer = document.querySelector('.send-anchor > .composer');
  const existing = document.getElementById('session-permission');
  const shownID = existing?.querySelector('.interaction')?.dataset.id;
  if (!here.length || !composer) {
    existing?.remove();
  } else if (shownID !== here[0].id) {
    existing?.remove();
    const bar = document.createElement('div');
    bar.id = 'session-permission';
    bar.className = 'session-permission';
    bar.innerHTML = renderPermission(here[0], 1, here.length);
    composer.before(bar);
    wirePermission(bar.querySelector('.interaction'));
  }

  // Questions belong to the session that raised them, just like permissions.
  // Preserve an unchanged card so polling does not clear choices or typed text.
  const asks = pendingInteractions.filter(p => isAskQuestion(p) && p.session_id === currentDetailId);
  const existingQuestions = document.getElementById('session-questions');
  // Show one queued question at a time. Key only by the visible interaction so
  // a newly queued question does not reset an answer already in progress.
  const askKey = asks[0]?.id || '';
  if (!asks.length || !composer) {
    existingQuestions?.remove();
    return;
  }
  if (existingQuestions?.dataset.interactions === askKey) return;
  existingQuestions?.remove();
  const questions = document.createElement('div');
  questions.id = 'session-questions';
  questions.className = 'session-interactions';
  questions.dataset.interactions = askKey;
  const current = asks[0];
  const sid = (current.session_id || '').slice(0, 8) || '(unknown)';
  questions.innerHTML = renderAskQuestion(current, sid);
  composer.before(questions);
  questions.querySelectorAll('.interaction').forEach(node => {
    const id = node.dataset.id;
    wireAskQuestion(node, id);
  });
}

function wirePermission(node) {
  if (!node) return;
  const id = node.dataset.id;
  node.querySelectorAll('button.allow,button.deny').forEach(btn => {
    btn.addEventListener('click', () => {
      const behavior = btn.classList.contains('allow') ? 'allow' : 'deny';
      btn.closest('.actions').querySelectorAll('button').forEach(b => { b.disabled = true; });
      respond(id, behavior, btn.dataset.scope || 'once');
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
