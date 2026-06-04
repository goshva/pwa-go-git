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
    return sanitizeFilename(name.replace(/\.(jpg|jpeg|png|gif|bmp|webp)$/i, '') + '.jpg');
}
function sanitizeFilename(name) {
    if (!name) return '';
    return String(name).replace(/\0/g, '').trim();
}
async function blobWithoutNullBytes(blob) {
    const buf = await blob.arrayBuffer();
    const bytes = new Uint8Array(buf);
    let hasNull = false;
    for (let i = 0; i < bytes.length; i++) {
        if (bytes[i] === 0) { hasNull = true; break; }
    }
    if (!hasNull) return blob;
    const clean = new Uint8Array(bytes.filter(b => b !== 0));
    return new Blob([clean], { type: blob.type || 'image/jpeg' });
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
        canvas.toBlob(blob => {
            if (!blob) return callback(new Error('Не удалось создать изображение'));
            callback(null, blob);
        }, 'image/jpeg', 0.92);
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
const cameraInput = document.getElementById('cameraInput');  // скрытый input
const commitMsgInput = document.getElementById('commitMsg');
const uploadBtn = document.getElementById('uploadBtn');
const uploadCameraBtn = document.getElementById('uploadCameraBtn');
const refreshBtn = document.getElementById('refreshBtn');
const positionBadge = document.getElementById('positionBadge');
const nextPhotoBtn = document.getElementById('nextPhotoBtn');
const loadingText = document.getElementById('loadingText');
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
function setLoadingText(text) {
    if (loadingText) loadingText.textContent = text || 'Загрузка...';
}
function showLoading(text) {
    activeRequests++;
    if (text) setLoadingText(text);
    loadingOverlay.style.display = 'flex';
    loadingOverlay.setAttribute('aria-hidden', 'false');
}
function hideLoading() {
    activeRequests--;
    if (activeRequests <= 0) {
        activeRequests = 0;
        loadingOverlay.style.display = 'none';
        loadingOverlay.setAttribute('aria-hidden', 'true');
        setLoadingText('Загрузка...');
    }
}
async function withLoading(promise, text) {
    showLoading(text);
    try { return await promise; } finally { hideLoading(); }
}
function showStatus(msg, isError = false) {
    console.log(isError ? `❌ ${msg}` : `✅ ${msg}`);
    statusDiv.textContent = msg;
    statusDiv.classList.toggle('error', isError);
    setTimeout(() => {
        if (statusDiv.textContent === msg) {
            statusDiv.textContent = 'Готово';
            statusDiv.classList.remove('error');
        }
    }, 3500);
}
function updatePositionBadge() {
    const label = filesList.length
        ? `${currentIndex + 1} / ${filesList.length}`
        : '—';
    if (positionBadge) positionBadge.textContent = label;
}
function formatPositionLabel(index) {
    if (!filesList.length) return '';
    return `№ ${index + 1} из ${filesList.length}`;
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
    const safeName = sanitizeFilename(file.name);
    const cleanBlob = await blobWithoutNullBytes(file);
    const safeFile = new File([cleanBlob], safeName, { type: file.type || 'image/jpeg' });
    const fd = new FormData();
    fd.append('file', safeFile);
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
    const cleanBlob = await blobWithoutNullBytes(rotatedBlob);
    const safeName = sanitizeFilename(file.name);
    await uploadFile(new File([cleanBlob], safeName, { type: 'image/jpeg' }), `Rotate ${safeName} by ${rotationDeg}°`);
    if (imageCache.has(file.name)) {
        const old = imageCache.get(file.name);
        if (old.url) URL.revokeObjectURL(old.url);
        imageCache.delete(file.name);
    }
    await preloadImage(file.name, true);
    rotationMap.delete(file.name);
    return true;
}

// ==================== РЕНДЕРИНГ КАРТОЧЕК ====================
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
        cardStack.innerHTML = '<div class="empty-state">📭 Нет файлов в photos/<br>Сфотографируйте или загрузите изображение</div>';
        currentCardElement = null;
        updatePositionBadge();
        return null;
    }
    const file = filesList[currentIndex];
    const rot = rotationMap.get(file.name) || 0;
    const cached = imageCache.get(file.name);
    const imgSrc = cached ? cached.url : '';
    const imgAlt = cached ? file.name : 'Загрузка...';
    const positionLabel = formatPositionLabel(currentIndex);
    const html = `
        <div class="tinder-card${cached ? '' : ' is-loading'}" data-filename="${escapeHtml(file.name)}" data-rotation="${rot}">
            <div class="card-inner">
                <div class="card-image-wrapper">
                    <img class="card-image" src="${imgSrc}" alt="${imgAlt}" style="transform: rotate(${rot}deg);">
                </div>
                <div class="card-info">
                    <span class="file-position">${escapeHtml(positionLabel)}</span>
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
            card.classList.remove('is-loading');
            const img = card.querySelector('.card-image');
            if (img && img.src !== cached.url) {
                img.src = cached.url;
                img.alt = file.name;
            }
        }).catch(() => {
            card.classList.remove('is-loading');
            const img = card.querySelector('.card-image');
            if (img) img.alt = 'Ошибка';
        });
    }
    updatePositionBadge();
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
            showLoading('Сохранение поворота...');
            saveRot.disabled = true;
            await saveRotationForFile(file, currentRotation);
            showStatus('✅ Поворот сохранён');
            await refreshInPlace(file.name);
        } catch (err) { showStatus(`❌ ${err.message}`, true); }
        finally {
            hideLoading();
            saveRot.disabled = false;
        }
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

// Переименование inline
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
            showLoading('Переименование...');
            const angle = rotationMap.get(oldName) || 0;
            if (angle % 360 !== 0) await saveRotationForFile(file, angle);
            await renameFile(oldName, newName);
            showStatus(`✅ Переименован: ${oldName} → ${newName}`);
            await refreshInPlace(newName);
        } catch (err) { showStatus(`❌ ${err.message}`, true); }
        finally { hideLoading(); }
        finally { cleanup(); }
    };
    const cleanup = () => { input.remove(); nameSpan.style.display = 'inline'; };
    input.addEventListener('blur', commit);
    input.addEventListener('keypress', e => { if (e.key === 'Enter') commit(); });
}

async function refreshInPlace(keepFileName) {
    const prevName = keepFileName || (filesList[currentIndex] && filesList[currentIndex].name);
    await loadFilesAndRefresh({ silent: true });
    if (prevName) {
        const idx = filesList.findIndex(f => f.name === prevName);
        if (idx !== -1) currentIndex = idx;
    }
    renderCurrentCardSync();
    updatePositionBadge();
}

async function loadFilesAndRefresh(opts = {}) {
    try {
        const fetched = await fetchFiles();
        filesList = fetched;
        if (filesList.length === 0) currentIndex = 0;
        else if (currentIndex >= filesList.length) currentIndex = filesList.length - 1;
        for (let fn of imageCache.keys()) {
            if (!filesList.some(f => f.name === fn)) {
                const old = imageCache.get(fn);
                if (old.url) URL.revokeObjectURL(old.url);
                imageCache.delete(fn);
                rotationMap.delete(fn);
            }
        }
        renderCurrentCardSync();
        updatePositionBadge();
        if (!opts.silent) showStatus(`В списке ${filesList.length} файлов`);
    } catch (err) {
        showStatus(`Ошибка загрузки: ${err.message}`, true);
        filesList = [];
        renderCurrentCardSync();
        updatePositionBadge();
    }
}

// ==================== ЗАГРУЗКА С КАМЕРЫ (СТАБИЛЬНАЯ) ====================
// Гарантированное открытие камеры через создание свежего input
// Загрузка с камеры (стабильная + сжатие)
function triggerCameraUpload() {
    const oldInput = document.getElementById('_cameraInputDynamic');
    if (oldInput) oldInput.remove();

    const input = document.createElement('input');
    input.type = 'file';
    input.accept = 'image/*';
    input.capture = 'environment';
    input.id = '_cameraInputDynamic';
    input.style.display = 'none';
    document.body.appendChild(input);

    input.addEventListener('change', async (e) => {
        const file = e.target.files[0];
        if (!file) return;

        uploadCameraBtn.disabled = true;
        nextPhotoBtn.hidden = true;
        showLoading('Сжатие и отправка...');
        showStatus('Сжатие фото...');

        try {
            const compressed = await compressToMaxSize(file, 300);
            setLoadingText('Отправка на сервер...');
            showStatus('Отправка на сервер...');
            await uploadFile(compressed, '');
            showStatus('✅ Фото загружено — можно снять следующее');
            await loadFilesAndRefresh({ silent: true });
            if (filesList.length) currentIndex = filesList.length - 1;
            updatePositionBadge();
            nextPhotoBtn.hidden = false;
        } catch (err) {
            showStatus(`❌ Ошибка загрузки: ${err.message}`, true);
        } finally {
            hideLoading();
            uploadCameraBtn.disabled = false;
            input.remove();
        }
    });

    input.click();
}
// Назначаем обработчик на кнопку
uploadCameraBtn.addEventListener('click', triggerCameraUpload);
nextPhotoBtn.addEventListener('click', triggerCameraUpload);
refreshBtn.addEventListener('click', () => withLoading(loadFilesAndRefresh(), 'Обновление списка...'));

// ==================== МНОЖЕСТВЕННАЯ ЗАГРУЗКА (как раньше) ====================
uploadBtn.addEventListener('click', async () => {
    const files = Array.from(fileInput.files);
    if (!files.length) { showStatus('Выберите файлы', true); return; }
    try {
        showLoading('Загрузка файлов...');
        const { successCount, failCount } = await uploadMultipleFiles(files, '');
        showStatus(`✅ Загружено ${successCount} из ${files.length}${failCount ? `, ошибок: ${failCount}` : ''}`);
        fileInput.value = '';
        await loadFilesAndRefresh({ silent: true });
        if (filesList.length) {
            currentIndex = filesList.length - successCount;
            if (currentIndex < 0) currentIndex = 0;
            renderCurrentCardSync();
        }
    } catch (err) {
        showStatus(`❌ Ошибка: ${err.message}`, true);
    } finally {
        hideLoading();
    }
});

// Навигация
prevBtn.addEventListener('click', goToPrev);
nextBtn.addEventListener('click', goToNext);
firstBtn.addEventListener('click', goToFirst);
lastBtn.addEventListener('click', goToLast);
//
// Сжатие JPEG/PNG до целевого размера с минимальной потерей качества
async function compressToMaxSize(file, maxSizeKB = 300) {
  // Если файл уже маленький, возвращаем как есть
  if (file.size <= maxSizeKB * 1024) return file;

  // Создаём изображение из blob
  const img = await new Promise((resolve, reject) => {
    const img = new Image();
    img.onload = () => resolve(img);
    img.onerror = reject;
    img.src = URL.createObjectURL(file);
  });

  // Рисуем на canvas для пережатия
  const canvas = document.createElement('canvas');
  canvas.width = img.width;
  canvas.height = img.height;
  const ctx = canvas.getContext('2d');
  ctx.drawImage(img, 0, 0);

  // Подбираем качество от 0.9 до 0.3 с шагом 0.1, затем уточняем
  let quality = 0.9;
  let blob = await new Promise(resolve => canvas.toBlob(resolve, 'image/jpeg', quality));
  
  // Быстрое снижение качества, если нужно
  while (blob.size > maxSizeKB * 1024 && quality > 0.3) {
    quality -= 0.1;
    blob = await new Promise(resolve => canvas.toBlob(resolve, 'image/jpeg', quality));
  }
  
  // Если всё ещё велик, уменьшаем разрешение на 20%
  if (blob.size > maxSizeKB * 1024) {
    const scale = Math.sqrt((maxSizeKB * 1024) / blob.size) * 0.9;
    canvas.width = Math.round(img.width * scale);
    canvas.height = Math.round(img.height * scale);
    ctx.drawImage(img, 0, 0, canvas.width, canvas.height);
    blob = await new Promise(resolve => canvas.toBlob(resolve, 'image/jpeg', 0.8));
  }

  URL.revokeObjectURL(img.src);
  const name = sanitizeFilename(file.name.replace(/\.(png|gif|bmp|webp)$/i, '.jpg'));
  const cleanBlob = await blobWithoutNullBytes(blob);
  return new File([cleanBlob], name, { type: 'image/jpeg' });
}
// Тема
function getSystemTheme() { return window.matchMedia('(prefers-color-scheme: dark)').matches ? 'dark' : 'light'; }
function applyTheme(theme) {
    document.body.classList.toggle('dark', theme === 'dark');
    themeToggle.textContent = theme === 'dark' ? '☀️' : '🌙';
    localStorage.setItem('theme', theme);
    const meta = document.getElementById('theme-color-meta');
    if (meta) meta.content = theme === 'dark' ? '#0f2e26' : '#1a5f4a';
}
function toggleTheme() { applyTheme(document.body.classList.contains('dark') ? 'light' : 'dark'); }
const saved = localStorage.getItem('theme');
applyTheme(saved === 'light' || saved === 'dark' ? saved : getSystemTheme());
themeToggle.addEventListener('click', toggleTheme);

// Инициализация
withLoading(loadFilesAndRefresh(), 'Загрузка списка...');

// ==================== АВТООБНОВЛЕНИЕ PWA ====================
if ('serviceWorker' in navigator) {
    navigator.serviceWorker.register('/sw.js').then(reg => {
        console.log('SW registered:', reg);
        reg.addEventListener('updatefound', () => {
            const newWorker = reg.installing;
            console.log('New SW found, state:', newWorker.state);
            newWorker.addEventListener('statechange', () => {
                if (newWorker.state === 'installed' && navigator.serviceWorker.controller) {
                    showUpdateNotification();
                }
            });
        });
    }).catch(err => console.log('SW registration failed:', err));

    navigator.serviceWorker.addEventListener('message', event => {
        if (event.data && event.data.type === 'SW_UPDATED') {
            console.log('Received SW_UPDATED message');
            showUpdateNotification();
        }
    });
}

function showUpdateNotification() {
    const notification = document.createElement('div');
    notification.style.position = 'fixed';
    notification.style.bottom = '20px';
    notification.style.left = '20px';
    notification.style.right = '20px';
    notification.style.backgroundColor = '#4CAF50';
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
    setTimeout(() => {
        if (notification.parentNode) notification.remove();
    }, 30000);
}