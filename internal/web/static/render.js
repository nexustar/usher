// usher SPA: markdown + turn rendering + appendChatMessage.

import {
  esc, renderMode, setRenderModeValue, renderPillMd, renderPillRaw,
  isNearBottom, suppressAppendScroll, currentDetailId,
} from './state.js';

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
    code(token) {
      if (/^diff\b/.test(token.lang)) {
        const lines = String(token.text).split('\n').map(line => {
          const e = esc(line);
          if (line.startsWith('+')) return '<span class="diff-add">' + e + '</span>';
          if (line.startsWith('-')) return '<span class="diff-del">' + e + '</span>';
          if (line.startsWith('@@')) return '<span class="diff-hunk">' + e + '</span>';
          return e;
        }).join('\n');
        return '<pre><code>' + lines + '</code></pre>';
      }
      return false;
    },
    html(token) { return esc(typeof token === 'string' ? token : token.text); },
    image(token) {
      const href = String(token.href || '');
      const alt = String(token.text || '');
      const title = token.title ? ' title="' + esc(String(token.title)) + '"' : '';
      // A relative/absolute filesystem path in a session transcript is served
      // through the same authenticated, contained endpoint as show_image.
      // URLs and hash references retain normal Markdown behavior.
      const external = /^(?:[a-z][a-z0-9+.-]*:|\/\/|#)/i.test(href);
      if (currentDetailId && href && !external) {
        const src = '/api/sessions/' + encodeURIComponent(currentDetailId) +
          '/image?path=' + encodeURIComponent(href);
        return '<span class="markdown-image"><a href="' + esc(src) +
          '" target="_blank" rel="noopener"><img loading="lazy" decoding="async" alt="' +
          esc(alt) + '" src="' + esc(src) + '"' + title + imgFallback + '></a></span>';
      }
      return '<img src="' + esc(href) + '" alt="' + esc(alt) + '"' + title + imgFallback + '>';
    },
  },
});

export function renderMarkdown(md) {
  if (renderMode === 'raw') {
    return '<pre class="raw-markdown">' + esc(md || '') + '</pre>';
  }
  let html = window.marked.parse(String(md || ''));
  html = html.replace(
    /(<(?:a|img)\b[^>]*?\b(?:href|src)=")\s*(?:javascript|data|vbscript)\s*:[^"]*"/gi,
    '$1#unsafe"',
  );
  // Markdown links open away from the SPA, except hash routes: those are
  // internal navigation (for example the server-generated "Switching to"
  // banner) and stay consistent with the header/sidebar focus links.
  html = html.replace(
    /<a (?=[^>]*\bhref="(?!#\/))/g,
    '<a target="_blank" rel="noopener" ',
  );
  return html;
}

// Re-render every element that carries a data-raw markdown source. Called
// whenever the user toggles md/raw mode. Streaming response stays connected
// because we don't tear down the SSE — only the in-place HTML changes.
export function rerenderAllContent() {
  document.querySelectorAll('[data-raw]').forEach(el => {
    el.innerHTML = renderMarkdown(el.dataset.raw);
  });
}

export function setRenderMode(mode) {
  setRenderModeValue(mode === 'raw' ? 'raw' : 'md');
  localStorage.setItem('usher.renderMode', renderMode);
  updateRenderModeBtn();
  rerenderAllContent();
}

export function updateRenderModeBtn() {
  if (!renderPillMd || !renderPillRaw) return;
  renderPillMd.setAttribute('aria-pressed', renderMode === 'md');
  renderPillRaw.setAttribute('aria-pressed', renderMode === 'raw');
}

if (renderPillMd && renderPillRaw) {
  renderPillMd.addEventListener('click', () => setRenderMode('md'));
  renderPillRaw.addEventListener('click', () => setRenderMode('raw'));
  updateRenderModeBtn();
}

function onImgError(img) {
  if (img.dataset.failed) return;
  img.dataset.failed = '1';
  const el = document.createElement('span');
  el.className = 'img-failed';
  el.textContent = (img.alt || 'image') + ' — failed to load';
  const wrapper = img.closest('.markdown-image, .tool-image');
  const link = wrapper && img.closest('a');
  if (link) link.replaceWith(el);
  else img.replaceWith(el);
}
window._onImgError = onImgError;
const imgFallback = ' onerror="window._onImgError(this)"';

// ---------- Rendering turns ----------

// renderToolPart renders a single tool part as a collapsible <details> element.
// Edit/Write expand by default; others collapse.
// parseImageDims pulls {w,h} out of the show_image tool's JSON result. We extract
// the first {…} span (codex fences tool output) rather than parsing the whole
// string. Returns null if absent/unparseable.
function parseImageDims(content) {
  if (!content) return null;
  const a = content.indexOf('{');
  const b = content.lastIndexOf('}');
  if (a < 0 || b <= a) return null;
  try {
    const o = JSON.parse(content.slice(a, b + 1));
    const w = parseInt(o.w, 10);
    const h = parseInt(o.h, 10);
    if (w > 0 && h > 0) return { w, h };
  } catch (_) { /* not dims JSON — just skip space reservation */ }
  return null;
}

export function renderToolPart(p) {
  const name = p.toolName || 'tool';
  const target = p.toolTarget || '';
  // show_image renders as an inline image (not a collapsible tool block). The
  // path travels only as an encodeURIComponent'd query param, so it can't break
  // out of the attribute; /image resolves+validates it against the session cwd.
  if (target && currentDetailId && /(^|__)show_image$/.test(name)) {
    const src = '/api/sessions/' + encodeURIComponent(currentDetailId) +
      '/image?path=' + encodeURIComponent(target);
    const fname = target.split('/').pop() || 'image';
    // width/height reserve layout space so the image doesn't reflow on load.
    const dims = parseImageDims(p.content);
    const dimAttrs = dims ? ' width="' + dims.w + '" height="' + dims.h + '"' : '';
    // <a> opens the full-size image (inline view is capped via .tool-image CSS).
    return '<div class="tool-image">' +
      '<a href="' + esc(src) + '" target="_blank" rel="noopener">' +
      '<img loading="lazy" decoding="async" alt="' + esc(fname) + '" src="' + esc(src) + '"' + dimAttrs + imgFallback + '>' +
      '</a></div>';
  }
  const expandByDefault = /^(Edit|Write)$/i.test(name);
  const openAttr = expandByDefault ? ' open' : '';
  // Inline SVG, not a text glyph (⧉ renders as tofu on fonts without coverage).
  const copyIcon =
    '<svg viewBox="0 0 16 16" width="14" height="14" fill="none" stroke="currentColor" stroke-width="1.5" aria-hidden="true">' +
    '<rect x="6" y="6" width="8" height="8" rx="1.5"/>' +
    '<path d="M4.5 10.5h-1a1.5 1.5 0 0 1-1.5-1.5V3.5A1.5 1.5 0 0 1 3.5 2h5.5A1.5 1.5 0 0 1 10.5 3.5v1"/>' +
    '</svg>';
  const label = target
    ? esc(name) + ' <span class="tool-target">' + esc(target) + '</span>' +
      '<button class="tool-copy" type="button" title="Copy" aria-label="Copy target">' + copyIcon + '</button>'
    : esc(name);
  return `<details class="tool-details"${openAttr}>` +
    `<summary>${label}</summary>` +
    `<div class="tool-body" data-raw="${esc(p.content || '')}">${renderMarkdown(p.content || '')}</div>` +
    `</details>`;
}

// forkBtnHTML is the fork control at the foot of a completed assistant turn —
// the fork keeps everything above the divider and drops everything below (on
// the last turn: fork the current state). data-uuid is the fork point the
// server expects; clicks are handled by a document-level delegate.
export function forkBtnHTML(uuid) {
  return `<button class="turn-fork" type="button" data-uuid="${esc(uuid)}" title="Fork: branch a new session from this point">⑃ fork</button>`;
}

// renderAssistantParts renders the parts array of a grouped assistant turn.
export function renderAssistantParts(parts) {
  return (parts || []).map(p => {
    if (p.type === 'tool') return renderToolPart(p);
    return `<div class="content" data-raw="${esc(p.content || '')}">${renderMarkdown(p.content || '')}</div>`;
  }).join('');
}

// beforeNode inserts ahead of an existing message instead of at the end;
// prepended history must never stick-scroll.
export function appendChatMessage(m, beforeNode) {
  const list = document.getElementById('chat-scroll');
  if (!list) return null;
  // Decide stick-to-bottom BEFORE inserting — the insert changes scrollHeight.
  const stick = !beforeNode && !suppressAppendScroll && isNearBottom(list);
  const div = document.createElement('div');
  const role = m.role || 'agent';
  div.className = 'chat-message ' + role + (m._placeholder ? ' placeholder' : '');
  const ts = m.ts ? `<span class="ts">${esc(new Date(m.ts).toLocaleString())}</span>` : '';
  const modelAttr = m.model ? ` title="${esc(m.model)}"` : '';
  // A relay message is a session's own reply forwarded verbatim into the main
  // chat — label it as the session speaking, linked to its detail view.
  let roleLabel = esc(role);
  // The user bubble (CSS) marks the speaker itself; keep only the ts.
  if (role === 'user' || role === 'assistant') roleLabel = '';
  if (role === 'relay') {
    roleLabel = m.source_session
      ? `<a class="relay-source" href="#/s/${esc(m.source_session)}">session ${esc(m.source_session.slice(0, 8))}</a>`
      : 'session';
  }

  if (role === 'system' && m.content === 'Context compacted') {
    div.classList.add('compaction');
    div.innerHTML = `<span>${esc(m.content)}${ts}</span>`;
  } else if (role === 'assistant' && m.parts) {
    // Grouped assistant turn: role header + structured parts. An EMPTY parts
    // array is the live-turn shell — header only; streamed parts append into
    // it (a flat .content div here would misrender the first tool part).
    // Completed turns close with the fork control at the card's bottom edge.
    div.innerHTML =
      renderAssistantParts(m.parts) +
      `<div class="turn-footer">` +
      `<div class="role"${modelAttr}>${roleLabel}${ts}</div>` +
      (m.uuid ? forkBtnHTML(m.uuid) : '') +
      `</div>`;
  } else if (role === 'summary') {
    // Compaction marker: earlier turns were folded into this standing
    // summary (model-side only — the full history stays above). Collapsed
    // by default to keep the chat readable.
    div.innerHTML =
      `<details class="summary-body">` +
      `<summary>earlier conversation compressed${ts}</summary>` +
      `<div class="content" data-raw="${esc(m.content || '')}">${renderMarkdown(m.content || '')}</div>` +
      `</details>`;
  } else {
    // User, error, agent, relay, or streaming placeholder (flat content).
    div.innerHTML =
      `<div class="content" data-raw="${esc(m.content || '')}">${renderMarkdown(m.content || '')}</div>` +
      `<div class="role"${modelAttr}>${roleLabel}${ts}</div>`;
  }
  // send-anchor lives inside chat-scroll (sticky at bottom). Insert
  // new messages before it so it stays the last child.
  const anchor = beforeNode || list.querySelector(':scope > .send-anchor');
  if (anchor) list.insertBefore(div, anchor);
  else list.appendChild(div);
  if (stick) list.scrollTop = list.scrollHeight;
  return div;
}

// updateMessageTs sets (or inserts) the timestamp span on an existing
// chat-message node. Used to canonicalize optimistic messages once the
// server returns the persisted ts.
export function updateMessageTs(node, ts) {
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

// statusDot renders the sidebar run-state indicator: a dim green dot when
// usher holds a warm-but-idle process ("live"), and a brighter pulsing dot
// while a turn is executing ("running"). Idle/undiscovered sessions get none.
export function statusDot(status) {
  if (status === 'running') return '<span class="running-dot executing" title="executing">●</span>';
  if (status === 'live') return '<span class="running-dot" title="process live">●</span>';
  return '';
}

// Inner SVG for each backend mark (24x24 viewBox; sized/tinted via CSS).
// claude: 10 round-capped spokes from the centre (12,12) out to radius 9.6,
//   tilted 5deg clockwise.
// codex: 6 outward 144deg arcs (radius 3.68) joining hexagon vertices on a
//   radius-7 circle, filled, tilted 15deg clockwise.
// pi: official press-kit pixel badge, adapted to currentColor.
export const BACKEND_MARKS = {
  claude: '<g stroke="currentColor" stroke-width="2.1" stroke-linecap="round">'
    + '<line x1="12" y1="12" x2="12.84" y2="2.44"/><line x1="12" y1="12" x2="18.30" y2="4.75"/>'
    + '<line x1="12" y1="12" x2="21.35" y2="9.84"/><line x1="12" y1="12" x2="20.84" y2="15.75"/>'
    + '<line x1="12" y1="12" x2="16.94" y2="20.23"/><line x1="12" y1="12" x2="11.16" y2="21.56"/>'
    + '<line x1="12" y1="12" x2="5.70" y2="19.25"/><line x1="12" y1="12" x2="2.65" y2="14.16"/>'
    + '<line x1="12" y1="12" x2="3.16" y2="8.25"/><line x1="12" y1="12" x2="7.06" y2="3.77"/></g>',
  codex: '<path d="M13.81,5.24A3.680 3.680 0 0 1 18.76,10.19A3.680 3.680 0 0 1 16.95,16.95'
    + 'A3.680 3.680 0 0 1 10.19,18.76A3.680 3.680 0 0 1 5.24,13.81A3.680 3.680 0 0 1 7.05,7.05'
    + 'A3.680 3.680 0 0 1 13.81,5.24Z" fill="currentColor"/>',
  pi: '<path fill="currentColor" fill-rule="evenodd" d="M4.96 4.96H15.52V12H12V15.52H8.48V19.04H4.96Z'
    + 'M8.48 8.48V12H12V8.48Z"/>'
    + '<path fill="currentColor" d="M15.52 12H19.04V19.04H15.52Z"/>',
};

// Unknown/empty backend falls back to claude, mirroring the router default.
export function backendMark(backend) {
  const b = Object.hasOwn(BACKEND_MARKS, backend) ? backend : 'claude';
  return `<svg class="backend-mark backend-mark--${b}" viewBox="0 0 24 24" aria-hidden="true" focusable="false">${BACKEND_MARKS[b]}</svg>`;
}

// ---------- inline image lightbox ----------
// Click an inline image to view it full-size in an overlay. The <a href> stays
// as the fallback: ctrl/cmd/shift-click (and no-JS) opens it in a new tab.
function openLightbox(src) {
  let ov = document.getElementById('img-lightbox');
  if (!ov) {
    ov = document.createElement('div');
    ov.id = 'img-lightbox';
    ov.innerHTML = '<img alt="">';
    ov.addEventListener('click', () => ov.classList.remove('open'));
    document.body.appendChild(ov);
  }
  ov.querySelector('img').src = src;
  ov.classList.add('open');
}

document.addEventListener('click', (e) => {
  const a = e.target.closest('.tool-image a, .markdown-image a');
  if (!a) return;
  if (e.metaKey || e.ctrlKey || e.shiftKey) return; // let the new-tab default win
  e.preventDefault();
  openLightbox(a.getAttribute('href'));
});

document.addEventListener('keydown', (e) => {
  if (e.key !== 'Escape') return;
  const ov = document.getElementById('img-lightbox');
  if (ov) ov.classList.remove('open');
});
