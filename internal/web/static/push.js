// usher SPA: service worker + web push notifications.

// PWA: register the service worker (installable + offline shell; caching
// strategy and the /api + SSE bypass live in sw.js). Best-effort — a failed
// registration just means no offline/install. Once registered, wire up web
// push (turn-done + permission notifications) behind the sidebar toggle.

// Every toggle bound with bindPushToggle, so the sidebar item and the Settings
// row always show the same state. Disconnected nodes are pruned on refresh —
// the settings pane rebuilds its DOM on every open.
const toggles = new Set();
// Cached subscription so the click handler can decide enable-vs-disable
// synchronously and, on iOS/WebKit, call requestPermission() inside the gesture
// before any await consumes the transient activation.
let pushSub = null;
let swReg = null;
let vapidKey = '';
let pushReady = false;

function urlBase64ToUint8Array(base64) {
  const padding = '='.repeat((4 - (base64.length % 4)) % 4);
  const b64 = (base64 + padding).replace(/-/g, '+').replace(/_/g, '/');
  const raw = atob(b64);
  const out = new Uint8Array(raw.length);
  for (let i = 0; i < raw.length; i++) out[i] = raw.charCodeAt(i);
  return out;
}

// setupPush decides whether notifications are usable here. Push needs a
// PushManager and a server VAPID key; without either every toggle stays hidden.
async function setupPush(reg) {
  if (!('PushManager' in window) || !('Notification' in window)) return;
  try {
    const res = await fetch('/api/push/vapid-key');
    if (!res.ok) return; // push not available server-side
    vapidKey = (await res.json()).key;
  } catch (_) {
    return;
  }
  if (!vapidKey) return;

  swReg = reg;
  pushReady = true;
  await refreshToggles();
}

// bindPushToggle adopts a button as a notifications toggle. Safe to call before
// the service worker is ready: the button stays hidden until setupPush says
// otherwise, and refreshToggles picks it up then.
export function bindPushToggle(el) {
  if (!el) return;
  toggles.add(el);
  // NOT async: requestPermission() must run synchronously in the gesture
  // (iOS/WebKit drops the prompt once an await intervenes). We branch off the
  // cached pushSub instead of awaiting getSubscription() here.
  el.addEventListener('click', () => {
    if (!pushReady) return;
    if (pushSub) {
      runToggle(disableNotifications(pushSub));
    } else {
      // Kick off the permission request first thing, still inside the gesture;
      // enableNotifications awaits the promise we hand it.
      runToggle(enableNotifications(swReg, vapidKey, Notification.requestPermission()));
    }
  });
  applyState(el);
}

// runToggle drives the busy state around an enable/disable op, then refreshes
// every bound toggle.
async function runToggle(op) {
  for (const el of toggles) el.disabled = true;
  try {
    await op;
  } catch (err) {
    console.warn('push toggle failed', err);
  } finally {
    await refreshToggles();
  }
}

async function refreshToggles() {
  if (pushReady && Notification.permission !== 'denied') {
    pushSub = await swReg.pushManager.getSubscription();
  }
  for (const el of toggles) {
    if (!el.isConnected) {
      toggles.delete(el);
      continue;
    }
    applyState(el);
  }
}

function applyState(el) {
  el.hidden = !pushReady;
  if (!pushReady) return;
  if (Notification.permission === 'denied') {
    el.textContent = 'Notifications blocked';
    el.disabled = true;
    return;
  }
  el.disabled = false;
  el.textContent = pushSub ? 'Disable notifications' : 'Enable notifications';
}

async function enableNotifications(reg, vapidKey, permPromise) {
  const perm = await permPromise;
  if (perm !== 'granted') return;
  const sub = await reg.pushManager.subscribe({
    userVisibleOnly: true,
    applicationServerKey: urlBase64ToUint8Array(vapidKey),
  });
  await fetch('/api/push/subscribe', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(sub),
  });
}

async function disableNotifications(sub) {
  // Tell the server first (we still have the endpoint), then drop the local sub.
  try {
    await fetch('/api/push/unsubscribe', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ endpoint: sub.endpoint }),
    });
  } catch (_) { /* best effort */ }
  await sub.unsubscribe();
}

export function initServiceWorker() {
  bindPushToggle(document.getElementById('notif-toggle'));
  if ('serviceWorker' in navigator) {
    window.addEventListener('load', () => {
      navigator.serviceWorker.register('/sw.js')
        .then(setupPush)
        .catch((err) => console.warn('service worker registration failed', err));
    });
    // The SW asks the page to route when a notification is clicked while a
    // window is already open.
    navigator.serviceWorker.addEventListener('message', (e) => {
      if (e.data && e.data.type === 'navigate' && e.data.url) {
        const hash = e.data.url.split('#')[1];
        if (hash) location.hash = '#' + hash;
      }
    });
  }
}
