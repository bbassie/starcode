// Service worker for notifications only: Web Push from the server, and
// the page's own notifications (Android Chrome shows a
// notification only through one of these). A tap on one needs a
// worker to bring the page forward. Nothing is cached and every fetch
// goes to the network as before.
self.addEventListener('install', () => self.skipWaiting());
self.addEventListener('activate', (e) => e.waitUntil(self.clients.claim()));
self.addEventListener('notificationclick', (e) => {
  e.notification.close();
  const url = e.notification.data?.url || '/';
  e.waitUntil(self.clients.matchAll({ type: 'window', includeUncontrolled: true }).then((list) => {
    // An open starcode tab takes the link; otherwise a new window does.
    for (const c of list) {
      if ('focus' in c) {
        if ('navigate' in c && new URL(c.url).pathname !== url) return c.navigate(url).then((w) => w?.focus());
        return c.focus();
      }
    }
    return self.clients.openWindow(url);
  }));
});

// Web Push from the server (internal/push): show it, unless a starcode
// window is in focus on that very thread, where the page says it all.
self.addEventListener('push', (e) => {
  let m = {};
  try { m = e.data ? e.data.json() : {}; } catch { m = { body: e.data?.text() }; }
  e.waitUntil(self.clients.matchAll({ type: 'window', includeUncontrolled: true }).then((list) => {
    if (list.some((c) => c.focused && new URL(c.url).pathname === m.url)) return;
    return self.registration.showNotification(m.title || 'starcode', {
      body: m.body || '', tag: m.tag, renotify: !!m.tag, data: { url: m.url || '/' }, icon: '/static/icon-maskable.svg',
    });
  }));
});

// The browser renewed the subscription on its own: tell the server.
self.addEventListener('pushsubscriptionchange', (e) => {
  e.waitUntil((async () => {
    const res = await fetch('/api/push/key');
    if (!res.ok) return;
    const key = (await res.json()).key;
    const raw = Uint8Array.from(atob(key.replace(/-/g, '+').replace(/_/g, '/')), (c) => c.charCodeAt(0));
    const sub = await self.registration.pushManager.subscribe({ userVisibleOnly: true, applicationServerKey: raw });
    await fetch('/api/push/subscribe', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(sub) });
  })());
});
