// Cache version is part of the name on purpose: activate() drops every other
// cache, so bumping it is how a poisoned or stale shell entry is evicted.
const CACHE = 'haosbot-webui-v2';
// Every script the server injects into index.html must be listed here, or the
// page silently loses a feature when it is served from the cache.
const SHELL = ['/', '/webui/app.css', '/webui/tailwind.css', '/webui/app.js', '/webui/control.js', '/webui/enhancements.js', '/webui/workbench.js', '/webui-hardening.js', '/brand/haosbot_mark.png', '/manifest.webmanifest'];
self.addEventListener('install', event => {
  event.waitUntil(caches.open(CACHE).then(cache => cache.addAll(SHELL)).then(() => self.skipWaiting()));
});
self.addEventListener('activate', event => {
  event.waitUntil(caches.keys().then(keys => Promise.all(keys.filter(k => k !== CACHE).map(k => caches.delete(k)))).then(() => self.clients.claim()));
});
self.addEventListener('fetch', event => {
  if (event.request.method !== 'GET') return;
  const url = new URL(event.request.url);
  if (url.origin !== self.location.origin || url.pathname.startsWith('/api/')) return;
  event.respondWith(fetch(event.request).then(response => {
    // Only successful responses are cached: a transient 404/500 would otherwise
    // be served from the cache long after the server recovered. waitUntil keeps
    // the write alive if the worker is terminated right after responding.
    if (response.ok) {
      const copy = response.clone();
      event.waitUntil(caches.open(CACHE).then(cache => cache.put(event.request, copy)).catch(() => {}));
    }
    return response;
  }).catch(() => caches.match(event.request).then(hit => {
    if (hit) return hit;
    // The cached HTML shell is only a valid answer to a navigation; handing it
    // to a <script>/<img> request would deliver HTML where a script is expected.
    if (event.request.mode === 'navigate') return caches.match('/');
    return Response.error();
  })));
});
