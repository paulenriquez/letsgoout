'use strict';

// This tombstone replaces previously installed PWA workers, clears their app-shell
// caches, and unregisters itself. It intentionally has no fetch handler.
self.addEventListener('install', (event) => {
    event.waitUntil(self.skipWaiting());
});

self.addEventListener('activate', (event) => {
    event.waitUntil(
        caches.keys()
            .then((keys) => Promise.all(keys.filter((key) => key.startsWith('letsgoout-')).map((key) => caches.delete(key))))
            .then(() => self.registration.unregister())
    );
});
