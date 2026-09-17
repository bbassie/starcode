// Service worker for notifications only: Android Chrome shows a
// notification only through one of these, and a tap on it needs a
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
