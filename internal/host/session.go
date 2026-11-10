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
	"remotedesk/internal/input"
	"remotedesk/internal/sysutil"
	"remotedesk/internal/video"
)

const (
	maxFPS        = 30
	maxClipBytes  = 1 << 20
	maxVideoWidth = 2560
	maxInflight   = 3  // viewer'ın henüz çizmediği en fazla H.264 karesi
	refineFrames  = 12 // ekran durduktan sonra kaliteyi iyileştirmek için gönderilen ek kare
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
	c        *websocket.Conn
	in       *input.Injector
	video    VideoInfo
	wantH264 bool

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
}

func newSession(c *websocket.Conn, vi VideoInfo, wantH264 bool) *session {
	s := &session{
		c: c, in: input.New(), video: vi, wantH264: wantH264,
		ack: make(chan struct{}, 1), vack: make(chan struct{}, 64), poke: make(chan struct{}, 1),
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
	go func() {
		s.readLoop(ctx)
		cancel()
	}()
	go s.clipboardLoop(ctx)
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
			s.in.Move(m.X, m.Y)
		case "mb":
			s.in.Button(m.B, m.D, m.X, m.Y)
		case "wh":
			s.in.Wheel(m.DX, m.DY)
		case "k":
			s.in.Key(m.C, m.D)
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

// captureRetry, yakalama başarısızsa (kilit ekranı, UAC) bekleyip tekrar dener.
func (s *session) captureRetry(ctx context.Context) (*capture.Frame, error) {
	for {
		f, err := s.cp.Capture(s.displays[s.cur])
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

// jpegLoop, değişen 64x64 karoları JPEG olarak gönderir. Akış kontrolü: viewer bir kareyi
// çizip "ack" gönderene kadar yeni kare gönderilmez.
func (s *session) jpegLoop(ctx context.Context) error {
	var prev []byte
	first := true
	var pw, ph, pstride int
	interval := time.Second / maxFPS
	select {
	case s.ack <- struct{}{}:
	default:
	}

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-s.ack:
		}

		for {
			start := time.Now()
			if restart, err := s.applyRequests(ctx); err != nil {
				return err
			} else if restart {
				return errRestart
			}
			f, err := s.captureRetry(ctx)
			if err != nil {
				return err
			}
			if first || f.W != pw || f.H != ph || f.Stride != pstride {
				first = false
				pw, ph, pstride = f.W, f.H, f.Stride
				prev = nil
				s.setMapping(s.displays[s.cur], f.W, f.H)
				if err := wsjson.Write(ctx, s.c, map[string]any{"t": "init", "w": f.W, "h": f.H}); err != nil {
					return err
				}
			}

			rects := diffRects(prev, f)
			if len(rects) == 0 {
				if !sleepCtx(ctx, interval-time.Since(start)) {
					return ctx.Err()
				}
				continue
			}

			for _, p := range encodeRects(f, rects, int(s.quality.Load())) {
				if err := s.c.Write(ctx, websocket.MessageBinary, p); err != nil {
					return err
				}
			}
			if err := s.c.Write(ctx, websocket.MessageBinary, []byte{msgFrameEnd}); err != nil {
				return err
			}

			if len(prev) != len(f.Pix) {
				prev = make([]byte, len(f.Pix))
			}
			copy(prev, f.Pix)

			if !sleepCtx(ctx, interval-time.Since(start)) {
				return ctx.Err()
			}
			break
		}
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
// Akış kontrolü: viewer'ın çözmediği kare sayısı maxInflight'ı geçemez.
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
	e, err := video.Start(ectx, bin, enc, inW, inH, outW, outH, video.BitrateFor(int(q)))
	if err != nil {
		return fmt.Errorf("%w: %v", errVideoFailed, err)
	}
	defer e.Close()

	s.setMapping(d, outW, outH)
	if err := wsjson.Write(ctx, s.c, map[string]any{"t": "init", "w": outW, "h": outH}); err != nil {
		return err
	}
	s.keyframe.Store(false)
	for len(s.vack) > 0 {
		<-s.vack
	}

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

	prev := make([]byte, len(f.Pix))
	copy(prev, f.Pix)
	if err := e.WriteFrame(f); err != nil {
		return fmt.Errorf("%w: %v", errVideoFailed, err)
	}
	inflight, refine := 1, refineFrames
	lastAck := time.Now()
	interval := time.Second / maxFPS

	writeFailed := func() error {
		select {
		case err := <-sendErr:
			return err
		case <-time.After(time.Second):
			return errEncoderDied
		}
	}

	for {
		start := time.Now()
	drain:
		for {
			select {
			case <-s.vack:
				inflight--
				lastAck = time.Now()
			default:
				break drain
			}
		}
		inflight = max(inflight, 0)

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

		if inflight >= maxInflight {
			if time.Since(lastAck) > time.Second {
				inflight = 0 // viewer bir onayı kaçırmış olabilir
			} else {
				select {
				case <-ctx.Done():
					return ctx.Err()
				case err := <-sendErr:
					return err
				case <-s.vack:
					inflight--
					lastAck = time.Now()
				case <-s.poke:
				case <-time.After(50 * time.Millisecond):
				}
				continue
			}
		}

		f, err := s.cp.Capture(d)
		if err != nil {
			log.Printf("yakalama hatası: %v", err)
			if !sleepCtx(ctx, 500*time.Millisecond) {
				return ctx.Err()
			}
			continue
		}
		if f.W != inW || f.H != inH {
			return errRestart
		}
		if !bytes.Equal(f.Pix, prev) {
			copy(prev, f.Pix)
			refine = refineFrames
		} else if refine > 0 {
			refine--
		} else {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-s.poke:
			case <-time.After(interval - time.Since(start)):
			}
			continue
		}
		if err := e.WriteFrame(f); err != nil {
			return writeFailed()
		}
		inflight++
		if !sleepCtx(ctx, interval-time.Since(start)) {
			return ctx.Err()
		}
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
