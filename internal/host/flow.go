package host

import (
	"context"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

// flow, gönderilip viewer tarafından henüz onaylanmamış kareleri sayar ve
// gidiş-dönüş süresine (RTT) göre aynı anda yolda olabilecek kare sayısını ayarlar.
// RTT'si 100 ms olan bir tünelde sabit 3 kare sınırı hızı ~30 fps'e kilitlerdi;
// burada sınır ≈ en düşük RTT / kare aralığı + 2 olur (4 ile 12 arası).
type flow struct {
	interval time.Duration
	sent     []time.Time
	rtt      time.Duration // üstel ortalama (gösterim için; kuyruk süresini de içerir)
	base     time.Duration // son birkaç saniyedeki en düşük RTT ≈ kuyruksuz gerçek gecikme
	baseAt   time.Time
}

func newFlow(fps int) *flow { return &flow{interval: time.Second / time.Duration(fps)} }

func (f *flow) onSend() { f.sent = append(f.sent, time.Now()) }

func (f *flow) onAck() {
	if len(f.sent) == 0 {
		return
	}
	d := time.Since(f.sent[0])
	f.sent = f.sent[1:]
	if f.rtt == 0 {
		f.rtt = d
	} else {
		f.rtt = (f.rtt*7 + d) / 8
	}
	// Pencere ortalamaya göre ayarlanırsa kendi kuyruğunu büyütür (kuyruk → RTT artar →
	// pencere büyür). En düşük RTT kuyruktan etkilenmez; ağ değişirse 5 sn'de bir tazelenir.
	if f.base == 0 || d < f.base || time.Since(f.baseAt) > 5*time.Second {
		f.base, f.baseAt = d, time.Now()
	}
}

func (f *flow) inflight() int { return len(f.sent) }

func (f *flow) limit() int {
	// Alt sınır 4: ffmpeg ilk çıktıdan önce birkaç giriş karesi biriktirir;
	// daha dar pencere başlangıçta kilitlenmeye yol açar.
	n := int(f.base/f.interval) + 2
	return max(4, min(n, 12))
}

// full, yeni kare gönderilmemesi gerekiyorsa true döner. Uzun süredir onay gelmiyorsa
// (viewer bir onayı kaçırdıysa) sayaç sıfırlanır, akış kilitlenmez.
func (f *flow) full() bool {
	if len(f.sent) > 0 && time.Since(f.sent[0]) > time.Second {
		f.sent = f.sent[:0]
	}
	return len(f.sent) >= f.limit()
}

// perf, host tarafındaki gerçek kare hızını ve yakalama süresini viewer'a bildirir.
type perf struct {
	start  time.Time
	frames int
	capSum time.Duration
	caps   int
}

func (p *perf) capture(d time.Duration) { p.capSum += d; p.caps++ }
func (p *perf) frame()                  { p.frames++ }

func (p *perf) report(ctx context.Context, c *websocket.Conn, fl *flow, w, h int) {
	el := time.Since(p.start)
	if p.start.IsZero() {
		p.start = time.Now()
		return
	}
	if el < 2*time.Second {
		return
	}
	var capMs float64
	if p.caps > 0 {
		capMs = float64(p.capSum.Microseconds()) / float64(p.caps) / 1000
	}
	wsjson.Write(ctx, c, map[string]any{
		"t":    "hstat",
		"fps":  float64(p.frames) / el.Seconds(),
		"cap":  capMs,
		"rtt":  fl.rtt.Milliseconds(),
		"base": fl.base.Milliseconds(),
		"win":  fl.limit(),
		"w":    w, "h": h,
	})
	*p = perf{start: time.Now()}
}

func drainAcks(ch chan struct{}, fl *flow) {
	for {
		select {
		case <-ch:
			fl.onAck()
		default:
			return
		}
	}
}

func clearChan(ch chan struct{}) {
	for {
		select {
		case <-ch:
		default:
			return
		}
	}
}
