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

  renderPerms(s.perms || {});

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

// macOS izinleri: eksik olan her izin için ayarları açan düğme + yeniden başlatma.
let permsKey = '';
function renderPerms(p) {
  const key = JSON.stringify(p);
  if (key === permsKey) return; // her SSE güncellemesinde düğmeleri yeniden çizme
  permsKey = key;
  const wb = $('warnings');
  wb.innerHTML = '';
  if (!p.needed || (p.screen && p.input)) return;

  const box = document.createElement('div');
  box.className = 'warn';
  const items = [];
  if (!p.screen) items.push(['screen', 'Ekran Kaydı izni yok: karşı taraf pencereleri göremez, sadece masaüstü arka planını görür.']);
  if (!p.input) items.push(['input', 'Erişilebilirlik izni yok: karşı taraf fare ve klavyeyi kullanamaz.']);
  for (const [kind, text] of items) {
    const row = document.createElement('div');
    row.style.marginBottom = '6px';
    row.textContent = text + ' ';
    const b = document.createElement('button');
    b.className = 'small';
    b.textContent = kind === 'screen' ? 'Ekran Kaydı ayarını aç' : 'Erişilebilirlik ayarını aç';
    b.onclick = () => api('perm/open', { kind }).catch((e) => toast(e.message));
    row.appendChild(b);
    box.appendChild(row);
  }
  const hint = document.createElement('div');
  hint.textContent = 'Açılan listede RemoteDesk\'i aç (anahtarı yak). macOS izni uygulamayı yeniden başlatınca uygular.';
  box.appendChild(hint);
  if (p.canRestart) {
    const actions = document.createElement('div');
    actions.className = 'warn-actions';
    const r = document.createElement('button');
    r.className = 'primary small';
    r.textContent = 'İzinleri verdim, yeniden başlat';
    r.onclick = async () => {
      try {
        await api('restart');
        toast('Yeniden başlatılıyor…');
        setTimeout(() => window.close(), 400);
      } catch (e) {
        toast(e.message);
      }
    };
    actions.appendChild(r);
    box.appendChild(actions);
  }
  wb.appendChild(box);
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
