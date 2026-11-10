'use strict';

const $ = (id) => document.getElementById(id);
const canvas = $('screen');
const ctx = canvas.getContext('2d', { alpha: false });
const overlay = $('overlay');

let ws = null;
let connected = false;
let everConnected = false;
let failed = false;
let pendingRects = [];
let drawChain = Promise.resolve();
let hostStat = null;
let frames = 0, bytes = 0, rtt = null;
let lastPos = { x: 0, y: 0 };
const pressedKeys = new Set();
const pressedButtons = new Set();

function showOverlay(text, isErr) {
  overlay.textContent = text;
  overlay.classList.toggle('err', !!isErr);
  overlay.classList.remove('hidden');
}

function send(obj) {
  if (ws && ws.readyState === WebSocket.OPEN) ws.send(JSON.stringify(obj));
}

// ID -> WebSocket adresi. Test için "127.0.0.1:port" veya tam URL de kabul edilir.
function wsURL(id) {
  if (/^wss?:\/\//i.test(id)) return id;
  if (/^https?:\/\//i.test(id)) {
    const u = new URL(id);
    return (u.protocol === 'https:' ? 'wss://' : 'ws://') + u.host + '/ws';
  }
  if (id.includes(':')) return 'ws://' + id + '/ws';
  if (id.includes('.')) return 'wss://' + id + '/ws';
  return 'wss://' + id + '.trycloudflare.com/ws';
}

function takeConnection() {
  try {
    if (window.opener && window.opener.takePendingConnection) return window.opener.takePendingConnection();
  } catch {}
  return null;
}

// webMode: sayfa tünel adresinde tarayıcıyla açıldı (uygulama değil). Bu durumda şifre
// sorulur ve aynı adrese bağlanılır; yerel uygulama API'si (pano, olaylar) yoktur.
const webMode = location.pathname === '/';
if (webMode) document.body.classList.add('web');

(async () => {
  if (webMode) { showWebLogin(); return; }
  let conn = null;
  const key = new URLSearchParams(location.search).get('k');
  if (key) {
    // Uygulamanın açtığı pencere: bilgiler tek kullanımlık anahtarla bir kez alınır.
    try {
      const r = await fetch('/api/viewer/take?k=' + encodeURIComponent(key));
      if (r.ok) conn = await r.json();
    } catch {}
    history.replaceState(null, '', 'view.html');
  } else {
    conn = takeConnection();
  }
  if (conn) connect(conn);
  else showOverlay('Bağlantı bilgisi bulunamadı. Bu pencereyi ana pencereden açmalısın.', true);
})();

function showWebLogin(err) {
  overlay.classList.add('hidden');
  document.body.classList.add('login');
  $('webLogin').classList.remove('hidden');
  $('wlErr').textContent = err || '';
  try { $('wlName').value = $('wlName').value || localStorage.getItem('name') || ''; } catch {}
  setTimeout(() => $('wlPw').focus(), 0);
}

$('webLogin').onsubmit = (e) => {
  e.preventDefault();
  const name = $('wlName').value.trim();
  try { localStorage.setItem('name', name); } catch {}
  $('webLogin').classList.add('hidden');
  document.body.classList.remove('login');
  failed = false;
  everConnected = false;
  connect({ id: location.origin, pw: $('wlPw').value, name, label: location.host.replace('.trycloudflare.com', '') });
  $('wlPw').value = '';
};

// Kalıcı ID ise önce sunucudan hedef cihazın o anki adresi istenir (cihaz bu sırada
// tünelini açar). Yeni açılan tünelin erişilebilir olması birkaç saniye sürebildiği için
// ilk bağlantı birkaç kez denenir.
// Bağlantı sırası:
//   1. Doğrudan (WebRTC): el sıkışma buluşma sunucusu ya da tünel üzerinden tek mesaj;
//      sonra görüntü ve kontrol iki bilgisayar arasında doğrudan akar.
//   2. Kurulamazsa (ağ engeli) tünel: bugünkü WebSocket yolu.
// Tarayıcıdan erişimde (webMode) doğrudan tünel kullanılır.
let transportKind = '';

const isHubId = (id) => /^\d{9}$/.test(id) || /^[a-z][a-z0-9-]{2,22}[a-z0-9]$/.test(id);

async function connect({ id, pw, name, label }) {
  label = label || id;
  document.title = label + ' · RemoteDesk';
  if (window.rdSetTitle) window.rdSetTitle(document.title);
  $('vTitle').textContent = label;

  if (!webMode) {
    showOverlay('Bağlanıyor…');
    const direct = await tryDirect(id);
    if (direct) {
      transportKind = 'direct';
      attach(direct, pw, name, null);
      return;
    }
  }

  transportKind = 'tunnel';
  let target = id;
  let retries = 0;
  if (!webMode && isHubId(id)) {
    showOverlay('Karşı cihaza ulaşılıyor…');
    try {
      const r = await fetch('/api/resolve', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ id }),
      });
      if (!r.ok) throw new Error((await r.text()).trim() || 'Bağlanılamadı.');
      target = (await r.json()).url;
      retries = 15; // yeni tünelin DNS kaydının yayılması ~10-20 sn sürebilir
    } catch (e) {
      // Sunucuda yoksa ve tünel adresine benziyorsa (kelime-kelime-kelime) öyle dene.
      if (/^\d+$/.test(id) || !id.includes('-')) {
        failed = true;
        showOverlay(e.message || 'Bağlanılamadı.', true);
        return;
      }
    }
  }
  openSocket(target, pw, name, retries);
}

function openSocket(target, pw, name, retries) {
  showOverlay('Bağlanıyor…');
  const sock = new WebSocket(wsURL(target));
  sock.binaryType = 'arraybuffer';
  attach(sock, pw, name, retries > 0
    ? () => { showOverlay('Bağlantı hazırlanıyor…'); setTimeout(() => openSocket(target, pw, name, retries - 1), 1500); }
    : null);
}

// attach: WebSocket ya da DirectSocket'i oturuma bağlar (ikisi aynı arayüzü sunar).
function attach(sock, pw, name, onEarlyFail) {
  ws = sock;
  let opened = false;
  const start = async () => {
    opened = true;
    showOverlay('Kimlik doğrulanıyor…');
    h264Supported = await checkH264();
    $('codec').querySelector('option[value=h264]').disabled = !h264Supported;
    sock.send(JSON.stringify({ t: 'auth', pw, name, h264: h264Supported, v: +$('quality').value }));
  };
  sock.onmessage = onMessage;
  sock.onclose = () => {
    if (ws !== sock) return;
    if (typeof call !== 'undefined' && call.state !== 'idle') { call.cleanup(); call.set('idle'); }
    connected = false;
    clearInputs(false);
    if (failed) return;
    if (!opened && onEarlyFail) { onEarlyFail(); return; }
    showOverlay(everConnected
      ? 'Bağlantı kapandı.'
      : 'Bağlanılamadı. ID doğru mu, karşı taraf çevrimiçi mi?', true);
  };
  if (sock.readyState === WebSocket.OPEN) start();
  else sock.onopen = start;
}

// ---- doğrudan bağlantı (WebRTC veri kanalları) ----

const ICE = [{ urls: ['stun:stun.cloudflare.com:3478', 'stun:stun.l.google.com:19302'] }];

// DirectSocket, iki veri kanalını WebSocket gibi gösterir: "ctl" giden (şifre, klavye/fare),
// "media" gelen (kareler + kontrol mesajları). Büyük ikili mesajlar parçalı gelir
// (ilk bayt: 0 tam, 1 devamı var, 2 son parça).
class DirectSocket {
  constructor(pc, ctl, media) {
    this.pc = pc;
    this.ctl = ctl;
    this.media = media;
    this.readyState = WebSocket.OPEN;
    this.parts = [];
    this.onmessage = null;
    this.onclose = null;
    this.onopen = null;
    media.onmessage = (e) => {
      if (typeof e.data === 'string') { this.emit(e.data); return; }
      const u = new Uint8Array(e.data);
      if (u[0] === 0) { this.emit(e.data.slice(1)); return; }
      this.parts.push(u.slice(1));
      if (u[0] !== 2) return;
      let n = 0;
      for (const p of this.parts) n += p.length;
      const out = new Uint8Array(n);
      let o = 0;
      for (const p of this.parts) { out.set(p, o); o += p.length; }
      this.parts = [];
      this.emit(out.buffer);
    };
    const down = () => this.shut();
    ctl.onclose = down;
    media.onclose = down;
    pc.onconnectionstatechange = () => {
      if (pc.connectionState === 'failed' || pc.connectionState === 'closed') down();
    };
  }
  emit(data) { if (this.onmessage) this.onmessage({ data }); }
  send(s) {
    if (this.readyState !== WebSocket.OPEN) return;
    try { this.ctl.send(s); } catch {}
  }
  close() { this.shut(); }
  shut() {
    if (this.readyState === WebSocket.CLOSED) return;
    this.readyState = WebSocket.CLOSED;
    try { this.pc.close(); } catch {}
    if (this.onclose) this.onclose();
  }
}

function waitFor(cond, ms) {
  return new Promise((resolve) => {
    const t0 = Date.now();
    const tick = () => (cond() || Date.now() - t0 > ms ? resolve(cond()) : setTimeout(tick, 50));
    tick();
  });
}

async function tryDirect(id) {
  if (typeof RTCPeerConnection === 'undefined') return null;
  let pc = null;
  const attempt = (async () => {
    pc = new RTCPeerConnection({ iceServers: ICE });
    const ctl = pc.createDataChannel('ctl');
    const media = pc.createDataChannel('media');
    media.binaryType = 'arraybuffer';
    await pc.setLocalDescription(await pc.createOffer());
    // Adaylar teklife girsin: el sıkışma tek gidiş-dönüş.
    await waitFor(() => pc.iceGatheringState === 'complete', 2500);
    const r = await fetch('/api/rtc/connect', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ id, sdp: pc.localDescription.sdp }),
    });
    if (!r.ok) throw new Error((await r.text()).trim());
    const { sdp } = await r.json();
    await pc.setRemoteDescription({ type: 'answer', sdp });
    const ok = await waitFor(() => ctl.readyState === 'open' && media.readyState === 'open', 8000);
    if (!ok) throw new Error('doğrudan bağlantı kurulamadı');
    return new DirectSocket(pc, ctl, media);
  })();
  try {
    return await Promise.race([attempt, new Promise((_, rej) => setTimeout(() => rej(new Error('zaman aşımı')), 14000))]);
  } catch (e) {
    console.log('doğrudan bağlantı yok, tünel kullanılacak:', e && e.message);
    if (pc) try { pc.close(); } catch {}
    return null;
  }
}

let controlAllowed = true;
let clipAllowed = true;
let secureOn = false;
let hostWarnings = [];

function renderWarnings() {
  const box = $('vwarn');
  box.innerHTML = '';
  const msgs = [...hostWarnings];
  if (secureOn) {
    msgs.unshift('Karşı tarafta UAC onay ekranı ya da kilit ekranı açık. Windows bu ekranı korur: ' +
      'görüntü ve kontrol, ekran kapanınca geri gelir. Onayı karşı bilgisayarın başındaki kişi vermeli.');
  }
  for (const t of msgs) {
    const d = document.createElement('div');
    d.textContent = '⚠ ' + t;
    box.appendChild(d);
  }
  box.classList.toggle('hidden', !msgs.length);
}

function onMessage(ev) {
  if (typeof ev.data === 'string') {
    let m;
    try { m = JSON.parse(ev.data); } catch { return; }
    switch (m.t) {
      case 'wait':
        showOverlay('Bağlantı isteği gönderildi. Karşı tarafın kabul etmesi bekleniyor…');
        break;
      case 'init':
        canvas.width = m.w;
        canvas.height = m.h;
        connected = everConnected = true;
        overlay.classList.add('hidden');
        canvas.focus();
        break;
      case 'err':
        failed = true;
        if (webMode && !everConnected) showWebLogin(m.msg || 'Bağlanılamadı.');
        else showOverlay(m.msg || 'Hata', true);
        break;
      case 'rtc':
        call.handle(m.rtc);
        break;
      case 'perm':
        controlAllowed = !!m.control;
        $('permBadge').classList.toggle('hidden', !!m.control);
        $('permBadge').title = 'Karşı taraf fare ve klavye iznini kapattı; ekranı yalnızca izleyebilirsin.';
        clipAllowed = !!m.clipboard;
        break;
      case 'secure':
        secureOn = !!m.on;
        renderWarnings();
        break;
      case 'pong':
        rtt = performance.now() - m.ts;
        break;
      case 'displays':
        renderDisplays(m.list || [], m.cur);
        break;
      case 'mode':
        mode = m.v;
        encoderName = m.enc || '';
        closeDecoder();
        decodeErrors = 0;
        pendingRects = [];
        $('codec').value = mode;
        break;
      case 'notice':
        toast(m.msg);
        break;
      case 'hstat':
        hostStat = m;
        break;
      case 'busy': {
        // Karşı taraf bilgisayarı kullanıyor: girişin ya bekletiliyor ya da simülasyon modunda.
        const b = $('busy');
        if (m.mode === 'wait') {
          b.textContent = 'Karşı taraf kullanıyor · girişin bekletiliyor';
          b.className = 'badge';
          b.title = 'Karşı taraf fareyi/klavyeyi bıraktıktan 1 sn sonra kontrol sana döner.';
        } else if (m.mode === 'sim') {
          b.textContent = 'Aynı anda kullanım · hayalet imleç';
          b.className = 'badge sim';
          b.title = 'Karşı tarafın imleci oynamıyor; senin kendi (hayalet) imlecin var ve tıkladığın pencereye doğrudan komut gönderiliyor. ' +
            'Klasik uygulamalarda tıklama ve yazı çalışır; tarayıcılar, Electron uygulamaları, kısayollar ve sürükle-bırak çalışmaz.';
        } else {
          b.className = 'badge hidden';
        }
        break;
      }
      case 'warn':
        hostWarnings = m.msgs || [];
        renderWarnings();
        break;
      case 'clip':
        if (!webMode && $('clipSync').checked && typeof m.text === 'string') {
          lastClip = m.text;
          fetch('/api/clipboard', {
            method: 'POST',
            headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify({ text: m.text }),
          }).catch(() => {});
        }
        break;
    }
    return;
  }

  const buf = ev.data;
  bytes += buf.byteLength;
  const dv = new DataView(buf);
  const type = dv.getUint8(0);
  if (type === 1) {
    const x = dv.getUint16(1, true), y = dv.getUint16(3, true);
    const blob = new Blob([new Uint8Array(buf, 9)], { type: 'image/jpeg' });
    pendingRects.push(createImageBitmap(blob).then((bmp) => ({ bmp, x, y }), () => null));
  } else if (type === 3) {
    onVideo(buf);
  } else if (type === 2) {
    // Birden fazla kare aynı anda yolda olabilir: kareler geliş sırasıyla çizilmeli.
    const list = pendingRects;
    pendingRects = [];
    drawChain = drawChain.then(() => Promise.all(list)).then((items) => {
      for (const it of items) {
        if (!it) continue;
        ctx.drawImage(it.bmp, it.x, it.y);
        it.bmp.close();
      }
      frames++;
      send({ t: 'ack' });
    });
  }
}

// ---- H.264 (WebCodecs) ----

let mode = 'jpeg';
let encoderName = '';
let h264Supported = false;
let decoder = null;
let decoderCodec = null;
let waitKey = true;
let kfRequested = false;
let decodeErrors = 0;
let vts = 0;

async function checkH264() {
  if (typeof VideoDecoder !== 'function') return false;
  try {
    const r = await VideoDecoder.isConfigSupported({ codec: 'avc1.42E01F', optimizeForLatency: true });
    return !!r.supported;
  } catch {
    return false;
  }
}

function closeDecoder() {
  if (decoder && decoder.state !== 'closed') {
    try { decoder.close(); } catch {}
  }
  decoder = null;
  decoderCodec = null;
  waitKey = true;
}

// SPS'ten (NAL tipi 7) codec dizesini çıkar: avc1.PPCCLL (profil, kısıtlar, seviye).
function codecFromSPS(u8) {
  for (let i = 0; i + 6 < u8.length; i++) {
    if (u8[i] === 0 && u8[i + 1] === 0 && u8[i + 2] === 1 && (u8[i + 3] & 0x1f) === 7) {
      const h = (b) => b.toString(16).padStart(2, '0').toUpperCase();
      return 'avc1.' + h(u8[i + 4]) + h(u8[i + 5]) + h(u8[i + 6]);
    }
  }
  return null;
}

function requestKeyframe() {
  if (kfRequested) return;
  kfRequested = true;
  send({ t: 'kf' });
}

function makeDecoder(codec) {
  closeDecoder();
  decoder = new VideoDecoder({
    output: (frame) => {
      ctx.drawImage(frame, 0, 0, canvas.width, canvas.height);
      frame.close();
      frames++;
      send({ t: 'vack' });
    },
    error: (e) => {
      console.warn('H.264 çözücü hatası', e);
      decodeErrors++;
      closeDecoder();
      if (decodeErrors >= 3) {
        toast('H.264 çözülemiyor, JPEG\'e geçiliyor');
        send({ t: 'codec', codec: 'jpeg' });
      } else {
        requestKeyframe();
      }
    },
  });
  decoder.configure({ codec, optimizeForLatency: true });
  decoderCodec = codec;
}

function onVideo(buf) {
  const key = new DataView(buf).getUint8(1) === 1;
  const data = new Uint8Array(buf, 2);
  if (key) {
    kfRequested = false;
    const codec = codecFromSPS(data) || decoderCodec || 'avc1.640034';
    if (!decoder || codec !== decoderCodec) {
      try {
        makeDecoder(codec);
      } catch (e) {
        console.warn(e);
        send({ t: 'codec', codec: 'jpeg' });
        return;
      }
    }
  }
  if (!decoder || (waitKey && !key)) {
    // Çözülemeyecek kare: akış kontrolü tıkanmasın diye yine de onayla, anahtar kare iste.
    send({ t: 'vack' });
    requestKeyframe();
    return;
  }
  waitKey = false;
  try {
    decoder.decode(new EncodedVideoChunk({ type: key ? 'key' : 'delta', timestamp: (vts += 33333), data }));
  } catch (e) {
    console.warn(e);
    send({ t: 'vack' });
    closeDecoder();
    requestKeyframe();
  }
}

$('codec').onchange = (e) => {
  send({ t: 'codec', codec: e.target.value });
  e.target.blur();
  canvas.focus();
};

function toast(msg) {
  const t = $('toast');
  t.textContent = msg;
  t.classList.add('show');
  clearTimeout(toast._t);
  toast._t = setTimeout(() => t.classList.remove('show'), 3500);
}

// ---- monitörler ----

function renderDisplays(list, cur) {
  const sel = $('display');
  sel.innerHTML = '';
  list.forEach((d, i) => {
    const o = document.createElement('option');
    o.value = i;
    o.textContent = `Ekran ${i + 1} · ${d.w}×${d.h}${d.primary ? ' · ana' : ''}`;
    sel.appendChild(o);
  });
  sel.value = String(cur);
  sel.classList.toggle('hidden', list.length < 2);
}

$('display').onchange = (e) => {
  send({ t: 'disp', i: +e.target.value });
  e.target.blur();
  canvas.focus();
};

// ---- pano ----
// Bu bilgisayarın panosu yerel uygulama üzerinden okunur/yazılır (tarayıcı izni gerekmez).
// lastClip, aynı metnin iki taraf arasında sonsuza kadar gidip gelmesini önler.

let clipSeq = null;
let lastClip = null;
let clipInitialSent = false;

async function pollClipboard() {
  if (webMode || !connected || !clipAllowed || !$('clipSync').checked) return;
  try {
    const r = await fetch('/api/clipboard?seq=' + encodeURIComponent(clipSeq || ''));
    const j = await r.json();
    const changed = j.seq !== clipSeq;
    clipSeq = j.seq;
    if (typeof j.text !== 'string' || j.text === '' || j.text === lastClip) return;
    // İlk bağlantıda mevcut panoyu da gönder (bağlanmadan önce kopyalanan metin yapıştırılabilsin).
    if (changed || !clipInitialSent) {
      lastClip = j.text;
      send({ t: 'clip', text: j.text });
    }
  } catch {}
  clipInitialSent = true;
}
setInterval(pollClipboard, 600);

$('clipSync').onchange = (e) => {
  e.target.blur();
  canvas.focus();
};

// Uygulama, açık sayfa kalmayınca kapanır; bu bağlantı viewer'ı "açık" sayar.
if (!webMode) { try { new EventSource('/api/events'); } catch {} }

// ---- istatistik ----

setInterval(() => {
  if (!connected) { $('vStat').textContent = ''; return; }
  const r = rtt == null ? '–' : Math.round(rtt) + ' ms';
  const codec = mode === 'h264' ? 'H.264' + (encoderName ? ' ' + encoderName.replace('h264_', '') : '') : 'JPEG';
  let host = '';
  if (hostStat) {
    host = `
Karşı taraf: ${hostStat.fps.toFixed(1)} kare/sn üretiyor, ekran yakalama ortalama ${hostStat.cap.toFixed(1)} ms, ` +
      `${hostStat.w}×${hostStat.h}, kare gecikmesi ${hostStat.rtt} ms, yolda en fazla ${hostStat.win} kare` +
      (hostStat.br ? `, hedef bit hızı ${(hostStat.br / 1e6).toFixed(1)} Mbps (bağlantıya göre ayarlanır).` : '.');
  }
  const mbps = (bytes * 8 / 1e6).toFixed(1);
  const via = transportKind === 'direct' ? 'Doğrudan' : transportKind === 'tunnel' ? 'Tünel' : '';
  $('vStat').textContent = `${via ? via + ' · ' : ''}${frames} fps · ${r} · ${mbps} Mbps`;
  $('vStat').title = (via === 'Doğrudan' ? 'Doğrudan bağlantı (WebRTC): görüntü iki bilgisayar arasında doğrudan akıyor.\n' : via === 'Tünel' ? 'Tünel üzerinden (Cloudflare): doğrudan bağlantı kurulamadı ya da desteklenmiyor.\n' : '') +
    `${codec} · ${frames} fps · gecikme ${r} · ${mbps} Mbps${host}`;
  frames = 0;
  bytes = 0;
}, 1000);
setInterval(() => send({ t: 'ping', ts: performance.now() }), 2000);

// ---- fare ----

function pos(e) {
  const r = canvas.getBoundingClientRect();
  const x = Math.round((e.clientX - r.left) * canvas.width / r.width);
  const y = Math.round((e.clientY - r.top) * canvas.height / r.height);
  lastPos = {
    x: Math.max(0, Math.min(canvas.width - 1, x)),
    y: Math.max(0, Math.min(canvas.height - 1, y)),
  };
  return lastPos;
}

let queuedMove = null;
function flushMove() {
  if (queuedMove) send({ t: 'mm', ...queuedMove });
  queuedMove = null;
}

canvas.addEventListener('mousemove', (e) => {
  if (!connected) return;
  if (!queuedMove) requestAnimationFrame(flushMove);
  queuedMove = pos(e);
});

canvas.addEventListener('mousedown', (e) => {
  if (!connected) return;
  e.preventDefault();
  canvas.focus();
  flushMove();
  pressedButtons.add(e.button);
  send({ t: 'mb', b: e.button, d: true, ...pos(e) });
});

window.addEventListener('mouseup', (e) => {
  if (!pressedButtons.has(e.button)) return;
  e.preventDefault();
  pressedButtons.delete(e.button);
  flushMove();
  send({ t: 'mb', b: e.button, d: false, ...pos(e) });
});

canvas.addEventListener('contextmenu', (e) => e.preventDefault());
canvas.addEventListener('auxclick', (e) => e.preventDefault());

canvas.addEventListener('wheel', (e) => {
  e.preventDefault();
  if (!connected) return;
  const k = e.deltaMode === 1 ? 40 : e.deltaMode === 2 ? 120 : 1.2;
  const dx = Math.round(e.deltaX * k), dy = Math.round(e.deltaY * k);
  if (dx || dy) send({ t: 'wh', dx, dy, ...pos(e) });
}, { passive: false });

// ---- klavye ----

function fromToolbar(e) {
  return e.target && e.target.closest && e.target.closest('#bar');
}

window.addEventListener('keydown', (e) => {
  if (!connected || fromToolbar(e) || !e.code || e.code === 'Unidentified') return;
  e.preventDefault();
  pressedKeys.add(e.code);
  send({ t: 'k', c: e.code, d: true, ch: printable(e) });
});

window.addEventListener('keyup', (e) => {
  if (!connected || fromToolbar(e) || !e.code || e.code === 'Unidentified') return;
  e.preventDefault();
  pressedKeys.delete(e.code);
  send({ t: 'k', c: e.code, d: false });
});

// Tuşun ürettiği karakter (simülasyon modunda hedef pencereye doğrudan yazılır).
// Kısayollar (Ctrl/Cmd ile) karakter sayılmaz; AltGr ile yazılan karakterler sayılır.
function printable(e) {
  if ([...e.key].length !== 1) return '';
  const altGr = e.getModifierState && e.getModifierState('AltGraph');
  if ((e.ctrlKey || e.metaKey) && !altGr) return '';
  return e.key;
}

// Pencere odağı kaybolunca (ör. Alt+Tab) basılı tuşları bırak.
function clearInputs(notify) {
  if (notify) {
    for (const c of pressedKeys) send({ t: 'k', c, d: false });
    for (const b of pressedButtons) send({ t: 'mb', b, d: false, ...lastPos });
  }
  pressedKeys.clear();
  pressedButtons.clear();
}
window.addEventListener('blur', () => clearInputs(true));

// ---- araç çubuğu ----

$('quality').onchange = (e) => {
  send({ t: 'q', v: +e.target.value });
  e.target.blur();
  canvas.focus();
};

$('scale').onchange = (e) => {
  const fit = !e.target.checked;
  canvas.classList.toggle('fit', fit);
  $('stage').classList.toggle('native', !fit);
  e.target.blur();
  canvas.focus();
};

// Ayarlar menüsü: dışarı tıklayınca veya Esc ile kapanır.
document.addEventListener('pointerdown', (e) => {
  const d = $('vset');
  if (d.open && !d.contains(e.target)) d.open = false;
});
document.addEventListener('keydown', (e) => {
  if (e.key === 'Escape' && $('vset').open && document.activeElement !== canvas) $('vset').open = false;
});

$('fs').onclick = async (e) => {
  e.target.blur();
  if (document.fullscreenElement) {
    await document.exitFullscreen();
    return;
  }
  try {
    await document.documentElement.requestFullscreen();
    // Tam ekranda Win, Alt+Tab, Esc gibi tuşlar da uzak bilgisayara gider.
    if (navigator.keyboard && navigator.keyboard.lock) await navigator.keyboard.lock();
  } catch {}
  canvas.focus();
};

document.addEventListener('fullscreenchange', () => {
  const on = !!document.fullscreenElement;
  document.body.classList.toggle('fs', on);
  $('fs').textContent = on ? 'Tam ekrandan çık' : 'Tam ekran';
  if (!on && navigator.keyboard && navigator.keyboard.unlock) navigator.keyboard.unlock();
});

$('disc').onclick = () => {
  failed = true;
  if (ws) ws.close();
  window.close();
};

// ---- sesli görüşme ----

const call = new VoiceCall({
  send: (m) => send({ t: 'rtc', rtc: m }),
  onState: renderCall,
});

function renderCall(state, info) {
  const btn = $('callBtn');
  btn.classList.toggle('danger', state !== 'idle' && state !== 'error' && state !== 'incoming');
  btn.textContent = state === 'calling' ? 'Aramayı iptal et'
    : state === 'connecting' || state === 'active' ? 'Görüşmeyi kapat'
    : 'Sesli görüşme';
  btn.classList.toggle('hidden', state === 'incoming');
  $('muteBtn').classList.toggle('hidden', state !== 'active');
  if (state !== 'active') $('muteBtn').textContent = 'Sessize al';
  const bar = $('callBar');
  if (state === 'incoming') {
    $('callText').textContent = 'Karşı taraf sesli görüşme başlatmak istiyor.';
    $('callAccept').classList.remove('hidden');
    $('callReject').textContent = 'Reddet';
    bar.classList.remove('hidden');
  } else if (info && (state === 'error' || state === 'idle' || state === 'calling' || state === 'connecting')) {
    $('callText').textContent = info;
    $('callAccept').classList.add('hidden');
    $('callReject').textContent = state === 'calling' || state === 'connecting' ? 'İptal' : 'Tamam';
    bar.classList.remove('hidden');
    if (state === 'error' || state === 'idle') setTimeout(() => { if (call.state === state) bar.classList.add('hidden'); }, 6000);
  } else {
    bar.classList.add('hidden');
  }
}

$('callBtn').onclick = (e) => {
  e.target.blur();
  if (!connected) return;
  if (call.state === 'idle' || call.state === 'error') call.start();
  else call.hangup();
  canvas.focus();
};
$('muteBtn').onclick = (e) => {
  e.target.textContent = call.toggleMute() ? 'Sesi aç' : 'Sessize al';
  e.target.blur();
  canvas.focus();
};
$('callAccept').onclick = () => { call.accept(); canvas.focus(); };
$('callReject').onclick = () => {
  if (call.state === 'incoming') call.reject();
  else if (call.state === 'calling' || call.state === 'connecting') call.hangup();
  $('callBar').classList.add('hidden');
  canvas.focus();
};
window.addEventListener('beforeunload', () => call.hangup());
