const API_BASE = '/api';

// ==================== ВСПОМОГАТЕЛЬНЫЕ ====================
function escapeHtml(str) {
    if (!str) return '';
    return str.replace(/[&<>]/g, m => m === '&' ? '&amp;' : m === '<' ? '&lt;' : '&gt;');
}
function formatBytes(bytes) {
    if (bytes === 0) return '0 B';
    const k = 1024;
    const sizes = ['B', 'KB', 'MB'];
    const i = Math.floor(Math.log(bytes) / Math.log(k));
    return parseFloat((bytes / Math.pow(k, i)).toFixed(1)) + ' ' + sizes[i];
}
function ensureJpgExtension(name) {
    return name.replace(/\.(jpg|jpeg|png|gif|bmp|webp)$/i, '') + '.jpg';
}
function rotateImageBlob(blob, angleDeg, callback) {
    console.log(`Rotating by ${angleDeg}°`);
    const img = new Image();
    const url = URL.createObjectURL(blob);
    img.onload = () => {
        URL.revokeObjectURL(url);
        const canvas = document.createElement('canvas');
        const rad = angleDeg * Math.PI / 180;
        const isVertical = (Math.abs(angleDeg) % 180 !== 0);
        canvas.width = isVertical ? img.height : img.width;
        canvas.height = isVertical ? img.width : img.height;
        const ctx = canvas.getContext('2d');
        ctx.translate(canvas.width / 2, canvas.height / 2);
        ctx.rotate(rad);
        ctx.drawImage(img, -img.width / 2, -img.height / 2, img.width, img.height);
        canvas.toBlob(blob => callback(null, blob), 'image/jpeg', 0.9);
    };
    img.onerror = () => callback(new Error('Failed to load image'));
    img.src = url;
}

// ==================== ГЛОБАЛЬНЫЕ ПЕРЕМЕННЫЕ ====================
let filesList = [];
let currentIndex = 0;
let activeRequests = 0;
let isAnimating = false;

// DOM
const fileInput = document.getElementById('fileInput');
const cameraInput = document.getElementById('cameraInput');
const commitMsgInput = document.getElementById('commitMsg');
const uploadBtn = document.getElementById('uploadBtn');
const uploadCameraBtn = document.getElementById('uploadCameraBtn');
const refreshBtn = document.getElementById('refreshBtn');
const statusDiv = document.getElementById('status');
const cardStack = document.getElementById('cardStack');
const prevBtn = document.getElementById('prevBtn');
const nextBtn = document.getElementById('nextBtn');
const firstBtn = document.getElementById('firstBtn');
const lastBtn = document.getElementById('lastBtn');
const themeToggle = document.getElementById('themeToggle');
const loadingOverlay = document.getElementById('loadingOverlay');
const viewPanel = document.getElementById('viewPanel');
const uploadPanel = document.getElementById('uploadPanel');
const tabBtns = document.querySelectorAll('.tab-btn');

// Кеш
const imageCache = new Map();
const rotationMap = new Map();

// ==================== УТИЛИТЫ ====================
function showLoading() { activeRequests++; loadingOverlay.style.display = 'flex'; }
function hideLoading() { activeRequests--; if (activeRequests <= 0) { activeRequests = 0; loadingOverlay.style.display = 'none'; } }
async function withLoading(promise) { showLoading(); try { return await promise; } finally { hideLoading(); } }
function showStatus(msg, isError = false) {
    console.log(isError ? `❌ ${msg}` : `✅ ${msg}`);
    statusDiv.textContent = msg;
    statusDiv.style.background = isError ? '#ffebee' : 'var(--card-bg)';
    setTimeout(() => {
        if (statusDiv.textContent === msg) statusDiv.textContent = 'Готово';
        statusDiv.style.background = 'var(--card-bg)';
    }, 3000);
}
// Табы
tabBtns.forEach(btn => {
    btn.addEventListener('click', () => {
        tabBtns.forEach(b => b.classList.remove('active'));
        btn.classList.add('active');
        const tabId = btn.dataset.tab;
        viewPanel.classList.toggle('active', tabId === 'view');
        uploadPanel.classList.toggle('active', tabId === 'upload');
    });
});

// ==================== API ====================
async function fetchFiles() {
    console.log('Fetching files...');
    const res = await fetch(`${API_BASE}/files`);
    if (!res.ok) throw new Error('Не удалось получить список');
    const data = await res.json();
    console.log(`Received ${data.length} files`);
    return data;
}
async function uploadFile(file, commitMsg) {
    const fd = new FormData();
    fd.append('file', file);
    if (commitMsg) fd.append('message', commitMsg);
    const res = await fetch(`${API_BASE}/upload`, { method: 'POST', body: fd });
    if (!res.ok) throw new Error(await res.text());
    return res.json();
}
async function uploadMultipleFiles(files, commitMsg) {
    let successCount = 0;
    let failCount = 0;
    for (let i = 0; i < files.length; i++) {
        const file = files[i];
        try {
            await uploadFile(file, commitMsg);
            successCount++;
            showStatus(`Загружено ${successCount}/${files.length}`, false);
        } catch (err) {
            console.error(`Failed to upload ${file.name}:`, err);
            failCount++;
            showStatus(`Ошибка ${file.name}: ${err.message}`, true);
        }
    }
    return { successCount, failCount };
}
async function renameFile(oldName, newName) {
    const res = await fetch(`${API_BASE}/rename`, { method: 'PUT', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ old_name: oldName, new_name: newName }) });
    if (!res.ok) throw new Error(await res.text());
    return res.json();
}
async function loadImageBlob(filename) {
    const res = await fetch(`${API_BASE}/file/${encodeURIComponent(filename)}`);
    if (!res.ok) throw new Error(`Ошибка загрузки ${filename}`);
    return await res.blob();
}
async function preloadImage(filename, forceReload = false) {
    if (!forceReload && imageCache.has(filename)) return imageCache.get(filename);
    if (imageCache.has(filename)) {
        const old = imageCache.get(filename);
        if (old.url) URL.revokeObjectURL(old.url);
        imageCache.delete(filename);
    }
    const blob = await loadImageBlob(filename);
    const url = URL.createObjectURL(blob);
    const data = { blob, url };
    imageCache.set(filename, data);
    return data;
}
async function preloadNeighbors() {
    const indices = [currentIndex + 1, currentIndex + 2, currentIndex - 1, currentIndex - 2];
    const unique = [...new Set(indices)].filter(i => i >= 0 && i < filesList.length && i !== currentIndex);
    for (let i of unique) {
        const file = filesList[i];
        if (file && !imageCache.has(file.name)) preloadImage(file.name).catch(e => console.warn);
    }
}
async function saveRotationForFile(file, rotationDeg) {
    if (rotationDeg % 360 === 0) return false;
    const cached = await preloadImage(file.name);
    const rotatedBlob = await new Promise((resolve, reject) => rotateImageBlob(cached.blob, rotationDeg, (err, blob) => err ? reject(err) : resolve(blob)));
    await uploadFile(new File([rotatedBlob], file.name, { type: 'image/jpeg' }), `Rotate ${file.name} by ${rotationDeg}°`);
    if (imageCache.has(file.name)) {
        const old = imageCache.get(file.name);
        if (old.url) URL.revokeObjectURL(old.url);
        imageCache.delete(file.name);
    }
    await preloadImage(file.name, true);
    rotationMap.delete(file.name);
    return true;
}

// ==================== РЕНДЕРИНГ КАРТОЧЕК МГНОВЕННО (с кешем) ====================
let currentCardElement = null;

function animateTransition(newCardCallback) {
    if (isAnimating) return Promise.resolve();
    isAnimating = true;
    const oldCard = currentCardElement;
    if (oldCard) {
        oldCard.style.transition = 'transform 0.2s ease, opacity 0.2s ease';
        oldCard.style.transform = 'translateX(100%) scale(0.9)';
        oldCard.style.opacity = '0';
        setTimeout(() => { if (oldCard && oldCard.parentNode) oldCard.remove(); }, 200);
    }
    const result = newCardCallback();
    const promise = result && result.then ? result : Promise.resolve(result);
    return promise.then(card => {
        if (!card) return;
        currentCardElement = card;
        card.style.transition = 'transform 0.25s cubic-bezier(0.2,0.9,0.4,1.1), opacity 0.2s';
        card.style.transform = 'translateX(0) scale(1)';
        card.style.opacity = '1';
        setTimeout(() => { isAnimating = false; }, 250);
    });
}

function renderCurrentCardSync() {
    if (!filesList.length) {
        cardStack.innerHTML = '<div class="empty-state">📭 Нет файлов в папке photos/<br>Загрузите изображение</div>';
        currentCardElement = null;
        return null;
    }
    const file = filesList[currentIndex];
    const rot = rotationMap.get(file.name) || 0;
    const cached = imageCache.get(file.name);
    const imgSrc = cached ? cached.url : '';
    const imgAlt = cached ? file.name : 'Загрузка...';
    const html = `
        <div class="tinder-card" data-filename="${escapeHtml(file.name)}" data-rotation="${rot}">
            <div class="card-inner">
                <div class="card-image-wrapper">
                    <img class="card-image" src="${imgSrc}" alt="${imgAlt}" style="transform: rotate(${rot}deg);">
                </div>
                <div class="card-info">
                    <span class="file-name">${escapeHtml(file.name)}</span>
                </div>
                <div class="card-actions">
                    <div class="rotate-group">
                        <button class="rotate-left">↺ 90°</button>
                        <button class="rotate-right">↻ 90°</button>
                    </div>
                    <button class="save-rotation">💾</button>
                    <button class="rename-card">✏️</button>
                </div>
            </div>
        </div>
    `;
    const temp = document.createElement('div');
    temp.innerHTML = html;
    const card = temp.firstElementChild;
    attachCardEvents(card, file);
    cardStack.innerHTML = '';
    cardStack.appendChild(card);
    currentCardElement = card;

    if (!cached) {
        preloadImage(file.name).then(cached => {
            const img = card.querySelector('.card-image');
            if (img && img.src !== cached.url) {
                img.src = cached.url;
                img.alt = file.name;
            }
        }).catch(e => {
            const img = card.querySelector('.card-image');
            if (img) img.alt = 'Ошибка';
        });
    }
    preloadNeighbors();
    return card;
}

async function updateCardsWithAnimation(newIndex) {
    if (newIndex === currentIndex) return;
    currentIndex = newIndex;
    await animateTransition(() => renderCurrentCardSync());
}

async function goToPrev() { if (filesList.length) await updateCardsWithAnimation(currentIndex > 0 ? currentIndex - 1 : filesList.length - 1); }
async function goToNext() { if (filesList.length) await updateCardsWithAnimation(currentIndex < filesList.length - 1 ? currentIndex + 1 : 0); }
async function goToFirst() { if (filesList.length && currentIndex !== 0) await updateCardsWithAnimation(0); }
async function goToLast() { if (filesList.length && currentIndex !== filesList.length - 1) await updateCardsWithAnimation(filesList.length - 1); }

// События карточки
function attachCardEvents(card, file) {
    const rotateLeft = card.querySelector('.rotate-left');
    const rotateRight = card.querySelector('.rotate-right');
    const saveRot = card.querySelector('.save-rotation');
    const renameBtn = card.querySelector('.rename-card');
    const img = card.querySelector('.card-image');
    const wrapper = card.querySelector('.card-image-wrapper');
    const fileNameSpan = card.querySelector('.file-name');
    let currentRotation = rotationMap.get(file.name) || 0;
    if (img) img.style.transform = `rotate(${currentRotation}deg)`;

    function updateRotation(angle) {
        currentRotation = ((angle % 360) + 360) % 360;
        const isZoomed = wrapper && wrapper.classList.contains('zoomed');
        if (img) img.style.transform = isZoomed ? `rotate(${currentRotation}deg) scale(3)` : `rotate(${currentRotation}deg)`;
        rotationMap.set(file.name, currentRotation);
    }
    rotateLeft.addEventListener('click', (e) => { e.stopPropagation(); updateRotation(currentRotation - 90); });
    rotateRight.addEventListener('click', (e) => { e.stopPropagation(); updateRotation(currentRotation + 90); });
    saveRot.addEventListener('click', async (e) => {
        e.stopPropagation();
        if (currentRotation % 360 === 0) { showStatus('Не повёрнуто'); return; }
        try {
            const nextNext = (currentIndex + 1 < filesList.length) ? filesList[currentIndex + 1].name : null;
            await saveRotationForFile(file, currentRotation);
            showStatus('✅ Поворот сохранён');
            await refreshAndNavigate(nextNext);
        } catch (err) { showStatus(`❌ ${err.message}`, true); }
    });
    renameBtn.addEventListener('click', (e) => { e.stopPropagation(); startInlineRename(card, file); });
    fileNameSpan.addEventListener('click', (e) => { e.stopPropagation(); startInlineRename(card, file); });
    img.addEventListener('click', (e) => {
        e.stopPropagation();
        if (!wrapper) return;
        const isZoomed = wrapper.classList.contains('zoomed');
        if (isZoomed) {
            wrapper.classList.remove('zoomed');
            if (img) img.style.transform = `rotate(${currentRotation}deg)`;
        } else {
            wrapper.classList.add('zoomed');
            const rect = img.getBoundingClientRect();
            const x = ((e.clientX - rect.left) / rect.width) * 100;
            const y = ((e.clientY - rect.top) / rect.height) * 100;
            img.style.transformOrigin = `${Math.min(100, Math.max(0, x))}% ${Math.min(100, Math.max(0, y))}%`;
            img.style.transform = `rotate(${currentRotation}deg) scale(3)`;
        }
    });
}

// Переименование inline (без лоадера)
function startInlineRename(card, file) {
    if (card.querySelector('.rename-input')) return;
    const oldName = file.name;
    const input = document.createElement('input');
    input.value = oldName.replace(/\.jpg$/i, '');
    input.className = 'rename-input';
    const container = card.querySelector('.card-info');
    const nameSpan = container.querySelector('.file-name');
    nameSpan.style.display = 'none';
    container.insertBefore(input, nameSpan.nextSibling);
    input.focus();
    input.select();

    const commit = async () => {
        let newBase = input.value.trim();
        if (!newBase) { cleanup(); return; }
        let newName = ensureJpgExtension(newBase);
        if (newName === oldName) { cleanup(); return; }
        try {
            const angle = rotationMap.get(oldName) || 0;
            if (angle % 360 !== 0) await saveRotationForFile(file, angle);
            const nextFile = (currentIndex + 1 < filesList.length) ? filesList[currentIndex + 1].name : null;
            await renameFile(oldName, newName);
            showStatus(`✅ Переименован: ${oldName} → ${newName}`);
            await refreshAndNavigate(nextFile);
        } catch (err) { showStatus(`❌ ${err.message}`, true); }
        finally { cleanup(); }
    };
    const cleanup = () => { input.remove(); nameSpan.style.display = 'inline'; };
    input.addEventListener('blur', commit);
    input.addEventListener('keypress', e => { if (e.key === 'Enter') commit(); });
}

// Обновление и навигация (без лоадера)
async function refreshAndNavigate(nextFileName) {
    await loadFilesAndRefresh();
    if (nextFileName) {
        const idx = filesList.findIndex(f => f.name === nextFileName);
        if (idx !== -1 && idx !== currentIndex) {
            currentIndex = idx;
            renderCurrentCardSync();
        }
    }
}

// Загрузка списка (без лоадера)
async function loadFilesAndRefresh() {
    try {
        const fetched = await fetchFiles();
        filesList = fetched;
        if (filesList.length === 0) currentIndex = 0;
        else if (currentIndex >= filesList.length) currentIndex = 0;
        for (let fn of imageCache.keys()) {
            if (!filesList.some(f => f.name === fn)) {
                const old = imageCache.get(fn);
                if (old.url) URL.revokeObjectURL(old.url);
                imageCache.delete(fn);
                rotationMap.delete(fn);
            }
        }
        renderCurrentCardSync();
        showStatus(`Загружено ${filesList.length} файлов`);
    } catch (err) {
        showStatus(`Ошибка загрузки: ${err.message}`, true);
        filesList = [];
        renderCurrentCardSync();
    }
}

// ==================== МНОЖЕСТВЕННАЯ ЗАГРУЗКА ====================
uploadBtn.addEventListener('click', async () => {
    const files = Array.from(fileInput.files);
    if (!files.length) { showStatus('Выберите файлы', true); return; }
    const commitMsg = commitMsgInput.value;
    try {
        showLoading();
        const { successCount, failCount } = await uploadMultipleFiles(files, commitMsg);
        showStatus(`✅ Загружено ${successCount} из ${files.length} файлов${failCount ? `, ошибок: ${failCount}` : ''}`);
        fileInput.value = '';
        commitMsgInput.value = '';
        await loadFilesAndRefresh();
        if (filesList.length) {
            currentIndex = filesList.length - 1;
            renderCurrentCardSync();
        }
    } catch (err) {
        showStatus(`❌ Ошибка: ${err.message}`, true);
    } finally {
        hideLoading();
    }
});

uploadCameraBtn.addEventListener('click', () => { cameraInput.click(); });
cameraInput.addEventListener('change', async (e) => {
    if (!cameraInput.files.length) return;
    const file = cameraInput.files[0];
    const commitMsg = commitMsgInput.value;
    try {
        await withLoading(uploadFile(file, commitMsg));
        showStatus(`✅ Фото с камеры "${file.name}" загружено`);
        cameraInput.value = '';
        commitMsgInput.value = '';
        await loadFilesAndRefresh();
        if (filesList.length) {
            currentIndex = filesList.length - 1;
            renderCurrentCardSync();
        }
    } catch (err) {
        showStatus(`❌ Ошибка: ${err.message}`, true);
    }
});

prevBtn.addEventListener('click', goToPrev);
nextBtn.addEventListener('click', goToNext);
firstBtn.addEventListener('click', goToFirst);
lastBtn.addEventListener('click', goToLast);

// Тема
function getSystemTheme() { return window.matchMedia('(prefers-color-scheme: dark)').matches ? 'dark' : 'light'; }
function applyTheme(theme) {
    document.body.classList.toggle('dark', theme === 'dark');
    themeToggle.textContent = theme === 'dark' ? '☀️' : '🌙';
    localStorage.setItem('theme', theme);
}
function toggleTheme() { applyTheme(document.body.classList.contains('dark') ? 'light' : 'dark'); }
const saved = localStorage.getItem('theme');
applyTheme(saved === 'light' || saved === 'dark' ? saved : getSystemTheme());
themeToggle.addEventListener('click', toggleTheme);

loadFilesAndRefresh();
if ('serviceWorker' in navigator) navigator.serviceWorker.register('/sw.js').catch(e => console.log);
// ==================== АВТООБНОВЛЕНИЕ PWA ====================
if ('serviceWorker' in navigator) {
    navigator.serviceWorker.register('/sw.js').then(reg => {
        console.log('SW registered:', reg);
        // Отслеживаем новые версии
        reg.addEventListener('updatefound', () => {
            const newWorker = reg.installing;
            console.log('New SW found, state:', newWorker.state);
            newWorker.addEventListener('statechange', () => {
                if (newWorker.state === 'installed' && navigator.serviceWorker.controller) {
                    // Новая версия готова, но не активирована, предлагаем обновить страницу
                    showUpdateNotification();
                }
            });
        });
    }).catch(err => console.log('SW registration failed:', err));

    // Обрабатываем сообщения от SW (например, после активации)
    navigator.serviceWorker.addEventListener('message', event => {
        if (event.data && event.data.type === 'SW_UPDATED') {
            console.log('Received SW_UPDATED message');
            showUpdateNotification();
        }
    });
}

function showUpdateNotification() {
    // Создаём уведомление поверх интерфейса
    const notification = document.createElement('div');
    notification.textContent = '🔄 Доступна новая версия приложения. Обновить?';
    notification.style.position = 'fixed';
    notification.style.bottom = '20px';
    notification.style.left = '20px';
    notification.style.right = '20px';
    notification.style.backgroundColor = 'var(--button-bg)';
    notification.style.color = 'white';
    notification.style.padding = '12px 20px';
    notification.style.borderRadius = '40px';
    notification.style.display = 'flex';
    notification.style.justifyContent = 'space-between';
    notification.style.alignItems = 'center';
    notification.style.zIndex = '10001';
    notification.style.boxShadow = '0 2px 10px rgba(0,0,0,0.2)';
    notification.style.gap = '12px';
    notification.style.flexWrap = 'wrap';
    notification.innerHTML = `
        <span>🔄 Новая версия приложения</span>
        <button id="updateBtn" style="background: white; color: black; border: none; padding: 6px 16px; border-radius: 40px; cursor: pointer;">Обновить</button>
    `;
    document.body.appendChild(notification);
    document.getElementById('updateBtn').addEventListener('click', () => {
        window.location.reload();
    });
    // Автоматически скрыть через 30 секунд, если пользователь не нажал
    setTimeout(() => {
        if (notification.parentNode) notification.remove();
    }, 30000);
}