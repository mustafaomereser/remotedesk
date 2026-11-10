'use strict';

const $ = (id) => document.getElementById(id);
let state = null;
let showPw = false;

// Viewer penceresi açıldığında bağlantı bilgisini buradan alır (şifre URL'ye yazılmaz).
const pendingConnections = [];
window.takePendingConnection = () => pendingConnections.shift() || null;

async function api(path, body) {
  const r = await fetch('/api/' + path, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(body || {}),
  });
  if (!r.ok) throw new Error((await r.text()).trim() || r.statusText);
}

function toast(msg) {
  const t = $('toast');
  t.textContent = msg;
  t.classList.add('show');
  clearTimeout(toast._t);
  toast._t = setTimeout(() => t.classList.remove('show'), 1600);
}

async function copy(text, what) {
  if (!text) return;
  try {
    await navigator.clipboard.writeText(text);
    toast(what + ' kopyalandı');
  } catch {
    toast('Kopyalanamadı');
  }
}

// ---- durum ----

function connectEvents() {
  const es = new EventSource('/api/events');
  es.onmessage = (e) => render(JSON.parse(e.data));
  es.onerror = () => {
    $('tunDot').className = 'dot err';
    $('tunMsg').textContent = 'Uygulamayla bağlantı koptu';
  };
}

function render(s) {
  state = s;
  $('myId').textContent = s.id || '—';
  $('myPw').textContent = showPw ? s.password : '•'.repeat(s.password.length);
  $('pwShow').textContent = showPw ? 'Gizle' : 'Göster';

  const dot = $('tunDot');
  dot.className = 'dot ' + (s.tunnel === 'on' ? 'ok' : s.tunnel === 'error' ? 'err' : 'busy');
  $('tunMsg').textContent = s.tunnelMsg;
  $('vidDot').className = 'dot ' + (s.video === 'on' ? 'ok' : s.video === 'error' ? 'err' : 'busy');
  $('vidMsg').textContent = s.videoMsg;
  $('autoAccept').checked = s.autoAccept;

  const wb = $('warnings');
  wb.innerHTML = '';
  for (const w of s.warnings || []) {
    const d = document.createElement('div');
    d.className = 'warn';
    d.textContent = w;
    wb.appendChild(d);
  }

  if (s.session) {
    $('sessionBox').classList.remove('hidden');
    $('sessRemote').textContent = s.session.remote;
  } else {
    $('sessionBox').classList.add('hidden');
  }

  if (s.pending) {
    $('reqRemote').textContent = s.pending.remote;
    $('modal').classList.remove('hidden');
  } else {
    $('modal').classList.add('hidden');
  }
}

setInterval(() => {
  if (!state) return;
  if (state.pending) {
    const left = Math.max(0, state.pending.expires - Date.now());
    $('reqBar').style.width = (left / 30000) * 100 + '%';
  }
  if (state.session) {
    const sec = Math.floor((Date.now() - state.session.since) / 1000);
    const m = Math.floor(sec / 60), s = sec % 60;
    $('sessTime').textContent = `· ${m}:${String(s).padStart(2, '0')}`;
  }
}, 250);

// ---- bu bilgisayar ----

$('idCopy').onclick = () => copy(state && state.id, 'ID');
$('pwCopy').onclick = () => copy(state && state.password, 'Şifre');
$('pwShow').onclick = () => { showPw = !showPw; if (state) render(state); };
$('pwNew').onclick = () => api('host/password', {}).then(() => toast('Yeni şifre üretildi')).catch((e) => toast(e.message));
$('pwEdit').onclick = () => {
  $('pwForm').classList.remove('hidden');
  $('pwInput').value = '';
  $('pwInput').focus();
};
$('pwCancel').onclick = () => $('pwForm').classList.add('hidden');
$('pwForm').onsubmit = async (e) => {
  e.preventDefault();
  try {
    await api('host/password', { password: $('pwInput').value });
    $('pwForm').classList.add('hidden');
    toast('Şifre değiştirildi');
  } catch (err) {
    toast(err.message);
  }
};
$('autoAccept').onchange = (e) => api('host/autoaccept', { value: e.target.checked }).catch((err) => toast(err.message));
$('kick').onclick = () => api('host/kick');
$('accept').onclick = () => state && state.pending && api('host/confirm', { id: state.pending.id, accept: true }).catch((e) => toast(e.message));
$('reject').onclick = () => state && state.pending && api('host/confirm', { id: state.pending.id, accept: false }).catch((e) => toast(e.message));
$('quit').onclick = async () => {
  await api('quit').catch(() => {});
  window.close();
};

// ---- uzak bilgisayar ----

function normalizeId(raw) {
  let id = raw.trim().replace(/\s+/g, '');
  const m = id.match(/^(?:https?:\/\/)?([a-z0-9-]+)\.trycloudflare\.com\/?$/i);
  if (m) id = m[1];
  return id.toLowerCase();
}

function loadRecent() {
  try { return JSON.parse(localStorage.getItem('recent') || '[]'); } catch { return []; }
}

function saveRecent(id) {
  const list = [id, ...loadRecent().filter((x) => x !== id)].slice(0, 5);
  try { localStorage.setItem('recent', JSON.stringify(list)); } catch {}
  renderRecent();
}

function renderRecent() {
  const box = $('recent');
  box.innerHTML = '';
  const list = loadRecent();
  if (!list.length) return;
  const h = document.createElement('div');
  h.className = 'lbl muted';
  h.style.cssText = 'font-size:12px;text-transform:uppercase;letter-spacing:.6px';
  h.textContent = 'Son bağlantılar';
  box.appendChild(h);
  for (const id of list) {
    const b = document.createElement('button');
    b.type = 'button';
    b.className = 'recent-item';
    b.innerHTML = '<span class="mono"></span><span class="muted">Seç</span>';
    b.firstChild.textContent = id;
    b.onclick = () => { $('remoteId').value = id; $('remotePw').focus(); };
    box.appendChild(b);
  }
}

$('connectForm').onsubmit = (e) => {
  e.preventDefault();
  $('connectErr').textContent = '';
  const id = normalizeId($('remoteId').value);
  const pw = $('remotePw').value;
  if (!id) return;
  if (state && id === state.id) {
    $('connectErr').textContent = 'Bu, bu bilgisayarın kendi ID\'si.';
    return;
  }
  pendingConnections.push({ id, pw });
  const w = window.open('view.html', '_blank', 'popup=yes,width=1280,height=800');
  if (!w) {
    pendingConnections.pop();
    $('connectErr').textContent = 'Pencere açılamadı.';
    return;
  }
  $('remotePw').value = '';
  saveRecent(id);
};

renderRecent();
connectEvents();
