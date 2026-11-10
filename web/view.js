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

const conn = takeConnection();
if (conn) connect(conn);
else showOverlay('Bağlantı bilgisi bulunamadı. Bu pencereyi ana pencereden açmalısın.', true);

function connect({ id, pw }) {
  document.title = id + ' · RemoteDesk';
  $('vTitle').textContent = id;
  showOverlay('Bağlanıyor…');

  ws = new WebSocket(wsURL(id));
  ws.binaryType = 'arraybuffer';
  ws.onopen = async () => {
    showOverlay('Kimlik doğrulanıyor…');
    h264Supported = await checkH264();
    $('codec').querySelector('option[value=h264]').disabled = !h264Supported;
    ws.send(JSON.stringify({ t: 'auth', pw, h264: h264Supported, v: +$('quality').value }));
  };
  ws.onmessage = onMessage;
  ws.onclose = () => {
    connected = false;
    clearInputs(false);
    if (failed) return;
    showOverlay(everConnected
      ? 'Bağlantı kapandı.'
      : 'Bağlanılamadı. ID doğru mu, karşı taraf çevrimiçi mi? (Yeni açılan tünellerin erişilebilir olması birkaç saniye sürebilir.)', true);
  };
}

function onMessage(ev) {
  if (typeof ev.data === 'string') {
    let m;
    try { m = JSON.parse(ev.data); } catch { return; }
    switch (m.t) {
      case 'wait':
        showOverlay('Karşı tarafın bağlantıyı onaylaması bekleniyor…');
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
        showOverlay(m.msg || 'Hata', true);
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
      case 'warn': {
        const box = $('vwarn');
        box.innerHTML = '';
        for (const t of m.msgs || []) {
          const d = document.createElement('div');
          d.textContent = '⚠ ' + t;
          box.appendChild(d);
        }
        box.classList.toggle('hidden', !(m.msgs && m.msgs.length));
        break;
      }
      case 'clip':
        if ($('clipSync').checked && typeof m.text === 'string') {
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
    const list = pendingRects;
    pendingRects = [];
    Promise.all(list).then((items) => {
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
    o.textContent = `Ekran ${i + 1} (${d.w}×${d.h})${d.primary ? ' · ana' : ''}`;
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
  if (!connected || !$('clipSync').checked) return;
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
try { new EventSource('/api/events'); } catch {}

// ---- istatistik ----

setInterval(() => {
  if (!connected) { $('vStat').textContent = ''; return; }
  const r = rtt == null ? '–' : Math.round(rtt) + ' ms';
  const codec = mode === 'h264' ? 'H.264' + (encoderName ? ' ' + encoderName.replace('h264_', '') : '') : 'JPEG';
  $('vStat').textContent = `${codec} · ${frames} fps · ${r} · ${Math.round(bytes / 1024)} KB/s`;
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
  if (dx || dy) send({ t: 'wh', dx, dy });
}, { passive: false });

// ---- klavye ----

function fromToolbar(e) {
  return e.target && e.target.closest && e.target.closest('#bar');
}

window.addEventListener('keydown', (e) => {
  if (!connected || fromToolbar(e) || !e.code || e.code === 'Unidentified') return;
  e.preventDefault();
  pressedKeys.add(e.code);
  send({ t: 'k', c: e.code, d: true });
});

window.addEventListener('keyup', (e) => {
  if (!connected || fromToolbar(e) || !e.code || e.code === 'Unidentified') return;
  e.preventDefault();
  pressedKeys.delete(e.code);
  send({ t: 'k', c: e.code, d: false });
});

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

$('scale').onclick = (e) => {
  const fit = canvas.classList.toggle('fit');
  $('stage').classList.toggle('native', !fit);
  e.target.textContent = fit ? 'Gerçek boyut' : 'Sığdır';
  e.target.blur();
  canvas.focus();
};

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
