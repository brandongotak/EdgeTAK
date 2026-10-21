'use strict';
// Service worker for installing the console as an app. It keeps a copy of
// the console's own files so the app opens quickly, fetching fresh copies
// first. API calls, video and map tiles always go to the network and are
// never stored.
const CACHE = 'edgetak-console-v1';

self.addEventListener('install', () => self.skipWaiting());

self.addEventListener('activate', (e) => {
  e.waitUntil(caches.keys()
    .then((keys) => Promise.all(keys.filter((k) => k !== CACHE).map((k) => caches.delete(k))))
    .then(() => self.clients.claim()));
});

self.addEventListener('fetch', (e) => {
  const req = e.request;
  const url = new URL(req.url);
  if (req.method !== 'GET' || url.origin !== self.location.origin) return;
  if (!url.pathname.startsWith('/admin/') || url.pathname.startsWith('/admin/api/') || url.pathname.startsWith('/admin/video/')) return;
  // Network first, so a new EdgeTAK version shows at once; the copy is only
  // used when the server cannot be reached.
  e.respondWith(fetch(req).then((res) => {
    if (res.ok && res.type === 'basic') {
      const copy = res.clone();
      caches.open(CACHE).then((c) => c.put(req, copy));
    }
    return res;
  }).catch(() => caches.match(req)));
});
