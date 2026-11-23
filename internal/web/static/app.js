import { VideoRTC } from '/go2rtc/video-rtc.js';

class FeatherNVRPlayer extends VideoRTC {
  set statusText(val) {
    if (!this.statusEl) return;
    this.statusEl.textContent = val || '';
    this.statusEl.classList.toggle('hidden', !val);
  }

  set errorText(val) {
    if (!this.statusEl) return;
    const raw = String(val || '');
    const lower = raw.toLowerCase();
    let msg = raw;
    if (lower.includes('codecs not matched') || lower.includes('h265') || lower.includes('hevc')) {
      msg = 'HEVC unsupported in this browser (use Edge/Chrome or set sub-stream to H.264)';
    }
    this.statusEl.textContent = msg;
    this.statusEl.classList.add('error');
    this.statusEl.classList.remove('hidden');
  }

  oninit() {
    super.oninit();
    this.video.autoplay = true;
    this.video.playsInline = true;
    this.video.controls = false;
    if (this.hasAttribute('grid')) {
      this.video.muted = true;
    }
    this.video.style.objectFit = 'contain';
    this.video.style.background = '#000';

    this.statusEl = document.createElement('div');
    this.statusEl.className = 'player-status hidden';
    this.appendChild(this.statusEl);
  }

  onconnect() {
    const ok = super.onconnect();
    if (ok && this.statusEl) {
      this.statusEl.classList.remove('error', 'playing');
      this.statusText = 'Loading...';
    }
    return ok;
  }

  ondisconnect() {
    super.ondisconnect();
    if (this.statusEl) {
      this.statusEl.classList.remove('error', 'playing');
      this.statusText = '';
    }
  }

  onopen() {
    const modes = super.onopen();
    this.onmessage['feathernvr'] = (msg) => {
      switch (msg.type) {
        case 'error':
          if (this.statusEl && !this.statusEl.classList.contains('playing')) {
            this.errorText = msg.value;
          }
          break;
        case 'mse':
        case 'hls':
        case 'mp4':
        case 'mjpeg':
          if (this.statusEl) {
            this.statusEl.classList.remove('error');
            this.statusEl.classList.add('playing');
          }
          this.statusText = msg.type.toUpperCase();
          break;
      }
    };
    return modes;
  }

  onpcvideo(video2) {
    super.onpcvideo(video2);
    if (this.pcState !== WebSocket.CLOSED && this.statusEl) {
      this.statusEl.classList.remove('error');
      this.statusEl.classList.add('playing');
      this.statusText = 'RTC';
    }
  }
}

if (!customElements.get('feathernvr-player')) {
  customElements.define('feathernvr-player', FeatherNVRPlayer);
}

function loadStoredCameraOrder() {
  try {
    const raw = JSON.parse(localStorage.getItem('feathernvr_cam_order') || '[]');
    if (Array.isArray(raw)) {
      return raw.filter((v) => typeof v === 'string' && v.trim() !== '');
    }
  } catch {}
  return [];
}

const state = {
  cameras: [],
  storage: null,
  uptimeSec: 0,
  gridCols: localStorage.getItem('feathernvr_cols_v2') || 'fit',
  recCols: localStorage.getItem('feathernvr_rec_cols_v1') || 'auto',
  cameraOrder: loadStoredCameraOrder(),
  editLayout: false,
  swapSelectedCam: null,
  tiles: new Map(), // camID -> { el, stage, zoomCtrl, player, streamName, configRev, nameEl, dot, recBadge, motBadge, ptzBadge, metaLeft, metaRight }
  activeModalCam: null,
  activeModalQuality: 'sub',
  modalPlayer: null,
  liveZoomCtrl: null,
  ptzPanelExpanded: true,
  activePtzBtn: null,
  recOffset: 0,
  recLimit: 40,
  recTotal: 0,
  daySession: null,
  clipZoomCtrl: null,
  statusTimer: null,
  recCameraSig: '',
  contextMenuCam: null,
  cameraDialog: {
    mode: 'add',
    id: null,
    idManual: false,
    ignoreZones: null,
  },
};

const qs = (sel) => document.querySelector(sel);

function formatBytes(bytes) {
  if (!bytes || bytes <= 0) return '0 MB';
  const gb = bytes / (1024 * 1024 * 1024);
  if (gb >= 1) return `${gb.toFixed(2)} GB`;
  const mb = bytes / (1024 * 1024);
  if (mb >= 1) return `${mb.toFixed(1)} MB`;
  return `${(bytes / 1024).toFixed(0)} KB`;
}

function formatDuration(sec) {
  sec = Math.max(0, Math.floor(sec || 0));
  const d = Math.floor(sec / 86400);
  const h = Math.floor((sec % 86400) / 3600);
  const m = Math.floor((sec % 3600) / 60);
  const s = sec % 60;
  if (d > 0) return `${d}d ${h}h`;
  if (h > 0) return `${h}h ${m}m`;
  if (m > 0) return `${m}m ${s}s`;
  return `${s}s`;
}

function formatUnix(ts) {
  if (!ts) return '--';
  return new Date(ts * 1000).toLocaleString();
}

function formatClockTime(unixSec) {
  if (!unixSec) return '--:--:--';
  return new Date(unixSec * 1000).toLocaleTimeString([], {
    hour12: false,
    hour: '2-digit',
    minute: '2-digit',
    second: '2-digit',
  });
}

function formatHM(unixSec) {
  if (!unixSec) return '--:--';
  return new Date(unixSec * 1000).toLocaleTimeString([], {
    hour12: false,
    hour: '2-digit',
    minute: '2-digit',
  });
}

function formatMMSS(sec) {
  sec = Math.max(0, Math.floor(sec || 0));
  const m = Math.floor(sec / 60);
  const s = sec % 60;
  return `${m}:${String(s).padStart(2, '0')}`;
}

function formatMMSSPrecise(sec) {
  sec = Math.max(0, Number(sec) || 0);
  const tenths = Math.round(sec * 10);
  const m = Math.floor(tenths / 600);
  const rem = (tenths % 600) / 10;
  return `${m}:${rem.toFixed(1).padStart(4, '0')}`;
}

function slugifyCameraName(name) {
  return String(name || '')
    .trim()
    .toLowerCase()
    .replace(/[^a-z0-9]+/g, '_')
    .replace(/^_+|_+$/g, '') || 'camera';
}

function escapeHTML(str) {
  return String(str ?? '').replace(/[&<>"']/g, (ch) => ({
    '&': '&amp;',
    '<': '&lt;',
    '>': '&gt;',
    '"': '&quot;',
    "'": '&#39;',
  }[ch]));
}

function wsStreamURL(streamName) {
  return `/go2rtc/api/ws?src=${encodeURIComponent(streamName)}`;
}

function isMobileViewport() {
  return window.matchMedia('(pointer: coarse)').matches
    || window.matchMedia('(hover: none)').matches
    || window.matchMedia('(max-width: 767px)').matches;
}

// Pan & Zoom controller (desktop only): wheel zoom [1.0, 8.0] (never < 1:1), drag pan when zoomed, tap callback when unmoved.
function createPanZoomController(viewportEl, stageEl, badgeEl, opts = {}) {
  const MIN_SCALE = 1;
  const MAX_SCALE = 8;
  let scale = 1;
  let tx = 0;
  let ty = 0;

  function clampTransform(nextScale, nextTx, nextTy) {
    const s = Math.min(MAX_SCALE, Math.max(MIN_SCALE, nextScale));
    if (s <= 1.001) {
      return { scale: 1, tx: 0, ty: 0 };
    }
    const rect = viewportEl.getBoundingClientRect();
    const maxTx = Math.max(0, (rect.width * (s - 1)) / 2);
    const maxTy = Math.max(0, (rect.height * (s - 1)) / 2);
    return {
      scale: s,
      tx: Math.max(-maxTx, Math.min(maxTx, nextTx)),
      ty: Math.max(-maxTy, Math.min(maxTy, nextTy)),
    };
  }

  function apply() {
    const zoomed = scale > 1.001;
    stageEl.style.transform = zoomed
      ? `translate(${tx.toFixed(1)}px, ${ty.toFixed(1)}px) scale(${scale.toFixed(3)})`
      : '';
    viewportEl.classList.toggle('zoomed', zoomed);
    if (badgeEl) {
      badgeEl.classList.toggle('hidden', !zoomed);
      if (zoomed) {
        badgeEl.textContent = `${scale.toFixed(1)}x · Reset`;
      }
    }
  }

  function reset() {
    scale = 1;
    tx = 0;
    ty = 0;
    viewportEl.classList.remove('panning');
    apply();
  }

  function zoomAround(targetScale, clientX, clientY) {
    const rect = viewportEl.getBoundingClientRect();
    const cx = rect.left + rect.width / 2;
    const cy = rect.top + rect.height / 2;
    const dx = clientX - cx;
    const dy = clientY - cy;

    const clampedScale = Math.min(MAX_SCALE, Math.max(MIN_SCALE, targetScale));
    const ratio = scale > 0 ? clampedScale / scale : 1;
    const nextTx = dx * (1 - ratio) + tx * ratio;
    const nextTy = dy * (1 - ratio) + ty * ratio;
    const clamped = clampTransform(clampedScale, nextTx, nextTy);
    scale = clamped.scale;
    tx = clamped.tx;
    ty = clamped.ty;
    apply();
  }

  viewportEl.addEventListener('wheel', (ev) => {
    if (isMobileViewport() || opts.isDisabled?.()) return;
    if (opts.allowPageScrollAt1x?.() && scale <= 1.001 && ev.deltaY > 0) {
      return;
    }
    ev.preventDefault();
    ev.stopPropagation();

    const rect = viewportEl.getBoundingClientRect();
    if (rect.width <= 0 || rect.height <= 0) return;

    let dy = ev.deltaY;
    if (ev.deltaMode === 1) dy *= 16;
    else if (ev.deltaMode === 2) dy *= rect.height;

    const factor = Math.exp(-dy * 0.0022);
    let nextScale = scale * factor;
    if (dy > 0 && nextScale < 1.02) nextScale = 1;
    zoomAround(nextScale, ev.clientX, ev.clientY);
  }, { passive: false });

  if (badgeEl) {
    badgeEl.addEventListener('click', (ev) => {
      ev.stopPropagation();
      reset();
    });
    badgeEl.addEventListener('pointerdown', (ev) => ev.stopPropagation());
    badgeEl.addEventListener('pointerup', (ev) => ev.stopPropagation());
  }

  const touchState = {
    activeIds: new Set(),
    startX: 0,
    startY: 0,
    moved: false,
    multi: false,
  };
  let mouseGesture = null;

  viewportEl.addEventListener('pointerdown', (ev) => {
    if (opts.isDisabled?.()) return;
    if (ev.target.closest?.('.zoom-badge')) return;

    if (ev.pointerType === 'touch' || isMobileViewport()) {
      touchState.activeIds.add(ev.pointerId);
      if (touchState.activeIds.size === 1) {
        touchState.startX = ev.clientX;
        touchState.startY = ev.clientY;
        touchState.moved = false;
        touchState.multi = false;
      } else {
        touchState.multi = true;
      }
      return;
    }

    if (ev.button > 0) return;
    try { viewportEl.setPointerCapture(ev.pointerId); } catch {}
    mouseGesture = {
      pointerId: ev.pointerId,
      startX: ev.clientX,
      startY: ev.clientY,
      startTx: tx,
      startTy: ty,
      moved: false,
    };
  });

  viewportEl.addEventListener('pointermove', (ev) => {
    if (opts.isDisabled?.()) return;

    if (ev.pointerType === 'touch' || isMobileViewport()) {
      if (!touchState.activeIds.has(ev.pointerId)) return;
      if (touchState.activeIds.size === 1 && !touchState.moved) {
        const dx = ev.clientX - touchState.startX;
        const dy = ev.clientY - touchState.startY;
        if (Math.hypot(dx, dy) > 10) {
          touchState.moved = true;
        }
      }
      return;
    }

    if (!mouseGesture || mouseGesture.pointerId !== ev.pointerId) return;
    const dx = ev.clientX - mouseGesture.startX;
    const dy = ev.clientY - mouseGesture.startY;
    if (Math.hypot(dx, dy) > 5) {
      mouseGesture.moved = true;
      if (scale > 1.001) {
        viewportEl.classList.add('panning');
        const clamped = clampTransform(scale, mouseGesture.startTx + dx, mouseGesture.startTy + dy);
        tx = clamped.tx;
        ty = clamped.ty;
        apply();
      }
    }
  });

  function finishPointer(ev, cancelled) {
    if (touchState.activeIds.has(ev.pointerId)) {
      touchState.activeIds.delete(ev.pointerId);
      if (touchState.activeIds.size === 0) {
        const shouldTap = !cancelled && !touchState.moved && !touchState.multi && !opts.isDisabled?.();
        touchState.moved = false;
        touchState.multi = false;
        if (shouldTap && opts.onTap) {
          opts.onTap(ev);
        }
      }
      return;
    }

    if (!mouseGesture || mouseGesture.pointerId !== ev.pointerId) return;
    try { viewportEl.releasePointerCapture(ev.pointerId); } catch {}
    viewportEl.classList.remove('panning');
    const shouldTap = !cancelled && !mouseGesture.moved && !opts.isDisabled?.();
    mouseGesture = null;
    if (shouldTap && opts.onTap) {
      opts.onTap(ev);
    }
  }

  viewportEl.addEventListener('pointerup', (ev) => finishPointer(ev, false));
  viewportEl.addEventListener('pointercancel', (ev) => finishPointer(ev, true));

  window.addEventListener('resize', () => {
    if (isMobileViewport() && scale > 1.001) {
      reset();
    }
  });

  function reclamp() {
    if (isMobileViewport()) {
      reset();
      return;
    }
    const clamped = clampTransform(scale, tx, ty);
    scale = clamped.scale;
    tx = clamped.tx;
    ty = clamped.ty;
    apply();
  }

  return {
    reset,
    reclamp,
    getScale: () => scale,
    apply,
  };
}

// Tabs
function switchTab(tabName) {
  document.querySelectorAll('.nav-btn').forEach((btn) => {
    btn.classList.toggle('active', btn.dataset.tab === tabName);
  });
  document.querySelectorAll('.tab-panel').forEach((panel) => {
    panel.classList.toggle('active', panel.id === `tab-${tabName}`);
  });
  syncBodyFitMode();
  if (tabName === 'live') {
    requestAnimationFrame(updateFitLayout);
  }
  if (tabName === 'recordings') {
    loadRecordings(false);
    requestAnimationFrame(updateRecFitLayout);
  }
}

document.querySelectorAll('.nav-btn').forEach((btn) => {
  btn.addEventListener('click', () => switchTab(btn.dataset.tab));
});

function syncBodyFitMode() {
  const liveActive = Boolean(qs('#tab-live')?.classList.contains('active'));
  const recActive = Boolean(qs('#tab-recordings')?.classList.contains('active'));
  const fitActive = (liveActive && state.gridCols === 'fit') || (recActive && state.recCols === 'fit');
  document.body.classList.toggle('fit-mode', fitActive);
  qs('.main-content')?.classList.toggle('fit-mode', fitActive);
}

function updateFitLayout() {
  syncBodyFitMode();
  const grid = qs('#camera-grid');
  if (!grid) return;
  const liveActive = Boolean(qs('#tab-live')?.classList.contains('active'));
  if (state.gridCols !== 'fit' || !liveActive) {
    grid.style.gridTemplateColumns = '';
    grid.style.gridTemplateRows = '';
    return;
  }
  const count = state.cameras.length || grid.children.length;
  if (!count) return;
  const w = grid.clientWidth;
  const h = grid.clientHeight;
  if (w <= 0 || h <= 0) return;

  const gap = parseFloat(getComputedStyle(grid).columnGap) || 6;
  const aspect = 16 / 9;
  let bestCols = 1;
  let bestRows = count;
  let bestArea = -1;

  for (let cols = 1; cols <= count; cols++) {
    const rows = Math.ceil(count / cols);
    const cellW = (w - gap * (cols - 1)) / cols;
    const cellH = (h - gap * (rows - 1)) / rows;
    if (cellW <= 0 || cellH <= 0) continue;
    const vidW = Math.min(cellW, cellH * aspect);
    const vidH = vidW / aspect;
    const area = vidW * vidH;
    if (area > bestArea + 0.5) {
      bestArea = area;
      bestCols = cols;
      bestRows = rows;
    }
  }

  grid.style.gridTemplateColumns = `repeat(${bestCols}, minmax(0, 1fr))`;
  grid.style.gridTemplateRows = `repeat(${bestRows}, minmax(0, 1fr))`;
}

function updateRecFitLayout() {
  syncBodyFitMode();
  const grid = qs('#recordings-list');
  if (!grid) return;
  const recActive = Boolean(qs('#tab-recordings')?.classList.contains('active'));
  if (state.recCols !== 'fit' || !recActive) {
    grid.style.gridTemplateColumns = '';
    grid.style.gridTemplateRows = '';
    return;
  }
  const count = grid.querySelectorAll('.rec-card').length;
  if (!count) {
    grid.style.gridTemplateColumns = '';
    grid.style.gridTemplateRows = '';
    return;
  }
  const w = grid.clientWidth;
  const h = grid.clientHeight;
  if (w <= 0 || h <= 0) return;

  const gap = parseFloat(getComputedStyle(grid).columnGap) || 6;
  const aspect = 16 / 9;
  const bodyReserveH = 44;
  let bestCols = 1;
  let bestRows = count;
  let bestArea = -1;

  for (let cols = 1; cols <= count; cols++) {
    const rows = Math.ceil(count / cols);
    const cellW = (w - gap * (cols - 1)) / cols;
    const cellH = (h - gap * (rows - 1)) / rows;
    if (cellW <= 0 || cellH <= 0) continue;
    const thumbMaxH = Math.max(24, cellH - bodyReserveH);
    const thumbW = Math.min(cellW, thumbMaxH * aspect);
    const thumbH = thumbW / aspect;
    const area = thumbW * thumbH;
    if (area > bestArea + 0.5) {
      bestArea = area;
      bestCols = cols;
      bestRows = rows;
    }
  }

  grid.style.gridTemplateColumns = `repeat(${bestCols}, minmax(0, 1fr))`;
  grid.style.gridTemplateRows = `repeat(${bestRows}, minmax(0, 1fr))`;
}

function updateAllFitLayouts() {
  updateFitLayout();
  updateRecFitLayout();
}

// Grid columns
function applyGridCols(cols) {
  state.gridCols = cols;
  localStorage.setItem('feathernvr_cols_v2', cols);
  const grid = qs('#camera-grid');
  grid.className = `camera-grid cols-${cols}`;
  grid.classList.toggle('editing', state.editLayout);
  document.querySelectorAll('#live-col-selector .chip').forEach((c) => {
    c.classList.toggle('active', c.dataset.cols === cols);
  });
  updateFitLayout();
  requestAnimationFrame(updateFitLayout);
}

function applyRecCols(cols) {
  state.recCols = cols;
  localStorage.setItem('feathernvr_rec_cols_v1', cols);
  const grid = qs('#recordings-list');
  if (grid) {
    grid.className = `recordings-grid cols-${cols}`;
  }
  document.querySelectorAll('#rec-col-selector .chip').forEach((c) => {
    c.classList.toggle('active', c.dataset.cols === cols);
  });
  updateRecFitLayout();
  requestAnimationFrame(updateRecFitLayout);
}

document.querySelectorAll('#live-col-selector .chip').forEach((c) => {
  c.addEventListener('click', () => applyGridCols(c.dataset.cols));
});
document.querySelectorAll('#rec-col-selector .chip').forEach((c) => {
  c.addEventListener('click', () => applyRecCols(c.dataset.cols));
});
window.addEventListener('resize', updateAllFitLayouts);
if (window.ResizeObserver && qs('.main-content')) {
  new ResizeObserver(updateAllFitLayouts).observe(qs('.main-content'));
}
applyGridCols(state.gridCols);
applyRecCols(state.recCols);

// Camera layout order & Edit Layout mode
function getEffectiveOrder(cameras) {
  const present = new Set((cameras || []).map((c) => c.id));
  const out = [];
  const seen = new Set();
  for (const id of state.cameraOrder) {
    if (present.has(id) && !seen.has(id)) {
      seen.add(id);
      out.push(id);
    }
  }
  for (const c of cameras || []) {
    if (!seen.has(c.id)) {
      seen.add(c.id);
      out.push(c.id);
    }
  }
  return out;
}

function applyCameraOrder() {
  const order = getEffectiveOrder(state.cameras);
  order.forEach((id, idx) => {
    const ref = state.tiles.get(id);
    if (ref) {
      ref.el.style.order = String(idx);
    }
  });
}

async function loadLayoutSettings() {
  try {
    const res = await fetch('/api/settings/layout', { cache: 'no-store' });
    if (!res.ok) return;
    const data = await res.json();
    if (Array.isArray(data.camera_order) && data.camera_order.length > 0) {
      state.cameraOrder = data.camera_order;
      localStorage.setItem('feathernvr_cam_order', JSON.stringify(state.cameraOrder));
      applyCameraOrder();
    }
  } catch {}
}

async function saveLayoutSettings() {
  try {
    localStorage.setItem('feathernvr_cam_order', JSON.stringify(state.cameraOrder));
  } catch {}
  try {
    await fetch('/api/settings/layout', {
      method: 'PUT',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ camera_order: state.cameraOrder }),
    });
  } catch {}
}

function clearSwapSelection() {
  if (!state.swapSelectedCam) return;
  const prev = state.tiles.get(state.swapSelectedCam);
  prev?.el.classList.remove('swap-selected');
  state.swapSelectedCam = null;
}

function swapCameras(idA, idB) {
  if (!idA || !idB || idA === idB) return;
  const order = getEffectiveOrder(state.cameras);
  const idxA = order.indexOf(idA);
  const idxB = order.indexOf(idB);
  if (idxA < 0 || idxB < 0) return;
  [order[idxA], order[idxB]] = [order[idxB], order[idxA]];
  state.cameraOrder = order;
  applyCameraOrder();
  saveLayoutSettings();
}

function setEditLayout(enabled) {
  state.editLayout = !!enabled;
  clearSwapSelection();
  const grid = qs('#camera-grid');
  grid?.classList.toggle('editing', state.editLayout);
  const btnEdit = qs('#btn-edit-layout');
  if (btnEdit) {
    btnEdit.classList.toggle('active', state.editLayout);
    btnEdit.textContent = state.editLayout ? 'Done Editing' : 'Edit Layout';
  }
  qs('#btn-reset-layout')?.classList.toggle('hidden', !state.editLayout);
  if (state.editLayout) {
    for (const ref of state.tiles.values()) {
      ref.zoomCtrl?.reset();
    }
  }
}

qs('#btn-edit-layout')?.addEventListener('click', () => setEditLayout(!state.editLayout));
qs('#btn-reset-layout')?.addEventListener('click', () => {
  clearSwapSelection();
  state.cameraOrder = state.cameras.map((c) => c.id);
  applyCameraOrder();
  saveLayoutSettings();
});

function attachTileDragHandlers(tile, cam) {
  tile.addEventListener('pointerdown', (ev) => {
    if (!state.editLayout || ev.button > 0) return;
    ev.preventDefault();

    const drag = {
      srcID: cam.id,
      startX: ev.clientX,
      startY: ev.clientY,
      dragging: false,
      ghost: null,
      hoverTile: null,
    };

    function clearHover() {
      if (drag.hoverTile) {
        drag.hoverTile.classList.remove('drop-target');
        drag.hoverTile = null;
      }
    }

    function onMove(moveEv) {
      const dx = moveEv.clientX - drag.startX;
      const dy = moveEv.clientY - drag.startY;
      if (!drag.dragging && Math.hypot(dx, dy) > 6) {
        drag.dragging = true;
        clearSwapSelection();
        tile.classList.add('dragging');
        const ghost = document.createElement('div');
        ghost.className = 'drag-ghost';
        ghost.textContent = cam.name || cam.id;
        document.body.appendChild(ghost);
        drag.ghost = ghost;
      }
      if (!drag.dragging) return;

      drag.ghost.style.left = `${moveEv.clientX + 12}px`;
      drag.ghost.style.top = `${moveEv.clientY + 12}px`;

      const hit = document.elementsFromPoint(moveEv.clientX, moveEv.clientY)
        .find((el) => el.classList?.contains('cam-tile') && el !== tile);
      if (hit !== drag.hoverTile) {
        clearHover();
        if (hit) {
          hit.classList.add('drop-target');
          drag.hoverTile = hit;
        }
      }
    }

    function cleanup() {
      window.removeEventListener('pointermove', onMove);
      window.removeEventListener('pointerup', onUp);
      window.removeEventListener('pointercancel', onCancel);
      tile.classList.remove('dragging');
      if (drag.ghost) {
        drag.ghost.remove();
        drag.ghost = null;
      }
      const target = drag.hoverTile;
      clearHover();
      return target;
    }

    function onUp() {
      const target = cleanup();
      if (drag.dragging) {
        if (target?.dataset.camId) {
          swapCameras(drag.srcID, target.dataset.camId);
        }
        return;
      }
      // Tap/click fallback in Edit Layout mode: tap first camera, then tap second camera to swap.
      if (!state.swapSelectedCam) {
        state.swapSelectedCam = drag.srcID;
        tile.classList.add('swap-selected');
      } else if (state.swapSelectedCam === drag.srcID) {
        clearSwapSelection();
      } else {
        const first = state.swapSelectedCam;
        clearSwapSelection();
        swapCameras(first, drag.srcID);
      }
    }

    function onCancel() {
      cleanup();
    }

    window.addEventListener('pointermove', onMove);
    window.addEventListener('pointerup', onUp);
    window.addEventListener('pointercancel', onCancel);
  });
}

function createGridPlayer(cam) {
  const player = document.createElement('feathernvr-player');
  player.setAttribute('grid', '');
  player.pcConfig = { bundlePolicy: 'max-bundle', iceServers: [] };
  player.mode = 'webrtc,mse,hls,mjpeg';
  player.media = 'video';
  // Observe the untransformed outer .cam-tile instead of the scaled player inside .zoom-stage,
  // otherwise CSS zoom >= ~2.6x shrinks intersectionRatio below 0.15 and triggers VideoRTC disconnect.
  player.visibilityThreshold = 0;
  player.visibilityCheck = true;
  if (player.video) {
    player.video.style.objectFit = cam.aspect_ratio === 'fill' ? 'cover' : 'contain';
  }
  return player;
}

const tileVisibilityObserver = 'IntersectionObserver' in window
  ? new IntersectionObserver((entries) => {
    for (const entry of entries) {
      const ref = state.tiles.get(entry.target.dataset.camId);
      const player = ref?.player;
      if (!player || !player.isConnected) continue;
      if (!entry.isIntersecting) {
        player.disconnectedCallback?.();
      } else {
        player.connectedCallback?.();
      }
    }
  }, { threshold: 0.15 })
  : null;

// Build/update live grid without tearing down existing video players
function syncLiveGrid(cameras) {
  const grid = qs('#camera-grid');
  if (!cameras.length) {
    clearSwapSelection();
    tileVisibilityObserver?.disconnect();
    for (const ref of state.tiles.values()) {
      if (typeof ref.player.ondisconnect === 'function') {
        ref.player.ondisconnect();
      }
    }
    state.tiles.clear();
    grid.style.gridTemplateColumns = '';
    grid.style.gridTemplateRows = '';
    grid.innerHTML = '';
    const empty = document.createElement('div');
    empty.className = 'empty-grid-state';
    empty.innerHTML = `
      <h3>No cameras configured yet</h3>
      <p class="muted">Click <strong>+ Add Camera</strong> to scan your LAN for ONVIF devices or add an RTSP stream.</p>
      <button type="button" class="btn btn-primary" id="btn-empty-add-cam">+ Add Camera</button>
    `;
    empty.querySelector('#btn-empty-add-cam')?.addEventListener('click', () => openCameraDialog(null));
    grid.appendChild(empty);
    qs('#live-summary').textContent = '0 cameras';
    return;
  }

  grid.querySelector('.empty-grid-state')?.remove();
  const nextIDs = cameras.map((c) => c.id);
  const changed = nextIDs.length !== state.tiles.size || nextIDs.some((id) => !state.tiles.has(id));

  if (changed) {
    clearSwapSelection();
    tileVisibilityObserver?.disconnect();
    for (const ref of state.tiles.values()) {
      if (typeof ref.player.ondisconnect === 'function') {
        ref.player.ondisconnect();
      }
    }
    state.tiles.clear();
    grid.innerHTML = '';

    for (const cam of cameras) {
      const tile = document.createElement('div');
      tile.className = 'cam-tile';
      tile.dataset.camId = cam.id;
      tile.classList.toggle('aspect-4-3', cam.aspect_ratio === '4:3');
      tile.classList.toggle('aspect-fill', cam.aspect_ratio === 'fill');

      const stage = document.createElement('div');
      stage.className = 'zoom-stage';

      const desiredStream = cam.live_stream === 'main' ? cam.stream_main : cam.stream_sub;
      const player = createGridPlayer(cam);

      stage.appendChild(player);

      const ovTop = document.createElement('div');
      ovTop.className = 'cam-overlay-top';
      ovTop.innerHTML = `
        <div class="cam-title">
          <span class="dot"></span>
          <span class="name-text">${escapeHTML(cam.name)}</span>
        </div>
        <div class="cam-badges">
          <span class="edit-drag-hint">&#8942;&#8942; Swap</span>
          <span class="badge ptz hidden">PTZ</span>
          <span class="badge motion hidden">MOT</span>
          <span class="badge rec hidden">REC</span>
        </div>
      `;

      const ovBottom = document.createElement('div');
      ovBottom.className = 'cam-overlay-bottom';
      ovBottom.innerHTML = `
        <span class="meta-left">--</span>
        <span class="meta-right">--</span>
      `;

      const zoomBadge = document.createElement('button');
      zoomBadge.type = 'button';
      zoomBadge.className = 'zoom-badge hidden';
      zoomBadge.textContent = '1.0x · Reset';

      tile.appendChild(stage);
      tile.appendChild(ovTop);
      tile.appendChild(ovBottom);
      tile.appendChild(zoomBadge);

      const zoomCtrl = createPanZoomController(tile, stage, zoomBadge, {
        isDisabled: () => state.editLayout,
        allowPageScrollAt1x: () => state.gridCols !== 'fit',
        onTap: () => {
          if (!state.editLayout) {
            openLiveModal(cam.id);
          }
        },
      });
      attachTileDragHandlers(tile, cam);
      tile.addEventListener('contextmenu', (ev) => {
        ev.preventDefault();
        ev.stopPropagation();
        openCameraContextMenu(cam.id, ev.clientX, ev.clientY);
      });

      grid.appendChild(tile);
      tileVisibilityObserver?.observe(tile);

      // Set src after connecting to DOM so isConnected is true.
      player.src = wsStreamURL(desiredStream);

      state.tiles.set(cam.id, {
        el: tile,
        stage,
        zoomCtrl,
        player,
        streamName: desiredStream,
        configRev: cam.config_rev || 0,
        nameEl: ovTop.querySelector('.name-text'),
        dot: ovTop.querySelector('.dot'),
        ptzBadge: ovTop.querySelector('.badge.ptz'),
        recBadge: ovTop.querySelector('.badge.rec'),
        motBadge: ovTop.querySelector('.badge.motion'),
        metaLeft: ovBottom.querySelector('.meta-left'),
        metaRight: ovBottom.querySelector('.meta-right'),
      });
    }
    applyCameraOrder();
    requestAnimationFrame(updateFitLayout);
  } else {
    applyCameraOrder();
  }

  let onlineCount = 0;
  let recCount = 0;
  for (const cam of cameras) {
    if (cam.online) onlineCount++;
    if (cam.recording) recCount++;

    const ref = state.tiles.get(cam.id);
    if (!ref) continue;
    ref.nameEl.textContent = cam.name;
    ref.el.classList.toggle('aspect-4-3', cam.aspect_ratio === '4:3');
    ref.el.classList.toggle('aspect-fill', cam.aspect_ratio === 'fill');

    const desiredStream = cam.live_stream === 'main' ? cam.stream_main : cam.stream_sub;
    const rev = cam.config_rev || 0;
    if (ref.streamName !== desiredStream || ref.configRev !== rev) {
      ref.streamName = desiredStream;
      ref.configRev = rev;
      if (typeof ref.player?.ondisconnect === 'function') {
        ref.player.ondisconnect();
      }
      ref.player?.remove();
      const nextPlayer = createGridPlayer(cam);
      ref.stage.appendChild(nextPlayer);
      nextPlayer.src = wsStreamURL(desiredStream);
      ref.player = nextPlayer;
      tileVisibilityObserver?.unobserve(ref.el);
      tileVisibilityObserver?.observe(ref.el);
    } else if (ref.player?.video) {
      ref.player.video.style.objectFit = cam.aspect_ratio === 'fill' ? 'cover' : 'contain';
    }

    ref.dot.classList.toggle('online', !!cam.online);
    ref.ptzBadge.classList.toggle('hidden', !cam.ptz_enabled);
    ref.recBadge.classList.toggle('hidden', !cam.recording);
    ref.motBadge.classList.toggle('hidden', !cam.motion_active);

    const resTag = (cam.width && cam.height)
      ? `${cam.width}x${cam.height} · `
      : (cam.resolution ? `${cam.resolution} · ` : '');
    ref.metaLeft.textContent = cam.online
      ? `${resTag}${cam.fps.toFixed(1)} fps · ${cam.bitrate_kbps.toFixed(0)} kbps`
      : 'Offline';
    ref.metaRight.textContent = `${cam.live_stream === 'main' ? 'MAIN' : 'SUB'} · ${cam.motion_mode || ''}`;
  }

  qs('#live-summary').textContent = `${onlineCount}/${cameras.length} online` + (recCount > 0 ? ` · ${recCount} recording` : '');

  if (state.activeModalCam) {
    const cam = cameras.find((c) => c.id === state.activeModalCam);
    if (cam) {
      qs('#modal-live-dot').classList.toggle('online', !!cam.online);
      qs('#modal-live-rec').classList.toggle('hidden', !cam.recording);
    }
  }
}

function renderStatusTab(data) {
  const st = data.storage || {};
  const pct = Math.min(100, Math.max(0, st.percent || 0));
  qs('#status-storage-bar').style.width = `${pct.toFixed(1)}%`;
  qs('#status-storage-used').textContent = `${formatBytes(st.used_bytes)} / ${formatBytes(st.max_bytes)} (${pct.toFixed(1)}%)`;
  qs('#status-storage-clips').textContent = String(st.recording_count ?? 0);
  qs('#status-storage-oldest').textContent = formatUnix(st.oldest_time);
  qs('#status-uptime').textContent = formatDuration(data.uptime_sec);
  qs('#top-storage-pill').textContent = `${formatBytes(st.used_bytes)} / ${formatBytes(st.max_bytes)}`;

  const tbody = qs('#diag-tbody');
  tbody.innerHTML = '';
  for (const cam of data.cameras || []) {
    const tr = document.createElement('tr');
    const score = Number(cam.motion_score || 0).toFixed(2);
    const motionLabel = cam.motion_active
      ? `${escapeHTML(cam.motion_mode)} (ACTIVE ${score}%)`
      : `${escapeHTML(cam.motion_mode)} (${score}%)`;
    tr.innerHTML = `
      <td><strong>${escapeHTML(cam.name)}</strong><div class="muted small">${escapeHTML(cam.id)}</div></td>
      <td>${cam.online ? '<span style="color:var(--ok)">Online</span>' : '<span style="color:var(--danger)">Offline</span>'}${cam.recording ? ' · <span style="color:var(--danger)">REC</span>' : ''}</td>
      <td>${cam.bitrate_kbps.toFixed(0)} kbps</td>
      <td>${cam.fps.toFixed(1)}</td>
      <td>${motionLabel}</td>
      <td>${cam.reconnects}</td>
      <td class="err-cell" title="${escapeHTML(cam.last_error || '')}">${escapeHTML(cam.last_error || '-')}</td>
      <td><button class="btn btn-secondary btn-sm">Trigger</button></td>
    `;
    tr.querySelector('button').addEventListener('click', () => triggerCamera(cam.id));
    tbody.appendChild(tr);
  }
}

function populateRecCameras(cameras) {
  const sel = qs('#rec-camera');
  if (!sel) return;
  const sig = cameras.map((c) => `${c.id}:${c.name}`).join('|');
  if (state.recCameraSig === sig) return;
  state.recCameraSig = sig;
  const prev = sel.value;
  sel.innerHTML = '<option value="">All Cameras</option>';
  for (const cam of cameras) {
    const opt = document.createElement('option');
    opt.value = cam.id;
    opt.textContent = cam.name;
    sel.appendChild(opt);
  }
  if (prev && cameras.some((c) => c.id === prev)) {
    sel.value = prev;
  }
}

async function pollStatus() {
  try {
    const res = await fetch('/api/status', { cache: 'no-store' });
    if (!res.ok) throw new Error(`HTTP ${res.status}`);
    const data = await res.json();
    state.cameras = data.cameras || [];
    state.storage = data.storage || null;
    state.uptimeSec = data.uptime_sec || 0;

    qs('#server-dot').classList.add('ok');
    syncLiveGrid(state.cameras);
    renderStatusTab(data);
    populateRecCameras(state.cameras);
  } catch (err) {
    qs('#server-dot').classList.remove('ok');
  }
}

function isStreamFullscreen(modalEl) {
  const card = modalEl?.querySelector('.modal-card');
  if (!card) return false;
  if (card.classList.contains('stream-only-fs')) return true;
  return Boolean(document.fullscreenElement && (document.fullscreenElement === card || modalEl.contains(document.fullscreenElement)));
}

function enterStreamFullscreen(modalEl) {
  const card = modalEl?.querySelector('.modal-card');
  if (!card) return;
  card.classList.add('stream-only-fs');
  if (card.requestFullscreen && !document.fullscreenElement) {
    card.requestFullscreen().catch(() => {});
  }
  requestAnimationFrame(() => {
    state.liveZoomCtrl?.reclamp();
    state.clipZoomCtrl?.reclamp();
  });
}

function exitStreamFullscreen(modalEl) {
  const card = modalEl?.querySelector('.modal-card');
  card?.classList.remove('stream-only-fs');
  if (document.fullscreenElement && modalEl && (document.fullscreenElement === card || modalEl.contains(document.fullscreenElement))) {
    document.exitFullscreen().catch(() => {});
  }
  requestAnimationFrame(() => {
    state.liveZoomCtrl?.reclamp();
    state.clipZoomCtrl?.reclamp();
  });
}

function toggleStreamFullscreen(modalEl) {
  if (isStreamFullscreen(modalEl)) {
    exitStreamFullscreen(modalEl);
  } else {
    enterStreamFullscreen(modalEl);
  }
}

document.addEventListener('fullscreenchange', () => {
  if (!document.fullscreenElement) {
    qs('#live-modal .modal-card')?.classList.remove('stream-only-fs');
    qs('#clip-modal .modal-card')?.classList.remove('stream-only-fs');
  }
  requestAnimationFrame(() => {
    state.liveZoomCtrl?.reclamp();
    state.clipZoomCtrl?.reclamp();
  });
});

// Single-camera modal
if (qs('#modal-live-player-wrap') && qs('#modal-live-zoom-stage')) {
  state.liveZoomCtrl = createPanZoomController(
    qs('#modal-live-player-wrap'),
    qs('#modal-live-zoom-stage'),
    qs('#live-zoom-badge'),
    {
      onTap: () => {
        const modal = qs('#live-modal');
        if (isStreamFullscreen(modal)) {
          exitStreamFullscreen(modal);
        } else {
          closeLiveModal();
        }
      },
    },
  );
}

function openLiveModal(camID) {
  const cam = state.cameras.find((c) => c.id === camID);
  if (!cam) return;
  state.activeModalCam = camID;
  state.activeModalQuality = cam.live_stream === 'main' ? 'main' : 'sub';
  state.ptzPanelExpanded = true;

  qs('#modal-live-title').textContent = cam.name;
  qs('#modal-live-dot').classList.toggle('online', !!cam.online);
  qs('#modal-live-rec').classList.toggle('hidden', !cam.recording);
  qs('#live-modal').classList.remove('hidden');

  loadModalPTZ(cam);
  mountModalStream();
}

function mountModalStream() {
  const cam = state.cameras.find((c) => c.id === state.activeModalCam);
  if (!cam) return;

  destroyModalPlayer();
  state.liveZoomCtrl?.reset();

  const streamName = state.activeModalQuality === 'main' ? cam.stream_main : cam.stream_sub;
  qs('#btn-stream-sub').classList.toggle('active', state.activeModalQuality === 'sub');
  qs('#btn-stream-main').classList.toggle('active', state.activeModalQuality === 'main');
  qs('#btn-modal-snap').href = `/go2rtc/api/frame.jpeg?src=${encodeURIComponent(streamName)}`;
  qs('#btn-modal-snap').download = `${cam.id}_${Date.now()}.jpg`;

  const stage = qs('#modal-live-zoom-stage');
  const player = document.createElement('feathernvr-player');
  player.pcConfig = { bundlePolicy: 'max-bundle', iceServers: [] };
  player.mode = 'webrtc,mse,hls,mjpeg';
  player.media = 'video,audio';
  player.visibilityThreshold = 0;
  player.visibilityCheck = true;
  stage.appendChild(player);

  if (player.video) {
    player.video.muted = true;
  }
  qs('#btn-modal-mute').textContent = 'Unmute';
  player.src = wsStreamURL(streamName);
  state.modalPlayer = player;
}

function destroyModalPlayer() {
  if (state.modalPlayer) {
    if (typeof state.modalPlayer.ondisconnect === 'function') {
      state.modalPlayer.ondisconnect();
    }
    state.modalPlayer.remove();
    state.modalPlayer = null;
  }
  const stage = qs('#modal-live-zoom-stage');
  if (stage) stage.innerHTML = '';
}

function closeLiveModal() {
  exitStreamFullscreen(qs('#live-modal'));
  state.liveZoomCtrl?.reset();
  stopActivePTZ();
  state.activeModalCam = null;
  destroyModalPlayer();
  qs('#modal-ptz-panel')?.classList.add('hidden');
  qs('#btn-modal-ptz-toggle')?.classList.add('hidden');
  if (qs('#modal-ptz-presets')) qs('#modal-ptz-presets').innerHTML = '';
  qs('#live-modal').classList.add('hidden');
}

function setPTZStatus(text) {
  const el = qs('#ptz-status');
  if (el) el.textContent = text || '';
}

function loadModalPTZ(cam) {
  const panel = qs('#modal-ptz-panel');
  const toggleBtn = qs('#btn-modal-ptz-toggle');
  const presetsEl = qs('#modal-ptz-presets');
  setPTZStatus('');
  if (presetsEl) presetsEl.innerHTML = '';
  if (!cam?.ptz_enabled) {
    panel?.classList.add('hidden');
    toggleBtn?.classList.add('hidden');
    return;
  }
  toggleBtn?.classList.remove('hidden');
  toggleBtn?.classList.toggle('active', state.ptzPanelExpanded);
  panel?.classList.toggle('hidden', !state.ptzPanelExpanded);
  loadPTZPresets(cam.id);
}

qs('#btn-modal-ptz-toggle')?.addEventListener('click', () => {
  state.ptzPanelExpanded = !state.ptzPanelExpanded;
  qs('#modal-ptz-panel')?.classList.toggle('hidden', !state.ptzPanelExpanded);
  qs('#btn-modal-ptz-toggle')?.classList.toggle('active', state.ptzPanelExpanded);
});

async function loadPTZPresets(camID) {
  const box = qs('#modal-ptz-presets');
  if (!box) return;
  box.innerHTML = '<span class="muted small">Loading...</span>';
  try {
    const res = await fetch(`/api/cameras/${encodeURIComponent(camID)}/ptz/presets`, { cache: 'no-store' });
    if (state.activeModalCam !== camID) return;
    if (!res.ok) {
      const errData = await res.json().catch(() => ({}));
      box.innerHTML = '';
      setPTZStatus(errData.error || `Presets HTTP ${res.status}`);
      return;
    }
    const data = await res.json();
    if (state.activeModalCam !== camID) return;
    const presets = (data.presets || []).slice(0, 32);
    box.innerHTML = '';
    if (!presets.length) {
      box.innerHTML = '<span class="muted small">None</span>';
      return;
    }
    for (const p of presets) {
      const btn = document.createElement('button');
      btn.type = 'button';
      btn.className = 'ptz-preset-btn';
      btn.textContent = p.name || p.id;
      btn.addEventListener('click', () => gotoPTZPreset(camID, p.id, btn.textContent));
      box.appendChild(btn);
    }
  } catch (err) {
    if (state.activeModalCam !== camID) return;
    box.innerHTML = '';
    setPTZStatus(err.message);
  }
}

async function ptzMove(camID, pan, tilt, zoom, durationMS) {
  setPTZStatus('');
  try {
    const res = await fetch(`/api/cameras/${encodeURIComponent(camID)}/ptz/move`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ pan, tilt, zoom, duration_ms: durationMS }),
    });
    if (!res.ok) {
      const errData = await res.json().catch(() => ({}));
      setPTZStatus(errData.error || `PTZ error ${res.status}`);
    }
  } catch (err) {
    setPTZStatus(err.message);
  }
}

async function ptzStop(camID) {
  if (!camID) return;
  if (state.ptzMovePromise) {
    await state.ptzMovePromise.catch(() => {});
  }
  try {
    await fetch(`/api/cameras/${encodeURIComponent(camID)}/ptz/stop`, { method: 'POST' });
  } catch {}
}

async function gotoPTZPreset(camID, presetID, label) {
  setPTZStatus(`Moving to ${label}...`);
  try {
    const res = await fetch(`/api/cameras/${encodeURIComponent(camID)}/ptz/presets/${encodeURIComponent(presetID)}`, {
      method: 'POST',
    });
    const data = await res.json().catch(() => ({}));
    if (!res.ok) {
      setPTZStatus(data.error || `Preset HTTP ${res.status}`);
      return;
    }
    setPTZStatus(`Preset: ${label}`);
  } catch (err) {
    setPTZStatus(err.message);
  }
}

function stopActivePTZ() {
  if (!state.activePtzBtn) return;
  state.activePtzBtn.classList.remove('moving');
  state.activePtzBtn = null;
  if (state.activeModalCam) {
    ptzStop(state.activeModalCam);
  }
}

qs('#ptz-speed')?.addEventListener('input', (ev) => {
  const valEl = qs('#ptz-speed-val');
  if (valEl) valEl.textContent = `${Math.round(Number(ev.target.value) * 100)}%`;
});

qs('#btn-ptz-stop')?.addEventListener('click', () => {
  state.activePtzBtn?.classList.remove('moving');
  state.activePtzBtn = null;
  if (state.activeModalCam) ptzStop(state.activeModalCam);
});

document.querySelectorAll('#modal-ptz-panel [data-pan], #modal-ptz-panel [data-zoom]').forEach((btn) => {
  btn.addEventListener('pointerdown', (ev) => {
    if (!state.activeModalCam || ev.button > 0) return;
    ev.preventDefault();
    if (state.activePtzBtn && state.activePtzBtn !== btn) {
      state.activePtzBtn.classList.remove('moving');
    }
    const speed = Number(qs('#ptz-speed')?.value || 0.6);
    const pan = Number(btn.dataset.pan || 0) * speed;
    const tilt = Number(btn.dataset.tilt || 0) * speed;
    const zoom = Number(btn.dataset.zoom || 0) * speed;
    state.activePtzBtn = btn;
    btn.classList.add('moving');
    try { btn.setPointerCapture(ev.pointerId); } catch {}
    state.ptzMovePromise = ptzMove(state.activeModalCam, pan, tilt, zoom, 4500);
  });
  btn.addEventListener('pointerup', stopActivePTZ);
  btn.addEventListener('pointercancel', stopActivePTZ);
  btn.addEventListener('lostpointercapture', stopActivePTZ);
  btn.addEventListener('click', (ev) => {
    // Keyboard activation fallback (Enter/Space) sends a short nudge.
    if (ev.detail === 0 && state.activeModalCam) {
      const speed = Number(qs('#ptz-speed')?.value || 0.6);
      const pan = Number(btn.dataset.pan || 0) * speed;
      const tilt = Number(btn.dataset.tilt || 0) * speed;
      const zoom = Number(btn.dataset.zoom || 0) * speed;
      state.ptzMovePromise = ptzMove(state.activeModalCam, pan, tilt, zoom, 350);
    }
  });
});

window.addEventListener('blur', stopActivePTZ);

qs('#btn-stream-sub').addEventListener('click', () => {
  state.activeModalQuality = 'sub';
  mountModalStream();
});

qs('#btn-stream-main').addEventListener('click', () => {
  state.activeModalQuality = 'main';
  mountModalStream();
});

qs('#btn-modal-mute').addEventListener('click', () => {
  const v = state.modalPlayer?.video;
  if (!v) return;
  v.muted = !v.muted;
  qs('#btn-modal-mute').textContent = v.muted ? 'Unmute' : 'Mute';
});

qs('#btn-modal-fs').addEventListener('click', () => {
  toggleStreamFullscreen(qs('#live-modal'));
});

qs('#btn-modal-settings')?.addEventListener('click', () => {
  const camID = state.activeModalCam;
  if (!camID) return;
  closeLiveModal();
  openCameraDialog(camID);
});

qs('#btn-modal-rec').addEventListener('click', async () => {
  if (state.activeModalCam) {
    await triggerCamera(state.activeModalCam);
    pollStatus();
  }
});

async function triggerCamera(camID) {
  await fetch(`/api/cameras/${encodeURIComponent(camID)}/trigger`, { method: 'POST' });
}

// Recordings tab (grouped per camera per day)
async function loadRecordings(append) {
  if (!append) {
    state.recOffset = 0;
  }
  const params = new URLSearchParams({
    limit: String(state.recLimit),
    offset: String(state.recOffset),
  });
  const cam = qs('#rec-camera').value;
  const date = qs('#rec-date').value;
  if (cam) params.set('camera', cam);
  if (date) params.set('date', date);

  const listEl = qs('#recordings-list');
  if (!append) {
    listEl.innerHTML = '<div class="muted">Loading clips...</div>';
  }

  const res = await fetch(`/api/recordings/days?${params.toString()}`, { cache: 'no-store' });
  if (!res.ok) {
    listEl.innerHTML = '<div class="muted">Failed to load recordings.</div>';
    return;
  }
  const data = await res.json();
  const items = data.days || [];
  state.recTotal = data.total || 0;

  if (!append) {
    listEl.innerHTML = '';
  }

  if (!append && items.length === 0) {
    listEl.innerHTML = '<div class="muted">No motion clips found for the selected filter.</div>';
  }

  for (const day of items) {
    const card = document.createElement('div');
    card.className = 'rec-card';
    const thumbHTML = day.thumb_url
      ? `<img loading="lazy" src="${escapeHTML(day.thumb_url)}" alt="">`
      : `<div class="muted small">No thumbnail</div>`;
    card.innerHTML = `
      <div class="rec-thumb-wrap">
        ${thumbHTML}
        <span class="rec-trigger-pill">${escapeHTML(day.date)}</span>
        <span class="rec-dur-pill">${day.clip_count} clip(s) &middot; ${escapeHTML(formatDuration(day.total_duration_sec))}</span>
      </div>
      <div class="rec-body">
        <div class="rec-title">${escapeHTML(day.camera_name || day.camera_id)}</div>
        <div class="rec-sub">
          <span>${escapeHTML(formatClockTime(day.first_start_time))} &ndash; ${escapeHTML(formatClockTime(day.last_end_time))}</span>
          <span>${formatBytes(day.total_size_bytes)}</span>
        </div>
      </div>
    `;
    card.addEventListener('click', () => openDayModal(day));
    listEl.appendChild(card);
  }

  const shown = state.recOffset + items.length;
  const hasMore = shown < state.recTotal;
  qs('#rec-count-label').textContent = `${state.recTotal} camera-day(s)`;
  qs('#rec-load-more').classList.toggle('hidden', !hasMore);
  qs('#rec-load-more-wrap')?.classList.toggle('hidden', !hasMore);
  updateRecFitLayout();
  requestAnimationFrame(updateRecFitLayout);
}

qs('#rec-camera').addEventListener('change', () => loadRecordings(false));
qs('#rec-date').addEventListener('change', () => loadRecordings(false));
qs('#rec-refresh').addEventListener('click', () => loadRecordings(false));
qs('#rec-clear-date').addEventListener('click', () => {
  qs('#rec-date').value = '';
  loadRecordings(false);
});
qs('#rec-load-more').addEventListener('click', () => {
  state.recOffset += state.recLimit;
  loadRecordings(true);
});

if (qs('#modal-clip-player-wrap') && qs('#modal-clip-zoom-stage')) {
  state.clipZoomCtrl = createPanZoomController(
    qs('#modal-clip-player-wrap'),
    qs('#modal-clip-zoom-stage'),
    qs('#clip-zoom-badge'),
    {
      onTap: () => {
        const modal = qs('#clip-modal');
        if (isStreamFullscreen(modal)) {
          exitStreamFullscreen(modal);
        } else {
          closeClipModal();
        }
      },
    },
  );
}

const clipDeck = [qs('#modal-clip-video'), qs('#modal-clip-video-next')].filter(Boolean);
let activeClipDeckIdx = 0;

function getActiveClipVideo() {
  return clipDeck[activeClipDeckIdx] || null;
}

function getStandbyClipVideo() {
  return clipDeck.length > 1 ? clipDeck[1 - activeClipDeckIdx] : null;
}

function syncClipDeckClasses() {
  clipDeck.forEach((el, idx) => {
    const isActive = idx === activeClipDeckIdx;
    el.classList.toggle('active', isActive);
    el.classList.toggle('standby', !isActive);
  });
}

function clearClipVideo(el) {
  if (!el) return;
  el.pause();
  el.autoplay = false;
  delete el.dataset.clipUrl;
  delete el.dataset.clipIndex;
  el.removeAttribute('src');
  el.load();
}

function resetClipDeck() {
  clipDeck.forEach(clearClipVideo);
  activeClipDeckIdx = 0;
  syncClipDeckClasses();
}

function preloadNextClip() {
  const session = state.daySession;
  const standby = getStandbyClipVideo();
  if (!session || !standby) return;

  const nextIdx = session.index + 1;
  if (nextIdx >= session.clips.length) {
    clearClipVideo(standby);
    return;
  }

  const nextClip = session.clips[nextIdx];
  if (!nextClip?.video_url) {
    clearClipVideo(standby);
    return;
  }

  if (standby.dataset.clipUrl === nextClip.video_url) {
    standby.dataset.clipIndex = String(nextIdx);
    standby.muted = true;
    standby.playbackRate = session.playbackRate || 1;
    if (standby.readyState >= 1 && standby.currentTime > 0.05) {
      standby.currentTime = 0;
    }
    return;
  }

  standby.pause();
  standby.autoplay = false;
  standby.preload = 'auto';
  standby.muted = true;
  standby.dataset.clipUrl = nextClip.video_url;
  standby.dataset.clipIndex = String(nextIdx);
  standby.src = nextClip.video_url;
  standby.playbackRate = session.playbackRate || 1;
  standby.load();
}

async function openDayModal(day) {
  const params = new URLSearchParams({
    camera: day.camera_id,
    date: day.date,
  });
  const res = await fetch(`/api/recordings/day?${params.toString()}`, { cache: 'no-store' });
  if (!res.ok) {
    alert('Failed to load day recordings.');
    return;
  }
  const data = await res.json();
  const clips = data.clips || [];
  if (!clips.length) {
    alert('No clips found for this day.');
    loadRecordings(false);
    return;
  }

  state.daySession = {
    camera_id: data.camera_id,
    camera_name: data.camera_name || day.camera_name || day.camera_id,
    date: data.date,
    day_start: data.day_start,
    day_end: data.day_end,
    tlViewStart: data.day_start,
    tlViewEnd: data.day_end,
    clips,
    index: 0,
    tlMode: '24h',
    playbackRate: Number(qs('#clip-speed')?.value || 1),
    muted: false,
    scrubbing: false,
    pendingSeek: 0,
    pendingAutoplay: true,
  };

  resetTimelineScrubAndPreview(false);
  resetClipDeck();
  qs('#btn-clip-mute').textContent = 'Mute';
  state.clipZoomCtrl?.reset();
  qs('#clip-modal').classList.remove('hidden');
  renderDayTimeline();
  playDayClip(0, 0, true);
}

function getTimelineWindow(session) {
  if (!session) return { winStart: 0, winEnd: 86400 };
  const clips = session.clips || [];
  if (session.tlMode === 'fit' && clips.length > 0) {
    const firstStart = clips[0].start_time;
    const lastEnd = clips.reduce(
      (mx, c) => Math.max(mx, c.end_time || 0, c.start_time + Math.round(c.duration_sec || 1)),
      firstStart,
    );
    const span = Math.max(60, lastEnd - firstStart);
    const pad = Math.max(300, Math.round(span * 0.08));
    let winStart = Math.max(session.day_start, firstStart - pad);
    let winEnd = Math.min(session.day_end, lastEnd + pad);
    if (winEnd - winStart < 600 && (session.day_end - session.day_start) >= 600) {
      const center = (winStart + winEnd) / 2;
      winStart = Math.max(session.day_start, Math.floor(center - 300));
      winEnd = Math.min(session.day_end, winStart + 600);
      winStart = Math.max(session.day_start, winEnd - 600);
    }
    return { winStart, winEnd };
  }
  return {
    winStart: session.tlViewStart ?? session.day_start,
    winEnd: session.tlViewEnd ?? session.day_end,
  };
}

function renderDayTimeline() {
  const session = state.daySession;
  if (!session) return;
  const { winStart, winEnd } = getTimelineWindow(session);
  const span = Math.max(1, winEnd - winStart);

  document.querySelectorAll('[data-tl-mode]').forEach((btn) => {
    btn.classList.toggle('active', btn.dataset.tlMode === session.tlMode);
  });

  const fullSpan = Math.max(1, session.day_end - session.day_start);
  const zoomed24h = session.tlMode === '24h' && span < fullSpan - 1;
  const resetBtn = qs('#btn-tl-reset-zoom');
  if (resetBtn) {
    resetBtn.classList.toggle('hidden', !zoomed24h);
    if (zoomed24h) {
      resetBtn.textContent = `${formatDuration(span)} · Reset`;
    }
  }

  const segsEl = qs('#day-timeline-segs');
  segsEl.innerHTML = '';
  session.clips.forEach((clip, idx) => {
    const cStart = clip.start_time;
    const cEnd = Math.max(clip.end_time || 0, clip.start_time + Math.round(clip.duration_sec || 1));
    const visStart = Math.max(winStart, cStart);
    const visEnd = Math.min(winEnd, cEnd);
    if (visEnd <= visStart) return;

    const leftPct = ((visStart - winStart) / span) * 100;
    const widthPct = ((visEnd - visStart) / span) * 100;

    const seg = document.createElement('div');
    seg.className = 'tl-seg' + (idx === session.index ? ' active' : '');
    seg.dataset.clipIndex = String(idx);
    seg.style.left = `${leftPct}%`;
    seg.style.width = `${Math.max(0.25, widthPct)}%`;
    segsEl.appendChild(seg);
  });

  const ticksEl = qs('#day-timeline-ticks');
  ticksEl.innerHTML = '';
  const tickCount = session.tlMode === '24h' ? 6 : 4;
  for (let i = 0; i <= tickCount; i++) {
    const ratio = i / tickCount;
    const t = Math.round(winStart + ratio * span);
    const tick = document.createElement('span');
    tick.className = 'tl-tick';
    tick.style.left = `${ratio * 100}%`;
    if (session.tlMode === '24h' && i === tickCount && Math.abs(winEnd - session.day_end) < 1) {
      tick.textContent = '24:00';
    } else if (span <= 3600) {
      tick.textContent = formatClockTime(t);
    } else {
      tick.textContent = formatHM(t);
    }
    ticksEl.appendChild(tick);
  }

  updateDayPlayhead();
}

function updateClipPlayButtons() {
  const video = getActiveClipVideo();
  const session = state.daySession;
  const playBtn = qs('#btn-clip-play');
  const pausedPill = qs('#clip-paused-pill');
  if (!video || !playBtn) return;

  const atEnd = Boolean(session && video.ended && session.index >= session.clips.length - 1);
  if (atEnd) {
    playBtn.textContent = 'Replay';
    pausedPill?.classList.add('hidden');
  } else if (video.paused) {
    playBtn.textContent = 'Play';
    pausedPill?.classList.remove('hidden');
  } else {
    playBtn.textContent = 'Pause';
    pausedPill?.classList.add('hidden');
  }

  if (session) {
    qs('#btn-clip-prev').disabled = session.index <= 0;
    qs('#btn-clip-next').disabled = session.index >= session.clips.length - 1;
  }
}

function updateDayPlayhead(overrideUnix = null) {
  const session = state.daySession;
  const playhead = qs('#day-timeline-playhead');
  if (!session || !session.clips.length) {
    playhead?.classList.add('hidden');
    return;
  }

  const clip = session.clips[session.index];
  const video = getActiveClipVideo();
  const cur = Number.isFinite(video?.currentTime) ? video.currentTime : 0;
  const dur = Number.isFinite(video?.duration) && video.duration > 0 ? video.duration : (clip.duration_sec || 0);
  const wallUnix = overrideUnix ?? (clip.start_time + cur);

  qs('#tl-current-time').textContent = formatClockTime(wallUnix);
  const curText = video?.paused ? formatMMSSPrecise(cur) : formatMMSS(cur);
  qs('#clip-time-readout').textContent = `${curText} / ${formatMMSS(dur)}`;

  const { winStart, winEnd } = getTimelineWindow(session);
  const span = Math.max(1, winEnd - winStart);
  if (playhead) {
    if (wallUnix < winStart || wallUnix > winEnd) {
      playhead.classList.add('hidden');
    } else {
      const pct = ((wallUnix - winStart) / span) * 100;
      playhead.classList.remove('hidden');
      playhead.style.left = `${pct}%`;
    }
  }
}

function playDayClip(index, offsetSec = 0, autoplay = true) {
  const session = state.daySession;
  if (!session || !session.clips.length) return;
  index = Math.max(0, Math.min(session.clips.length - 1, index));
  session.index = index;
  session.pendingSeek = Math.max(0, offsetSec || 0);
  session.pendingAutoplay = Boolean(autoplay);

  const clip = session.clips[index];
  qs('#modal-clip-title').textContent = `${session.camera_name || session.camera_id} · ${session.date}`;
  qs('#modal-clip-meta').textContent =
    `Clip ${index + 1}/${session.clips.length} · ${formatClockTime(clip.start_time)}–${formatClockTime(clip.end_time)} · ${clip.trigger_source || 'motion'} · ${formatBytes(clip.size_bytes)}`;
  qs('#tl-clip-range').textContent = `Clip ${index + 1} of ${session.clips.length}`;
  qs('#btn-clip-download').href = clip.video_url;
  qs('#btn-clip-download').download = clip.file_path.split('/').pop() || 'clip.mp4';

  qs('#day-timeline-segs')?.querySelectorAll('.tl-seg').forEach((seg) => {
    seg.classList.toggle('active', Number(seg.dataset.clipIndex) === index);
  });

  const active = getActiveClipVideo();
  const standby = getStandbyClipVideo();

  // Case 1: Pre-buffered Standby Hit -> swap decks seamlessly
  if (standby && standby.dataset.clipUrl === clip.video_url) {
    active?.pause();
    activeClipDeckIdx = 1 - activeClipDeckIdx;
    syncClipDeckClasses();

    const v = getActiveClipVideo();
    if (v) {
      v.muted = Boolean(session.muted);
      v.playbackRate = session.playbackRate || 1;
      v.autoplay = Boolean(autoplay);
      if (v.readyState >= 1) {
        if (session.pendingSeek > 0) {
          const maxT = Number.isFinite(v.duration) && v.duration > 0 ? Math.max(0, v.duration - 0.05) : session.pendingSeek;
          v.currentTime = Math.min(session.pendingSeek, maxT);
        } else if (v.currentTime > 0.05) {
          v.currentTime = 0;
        }
        session.pendingSeek = 0;
        session.pendingAutoplay = false;
        if (autoplay) {
          const p = v.play();
          if (p && typeof p.catch === 'function') {
            p.catch(() => {
              session.muted = true;
              v.muted = true;
              qs('#btn-clip-mute').textContent = 'Unmute';
              v.play().catch(() => {});
            });
          }
        } else if (!v.paused) {
          v.pause();
        }
        preloadNextClip();
      }
    }
    updateClipPlayButtons();
    updateDayPlayhead();
    return;
  }

  // Case 2: Same clip already loaded on Active -> seek directly without reloading
  if (active && active.dataset.clipUrl === clip.video_url && active.readyState >= 1) {
    active.muted = Boolean(session.muted);
    active.playbackRate = session.playbackRate || 1;
    const maxT = Number.isFinite(active.duration) && active.duration > 0 ? Math.max(0, active.duration - 0.05) : session.pendingSeek;
    active.currentTime = Math.min(session.pendingSeek, maxT);
    session.pendingSeek = 0;
    session.pendingAutoplay = false;
    if (autoplay) {
      const p = active.play();
      if (p && typeof p.catch === 'function') {
        p.catch(() => {
          session.muted = true;
          active.muted = true;
          qs('#btn-clip-mute').textContent = 'Unmute';
          active.play().catch(() => {});
        });
      }
    } else if (!active.paused) {
      active.pause();
    }
    preloadNextClip();
    updateClipPlayButtons();
    updateDayPlayhead();
    return;
  }

  // Case 3: Cold load on Active (clear Standby so initial clip gets priority)
  clearClipVideo(standby);
  if (active) {
    active.muted = Boolean(session.muted);
    active.playbackRate = session.playbackRate || 1;
    active.autoplay = Boolean(autoplay);
    active.dataset.clipUrl = clip.video_url;
    active.dataset.clipIndex = String(index);
    active.src = clip.video_url;
    active.load();
  }
  updateClipPlayButtons();
  updateDayPlayhead();
}

function getCameraFrameStep(camID) {
  const cam = state.cameras.find((c) => c.id === camID);
  const fps = Number(cam?.fps);
  const effFPS = Number.isFinite(fps) && fps >= 4 && fps <= 60 ? fps : 15;
  return 1 / effFPS;
}

function stepDayFrame(dir) {
  const session = state.daySession;
  const video = getActiveClipVideo();
  if (!session || !session.clips.length || !video) return;
  if (!video.paused) {
    video.pause();
  }
  const step = getCameraFrameStep(session.camera_id);
  const dur = Number.isFinite(video.duration) && video.duration > 0
    ? video.duration
    : (session.clips[session.index]?.duration_sec || 0);
  const cur = Number.isFinite(video.currentTime) ? video.currentTime : 0;
  const target = cur + dir * step;

  if (dir < 0 && target < 0) {
    if (session.index > 0) {
      const prevClip = session.clips[session.index - 1];
      const prevDur = Number(prevClip?.duration_sec) || 0;
      playDayClip(session.index - 1, Math.max(0, prevDur - step), false);
      return;
    }
    video.currentTime = 0;
  } else if (dir > 0 && dur > 0 && target >= dur - 0.005) {
    if (session.index < session.clips.length - 1) {
      playDayClip(session.index + 1, 0, false);
      return;
    }
    video.currentTime = Math.max(0, dur - 0.005);
  } else {
    const maxT = dur > 0 ? Math.max(0, dur - 0.005) : target;
    video.currentTime = Math.max(0, Math.min(maxT, target));
  }
  updateClipPlayButtons();
  updateDayPlayhead();
}

function findClipForUnix(session, unix) {
  const clips = session?.clips || [];
  if (!clips.length) return { index: 0, offset: 0 };
  for (let i = 0; i < clips.length; i++) {
    const c = clips[i];
    const cEnd = Math.max(c.end_time || 0, c.start_time + Math.ceil(c.duration_sec || 1));
    if (unix >= c.start_time && unix <= cEnd) {
      return { index: i, offset: Math.max(0, unix - c.start_time) };
    }
  }
  for (let i = 0; i < clips.length; i++) {
    if (clips[i].start_time >= unix) {
      return { index: i, offset: 0 };
    }
  }
  return { index: clips.length - 1, offset: 0 };
}

function seekDayTimeline(targetUnix, autoplay = true) {
  const session = state.daySession;
  if (!session || !session.clips.length) return;
  const hit = findClipForUnix(session, targetUnix);
  const video = getActiveClipVideo();
  if (hit.index === session.index && video?.readyState >= 1) {
    const maxT = Number.isFinite(video.duration) && video.duration > 0 ? Math.max(0, video.duration - 0.05) : hit.offset;
    video.currentTime = Math.max(0, Math.min(maxT, hit.offset));
    if (autoplay) {
      if (video.paused) {
        const p = video.play();
        if (p && typeof p.catch === 'function') {
          p.catch(() => {
            session.muted = true;
            video.muted = true;
            qs('#btn-clip-mute').textContent = 'Unmute';
            video.play().catch(() => {});
          });
        }
      }
    } else if (!video.paused) {
      video.pause();
    }
    updateDayPlayhead();
  } else {
    playDayClip(hit.index, hit.offset, autoplay);
  }
}

let onActiveClipScrubSeeked = () => {};
let resetTimelineScrubAndPreview = () => {};

clipDeck.forEach((videoEl) => {
  videoEl.addEventListener('loadedmetadata', () => {
    if (videoEl !== getActiveClipVideo()) return;
    const session = state.daySession;
    if (!session) return;
    videoEl.playbackRate = session.playbackRate || 1;
    videoEl.muted = Boolean(session.muted);
    if (session.pendingSeek > 0) {
      const maxT = Number.isFinite(videoEl.duration) && videoEl.duration > 0
        ? Math.max(0, videoEl.duration - 0.05)
        : session.pendingSeek;
      videoEl.currentTime = Math.min(session.pendingSeek, maxT);
      session.pendingSeek = 0;
    }
    if (session.pendingAutoplay) {
      session.pendingAutoplay = false;
      const p = videoEl.play();
      if (p && typeof p.catch === 'function') {
        p.catch(() => {
          session.muted = true;
          videoEl.muted = true;
          qs('#btn-clip-mute').textContent = 'Unmute';
          videoEl.play().catch(() => {});
        });
      }
    } else if (!videoEl.paused) {
      videoEl.pause();
    }
    updateClipPlayButtons();
    if (!session.scrubbing) {
      updateDayPlayhead();
    }
    if (videoEl.readyState >= 2) {
      preloadNextClip();
    }
  });

  videoEl.addEventListener('loadeddata', () => {
    if (videoEl !== getActiveClipVideo()) return;
    preloadNextClip();
  });

  videoEl.addEventListener('timeupdate', () => {
    if (videoEl !== getActiveClipVideo()) return;
    if (state.daySession?.scrubbing) return;
    updateDayPlayhead();
  });
  videoEl.addEventListener('seeked', () => {
    if (videoEl !== getActiveClipVideo()) return;
    if (state.daySession?.scrubbing) {
      onActiveClipScrubSeeked(videoEl);
      return;
    }
    updateDayPlayhead();
  });
  videoEl.addEventListener('play', () => {
    if (videoEl !== getActiveClipVideo()) return;
    updateClipPlayButtons();
  });
  videoEl.addEventListener('pause', () => {
    if (videoEl !== getActiveClipVideo()) return;
    updateClipPlayButtons();
    if (!state.daySession?.scrubbing) {
      updateDayPlayhead();
    }
  });
  videoEl.addEventListener('ended', () => {
    if (videoEl !== getActiveClipVideo()) return;
    const session = state.daySession;
    if (session && session.index < session.clips.length - 1) {
      playDayClip(session.index + 1, 0, true);
    } else {
      updateClipPlayButtons();
    }
  });
});

// Timeline track hover preview, live draggable scrub & desktop mouse-wheel zoom in 24h mode
(() => {
  const panel = qs('#day-timeline-panel');
  const track = qs('#day-timeline-track');
  const hover = qs('#day-timeline-hover');
  const previewMedia = qs('#day-timeline-preview-media');
  const previewImg = qs('#day-timeline-preview-img');
  const previewCanvas = qs('#day-timeline-preview-canvas');
  const hoverLabel = qs('#day-timeline-hover-label');
  if (!track) return;

  const MIN_TL_SPAN = 60;
  const MAX_PREVIEW_CACHE = 120;

  let scrubbing = false;
  let scrubWasPlaying = false;
  let scrubWasEnded = false;
  let scrubPendingOffset = null;
  let scrubPendingClipTarget = null;
  let scrubClipSwitchTimer = null;
  let scrubLatestUnix = null;

  let hoverPreviewTimer = null;
  let hoverFetchCtrl = null;
  let currentHoverTarget = null;
  let displayedClipId = null;

  const previewCache = new Map();
  const previewVideo = document.createElement('video');
  previewVideo.muted = true;
  previewVideo.playsInline = true;
  previewVideo.preload = 'auto';

  let previewVideoUrl = '';
  let previewVideoBrokenUrl = '';
  let previewVideoPendingSeek = null;
  let previewVideoActiveReq = null;

  function setPreviewCache(key, entry) {
    if (previewCache.has(key)) {
      const prev = previewCache.get(key);
      if (prev?.type === 'objectUrl' && prev.value !== entry.value) {
        URL.revokeObjectURL(prev.value);
      }
      previewCache.delete(key);
    }
    previewCache.set(key, entry);
    while (previewCache.size > MAX_PREVIEW_CACHE) {
      const oldestKey = previewCache.keys().next().value;
      const oldest = previewCache.get(oldestKey);
      previewCache.delete(oldestKey);
      if (oldest?.type === 'objectUrl') {
        URL.revokeObjectURL(oldest.value);
      }
    }
  }

  function clearPreviewCache() {
    for (const entry of previewCache.values()) {
      if (entry?.type === 'objectUrl') {
        URL.revokeObjectURL(entry.value);
      }
    }
    previewCache.clear();
  }

  function showPreviewImage(src) {
    if (!previewImg) return;
    previewCanvas?.classList.add('hidden');
    if (src) {
      if (previewImg.getAttribute('src') !== src) {
        previewImg.src = src;
      }
      previewImg.classList.remove('hidden');
    } else {
      previewImg.removeAttribute('src');
      previewImg.classList.add('hidden');
    }
  }

  function showPreviewCanvas() {
    previewImg?.classList.add('hidden');
    previewCanvas?.classList.remove('hidden');
  }

  function hidePreviewMedia() {
    previewMedia?.classList.add('hidden');
    hover?.classList.remove('has-preview');
  }

  function cancelPendingHoverPreview() {
    if (hoverPreviewTimer) {
      clearTimeout(hoverPreviewTimer);
      hoverPreviewTimer = null;
    }
    if (hoverFetchCtrl) {
      hoverFetchCtrl.abort();
      hoverFetchCtrl = null;
    }
    previewVideoPendingSeek = null;
    previewVideoActiveReq = null;
  }

  function drawVideoToPreviewCanvas(videoEl) {
    if (!previewCanvas || !videoEl || !videoEl.videoWidth || !videoEl.videoHeight) {
      return false;
    }
    const ctx = previewCanvas.getContext('2d');
    if (!ctx) return false;
    const cw = previewCanvas.width;
    const ch = previewCanvas.height;
    ctx.fillStyle = '#000';
    ctx.fillRect(0, 0, cw, ch);
    const scale = Math.min(cw / videoEl.videoWidth, ch / videoEl.videoHeight);
    const dw = videoEl.videoWidth * scale;
    const dh = videoEl.videoHeight * scale;
    const dx = (cw - dw) / 2;
    const dy = (ch - dh) / 2;
    ctx.drawImage(videoEl, dx, dy, dw, dh);
    return true;
  }

  function renderPreviewVideoFrame(req) {
    if (!req || !drawVideoToPreviewCanvas(previewVideo)) return;
    try {
      const dataUrl = previewCanvas.toDataURL('image/jpeg', 0.72);
      setPreviewCache(req.cacheKey, { type: 'dataUrl', value: dataUrl });
    } catch {}
    if (!scrubbing && state.daySession && currentHoverTarget?.clipId === req.clipId) {
      showPreviewCanvas();
      displayedClipId = req.clipId;
    }
  }

  function seekPreviewVideo(req) {
    if (!req || previewVideoUrl !== req.videoUrl) return;
    if (previewVideo.readyState < 2) {
      previewVideoPendingSeek = req;
      return;
    }
    const dur = Number.isFinite(previewVideo.duration) && previewVideo.duration > 0
      ? Math.max(0, previewVideo.duration - 0.05)
      : req.qSec;
    const clamped = Math.max(0, Math.min(dur, req.qSec));
    if (previewVideo.seeking) {
      previewVideoPendingSeek = req;
      return;
    }
    previewVideoPendingSeek = null;
    previewVideoActiveReq = req;
    if (Math.abs(previewVideo.currentTime - clamped) < 0.02) {
      previewVideoActiveReq = null;
      renderPreviewVideoFrame(req);
      return;
    }
    previewVideo.currentTime = clamped;
  }

  previewVideo.addEventListener('loadeddata', () => {
    if (previewVideoPendingSeek && previewVideoPendingSeek.videoUrl === previewVideoUrl) {
      const req = previewVideoPendingSeek;
      previewVideoPendingSeek = null;
      seekPreviewVideo(req);
    }
  });

  previewVideo.addEventListener('seeked', () => {
    if (previewVideoPendingSeek && previewVideoPendingSeek.videoUrl === previewVideoUrl) {
      const nextReq = previewVideoPendingSeek;
      previewVideoPendingSeek = null;
      seekPreviewVideo(nextReq);
      return;
    }
    const req = previewVideoActiveReq;
    previewVideoActiveReq = null;
    if (req && req.videoUrl === previewVideoUrl) {
      renderPreviewVideoFrame(req);
    }
  });

  previewVideo.addEventListener('error', () => {
    if (!previewVideoUrl) return;
    previewVideoBrokenUrl = previewVideoUrl;
    previewVideoUrl = '';
    previewVideoPendingSeek = null;
    previewVideoActiveReq = null;
    if (!scrubbing && currentHoverTarget && currentHoverTarget.clip?.video_url === previewVideoBrokenUrl) {
      fetchServerPreviewFrame(currentHoverTarget);
    }
  });

  function requestClientPreviewFrame(target) {
    if (!target || scrubbing || !state.daySession || currentHoverTarget?.cacheKey !== target.cacheKey) {
      return;
    }
    const videoUrl = target.clip?.video_url;
    if (!videoUrl || previewVideoBrokenUrl === videoUrl) {
      fetchServerPreviewFrame(target);
      return;
    }
    const req = {
      clipId: target.clipId,
      cacheKey: target.cacheKey,
      qSec: target.qSec,
      videoUrl,
    };
    if (previewVideoUrl !== videoUrl) {
      previewVideoUrl = videoUrl;
      previewVideoPendingSeek = req;
      previewVideoActiveReq = null;
      previewVideo.src = videoUrl;
      previewVideo.load();
      return;
    }
    seekPreviewVideo(req);
  }

  async function fetchServerPreviewFrame(target) {
    if (!target || scrubbing || !state.daySession || currentHoverTarget?.cacheKey !== target.cacheKey) {
      return;
    }
    if (hoverFetchCtrl) {
      hoverFetchCtrl.abort();
    }
    const ctrl = new AbortController();
    hoverFetchCtrl = ctrl;
    try {
      const res = await fetch(
        `/api/recordings/${encodeURIComponent(target.clipId)}/frame?t=${target.qSec.toFixed(1)}`,
        { signal: ctrl.signal },
      );
      if (!res.ok || ctrl.signal.aborted) return;
      const blob = await res.blob();
      if (ctrl.signal.aborted || !blob.size) return;
      const objUrl = URL.createObjectURL(blob);
      setPreviewCache(target.cacheKey, { type: 'objectUrl', value: objUrl });
      if (!scrubbing && state.daySession && currentHoverTarget?.cacheKey === target.cacheKey) {
        showPreviewImage(objUrl);
        displayedClipId = target.clipId;
      }
    } catch {}
  }

  function positionHoverPopover(clientX, ratio) {
    if (!hover) return;
    hover.classList.remove('hidden');
    const rect = track.getBoundingClientRect();
    if (rect.width <= 0) {
      hover.style.left = `${Math.max(4, Math.min(96, ratio * 100))}%`;
      return;
    }
    const half = (hover.offsetWidth || 0) / 2;
    const localX = clientX - rect.left;
    const clampedX = Math.max(half, Math.min(Math.max(half, rect.width - half), localX));
    hover.style.left = `${clampedX}px`;
  }

  resetTimelineScrubAndPreview = (clearCache) => {
    if (scrubClipSwitchTimer) {
      clearTimeout(scrubClipSwitchTimer);
      scrubClipSwitchTimer = null;
    }
    scrubbing = false;
    scrubWasPlaying = false;
    scrubWasEnded = false;
    scrubPendingOffset = null;
    scrubPendingClipTarget = null;
    scrubLatestUnix = null;
    if (state.daySession) {
      state.daySession.scrubbing = false;
    }
    track.classList.remove('scrubbing');

    cancelPendingHoverPreview();
    currentHoverTarget = null;
    displayedClipId = null;
    hidePreviewMedia();
    showPreviewImage('');
    hover?.classList.add('hidden');

    previewVideo.pause();
    previewVideoUrl = '';
    previewVideoBrokenUrl = '';
    previewVideo.removeAttribute('src');
    previewVideo.load();

    if (clearCache) {
      clearPreviewCache();
    }
  };

  onActiveClipScrubSeeked = (videoEl) => {
    if (!scrubbing || !state.daySession || videoEl !== getActiveClipVideo()) return;
    if (scrubPendingOffset === null) return;
    const next = scrubPendingOffset;
    scrubPendingOffset = null;
    const maxT = Number.isFinite(videoEl.duration) && videoEl.duration > 0
      ? Math.max(0, videoEl.duration - 0.05)
      : next;
    const target = Math.max(0, Math.min(maxT, next));
    if (Math.abs(videoEl.currentTime - target) > 0.04) {
      videoEl.currentTime = target;
    }
  };

  function clientXToInfo(clientX) {
    const session = state.daySession;
    const rect = track.getBoundingClientRect();
    const ratio = rect.width > 0 ? Math.max(0, Math.min(1, (clientX - rect.left) / rect.width)) : 0;
    const { winStart, winEnd } = getTimelineWindow(session);
    const unix = winStart + ratio * (winEnd - winStart);
    return { ratio, unix };
  }

  function updateHover(clientX) {
    const session = state.daySession;
    if (!session || !hover) return;
    if (isMobileViewport() && !scrubbing) {
      hover.classList.add('hidden');
      return;
    }

    const { ratio, unix } = clientXToInfo(clientX);
    let label = formatClockTime(unix);
    const hitIdx = session.clips.findIndex((c) => {
      const cEnd = Math.max(c.end_time || 0, c.start_time + Math.ceil(c.duration_sec || 1));
      return unix >= c.start_time && unix <= cEnd;
    });
    if (hitIdx >= 0) {
      label += ` · Clip ${hitIdx + 1}`;
    }
    if (hoverLabel) {
      hoverLabel.textContent = label;
    } else {
      hover.textContent = label;
    }

    if (scrubbing || hitIdx < 0 || isMobileViewport()) {
      cancelPendingHoverPreview();
      currentHoverTarget = null;
      displayedClipId = null;
      hidePreviewMedia();
      positionHoverPopover(clientX, ratio);
      return;
    }

    const clip = session.clips[hitIdx];
    const clipDur = Math.max(0.1, Number(clip.duration_sec) || Math.max(1, (clip.end_time || clip.start_time) - clip.start_time));
    const offsetSec = Math.max(0, Math.min(clipDur, unix - clip.start_time));
    const bucket = Math.round(offsetSec * 2);
    const qSec = bucket / 2;
    const cacheKey = `${clip.id}:${bucket}`;
    const prevKey = currentHoverTarget?.cacheKey;

    currentHoverTarget = { clipId: clip.id, clip, hitIdx, qSec, cacheKey };
    previewMedia?.classList.remove('hidden');
    hover.classList.add('has-preview');

    const cached = previewCache.get(cacheKey);
    if (cached) {
      previewCache.delete(cacheKey);
      previewCache.set(cacheKey, cached);
      showPreviewImage(cached.value);
      displayedClipId = clip.id;
      cancelPendingHoverPreview();
      positionHoverPopover(clientX, ratio);
      return;
    }

    if (displayedClipId !== clip.id) {
      showPreviewImage(clip.thumb_url || '');
      displayedClipId = clip.id;
    }
    positionHoverPopover(clientX, ratio);

    if (prevKey === cacheKey) {
      return;
    }
    cancelPendingHoverPreview();

    if (qSec < 1 && clip.thumb_url && hitIdx !== session.index) {
      return;
    }
    if (hitIdx === session.index && clip.video_url && previewVideoBrokenUrl !== clip.video_url) {
      const target = currentHoverTarget;
      hoverPreviewTimer = setTimeout(() => {
        hoverPreviewTimer = null;
        requestClientPreviewFrame(target);
      }, 60);
    } else {
      const target = currentHoverTarget;
      hoverPreviewTimer = setTimeout(() => {
        hoverPreviewTimer = null;
        fetchServerPreviewFrame(target);
      }, 140);
    }
  }

  function applyLiveScrub(unix, immediateSwitch) {
    const session = state.daySession;
    if (!session || !session.clips.length) return;
    const hit = findClipForUnix(session, unix);
    const video = getActiveClipVideo();

    if (hit.index === session.index) {
      if (scrubClipSwitchTimer) {
        clearTimeout(scrubClipSwitchTimer);
        scrubClipSwitchTimer = null;
      }
      scrubPendingClipTarget = null;
      if (video && video.readyState >= 1) {
        const maxT = Number.isFinite(video.duration) && video.duration > 0
          ? Math.max(0, video.duration - 0.05)
          : hit.offset;
        const target = Math.max(0, Math.min(maxT, hit.offset));
        if (video.seeking) {
          scrubPendingOffset = target;
        } else if (Math.abs(video.currentTime - target) > 0.04) {
          scrubPendingOffset = null;
          video.currentTime = target;
        }
      } else {
        session.pendingSeek = hit.offset;
      }
    } else {
      scrubPendingOffset = null;
      scrubPendingClipTarget = { index: hit.index, offset: hit.offset };
      if (immediateSwitch) {
        if (scrubClipSwitchTimer) {
          clearTimeout(scrubClipSwitchTimer);
          scrubClipSwitchTimer = null;
        }
        const target = scrubPendingClipTarget;
        scrubPendingClipTarget = null;
        playDayClip(target.index, target.offset, false);
      } else if (!scrubClipSwitchTimer) {
        scrubClipSwitchTimer = setTimeout(() => {
          scrubClipSwitchTimer = null;
          if (!scrubbing || !state.daySession || !scrubPendingClipTarget) return;
          const target = scrubPendingClipTarget;
          scrubPendingClipTarget = null;
          if (target.index !== state.daySession.index) {
            playDayClip(target.index, target.offset, false);
            if (scrubLatestUnix !== null) {
              updateDayPlayhead(scrubLatestUnix);
            }
          }
        }, 110);
      }
    }

    scrubLatestUnix = unix;
    updateDayPlayhead(unix);
  }

  track.addEventListener('pointerdown', (ev) => {
    const session = state.daySession;
    if (!session || ev.button > 0) return;
    ev.preventDefault();

    cancelPendingHoverPreview();
    currentHoverTarget = null;
    displayedClipId = null;

    const active = getActiveClipVideo();
    scrubWasPlaying = Boolean(active && !active.paused && !active.ended);
    scrubWasEnded = Boolean(active && active.ended);
    if (active && !active.paused) {
      active.pause();
    }

    scrubbing = true;
    session.scrubbing = true;
    scrubPendingOffset = null;
    scrubPendingClipTarget = null;
    track.classList.add('scrubbing');
    try { track.setPointerCapture(ev.pointerId); } catch {}

    const { unix } = clientXToInfo(ev.clientX);
    updateHover(ev.clientX);
    applyLiveScrub(unix, true);
  });

  track.addEventListener('pointermove', (ev) => {
    if (!state.daySession) return;
    if (scrubbing) {
      const { unix } = clientXToInfo(ev.clientX);
      updateHover(ev.clientX);
      applyLiveScrub(unix, false);
      return;
    }
    updateHover(ev.clientX);
  });

  function finishScrub(ev, cancelled) {
    if (!scrubbing) return;
    if (scrubClipSwitchTimer) {
      clearTimeout(scrubClipSwitchTimer);
      scrubClipSwitchTimer = null;
    }
    scrubPendingOffset = null;
    scrubPendingClipTarget = null;
    scrubLatestUnix = null;
    scrubbing = false;
    if (state.daySession) {
      state.daySession.scrubbing = false;
    }
    track.classList.remove('scrubbing');
    try { track.releasePointerCapture(ev.pointerId); } catch {}

    if (!cancelled) {
      const { unix } = clientXToInfo(ev.clientX);
      seekDayTimeline(unix, scrubWasPlaying || scrubWasEnded);
      if (!isMobileViewport()) {
        updateHover(ev.clientX);
      } else {
        hover?.classList.add('hidden');
      }
    } else {
      hover?.classList.add('hidden');
    }
  }

  track.addEventListener('pointerup', (ev) => finishScrub(ev, false));
  track.addEventListener('pointercancel', (ev) => finishScrub(ev, true));
  track.addEventListener('pointerleave', () => {
    if (!scrubbing) {
      cancelPendingHoverPreview();
      currentHoverTarget = null;
      displayedClipId = null;
      hidePreviewMedia();
      hover?.classList.add('hidden');
    }
  });

  (panel || track).addEventListener('wheel', (ev) => {
    const session = state.daySession;
    if (!session || session.tlMode !== '24h' || isMobileViewport()) return;
    ev.preventDefault();
    ev.stopPropagation();

    const rect = track.getBoundingClientRect();
    const fullSpan = Math.max(1, session.day_end - session.day_start);
    const winStart = session.tlViewStart ?? session.day_start;
    const winEnd = session.tlViewEnd ?? session.day_end;
    const curSpan = Math.max(MIN_TL_SPAN, Math.min(fullSpan, winEnd - winStart));

    if (ev.shiftKey || Math.abs(ev.deltaX) > Math.abs(ev.deltaY)) {
      if (curSpan >= fullSpan - 1) return;
      let delta = Math.abs(ev.deltaX) > Math.abs(ev.deltaY) ? ev.deltaX : ev.deltaY;
      if (ev.deltaMode === 1) delta *= 16;
      else if (ev.deltaMode === 2) delta *= 200;
      const panSec = (delta / Math.max(200, rect.width)) * curSpan * 0.8;
      const nextStart = Math.max(session.day_start, Math.min(session.day_end - curSpan, winStart + panSec));
      session.tlViewStart = nextStart;
      session.tlViewEnd = nextStart + curSpan;
    } else {
      const ratio = rect.width > 0 ? Math.max(0, Math.min(1, (ev.clientX - rect.left) / rect.width)) : 0.5;
      const anchorUnix = winStart + ratio * curSpan;
      let dy = ev.deltaY;
      if (ev.deltaMode === 1) dy *= 16;
      else if (ev.deltaMode === 2) dy *= 200;
      const factor = Math.exp(dy * 0.0025);
      const nextSpan = Math.max(MIN_TL_SPAN, Math.min(fullSpan, curSpan * factor));

      if (dy > 0 && nextSpan > fullSpan * 0.96) {
        session.tlViewStart = session.day_start;
        session.tlViewEnd = session.day_end;
      } else {
        let nextStart = anchorUnix - ratio * nextSpan;
        nextStart = Math.max(session.day_start, Math.min(session.day_end - nextSpan, nextStart));
        session.tlViewStart = nextStart;
        session.tlViewEnd = Math.min(session.day_end, nextStart + nextSpan);
      }
    }

    renderDayTimeline();
    updateHover(ev.clientX);
  }, { passive: false });
})();

document.querySelectorAll('[data-tl-mode]').forEach((btn) => {
  btn.addEventListener('click', () => {
    const session = state.daySession;
    if (!session) return;
    session.tlMode = btn.dataset.tlMode;
    if (session.tlMode === '24h') {
      session.tlViewStart = session.day_start;
      session.tlViewEnd = session.day_end;
    }
    renderDayTimeline();
  });
});

qs('#btn-tl-reset-zoom')?.addEventListener('click', () => {
  const session = state.daySession;
  if (!session) return;
  session.tlViewStart = session.day_start;
  session.tlViewEnd = session.day_end;
  renderDayTimeline();
});

qs('#btn-clip-prev')?.addEventListener('click', () => {
  if (state.daySession && state.daySession.index > 0) {
    playDayClip(state.daySession.index - 1, 0, true);
  }
});

qs('#btn-clip-prev-frame')?.addEventListener('click', () => stepDayFrame(-1));
qs('#btn-clip-next-frame')?.addEventListener('click', () => stepDayFrame(1));

qs('#btn-clip-next')?.addEventListener('click', () => {
  if (state.daySession && state.daySession.index < state.daySession.clips.length - 1) {
    playDayClip(state.daySession.index + 1, 0, true);
  }
});

qs('#btn-clip-play')?.addEventListener('click', () => {
  const video = getActiveClipVideo();
  const session = state.daySession;
  if (!video || !session) return;
  if (video.ended && session.index >= session.clips.length - 1) {
    playDayClip(0, 0, true);
    return;
  }
  if (video.paused) {
    video.play().catch(() => {});
  } else {
    video.pause();
  }
});

qs('#clip-speed')?.addEventListener('change', (ev) => {
  const rate = Number(ev.target.value) || 1;
  if (state.daySession) state.daySession.playbackRate = rate;
  clipDeck.forEach((el) => { el.playbackRate = rate; });
});

qs('#btn-clip-mute')?.addEventListener('click', () => {
  const video = getActiveClipVideo();
  if (!video) return;
  const nextMuted = !video.muted;
  video.muted = nextMuted;
  if (state.daySession) state.daySession.muted = nextMuted;
  qs('#btn-clip-mute').textContent = nextMuted ? 'Unmute' : 'Mute';
});

qs('#btn-clip-fs')?.addEventListener('click', () => {
  toggleStreamFullscreen(qs('#clip-modal'));
});

function closeClipModal() {
  exitStreamFullscreen(qs('#clip-modal'));
  state.clipZoomCtrl?.reset();
  resetTimelineScrubAndPreview(true);
  resetClipDeck();
  state.daySession = null;
  qs('#clip-modal').classList.add('hidden');
}

qs('#btn-clip-delete').addEventListener('click', async () => {
  const session = state.daySession;
  if (!session || !session.clips.length) return;
  const clip = session.clips[session.index];
  if (!clip) return;
  if (!confirm('Delete this recording clip permanently?')) return;
  const res = await fetch(`/api/recordings/${clip.id}`, { method: 'DELETE' });
  if (res.ok) {
    session.clips.splice(session.index, 1);
    if (!session.clips.length) {
      closeClipModal();
    } else {
      const nextIdx = Math.min(session.index, session.clips.length - 1);
      renderDayTimeline();
      playDayClip(nextIdx, 0, true);
    }
    loadRecordings(false);
    pollStatus();
  }
});

// ONVIF Discovery
qs('#btn-discover').addEventListener('click', async () => {
  const btn = qs('#btn-discover');
  const box = qs('#discover-results');
  btn.disabled = true;
  btn.textContent = 'Scanning...';
  box.innerHTML = '<div class="muted small">Sending WS-Discovery multicast probe (3s)...</div>';
  try {
    const res = await fetch('/api/discover');
    const data = await res.json();
    const devs = data.devices || [];
    if (!devs.length) {
      box.innerHTML = '<div class="muted small">No ONVIF devices responded.</div>';
    } else {
      box.innerHTML = '';
      for (const dev of devs) {
        const el = document.createElement('div');
        el.className = 'discover-item';
        el.innerHTML = `
          <div><strong>${escapeHTML(dev.name || 'ONVIF Camera')}</strong> <span class="muted">(${escapeHTML(dev.ip)})</span></div>
          ${(dev.xaddrs || []).map((x) => `<code>${escapeHTML(x)}</code>`).join('')}
        `;
        box.appendChild(el);
      }
    }
  } catch (err) {
    box.innerHTML = `<div class="muted small">Discovery error: ${escapeHTML(err.message)}</div>`;
  } finally {
    btn.disabled = false;
    btn.textContent = 'Scan LAN';
  }
});

// Modal close listeners
document.querySelectorAll('[data-close="live-modal"]').forEach((el) => {
  el.addEventListener('click', closeLiveModal);
});
document.querySelectorAll('[data-close="clip-modal"]').forEach((el) => {
  el.addEventListener('click', closeClipModal);
});
document.querySelectorAll('[data-close="camera-dialog"]').forEach((el) => {
  el.addEventListener('click', closeCameraDialog);
});
document.addEventListener('keydown', (ev) => {
  if (ev.key === 'Escape') {
    if (!qs('#cam-context-menu')?.classList.contains('hidden')) {
      closeCameraContextMenu();
      return;
    }
    if (!qs('#camera-dialog')?.classList.contains('hidden')) {
      closeCameraDialog();
      return;
    }
    if (isStreamFullscreen(qs('#live-modal'))) {
      exitStreamFullscreen(qs('#live-modal'));
      return;
    }
    if (isStreamFullscreen(qs('#clip-modal'))) {
      exitStreamFullscreen(qs('#clip-modal'));
      return;
    }
    if (state.activeModalCam) closeLiveModal();
    if (state.daySession) closeClipModal();
    return;
  }
  const tag = ev.target?.tagName;
  if (tag === 'INPUT' || tag === 'TEXTAREA' || tag === 'SELECT') return;
  if (!qs('#camera-dialog')?.classList.contains('hidden')) return;

  if (ev.key === '0') {
    if (state.activeModalCam) state.liveZoomCtrl?.reset();
    if (state.daySession) state.clipZoomCtrl?.reset();
    return;
  }

  const activeVideo = getActiveClipVideo();
  if (!state.daySession || !activeVideo) return;
  if (ev.key === ',') {
    ev.preventDefault();
    stepDayFrame(-1);
    return;
  }
  if (ev.key === '.') {
    ev.preventDefault();
    stepDayFrame(1);
    return;
  }
  if (ev.key === ' ' || ev.code === 'Space') {
    if (tag === 'BUTTON' || tag === 'A') return;
    ev.preventDefault();
    if (activeVideo.ended && state.daySession.index >= state.daySession.clips.length - 1) {
      playDayClip(0, 0, true);
    } else if (activeVideo.paused) {
      activeVideo.play().catch(() => {});
    } else {
      activeVideo.pause();
    }
  } else if (ev.key === 'ArrowLeft') {
    ev.preventDefault();
    const nextT = activeVideo.currentTime - 5;
    if (nextT >= 0) {
      activeVideo.currentTime = nextT;
      updateDayPlayhead();
    } else if (state.daySession.index > 0) {
      const prevClip = state.daySession.clips[state.daySession.index - 1];
      const prevDur = prevClip.duration_sec || 0;
      playDayClip(state.daySession.index - 1, Math.max(0, prevDur + nextT), true);
    } else {
      activeVideo.currentTime = 0;
      updateDayPlayhead();
    }
  } else if (ev.key === 'ArrowRight') {
    ev.preventDefault();
    const dur = Number.isFinite(activeVideo.duration) ? activeVideo.duration : (state.daySession.clips[state.daySession.index]?.duration_sec || 0);
    const nextT = activeVideo.currentTime + 5;
    if (nextT < dur) {
      activeVideo.currentTime = nextT;
      updateDayPlayhead();
    } else if (state.daySession.index < state.daySession.clips.length - 1) {
      playDayClip(state.daySession.index + 1, 0, true);
    }
  }
});

// Storage cap settings
async function loadStorageSettings() {
  try {
    const res = await fetch('/api/settings/storage', { cache: 'no-store' });
    if (!res.ok) return;
    const data = await res.json();
    if (typeof data.max_gb === 'number' && data.max_gb > 0) {
      qs('#input-max-gb').value = String(data.max_gb);
    }
  } catch {}
}

qs('#btn-save-cap').addEventListener('click', async () => {
  const input = qs('#input-max-gb');
  const msg = qs('#cap-save-msg');
  const btn = qs('#btn-save-cap');
  const val = Number(input.value);
  if (!Number.isFinite(val) || val <= 0) {
    msg.textContent = 'Enter a valid GB value > 0';
    return;
  }
  btn.disabled = true;
  msg.textContent = 'Saving...';
  try {
    const res = await fetch('/api/settings/storage', {
      method: 'PUT',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ max_gb: val }),
    });
    const data = await res.json();
    if (!res.ok) {
      msg.textContent = data.error || `Error ${res.status}`;
      return;
    }
    msg.textContent = data.persisted ? 'Saved to config' : 'Saved (in memory)';
    pollStatus();
  } catch (err) {
    msg.textContent = err.message;
  } finally {
    btn.disabled = false;
  }
});

// Right-click Camera Context Menu & Unified Add/Edit Camera Dialog
function closeCameraContextMenu() {
  state.contextMenuCam = null;
  qs('#cam-context-menu')?.classList.add('hidden');
}

function openCameraContextMenu(camID, x, y) {
  const menu = qs('#cam-context-menu');
  const cam = state.cameras.find((c) => c.id === camID);
  if (!menu || !cam) return;

  state.contextMenuCam = camID;
  qs('#ctx-cam-name').textContent = cam.name || camID;
  const toggleBtn = qs('#ctx-toggle-stream');
  if (toggleBtn) {
    toggleBtn.textContent = cam.live_stream === 'main'
      ? 'Switch Live to Sub Stream'
      : 'Switch Live to Main Stream';
  }

  menu.style.left = '0px';
  menu.style.top = '0px';
  menu.classList.remove('hidden');

  const rect = menu.getBoundingClientRect();
  const left = Math.max(8, Math.min(x, window.innerWidth - rect.width - 8));
  const top = Math.max(8, Math.min(y, window.innerHeight - rect.height - 8));
  menu.style.left = `${left}px`;
  menu.style.top = `${top}px`;
}

function dtoToUpdatePayload(dto, overrides = {}) {
  const m = dto.motion || {};
  const p = dto.ptz || {};
  return {
    id: dto.id,
    name: dto.name,
    enabled: Boolean(dto.enabled),
    main_rtsp: dto.main_rtsp || '',
    sub_rtsp: dto.sub_rtsp || '',
    username: dto.username || '',
    password: '',
    live_stream: overrides.live_stream ?? (dto.live_stream || 'sub'),
    resolution: dto.resolution || '',
    aspect_ratio: dto.aspect_ratio || '',
    pre_record_sec: dto.pre_record_sec ?? null,
    post_record_sec: dto.post_record_sec ?? null,
    max_clip_sec: dto.max_clip_sec ?? null,
    motion: {
      mode: m.mode || 'pixel',
      onvif_url: m.onvif_url || '',
      username: m.username || '',
      password: '',
      keep_password: true,
      dvr: m.dvr || '',
      channel: m.channel || 0,
      fps: m.fps || 3,
      width: m.width || 320,
      height: m.height || 180,
      threshold: m.threshold || 25,
      min_area_percent: m.min_area_percent || 1.2,
      max_area_percent: m.max_area_percent || 85,
      min_frames: m.min_frames || 2,
      ignore_zones: m.ignore_zones || null,
    },
    ptz: {
      enabled: Boolean(p.enabled),
      type: p.type || 'onvif',
      url: p.url || '',
      username: p.username || '',
      password: '',
      keep_password: true,
      profile_token: p.profile_token || '',
      channel: p.channel || 0,
    },
  };
}

async function toggleCameraLiveStream(camID) {
  const res = await fetch(`/api/cameras/${encodeURIComponent(camID)}`, { cache: 'no-store' });
  if (!res.ok) return;
  const dto = await res.json();
  const nextLive = dto.live_stream === 'main' ? 'sub' : 'main';
  const putRes = await fetch(`/api/cameras/${encodeURIComponent(camID)}`, {
    method: 'PUT',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(dtoToUpdatePayload(dto, { live_stream: nextLive })),
  });
  if (putRes.ok) {
    pollStatus();
  }
}

async function deleteCameraByID(camID) {
  const cam = state.cameras.find((c) => c.id === camID);
  const label = cam?.name || camID;
  if (!confirm(`Remove camera "${label}" from config? Existing recorded clips will be kept on disk.`)) {
    return false;
  }
  const res = await fetch(`/api/cameras/${encodeURIComponent(camID)}`, { method: 'DELETE' });
  const data = await res.json().catch(() => ({}));
  if (!res.ok) {
    const errEl = qs('#cam-dlg-error');
    if (errEl && !qs('#camera-dialog')?.classList.contains('hidden')) {
      errEl.textContent = data.error || `Delete failed (HTTP ${res.status})`;
    } else {
      alert(data.error || `Delete failed (HTTP ${res.status})`);
    }
    return false;
  }
  if (state.activeModalCam === camID) closeLiveModal();
  if (state.cameraDialog.id === camID && !qs('#camera-dialog')?.classList.contains('hidden')) {
    closeCameraDialog();
  }
  pollStatus();
  return true;
}

qs('#cam-context-menu')?.addEventListener('click', async (ev) => {
  const btn = ev.target.closest('[data-ctx-action]');
  if (!btn) return;
  const action = btn.dataset.ctxAction;
  const camID = state.contextMenuCam;
  closeCameraContextMenu();
  if (!camID) return;

  switch (action) {
    case 'settings':
      openCameraDialog(camID);
      break;
    case 'open':
      openLiveModal(camID);
      break;
    case 'toggle-stream':
      await toggleCameraLiveStream(camID);
      break;
    case 'record':
      await triggerCamera(camID);
      pollStatus();
      break;
    case 'delete':
      await deleteCameraByID(camID);
      break;
  }
});

window.addEventListener('pointerdown', (ev) => {
  const menu = qs('#cam-context-menu');
  if (menu && !menu.classList.contains('hidden') && !menu.contains(ev.target)) {
    closeCameraContextMenu();
  }
});
window.addEventListener('scroll', closeCameraContextMenu, true);
window.addEventListener('resize', closeCameraContextMenu);
document.addEventListener('contextmenu', (ev) => {
  if (!ev.target.closest('.cam-tile')) {
    closeCameraContextMenu();
  }
});

function extractHostPort(raw) {
  const s = String(raw || '').trim();
  if (!s) return { host: '', port: '' };
  try {
    const withScheme = s.includes('://') ? s : `http://${s}`;
    const u = new URL(withScheme);
    return { host: u.hostname || '', port: u.port || '' };
  } catch {
    return { host: '', port: '' };
  }
}

function updateCameraFormVisibility() {
  const mode = qs('#cam-motion-mode')?.value || 'pixel';
  const showONVIF = mode === 'onvif' || mode === 'onvif_or_pixel';
  const showDVR = mode === 'hikvision_isapi' || mode === 'isapi_or_pixel';
  const showPixel = mode === 'pixel' || mode === 'onvif_or_pixel' || mode === 'isapi_or_pixel';

  qs('#cam-motion-onvif-group')?.classList.toggle('hidden', !showONVIF);
  qs('#cam-motion-dvr-group')?.classList.toggle('hidden', !showDVR);
  qs('#cam-motion-pixel-group')?.classList.toggle('hidden', !showPixel);

  const ptzEnabled = Boolean(qs('#cam-ptz-enabled')?.checked);
  const ptzType = qs('#cam-ptz-type')?.value || 'onvif';
  qs('#cam-ptz-group')?.classList.toggle('hidden', !ptzEnabled);
  qs('.ptz-onvif-field')?.classList.toggle('hidden', ptzType === 'hikvision_isapi');
  qs('.ptz-isapi-field')?.classList.toggle('hidden', ptzType !== 'hikvision_isapi');
}

function populateDVRSelect(dvrs, selectedID = '') {
  const sel = qs('#cam-motion-dvr');
  if (!sel) return;
  sel.innerHTML = '<option value="">Select DVR...</option>';
  const seen = new Set();
  for (const d of dvrs || []) {
    seen.add(d.id);
    const opt = document.createElement('option');
    opt.value = d.id;
    opt.textContent = d.host ? `${d.id} (${d.host}:${d.port || 80})` : d.id;
    sel.appendChild(opt);
  }
  if (selectedID && !seen.has(selectedID)) {
    const opt = document.createElement('option');
    opt.value = selectedID;
    opt.textContent = selectedID;
    sel.appendChild(opt);
  }
  sel.value = selectedID || (dvrs?.length === 1 ? dvrs[0].id : '');
}

function closeCameraDialog() {
  qs('#camera-dialog')?.classList.add('hidden');
  qs('#cam-dlg-error').textContent = '';
}

async function openCameraDialog(camID = null) {
  closeCameraContextMenu();
  const isEdit = Boolean(camID);
  state.cameraDialog = {
    mode: isEdit ? 'edit' : 'add',
    id: camID,
    idManual: isEdit,
    ignoreZones: null,
  };

  qs('#cam-dlg-title').textContent = isEdit ? 'Camera Settings' : 'Add Camera';
  qs('#cam-dlg-subtitle').textContent = isEdit
    ? `Editing camera "${camID}" · Saves to config.yaml and hot-reloads streams`
    : 'Auto-detect ONVIF streams, test connectivity, and save without restarting.';
  qs('#cam-dlg-error').textContent = '';
  qs('#cam-scan-status').textContent = '';
  qs('#cam-scan-results').innerHTML = '';
  qs('#cam-scan-results').classList.add('hidden');
  qs('#cam-probe-status').textContent = '';
  qs('#cam-probe-profiles').innerHTML = '';
  qs('#cam-probe-profiles').classList.add('hidden');
  qs('#cam-test-preview').classList.add('hidden');
  qs('#cam-test-img').classList.add('hidden');
  qs('#cam-test-img').removeAttribute('src');
  qs('#cam-test-meta').textContent = '';
  qs('#cam-test-msg').textContent = '';

  let dvrs = [];
  let dto = null;
  try {
    const listReq = fetch('/api/cameras', { cache: 'no-store' });
    const detailReq = isEdit ? fetch(`/api/cameras/${encodeURIComponent(camID)}`, { cache: 'no-store' }) : Promise.resolve(null);
    const [listRes, detailRes] = await Promise.all([listReq, detailReq]);
    if (listRes.ok) {
      const listData = await listRes.json();
      dvrs = listData.dvrs || [];
    }
    if (isEdit) {
      if (!detailRes || !detailRes.ok) {
        alert(`Failed to load camera settings (HTTP ${detailRes?.status || 'err'})`);
        return;
      }
      dto = await detailRes.json();
    }
  } catch (err) {
    alert(`Failed to load camera dialog: ${err.message}`);
    return;
  }

  const idInput = qs('#cam-id');
  const delBtn = qs('#btn-cam-delete');
  if (isEdit && dto) {
    state.cameraDialog.ignoreZones = dto.motion?.ignore_zones || null;
    const inferred = extractHostPort(dto.motion?.onvif_url || dto.ptz?.url || dto.main_rtsp || dto.sub_rtsp);
    qs('#cam-probe-host').value = inferred.host;
    qs('#cam-probe-port').value = dto.motion?.onvif_url && inferred.port ? inferred.port : '80';
    qs('#cam-user').value = dto.username || '';
    qs('#cam-pass').value = '';
    qs('#cam-pass').placeholder = (dto.has_main_password || dto.has_sub_password)
      ? 'Saved (leave blank to keep)'
      : 'Enter password';

    qs('#cam-name').value = dto.name || '';
    idInput.value = dto.id || '';
    idInput.disabled = true;
    qs('#cam-enabled').checked = Boolean(dto.enabled);
    qs('#cam-main-rtsp').value = dto.main_rtsp || '';
    qs('#cam-sub-rtsp').value = dto.sub_rtsp || '';
    qs('#cam-live-stream').value = dto.live_stream || 'sub';
    qs('#cam-resolution').value = dto.resolution || '';
    qs('#cam-aspect-ratio').value = dto.aspect_ratio || '';

    const m = dto.motion || {};
    qs('#cam-motion-mode').value = m.mode || 'pixel';
    qs('#cam-motion-onvif-url').value = m.onvif_url || '';
    qs('#cam-motion-user').value = m.username || '';
    qs('#cam-motion-pass').value = '';
    qs('#cam-motion-pass').placeholder = m.has_password ? 'Saved (leave blank to keep)' : 'Leave blank to share camera password';
    populateDVRSelect(dvrs, m.dvr || '');
    qs('#cam-motion-channel').value = m.channel ? String(m.channel) : '1';
    qs('#cam-motion-fps').value = String(m.fps || 3);
    qs('#cam-motion-width').value = String(m.width || 320);
    qs('#cam-motion-height').value = String(m.height || 180);
    qs('#cam-motion-threshold').value = String(m.threshold || 25);
    qs('#cam-motion-min-area').value = String(m.min_area_percent || 1.2);
    qs('#cam-motion-max-area').value = String(m.max_area_percent || 85);
    qs('#cam-motion-min-frames').value = String(m.min_frames || 2);

    qs('#cam-pre-rec').value = dto.pre_record_sec ?? '';
    qs('#cam-post-rec').value = dto.post_record_sec ?? '';
    qs('#cam-max-clip').value = dto.max_clip_sec ?? '';

    const p = dto.ptz || {};
    qs('#cam-ptz-enabled').checked = Boolean(p.enabled);
    qs('#cam-ptz-type').value = p.type || 'onvif';
    qs('#cam-ptz-url').value = p.url || '';
    qs('#cam-ptz-user').value = p.username || '';
    qs('#cam-ptz-pass').value = '';
    qs('#cam-ptz-pass').placeholder = p.has_password ? 'Saved (leave blank to keep)' : 'Leave blank to share camera password';
    qs('#cam-ptz-token').value = p.profile_token || '';
    qs('#cam-ptz-channel').value = p.channel ? String(p.channel) : '';

    delBtn?.classList.remove('hidden');
  } else {
    qs('#cam-probe-host').value = '';
    qs('#cam-probe-port').value = '80';
    qs('#cam-user').value = '';
    qs('#cam-pass').value = '';
    qs('#cam-pass').placeholder = 'Enter password';

    qs('#cam-name').value = '';
    idInput.value = '';
    idInput.disabled = false;
    qs('#cam-enabled').checked = true;
    qs('#cam-main-rtsp').value = '';
    qs('#cam-sub-rtsp').value = '';
    qs('#cam-live-stream').value = 'sub';
    qs('#cam-resolution').value = '';
    qs('#cam-aspect-ratio').value = '';

    qs('#cam-motion-mode').value = 'pixel';
    qs('#cam-motion-onvif-url').value = '';
    qs('#cam-motion-user').value = '';
    qs('#cam-motion-pass').value = '';
    qs('#cam-motion-pass').placeholder = 'Leave blank to share camera password';
    populateDVRSelect(dvrs, '');
    qs('#cam-motion-channel').value = '1';
    qs('#cam-motion-fps').value = '3';
    qs('#cam-motion-width').value = '320';
    qs('#cam-motion-height').value = '180';
    qs('#cam-motion-threshold').value = '25';
    qs('#cam-motion-min-area').value = '1.2';
    qs('#cam-motion-max-area').value = '85';
    qs('#cam-motion-min-frames').value = '2';

    qs('#cam-pre-rec').value = '';
    qs('#cam-post-rec').value = '';
    qs('#cam-max-clip').value = '';

    qs('#cam-ptz-enabled').checked = false;
    qs('#cam-ptz-type').value = 'onvif';
    qs('#cam-ptz-url').value = '';
    qs('#cam-ptz-user').value = '';
    qs('#cam-ptz-pass').value = '';
    qs('#cam-ptz-pass').placeholder = 'Leave blank to share camera password';
    qs('#cam-ptz-token').value = '';
    qs('#cam-ptz-channel').value = '';

    delBtn?.classList.add('hidden');
  }

  updateCameraFormVisibility();
  qs('#camera-dialog')?.classList.remove('hidden');
}

qs('#btn-add-camera')?.addEventListener('click', () => openCameraDialog(null));
qs('#cam-motion-mode')?.addEventListener('change', updateCameraFormVisibility);
qs('#cam-ptz-enabled')?.addEventListener('change', updateCameraFormVisibility);
qs('#cam-ptz-type')?.addEventListener('change', updateCameraFormVisibility);

qs('#cam-name')?.addEventListener('input', () => {
  if (state.cameraDialog.mode === 'add' && !state.cameraDialog.idManual) {
    const v = qs('#cam-name').value;
    qs('#cam-id').value = v.trim() ? slugifyCameraName(v) : '';
  }
});

qs('#cam-id')?.addEventListener('input', () => {
  state.cameraDialog.idManual = qs('#cam-id').value.trim() !== '';
});

qs('#btn-cam-scan-lan')?.addEventListener('click', async () => {
  const btn = qs('#btn-cam-scan-lan');
  const status = qs('#cam-scan-status');
  const box = qs('#cam-scan-results');
  btn.disabled = true;
  status.textContent = 'Scanning LAN (3s)...';
  box.innerHTML = '';
  box.classList.add('hidden');
  try {
    const res = await fetch('/api/discover', { cache: 'no-store' });
    const data = await res.json();
    const devs = data.devices || [];
    if (!devs.length) {
      status.textContent = 'No ONVIF devices found.';
      return;
    }
    status.textContent = `Found ${devs.length} device(s) — click to fill host:`;
    box.classList.remove('hidden');
    for (const dev of devs) {
      const chip = document.createElement('button');
      chip.type = 'button';
      chip.className = 'scan-chip';
      chip.textContent = `${dev.name || 'ONVIF Camera'} (${dev.ip})`;
      chip.addEventListener('click', () => {
        const xaddr = (dev.xaddrs || [])[0] || '';
        if (xaddr) {
          qs('#cam-probe-host').value = xaddr;
          const parsed = extractHostPort(xaddr);
          if (parsed.port) qs('#cam-probe-port').value = parsed.port;
        } else {
          qs('#cam-probe-host').value = dev.ip || '';
        }
        if (!qs('#cam-name').value.trim() && dev.name) {
          qs('#cam-name').value = dev.name;
          qs('#cam-name').dispatchEvent(new Event('input'));
        }
      });
      box.appendChild(chip);
    }
  } catch (err) {
    status.textContent = `Scan error: ${err.message}`;
  } finally {
    btn.disabled = false;
  }
});

qs('#btn-cam-autodetect')?.addEventListener('click', async () => {
  const btn = qs('#btn-cam-autodetect');
  const status = qs('#cam-probe-status');
  const profBox = qs('#cam-probe-profiles');

  let host = qs('#cam-probe-host').value.trim();
  if (!host) {
    const fallback = extractHostPort(qs('#cam-motion-onvif-url').value || qs('#cam-main-rtsp').value);
    host = fallback.host;
    if (host) qs('#cam-probe-host').value = host;
  }
  if (!host) {
    status.textContent = 'Enter a Camera Host / IP / ONVIF URL first.';
    return;
  }

  btn.disabled = true;
  status.textContent = 'Probing ONVIF capabilities & stream URIs...';
  profBox.innerHTML = '';
  profBox.classList.add('hidden');

  try {
    const res = await fetch('/api/cameras/probe', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({
        host,
        port: Number(qs('#cam-probe-port').value) || 80,
        username: qs('#cam-user').value.trim(),
        password: qs('#cam-pass').value,
        camera_id: state.cameraDialog.mode === 'edit' ? state.cameraDialog.id : '',
      }),
    });
    const data = await res.json().catch(() => ({}));
    if (!res.ok) {
      status.textContent = data.error || `Probe failed (HTTP ${res.status})`;
      return;
    }

    if (!qs('#cam-name').value.trim() && data.name) {
      qs('#cam-name').value = data.name;
      qs('#cam-name').dispatchEvent(new Event('input'));
    }
    if (data.main_rtsp) qs('#cam-main-rtsp').value = data.main_rtsp;
    if (data.sub_rtsp) qs('#cam-sub-rtsp').value = data.sub_rtsp;
    if (data.onvif_url) qs('#cam-motion-onvif-url').value = data.onvif_url;
    if (data.suggested_motion_mode) qs('#cam-motion-mode').value = data.suggested_motion_mode;

    qs('#cam-ptz-enabled').checked = Boolean(data.ptz_enabled);
    if (data.ptz_enabled) {
      qs('#cam-ptz-type').value = data.ptz_type || 'onvif';
      if (data.ptz_url) qs('#cam-ptz-url').value = data.ptz_url;
      if (data.ptz_profile_token) qs('#cam-ptz-token').value = data.ptz_profile_token;
      if (data.ptz_channel) qs('#cam-ptz-channel').value = String(data.ptz_channel);
    }

    const profiles = data.profiles || [];
    if (profiles.length) {
      const mainProf = profiles.find((p) => p.rtsp_url === data.main_rtsp) || profiles[0];
      if (mainProf?.width && mainProf?.height) {
        qs('#cam-resolution').value = `${mainProf.width}x${mainProf.height}`;
      }
      profBox.classList.remove('hidden');
      for (const p of profiles) {
        const card = document.createElement('div');
        card.className = 'probe-profile-card';
        const info = document.createElement('span');
        const spec = [
          p.name || p.token,
          p.width && p.height ? `${p.width}x${p.height}` : '',
          p.codec || '',
          p.fps ? `${p.fps}fps` : '',
        ].filter(Boolean).join(' · ');
        info.textContent = spec;

        const actions = document.createElement('div');
        actions.className = 'probe-profile-actions';
        const useMain = document.createElement('button');
        useMain.type = 'button';
        useMain.className = 'btn btn-secondary btn-sm';
        useMain.textContent = 'Main';
        useMain.addEventListener('click', () => {
          qs('#cam-main-rtsp').value = p.rtsp_url || '';
          if (p.width && p.height) qs('#cam-resolution').value = `${p.width}x${p.height}`;
        });
        const useSub = document.createElement('button');
        useSub.type = 'button';
        useSub.className = 'btn btn-secondary btn-sm';
        useSub.textContent = 'Sub';
        useSub.addEventListener('click', () => {
          qs('#cam-sub-rtsp').value = p.rtsp_url || '';
        });
        actions.append(useMain, useSub);
        card.append(info, actions);
        profBox.appendChild(card);
      }
    }

    updateCameraFormVisibility();
    const parts = [
      [data.manufacturer, data.model].filter(Boolean).join(' ') || 'Detected',
      data.pull_point_supported === true ? 'PullPoint supported' : (data.pull_point_supported === false ? 'Pixel fallback' : ''),
      data.ptz_enabled ? `PTZ (${data.ptz_type || 'onvif'})` : 'No PTZ',
    ].filter(Boolean);
    status.textContent = parts.join(' · ');
  } catch (err) {
    status.textContent = `Auto-Detect error: ${err.message}`;
  } finally {
    btn.disabled = false;
  }
});

async function testCameraStream(streamKind) {
  const input = streamKind === 'sub' ? qs('#cam-sub-rtsp') : qs('#cam-main-rtsp');
  const rtspURL = input?.value.trim() || '';
  const box = qs('#cam-test-preview');
  const img = qs('#cam-test-img');
  const meta = qs('#cam-test-meta');
  const msg = qs('#cam-test-msg');

  box.classList.remove('hidden');
  img.classList.add('hidden');
  img.removeAttribute('src');

  if (!rtspURL) {
    meta.textContent = `${streamKind.toUpperCase()} Stream`;
    msg.textContent = `Enter a ${streamKind} RTSP URL first.`;
    return;
  }

  meta.textContent = `Testing ${streamKind.toUpperCase()} stream via go2rtc...`;
  msg.textContent = '';

  const btnMain = qs('#btn-cam-test-main');
  const btnSub = qs('#btn-cam-test-sub');
  if (btnMain) btnMain.disabled = true;
  if (btnSub) btnSub.disabled = true;

  try {
    const res = await fetch('/api/cameras/test', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({
        rtsp_url: rtspURL,
        username: qs('#cam-user').value.trim(),
        password: qs('#cam-pass').value,
        camera_id: state.cameraDialog.mode === 'edit' ? state.cameraDialog.id : '',
        stream_kind: streamKind,
      }),
    });
    const data = await res.json().catch(() => ({}));
    if (res.ok && data.ok) {
      if (data.snapshot_b64) {
        img.src = `data:image/jpeg;base64,${data.snapshot_b64}`;
        img.classList.remove('hidden');
      }
      const dims = data.width && data.height ? `${data.width}x${data.height}` : 'Dimensions unknown';
      meta.textContent = `${streamKind.toUpperCase()} OK · ${dims} · ${data.codec || 'unknown'} · ${data.latency_ms} ms`;
      msg.textContent = data.error || '';
      if (streamKind === 'main' && data.width && data.height && !qs('#cam-resolution').value.trim()) {
        qs('#cam-resolution').value = `${data.width}x${data.height}`;
      }
    } else {
      meta.textContent = `${streamKind.toUpperCase()} Test Failed`;
      msg.textContent = data.error || `HTTP ${res.status}`;
    }
  } catch (err) {
    meta.textContent = `${streamKind.toUpperCase()} Test Error`;
    msg.textContent = err.message;
  } finally {
    if (btnMain) btnMain.disabled = false;
    if (btnSub) btnSub.disabled = false;
  }
}

qs('#btn-cam-test-main')?.addEventListener('click', () => testCameraStream('main'));
qs('#btn-cam-test-sub')?.addEventListener('click', () => testCameraStream('sub'));

qs('#btn-cam-delete')?.addEventListener('click', async () => {
  if (state.cameraDialog.mode === 'edit' && state.cameraDialog.id) {
    await deleteCameraByID(state.cameraDialog.id);
  }
});

qs('#cam-dlg-form')?.addEventListener('submit', async (ev) => {
  ev.preventDefault();
  const errEl = qs('#cam-dlg-error');
  const saveBtn = qs('#btn-cam-save');
  errEl.textContent = '';

  const isEdit = state.cameraDialog.mode === 'edit';
  const optInt = (el) => {
    const s = el?.value.trim() || '';
    if (s === '') return null;
    const n = Number(s);
    return Number.isFinite(n) ? Math.trunc(n) : null;
  };
  const intOr = (el, def) => {
    const n = Number(el?.value);
    return Number.isFinite(n) && n > 0 ? Math.trunc(n) : def;
  };

  const payload = {
    id: isEdit ? state.cameraDialog.id : qs('#cam-id').value.trim(),
    name: qs('#cam-name').value.trim(),
    enabled: Boolean(qs('#cam-enabled').checked),
    main_rtsp: qs('#cam-main-rtsp').value.trim(),
    sub_rtsp: qs('#cam-sub-rtsp').value.trim(),
    username: qs('#cam-user').value.trim(),
    password: qs('#cam-pass').value,
    live_stream: qs('#cam-live-stream').value || 'sub',
    resolution: qs('#cam-resolution').value.trim(),
    aspect_ratio: qs('#cam-aspect-ratio').value || '',
    pre_record_sec: optInt(qs('#cam-pre-rec')),
    post_record_sec: optInt(qs('#cam-post-rec')),
    max_clip_sec: optInt(qs('#cam-max-clip')),
    motion: {
      mode: qs('#cam-motion-mode').value || 'pixel',
      onvif_url: qs('#cam-motion-onvif-url').value.trim(),
      username: qs('#cam-motion-user').value.trim(),
      password: qs('#cam-motion-pass').value,
      keep_password: isEdit && qs('#cam-motion-pass').value === '',
      dvr: qs('#cam-motion-dvr').value || '',
      channel: Number(qs('#cam-motion-channel').value) || 0,
      fps: intOr(qs('#cam-motion-fps'), 3),
      width: intOr(qs('#cam-motion-width'), 320),
      height: intOr(qs('#cam-motion-height'), 180),
      threshold: intOr(qs('#cam-motion-threshold'), 25),
      min_area_percent: Number(qs('#cam-motion-min-area').value) || 1.2,
      max_area_percent: Number(qs('#cam-motion-max-area').value) || 85,
      min_frames: intOr(qs('#cam-motion-min-frames'), 2),
      ignore_zones: state.cameraDialog.ignoreZones,
    },
    ptz: {
      enabled: Boolean(qs('#cam-ptz-enabled').checked),
      type: qs('#cam-ptz-type').value || 'onvif',
      url: qs('#cam-ptz-url').value.trim(),
      username: qs('#cam-ptz-user').value.trim(),
      password: qs('#cam-ptz-pass').value,
      keep_password: isEdit && qs('#cam-ptz-pass').value === '',
      profile_token: qs('#cam-ptz-token').value.trim(),
      channel: Number(qs('#cam-ptz-channel').value) || 0,
    },
  };

  saveBtn.disabled = true;
  try {
    const url = isEdit
      ? `/api/cameras/${encodeURIComponent(state.cameraDialog.id)}`
      : '/api/cameras';
    const res = await fetch(url, {
      method: isEdit ? 'PUT' : 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(payload),
    });
    const data = await res.json().catch(() => ({}));
    if (!res.ok) {
      errEl.textContent = data.error || `Save failed (HTTP ${res.status})`;
      return;
    }
    closeCameraDialog();
    pollStatus();
  } catch (err) {
    errEl.textContent = err.message;
  } finally {
    saveBtn.disabled = false;
  }
});

// Start status polling
loadLayoutSettings();
loadStorageSettings();
pollStatus();
state.statusTimer = setInterval(pollStatus, 3000);
