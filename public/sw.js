// Service worker: keeps the scanner and pass page usable with no network.
//
// Bump CACHE_VERSION on every deploy that changes a file below, otherwise
// devices keep the previous shell until it is revalidated in the background.
const CACHE_VERSION = 'nsac-shell-v7';
const FONT_CACHE = 'nsac-fonts-v1';

const SHELL = [
  '/scanner.html',
  '/pass.html',
  '/js/db.js',
  '/js/scanner.js',
  '/js/pass.js',
  '/js/pass-card.js',
  '/assets/pass-template.jpg',
  '/assets/colombo-mark.png',
  '/assets/colombo-logo.png',       // pass page logo, light theme
  '/assets/colombo-logo-white.png', // pass page logo, dark theme and pass band
  '/vendor/dexie-4.0.11.mjs',
  '/vendor/html5-qrcode-2.3.8.min.js',
  '/vendor/qrcode-1.0.0.min.js',
  '/vendor/qrcode-gen-1.5.4.min.js',
  '/manifest.webmanifest',
  '/icon.svg',
  '/icon-192.png',
  '/icon-512.png',
];

self.addEventListener('install', (event) => {
  event.waitUntil(caches.open(CACHE_VERSION).then((c) => c.addAll(SHELL)).then(() => self.skipWaiting()));
});

self.addEventListener('activate', (event) => {
  event.waitUntil(
    caches.keys()
      .then((keys) => Promise.all(keys
        .filter((k) => k !== CACHE_VERSION && k !== FONT_CACHE)
        .map((k) => caches.delete(k))))
      .then(() => self.clients.claim()),
  );
});

self.addEventListener('fetch', (event) => {
  const { request } = event;
  if (request.method !== 'GET') return;
  const url = new URL(request.url);

  // API traffic is never cached: Dexie is the offline store for data.
  if (url.origin === self.location.origin && url.pathname.startsWith('/api/')) return;

  // The admin dashboard is online-only. Serving it stale from the scanner's
  // cache would show an old dashboard after a deploy.
  if (url.origin === self.location.origin && url.pathname.startsWith('/admin/')) return;

  // Google Fonts: cache-first so the typeface survives going offline.
  if (url.hostname === 'fonts.googleapis.com' || url.hostname === 'fonts.gstatic.com') {
    event.respondWith(cacheFirst(request, FONT_CACHE));
    return;
  }

  // App shell: stale-while-revalidate. Serve instantly from cache, refresh
  // in the background for the next launch.
  if (url.origin === self.location.origin) {
    event.respondWith(staleWhileRevalidate(request, event));
  }
});

async function cacheFirst(request, cacheName) {
  const cache = await caches.open(cacheName);
  const hit = await cache.match(request);
  if (hit) return hit;
  const res = await fetch(request);
  if (res.ok || res.type === 'opaque') cache.put(request, res.clone());
  return res;
}

async function staleWhileRevalidate(request, event) {
  const cache = await caches.open(CACHE_VERSION);
  const hit = await cache.match(request, { ignoreSearch: request.mode === 'navigate' });
  const network = fetch(request)
    .then((res) => {
      if (res.ok) cache.put(request, res.clone());
      return res;
    })
    .catch(() => null);
  if (hit) {
    event.waitUntil(network);
    return hit;
  }
  return (await network) || new Response('Offline and not cached yet.', { status: 503, headers: { 'Content-Type': 'text/plain' } });
}
