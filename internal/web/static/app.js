// usher SPA: entry point.
// Hash-based routing between session list, detail view, new session, and main chat.

import { closeES, clearListInterval, setEditorUrl } from './state.js';
import './render.js'; // side-effect: sets up marked, render-pill listeners
import { loadSidebar, updateSidebarActive } from './sidebar.js';
import { showList, loadList } from './list.js';
import { showDetail, showNewSession, showMainChat } from './detail.js';
import { hideSettings, showSettings } from './settings.js';
import { pollInteractions } from './interaction.js';
import { initServiceWorker } from './push.js';

window.addEventListener('hashchange', route);

let contentHash = '';
let contentRendered = false;
let settingsOpenedOverContent = false;

function route() {
  const hash = location.hash || '#/';
  // Settings is a layer, not a view: the content underneath stays mounted so
  // closing restores the exact frame (scroll, live stream) the user left.
  if (hash === '#/settings' || hash.startsWith('#/settings/')) {
    settingsOpenedOverContent = contentRendered;
    if (!contentRendered) {
      showList();
      contentHash = '#/';
      contentRendered = true;
    }
    // '#/settings' is the section list; wide layouts render the first section
    // beside it, narrow ones stop at the list.
    showSettings(hash.slice('#/settings/'.length), closeSettings);
    updateSidebarActive();
    return;
  }

  if (hideSettings() && hash === contentHash) {
    settingsOpenedOverContent = false;
    updateSidebarActive();
    return;
  }

  if (hash === '#/' || hash === '') {
    showList();
  } else if (hash === '#/new') {
    showNewSession();
  } else if (hash.startsWith('#/new/')) {
    // cwds are encoded whole, so they never contain a slash — an "agent/"
    // prefix is unambiguous.
    const rest = hash.slice('#/new/'.length);
    showNewSession(rest.startsWith('agent/')
      ? {agent: decodeURIComponent(rest.slice('agent/'.length))}
      : {cwd: decodeURIComponent(rest)});
  } else if (hash === '#/chat' || hash.startsWith('#/chat/')) {
    const id = hash === '#/chat' ? 'default' : decodeURIComponent(hash.slice('#/chat/'.length));
    showMainChat(id);
  } else if (hash.startsWith('#/s/')) {
    showDetail(decodeURIComponent(hash.slice(4)));
  }
  contentHash = hash;
  contentRendered = true;
  settingsOpenedOverContent = false;
  updateSidebarActive();
}

function closeSettings() {
  if (settingsOpenedOverContent) {
    history.back();
  } else {
    location.replace(contentHash);
  }
}

setInterval(pollInteractions, 2000);
pollInteractions();

setInterval(loadSidebar, 5000);
loadSidebar();

route();

fetch('/api/config')
  .then(r => (r.ok ? r.json() : null))
  .then(c => { if (c) setEditorUrl(c.editor_url || ''); })
  .catch(() => {});

initServiceWorker();
