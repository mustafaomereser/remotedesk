// Package app; host sunucusunu, cloudflared tünelini ve yerel web arayüzünü birbirine bağlar.
package app

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"os/user"
	"path/filepath"
	"remotedesk/internal/hub"
	"remotedesk/internal/update"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"remotedesk/internal/clipboard"
	"remotedesk/internal/host"
	"remotedesk/internal/sysutil"
	"remotedesk/internal/tunnel"
	"remotedesk/internal/video"
	"remotedesk/web"
)

const WindowTitle = "RemoteDesk"

type Options struct {
	DataDir    string
	Headless   bool
	AutoAccept bool
	NoTunnel   bool
	Password   string
	Concurrent bool
	// Server: buluşma sunucusunun adresi (https://…). Boşsa kalıcı ID / cihaz listesi kapalıdır.
	Server string
	// UpdateRepo: güncellemelerin denetlendiği GitHub deposu ("sahip/ad"). Boşsa kapalı.
	UpdateRepo string
	// Version: çalışan sürüm (güncelleme karşılaştırması için).
	Version string
}

type Session struct {
	Name   string `json:"name"`
	Remote string `json:"remote"`
	Since  int64  `json:"since"`
}

type Pending struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Remote  string `json:"remote"`
	Expires int64  `json:"expires"`
}

// Perms, macOS'taki Ekran Kaydı / Erişilebilirlik izinlerinin durumudur.
type Perms struct {
	Needed     bool `json:"needed"` // bu platformda izin gerekiyor mu
	Screen     bool `json:"screen"`
	Input      bool `json:"input"`
	CanRestart bool `json:"canRestart"`
}

// State, arayüze SSE ile gönderilen anlık durumdur.
type State struct {
	Tunnel     string   `json:"tunnel"` // starting | downloading | on | error
	TunnelMsg  string   `json:"tunnelMsg"`
	ID         string   `json:"id"`
	Password   string   `json:"password"`
	AutoAccept bool     `json:"autoAccept"`
	Concurrent bool     `json:"concurrent"` // aynı anda kullanım (simülasyon modu)
	Session    *Session `json:"session"`
	Pending    *Pending `json:"pending"`
	Perms      Perms    `json:"perms"`
	Video      string   `json:"video"` // starting | downloading | on | error
	VideoMsg   string   `json:"videoMsg"`

	// Buluşma sunucusu: kalıcı cihaz ID'si ve bağlantı durumu.
	DeviceID string `json:"deviceId"`
	Hub      string `json:"hub"` // "" (kapalı) | connecting | online | error
	HubMsg   string `json:"hubMsg"`

	// Online: gelen bağlantılara açık mı (sunucuya bağlı / tünel açık).
	Online         bool   `json:"online"`
	OnlineAtStart  bool   `json:"onlineAtStart"`
	AllowControl   bool   `json:"allowControl"`
	AllowClipboard bool   `json:"allowClipboard"`
	FixedPassword  bool   `json:"fixedPassword"` // sabit şifre ayarlı mı (şifrenin kendisi gönderilmez)
	AllowBrowser   bool   `json:"allowBrowser"`
	Server         string `json:"server"` // buluşma sunucusu (tarayıcı adresi için)

	Elevated   bool        `json:"elevated"`   // Windows: yönetici olarak çalışıyor
	CanElevate bool        `json:"canElevate"` // "Yönetici olarak yeniden başlat" sunulabilir
	Version    string      `json:"version"`
	Update     *UpdateInfo `json:"update"`
}

type App struct {
	opts Options
	host *host.Server

	mu      sync.Mutex
	st      State
	subs    map[chan []byte]struct{}
	pending map[string]chan bool

	clients    int
	everClient bool
	lastLeave  time.Time

	hostAddr string
	uiAddr   string
	browser  *exec.Cmd

	ffmpegBin string // mu ile korunur; kodlayıcı hazır olunca dolar
	ffmpegEnc string

	quit     chan struct{}
	quitOnce sync.Once

	tunnelReq chan bool // arayüzden "Tüneli aç / kapat"

	// uiToken: yerel arayüz API'sinin oturum anahtarı. Pencere "/?k=<anahtar>" ile açılır,
	// sunucu bunu çereze çevirir; çerezi olmayan (başka kullanıcı, başka süreç, başka site)
	// istekler /api/* uçlarına erişemez.
	uiToken string

	tunnelAuto   atomic.Bool  // tünel sunucu isteğiyle açıldı: boşta kalınca kapat
	tunnelNeeded atomic.Int64 // tünelin en son gerektiği an (unix ns)
	peers        peerStore

	set      Settings      // kalıcı ayarlar (mu ile korunur)
	dev      hub.Device    // sunucudaki kimlik (mu ile korunur)
	onlineCh chan struct{} // çevrimiçi durumu ya da ID değişti: sunucu bağlantısını yeniden değerlendir
	upd      *update.Release

	rtcSubs map[chan []byte]struct{} // sesli görüşme sinyali dinleyen ana pencere(ler)
	viewers viewerSet

	hubc         *hub.Client // sunucu bağlantısı (mu ile korunur)
	hubConnected bool
}

func New(o Options) *App {
	pw := o.Password
	if pw == "" {
		pw = genPassword()
	}
	a := &App{
		tunnelReq: make(chan bool, 4),
		onlineCh:  make(chan struct{}, 1),
		rtcSubs:   map[chan []byte]struct{}{},
		uiToken:   randString(32, "abcdef0123456789"),
		opts:      o,
		subs:      map[chan []byte]struct{}{},
		pending:   map[string]chan bool{},
		quit:      make(chan struct{}),
		st: State{
			Tunnel: "starting", TunnelMsg: "Başlatılıyor…",
			Video: "starting", VideoMsg: "H.264 hazırlanıyor…",
			Password: pw, Version: o.Version, Server: o.Server,
			Elevated: sysutil.IsElevated(), CanElevate: sysutil.CanElevate(),
		},
	}
	a.set = a.loadSettings()
	if o.AutoAccept {
		a.set.AutoAccept = true
	}
	if o.Concurrent {
		a.set.Concurrent = true
	}
	a.syncStateLocked()
	a.st.Online = a.set.OnlineAtStart || o.Headless
	a.host = &host.Server{
		Auth: a.checkPassword, Confirm: a.confirm, OnSession: a.onSession,
		Video: a.videoInfo, Concurrent: a.concurrent, Perms: a.permissions,
		Browser: a.browserAllowed, WebFS: web.FS, OnRTC: a.pushRTC,
	}
	a.peers.load(o.DataDir)
	if sysutil.PermissionsNeeded() {
		sysutil.RequestPermissions()
		a.st.Perms = currentPerms()
		if !a.st.Perms.Screen {
			log.Printf("UYARI: Ekran Kaydı izni yok")
		}
		if !a.st.Perms.Input {
			log.Printf("UYARI: Erişilebilirlik izni yok")
		}
	}
	return a
}

func currentPerms() Perms {
	screen, input := sysutil.PermissionStatus()
	return Perms{Needed: true, Screen: screen, Input: input, CanRestart: sysutil.CanRestart()}
}

// permLoop, kullanıcı Sistem Ayarları'nda izin verdiğinde arayüzü günceller.
func (a *App) permLoop(ctx context.Context) {
	if !sysutil.PermissionsNeeded() {
		return
	}
	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		p := currentPerms()
		a.mu.Lock()
		if p != a.st.Perms {
			a.st.Perms = p
			a.broadcastLocked()
		}
		a.mu.Unlock()
	}
}

func (a *App) Quit() { a.quitOnce.Do(func() { close(a.quit) }) }

func (a *App) Run() error {
	hl, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	a.hostAddr = hl.Addr().String()
	go (&http.Server{Handler: a.host.Handler(), ReadHeaderTimeout: 10 * time.Second}).Serve(hl)
	log.Printf("host sunucusu: %s", a.hostAddr)

	ctx, cancel := context.WithCancel(context.Background())
	tunDone := make(chan struct{})
	go func() {
		a.tunnelLoop(ctx)
		close(tunDone)
	}()
	go a.hubLoop(ctx)
	go a.updateLoop(ctx)
	go a.videoSetup(ctx)
	go a.permLoop(ctx)

	if !a.opts.Headless {
		ul, err := listenUI()
		if err != nil {
			cancel()
			return err
		}
		a.uiAddr = ul.Addr().String()
		go (&http.Server{Handler: a.uiHandler(), ReadHeaderTimeout: 10 * time.Second}).Serve(ul)
		log.Printf("arayüz: http://%s/", a.uiAddr)
		a.openUI()
	}

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	select {
	case <-a.quit:
	case <-sig:
	}
	log.Printf("kapatılıyor")
	if a.browser != nil {
		closeBrowser(a.browser)
	}
	a.host.Kick()
	cancel()
	select {
	case <-tunDone:
	case <-time.After(3 * time.Second):
	}
	return nil
}

// listenUI sabit bir port dener ki tarayıcı localStorage'ı (son bağlantılar) korunsun.
func listenUI() (net.Listener, error) {
	for p := 47800; p < 47810; p++ {
		if l, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", p)); err == nil {
			return l, nil
		}
	}
	return net.Listen("tcp", "127.0.0.1:0")
}

// ---- durum yönetimi ----

func (a *App) update(f func(s *State)) {
	a.mu.Lock()
	defer a.mu.Unlock()
	f(&a.st)
	a.broadcastLocked()
}

func (a *App) broadcastLocked() {
	b, _ := json.Marshal(a.st)
	for ch := range a.subs {
		select {
		case ch <- b:
		default: // eski durumu at, yenisini koy
			select {
			case <-ch:
			default:
			}
			select {
			case ch <- b:
			default:
			}
		}
	}
}

func (a *App) concurrent() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.set.Concurrent
}

func (a *App) onSession(remote, name string, active bool) {
	a.tunnelNeeded.Store(time.Now().UnixNano())
	if !active {
		a.pushRTC(json.RawMessage(`{"type":"bye"}`)) // oturum bitti: açık görüşme kapansın
	}
	a.update(func(s *State) {
		if active {
			s.Session = &Session{Name: name, Remote: remote, Since: time.Now().UnixMilli()}
		} else {
			s.Session = nil
		}
	})
}

func (a *App) confirm(ctx context.Context, remote, name string) bool {
	a.mu.Lock()
	if a.opts.Headless {
		auto := a.set.AutoAccept
		a.mu.Unlock()
		if !auto {
			log.Printf("%s isteği reddedildi: pencere yok (şifreyle bağlanılmalı)", remote)
		}
		return auto // -headless -autoaccept: test için şifresiz isteği de kabul et
	}
	id := randString(12, "abcdefghijklmnopqrstuvwxyz0123456789")
	ch := make(chan bool, 1)
	a.pending[id] = ch
	const timeout = 30 * time.Second
	a.st.Pending = &Pending{ID: id, Name: name, Remote: remote, Expires: time.Now().Add(timeout).UnixMilli()}
	a.broadcastLocked()
	a.mu.Unlock()

	go sysutil.Attention(WindowTitle)

	var ok bool
	select {
	case ok = <-ch:
	case <-time.After(timeout):
	case <-ctx.Done():
	}

	a.mu.Lock()
	delete(a.pending, id)
	if a.st.Pending != nil && a.st.Pending.ID == id {
		a.st.Pending = nil
	}
	a.broadcastLocked()
	a.mu.Unlock()
	return ok
}

// ---- H.264 (ffmpeg) ----

func (a *App) videoInfo() (string, string, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.ffmpegBin, a.ffmpegEnc, a.ffmpegBin != "" && a.ffmpegEnc != ""
}

// videoSetup, ffmpeg'i bulur (yoksa indirir) ve çalışan bir H.264 kodlayıcı seçer.
// Hazır olana kadar oturumlar JPEG ile çalışır.
func (a *App) videoSetup(ctx context.Context) {
	bin := video.Find(a.opts.DataDir)
	if bin == "" {
		a.update(func(s *State) { s.Video, s.VideoMsg = "downloading", "ffmpeg indiriliyor…" })
		var err error
		bin, err = video.Download(ctx, a.opts.DataDir, func(done, total int64) {
			msg := fmt.Sprintf("ffmpeg indiriliyor… %.0f MB", float64(done)/1e6)
			if total > 0 {
				msg = fmt.Sprintf("ffmpeg indiriliyor… %%%d", done*100/total)
			}
			a.update(func(s *State) { s.VideoMsg = msg })
		})
		if err != nil {
			if ctx.Err() == nil {
				log.Printf("ffmpeg indirilemedi: %v", err)
				a.update(func(s *State) { s.Video, s.VideoMsg = "error", "ffmpeg indirilemedi, JPEG kullanılacak" })
			}
			return
		}
	}
	a.update(func(s *State) { s.Video, s.VideoMsg = "starting", "H.264 kodlayıcı test ediliyor…" })
	enc, err := video.Detect(ctx, bin)
	if err != nil {
		if ctx.Err() == nil {
			log.Printf("H.264 kodlayıcı yok: %v", err)
			a.update(func(s *State) { s.Video, s.VideoMsg = "error", "H.264 kodlayıcı bulunamadı, JPEG kullanılacak" })
		}
		return
	}
	log.Printf("H.264 hazır: %s (%s)", enc, bin)
	a.mu.Lock()
	a.ffmpegBin, a.ffmpegEnc = bin, enc
	a.mu.Unlock()
	a.update(func(s *State) { s.Video, s.VideoMsg = "on", "H.264 hazır ("+encoderLabel(enc)+")" })
}

func encoderLabel(enc string) string {
	switch enc {
	case "h264_nvenc":
		return "NVIDIA NVENC"
	case "h264_qsv":
		return "Intel Quick Sync"
	case "h264_amf":
		return "AMD AMF"
	case "h264_videotoolbox":
		return "VideoToolbox"
	case "h264_mf":
		return "Media Foundation"
	case "libx264":
		return "x264, yazılımsal"
	}
	return enc
}

// ---- tünel ----

func (a *App) tunnelLoop(ctx context.Context) {
	if a.opts.NoTunnel {
		a.update(func(s *State) {
			s.Tunnel, s.ID, s.TunnelMsg = "on", a.hostAddr, "Yerel mod (tünel kapalı)"
		})
		a.announce()
		<-ctx.Done()
		return
	}
	// Pencereli kullanımda tünel kullanıcı isteyince açılır; -headless'ta hemen.
	var stop context.CancelFunc
	var done chan struct{}
	start := func() {
		if stop != nil {
			return
		}
		tctx, cancel := context.WithCancel(ctx)
		stop, done = cancel, make(chan struct{})
		go func(d chan struct{}) {
			a.runTunnel(tctx)
			close(d)
		}(done)
	}
	halt := func() {
		if stop == nil {
			return
		}
		stop()
		<-done
		stop = nil
		a.host.Kick()
	}
	setOff := func() {
		a.update(func(s *State) {
			s.Tunnel, s.ID, s.TunnelMsg = "off", "", "Tünel kapalı"
		})
	}
	// Sunucusuz modda "gelen bağlantılara açık" = tünel açık. Sunuculu modda tünel ancak
	// biri bağlanmak isteyince açılır (hub.go).
	if a.isOnline() && (a.opts.Server == "" || a.opts.Headless) {
		start()
	} else {
		setOff()
	}
	for {
		select {
		case on := <-a.tunnelReq:
			if on {
				start()
			} else if stop != nil {
				halt()
				setOff()
			}
		case <-ctx.Done():
			if done != nil {
				<-done
			}
			return
		}
	}
}

func (a *App) runTunnel(ctx context.Context) {
	cfHome := filepath.Join(a.opts.DataDir, "cloudflared-home")
	for ctx.Err() == nil {
		bin := tunnel.Find(a.opts.DataDir)
		if bin == "" {
			a.update(func(s *State) { s.Tunnel, s.TunnelMsg = "downloading", "cloudflared indiriliyor…" })
			var err error
			bin, err = tunnel.Download(ctx, a.opts.DataDir, func(done, total int64) {
				msg := fmt.Sprintf("cloudflared indiriliyor… %.1f MB", float64(done)/1e6)
				if total > 0 {
					msg = fmt.Sprintf("cloudflared indiriliyor… %%%d", done*100/total)
				}
				a.update(func(s *State) { s.TunnelMsg = msg })
			})
			if err != nil {
				log.Printf("cloudflared indirilemedi: %v", err)
				a.update(func(s *State) { s.Tunnel, s.TunnelMsg = "error", "cloudflared indirilemedi: "+err.Error() })
				if !sleepCtx(ctx, 10*time.Second) {
					return
				}
				continue
			}
		}

		a.update(func(s *State) { s.Tunnel, s.ID, s.TunnelMsg = "starting", "", "Tünel açılıyor…" })
		t, err := tunnel.Start(ctx, bin, "http://"+a.hostAddr, cfHome)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			log.Printf("tünel: %v", err)
			a.update(func(s *State) { s.Tunnel, s.TunnelMsg = "error", "Tünel açılamadı: "+err.Error() })
			if !sleepCtx(ctx, 5*time.Second) {
				return
			}
			continue
		}
		log.Printf("tünel hazır: %s", t.URL)
		a.update(func(s *State) { s.Tunnel, s.ID, s.TunnelMsg = "on", t.ID(), "Hazır, bağlantı bekleniyor" })
		a.announce()

		select {
		case <-t.Done():
			log.Printf("tünel kapandı: %v", t.Err())
			a.update(func(s *State) {
				s.Tunnel, s.ID, s.TunnelMsg = "starting", "", "Tünel koptu, yeniden bağlanılıyor…"
			})
			if !sleepCtx(ctx, 2*time.Second) {
				return
			}
		case <-ctx.Done():
			<-t.Done()
			return
		}
	}
}

func (a *App) announce() {
	if !a.opts.Headless {
		return
	}
	a.mu.Lock()
	id, pw, dev := a.st.ID, a.st.Password, a.st.DeviceID
	a.mu.Unlock()
	if dev != "" {
		fmt.Printf("\n  Cihaz : %s", dev)
	}
	fmt.Printf("\n  ID    : %s\n  Şifre : %s\n\n", id, pw)
}

// ---- yerel arayüz ----

func (a *App) uiHandler() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /", http.FileServerFS(web.FS))
	mux.HandleFunc("GET /api/events", a.handleEvents)
	mux.HandleFunc("POST /api/host/password", a.handlePassword)
	mux.HandleFunc("POST /api/settings", a.handleSettings)
	mux.HandleFunc("POST /api/viewer/open", a.handleViewerOpen)
	mux.HandleFunc("POST /api/rtc/connect", a.handleRTCConnect)
	mux.HandleFunc("GET /api/viewer/take", a.handleViewerTake)
	mux.HandleFunc("GET /api/rtc/events", a.handleRTCEvents)
	mux.HandleFunc("POST /api/rtc", a.handleRTCSend)
	mux.HandleFunc("POST /api/settings/fixedpw", a.handleFixedPassword)
	mux.HandleFunc("POST /api/settings/rename", a.handleRename)
	mux.HandleFunc("POST /api/elevate", a.handleElevate)
	mux.HandleFunc("POST /api/update/check", a.handleUpdateCheck)
	mux.HandleFunc("POST /api/update/install", a.handleUpdateInstall)
	mux.HandleFunc("POST /api/host/tunnel", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Value bool `json:"value"`
		}
		if err := readJSON(r, &body); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		a.tunnelAuto.Store(false) // elle açılan tünel kendiliğinden kapanmaz
		if a.opts.Server == "" {
			a.setOnline(body.Value)
		} else {
			select {
			case a.tunnelReq <- body.Value:
			default:
			}
		}
		writeOK(w)
	})
	mux.HandleFunc("GET /api/peers", a.handlePeers)
	mux.HandleFunc("POST /api/peers", a.handlePeerSave)
	mux.HandleFunc("POST /api/resolve", a.handleResolve)
	mux.HandleFunc("GET /api/me", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"name": defaultName()})
	})
	mux.HandleFunc("POST /api/host/confirm", a.handleConfirm)
	mux.HandleFunc("POST /api/host/kick", func(w http.ResponseWriter, r *http.Request) {
		a.host.Kick()
		writeOK(w)
	})
	mux.HandleFunc("POST /api/perm/open", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Kind string `json:"kind"`
		}
		if err := readJSON(r, &body); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		if err := sysutil.OpenPermissionSettings(body.Kind); err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		writeOK(w)
	})
	mux.HandleFunc("POST /api/restart", func(w http.ResponseWriter, r *http.Request) {
		if err := sysutil.Restart(); err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		writeOK(w)
		go func() { time.Sleep(200 * time.Millisecond); a.Quit() }()
	})
	mux.HandleFunc("GET /api/clipboard", a.handleClipboardGet)
	mux.HandleFunc("POST /api/clipboard", a.handleClipboardSet)
	mux.HandleFunc("POST /api/quit", func(w http.ResponseWriter, r *http.Request) {
		writeOK(w)
		go func() { time.Sleep(200 * time.Millisecond); a.Quit() }()
	})
	return a.guard(mux)
}

// guard, DNS rebinding ve başka sitelerden gelen CSRF isteklerine karşı korur.
func (a *App) guard(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Host != a.uiAddr {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		if r.URL.Path == "/" && r.URL.Query().Get("k") != "" {
			if subtle.ConstantTimeCompare([]byte(r.URL.Query().Get("k")), []byte(a.uiToken)) != 1 {
				http.Error(w, "forbidden", http.StatusForbidden)
				return
			}
			http.SetCookie(w, &http.Cookie{Name: "rd", Value: a.uiToken, Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode})
			http.Redirect(w, r, "/", http.StatusSeeOther)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/api/") {
			c, err := r.Cookie("rd")
			if err != nil || subtle.ConstantTimeCompare([]byte(c.Value), []byte(a.uiToken)) != 1 {
				http.Error(w, "forbidden", http.StatusForbidden)
				return
			}
		}
		if r.Method != http.MethodGet {
			if o := r.Header.Get("Origin"); o != "" && o != "http://"+a.uiAddr {
				http.Error(w, "forbidden", http.StatusForbidden)
				return
			}
		}
		w.Header().Set("Cache-Control", "no-store")
		h.ServeHTTP(w, r)
	})
}

func (a *App) handleEvents(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "stream yok", 500)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	ch := make(chan []byte, 1)

	a.mu.Lock()
	a.subs[ch] = struct{}{}
	a.clients++
	a.everClient = true
	b, _ := json.Marshal(a.st)
	a.mu.Unlock()

	defer func() {
		a.mu.Lock()
		delete(a.subs, ch)
		a.clients--
		a.lastLeave = time.Now()
		a.mu.Unlock()
	}()

	fmt.Fprintf(w, "data: %s\n\n", b)
	fl.Flush()
	keep := time.NewTicker(20 * time.Second)
	defer keep.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-a.quit:
			return
		case b := <-ch:
			fmt.Fprintf(w, "data: %s\n\n", b)
			fl.Flush()
		case <-keep.C:
			fmt.Fprint(w, ": ping\n\n")
			fl.Flush()
		}
	}
}

func (a *App) handlePassword(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Password string `json:"password"`
	}
	if err := readJSON(r, &body); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	pw := body.Password
	if pw == "" {
		pw = genPassword()
	} else if len([]rune(pw)) < 6 {
		http.Error(w, "Şifre en az 6 karakter olmalı", 400)
		return
	}
	a.update(func(s *State) { s.Password = pw })
	writeOK(w)
}

func (a *App) handleConfirm(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ID     string `json:"id"`
		Accept bool   `json:"accept"`
	}
	if err := readJSON(r, &body); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	a.mu.Lock()
	ch, ok := a.pending[body.ID]
	a.mu.Unlock()
	if !ok {
		http.Error(w, "İstek zaman aşımına uğradı", 404)
		return
	}
	select {
	case ch <- body.Accept:
	default:
	}
	writeOK(w)
}

// handleClipboardGet, viewer pencerelerinin bu bilgisayarın panosunu okumasını sağlar.
// seq parametresi güncel sayıyla aynıysa metin gönderilmez.
func (a *App) handleClipboardGet(w http.ResponseWriter, r *http.Request) {
	seq := clipboard.Seq()
	resp := map[string]any{"seq": strconv.FormatUint(seq, 10)}
	if r.URL.Query().Get("seq") != resp["seq"] {
		if text, err := clipboard.Read(); err == nil {
			resp["text"] = text
		}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

func (a *App) handleClipboardSet(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Text string `json:"text"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<20)).Decode(&body); err != nil {
		http.Error(w, "geçersiz istek", 400)
		return
	}
	if err := clipboard.Write(body.Text); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	writeOK(w)
}

func readJSON(r *http.Request, v any) error {
	if err := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 4096)).Decode(v); err != nil {
		return errors.New("geçersiz istek")
	}
	return nil
}

func writeOK(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(`{"ok":true}`))
}

// ---- pencere ----

func (a *App) openUI() {
	url := "http://" + a.uiAddr + "/?k=" + a.uiToken
	if openNativeWindow(url, a.opts.DataDir, a.Quit) {
		log.Printf("arayüz uygulama penceresinde açıldı")
		return
	}
	start := time.Now()
	cmd, err := openAppWindow(url, filepath.Join(a.opts.DataDir, "browser"))
	if err != nil {
		log.Printf("uygulama penceresi açılamadı (%v), varsayılan tarayıcı kullanılıyor", err)
		openDefaultBrowser(url)
		go a.watchdog()
		return
	}
	a.browser = cmd
	go func() {
		cmd.Wait()
		// Tarayıcı hemen döndüyse (zaten açık bir örneğe devretti) watchdog'a güveniyoruz.
		if time.Since(start) > 5*time.Second {
			a.Quit()
		}
	}()
	// macOS'ta pencere kapanınca tarayıcı süreci yaşamaya devam eder;
	// bu yüzden açık sayfa (ana pencere + viewer'lar) kalmayınca da kapanıyoruz.
	go a.watchdog()
}

// watchdog, uygulamaya ait hiçbir sayfa açık kalmayınca uygulamayı kapatır.
func (a *App) watchdog() {
	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-a.quit:
			return
		case <-t.C:
			a.mu.Lock()
			gone := a.everClient && a.clients == 0 && time.Since(a.lastLeave) > 10*time.Second
			a.mu.Unlock()
			if gone {
				a.Quit()
				return
			}
		}
	}
}

// ---- yardımcılar ----

// defaultName, viewer'daki "Adın" alanının varsayılanıdır: işletim sistemindeki ad.
func defaultName() string {
	u, err := user.Current()
	if err != nil {
		return ""
	}
	if n := strings.TrimSpace(u.Name); n != "" {
		return n
	}
	name := u.Username
	if i := strings.LastIndexByte(name, '\\'); i >= 0 { // Windows: ALAN\kullanıcı
		name = name[i+1:]
	}
	return name
}

func genPassword() string {
	return randString(8, "abcdefghjkmnpqrstuvwxyz23456789")
}

func randString(n int, alphabet string) string {
	b := make([]byte, n)
	rand.Read(b)
	for i := range b {
		b[i] = alphabet[int(b[i])%len(alphabet)]
	}
	return string(b)
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
