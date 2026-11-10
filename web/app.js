'use strict';

const $ = (id) => document.getElementById(id);
let state = null;
Object.defineProperty(window, 'state', { get: () => state });
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

const fmtId = (id) => id.replace(/(\d{3})(?=\d)/g, '$1 ');

// Başkasına verilecek ID: sunucu varsa kalıcı ID, yoksa (ya da sunucu yokken) tünel adresi.
function myShownId(s) {
  if (s.hub && s.deviceId && (s.hub === 'online' || !s.id)) return s.deviceId;
  return s.id || (s.hub ? s.deviceId : '') || '';
}

function render(s) {
  const modeChanged = !state || !!state.hub !== !!s.hub;
  state = s;
  if (modeChanged) setTimeout(renderPeers, 0);
  $('remoteId').placeholder = s.hub ? '123 456 789' : 'kelime-kelime-kelime';
  // Kalıcı ID (sunucu açıksa) ya da geçici tünel adresi.
  const hubOn = !!s.hub;
  const local = (s.tunnelMsg || '').startsWith('Yerel mod');
  const shown = myShownId(s);
  $('myId').textContent = shown
    ? (/^\d{9}$/.test(shown) ? fmtId(shown) : shown)
    : hubOn ? 'Hazırlanıyor…' : s.tunnel === 'off' ? 'Bağlantı almak için tüneli aç' : 'Hazırlanıyor…';
  $('myId').classList.toggle('off', !shown);
  const tb = $('tunBtn');
  // Sunucu çevrimiçiyken tünel kendiliğinden açılır; düğme yalnızca yedek yol olarak görünür.
  tb.classList.toggle('hidden', local || (hubOn && !!s.deviceId && (s.hub === 'online' || s.hub === 'offline')));
  tb.textContent = s.tunnel === 'off' ? 'Tüneli aç' : 'Tüneli kapat';
  tb.classList.toggle('primary', s.tunnel === 'off' && !hubOn);
  $('idCopy').classList.toggle('hidden', !shown);
  let hint = '';
  if (hubOn && s.hub === 'offline') {
    hint = 'Bağlantılara kapalı. Bağlanılabilmesi için aşağıdaki anahtarı aç.';
  } else if (hubOn && s.deviceId && s.hub === 'online') {
    hint = 'Bu ID kalıcıdır. Biri bağlanmak istediğinde bağlantı kendiliğinden açılır.';
  } else if (hubOn && s.deviceId) {
    hint = 'Sunucuya ulaşılamıyor; ID ile bağlantı alınamaz. İstersen tüneli açıp geçici adresi ver' +
      (s.id ? ': ' + s.id : '.');
  } else if (hubOn) {
    hint = 'Sunucuya ulaşılamıyor. Geçici adresle bağlantı almak için tüneli aç.';
  }
  $('idHint').textContent = hint;
  $('myPw').textContent = showPw ? s.password : '•'.repeat(s.password.length);
  $('pwShow').textContent = showPw ? 'Gizle' : 'Göster';

  const dot = $('tunDot');
  if (hubOn) {
    dot.className = 'dot ' + (s.hub === 'online' ? 'ok' : s.hub === 'error' ? 'err' : s.hub === 'offline' ? 'off' : 'busy');
    $('tunMsg').textContent = s.hubMsg + (s.tunnel === 'on' ? ' · bağlantı açık' : s.tunnel === 'starting' ? ' · bağlantı açılıyor' : '');
  } else {
    dot.className = 'dot ' + (s.tunnel === 'on' ? 'ok' : s.tunnel === 'error' ? 'err' : s.tunnel === 'off' ? 'off' : 'busy');
    $('tunMsg').textContent = s.tunnelMsg;
  }
  $('vidDot').className = 'dot ' + (s.video === 'on' ? 'ok' : s.video === 'error' ? 'err' : 'busy');
  $('vidMsg').textContent = s.videoMsg;
  renderSettings(s);

  renderPerms(s.perms || {});

  if (s.session) {
    $('sessionBox').classList.remove('hidden');
    $('sessName').textContent = s.session.name || '';
    $('sessRemote').textContent = '(' + s.session.remote + ')';
  } else {
    $('sessionBox').classList.add('hidden');
  }

  if (s.pending) {
    $('reqName').textContent = s.pending.name || '';
    $('reqRemote').textContent = '(' + s.pending.remote + ')';
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

$('tunBtn').onclick = () => api('host/tunnel', { value: state.tunnel === 'off' }).catch((e) => toast(e.message));
$('idCopy').onclick = () => copy(state && myShownId(state), 'ID');
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
$('kick').onclick = () => api('host/kick');

// ---- ayarlar ----

const SWITCHES = ['online', 'onlineAtStart', 'autoAccept', 'allowBrowser', 'allowControl', 'allowClipboard', 'concurrent'];
for (const k of SWITCHES) {
  $(k).onchange = (e) => api('settings', { [k]: e.target.checked }).catch((err) => { toast(err.message); if (state) render(state); });
}

function renderSettings(s) {
  for (const k of SWITCHES) {
    if (document.activeElement !== $(k)) $(k).checked = !!s[k];
  }
  $('onlineHint').textContent = s.online
    ? (s.hub ? 'ID ile bağlanılabilir.' : 'Tünel açık; geçici adresle bağlanılabilir.')
    : 'Kapalıyken kimse bu bilgisayara bağlanamaz.';

  const id = s.deviceId || '';
  $('setId').textContent = id ? (/^\d{9}$/.test(id) ? fmtId(id) : id) : '—';
  $('setIdHint').textContent = s.server
    ? (id ? 'Kalıcıdır; uygulama kapanıp açılınca değişmez.' : 'Sunucuya bağlanınca görünür.')
    : 'Sunucusuz modda kalıcı ID yok; tünel her açılışta yeni adres alır.';
  $('renameForm').classList.toggle('hidden', !s.server || !id);

  const link = s.server && id ? s.server + '/c/' + id : '';
  $('browserLink').textContent = link;
  $('browserLinkRow').classList.toggle('hidden', !s.allowBrowser || !link);
  $('browserHint').textContent = s.allowBrowser
    ? (link ? 'Aşağıdaki adresi açan kişi şifreyle bağlanabilir.' : 'Tünel adresini tarayıcıda açan kişi şifreyle bağlanabilir.')
    : 'Uygulaması olmayan biri tarayıcıdan şifreyle bağlanabilir.';

  $('fixedState').textContent = s.fixedPassword ? 'Sabit şifre ayarlı' : 'Ayarlı değil';
  $('fixedRemove').classList.toggle('hidden', !s.fixedPassword);
  $('fixedSave').textContent = s.fixedPassword ? 'Değiştir' : 'Kaydet';

  $('adminSection').classList.toggle('hidden', !s.elevated && !s.canElevate);
  $('adminState').textContent = s.elevated ? 'Yönetici olarak çalışıyor' : 'Normal kullanıcı olarak çalışıyor';
  $('elevate').classList.toggle('hidden', !s.canElevate);

  $('appVersion').textContent = s.version || '';
  const u = s.update;
  $('updateBar').classList.toggle('hidden', !u);
  if (u) {
    $('updText').textContent = u.status === 'available'
      ? `Yeni sürüm var: ${u.version}` + (u.msg ? ' · ' + u.msg : '')
      : `${u.version}: ${u.msg}`;
    $('updNotes').href = u.url || '#';
    $('updInstall').classList.toggle('hidden', u.status === 'downloading' || u.status === 'installing' || !!u.msg && u.status === 'available');
    $('updInstall').textContent = u.status === 'error' ? 'Tekrar dene' : 'Güncelle';
    $('updState').textContent = u.status === 'available' ? `Yeni sürüm var: ${u.version}` : u.msg;
  }
}

function openSettings() {
  $('settings').classList.remove('hidden');
  $('closeSettings').focus();
}
function closeSettings() { $('settings').classList.add('hidden'); }
$('openSettings').onclick = openSettings;
$('closeSettings').onclick = closeSettings;
$('settings').onclick = (e) => { if (e.target === $('settings')) closeSettings(); };
document.addEventListener('keydown', (e) => {
  if (e.key === 'Escape' && !$('settings').classList.contains('hidden')) closeSettings();
});

$('renameForm').onsubmit = async (e) => {
  e.preventDefault();
  const v = $('renameInput').value.trim();
  if (!v) return;
  try {
    await api('settings/rename', { id: v });
    $('renameInput').value = '';
    toast('ID değişti');
  } catch (err) { toast(err.message); }
};
$('renameRandom').onclick = async () => {
  try {
    await api('settings/rename', { id: '' });
    toast('Yeni ID alındı');
  } catch (err) { toast(err.message); }
};
$('browserCopy').onclick = () => copy($('browserLink').textContent, 'Adres');

$('fixedForm').onsubmit = async (e) => {
  e.preventDefault();
  const v = $('fixedInput').value;
  if (v.length < 8) { toast('Sabit şifre en az 8 karakter olmalı'); return; }
  try {
    await api('settings/fixedpw', { password: v });
    $('fixedInput').value = '';
    toast('Sabit şifre kaydedildi');
  } catch (err) { toast(err.message); }
};
$('fixedRemove').onclick = async () => {
  try {
    await api('settings/fixedpw', { password: '' });
    toast('Sabit şifre kaldırıldı');
  } catch (err) { toast(err.message); }
};

$('elevate').onclick = async () => {
  try {
    await api('elevate');
    toast('Yönetici olarak yeniden başlatılıyor…');
  } catch (err) { toast(err.message); }
};

$('updCheck').onclick = async () => {
  $('updCheck').disabled = true;
  try {
    const r = await fetch('/api/update/check', { method: 'POST' });
    if (!r.ok) throw new Error((await r.text()).trim());
    const j = await r.json();
    if (!j.update) { $('updState').textContent = 'Güncel: en son sürümü kullanıyorsun.'; toast('Güncel'); }
  } catch (err) { toast(err.message || 'Denetlenemedi'); }
  $('updCheck').disabled = false;
};
$('updInstall').onclick = () => api('update/install').catch((err) => toast(err.message));

// Ad: bir kez girilir, sonraki açılışlarda hatırlanır. Boşsa işletim sistemindeki ad.
(function initName() {
  let saved = '';
  try { saved = localStorage.getItem('name') || ''; } catch {}
  if (saved) {
    $('myName').value = saved;
  } else {
    fetch('/api/me').then((r) => r.json()).then((j) => { if (!$('myName').value) $('myName').value = j.name || ''; }).catch(() => {});
  }
  $('myName').onchange = () => { try { localStorage.setItem('name', $('myName').value.trim()); } catch {} };
})();
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

// Açık bağlantı pencereleri: id -> pencere. Aynı cihaza ikinci kez tıklanınca o pencereye geçilir.
const openWins = new Map();
function openWin(id) {
  const w = openWins.get(id);
  if (w && !w.closed) return w;
  openWins.delete(id);
  return null;
}

function startConnection(id, pw) {
  const label = /^\d{9}$/.test(id) ? fmtId(id) : id;
  pendingConnections.push({ id, pw, name: $('myName').value.trim(), label });
  const w = window.open('view.html', 'rd_' + id.replace(/[^a-z0-9]/gi, '_'), 'popup=yes,width=1280,height=800');
  if (!w) {
    pendingConnections.pop();
    $('connectErr').textContent = 'Pencere açılamadı.';
    return;
  }
  openWins.set(id, w);
  setTimeout(loadPeers, 3000);
}

$('connectForm').onsubmit = (e) => {
  e.preventDefault();
  $('connectErr').textContent = '';
  const id = normalizeId($('remoteId').value);
  const pw = $('remotePw').value;
  if (!id) return;
  if (state && (id === state.id || id === state.deviceId)) {
    $('connectErr').textContent = 'Bu, bu bilgisayarın kendi ID\'si.';
    return;
  }
  const w = openWin(id);
  if (w) { w.focus(); return; }
  startConnection(id, pw);
  $('remotePw').value = '';
};

// ---- kayıtlı cihazlar ----

let peers = [];
let editing = null; // adı düzenlenen cihazın ID'si

async function loadPeers() {
  try {
    const r = await fetch('/api/peers');
    if (!r.ok) return;
    peers = await r.json();
    if (!editing) renderPeers();
  } catch {}
}

function savePeer(body) {
  return api('peers', body).then(loadPeers).catch((e) => toast(e.message));
}

function iconBtn(text, title, onclick) {
  const b = document.createElement('button');
  b.type = 'button';
  b.className = 'icon';
  b.textContent = text;
  b.title = title;
  b.onclick = (e) => { e.stopPropagation(); onclick(); };
  return b;
}

function renderPeers() {
  const box = $('peers');
  box.innerHTML = '';
  // Sunucusuz modda kalıcı ID yok: kayıtlı cihaz listesi gösterilmez.
  if (!peers.length || (state && !state.hub)) return;
  const h = document.createElement('div');
  h.className = 'recent-head';
  h.textContent = 'Cihazlar';
  box.appendChild(h);

  for (const p of peers) {
    const row = document.createElement('div');
    row.className = 'peer';
    row.tabIndex = 0;
    const connectedWin = openWin(p.id);

    const dot = document.createElement('span');
    dot.className = 'dot ' + (p.online ? 'ok' : 'off');
    dot.title = p.online ? 'Çevrimiçi' : 'Çevrimdışı';
    row.appendChild(dot);

    const text = document.createElement('div');
    text.className = 'peer-text';
    if (editing === p.id) {
      const inp = document.createElement('input');
      inp.type = 'text';
      inp.maxLength = 40;
      inp.value = p.name || '';
      inp.placeholder = 'Cihaz adı';
      inp.onclick = (e) => e.stopPropagation();
      const done = (save) => {
        if (editing !== p.id) return;
        editing = null;
        if (save) savePeer({ id: p.id, name: inp.value }); else renderPeers();
      };
      inp.onkeydown = (e) => {
        e.stopPropagation();
        if (e.key === 'Enter') done(true);
        if (e.key === 'Escape') done(false);
      };
      inp.onblur = () => done(true);
      text.appendChild(inp);
      setTimeout(() => inp.focus(), 0);
    } else {
      const name = document.createElement('div');
      name.className = 'peer-name';
      name.textContent = p.name || fmtId(p.id);
      const sub = document.createElement('div');
      sub.className = 'peer-sub';
      sub.textContent = (p.name ? fmtId(p.id) + ' · ' : '') +
        (connectedWin ? 'bağlısın' : p.online ? 'çevrimiçi' : 'çevrimdışı');
      text.append(name, sub);
    }
    row.appendChild(text);

    const acts = document.createElement('div');
    acts.className = 'peer-acts';
    const star = iconBtn(p.fav ? '★' : '☆', p.fav ? 'Favorilerden çıkar' : 'Favorilere ekle',
      () => savePeer({ id: p.id, fav: !p.fav }));
    if (p.fav) star.classList.add('on');
    acts.append(
      star,
      iconBtn('✎', 'Ad ver', () => { editing = p.id; renderPeers(); }),
      iconBtn('✕', 'Listeden kaldır', () => savePeer({ id: p.id, delete: true })),
    );
    row.appendChild(acts);

    const pick = () => {
      const w = openWin(p.id);
      if (w) { w.focus(); return; }
      $('remoteId').value = fmtId(p.id);
      $('remotePw').value = '';
      $('remotePw').focus();
      if (!p.online) toast('Bu cihaz şu an çevrimdışı görünüyor');
    };
    row.onclick = pick;
    row.onkeydown = (e) => { if (e.key === 'Enter' && e.target === row) pick(); };
    box.appendChild(row);
  }
}

loadPeers();
setInterval(loadPeers, 5000);
connectEvents();

// ---- sesli görüşme (bağlanan kişiyle) ----

const hcall = new VoiceCall({
  send: (m) => fetch('/api/rtc', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(m) })
    .then((r) => { if (!r.ok) throw new Error(); })
    .catch(() => { hcall.cleanup(); hcall.set('error', 'Bağlı kimse yok.'); }),
  onState: renderHostCall,
});

function renderHostCall(state, info) {
  const btn = $('hCall');
  btn.classList.toggle('danger', state === 'calling' || state === 'connecting' || state === 'active');
  btn.textContent = state === 'calling' ? 'Aramayı iptal et'
    : state === 'connecting' || state === 'active' ? 'Görüşmeyi kapat'
    : 'Sesli görüşme';
  btn.classList.toggle('hidden', state === 'incoming');
  $('hMute').classList.toggle('hidden', state !== 'active');
  if (state !== 'active') $('hMute').textContent = 'Sessize al';
  const bar = $('hCallBar');
  if (state === 'incoming') {
    const who = (state && window.state && window.state.session && window.state.session.name) || 'Bağlanan kişi';
    $('hCallText').textContent = who + ' sesli görüşme başlatmak istiyor.';
    $('hAccept').classList.remove('hidden');
    $('hReject').textContent = 'Reddet';
    bar.classList.remove('hidden');
    try { new Notification('RemoteDesk', { body: $('hCallText').textContent }); } catch {}
  } else if (info && (state === 'error' || state === 'idle' || state === 'calling')) {
    $('hCallText').textContent = info;
    $('hAccept').classList.add('hidden');
    $('hReject').textContent = state === 'calling' ? 'İptal' : 'Tamam';
    bar.classList.remove('hidden');
    if (state !== 'calling') setTimeout(() => { if (hcall.state === state) bar.classList.add('hidden'); }, 6000);
  } else {
    bar.classList.add('hidden');
  }
}

$('hCall').onclick = () => {
  if (hcall.state === 'idle' || hcall.state === 'error') hcall.start();
  else hcall.hangup();
};
$('hMute').onclick = (e) => { e.target.textContent = hcall.toggleMute() ? 'Sesi aç' : 'Sessize al'; };
$('hAccept').onclick = () => hcall.accept();
$('hReject').onclick = () => {
  if (hcall.state === 'incoming') hcall.reject();
  else if (hcall.state === 'calling') hcall.hangup();
  $('hCallBar').classList.add('hidden');
};

(function rtcEvents() {
  const es = new EventSource('/api/rtc/events');
  es.onmessage = (e) => {
    let m;
    try { m = JSON.parse(e.data); } catch { return; }
    hcall.handle(m);
  };
})();
