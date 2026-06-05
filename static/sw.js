const CACHE_NAME = 'git-file-cache-v10';
const STATIC_ASSETS = [
    '/',
    '/index.html',
    '/style.css',
    '/app.js',
    '/manifest.json',
    '/icon.svg'
];

self.addEventListener('install', event => {
    console.log('SW installing...');
    event.waitUntil(
        caches.open(CACHE_NAME).then(cache => cache.addAll(STATIC_ASSETS))
    );
    self.skipWaiting(); // активируем сразу после установки
});

self.addEventListener('activate', event => {
    console.log('SW activating...');
    event.waitUntil(
        caches.keys().then(keys => Promise.all(
            keys.filter(key => key !== CACHE_NAME).map(key => caches.delete(key))
        ))
    );
    // Уведомляем клиентов о новой версии
    event.waitUntil(self.clients.claim());
    // Отправляем сообщение всем клиентам
    self.clients.matchAll().then(clients => {
        clients.forEach(client => client.postMessage({ type: 'SW_UPDATED' }));
    });
});

const isCacheableRequest = (request) => {
    if (request.method !== 'GET') return false;
    const url = new URL(request.url);
    if (url.protocol !== 'http:' && url.protocol !== 'https:') return false;
    if (url.origin !== self.location.origin) return false;
    if (url.pathname.startsWith('/api/')) return false;
    return true;
};

self.addEventListener('fetch', event => {
    const url = new URL(event.request.url);

    if (url.protocol !== 'http:' && url.protocol !== 'https:') return;

    if (url.pathname.startsWith('/api/')) {
        event.respondWith(fetch(event.request).catch(() => {
            return new Response(JSON.stringify({ error: 'Network error' }), {
                status: 503,
                headers: { 'Content-Type': 'application/json' }
            });
        }));
        return;
    }

    if (!isCacheableRequest(event.request)) return;

    event.respondWith(
        caches.match(event.request).then(cached => {
            if (cached) return cached;
            return fetch(event.request).then(response => {
                if (response && response.status === 200 && response.type === 'basic') {
                    const clone = response.clone();
                    caches.open(CACHE_NAME)
                        .then(cache => cache.put(event.request, clone))
                        .catch(() => {});
                }
                return response;
            });
        })
    );
});