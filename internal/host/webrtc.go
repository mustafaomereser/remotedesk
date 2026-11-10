package host

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"sync"
	"time"

	"github.com/pion/webrtc/v4"
)

// WebRTC ile doğrudan bağlantı. Görüntüleyen taraf iki veri kanalı açar:
//   "ctl"   görüntüleyen -> bu bilgisayar: şifre, klavye/fare, onaylar (JSON)
//   "media" bu bilgisayar -> görüntüleyen: kareler (ikili) ve tüm kontrol mesajları (JSON)
// Bu bilgisayardan giden her şey tek kanaldan gittiği için sıralama WebSocket'tekiyle
// aynıdır; klavye/fare ise ayrı kanalda olduğu için büyük bir karenin arkasında beklemez.
// Kanal tek mesajda en çok ~64 KB taşır: büyük ikili mesajlar parçalanır
// (ilk bayt 0 = tam mesaj, 1 = parça devam ediyor, 2 = son parça).

var iceServers = []webrtc.ICEServer{
	{URLs: []string{"stun:stun.cloudflare.com:3478", "stun:stun.l.google.com:19302"}},
}

const (
	dcChunk     = 48 << 10
	dcHighWater = 4 << 20 // kanalda bu kadar veri birikirse gönderen bekler
	maxPending  = 4       // aynı anda el sıkışmada olabilecek bağlantı
)

var errClosed = errors.New("bağlantı kapandı")

type dcConn struct {
	pc         *webrtc.PeerConnection
	ctl, media *webrtc.DataChannel
	in         chan []byte
	closed     chan struct{}
	once       sync.Once
	low        chan struct{}
	wmu        sync.Mutex // ikili parçalar başka mesajlarla karışmasın
}

func (d *dcConn) Kind() string { return "direct" }

func (d *dcConn) shut() {
	d.once.Do(func() {
		close(d.closed)
		d.pc.Close()
	})
}

func (d *dcConn) Close(string) {
	// Son mesajların (ör. hata) gitmesi için kısa süre tanı.
	go func() {
		time.Sleep(300 * time.Millisecond)
		d.shut()
	}()
}

func (d *dcConn) ReadJSON(ctx context.Context, v any) error {
	select {
	case b := <-d.in:
		return json.Unmarshal(b, v)
	case <-d.closed:
		return errClosed
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (d *dcConn) wait(ctx context.Context) error {
	for d.media.BufferedAmount() > dcHighWater {
		select {
		case <-d.low:
		case <-time.After(50 * time.Millisecond):
		case <-d.closed:
			return errClosed
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

func (d *dcConn) WriteJSON(ctx context.Context, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	d.wmu.Lock()
	defer d.wmu.Unlock()
	if err := d.wait(ctx); err != nil {
		return err
	}
	return d.media.SendText(string(b))
}

func (d *dcConn) WriteBinary(ctx context.Context, b []byte) error {
	d.wmu.Lock()
	defer d.wmu.Unlock()
	if len(b) < dcChunk {
		if err := d.wait(ctx); err != nil {
			return err
		}
		return d.media.Send(append([]byte{0}, b...))
	}
	for off := 0; off < len(b); off += dcChunk {
		end := min(off+dcChunk, len(b))
		flag := byte(1)
		if end == len(b) {
			flag = 2
		}
		if err := d.wait(ctx); err != nil {
			return err
		}
		part := make([]byte, 1+end-off)
		part[0] = flag
		copy(part[1:], b[off:end])
		if err := d.media.Send(part); err != nil {
			return err
		}
	}
	return nil
}

// HandleOffer, görüntüleyenin WebRTC teklifini yanıtlar. Bağlantı kurulunca oturum,
// tünel bağlantısıyla aynı akıştan (şifre / onay) geçer.
func (s *Server) HandleOffer(offer string) (string, error) {
	s.mu.Lock()
	if s.rtcPending >= maxPending {
		s.mu.Unlock()
		return "", errors.New("meşgul")
	}
	s.rtcPending++
	s.mu.Unlock()
	pendingDone := sync.OnceFunc(func() {
		s.mu.Lock()
		s.rtcPending--
		s.mu.Unlock()
	})

	pc, err := webrtc.NewPeerConnection(webrtc.Configuration{ICEServers: iceServers})
	if err != nil {
		pendingDone()
		return "", err
	}
	d := &dcConn{pc: pc, in: make(chan []byte, 256), closed: make(chan struct{}), low: make(chan struct{}, 1)}
	var mu sync.Mutex
	opened := 0
	start := func() {
		pendingDone()
		remote := "doğrudan"
		if sctp := pc.SCTP(); sctp != nil && sctp.Transport() != nil && sctp.Transport().ICETransport() != nil {
			if pair, err := sctp.Transport().ICETransport().GetSelectedCandidatePair(); err == nil && pair != nil && pair.Remote != nil {
				remote = pair.Remote.Address
			}
		}
		go func() {
			s.serve(d, remote)
			d.shut()
		}()
	}
	pc.OnDataChannel(func(dc *webrtc.DataChannel) {
		switch dc.Label() {
		case "ctl":
			d.ctl = dc
			dc.OnMessage(func(m webrtc.DataChannelMessage) {
				if len(m.Data) > maxClipBytes*3 {
					return
				}
				select {
				case d.in <- m.Data:
				case <-d.closed:
				}
			})
		case "media":
			d.media = dc
			dc.SetBufferedAmountLowThreshold(dcHighWater / 2)
			dc.OnBufferedAmountLow(func() {
				select {
				case d.low <- struct{}{}:
				default:
				}
			})
		default:
			dc.Close()
			return
		}
		dc.OnOpen(func() {
			mu.Lock()
			opened++
			ready := opened == 2 && d.ctl != nil && d.media != nil
			mu.Unlock()
			if ready {
				start()
			}
		})
		dc.OnClose(d.shut)
	})
	pc.OnConnectionStateChange(func(st webrtc.PeerConnectionState) {
		switch st {
		case webrtc.PeerConnectionStateFailed, webrtc.PeerConnectionStateClosed, webrtc.PeerConnectionStateDisconnected:
			if st != webrtc.PeerConnectionStateDisconnected {
				pendingDone()
				d.shut()
			}
		}
	})
	// Kurulamazsa (ağ engeli) kaynakları bırak.
	time.AfterFunc(20*time.Second, func() {
		mu.Lock()
		ok := opened == 2
		mu.Unlock()
		if !ok {
			pendingDone()
			d.shut()
		}
	})

	if err := pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: offer}); err != nil {
		d.shut()
		pendingDone()
		return "", err
	}
	answer, err := pc.CreateAnswer(nil)
	if err != nil {
		d.shut()
		pendingDone()
		return "", err
	}
	gathered := webrtc.GatheringCompletePromise(pc)
	if err := pc.SetLocalDescription(answer); err != nil {
		d.shut()
		pendingDone()
		return "", err
	}
	// Adayların hepsi yanıta girsin: tek gidiş-dönüşte el sıkışma (sunucuya tek mesaj).
	select {
	case <-gathered:
	case <-time.After(3 * time.Second):
		log.Printf("webrtc: aday toplama zaman aşımı, eldekilerle devam")
	}
	return pc.LocalDescription().SDP, nil
}
