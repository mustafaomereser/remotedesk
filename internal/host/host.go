// Package host, bu bilgisayarı uzaktan kontrol için WebSocket üzerinden yayınlar.
// Bu handler cloudflared tüneli ile dışarı açılır; yerel UI API'si asla buraya eklenmemelidir.
package host

import (
	"context"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

type Server struct {
	// Auth, verilen şifre doğruysa true döner.
	Auth func(pw string) bool
	// Confirm, kullanıcıdan bağlantı onayı ister (nil ise otomatik kabul).
	Confirm func(ctx context.Context, remote, name string) bool
	// OnSession, oturum başladığında/bittiğinde çağrılır.
	OnSession func(remote, name string, active bool)
	// Video, H.264 için ffmpeg hazırsa yolunu ve kodlayıcıyı döner (nil olabilir).
	Video VideoInfo
	// Concurrent, "aynı anda kullanım" (simülasyon modu) açıksa true döner (nil olabilir).
	Concurrent func() bool

	mu        sync.Mutex
	active    context.CancelFunc
	fails     int
	lockUntil time.Time
	unauthed  int // henüz şifresini göndermemiş bağlantı sayısı
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
func (s *Server) tryAuth(pw string) (ok, locked bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if time.Now().Before(s.lockUntil) {
		return false, true
	}
	if s.Auth(pw) {
		s.fails = 0
		return true, false
	}
	s.fails++
	if s.fails >= 5 {
		s.fails = 0
		s.lockUntil = time.Now().Add(time.Minute)
	}
	return false, false
}

type message struct {
	T     string  `json:"t"`
	Pw    string  `json:"pw,omitempty"`
	X     int     `json:"x,omitempty"`
	Y     int     `json:"y,omitempty"`
	B     int     `json:"b,omitempty"`
	D     bool    `json:"d,omitempty"`
	DX    int     `json:"dx,omitempty"`
	DY    int     `json:"dy,omitempty"`
	C     string  `json:"c,omitempty"`
	V     int     `json:"v,omitempty"`
	I     int     `json:"i,omitempty"`
	H264  bool    `json:"h264,omitempty"`
	Codec string  `json:"codec,omitempty"`
	TS    float64 `json:"ts,omitempty"`
	Text  string  `json:"text,omitempty"`
	Name  string  `json:"name,omitempty"`
	Ch    string  `json:"ch,omitempty"` // tuşun ürettiği karakter (simülasyon modu için)
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
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Write([]byte("RemoteDesk host\n"))
	})
	return mux
}

// Kick aktif oturumu sonlandırır.
func (s *Server) Kick() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.active != nil {
		s.active()
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

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	remote := remoteAddr(r)
	var name string

	if !s.enterAuth() {
		sendErr(ctx, c, "Sunucu meşgul. Biraz sonra tekrar dene.")
		return
	}
	actx, acancel := context.WithTimeout(ctx, 10*time.Second)
	var m message
	err = wsjson.Read(actx, c, &m)
	acancel()
	if err != nil || m.T != "auth" {
		s.leaveAuth()
		return
	}
	ok, locked := s.tryAuth(m.Pw)
	if !ok && !locked {
		log.Printf("hatalı şifre denemesi: %s", remoteAddr(r))
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
	if !ok {
		sendErr(ctx, c, "Şifre yanlış.")
		return
	}

	if !s.claim(cancel) {
		sendErr(ctx, c, "Bu bilgisayara şu anda başka biri bağlı.")
		return
	}
	defer s.release()

	if s.Confirm != nil {
		wsjson.Write(ctx, c, map[string]string{"t": "wait"})
		if !s.Confirm(ctx, remote, name) {
			sendErr(ctx, c, "Bağlantı karşı tarafça reddedildi.")
			return
		}
	}

	log.Printf("oturum başladı: %s (%s)", remote, name)
	if s.OnSession != nil {
		s.OnSession(remote, name, true)
		defer s.OnSession(remote, name, false)
	}
	sess := newSession(c, s.Video, m.H264)
	sess.name = name
	sess.concurrent = s.Concurrent
	if m.V >= 10 && m.V <= 95 {
		sess.quality.Store(int32(m.V))
	}
	err = sess.run(ctx)
	log.Printf("oturum bitti: %s (%v)", remote, err)
	c.Close(websocket.StatusNormalClosure, "")
}

func sendErr(ctx context.Context, c *websocket.Conn, msg string) {
	wctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	wsjson.Write(wctx, c, map[string]string{"t": "err", "msg": msg})
	c.Close(websocket.StatusNormalClosure, "")
}

func (s *Server) claim(cancel context.CancelFunc) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.active != nil {
		return false
	}
	s.active = cancel
	return true
}

func (s *Server) release() {
	s.mu.Lock()
	s.active = nil
	s.mu.Unlock()
}
