// usher SPA: markdown + turn rendering + appendChatMessage.

import { icon } from './widgets.js';
import {
  esc, renderMode, setRenderModeValue, renderPillMd, renderPillRaw,
  isNearBottom, suppressAppendScroll, currentDetailId, getLastSessions,
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
      if (/^diff\b/.test(token.lang)) return '<pre><code>' + diffHTML(token.text) + '</code></pre>';
      if (/^mermaid\b/.test(token.lang)) return diagramHTML(token);
      return false;
    },
    html(token) { return esc(typeof token === 'string' ? token : token.text); },
    image(token) {
      const href = String(token.href || '');
      const alt = String(token.text || '');
      const title = token.title ? ' title="' + esc(String(token.title)) + '"' : '';
      // A filesystem path (or file: URL) in a session transcript is served
      // through the same endpoint as show_image. A remote image is only a
      // link: loading it on render would let a prompt-injected agent send
      // data to any host.
      const path = /^file:\/\//i.test(href) ? fileURLPath(href) : href;
      const external = /^(?:[a-z][a-z0-9+.-]*:|\/\/|#)/i.test(path);
      if (currentDetailId && path && !external) return sessionImageHTML(path, alt, title);
      return remoteImageHTML(href, alt);
    },
  },
});

export function diffHTML(text) {
  return String(text).split('\n').map(line => {
    const e = esc(line);
    if (line.startsWith('+')) return '<span class="diff-add">' + e + '</span>';
    if (line.startsWith('-')) return '<span class="diff-del">' + e + '</span>';
    if (line.startsWith('@@')) return '<span class="diff-hunk">' + e + '</span>';
    return e;
  }).join('\n');
}

// Mermaid fences render as diagrams through the vendored pele, imported on the
// first one. A fence still being streamed, and one pele rejects, stays a code
// block.
let pele = null;
let peleImport = null;
const diagrams = new Map();

function diagramHTML(token) {
  if (!/\n {0,3}(?:`{3,}|~{3,})[ \t]*\n*$/.test(token.raw)) return false;
  if (!pele) {
    peleImport ??= import('./vendor/pele.js').then(m => { pele = m; rerenderAllContent(); }, () => {});
    return false;
  }
  const src = token.text;
  let html = diagrams.get(src);
  if (html === undefined) {
    try {
      // Node ids become element ids, so a per-source prefix keeps them from
      // colliding with the page's own.
      const { svg } = pele.render(src, {
        idPrefix: 'd' + hashString(src) + '-',
        fontFamily: getComputedStyle(document.body).fontFamily,
        fontSize: 14,
        links: false,
        images: false,
      });
      html = '<div class="diagram">' + svg + '</div>';
    } catch {
      html = false;
    }
    if (diagrams.size >= 200) diagrams.clear();
    diagrams.set(src, html);
  }
  return html;
}

function hashString(s) {
  let h = 0;
  for (let i = 0; i < s.length; i++) h = Math.imul(h, 31) + s.charCodeAt(i) | 0;
  return (h >>> 0).toString(36);
}

function fileURLPath(href) {
  try {
    return decodeURIComponent(new URL(href).pathname);
  } catch {
    return href;
  }
}

function remoteImageHTML(href, alt) {
  let host = '';
  try {
    host = new URL(href, location.href).host;
  } catch { /* shown without a host */ }
  return '<a class="remote-image" href="' + esc(href) + '" target="_blank" rel="noopener noreferrer">' +
    'Image' + (alt ? ': ' + esc(alt) : '') + (host ? ' (' + esc(host) + ')' : '') + '</a>';
}

function sessionImageHTML(path, alt, titleAttr) {
  const src = '/api/v1/sessions/' + encodeURIComponent(currentDetailId) +
    '/image?path=' + encodeURIComponent(path);
  return '<span class="markdown-image"><a href="' + esc(src) +
    '" target="_blank" rel="noopener"><img loading="lazy" decoding="async" alt="' +
    esc(alt) + '" src="' + esc(src) + '" data-path="' + esc(path) + '"' +
    (titleAttr || '') + imgFallback + '></a></span>';
}

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
  // internal navigation, like the links to sessions.
  html = html.replace(
    /<a (?=[^>]*\bhref="(?!#\/))/g,
    '<a target="_blank" rel="noopener" ',
  );
  return linkSessions(html);
}

// A session id, whole or as its first eight characters.
const SESSION_REF = /\b[0-9a-f]{8}(?:-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})?\b/g;

// linkSessions turns the session ids in rendered text into links to those
// sessions. Only ids that name exactly one session usher knows are linked, so
// a commit hash or any other hex string is left alone.
function linkSessions(html) {
  const sessions = getLastSessions();
  if (!sessions.length || html.search(SESSION_REF) < 0) return html;
  const resolve = (ref) => {
    const found = sessions.filter(s => s.id === ref || (ref.length === 8 && s.id.startsWith(ref)));
    return found.length === 1 ? found[0] : null;
  };
  const tpl = document.createElement('template');
  tpl.innerHTML = html;
  const walker = document.createTreeWalker(tpl.content, NodeFilter.SHOW_TEXT);
  const texts = [];
  while (walker.nextNode()) {
    const node = walker.currentNode;
    // Not inside a link already, a code block or a diagram.
    if (!node.parentElement?.closest('a, pre, svg')) texts.push(node);
  }
  let linked = false;
  for (const node of texts) {
    const text = node.nodeValue;
    const out = document.createDocumentFragment();
    let last = 0;
    for (const m of text.matchAll(SESSION_REF)) {
      const sess = resolve(m[0]);
      if (!sess) continue;
      out.append(text.slice(last, m.index));
      const a = document.createElement('a');
      a.href = '#/s/' + encodeURIComponent(sess.id);
      a.className = 'session-link';
      if (sess.title) a.title = sess.title;
      a.textContent = m[0];
      out.append(a);
      last = m.index + m[0].length;
    }
    if (!last) continue;
    out.append(text.slice(last));
    node.replaceWith(out);
    linked = true;
  }
  return linked ? tpl.innerHTML : html;
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
  el.textContent = (img.dataset.path || img.alt || 'image') + ' — failed to load';
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
// the first {…} span rather than parsing the whole string. Returns null if
// absent/unparseable.
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

// Tool output is data: never laid out as Markdown, whatever the md/raw switch.
function toolBodyHTML(p) {
  if (p.content_kind === 'image') {
    return p.tool_target && currentDetailId
      ? sessionImageHTML(p.tool_target, p.tool_target.split('/').pop() || 'image')
      : '';
  }
  if (!p.content) return '';
  return '<pre>' + (p.content_kind === 'diff' ? diffHTML(p.content) : esc(p.content)) + '</pre>';
}

// parseRoutedSession pulls the target session id out of a routing tool's JSON
// result. The id is only there: create_session doesn't know it when called.
export function parseRoutedSession(content) {
  if (!content) return '';
  const a = content.indexOf('{');
  const b = content.lastIndexOf('}');
  if (a < 0 || b <= a) return '';
  try {
    const o = JSON.parse(content.slice(a, b + 1));
    return typeof o.session_id === 'string' ? o.session_id : '';
  } catch (_) {
    return '';
  }
}

// renderRoutedPart marks where work was handed to another session, and
// unfolds to what was sent. The detail view places the reply.
function renderRoutedPart(p) {
  const id = parseRoutedSession(p.content);
  if (!id) return '';
  let sent = '';
  try {
    const input = JSON.parse(p.tool_input || '{}');
    sent = input.text || input.initial_message || '';
  } catch (_) { /* no input recorded */ }
  // The title is filled in once the detail view knows the session's name.
  return '<details class="tool-details routed-part" data-session="' + esc(id) + '">' +
    '<summary><span class="routed-arrow" aria-hidden="true">→</span> ' +
    '<span class="routed-title">' + esc(sessionTitle(id)) + '</span>' +
    '<a class="routed-open" href="#/s/' + esc(encodeURIComponent(id)) + '" title="Open this session">open ' + icon('arrow-up-right') + '</a></summary>' +
    (sent ? '<div class="tool-scroll"><pre class="tool-input">' + esc(sent) + '</pre></div>' : '') +
    '</details>';
}

// sessionTitle is a session's title as the sidebar last saw it, else its
// short id.
function sessionTitle(id) {
  const known = getLastSessions().find(s => s.id === id);
  return (known && known.title) || id.slice(0, 8);
}

export function renderToolPart(p) {
  const name = p.tool_name || 'tool';
  const target = p.tool_target || '';
  // Falls through to the plain tool block if the result carried no session
  // id — a rejected call.
  if (/(^|__)(send_to_session|create_session)$/.test(name)) {
    const routed = renderRoutedPart(p);
    if (routed) return routed;
  }
  // show_image renders as an inline image (not a collapsible tool block). The
  // path travels only as an encodeURIComponent'd query param, so it can't break
  // out of the attribute; /image resolves+validates it against the session cwd.
  if (target && currentDetailId && /(^|__)show_image$/.test(name)) {
    const src = '/api/v1/sessions/' + encodeURIComponent(currentDetailId) +
      '/image?path=' + encodeURIComponent(target);
    const fname = target.split('/').pop() || 'image';
    // Known dims let a.sized reserve the box before load.
    const dims = parseImageDims(p.content);
    const dimAttrs = dims ? ' width="' + dims.w + '" height="' + dims.h + '"' : '';
    const sized = dims ? ' class="sized" style="--w:' + dims.w + ';--h:' + dims.h + '"' : '';
    // <a> opens the full-size image (inline view is capped via .tool-image CSS).
    return '<div class="tool-image">' +
      '<a' + sized + ' href="' + esc(src) + '" target="_blank" rel="noopener">' +
      '<img loading="lazy" decoding="async" alt="' + esc(fname) + '" src="' + esc(src) + '"' + dimAttrs + imgFallback + '>' +
      '</a></div>';
  }
  const expandByDefault = /^(Edit|Write)$/i.test(name);
  const openAttr = expandByDefault ? ' open' : '';
  const copyIcon = icon('copy');
  // tool_input is the full command / JSON arguments, sent only when the title
  // doesn't already show it all.
  const input = p.tool_input || '';
  // Shell input gets a CSS-drawn `$ ` prompt, so copying never picks it up.
  const inputClass = /^(bash|shell|powershell)$/i.test(name) ? 'tool-input shell' : 'tool-input';
  // A title that is only the input's first line gets a CSS-drawn ellipsis.
  const targetClass = input.includes('\n') && input.split('\n', 1)[0].trim() === target
    ? 'tool-target more' : 'tool-target';
  const label = esc(name) +
    (target ? ' <span class="' + targetClass + '">' + esc(target) + '</span>' : '') +
    (target || input
      ? '<button class="tool-copy" type="button" title="Copy" aria-label="Copy">' + copyIcon + '</button>'
      : '');
  return `<details class="tool-details${p.tool_error ? ' failed' : ''}"${openAttr}>` +
    `<summary>${label}</summary>` +
    // One scroller, so input and output move sideways together.
    `<div class="tool-scroll">` +
    (input ? `<pre class="${inputClass}">${esc(input)}</pre>` : '') +
    `<div class="tool-body">${toolBodyHTML(p)}</div>` +
    `</div>` +
    `</details>`;
}

// forkBtnHTML is the fork control at the foot of a completed assistant turn —
// the fork keeps everything above the divider and drops everything below (on
// the last turn: fork the current state). data-uuid is the fork point the
// server expects; clicks are handled by a document-level delegate.
export function forkBtnHTML(uuid) {
  return `<button class="turn-fork" type="button" data-uuid="${esc(uuid)}" title="Fork: branch a new session from this point">${icon('fork')} fork</button>`;
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
  div.className = 'chat-message ' + role + (m._placeholder ? ' placeholder' : '') + (m.origin ? ' injected' : '');
  const ts = m.ts ? `<span class="ts">${esc(new Date(m.ts).toLocaleString())}</span>` : '';
  const modelAttr = m.model ? ` title="${esc(m.model)}"` : '';
  let roleLabel = esc(role);
  // The user bubble (CSS) marks the speaker itself; keep only the ts.
  if (role === 'user' || role === 'assistant') roleLabel = '';

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
  } else if (role === 'error') {
    div.innerHTML =
      `<div class="content turn-error">${esc(m.content || '')}</div>` +
      `<div class="role">${ts}</div>`;
  } else if (role === 'system') {
    div.innerHTML =
      `<div class="content notice${m.level === 'warning' ? ' warning' : ''}">${esc(m.content || '')}</div>` +
      `<div class="role">${ts}</div>`;
  } else {
    // User or streaming placeholder (flat content).
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
  if (status === 'running') return '<span class="running-dot executing" title="executing"></span>';
  if (status === 'live') return '<span class="running-dot" title="process live"></span>';
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
// Click an inline image to view it full-size in a modal <dialog>. The <a href>
// stays as the fallback: ctrl/cmd/shift-click (and no-JS) opens it in a new tab.
function openLightbox(link) {
  let ov = document.getElementById('img-lightbox');
  if (!ov) {
    ov = document.createElement('dialog');
    ov.id = 'img-lightbox';
    ov.innerHTML = '<img alt="">';
    ov.addEventListener('click', () => ov.close());
    document.body.appendChild(ov);
  }
  const big = ov.querySelector('img');
  const small = link.querySelector('img');
  big.alt = small ? small.alt : '';
  big.src = link.getAttribute('href');
  // Long captures (h > 3w) would contain to a sliver; .tall fits the width and
  // scrolls. The inline image's natural size is 0 until loaded, so settle after decode.
  const tall = (img) => img.naturalHeight > img.naturalWidth * 3;
  ov.classList.toggle('tall', !!small && tall(small));
  big.decode().catch(() => {}).then(() => ov.classList.toggle('tall', tall(big)));
  if (!ov.open) ov.showModal();
  ov.scrollTop = 0; // Chrome keeps the scroll offset across close()/showModal()
}

document.addEventListener('click', (e) => {
  const a = e.target.closest('.tool-image a, .markdown-image a');
  if (!a) return;
  if (e.metaKey || e.ctrlKey || e.shiftKey) return; // let the new-tab default win
  e.preventDefault();
  openLightbox(a);
});
