'use strict';

// Sesli görüşme (WebRTC). Ses iki taraf arasında doğrudan (eşler arası) gider; teklif,
// yanıt ve ICE adayları mevcut oturum bağlantısı üzerinden taşınır (sinyalleşme).
// Hem izleme penceresi (view.js) hem ana pencere (app.js) bu sınıfı kullanır.
//
// Mesajlar: {type:'offer', sdp} {type:'answer', sdp} {type:'ice', c} {type:'bye'} {type:'reject'}

const RTC_CONFIG = {
  iceServers: [
    { urls: 'stun:stun.cloudflare.com:3478' },
    { urls: 'stun:stun.l.google.com:19302' },
  ],
};

class VoiceCall {
  // send: sinyal mesajını karşı tarafa iletir. onState(state, info): arayüzü günceller.
  // state: idle | incoming | calling | connecting | active | error
  // getStream (isteğe bağlı): gönderilecek ses (varsayılan: mikrofon). Toplantıda ana
  // bilgisayar her kişiye "mikrofon + diğerlerinin sesi" karışımını gönderir.
  // onTrack (isteğe bağlı): karşı tarafın sesi geldiğinde (karıştırmak için).
  constructor({ send, onState, getStream, onTrack }) {
    this.send = send;
    this.onState = onState || (() => {});
    this.getStream = getStream || null;
    this.onTrack = onTrack || null;
    this.state = 'idle';
    this.pc = null;
    this.stream = null;
    this.audio = null;
    this.pendingOffer = null;
    this.pendingIce = [];
    this.muted = false;
  }

  set(state, info) {
    this.state = state;
    this.onState(state, info || '');
  }

  async micStream() {
    if (!navigator.mediaDevices || !navigator.mediaDevices.getUserMedia) {
      throw new Error('Bu pencerede mikrofon kullanılamıyor.');
    }
    try {
      return await navigator.mediaDevices.getUserMedia({
        audio: { echoCancellation: true, noiseSuppression: true, autoGainControl: true },
        video: false,
      });
    } catch (e) {
      if (e && e.name === 'NotAllowedError') throw new Error('Mikrofon izni verilmedi.');
      if (e && e.name === 'NotFoundError') throw new Error('Mikrofon bulunamadı.');
      throw new Error('Mikrofon açılamadı.');
    }
  }

  newPeer() {
    const pc = new RTCPeerConnection(RTC_CONFIG);
    pc.onicecandidate = (e) => { if (e.candidate) this.send({ type: 'ice', c: e.candidate.toJSON() }); };
    pc.ontrack = (e) => {
      if (!this.audio) {
        this.audio = new Audio();
        this.audio.autoplay = true;
      }
      this.audio.srcObject = e.streams[0];
      this.audio.play().catch(() => {});
      if (this.onTrack) this.onTrack(e.streams[0]);
    };
    pc.onconnectionstatechange = () => {
      const s = pc.connectionState;
      if (s === 'connected') this.set('active');
      else if (s === 'failed') { this.cleanup(); this.set('error', 'Ses bağlantısı kurulamadı (ağ engeli olabilir).'); }
      else if (s === 'disconnected') this.set('connecting', 'Ses bağlantısı koptu, yeniden deneniyor…');
    };
    this.pc = pc;
    return pc;
  }

  // start: görüşmeyi bu taraf başlatır.
  async start() {
    if (this.state !== 'idle' && this.state !== 'error') return;
    this.set('calling', 'Karşı tarafın kabul etmesi bekleniyor…');
    try {
      this.stream = this.getStream ? await this.getStream() : await this.micStream();
      const pc = this.newPeer();
      for (const t of this.stream.getTracks()) pc.addTrack(t, this.stream);
      const offer = await pc.createOffer();
      await pc.setLocalDescription(offer);
      this.send({ type: 'offer', sdp: pc.localDescription.sdp });
    } catch (e) {
      this.cleanup();
      this.set('error', e.message);
    }
  }

  // accept: gelen görüşmeyi kabul eder.
  async accept() {
    if (this.state !== 'incoming' || !this.pendingOffer) return;
    this.set('connecting', 'Bağlanıyor…');
    try {
      this.stream = this.getStream ? await this.getStream() : await this.micStream();
      const pc = this.newPeer();
      for (const t of this.stream.getTracks()) pc.addTrack(t, this.stream);
      await pc.setRemoteDescription({ type: 'offer', sdp: this.pendingOffer });
      this.pendingOffer = null;
      for (const c of this.pendingIce.splice(0)) await pc.addIceCandidate(c).catch(() => {});
      const answer = await pc.createAnswer();
      await pc.setLocalDescription(answer);
      this.send({ type: 'answer', sdp: pc.localDescription.sdp });
    } catch (e) {
      this.send({ type: 'bye' });
      this.cleanup();
      this.set('error', e.message);
    }
  }

  reject() {
    if (this.state !== 'incoming') return;
    this.send({ type: 'reject' });
    this.pendingOffer = null;
    this.pendingIce = [];
    this.set('idle');
  }

  hangup() {
    if (this.state === 'idle') return;
    this.send({ type: 'bye' });
    this.cleanup();
    this.set('idle');
  }

  toggleMute() {
    this.muted = !this.muted;
    if (this.stream) for (const t of this.stream.getAudioTracks()) t.enabled = !this.muted;
    return this.muted;
  }

  // handle: karşı taraftan gelen sinyal mesajı.
  async handle(m) {
    if (!m || typeof m.type !== 'string') return;
    switch (m.type) {
      case 'offer':
        if (this.state !== 'idle' && this.state !== 'error') { this.send({ type: 'reject', busy: true }); return; }
        this.pendingOffer = m.sdp;
        this.pendingIce = [];
        this.set('incoming');
        break;
      case 'answer':
        if (this.pc && this.state === 'calling') {
          this.set('connecting', 'Bağlanıyor…');
          await this.pc.setRemoteDescription({ type: 'answer', sdp: m.sdp }).catch(() => {});
        }
        break;
      case 'ice':
        if (!m.c) return;
        if (this.pc && this.pc.remoteDescription) await this.pc.addIceCandidate(m.c).catch(() => {});
        else this.pendingIce.push(m.c);
        break;
      case 'reject':
        this.cleanup();
        this.set('error', m.busy ? 'Karşı taraf başka bir görüşmede.' : 'Karşı taraf görüşmeyi reddetti.');
        break;
      case 'bye':
        this.cleanup();
        this.set('idle', 'Görüşme bitti.');
        break;
    }
  }

  cleanup() {
    if (this.pc) { try { this.pc.close(); } catch {} this.pc = null; }
    if (this.stream) { for (const t of this.stream.getTracks()) t.stop(); this.stream = null; }
    if (this.audio) { this.audio.srcObject = null; }
    this.pendingOffer = null;
    this.pendingIce = [];
    this.muted = false;
  }
}

window.VoiceCall = VoiceCall;
