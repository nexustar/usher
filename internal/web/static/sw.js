// usher service worker — makes the app installable and lets the static
// shell load offline. It is deliberately conservative:
//
//   - It only ever touches same-origin GETs for the app shell + static
//     assets. /api/*, the SSE event stream, and /login,/logout,/healthz are
//     never intercepted, so authentication redirects and live data behave
//     exactly as they do without a service worker.
//   - Navigations are network-first (so a 401 -> /login redirect or a fresh
//     index.html always wins online) and fall back to the cached shell
//     offline.
//   - Other static assets are stale-while-revalidate: served instantly from
//     cache, refreshed in the background. A code change therefore propagates
//     after one extra reload. Bump CACHE to force-evict everything.

const CACHE = 'usher-shell-v4';
const SHELL = [
  '/',
  '/index.html',
  '/app.js',
  '/state.js',
  '/render.js',
  '/sidebar.js',
  '/list.js',
  '/detail.js',
  '/settings.js',
  '/terminal.js',
  '/interaction.js',
  '/push.js',
  '/style.css',
  '/vendor/marked.umd.js',
  '/manifest.webmanifest',
  '/icons/icon.svg',
  '/icons/icon-192.png',
  '/icons/icon-512.png',
];

self.addEventListener('install', (e) => {
  e.waitUntil(
    caches.open(CACHE)
      // Tolerate a missing asset (e.g. a renamed vendor file) so install
      // never wedges the whole worker.
      .then((c) => Promise.allSettled(SHELL.map((u) => c.add(u))))
      .then(() => self.skipWaiting())
  );
});

self.addEventListener('activate', (e) => {
  e.waitUntil(
    caches.keys()
      .then((keys) => Promise.all(keys.filter((k) => k !== CACHE).map((k) => caches.delete(k))))
      .then(() => self.clients.claim())
  );
});

// --- web push ------------------------------------------------------------
//
// The server (internal/push) sends an encrypted JSON notification on turn-end
// and on a new permission prompt. We render the two kinds differently: a
// permission prompt is sticky and carries inline Allow/Deny actions wired
// straight to the interactions API; a turn-done is a quiet, collapsible note.

self.addEventListener('push', (e) => {
  let d = {};
  try { d = e.data ? e.data.json() : {}; } catch (_) { d = {}; }

  const opts = {
    body: d.body || '',
    tag: d.tag || 'usher',
    icon: '/icons/icon-192.png',
    badge: '/icons/icon-192.png',
    data: { url: d.url || '/', interaction_id: d.interaction_id || '' },
  };
  if (d.kind === 'permission') {
    opts.requireInteraction = true; // stays until acted on (desktop)
    opts.actions = [
      { action: 'allow', title: 'Allow' },
      { action: 'deny', title: 'Deny' },
    ];
  } else {
    opts.silent = true; // turn-done is low-priority
  }
  e.waitUntil(self.registration.showNotification(d.title || 'usher', opts));
});

self.addEventListener('notificationclick', (e) => {
  e.notification.close();
  const data = e.notification.data || {};

  // Inline Allow/Deny answers the pending interaction without opening the app.
  // Same-origin fetch carries the auth cookie, so no extra credentials needed.
  if ((e.action === 'allow' || e.action === 'deny') && data.interaction_id) {
    e.waitUntil(fetch('/api/interactions/' + data.interaction_id + '/respond', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ behavior: e.action, scope: 'once' }),
    }).catch(() => {}));
    return;
  }

  const url = data.url || '/';
  e.waitUntil(
    self.clients.matchAll({ type: 'window', includeUncontrolled: true }).then((cs) => {
      for (const c of cs) {
        if ('focus' in c) {
          c.focus();
          c.postMessage({ type: 'navigate', url });
          return undefined;
        }
      }
      return self.clients.openWindow(url);
    })
  );
});

self.addEventListener('fetch', (e) => {
  const req = e.request;
  if (req.method !== 'GET') return;

  const url = new URL(req.url);
  if (url.origin !== self.location.origin) return;

  // Leave live/auth traffic entirely to the network.
  const p = url.pathname;
  if (p.startsWith('/api/') || p === '/login' || p === '/logout' || p === '/healthz') {
    return;
  }

  if (req.mode === 'navigate') {
    e.respondWith(
      fetch(req).catch(() =>
        caches.match('/index.html').then((r) => r || caches.match('/'))
      )
    );
    return;
  }

  // Static assets: stale-while-revalidate.
  e.respondWith(
    caches.match(req).then((cached) => {
      const network = fetch(req)
        .then((res) => {
          if (res && res.ok && res.type === 'basic') {
            const copy = res.clone();
            caches.open(CACHE).then((c) => c.put(req, copy));
          }
          return res;
        })
        .catch(() => cached);
      return cached || network;
    })
  );
});
