package app

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Sesli görüşme sinyalleşmesi: bağlanan kişiden gelen WebRTC mesajları ana pencereye
// (SSE), ana pencereden gelenler oturum bağlantısı üzerinden bağlanan kişiye gider.
// Durum akışından (/api/events) ayrı tutulur: o akış eski durumu atabilir, bu akış atmaz.

// pushRTC: mesaj, gönderen kişinin oturum numarasıyla sarılır: {"sid":N,"m":{...}}.
// Ana pencere toplantıda her kişiyle ayrı bir ses bağlantısı kurar.
func (a *App) pushRTC(sid int, msg json.RawMessage) {
	b, _ := json.Marshal(struct {
		SID int             `json:"sid"`
		M   json.RawMessage `json:"m"`
	}{sid, msg})
	a.mu.Lock()
	defer a.mu.Unlock()
	for ch := range a.rtcSubs {
		select {
		case ch <- b:
		default: // pencere yetişemiyor: bu mesaj kaybolur (görüşme zaman aşımına düşer)
		}
	}
}

func (a *App) handleRTCEvents(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "stream yok", 500)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	ch := make(chan []byte, 64)
	a.mu.Lock()
	a.rtcSubs[ch] = struct{}{}
	a.mu.Unlock()
	defer func() {
		a.mu.Lock()
		delete(a.rtcSubs, ch)
		a.mu.Unlock()
	}()
	fmt.Fprint(w, ": ok\n\n")
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

func (a *App) handleRTCSend(w http.ResponseWriter, r *http.Request) {
	b, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 64<<10))
	var body struct {
		SID int             `json:"sid"`
		M   json.RawMessage `json:"m"`
	}
	if err != nil || json.Unmarshal(b, &body) != nil || len(body.M) == 0 {
		http.Error(w, "geçersiz istek", 400)
		return
	}
	if err := a.host.SendRTC(body.SID, body.M); err != nil {
		http.Error(w, "Bağlı kimse yok.", 409)
		return
	}
	writeOK(w)
}
