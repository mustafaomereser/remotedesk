// RemoteDesk buluşma sunucusu (Cloudflare Workers + Durable Object).
//
// Görevi: kalıcı cihaz ID'si vermek, kimin çevrimiçi olduğunu bilmek ve bağlanmak isteyen
// kişiye hedef cihazın o anki tünel adresini iletmek. Görüntü, klavye/fare ve ŞİFRE buradan
// geçmez: şifre doğrulaması hedef cihazda, tünel üzerinden yapılır.
//
//   POST /register            {secret}            -> {id}
//   GET  /host?id=..          (WebSocket, Authorization: Bearer <secret>)  cihazın kalıcı bağlantısı
//   GET  /status?ids=a,b,c                        -> {a:true, b:false}
//   POST /connect             {id}                -> {url}   (cihaza "tünel aç" der, adresi bekler)

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

const validId = (s) => typeof s === "string" && /^[1-9]\d{8}$/.test(s);

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

  online(id) {
    const now = Date.now();
    for (const ws of this.state.getWebSockets(id)) {
      const att = ws.deserializeAttachment() || {};
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
        case "/":
          return new Response("RemoteDesk hub\n");
      }
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
    pair[1].serializeAttachment({ id, at: Date.now() });
    return new Response(null, { status: 101, webSocket: pair[0] });
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
    if (typeof msg !== "string" || msg.length > 2048) return;
    let m;
    try { m = JSON.parse(msg); } catch { return; }
    if (m && m.t === "url" && typeof m.req === "string") {
      const resolve = this.pending.get(m.req);
      if (resolve) resolve(m);
    }
  }

  webSocketClose(ws, code) {
    try { ws.close(code === 1005 || code === 1006 ? 1000 : code, ""); } catch {}
  }

  webSocketError() {}
}
