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

  renderSessions(s.sessions || []);

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
  for (const el of document.querySelectorAll('#sessList [data-since]')) {
    const sec = Math.floor((Date.now() - +el.dataset.since) / 1000);
    const m = Math.floor(sec / 60), x = sec % 60;
    el.textContent = `${m}:${String(x).padStart(2, '0')}`;
  }
}, 250);

// ---- bağlı kişiler (toplantıda birden fazla) ----

let sessKey = '';
function renderSessions(list) {
  $('sessionBox').classList.toggle('hidden', list.length === 0);
  $('sessTitle').textContent = list.length === 1 ? 'Bağlı: 1 kişi' : `Bağlı: ${list.length} kişi`;
  $('kick').textContent = list.length > 1 ? 'Hepsini çıkar' : 'Bağlantıyı kes';
  const key = JSON.stringify(list) + '|' + conf.key();
  if (key === sessKey) return;
  sessKey = key;
  const ul = $('sessList');
  ul.innerHTML = '';
  for (const p of list) {
    const li = document.createElement('li');
    const who = document.createElement('span');
    who.className = 'who';
    const b = document.createElement('b');
    b.textContent = p.name || 'Uzak kullanıcı';
    const meta = document.createElement('span');
    meta.className = 'muted mono';
    meta.textContent = ' ' + p.remote + ' · ';
    const t = document.createElement('span');
    t.className = 'muted';
    t.dataset.since = p.since;
    who.append(b, meta, t);
    const cs = conf.stateOf(p.id);
    if (cs) {
      const c = document.createElement('span');
      c.className = 'call-state';
      c.textContent = cs === 'active' ? ' · sesli' : cs === 'incoming' ? ' · arıyor' : ' · bağlanıyor';
      who.append(c);
    }
    const ctl = document.createElement('label');
    ctl.className = 'ctl-toggle';
    ctl.title = 'Bu kişi fare ve klavyeyi kullanabilsin mi? Kapalıyken yalnızca izler (ve ekrana çizebilir).';
    const sw = document.createElement('input');
    sw.type = 'checkbox';
    sw.className = 'switch';
    sw.checked = !!p.control && !!(state && state.allowControl);
    sw.disabled = !(state && state.allowControl);
    sw.onchange = () => api('host/session', { id: p.id, control: sw.checked }).catch((e) => toast(e.message));
    ctl.append(document.createTextNode('Kontrol'), sw);
    const k = document.createElement('button');
    k.className = 'small';
    k.textContent = 'Çıkar';
    k.onclick = () => api('host/kick', { id: p.id });
    li.append(who, ctl, k);
    ul.appendChild(li);
  }
  conf.syncSessions(list);
}

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
$('kick').onclick = () => api('host/kick', {});
$('inkClear').onclick = () => api('host/inkclear').then(() => toast('Çizimler silindi')).catch((e) => toast(e.message));

// ---- ayarlar ----

const SWITCHES = ['online', 'onlineAtStart', 'allowBrowser', 'allowControl', 'allowClipboard', 'concurrent'];
for (const k of SWITCHES) {
  $(k).onchange = (e) => api('settings', { [k]: e.target.checked }).catch((err) => { toast(err.message); if (state) render(state); });
}

$('maxViewers').onchange = (e) => api('settings', { maxViewers: +e.target.value }).catch((err) => toast(err.message));

function renderSettings(s) {
  if (document.activeElement !== $('maxViewers')) $('maxViewers').value = String(s.maxViewers || 5);
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

async function startConnection(id, pw) {
  const label = /^\d{9}$/.test(id) ? fmtId(id) : id;
  const conn = { id, pw, name: $('myName').value.trim(), label };
  // Uygulamanın kendi penceresi (çerçevesiz WebView2 / WKWebView). Yoksa tarayıcı penceresi.
  try {
    const r = await fetch('/api/viewer/open', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(conn),
    });
    if (r.ok) {
      setTimeout(loadPeers, 1500);
      return;
    }
  } catch {}
  pendingConnections.push(conn);
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
  syncConnectLabel();
};

// ---- kayıtlı cihazlar, gruplar ve takımlar ----

let peers = [];
let teams = [];
let editing = null;   // adı düzenlenen cihazın ID'si
let grouping = null;  // grubu düzenlenen cihazın ID'si
let teamForm = '';    // '' | 'create' | 'join'
let openTeam = null;  // ayrıntısı (davet kodu, ayarlar) açık takım
const collapsed = new Set(JSON.parse(localStorageGet('rd_collapsed') || '[]'));

function localStorageGet(k) { try { return localStorage.getItem(k); } catch { return null; } }
function localStorageSet(k, v) { try { localStorage.setItem(k, v); } catch {} }

async function loadPeers() {
  try {
    const [r1, r2] = await Promise.all([fetch('/api/peers'), fetch('/api/teams')]);
    if (r1.ok) peers = await r1.json();
    if (r2.ok) teams = await r2.json();
    if (!editing && !grouping && !(document.activeElement && document.activeElement.closest('#peers form'))) renderPeers();
  } catch {}
}

function savePeer(body) {
  return api('peers', body).then(loadPeers).catch((e) => toast(e.message));
}

function teamOp(body) {
  return api('teams', body).then(() => setTimeout(loadPeers, 300)).catch((e) => { toast(e.message); throw e; });
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

function el(tag, cls, text) {
  const e = document.createElement(tag);
  if (cls) e.className = cls;
  if (text != null) e.textContent = text;
  return e;
}

// connectMany: listedeki çevrimiçi cihazların hepsine ayrı pencerelerde bağlanır
// (şifre yok: her biri bağlantı isteği olarak gider).
function connectMany(ids) {
  const list = ids.filter((id) => !openWin(id));
  if (!list.length) { toast('Bağlanılacak çevrimiçi cihaz yok'); return; }
  list.forEach((id, i) => setTimeout(() => startConnection(id, ''), i * 400));
  toast(`${list.length} cihaza bağlantı isteği gönderiliyor`);
}

function sectionHead(key, title, count, actions) {
  const h = el('div', 'group-head');
  const t = el('button', 'group-toggle');
  t.type = 'button';
  t.setAttribute('aria-expanded', String(!collapsed.has(key)));
  t.append(el('span', 'chev', collapsed.has(key) ? '▸' : '▾'), el('span', 'group-title', title));
  if (count) t.append(el('span', 'group-count', count));
  t.onclick = () => {
    if (collapsed.has(key)) collapsed.delete(key); else collapsed.add(key);
    localStorageSet('rd_collapsed', JSON.stringify([...collapsed]));
    renderPeers();
  };
  h.appendChild(t);
  const acts = el('div', 'group-acts');
  for (const a of actions || []) if (a) acts.appendChild(a);
  h.appendChild(acts);
  return h;
}

function smallBtn(text, title, onclick, cls) {
  const b = el('button', 'small ' + (cls || ''), text);
  b.type = 'button';
  b.title = title || '';
  b.onclick = (e) => { e.stopPropagation(); onclick(); };
  return b;
}

// deviceRow: tek cihaz satırı (kayıtlı cihaz ya da takım üyesi).
function deviceRow(d, opts) {
  const row = el('div', 'peer');
  row.tabIndex = 0;
  const connectedWin = d.open || openWin(d.id);
  const dot = el('span', 'dot ' + (d.online ? 'ok' : 'off'));
  dot.title = d.online ? 'Çevrimiçi' : 'Çevrimdışı';
  row.appendChild(dot);

  const text = el('div', 'peer-text');
  const inlineInput = (value, placeholder, save, listId) => {
    const inp = document.createElement('input');
    inp.type = 'text';
    inp.maxLength = 40;
    inp.value = value || '';
    inp.placeholder = placeholder;
    if (listId) inp.setAttribute('list', listId);
    inp.onclick = (e) => e.stopPropagation();
    let finished = false;
    const done = (ok) => {
      if (finished) return;
      finished = true;
      editing = grouping = null;
      if (ok) save(inp.value); else renderPeers();
    };
    inp.onkeydown = (e) => {
      e.stopPropagation();
      if (e.key === 'Enter') done(true);
      if (e.key === 'Escape') done(false);
    };
    inp.onblur = () => done(true);
    setTimeout(() => inp.focus(), 0);
    return inp;
  };
  if (!opts.team && editing === d.id) {
    text.appendChild(inlineInput(d.name, 'Cihaz adı', (v) => savePeer({ id: d.id, name: v })));
  } else if (!opts.team && grouping === d.id) {
    text.appendChild(inlineInput(d.group, 'Grup adı (boş: gruptan çıkar)', (v) => savePeer({ id: d.id, group: v }), 'groupNames'));
  } else {
    const title = opts.team ? (d.name || d.label || fmtId(d.id)) : (d.name || fmtId(d.id));
    const name = el('div', 'peer-name', title);
    if (d.self) name.appendChild(el('span', 'tag', 'bu bilgisayar'));
    if (d.owner) name.appendChild(el('span', 'tag', 'sahip'));
    const sub = el('div', 'peer-sub', (title !== fmtId(d.id) ? fmtId(d.id) + ' · ' : '') +
      (d.self ? 'sen' : connectedWin ? 'bağlısın' : d.online ? 'çevrimiçi' : 'çevrimdışı'));
    text.append(name, sub);
  }
  row.appendChild(text);

  const acts = el('div', 'peer-acts');
  if (opts.team) {
    if (opts.canKick && !d.self) {
      acts.append(iconBtn('✕', 'Takımdan çıkar', () => {
        if (confirmInline(row, `${d.label || fmtId(d.id)} takımdan çıkarılsın mı?`, () => teamOp({ op: 'kick', tid: opts.tid, id: d.id }))) return;
      }));
    }
  } else {
    const star = iconBtn(d.fav ? '★' : '☆', d.fav ? 'Favorilerden çıkar' : 'Favorilere ekle',
      () => savePeer({ id: d.id, fav: !d.fav }));
    if (d.fav) star.classList.add('on');
    acts.append(
      star,
      iconBtn('▤', 'Gruba ekle / grubunu değiştir', () => { grouping = d.id; renderPeers(); }),
      iconBtn('✎', 'Ad ver', () => { editing = d.id; renderPeers(); }),
      iconBtn('✕', 'Listeden kaldır', () => savePeer({ id: d.id, delete: true })),
    );
  }
  row.appendChild(acts);

  const pick = () => {
    if (d.self) return;
    if (d.open) { startConnection(d.id, ''); return; } // açık pencereyi öne getirir
    const w = openWin(d.id);
    if (w) { w.focus(); return; }
    $('remoteId').value = fmtId(d.id);
    $('remotePw').value = '';
    $('remotePw').focus();
    syncConnectLabel();
    if (!d.online) toast('Bu cihaz şu an çevrimdışı görünüyor');
  };
  row.onclick = pick;
  row.onkeydown = (e) => { if (e.key === 'Enter' && e.target === row) pick(); };
  return row;
}

// confirmInline: satırın altında küçük bir "emin misin" kutusu (tarayıcı confirm() yerine).
function confirmInline(after, text, onYes) {
  const old = document.querySelector('.confirm-inline');
  if (old) old.remove();
  const box = el('div', 'confirm-inline');
  box.append(el('span', '', text));
  const yes = smallBtn('Evet', '', () => { box.remove(); onYes(); }, 'danger');
  const no = smallBtn('Vazgeç', '', () => box.remove());
  box.append(yes, no);
  after.after(box);
  return true;
}

function teamForms(box) {
  if (!teamForm) return;
  const f = document.createElement('form');
  f.className = 'team-form';
  const inp = document.createElement('input');
  inp.type = 'text';
  inp.maxLength = teamForm === 'join' ? 12 : 40;
  inp.placeholder = teamForm === 'join' ? 'Davet kodu (ör. K7M2QX9PRT)' : 'Takım adı (ör. Ofis, Aile)';
  inp.spellcheck = false;
  if (teamForm === 'join') inp.style.textTransform = 'uppercase';
  const ok = el('button', 'primary small', teamForm === 'join' ? 'Katıl' : 'Oluştur');
  ok.type = 'submit';
  const cancel = smallBtn('Vazgeç', '', () => { teamForm = ''; renderPeers(); });
  f.append(inp, ok, cancel);
  f.onsubmit = async (e) => {
    e.preventDefault();
    const v = inp.value.trim();
    if (!v) return;
    ok.disabled = true;
    try {
      if (teamForm === 'join') await teamOp({ op: 'join', code: v });
      else await teamOp({ op: 'create', name: v });
      toast(teamForm === 'join' ? 'Takıma katıldın' : 'Takım oluşturuldu');
      teamForm = '';
      renderPeers();
    } catch {
      ok.disabled = false;
    }
  };
  box.appendChild(f);
  setTimeout(() => inp.focus(), 0);
}

function renderTeam(box, t) {
  const online = t.members.filter((m) => m.online && !m.self);
  const key = 'team:' + t.tid;
  const detail = openTeam === t.tid;
  box.appendChild(sectionHead(key, t.name, `${online.length + 1}/${t.members.length} çevrimiçi`, [
    online.length > 1 ? smallBtn('Hepsine bağlan', 'Takımdaki çevrimiçi cihazların hepsine ayrı pencerelerde bağlan', () => connectMany(online.map((m) => m.id))) : null,
    smallBtn(detail ? 'Kapat' : 'Davet et', 'Davet kodu ve takım ayarları', () => { openTeam = detail ? null : t.tid; renderPeers(); }),
  ]));
  if (collapsed.has(key)) return;
  if (detail) {
    const d = el('div', 'team-detail');
    const row = el('div', 'team-code');
    row.append(el('span', 'muted', 'Davet kodu'), el('b', 'mono', t.code));
    row.append(smallBtn('Kopyala', '', () => copy(t.code, 'Davet kodu')));
    d.appendChild(row);
    d.appendChild(el('p', 'hint', 'Bu kodu takıma katılacak bilgisayarda Cihazlar > Koda katıl alanına girin. Takımdakiler birbirinin cihazlarını ve çevrimiçi durumunu görür; bağlanmak için yine şifre ya da onay gerekir.'));
    const acts = el('div', 'team-acts');
    if (t.owner) {
      acts.append(smallBtn('Kodu yenile', 'Eski kod geçersiz olur', () => teamOp({ op: 'code', tid: t.tid }).then(() => toast('Yeni davet kodu oluşturuldu'), () => {})));
      const del = smallBtn('Takımı sil', '', () => confirmInline(del, `"${t.name}" herkes için silinsin mi?`, () => teamOp({ op: 'delete', tid: t.tid }).then(() => { openTeam = null; }, () => {})), 'danger');
      acts.append(del);
    }
    const leave = smallBtn('Takımdan ayrıl', '', () => confirmInline(leave, `"${t.name}" takımından ayrılınsın mı?`, () => teamOp({ op: 'leave', tid: t.tid }).then(() => { openTeam = null; }, () => {})), 'danger');
    acts.append(leave);
    d.appendChild(acts);
    box.appendChild(d);
  }
  for (const m of t.members) box.appendChild(deviceRow(m, { team: true, tid: t.tid, canKick: t.owner }));
}

function renderPeers() {
  const box = $('peers');
  box.innerHTML = '';
  // Sunucusuz modda kalıcı ID yok: kayıtlı cihaz listesi ve takımlar gösterilmez.
  if (state && !state.hub) return;

  // Takımlar
  const head = el('div', 'recent-head');
  head.append(el('span', '', 'Takımlar'));
  const ha = el('div', 'head-acts');
  ha.append(
    smallBtn('Takım oluştur', 'Yeni takım: davet koduyla başka bilgisayarlar katılır', () => { teamForm = teamForm === 'create' ? '' : 'create'; renderPeers(); }),
    smallBtn('Koda katıl', 'Davet koduyla bir takıma katıl', () => { teamForm = teamForm === 'join' ? '' : 'join'; renderPeers(); }),
  );
  head.appendChild(ha);
  box.appendChild(head);
  teamForms(box);
  if (!teams.length && !teamForm) {
    box.appendChild(el('p', 'hint empty-hint', 'Takım kurarak ofisteki ya da ailedeki bilgisayarları tek listede toplayabilir, hangisinin açık olduğunu görebilirsin.'));
  }
  for (const t of teams) renderTeam(box, t);

  // Kendi cihaz listem: gruplara göre
  if (!peers.length) return;
  const h2 = el('div', 'recent-head');
  h2.append(el('span', '', 'Cihazlarım'));
  box.appendChild(h2);
  const groups = new Map();
  for (const p of peers) {
    const g = p.group || '';
    if (!groups.has(g)) groups.set(g, []);
    groups.get(g).push(p);
  }
  const names = [...groups.keys()].filter(Boolean).sort((x, y) => x.localeCompare(y, 'tr'));
  // Grup adı önerileri
  const dl = document.createElement('datalist');
  dl.id = 'groupNames';
  for (const n of names) { const o = document.createElement('option'); o.value = n; dl.appendChild(o); }
  box.appendChild(dl);
  const order = names.length ? [...names, ''] : [''];
  for (const g of order) {
    const list = groups.get(g);
    if (!list) continue;
    if (names.length) {
      const key = 'grp:' + g;
      const on = list.filter((p) => p.online);
      box.appendChild(sectionHead(key, g || 'Grupsuz', `${on.length}/${list.length}`, [
        on.length > 1 ? smallBtn('Hepsine bağlan', 'Bu gruptaki çevrimiçi cihazların hepsine bağlan', () => connectMany(on.map((p) => p.id))) : null,
      ]));
      if (collapsed.has(key)) continue;
    }
    for (const p of list) box.appendChild(deviceRow(p, {}));
  }
}

loadPeers();
setInterval(loadPeers, 5000);
connectEvents();

// ---- sesli görüşme (toplantı) ----
// Bu bilgisayar her bağlı kişiyle ayrı bir ses bağlantısı kurar ve her birine
// "mikrofonum + diğer herkesin sesi" karışımını gönderir: herkes herkesi duyar.

class Conference {
  constructor() {
    this.on = false;
    this.calls = new Map(); // sid -> {call, dest, src}
    this.ctx = null;
    this.mic = null;
    this.micGain = null;
    this.muted = false;
    this.incoming = null; // görüşme kapalıyken gelen ilk arama (sid)
    this.known = new Set(); // görülmüş oturumlar: yalnızca sonradan katılan aranır
  }

  key() {
    return [...this.calls].map(([sid, c]) => sid + c.call.state).join(',') + (this.on ? '+' : '-');
  }
  stateOf(sid) {
    const c = this.calls.get(sid);
    if (!c || c.call.state === 'idle' || c.call.state === 'error') return '';
    return c.call.state === 'active' ? 'active' : c.call.state === 'incoming' ? 'incoming' : 'connecting';
  }

  async ensureAudio() {
    if (this.ctx) return;
    if (!navigator.mediaDevices || !navigator.mediaDevices.getUserMedia) throw new Error('Bu pencerede mikrofon kullanılamıyor.');
    let mic;
    try {
      mic = await navigator.mediaDevices.getUserMedia({ audio: { echoCancellation: true, noiseSuppression: true, autoGainControl: true }, video: false });
    } catch (e) {
      throw new Error(e && e.name === 'NotAllowedError' ? 'Mikrofon izni verilmedi.' : 'Mikrofon açılamadı.');
    }
    this.ctx = new AudioContext();
    this.mic = mic;
    this.micGain = this.ctx.createGain();
    this.ctx.createMediaStreamSource(mic).connect(this.micGain);
  }

  // streamFor: sid'ye gönderilecek karışım (mikrofon + o kişi hariç herkes).
  streamFor(sid) {
    const dest = this.ctx.createMediaStreamDestination();
    this.micGain.connect(dest);
    for (const [other, c] of this.calls) if (other !== sid && c.src) c.src.connect(dest);
    this.calls.get(sid).dest = dest;
    return dest.stream;
  }

  addRemote(sid, stream) {
    const me = this.calls.get(sid);
    if (!me || !this.ctx) return;
    if (me.src) try { me.src.disconnect(); } catch {}
    me.src = this.ctx.createMediaStreamSource(stream);
    for (const [other, c] of this.calls) if (other !== sid && c.dest) me.src.connect(c.dest);
  }

  entry(sid) {
    let e = this.calls.get(sid);
    if (e) return e;
    const call = new VoiceCall({
      send: (m) => fetch('/api/rtc', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ sid, m }) })
        .then((r) => { if (!r.ok) throw new Error(); })
        .catch(() => { call.cleanup(); this.drop(sid); }),
      onState: (st, info) => this.onCallState(sid, st, info),
      getStream: async () => { await this.ensureAudio(); return this.streamFor(sid); },
      onTrack: (stream) => this.addRemote(sid, stream),
    });
    e = { call, dest: null, src: null };
    this.calls.set(sid, e);
    return e;
  }

  drop(sid) {
    const e = this.calls.get(sid);
    if (!e) return;
    if (e.src) try { e.src.disconnect(); } catch {}
    this.calls.delete(sid);
    if (this.incoming === sid) this.incoming = null;
    this.render();
  }

  onCallState(sid, st, info) {
    if (st === 'idle' || st === 'error') {
      this.drop(sid);
      if (st === 'error' && info) toast(info);
    }
    this.render();
  }

  // start: herkesi arar (görüşmeyi bu bilgisayar başlatır).
  async start() {
    try {
      await this.ensureAudio();
    } catch (e) {
      toast(e.message);
      return;
    }
    this.on = true;
    for (const p of (state && state.sessions) || []) {
      const e = this.entry(p.id);
      if (e.call.state === 'idle') e.call.start();
      else if (e.call.state === 'incoming') e.call.accept();
    }
    this.render();
  }

  async acceptIncoming() {
    const sid = this.incoming;
    this.incoming = null;
    try {
      await this.ensureAudio();
    } catch (e) {
      toast(e.message);
      if (sid != null && this.calls.get(sid)) this.calls.get(sid).call.reject();
      return;
    }
    this.on = true;
    if (sid != null && this.calls.get(sid)) this.calls.get(sid).call.accept();
    this.render();
  }

  rejectIncoming() {
    const sid = this.incoming;
    this.incoming = null;
    if (sid != null && this.calls.get(sid)) this.calls.get(sid).call.reject();
    this.render();
  }

  hangup() {
    for (const [, e] of this.calls) e.call.hangup();
    this.calls.clear();
    this.on = false;
    this.incoming = null;
    if (this.mic) for (const t of this.mic.getTracks()) t.stop();
    if (this.ctx) this.ctx.close().catch(() => {});
    this.ctx = this.mic = this.micGain = null;
    this.muted = false;
    this.render();
  }

  toggleMute() {
    this.muted = !this.muted;
    if (this.micGain) this.micGain.gain.value = this.muted ? 0 : 1;
    return this.muted;
  }

  handle(sid, m) {
    if (!m || typeof m.type !== 'string') return;
    if (m.type === 'offer') {
      const e = this.entry(sid);
      e.call.handle(m).then(() => {
        if (this.on) e.call.accept(); // görüşme açık: katılmak isteyen doğrudan girer
        else if (this.incoming == null) this.incoming = sid;
        else e.call.reject();
        this.render();
      });
      return;
    }
    const e = this.calls.get(sid);
    if (e) e.call.handle(m);
  }

  // Yeni katılan kişi: görüşme açıksa o da aranır; ayrılan kişinin bağlantısı kapanır.
  syncSessions(list) {
    const ids = new Set(list.map((p) => p.id));
    for (const sid of [...this.calls.keys()]) if (!ids.has(sid)) { this.calls.get(sid).call.cleanup(); this.drop(sid); }
    for (const p of list) {
      if (this.known.has(p.id)) continue;
      this.known.add(p.id);
      if (this.on && !this.calls.has(p.id)) this.entry(p.id).call.start();
    }
  }

  render() {
    const btn = $('hCall');
    const active = this.on;
    btn.classList.toggle('danger', active);
    btn.textContent = active ? 'Görüşmeyi kapat' : 'Sesli görüşme';
    $('hMute').classList.toggle('hidden', !active);
    if (!active) $('hMute').textContent = 'Sessize al';
    const bar = $('hCallBar');
    if (this.incoming != null) {
      const p = ((state && state.sessions) || []).find((x) => x.id === this.incoming);
      $('hCallText').textContent = (p ? p.name : 'Bağlanan kişi') + ' sesli görüşme başlatmak istiyor.';
      $('hAccept').classList.remove('hidden');
      $('hReject').textContent = 'Reddet';
      bar.classList.remove('hidden');
      try { new Notification('RemoteDesk', { body: $('hCallText').textContent }); } catch {}
    } else {
      bar.classList.add('hidden');
    }
    if (state) renderSessions(state.sessions || []);
  }
}

const conf = new Conference();

$('hCall').onclick = () => { if (conf.on) conf.hangup(); else conf.start(); };
$('hMute').onclick = (e) => { e.target.textContent = conf.toggleMute() ? 'Sesi aç' : 'Sessize al'; };
$('hAccept').onclick = () => conf.acceptIncoming();
$('hReject').onclick = () => conf.rejectIncoming();

(function rtcEvents() {
  const es = new EventSource('/api/rtc/events');
  es.onmessage = (e) => {
    let m;
    try { m = JSON.parse(e.data); } catch { return; }
    if (m && typeof m.sid === 'number') conf.handle(m.sid, m.m);
  };
})();

// Şifre doluysa doğrudan bağlanılır, boşsa istek gider: düğme metni buna göre.
function syncConnectLabel() {
  $('connectBtn').textContent = $('remotePw').value ? 'Bağlan' : 'Bağlantı isteği gönder';
}
$('remotePw').addEventListener('input', syncConnectLabel);
syncConnectLabel();
