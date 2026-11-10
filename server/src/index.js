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
// Takımlar (cihaz bağlantısı üzerinden, kimliği doğrulanmış cihazlar):
//        cihaz -> sunucu: {t:"team", op:"create"|"join"|"leave"|"kick"|"delete"|"code", req, name, code, tid, id}
//        sunucu -> cihaz: {t:"team-res", req, err?}  {t:"teams", list:[{tid, name, code, owner, members:[{id, label}]}]}
//        Takım üyeleri birbirinin cihazlarını listede görür; davet kodunu bilen katılır.
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

const TEAM_MAX_PER_DEVICE = 10;
const TEAM_MAX_MEMBERS = 50;
const CODE_CHARS = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789"; // karışan harfler (0/O, 1/I) yok

function randomCode(n) {
  const buf = new Uint8Array(n);
  crypto.getRandomValues(buf);
  let s = "";
  for (const b of buf) s += CODE_CHARS[b % CODE_CHARS.length];
  return s;
}

const cleanText = (s, max) =>
  typeof s === "string" ? s.replace(/[\u0000-\u001f\u007f]/g, "").trim().slice(0, max) : "";

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
    await this.renameInTeams(body.id, newId);
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

  // ---- takımlar ----

  async teamsOf(id) {
    return (await this.state.storage.get("devteams:" + id)) || [];
  }

  async teamList(id) {
    const out = [];
    for (const tid of await this.teamsOf(id)) {
      const t = await this.state.storage.get("team:" + tid);
      if (!t || !t.members[id]) continue;
      out.push({
        tid, name: t.name, code: t.code, owner: t.owner === id,
        members: Object.entries(t.members).map(([mid, m]) => ({ id: mid, label: m.label || "", owner: t.owner === mid })),
      });
    }
    return out;
  }

  async pushTeams(id) {
    const list = await this.teamList(id);
    for (const ws of this.state.getWebSockets(id)) {
      try { ws.send(JSON.stringify({ t: "teams", list })); } catch {}
    }
  }

  // pushTeamMembers: takımın tüm çevrimiçi üyelerine güncel listeyi gönderir.
  async pushTeamMembers(ids) {
    for (const id of new Set(ids)) await this.pushTeams(id);
  }

  async renameInTeams(oldId, newId) {
    const tids = await this.teamsOf(oldId);
    if (!tids.length) return;
    const affected = [];
    for (const tid of tids) {
      const t = await this.state.storage.get("team:" + tid);
      if (!t || !t.members[oldId]) continue;
      t.members[newId] = t.members[oldId];
      delete t.members[oldId];
      if (t.owner === oldId) t.owner = newId;
      await this.state.storage.put("team:" + tid, t);
      affected.push(...Object.keys(t.members));
    }
    await this.state.storage.put("devteams:" + newId, tids);
    await this.state.storage.delete("devteams:" + oldId);
    await this.pushTeamMembers(affected.filter((x) => x !== newId));
  }

  async updateLabel(id, label) {
    label = cleanText(label, 40);
    if (!label) return;
    for (const tid of await this.teamsOf(id)) {
      const t = await this.state.storage.get("team:" + tid);
      if (!t || !t.members[id] || t.members[id].label === label) continue;
      t.members[id].label = label;
      await this.state.storage.put("team:" + tid, t);
      await this.pushTeamMembers(Object.keys(t.members).filter((x) => x !== id));
    }
  }

  async removeMember(t, tid, id) {
    delete t.members[id];
    const tids = (await this.teamsOf(id)).filter((x) => x !== tid);
    if (tids.length) await this.state.storage.put("devteams:" + id, tids);
    else await this.state.storage.delete("devteams:" + id);
  }

  async deleteTeam(t, tid) {
    for (const mid of Object.keys(t.members)) await this.removeMember(t, tid, mid);
    await this.state.storage.delete("code:" + t.code);
    await this.state.storage.delete("team:" + tid);
  }

  async teamOp(ws, att, m) {
    const id = att.id;
    const reply = (err) => { try { ws.send(JSON.stringify({ t: "team-res", req: m.req, err })); } catch {} };
    if (this.limited("tm:" + id, 30, 3600_000)) return reply("Çok fazla takım işlemi. Biraz sonra tekrar dene.");
    const st = this.state.storage;
    const mine = await this.teamsOf(id);
    switch (m.op) {
      case "create": {
        const name = cleanText(m.name, 40);
        if (!name) return reply("Takıma bir ad ver.");
        if (mine.length >= TEAM_MAX_PER_DEVICE) return reply("En fazla " + TEAM_MAX_PER_DEVICE + " takımda olabilirsin.");
        const tid = randomCode(12).toLowerCase();
        let code = randomCode(10);
        while ((await st.get("code:" + code)) !== undefined) code = randomCode(10);
        await st.put("team:" + tid, { name, code, owner: id, created: Date.now(), members: { [id]: { label: cleanText(att.label, 40), joined: Date.now() } } });
        await st.put("code:" + code, tid);
        await st.put("devteams:" + id, [...mine, tid]);
        reply();
        return this.pushTeams(id);
      }
      case "join": {
        const code = typeof m.code === "string" ? m.code.toUpperCase().replace(/[^A-Z0-9]/g, "") : "";
        if (this.limited("tj:" + id, 20, 3600_000)) return reply("Çok fazla deneme. Biraz sonra tekrar dene.");
        const tid = code.length === 10 ? await st.get("code:" + code) : undefined;
        const t = tid && (await st.get("team:" + tid));
        if (!t) return reply("Davet kodu geçersiz.");
        if (t.members[id]) return reply("Zaten bu takımdasın.");
        if (mine.length >= TEAM_MAX_PER_DEVICE) return reply("En fazla " + TEAM_MAX_PER_DEVICE + " takımda olabilirsin.");
        if (Object.keys(t.members).length >= TEAM_MAX_MEMBERS) return reply("Takım dolu.");
        t.members[id] = { label: cleanText(att.label, 40), joined: Date.now() };
        await st.put("team:" + tid, t);
        await st.put("devteams:" + id, [...mine, tid]);
        reply();
        return this.pushTeamMembers(Object.keys(t.members));
      }
      case "leave":
      case "kick":
      case "delete":
      case "code": {
        const tid = typeof m.tid === "string" ? m.tid : "";
        const t = mine.includes(tid) && (await st.get("team:" + tid));
        if (!t || !t.members[id]) return reply("Takım bulunamadı.");
        const members = Object.keys(t.members);
        if (m.op === "leave") {
          if (t.owner === id) {
            // Sahip ayrılırsa takım en eski üyeye geçer; tek kişiyse silinir.
            const rest = Object.entries(t.members).filter(([mid]) => mid !== id).sort((a, b) => a[1].joined - b[1].joined);
            if (!rest.length) { await this.deleteTeam(t, tid); reply(); return this.pushTeams(id); }
            t.owner = rest[0][0];
          }
          await this.removeMember(t, tid, id);
          await st.put("team:" + tid, t);
        } else {
          if (t.owner !== id) return reply("Bunu yalnızca takımın sahibi yapabilir.");
          if (m.op === "kick") {
            if (!validId(m.id) || m.id === id || !t.members[m.id]) return reply("Üye bulunamadı.");
            await this.removeMember(t, tid, m.id);
            await st.put("team:" + tid, t);
          } else if (m.op === "delete") {
            await this.deleteTeam(t, tid);
          } else {
            await st.delete("code:" + t.code);
            let code = randomCode(10);
            while ((await st.get("code:" + code)) !== undefined) code = randomCode(10);
            t.code = code;
            await st.put("code:" + code, tid);
            await st.put("team:" + tid, t);
          }
        }
        reply();
        return this.pushTeamMembers(members);
      }
    }
    return reply("Bilinmeyen işlem.");
  }

  async webSocketMessage(ws, msg) {
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
      case "team":
        if (typeof m.req === "string" && m.req.length < 40) await this.teamOp(ws, att, m);
        return;
      case "hello":
      case "avail":
      case "watch": {
        const was = !!att.avail;
        if (m.t === "hello") {
          att.avail = !!m.avail;
          att.label = cleanText(m.label, 40);
        }
        if (m.t === "avail") att.avail = !!m.on;
        if (m.t === "hello" || m.t === "watch") att.watch = cleanIds(m.t === "hello" ? m.watch : m.ids);
        att.at = Date.now();
        ws.serializeAttachment(att);
        if (was !== att.avail) this.notify(att.id, att.avail);
        if (m.t !== "avail") this.snapshot(ws, att.watch);
        if (m.t === "hello") {
          await this.updateLabel(att.id, att.label);
          const list = await this.teamList(att.id);
          try { ws.send(JSON.stringify({ t: "teams", list })); } catch {}
        }
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
