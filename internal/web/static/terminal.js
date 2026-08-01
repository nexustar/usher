// usher SPA: session terminal.

import {
  closeTerminalES, setCurrentTerminalES,
} from './state.js';

// Base 16-colour ANSI palette (30-37 normal, 90-97 bright), toned to read on
// the dark terminal background.
const ANSI_COLORS = [
  '#484f58', '#ff7b72', '#3fb950', '#d29922', '#58a6ff', '#bc8cff', '#39c5cf', '#b1bac4',
  '#6e7681', '#ffa198', '#56d364', '#e3b341', '#79c0ff', '#d2a8ff', '#56d4dd', '#f0f6fc',
];
const TERM_BG = '#0d1117';
const TERM_FG = '#c9d1d9';

function color256(n) {
  if (n < 16) return ANSI_COLORS[n];
  if (n >= 232) { const v = 8 + (n - 232) * 10; return `rgb(${v},${v},${v})`; }
  n -= 16;
  const r = Math.floor(n / 36), g = Math.floor((n % 36) / 6), b = n % 6;
  const c = (x) => (x === 0 ? 0 : 55 + x * 40);
  return `rgb(${c(r)},${c(g)},${c(b)})`;
}

// Subscribe to changed screen snapshots and shell closure.
export function openTerminalFeed(id, cols, rows, onFrame, onClosed) {
  closeTerminalES();
  const params = [];
  if (cols) params.push('cols=' + cols);
  if (rows) params.push('rows=' + rows);
  const q = params.length ? ('?' + params.join('&')) : '';
  const es = new EventSource('/api/sessions/' + encodeURIComponent(id) + '/terminal/screen' + q);
  setCurrentTerminalES(es);
  let lastRaw = null;
  es.addEventListener('screen', (ev) => {
    if (ev.data == null) return;
    let s;
    try { s = JSON.parse(ev.data); } catch { return; }
    if (s === lastRaw) return;
    lastRaw = s;
    onFrame(s);
  });
  if (onClosed) es.addEventListener('closed', () => { lastRaw = null; onClosed(); });
  es.onerror = () => {/* SSE auto-reconnects; no user-visible noise */};
  return es;
}

// Render the shell pane, following output while already near the bottom.
export function openTerminalScreen(id, screenEl, cols, rows, onClosed) {
  return openTerminalFeed(id, cols, rows, (s) => {
    const wrap = screenEl.parentElement;
    const atBottom = wrap ? (wrap.scrollHeight - wrap.scrollTop - wrap.clientHeight < 40) : true;
    screenEl.classList.remove('muted');
    screenEl.innerHTML = ansiToHtml(trimTerminalFrame(s));
    if (atBottom && wrap) wrap.scrollTop = wrap.scrollHeight;
  }, () => {
    screenEl.classList.add('muted');
    screenEl.textContent = 'terminal closed.';
    if (onClosed) onClosed();
  });
}

// Post the fixed control keys exposed by the panel.
export function wireTerminalControls(id) {
  document.querySelectorAll('#term-panel button[data-control]').forEach((btn) => {
    btn.addEventListener('click', async () => {
      try {
        const res = await fetch('/api/sessions/' + encodeURIComponent(id) + '/terminal/control', {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ control: btn.dataset.control }),
        });
        if (!res.ok) {
          btn.classList.add('term-key-err');
          setTimeout(() => btn.classList.remove('term-key-err'), 500);
        }
      } catch {/* transient; the next tap or frame recovers */}
    });
  });
}

// Measure terminal columns with a hidden monospace ruler.
export function measureCols(boxEl) {
  const ruler = document.createElement('span');
  ruler.textContent = '0'.repeat(100);
  ruler.style.cssText =
    'position:absolute;visibility:hidden;white-space:pre;' +
    // must match the grid's font, or cols mis-measure
    'font-size:var(--term-size);font-family:var(--term-font)';
  document.body.appendChild(ruler);
  const charPx = ruler.getBoundingClientRect().width / 100;
  ruler.remove();
  if (!charPx || !boxEl) return 80;
  // clientWidth includes the side padding; trim a little so a full-width line
  // doesn't trip horizontal scroll.
  const cols = Math.floor((boxEl.clientWidth - 24) / charPx);
  return cols > 0 ? cols : 80;
}

// trimTerminalFrame drops tmux's trailing blank pad.
export function trimTerminalFrame(s) {
  const lines = String(s).split('\n');
  while (lines.length &&
         lines[lines.length - 1].replace(/\x1b\[[0-9;]*m/g, '').trim() === '') {
    lines.pop();
  }
  return lines.join('\n');
}

// ansiToHtml converts a `capture-pane -e` frame (plain text + SGR colour
// escapes) into HTML spans. We style SGR (`ESC [ … m`): bold/dim/italic/
// underline/inverse, the 16 base colours, 256-colour, and truecolour. Inverse
// swaps fg/bg — that's how a TUI paints its selected menu row, the whole reason
// this mirror exists. OSC sequences (e.g. OSC 8 hyperlinks) also slip into the
// frame; we strip those whole rather than render them.
export function ansiToHtml(s) {
  const str = String(s);
  let fg = null, bg = null, bold = false, dim = false, ital = false, ul = false, inv = false;
  let out = '', open = false;
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
    if (ch === '\x1b' && str[i + 1] === ']') {
      // OSC (e.g. OSC 8 hyperlinks): skip through the string terminator (BEL or ESC\).
      // capture-pane keeps these even though we only style SGR — strip whole, keep text.
      const mo = /^\x1b\][^\x07\x1b]*(?:\x07|\x1b\\)/.exec(str.slice(i));
      if (mo) { i += mo[0].length; continue; }
    }
    if (ch === '\x1b') { i++; continue; } // drop a stray/unrecognised escape
    out += esc1(ch);
    i++;
  }
  closeSpan();
  return out;
}
