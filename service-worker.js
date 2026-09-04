const CACHE = 'letsgoout-v8';
const ASSETS = [
    '/',
    '/index.html',
    '/styles.css?v=8',
    '/app.js?v=8',
    '/manifest.webmanifest',
    '/icon-192.png',
    '/icon-512.png',
    '/icon-maskable-512.png',
    '/apple-touch-icon.png',
    '/fonts/fredoka-regular.woff2',
    '/fonts/fredoka-semibold.woff2'
];
const ALLOWED_PATHS = new Set(ASSETS.map((asset) => new URL(asset, self.location.origin).pathname));

self.addEventListener('install', (event) => {
    event.waitUntil(caches.open(CACHE).then((cache) => cache.addAll(ASSETS)).then(() => self.skipWaiting()));
});

self.addEventListener('activate', (event) => {
    event.waitUntil(
        caches.keys()
            .then((keys) => Promise.all(keys.filter((key) => key !== CACHE).map((key) => caches.delete(key))))
            .then(() => self.clients.claim())
    );
});

self.addEventListener('fetch', (event) => {
    const request = event.request;
    const url = new URL(request.url);
    if (request.method !== 'GET' || url.origin !== self.location.origin || url.pathname.startsWith('/api/')) return;
    if (!ALLOWED_PATHS.has(url.pathname)) return;
    event.respondWith(caches.match(request).then((cached) => cached || fetch(request)));
});
