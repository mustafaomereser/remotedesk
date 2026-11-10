package host

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"remotedesk/internal/capture"
	"remotedesk/internal/clipboard"
	"remotedesk/internal/desktop"
	"remotedesk/internal/input"
	"remotedesk/internal/sysutil"
	"remotedesk/internal/video"
)

const (
	maxFPS        = 60
	maxClipBytes  = 1 << 20
	maxVideoWidth = 2560
	refineFrames  = 20 // ekran durduktan sonra kaliteyi iyileştirmek için gönderilen ek kare
)

var (
	errRestart     = errors.New("akış yeniden başlatılıyor")
	errVideoFailed = errors.New("H.264 kullanılamıyor")
	errEncoderDied = errors.New("ffmpeg beklenmedik şekilde kapandı")
)

// VideoInfo, H.264 için hazır ffmpeg yolunu ve kodlayıcıyı döner.
type VideoInfo func() (bin, encoder string, ok bool)

const (
	codecNone = iota
	codecJPEG
	codecH264
)

type session struct {
	c          *websocket.Conn
	in         *input.Injector
	video      VideoInfo
	wantH264   bool
	name       string      // bağlanan kişinin adı (imleç etiketi)
	concurrent func() bool // "aynı anda kullanım" açık mı
	lastBusy   time.Time   // "karşı taraf kullanıyor" bildiriminin son gönderimi
	lastRoute  int
	remotePos  atomic.Uint64 // uzaktaki kişinin son imleç konumu (x<<32 | y), 0 = yok

	quality     atomic.Int32
	wantDisplay atomic.Int32 // -1: değişiklik yok
	wantCodec   atomic.Int32
	keyframe    atomic.Bool
	ack         chan struct{} // JPEG: kare çizildi
	vack        chan struct{} // H.264: kare çözüldü
	poke        chan struct{} // ayar değişti

	clipMu     sync.Mutex
	clipRemote string // viewer'dan gelip panoya yazdığımız son metin (yankıyı önlemek için)
	clipSent   string

	// Aşağıdakileri sadece akış goroutine'i kullanır.
	cp       *capture.Capturer
	displays []capture.Display
	cur      int
	lastEnum time.Time
	mode     string
	half     capture.Frame // Retina küçültme tamponu
}

func newSession(c *websocket.Conn, vi VideoInfo, wantH264 bool) *session {
	s := &session{
		c: c, in: input.New(), video: vi, wantH264: wantH264,
		ack: make(chan struct{}, 64), vack: make(chan struct{}, 64), poke: make(chan struct{}, 1),
	}
	s.quality.Store(70)
	s.wantDisplay.Store(-1)
	return s
}

func (s *session) pokeLoop() {
	select {
	case s.poke <- struct{}{}:
	default:
	}
}

func (s *session) videoReady() (string, string, bool) {
	if s.video == nil {
		return "", "", false
	}
	return s.video()
}

func (s *session) run(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	defer s.in.ReleaseAll()
	defer desktop.HideLabel()
	go func() {
		s.readLoop(ctx)
		cancel()
	}()
	go s.clipboardLoop(ctx)
	go s.takeoverLabel(ctx)
	return s.streamLoop(ctx)
}

func (s *session) readLoop(ctx context.Context) {
	for {
		var m message
		if err := wsjson.Read(ctx, s.c, &m); err != nil {
			return
		}
		switch m.T {
		case "ack":
			select {
			case s.ack <- struct{}{}:
			default:
			}
		case "vack":
			select {
			case s.vack <- struct{}{}:
			default:
			}
		case "mm":
			s.remotePos.Store(uint64(uint32(m.X))<<32 | uint64(uint32(m.Y)) | 1<<63)
			switch s.route(ctx, false) {
			case routeNormal:
				s.in.Move(m.X, m.Y)
			case routeDrop:
				s.label(m.X, m.Y, false)
			case routeSim:
				s.in.BgMove(m.X, m.Y)
				s.label(m.X, m.Y, true)
			}
		case "mb":
			// Normal yoldan basılan düğme normal yoldan bırakılmalı (takılı kalmasın).
			if !m.D && s.in.ButtonDown(m.B) {
				s.in.Button(m.B, false, m.X, m.Y)
				break
			}
			switch s.route(ctx, !m.D) {
			case routeNormal:
				s.in.Button(m.B, m.D, m.X, m.Y)
			case routeSim:
				s.in.BgButton(m.B, m.D, m.X, m.Y)
				s.label(m.X, m.Y, true)
			}
		case "wh":
			switch s.route(ctx, false) {
			case routeNormal:
				s.in.Wheel(m.DX, m.DY)
			case routeSim:
				s.in.BgWheel(m.DX, m.DY, m.X, m.Y)
			}
		case "k":
			if !m.D && s.in.KeyDown(m.C) {
				s.in.Key(m.C, false)
				break
			}
			switch s.route(ctx, !m.D) {
			case routeNormal:
				s.in.Key(m.C, m.D)
			case routeSim:
				s.in.BgKey(m.C, m.Ch, m.D)
			}
		case "q":
			if m.V >= 10 && m.V <= 95 {
				s.quality.Store(int32(m.V))
				s.pokeLoop()
			}
		case "disp":
			s.wantDisplay.Store(int32(m.I))
			s.pokeLoop()
		case "codec":
			if m.Codec == "h264" {
				s.wantCodec.Store(codecH264)
			} else {
				s.wantCodec.Store(codecJPEG)
			}
			s.pokeLoop()
		case "kf":
			s.keyframe.Store(true)
			s.pokeLoop()
		case "clip":
			if len(m.Text) <= maxClipBytes {
				s.clipMu.Lock()
				s.clipRemote = m.Text
				s.clipMu.Unlock()
				if err := clipboard.Write(m.Text); err != nil {
					log.Printf("pano yazılamadı: %v", err)
				}
			}
		case "ping":
			wsjson.Write(ctx, s.c, map[string]any{"t": "pong", "ts": m.TS})
		}
	}
}

// Giriş yönlendirme: yerel kullanıcı son 1 sn içinde fare/klavye kullandıysa uzaktaki
// kişinin girişi ya simülasyon moduna gider ("aynı anda kullanım" açıksa) ya da bekletilir.
const (
	routeNormal = iota
	routeSim
	routeDrop
)

const localPriority = time.Second

func (s *session) route(ctx context.Context, release bool) int {
	r := routeNormal
	switch {
	case s.concurrent != nil && s.concurrent():
		// Aynı anda kullanım: yerel kullanıcının imleci hiç oynamaz; uzaktaki kişi her zaman
		// kendi hayalet imleciyle (simülasyon) çalışır.
		r = routeSim
	case desktop.LocalActive(localPriority) && !release:
		r = routeDrop // yerel kullanıcıya öncelik; bırakma olayları her zaman uygulanır
	}
	// Mod değiştiğinde (veya beklerken saniyede bir) viewer'a bildir.
	if r != s.lastRoute || (r == routeDrop && time.Since(s.lastBusy) > time.Second) {
		s.lastRoute, s.lastBusy = r, time.Now()
		mode := map[int]string{routeNormal: "normal", routeSim: "sim", routeDrop: "wait"}[r]
		wsjson.Write(ctx, s.c, map[string]any{"t": "busy", "mode": mode})
	}
	return r
}

// takeoverLabel: kontrol bağlanan kişideyken ad etiketi gösterilmez (imleç zaten onun).
// Ana kullanıcı fareyi/klavyeyi devralınca, bağlanan kişinin imlecinin nerede kaldığı
// ad etiketiyle gösterilir; ana kullanıcı bırakınca etiket kaybolur.
func (s *session) takeoverLabel(ctx context.Context) {
	t := time.NewTicker(150 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if s.concurrent != nil && s.concurrent() {
			continue // aynı anda kullanımda hayalet imleç zaten çiziliyor
		}
		p := s.remotePos.Load()
		if p == 0 || !desktop.LocalActive(localPriority) {
			continue
		}
		s.label(int(int32(uint32(p>>32)&0x7FFFFFFF)), int(int32(uint32(p))), false)
	}
}

// label, ad etiketini uzaktaki kişinin işlem yaptığı noktaya koyar. ghost=true ise
// (simülasyon) gerçek imleç orada olmadığı için yanında bir ok imleci de çizilir.
func (s *session) label(x, y int, ghost bool) {
	dx, dy := s.in.Desktop(x, y)
	desktop.RemoteActivity(s.name, dx, dy, ghost)
}

// clipboardLoop, bu bilgisayarın panosu değişince metni viewer'a gönderir.
func (s *session) clipboardLoop(ctx context.Context) {
	last := clipboard.Seq()
	t := time.NewTicker(500 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		seq := clipboard.Seq()
		if seq == last {
			continue
		}
		text, err := clipboard.Read()
		if err != nil {
			log.Printf("pano okunamadı: %v", err)
			continue // last güncellenmedi, bir sonraki turda tekrar denenir
		}
		last = seq
		if text == "" || len(text) > maxClipBytes {
			continue
		}
		s.clipMu.Lock()
		skip := text == s.clipRemote || text == s.clipSent
		s.clipSent = text
		s.clipMu.Unlock()
		if !skip {
			log.Printf("pano değişti, viewer'a gönderiliyor (%d bayt)", len(text))
			wsjson.Write(ctx, s.c, map[string]any{"t": "clip", "text": text})
		}
	}
}

type displayInfo struct {
	W       int  `json:"w"`
	H       int  `json:"h"`
	Primary bool `json:"primary"`
}

func (s *session) sendDisplays(ctx context.Context) error {
	list := make([]displayInfo, len(s.displays))
	for i, d := range s.displays {
		list[i] = displayInfo{d.W, d.H, d.Primary}
	}
	return wsjson.Write(ctx, s.c, map[string]any{"t": "displays", "list": list, "cur": s.cur})
}

func (s *session) sendMode(ctx context.Context) error {
	m := map[string]any{"t": "mode", "v": s.mode}
	if s.mode == "h264" {
		_, enc, _ := s.videoReady()
		m["enc"] = enc
	}
	return wsjson.Write(ctx, s.c, m)
}

func (s *session) notice(ctx context.Context, msg string) {
	wsjson.Write(ctx, s.c, map[string]any{"t": "notice", "msg": msg})
}

// applyRequests, viewer'ın monitör/codec isteklerini ve monitör takılıp çıkarılmasını işler.
// Akışın yeniden kurulması gerekiyorsa restart=true döner.
func (s *session) applyRequests(ctx context.Context) (restart bool, err error) {
	changed := false
	if want := int(s.wantDisplay.Swap(-1)); want >= 0 && want < len(s.displays) && want != s.cur {
		s.cur, changed = want, true
	}
	if time.Since(s.lastEnum) > 2*time.Second {
		s.lastEnum = time.Now()
		if ds, err := capture.Displays(); err == nil && !capture.SameDisplays(ds, s.displays) {
			s.displays = ds
			if s.cur >= len(ds) {
				s.cur = capture.PrimaryIndex(ds)
			}
			changed = true
		}
	}
	if changed {
		if err := s.sendDisplays(ctx); err != nil {
			return false, err
		}
		restart = true
	}
	if c := s.wantCodec.Swap(codecNone); c != codecNone {
		m := "jpeg"
		if c == codecH264 {
			if _, _, ok := s.videoReady(); ok {
				m = "h264"
			} else {
				s.notice(ctx, "H.264 bu bilgisayarda henüz hazır değil (ffmpeg yok veya kuruluyor).")
			}
		}
		if m != s.mode {
			s.mode = m
			restart = true
		} else if err := s.sendMode(ctx); err != nil {
			return false, err
		}
	}
	return restart, nil
}

func (s *session) streamLoop(ctx context.Context) error {
	cp, err := capture.New()
	if err != nil {
		return err
	}
	defer cp.Close()
	s.cp = cp

	if s.displays, err = capture.Displays(); err != nil {
		return err
	}
	s.cur = capture.PrimaryIndex(s.displays)
	s.lastEnum = time.Now()
	if err := s.sendDisplays(ctx); err != nil {
		return err
	}

	// macOS'ta izin yoksa görüntü sadece duvar kağıdıdır ve giriş çalışmaz; viewer'a söyle.
	if screen, input := sysutil.PermissionStatus(); !screen || !input {
		var msgs []string
		if !screen {
			msgs = append(msgs, "Karşı bilgisayarda Ekran Kaydı izni yok: pencereler görünmez, sadece masaüstü arka planı gelir.")
		}
		if !input {
			msgs = append(msgs, "Karşı bilgisayarda Erişilebilirlik izni yok: fare ve klavye çalışmaz.")
		}
		wsjson.Write(ctx, s.c, map[string]any{"t": "warn", "msgs": msgs})
	}

	s.mode = "jpeg"
	if _, _, ok := s.videoReady(); ok && s.wantH264 {
		s.mode = "h264"
	}

	var deaths []time.Time
	for {
		if err := s.sendMode(ctx); err != nil {
			return err
		}
		var err error
		if s.mode == "h264" {
			err = s.h264Loop(ctx)
		} else {
			err = s.jpegLoop(ctx)
		}
		switch {
		case errors.Is(err, errRestart):
		case errors.Is(err, errEncoderDied):
			// Kısa sürede 3 kez çökerse JPEG'e düş.
			now := time.Now()
			deaths = append(deaths, now)
			if len(deaths) > 3 {
				deaths = deaths[1:]
			}
			if len(deaths) == 3 && now.Sub(deaths[0]) < 30*time.Second {
				log.Printf("ffmpeg sürekli kapanıyor, JPEG'e geçiliyor")
				s.mode = "jpeg"
				s.notice(ctx, "H.264 kodlayıcı sürekli hata verdi, JPEG'e geçildi.")
			}
		case errors.Is(err, errVideoFailed):
			log.Printf("%v, JPEG'e geçiliyor", err)
			s.mode = "jpeg"
			s.notice(ctx, "H.264 başlatılamadı, JPEG kullanılıyor.")
		default:
			return err
		}
	}
}

func (s *session) setMapping(d capture.Display, w, h int) {
	s.in.SetMapping(input.Mapping{
		OX: float64(d.X), OY: float64(d.Y),
		SX: float64(d.W) / float64(w), SY: float64(d.H) / float64(h),
	})
}

// grab, seçili monitörü yakalar. Retina ekranlarda (kalite "Kaliteli" değilse)
// görüntüyü yarıya indirir: 4 kat daha az piksel yakalama sonrası her adımı hızlandırır.
func (s *session) grab(d capture.Display) (*capture.Frame, time.Duration, error) {
	t := time.Now()
	s.cp.Full = s.quality.Load() >= 85
	f, err := s.cp.Capture(d)
	if err != nil {
		return nil, 0, err
	}
	if s.quality.Load() < 85 && f.W >= d.W*3/2 && f.H >= d.H*3/2 {
		capture.Half(f, &s.half)
		f = &s.half
	}
	return f, time.Since(t), nil
}

// grabbed, yakalayıcı goroutine'in ürettiği, sıkıştırılmış (Stride = W*4) bir karedir.
type grabbed struct {
	f   capture.Frame
	dur time.Duration
	err error
}

// startGrabber, ekranı ayrı bir goroutine'de maxFPS hızında yakalar. Böylece bir kare
// işlenirken (karşılaştırma, kodlama, gönderme) bir sonraki yakalanır: hız, iki işin
// toplamıyla değil yavaş olanıyla sınırlanır. Tüketici yetişemezse en eski kare atılır,
// her zaman en yeni kare verilir. Kullanılan kare free'ye geri konmalıdır.
// stop çağrıldığında goroutine'in bitmesi beklenir (Capturer'ı tek goroutine kullanır).
func (s *session) startGrabber(ctx context.Context, d capture.Display) (frames <-chan *grabbed, free chan<- *grabbed, stop func()) {
	ctx, cancel := context.WithCancel(ctx)
	out := make(chan *grabbed, 1)
	pool := make(chan *grabbed, 3)
	for range 3 {
		pool <- &grabbed{}
	}
	done := make(chan struct{})
	interval := time.Second / maxFPS
	go func() {
		defer close(done)
		for {
			start := time.Now()
			var g *grabbed
			select {
			case <-ctx.Done():
				return
			case g = <-pool:
			}
			f, dur, err := s.grab(d)
			g.dur, g.err = dur, err
			if err == nil {
				row := f.W * 4
				if need := row * f.H; cap(g.f.Pix) < need {
					g.f.Pix = make([]byte, need)
				} else {
					g.f.Pix = g.f.Pix[:need]
				}
				g.f.W, g.f.H, g.f.Stride = f.W, f.H, row
				if f.Stride == row {
					copy(g.f.Pix, f.Pix[:row*f.H])
				} else {
					for y := range f.H {
						copy(g.f.Pix[y*row:(y+1)*row], f.Pix[y*f.Stride:])
					}
				}
			}
			select {
			case out <- g:
			default: // tüketici yetişemiyor: eski kareyi geri dönüştür, yenisini koy
				select {
				case old := <-out:
					pool <- old
				default:
				}
				out <- g
			}
			wait := interval - time.Since(start)
			if err != nil {
				wait = 500 * time.Millisecond
			}
			if !sleepCtx(ctx, wait) {
				return
			}
		}
	}()
	return out, pool, func() { cancel(); <-done }
}

// captureRetry, yakalama başarısızsa (kilit ekranı, UAC) bekleyip tekrar dener.
func (s *session) captureRetry(ctx context.Context) (*capture.Frame, error) {
	for {
		f, _, err := s.grab(s.displays[s.cur])
		if err == nil {
			return f, nil
		}
		log.Printf("yakalama hatası: %v", err)
		if !sleepCtx(ctx, 500*time.Millisecond) {
			return nil, ctx.Err()
		}
		if restart, err := s.applyRequests(ctx); err != nil {
			return nil, err
		} else if restart {
			return nil, errRestart
		}
	}
}

// waitWindow, akış penceresi doluysa bir onay (veya ayar değişikliği) gelene kadar bekler.
func (s *session) waitWindow(ctx context.Context, fl *flow, ack chan struct{}, extra <-chan error) error {
	for fl.full() {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-extra:
			return err
		case <-ack:
			fl.onAck()
		case <-s.poke:
			return nil
		case <-time.After(100 * time.Millisecond):
		}
	}
	return nil
}

// jpegLoop, değişen 64x64 karoları JPEG olarak gönderir. Viewer her kareyi çizince
// "ack" gönderir; yolda olabilecek kare sayısı RTT'ye göre ayarlanır (flow).
func (s *session) jpegLoop(ctx context.Context) error {
	var prev []byte
	first := true
	var pw, ph, pstride int
	d := s.displays[s.cur]
	frames, free, stop := s.startGrabber(ctx, d)
	defer stop()
	fl := newFlow(maxFPS)
	var pf perf
	clearChan(s.ack)

	for {
		drainAcks(s.ack, fl)
		if err := s.waitWindow(ctx, fl, s.ack, nil); err != nil {
			return err
		}
		if restart, err := s.applyRequests(ctx); err != nil {
			return err
		} else if restart {
			return errRestart
		}

		var g *grabbed
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-s.poke:
			continue
		case g = <-frames:
		}
		if g.err != nil {
			log.Printf("yakalama hatası: %v", g.err)
			free <- g
			continue
		}
		pf.capture(g.dur)
		f := &g.f
		if first || f.W != pw || f.H != ph || f.Stride != pstride {
			first = false
			pw, ph, pstride = f.W, f.H, f.Stride
			prev = nil
			s.setMapping(d, f.W, f.H)
			if err := wsjson.Write(ctx, s.c, map[string]any{"t": "init", "w": f.W, "h": f.H}); err != nil {
				return err
			}
		}

		rects := diffRects(prev, f)
		if len(rects) > 0 {
			for _, p := range encodeRects(f, rects, int(s.quality.Load())) {
				if err := s.c.Write(ctx, websocket.MessageBinary, p); err != nil {
					return err
				}
			}
			if err := s.c.Write(ctx, websocket.MessageBinary, []byte{msgFrameEnd}); err != nil {
				return err
			}
			fl.onSend()
			pf.frame()
			if len(prev) != len(f.Pix) {
				prev = make([]byte, len(f.Pix))
			}
			copy(prev, f.Pix)
		}
		free <- g
		pf.report(ctx, s.c, fl, pw, ph)
	}
}

func fitEven(w, h, maxW int) (int, int) {
	if w > maxW {
		h = h * maxW / w
		w = maxW
	}
	return w &^ 1, h &^ 1
}

// h264Loop, ekranı ffmpeg ile H.264'e kodlar. Ekran değiştiğinde (ve ardından kaliteyi
// iyileştirmek için birkaç kare daha) kare gönderilir; ekran durağanken trafik olmaz.
// Akış kontrolü: viewer'ın çözmediği kare sayısı RTT'ye göre sınırlanır (flow).
func (s *session) h264Loop(ctx context.Context) error {
	bin, enc, ok := s.videoReady()
	if !ok {
		return errVideoFailed
	}
	f, err := s.captureRetry(ctx)
	if err != nil {
		return err
	}
	d := s.displays[s.cur]
	inW, inH := f.W, f.H
	outW, outH := fitEven(inW, inH, maxVideoWidth)
	q := s.quality.Load()

	ectx, cancel := context.WithCancel(ctx)
	defer cancel()
	e, err := video.Start(ectx, bin, enc, inW, inH, outW, outH, maxFPS, video.BitrateFor(int(q)))
	if err != nil {
		return fmt.Errorf("%w: %v", errVideoFailed, err)
	}
	defer e.Close()

	s.setMapping(d, outW, outH)
	if err := wsjson.Write(ctx, s.c, map[string]any{"t": "init", "w": outW, "h": outH}); err != nil {
		return err
	}
	s.keyframe.Store(false)
	clearChan(s.vack)

	// ffmpeg çıktısı -> websocket
	sendErr := make(chan error, 1)
	go func() {
		got := 0
		for fr := range e.Frames() {
			got++
			msg := make([]byte, 2+len(fr.Data))
			msg[0] = msgVideo
			if fr.Key {
				msg[1] = 1
			}
			copy(msg[2:], fr.Data)
			if err := s.c.Write(ectx, websocket.MessageBinary, msg); err != nil {
				sendErr <- err
				return
			}
		}
		if ectx.Err() != nil {
			sendErr <- ectx.Err()
		} else if got == 0 {
			sendErr <- fmt.Errorf("%w: %s", errVideoFailed, e.ErrText())
		} else {
			sendErr <- fmt.Errorf("%w: %s", errEncoderDied, e.ErrText())
		}
	}()

	fl := newFlow(maxFPS)
	var pf perf
	prev := make([]byte, len(f.Pix))
	copy(prev, f.Pix)
	if err := e.WriteFrame(f); err != nil {
		return fmt.Errorf("%w: %v", errVideoFailed, err)
	}
	fl.onSend()
	refine := refineFrames
	frames, free, stop := s.startGrabber(ctx, d)
	defer stop()

	writeFailed := func() error {
		select {
		case err := <-sendErr:
			return err
		case <-time.After(time.Second):
			return errEncoderDied
		}
	}

	for {
		drainAcks(s.vack, fl)
		select {
		case err := <-sendErr:
			return err
		default:
		}
		if restart, err := s.applyRequests(ctx); err != nil {
			return err
		} else if restart || s.keyframe.Load() || s.quality.Load() != q {
			return errRestart
		}
		if err := s.waitWindow(ctx, fl, s.vack, sendErr); err != nil {
			return err
		}
		if fl.full() {
			continue // poke ile uyandık; istekleri yukarıda işle
		}

		var g *grabbed
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-sendErr:
			return err
		case <-s.poke:
			continue
		case g = <-frames:
		}
		if g.err != nil {
			log.Printf("yakalama hatası: %v", g.err)
			free <- g
			continue
		}
		pf.capture(g.dur)
		f := &g.f
		if f.W != inW || f.H != inH {
			free <- g
			return errRestart
		}
		pf.report(ctx, s.c, fl, outW, outH)
		send := true
		if !bytes.Equal(f.Pix, prev) {
			copy(prev, f.Pix)
			refine = refineFrames
		} else if refine > 0 {
			refine--
		} else {
			send = false // ekran durağan: trafik yok
		}
		if send {
			if err := e.WriteFrame(f); err != nil {
				free <- g
				return writeFailed()
			}
			fl.onSend()
			pf.frame()
		}
		free <- g
	}
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
