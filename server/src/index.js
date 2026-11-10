// RemoteDesk buluşma sunucusu (Cloudflare Workers + Durable Object).
//
// Görevi: kalıcı cihaz ID'si vermek, kimin çevrimiçi olduğunu bilmek ve bağlanmak isteyen
// kişiye hedef cihazın o anki tünel adresini iletmek. Görüntü, klavye/fare ve ŞİFRE buradan
// geçmez: şifre doğrulaması hedef cihazda, tünel üzerinden yapılır.
//
//   POST /register            {secret}            -> {id}
//   GET  /host?id=..          (WebSocket, Authorization: Bearer <secret>)  cihazın kalıcı bağlantısı
//        cihaz -> sunucu: {t:"hello", avail, watch:[id]}  {t:"avail", on}  {t:"watch", ids:[id]}  {t:"url", req, url|err}
//        sunucu -> cihaz: {t:"open", req}  {t:"presence", s:{id: bool}}
//        Çevrimiçi bilgisi sorgulanmaz, değişince izleyenlere gönderilir (kota dostu).
//   GET  /status?ids=a,b,c                        -> {a:true, b:false}
//   POST /connect             {id}                -> {url}   (cihaza "tünel aç" der, adresi bekler)
//   POST /rename              {id, secret, newId} -> {id}    (newId boşsa rastgele yeni ID)
//   POST /signal              {id, sdp}           -> {sdp}   (WebRTC teklifini cihaza iletir, yanıtı döner)
//
// ID biçimi: 9 haneli sayı (rastgele) ya da kullanıcının seçtiği ad (harfle başlar,
// küçük harf/rakam/tire, 4-24 karakter; ör. "mustafa-pc").

const PING_STALE_MS = 100_000; // cihaz 30 sn'de bir ping atar; bu kadar sessizse çevrimdışı say
const CONNECT_WAIT_MS = 40_000;

export default {
  async fetch(req, env) {
    return env.HUB.get(env.HUB.idFromName("hub")).fetch(req);
  },
};

function json(obj, status = 200) {
  return new Response(JSON.stringify(obj), {
    status,
    headers: { "Content-Type": "application/json", "Cache-Control": "no-store" },
  });
}

async function sha256hex(s) {
  const d = await crypto.subtle.digest("SHA-256", new TextEncoder().encode(s));
  return [...new Uint8Array(d)].map((b) => b.toString(16).padStart(2, "0")).join("");
}

function sameString(a, b) {
  if (a.length !== b.length) return false;
  let r = 0;
  for (let i = 0; i < a.length; i++) r |= a.charCodeAt(i) ^ b.charCodeAt(i);
  return r === 0;
}

const validId = (s) => typeof s === "string" && (/^[1-9]\d{8}$/.test(s) || /^[a-z][a-z0-9-]{2,22}[a-z0-9]$/.test(s));
const RESERVED = new Set(["admin", "root", "remotedesk", "support", "destek", "help", "test", "system", "server"]);

export class Hub {
  constructor(state, env) {
    this.state = state;
    this.dev = env && env.DEV === "1"; // yerel test: tünelsiz (127.0.0.1) adreslere izin ver
    this.pending = new Map(); // istek no -> resolve
    this.hits = new Map(); // hız sınırı sayaçları (bellekte; uyuyunca sıfırlanır)
    this.seq = 0;
    state.setWebSocketAutoResponse(new WebSocketRequestResponsePair("ping", "pong"));
  }

  // limited: anahtar başına `max` istek / `windowMs`.
  limited(key, max, windowMs) {
    const now = Date.now();
    if (this.hits.size > 5000) {
      for (const [k, v] of this.hits) if (now - v.t > v.w) this.hits.delete(k);
    }
    const h = this.hits.get(key);
    if (!h || now - h.t > windowMs) {
      this.hits.set(key, { t: now, n: 1, w: windowMs });
      return false;
    }
    return ++h.n > max;
  }

  online(id, except) {
    const now = Date.now();
    for (const ws of this.state.getWebSockets(id)) {
      if (ws === except) continue; // kapanmakta olan bağlantı
      const att = ws.deserializeAttachment() || {};
      if (!att.avail) continue;
      const ping = this.state.getWebSocketAutoResponseTimestamp(ws);
      const seen = Math.max(att.at || 0, ping ? ping.getTime() : 0);
      if (now - seen < PING_STALE_MS) return ws;
    }
    return null;
  }

  async fetch(req) {
    const url = new URL(req.url);
    const ip = req.headers.get("CF-Connecting-IP") || "?";
    try {
      switch (url.pathname) {
        case "/register":
          return await this.register(req, ip);
        case "/host":
          return await this.host(req, url);
        case "/status":
          return this.status(url, ip);
        case "/connect":
          return await this.connect(req, ip);
        case "/rename":
          return await this.rename(req, ip);
        case "/signal":
          return await this.signal(req, ip);
        case "/":
          return new Response("RemoteDesk hub\n");
      }
      if (url.pathname.startsWith("/c/")) return this.browserPage(url);
      return json({ error: "bulunamadı" }, 404);
    } catch (e) {
      return json({ error: "sunucu hatası" }, 500);
    }
  }

  async register(req, ip) {
    if (req.method !== "POST") return json({ error: "yöntem" }, 405);
    if (this.limited("reg:" + ip, 10, 3600_000)) return json({ error: "Çok fazla kayıt denemesi." }, 429);
    const body = await req.json().catch(() => null);
    const secret = body && body.secret;
    if (typeof secret !== "string" || secret.length < 32 || secret.length > 128) {
      return json({ error: "geçersiz istek" }, 400);
    }
    const hash = await sha256hex(secret);
    for (let i = 0; i < 20; i++) {
      const buf = new Uint32Array(1);
      crypto.getRandomValues(buf);
      const id = String(100_000_000 + (buf[0] % 900_000_000));
      if ((await this.state.storage.get("dev:" + id)) === undefined) {
        await this.state.storage.put("dev:" + id, { hash, created: Date.now() });
        return json({ id });
      }
    }
    return json({ error: "ID üretilemedi" }, 503);
  }

  // Tarayıcıdan bağlanma: https://<hub>/c/<id> sayfası karşı cihazdan tünel açmasını ister,
  // tünel adresi erişilebilir olunca oraya yönlendirir. Şifre tünel sayfasında sorulur.
  browserPage(url) {
    const id = decodeURIComponent(url.pathname.slice(3)).replace(/\s+/g, "").toLowerCase();
    if (!validId(id)) return new Response("Geçersiz ID", { status: 404 });
    const html = `<!doctype html><html lang="tr"><meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1"><title>RemoteDesk · ${id}</title>
<style>body{margin:0;min-height:100vh;display:grid;place-items:center;background:#111113;color:#b0b4ba;font:15px system-ui,-apple-system,"Segoe UI",sans-serif;text-align:center;padding:24px}
.t{font-size:18px;font-weight:600;color:#edeef0;margin-bottom:8px}.e{color:#ff6369}
.s{width:22px;height:22px;border:2px solid #2e3135;border-top-color:#edeef0;border-radius:50%;margin:0 auto 16px;animation:r 1s linear infinite}
@keyframes r{to{transform:rotate(360deg)}}</style>
<body><div><div class="s" id="sp"></div><div class="t">RemoteDesk</div><div id="m">${id} cihazına ulaşılıyor…</div></div>
<script>
const id=${JSON.stringify(id)};const m=document.getElementById('m');
function fail(t){document.getElementById('sp').remove();m.className='e';m.textContent=t;}
(async()=>{
  let r;
  try{r=await fetch('/connect',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({id})});}
  catch{return fail('Sunucuya ulaşılamadı.');}
  const j=await r.json().catch(()=>({}));
  if(!r.ok||!j.url)return fail(j.error||'Bağlanılamadı.');
  m.textContent='Bağlantı hazırlanıyor…';
  // Yeni tünelin adresi birkaç saniye içinde erişilebilir olur.
  for(let i=0;i<30;i++){
    try{await fetch(j.url+'/style.css',{mode:'no-cors',cache:'no-store'});location.replace(j.url+'/');return;}
    catch{await new Promise(s=>setTimeout(s,1000));}
  }
  fail('Karşı cihazın bağlantısı zamanında hazır olmadı. Sayfayı yenileyip tekrar dene.');
})();
</script>`;
    return new Response(html, {
      headers: { "Content-Type": "text/html; charset=utf-8", "Cache-Control": "no-store", "X-Frame-Options": "DENY", "Referrer-Policy": "no-referrer" },
    });
  }

  // signal: WebRTC el sıkışması. Teklif cihaza iletilir, cihazın yanıtı döner. Bağlantı
  // başına tek istek; sonrasında görüntü ve ses iki bilgisayar arasında doğrudan akar.
  async signal(req, ip) {
    if (req.method !== "POST") return json({ error: "yöntem" }, 405);
    const body = await req.json().catch(() => null);
    const id = body && body.id;
    const sdp = body && body.sdp;
    if (!validId(id) || typeof sdp !== "string" || sdp.length > 16000) return json({ error: "Geçersiz istek." }, 400);
    if (this.limited("cn:" + ip, 20, 60_000) || this.limited("ct:" + id, 30, 60_000)) {
      return json({ error: "Çok fazla deneme. Biraz sonra tekrar dene." }, 429);
    }
    const ws = this.online(id);
    if (!ws) return json({ error: "Bu cihaz çevrimdışı ya da ID yanlış." }, 404);
    const reqNo = "s" + String(++this.seq) + "-" + Date.now();
    const answer = new Promise((resolve) => {
      this.pending.set(reqNo, resolve);
      setTimeout(() => resolve(null), 15_000);
    });
    try {
      ws.send(JSON.stringify({ t: "signal", req: reqNo, sdp }));
    } catch {
      this.pending.delete(reqNo);
      return json({ error: "Bu cihaz çevrimdışı." }, 404);
    }
    const m = await answer;
    this.pending.delete(reqNo);
    if (!m) return json({ error: "Karşı cihaz yanıt vermedi." }, 504);
    if (m.err || typeof m.sdp !== "string") return json({ error: m.err || "Doğrudan bağlantı kurulamadı." }, 502);
    return json({ sdp: m.sdp });
  }

  async rename(req, ip) {
    if (req.method !== "POST") return json({ error: "yöntem" }, 405);
    if (this.limited("rn:" + ip, 10, 3600_000)) return json({ error: "Çok fazla ID değişikliği. Bir saat sonra tekrar dene." }, 429);
    const body = await req.json().catch(() => null);
    if (!body || !validId(body.id) || typeof body.secret !== "string") return json({ error: "geçersiz istek" }, 400);
    const dev = await this.state.storage.get("dev:" + body.id);
    if (!dev || !sameString(dev.hash, await sha256hex(body.secret))) return json({ error: "yetkisiz" }, 401);

    let newId = typeof body.newId === "string" ? body.newId.trim().toLowerCase() : "";
    if (newId) {
      if (!validId(newId) || /^\d+$/.test(newId)) {
        return json({ error: "ID harfle başlamalı; küçük harf, rakam ve tire içerebilir (4-24 karakter)." }, 400);
      }
      if (RESERVED.has(newId)) return json({ error: "Bu ID kullanılamaz." }, 409);
      if (newId === body.id) return json({ id: newId });
      if ((await this.state.storage.get("dev:" + newId)) !== undefined) return json({ error: "Bu ID başkası tarafından alınmış." }, 409);
    } else {
      for (let i = 0; i < 20 && !newId; i++) {
        const buf = new Uint32Array(1);
        crypto.getRandomValues(buf);
        const id = String(100_000_000 + (buf[0] % 900_000_000));
        if ((await this.state.storage.get("dev:" + id)) === undefined) newId = id;
      }
      if (!newId) return json({ error: "ID üretilemedi" }, 503);
    }
    await this.state.storage.put("dev:" + newId, { hash: dev.hash, created: dev.created, renamed: Date.now() });
    await this.state.storage.delete("dev:" + body.id);
    // Eski ID ile açık bağlantı kapanır; cihaz yeni ID ile yeniden bağlanır.
    for (const ws of this.state.getWebSockets(body.id)) {
      try { ws.close(4001, "id degisti"); } catch {}
    }
    return json({ id: newId });
  }

  async host(req, url) {
    if (req.headers.get("Upgrade") !== "websocket") return json({ error: "websocket gerekli" }, 426);
    const id = url.searchParams.get("id");
    const auth = req.headers.get("Authorization") || "";
    const secret = auth.startsWith("Bearer ") ? auth.slice(7) : "";
    if (!validId(id) || !secret) return json({ error: "yetkisiz" }, 401);
    const dev = await this.state.storage.get("dev:" + id);
    if (!dev || !sameString(dev.hash, await sha256hex(secret))) return json({ error: "yetkisiz" }, 401);

    // Aynı cihazın eski bağlantısı kaldıysa kapat (yeniden bağlanma).
    for (const old of this.state.getWebSockets(id)) {
      try { old.close(4000, "yerine yenisi baglandi"); } catch {}
    }
    const pair = new WebSocketPair();
    this.state.acceptWebSocket(pair[1], [id]);
    pair[1].serializeAttachment({ id, at: Date.now(), avail: false, watch: [] });
    return new Response(null, { status: 101, webSocket: pair[0] });
  }

  // notify: id'nin çevrimiçi durumu değişti; onu izleyen cihazlara bildir.
  notify(id, on) {
    const msg = JSON.stringify({ t: "presence", s: { [id]: on } });
    for (const ws of this.state.getWebSockets()) {
      const att = ws.deserializeAttachment() || {};
      if (att.id !== id && Array.isArray(att.watch) && att.watch.includes(id)) {
        try { ws.send(msg); } catch {}
      }
    }
  }

  snapshot(ws, ids) {
    const s = {};
    for (const id of ids) s[id] = !!this.online(id);
    try { ws.send(JSON.stringify({ t: "presence", s })); } catch {}
  }

  status(url, ip) {
    if (this.limited("st:" + ip, 120, 60_000)) return json({ error: "Çok fazla istek." }, 429);
    const ids = (url.searchParams.get("ids") || "").split(",").filter(validId).slice(0, 50);
    const out = {};
    for (const id of ids) out[id] = !!this.online(id);
    return json(out);
  }

  async connect(req, ip) {
    if (req.method !== "POST") return json({ error: "yöntem" }, 405);
    const body = await req.json().catch(() => null);
    const id = body && body.id;
    if (!validId(id)) return json({ error: "Geçersiz ID." }, 400);
    // Hem isteyen IP'ye hem hedef cihaza sınır: tüneli sürekli uyandırıp yormak engellenir.
    if (this.limited("cn:" + ip, 20, 60_000) || this.limited("ct:" + id, 30, 60_000)) {
      return json({ error: "Çok fazla deneme. Biraz sonra tekrar dene." }, 429);
    }
    const ws = this.online(id);
    if (!ws) return json({ error: "Bu cihaz çevrimdışı ya da ID yanlış." }, 404);

    const reqNo = String(++this.seq) + "-" + Date.now();
    const answer = new Promise((resolve) => {
      this.pending.set(reqNo, resolve);
      setTimeout(() => resolve(null), CONNECT_WAIT_MS);
    });
    try {
      ws.send(JSON.stringify({ t: "open", req: reqNo }));
    } catch {
      this.pending.delete(reqNo);
      return json({ error: "Bu cihaz çevrimdışı." }, 404);
    }
    const m = await answer;
    this.pending.delete(reqNo);
    if (!m) return json({ error: "Karşı cihaz yanıt vermedi." }, 504);
    if (m.err || typeof m.url !== "string") return json({ error: m.err || "Karşı cihaz tünel açamadı." }, 502);
    const okUrl = /^https:\/\/[a-z0-9-]+\.trycloudflare\.com$/.test(m.url) ||
      (this.dev && /^http:\/\/127\.0\.0\.1:\d+$/.test(m.url));
    if (!okUrl) return json({ error: "Geçersiz adres." }, 502);
    return json({ url: m.url });
  }

  webSocketMessage(ws, msg) {
    if (typeof msg !== "string" || msg.length > 20000) return;
    let m;
    try { m = JSON.parse(msg); } catch { return; }
    if (!m || typeof m.t !== "string") return;
    const att = ws.deserializeAttachment() || {};
    const cleanIds = (a) => (Array.isArray(a) ? [...new Set(a.filter(validId))].slice(0, 100) : []);
    switch (m.t) {
      case "signal-answer":
      case "url": {
        const resolve = typeof m.req === "string" && this.pending.get(m.req);
        if (resolve) resolve(m);
        return;
      }
      case "hello":
      case "avail":
      case "watch": {
        const was = !!att.avail;
        if (m.t === "hello") att.avail = !!m.avail;
        if (m.t === "avail") att.avail = !!m.on;
        if (m.t === "hello" || m.t === "watch") att.watch = cleanIds(m.t === "hello" ? m.watch : m.ids);
        att.at = Date.now();
        ws.serializeAttachment(att);
        if (was !== att.avail) this.notify(att.id, att.avail);
        if (m.t !== "avail") this.snapshot(ws, att.watch);
        return;
      }
    }
  }

  webSocketClose(ws, code) {
    const att = ws.deserializeAttachment() || {};
    try { ws.close(code === 1005 || code === 1006 ? 1000 : code, ""); } catch {}
    // Aynı cihazın yeni bağlantısı yoksa çevrimdışı oldu.
    if (att.avail && att.id && !this.online(att.id, ws)) this.notify(att.id, false);
  }

  webSocketError() {}
}
