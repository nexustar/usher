// usher SPA: controls shared by more than one screen. Pure DOM, no page
// state; a screen's own menus stay with that screen.

import { esc, APPROVALS } from './state.js';

// ---------- icons ----------

// Inline SVG, 16px box, 1.5 stroke, sized by the text around it (.icon).
// Font glyphs (✓ ▸ ⋮ ⑃ ϟ ⏎ ↗) depend on the font; these don't.
const ICONS = {
  check: '<path d="M3.5 8.5l3 3 6-7"/>',
  'chevron-down': '<path d="M3.5 6l4.5 4.5L12.5 6"/>',
  'chevron-right': '<path d="M6 3l5 5-5 5"/>',
  kebab: '<circle cx="8" cy="4" r="1"/><circle cx="8" cy="8" r="1"/><circle cx="8" cy="12" r="1"/>', // filled: see icon()
  fork: '<circle cx="4" cy="3.5" r="1.5"/><circle cx="4" cy="12.5" r="1.5"/><circle cx="12" cy="4.5" r="1.5"/><path d="M4 5v6M12 6c0 3.5-8 2-8 5"/>',
  bolt: '<path d="M9 1.5 3.5 9h4l-1 5.5L12.5 7h-4z"/>',
  enter: '<path d="M13 3v4.5a2 2 0 0 1-2 2H4M6.5 7 4 9.5 6.5 12"/>',
  'arrow-up-right': '<path d="M4.5 11.5l7-7M6 4.5h5.5V10"/>',
  copy: '<rect x="6" y="6" width="8" height="8" rx="1.5"/><path d="M4.5 10.5h-1a1.5 1.5 0 0 1-1.5-1.5V3.5A1.5 1.5 0 0 1 3.5 2h5.5A1.5 1.5 0 0 1 10.5 3.5v1"/>',
  plus: '<path d="M8 3v10M3 8h10" stroke-width="1.6"/>', // bare lines read lighter
};

export function icon(name, cls = '') {
  const paint = name === 'kebab' ? 'fill="currentColor" ' : 'fill="none" ' +
    'stroke="currentColor" stroke-width="1.5" stroke-linecap="round" stroke-linejoin="round" ';
  return `<svg class="icon icon-${name}${cls ? ' ' + cls : ''}" viewBox="0 0 16 16" ${paint}` +
    `aria-hidden="true" focusable="false">${ICONS[name]}</svg>`;
}

// ---------- approval toggle ----------

// approvalToggleHTML is the composer's approval control: a button that opens
// its menu (see wireApprovalToggle).
export function approvalToggleHTML(approval, hidden) {
  return `<div class="approval-wrap"${hidden ? ' hidden' : ''}>
    <button class="approval-toggle" type="button" aria-haspopup="menu" aria-expanded="false"
      data-approval="${esc(approval)}" title="who answers the agent’s approval requests">
      <span class="t-icon">${icon('bolt')}</span><span class="t-full">approve:</span><span class="toggle-val">${esc(approval)}</span>
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
        `<span class="config-check">${a === current ? icon('check') : ''}</span></button>`).join('') + '</div>';
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

// ---------- popup geometry ----------

const MARGIN = 8;
const GAP = 4;

// placePopup puts menu under anchor, or above when only that fits. A
// popover's left/top are viewport coordinates.
function placePopup(menu, anchor, minWidth) {
  const rect = anchor.getBoundingClientRect();
  // The visual viewport excludes the on-screen keyboard.
  const viewH = window.visualViewport?.height ?? window.innerHeight;
  const below = viewH - rect.bottom - GAP - MARGIN;
  const above = rect.top - GAP - MARGIN;
  const up = below < 160 && above > below;
  menu.style.maxHeight = Math.min(up ? above : below, 256) + 'px';
  const width = Math.min(Math.max(rect.width, minWidth), window.innerWidth - MARGIN * 2);
  menu.style.width = width + 'px';
  menu.style.left = Math.max(MARGIN, Math.min(rect.left, window.innerWidth - width - MARGIN)) + 'px';
  menu.style.top = (up ? rect.top - GAP - menu.offsetHeight : rect.bottom + GAP) + 'px';
}

// followViewport calls place whenever the anchor may have moved, and returns
// the function that stops it. Scrolls of the menu itself don't count.
function followViewport(menu, place) {
  const onChange = e => { if (e.target !== menu) place(); };
  window.addEventListener('scroll', onChange, true);
  window.addEventListener('resize', onChange);
  window.visualViewport?.addEventListener('resize', onChange);
  return () => {
    window.removeEventListener('scroll', onChange, true);
    window.removeEventListener('resize', onChange);
    window.visualViewport?.removeEventListener('resize', onChange);
  };
}

// ---------- cwd picker ----------

// cwd picker: known cwds under a free-text input. <datalist> can't be styled
// and filters by the current value, hiding every cwd once the field is filled.
// A popover, so the settings <dialog> doesn't clip it.

let seq = 0;

// fillHit fills el with text, marking the part of [start, end) it covers.
// offset is where text begins within the full path.
function fillHit(el, text, offset, start, end) {
  const from = Math.max(start - offset, 0);
  const to = Math.min(end - offset, text.length);
  if (from >= to) { el.textContent = text; return; }
  const mark = document.createElement('mark');
  mark.textContent = text.slice(from, to);
  el.replaceChildren(text.slice(0, from), mark, text.slice(to));
}

// cwds is ordered by preference.
export function attachCwdPicker(input, cwds) {
  if (!cwds.length || !('popover' in HTMLElement.prototype)) return;
  const menu = document.createElement('div');
  menu.className = 'cwd-picker';
  menu.id = 'cwd-picker-' + ++seq;
  menu.popover = 'manual';
  menu.setAttribute('role', 'listbox');
  (input.closest('label') ?? input).after(menu); // outside the input's accessible name
  input.setAttribute('role', 'combobox');
  input.setAttribute('aria-autocomplete', 'list');
  input.setAttribute('aria-controls', menu.id);
  input.setAttribute('aria-expanded', 'false');

  let active = -1; // keyboard-highlighted row
  let typed = false; // the value is the user's own text, not a prefill or a pick
  let stopFollowing = null;

  const isOpen = () => menu.matches(':popover-open');
  const rows = () => [...menu.children];

  function place() {
    if (!input.isConnected) { close(); return; }
    placePopup(menu, input, 384);
  }

  function close() {
    if (isOpen()) menu.hidePopover();
    active = -1;
    input.setAttribute('aria-expanded', 'false');
    input.removeAttribute('aria-activedescendant');
    stopFollowing?.();
    stopFollowing = null;
  }

  // show lists every cwd until the user types: a prefill shouldn't narrow it.
  // Matches in the directory name rank first.
  function show() {
    const value = input.value.trim();
    const query = typed ? value.toLowerCase() : '';
    const hit = cwd => cwd.toLowerCase().indexOf(query);
    const inName = cwd => hit(cwd) + query.length > cwd.replace(/\/+$/, '').lastIndexOf('/') + 1;
    const matches = cwds.filter(cwd => hit(cwd) >= 0);
    const shown = [...matches.filter(inName), ...matches.filter(cwd => !inName(cwd))];
    if (!shown.length || (shown.length === 1 && shown[0] === value)) {
      close();
      return;
    }
    active = -1;
    input.removeAttribute('aria-activedescendant');
    menu.replaceChildren(...shown.map((cwd, i) => {
      const row = document.createElement('button');
      row.type = 'button';
      row.tabIndex = -1;
      row.id = menu.id + '-' + i;
      row.setAttribute('role', 'option');
      row.dataset.cwd = cwd;
      row.title = cwd;
      const slash = cwd.replace(/\/+$/, '').lastIndexOf('/');
      const start = query ? hit(cwd) : 0;
      const end = start + query.length;
      const parent = document.createElement('span');
      parent.className = 'cwd-picker-parent';
      fillHit(parent, cwd.slice(0, slash + 1), 0, start, end);
      const name = document.createElement('span');
      fillHit(name, cwd.slice(slash + 1), slash + 1, start, end);
      // rtl puts the ellipsis on the left, keeping the end of the path, which
      // is what tells siblings apart; <bdi> keeps the path itself readable.
      const path = document.createElement('span');
      path.className = 'cwd-picker-path';
      path.dir = 'rtl';
      path.append(document.createElement('bdi'));
      path.firstChild.append(parent, name);
      row.append(path);
      return row;
    }));
    if (!isOpen()) {
      menu.showPopover();
      input.setAttribute('aria-expanded', 'true');
      stopFollowing = followViewport(menu, place);
    }
    place();
    menu.scrollTop = 0;
  }

  function highlight(index) {
    const all = rows();
    all[active]?.classList.remove('active');
    all[active]?.removeAttribute('aria-selected');
    active = index;
    all[active].classList.add('active');
    all[active].setAttribute('aria-selected', 'true');
    all[active].scrollIntoView({block: 'nearest'});
    input.setAttribute('aria-activedescendant', all[active].id);
  }

  function pick(row) {
    input.value = row.dataset.cwd;
    input.dispatchEvent(new Event('input', {bubbles: true})); // forms track dirty state
    typed = false;
    close();
  }

  input.addEventListener('input', () => {
    typed = true;
    show();
  });
  input.addEventListener('click', () => { if (!isOpen()) show(); });
  input.addEventListener('blur', () => {
    typed = false; // whatever is left is the field's value, however it got there
    close();
  });
  input.addEventListener('keydown', e => {
    if (e.isComposing) return;
    if (e.key === 'ArrowDown' || e.key === 'ArrowUp') {
      e.preventDefault();
      if (!isOpen()) show();
      if (!isOpen()) return;
      const count = menu.children.length;
      const step = e.key === 'ArrowDown' ? 1 : -1;
      highlight(Math.max(0, Math.min(count - 1, active + step)));
    } else if (e.key === 'Enter' && isOpen() && active >= 0) {
      e.preventDefault();
      pick(rows()[active]);
    } else if (e.key === 'Escape' && isOpen()) {
      e.preventDefault(); // only the menu closes, not the surrounding dialog
      e.stopPropagation();
      close();
    }
  });
  menu.addEventListener('mousedown', e => e.preventDefault()); // keep input focus
  menu.addEventListener('click', e => {
    const row = e.target.closest('[role="option"]');
    if (row) pick(row);
  });
}
