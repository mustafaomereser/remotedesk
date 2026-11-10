// Package host, bu bilgisayarı uzaktan kontrol için WebSocket üzerinden yayınlar.
// Bu handler cloudflared tüneli ile dışarı açılır; yerel UI API'si asla buraya eklenmemelidir.
package host

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"log"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/coder/websocket"
)

type Server struct {
	// Auth şifreyi doğrular. unattended=true ise sabit (gözetimsiz erişim) şifresiyle
	// gelinmiştir: bağlantı onay sorulmadan kabul edilir.
	Auth func(pw string) (ok, unattended bool)
	// Perms, bağlanan kişinin o anki izinlerini döner (oturum boyunca canlı okunur).
	Perms func() Perms
	// Browser, tünel adresinin tarayıcıda açılıp açılamayacağını söyler (nil: kapalı).
	// Açıksa "/" adresinde şifre soran izleme sayfası (WebFS'teki view.html) sunulur.
	Browser func() bool
	// OnRTC, bağlanan kişiden (sid) gelen sesli görüşme sinyalini (WebRTC) yerel arayüze iletir.
	OnRTC func(sid int, msg json.RawMessage)
	WebFS fs.FS
	// Confirm, şifresiz gelen bağlantı isteği için kullanıcıdan onay ister
	// (nil ise şifresiz istek kabul edilmez).
	Confirm func(ctx context.Context, remote, name string) bool
	// OnSession, oturum başladığında/bittiğinde çağrılır (sid: oturum numarası).
	OnSession func(sid int, remote, name string, active bool)
	// MaxViewers, aynı anda bağlı olabilecek kişi sayısıdır (nil: 1). Toplantıda herkes
	// aynı ekranı görür; fare/klavye izni oturum başına kapatılabilir.
	MaxViewers func() int
	// Video, H.264 için ffmpeg hazırsa yolunu ve kodlayıcıyı döner (nil olabilir).
	Video VideoInfo
	// Concurrent, "aynı anda kullanım" (simülasyon modu) açıksa true döner (nil olabilir).
	Concurrent func() bool

	mu         sync.Mutex
	sessions   map[int]*member // bağlı kişiler (sid -> oturum)
	nextSID    int
	ink        inkStore // ekrana çizimler (sonradan katılana da gönderilir)
	fails      int
	lockUntil  time.Time
	unauthed   int // henüz şifresini göndermemiş bağlantı sayısı
	rtcPending int // el sıkışmadaki WebRTC bağlantıları
	lastReq    time.Time
}

// allowRequest: şifresiz bağlantı isteklerini sınırlar (karşı tarafı onay pencereleriyle
// boğmamak için en çok 5 saniyede bir).
func (s *Server) allowRequest() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if time.Since(s.lastReq) < 5*time.Second {
		return false
	}
	s.lastReq = time.Now()
	return true
}

// maxUnauthed: aynı anda şifre aşamasında bekleyebilecek bağlantı sayısı. Tünel adresini
// bilen biri yüzlerce bağlantı açıp kaynak tüketemesin ve kilidi paralel denemelerle aşamasın.
const maxUnauthed = 8

func (s *Server) enterAuth() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.unauthed >= maxUnauthed {
		return false
	}
	s.unauthed++
	return true
}

func (s *Server) leaveAuth() {
	s.mu.Lock()
	s.unauthed--
	s.mu.Unlock()
}

// tryAuth, kilit kontrolü + şifre doğrulama + hata sayımını tek kilit altında yapar:
// paralel bağlantılar 5 deneme sınırını aşamaz.
func (s *Server) tryAuth(pw string) (ok, unattended, locked bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if time.Now().Before(s.lockUntil) {
		return false, false, true
	}
	if ok, un := s.Auth(pw); ok {
		s.fails = 0
		return true, un, false
	}
	s.fails++
	if s.fails >= 5 {
		s.fails = 0
		s.lockUntil = time.Now().Add(time.Minute)
	}
	return false, false, false
}

// Perms, bağlanan kişinin neleri yapabileceğidir.
type Perms struct {
	Control   bool `json:"control"`   // fare ve klavye
	Clipboard bool `json:"clipboard"` // pano paylaşımı (iki yönlü)
}

type message struct {
	T     string          `json:"t"`
	Pw    string          `json:"pw,omitempty"`
	X     int             `json:"x,omitempty"`
	Y     int             `json:"y,omitempty"`
	B     int             `json:"b,omitempty"`
	D     bool            `json:"d,omitempty"`
	DX    int             `json:"dx,omitempty"`
	DY    int             `json:"dy,omitempty"`
	C     string          `json:"c,omitempty"`
	V     int             `json:"v,omitempty"`
	I     int             `json:"i,omitempty"`
	H264  bool            `json:"h264,omitempty"`
	Codec string          `json:"codec,omitempty"`
	TS    float64         `json:"ts,omitempty"`
	Text  string          `json:"text,omitempty"`
	Name  string          `json:"name,omitempty"`
	Ch    string          `json:"ch,omitempty"`    // tuşun ürettiği karakter (simülasyon modu için)
	RTC   json.RawMessage `json:"rtc,omitempty"`   // sesli görüşme sinyali (WebRTC)
	Caret bool            `json:"caret,omitempty"` // dokunmatik: bu tıklamadan sonra yazı alanı kontrolü iste
	// Ekrana çizim
	K    int    `json:"k,omitempty"`    // çizgi numarası (izleyiciye göre)
	Tool string `json:"tool,omitempty"` // pen | laser
	Col  string `json:"col,omitempty"`
	Wd   int    `json:"wd,omitempty"`
	P    []int  `json:"p,omitempty"`
	E    bool   `json:"e,omitempty"`
}

// cleanName, viewer'ın gönderdiği adı ekranda gösterilebilir hale getirir.
func cleanName(s string) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, strings.TrimSpace(s))
	if r := []rune(s); len(r) > 32 {
		s = string(r[:32])
	}
	return s
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/ws", s.handleWS)
	// Tarayıcıdan erişim: yalnızca izleme sayfası ve onun dosyaları sunulur
	// (yerel arayüz ve API asla buradan sunulmaz).
	page := func(name, ctype string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if !s.browserAllowed() || s.WebFS == nil {
				w.Header().Set("Content-Type", "text/html; charset=utf-8")
				w.WriteHeader(http.StatusForbidden)
				w.Write([]byte(browserOff))
				return
			}
			b, err := fs.ReadFile(s.WebFS, name)
			if err != nil {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Type", ctype)
			w.Header().Set("Cache-Control", "no-store")
			w.Header().Set("X-Frame-Options", "DENY")
			w.Header().Set("Referrer-Policy", "no-referrer")
			w.Write(b)
		}
	}
	mux.HandleFunc("GET /{$}", page("view.html", "text/html; charset=utf-8"))
	mux.HandleFunc("GET /view.js", page("view.js", "text/javascript; charset=utf-8"))
	mux.HandleFunc("GET /style.css", page("style.css", "text/css; charset=utf-8"))
	mux.HandleFunc("GET /rtc.js", page("rtc.js", "text/javascript; charset=utf-8"))
	mux.HandleFunc("GET /titlebar.js", page("titlebar.js", "text/javascript; charset=utf-8"))
	// WebRTC el sıkışması (sunucusuz modda tünel üzerinden; sunuculu modda buluşma sunucusu
	// üzerinden HandleOffer doğrudan çağrılır).
	mux.HandleFunc("POST /rtc", func(w http.ResponseWriter, r *http.Request) {
		if fromBrowser(r) {
			http.Error(w, "yalnızca uygulama", http.StatusForbidden)
			return
		}
		var body struct {
			SDP string `json:"sdp"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&body); err != nil || body.SDP == "" {
			http.Error(w, "geçersiz istek", 400)
			return
		}
		ans, err := s.HandleOffer(body.SDP)
		if err != nil {
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"sdp": ans})
	})
	mux.HandleFunc("/", http.NotFound)
	return mux
}

const browserOff = `<!doctype html><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>RemoteDesk</title><body style="margin:0;min-height:100vh;display:grid;place-items:center;background:#111113;color:#b0b4ba;font:15px system-ui,sans-serif;text-align:center;padding:24px">
<div><div style="font-size:18px;font-weight:600;color:#edeef0;margin-bottom:8px">Tarayıcıdan erişim kapalı</div>
Bu bilgisayara yalnızca RemoteDesk uygulamasıyla bağlanılabilir.</div></body>`

func (s *Server) browserAllowed() bool { return s.Browser != nil && s.Browser() }

// fromBrowser: bağlantı tünel adresinde açılmış bir web sayfasından mı geliyor?
// Uygulamanın izleme penceresi yerel adresten (127.0.0.1) bağlanır. Not: Origin başlığını
// tarayıcı dışı bir istemci taklit edebilir; asıl koruma şifredir, bu ayar tarayıcıyla
// erişimi kapatır.
func fromBrowser(r *http.Request) bool {
	o := r.Header.Get("Origin")
	if o == "" {
		return false
	}
	u, err := url.Parse(o)
	if err != nil {
		return true
	}
	h := u.Hostname()
	return h != "127.0.0.1" && h != "localhost" && h != "::1"
}

// member, bağlı bir kişidir. sess, onay/şifre aşaması bitip oturum başlayınca dolar.
type member struct {
	cancel context.CancelFunc
	sess   *session
	name   string
	remote string
}

// SendRTC, yerel arayüzden gelen sesli görüşme sinyalini sid numaralı kişiye iletir.
func (s *Server) SendRTC(sid int, msg json.RawMessage) error {
	sess := s.session(sid)
	if sess == nil {
		return errors.New("aktif oturum yok")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return sess.c.WriteJSON(ctx, map[string]any{"t": "rtc", "rtc": msg})
}

func (s *Server) session(sid int) *session {
	s.mu.Lock()
	defer s.mu.Unlock()
	if m := s.sessions[sid]; m != nil {
		return m.sess
	}
	return nil
}

// live, oturumu başlamış kişilerdir.
func (s *Server) live() map[int]*session {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[int]*session, len(s.sessions))
	for id, m := range s.sessions {
		if m.sess != nil {
			out[id] = m.sess
		}
	}
	return out
}

// Kick tüm oturumları sonlandırır.
func (s *Server) Kick() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, m := range s.sessions {
		m.cancel()
	}
}

// KickSession, tek bir kişinin bağlantısını keser.
func (s *Server) KickSession(sid int) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if m := s.sessions[sid]; m != nil {
		m.cancel()
		return true
	}
	return false
}

// SetControl, tek bir kişinin fare/klavye iznini açar ya da kapatır (genel izin kapalıysa
// açmak bir şey değiştirmez).
func (s *Server) SetControl(sid int, on bool) bool {
	sess := s.session(sid)
	if sess == nil {
		return false
	}
	sess.noControl.Store(!on)
	return true
}

// broadcast, bir mesajı (except hariç) tüm oturumlara gönderir.
func (s *Server) broadcast(msg any, except int) {
	for id, sess := range s.live() {
		if id == except {
			continue
		}
		go func(sess *session) {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			sess.c.WriteJSON(ctx, msg)
		}(sess)
	}
}

// sendPeople, herkese bağlı kişilerin listesini gönderir (toplantıda kimler var).
func (s *Server) sendPeople() {
	type person struct {
		ID   int    `json:"id"`
		Name string `json:"name"`
	}
	s.mu.Lock()
	list := []person{}
	for id, m := range s.sessions {
		if m.sess != nil {
			list = append(list, person{id, m.name})
		}
	}
	s.mu.Unlock()
	sort.Slice(list, func(i, j int) bool { return list[i].ID < list[j].ID })
	for id, sess := range s.live() {
		go func(id int, sess *session) {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			sess.c.WriteJSON(ctx, map[string]any{"t": "people", "list": list, "me": id})
		}(id, sess)
	}
}

func remoteAddr(r *http.Request) string {
	if ip := r.Header.Get("Cf-Connecting-Ip"); ip != "" {
		return ip
	}
	h, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return h
}

func (s *Server) handleWS(w http.ResponseWriter, r *http.Request) {
	if fromBrowser(r) && !s.browserAllowed() {
		http.Error(w, "Tarayıcıdan erişim kapalı.", http.StatusForbidden)
		return
	}
	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		// Viewer farklı bir origin'den (yerel UI) bağlanır; yetkilendirme şifre ile yapılır.
		InsecureSkipVerify: true,
		CompressionMode:    websocket.CompressionDisabled,
	})
	if err != nil {
		return
	}
	defer c.CloseNow()
	c.SetReadLimit(maxClipBytes * 3) // pano metni JSON içinde kaçışlı gelebilir
	s.serve(wsConn{c}, remoteAddr(r))
}

// serve, bağlantı türünden (WebSocket / WebRTC) bağımsız oturum akışıdır:
// şifre ya da onay, tek oturum kuralı, oturum.
func (s *Server) serve(c Conn, remote string) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var name string
	var err error

	if !s.enterAuth() {
		sendErr(ctx, c, "Sunucu meşgul. Biraz sonra tekrar dene.")
		return
	}
	actx, acancel := context.WithTimeout(ctx, 10*time.Second)
	var m message
	err = c.ReadJSON(actx, &m)
	acancel()
	if err != nil || m.T != "auth" {
		s.leaveAuth()
		return
	}
	// Şifresiz bağlantı = istek: karşı tarafta "Kabul et / Reddet" çıkar.
	// Şifreyle (tek kullanımlık ya da sabit) gelen doğrudan bağlanır.
	request := m.Pw == ""
	var ok, locked bool
	if !request {
		ok, _, locked = s.tryAuth(m.Pw)
	}
	if !request && !ok && !locked {
		log.Printf("hatalı şifre denemesi: %s", remote)
		time.Sleep(time.Second) // bu bağlantı şifre aşaması kotasını tutmaya devam eder
	}
	s.leaveAuth()
	name = cleanName(m.Name)
	if name == "" {
		name = "Uzak kullanıcı"
	}

	if locked {
		sendErr(ctx, c, "Çok fazla hatalı deneme. Biraz sonra tekrar dene.")
		return
	}
	if !request && !ok {
		sendErr(ctx, c, "Şifre yanlış.")
		return
	}
	if request {
		if s.Confirm == nil {
			sendErr(ctx, c, "Bu bilgisayar şifresiz bağlantı isteği kabul etmiyor. Şifreyle bağlan.")
			return
		}
		if !s.allowRequest() {
			sendErr(ctx, c, "Çok sık bağlantı isteği gönderildi. Biraz sonra tekrar dene.")
			return
		}
	}

	sid, full := s.claim(cancel, name, remote)
	if full {
		if s.maxViewers() <= 1 {
			sendErr(ctx, c, "Bu bilgisayara şu anda başka biri bağlı.")
		} else {
			sendErr(ctx, c, "Bu bilgisayara bağlanabilecek kişi sayısı doldu.")
		}
		return
	}
	defer s.release(sid)

	if request {
		c.WriteJSON(ctx, map[string]string{"t": "wait"})
		if !s.Confirm(ctx, remote, name) {
			sendErr(ctx, c, "Bağlantı karşı tarafça reddedildi.")
			return
		}
	}

	log.Printf("oturum başladı: %s (%s, %s)", remote, name, map[string]string{"tunnel": "tünel", "direct": "doğrudan"}[c.Kind()])
	sess := newSession(c, s.Video, m.H264)
	sess.name = name
	sess.sid = sid
	sess.srv = s
	sess.concurrent = s.Concurrent
	sess.perms = s.Perms
	if s.OnRTC != nil {
		sess.onRTC = func(msg json.RawMessage) { s.OnRTC(sid, msg) }
	}
	s.mu.Lock()
	if mb := s.sessions[sid]; mb != nil {
		mb.sess = sess
	}
	s.mu.Unlock()
	if s.OnSession != nil {
		s.OnSession(sid, remote, name, true)
		defer s.OnSession(sid, remote, name, false)
	}
	s.sendPeople()
	defer func() {
		// Ayrılan kişi listeden düşsün, kalanlara güncel liste gitsin.
		s.mu.Lock()
		if mb := s.sessions[sid]; mb != nil {
			mb.sess = nil
		}
		s.mu.Unlock()
		s.sendPeople()
	}()
	s.ink.replay(ctx, sess)
	if request {
		log.Printf("bağlantı isteği kabul edildi: %s", remote)
	} else {
		log.Printf("şifreyle bağlanıldı: %s", remote)
	}
	if m.V >= 10 && m.V <= 95 {
		sess.quality.Store(int32(m.V))
	}
	err = sess.run(ctx)
	log.Printf("oturum bitti: %s (%v)", remote, err)
	c.Close("")
}

func sendErr(ctx context.Context, c Conn, msg string) {
	wctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	c.WriteJSON(wctx, map[string]string{"t": "err", "msg": msg})
	c.Close("")
}

func (s *Server) maxViewers() int {
	if s.MaxViewers == nil {
		return 1
	}
	return min(max(s.MaxViewers(), 1), 16)
}

// claim, yeni bir kişi için yer ayırır; yer yoksa full=true.
func (s *Server) claim(cancel context.CancelFunc, name, remote string) (sid int, full bool) {
	limit := s.maxViewers()
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.sessions) >= limit {
		return 0, true
	}
	if s.sessions == nil {
		s.sessions = map[int]*member{}
	}
	s.nextSID++
	s.sessions[s.nextSID] = &member{cancel: cancel, name: name, remote: remote}
	return s.nextSID, false
}

func (s *Server) release(sid int) {
	s.mu.Lock()
	delete(s.sessions, sid)
	empty := len(s.sessions) == 0
	s.mu.Unlock()
	if empty {
		s.ClearInk() // herkes çıkınca ekrandaki çizimler de silinir
	}
}
