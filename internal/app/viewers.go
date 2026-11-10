package app

import (
	"encoding/json"
	"log"
	"net/http"
	"sync"
	"time"

	"remotedesk/internal/frame"
)

// Bağlantı pencereleri: her uzak cihaz kendi penceresinde açılır. Aynı cihaza ikinci kez
// bağlanılmak istenirse yeni pencere açılmaz, mevcut pencere öne getirilir. Bağlantı
// bilgisi (şifre dahil) pencereye adres çubuğunda değil, tek kullanımlık anahtarla verilir.

type viewerConn struct {
	ID    string `json:"id"`
	Pw    string `json:"pw"`
	Name  string `json:"name"`
	Label string `json:"label"`
}

type viewerSet struct {
	mu      sync.Mutex
	open    map[string]frame.Window // uzak cihaz ID'si -> pencere
	pending map[string]pendingConn  // anahtar -> bağlantı bilgisi
}

type pendingConn struct {
	c  viewerConn
	at time.Time
}

func (v *viewerSet) isOpen(id string) bool {
	v.mu.Lock()
	defer v.mu.Unlock()
	_, ok := v.open[id]
	return ok
}

func (a *App) handleViewerOpen(w http.ResponseWriter, r *http.Request) {
	var c viewerConn
	if err := readJSON(r, &c); err != nil || c.ID == "" {
		http.Error(w, "geçersiz istek", 400)
		return
	}
	v := &a.viewers
	v.mu.Lock()
	if v.open == nil {
		v.open, v.pending = map[string]frame.Window{}, map[string]pendingConn{}
	}
	if win, ok := v.open[c.ID]; ok {
		v.mu.Unlock()
		win.Focus()
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"focused":true}`))
		return
	}
	for k, p := range v.pending { // süresi geçmiş anahtarları temizle
		if time.Since(p.at) > time.Minute {
			delete(v.pending, k)
		}
	}
	key := randString(24, "abcdefghijklmnopqrstuvwxyz0123456789")
	v.pending[key] = pendingConn{c: c, at: time.Now()}
	v.mu.Unlock()

	label := c.Label
	if label == "" {
		label = c.ID
	}
	id := c.ID
	win, err := frame.Open(frame.Options{
		URL:   "http://" + a.uiAddr + "/view.html?k=" + key,
		Title: label + " · RemoteDesk", W: 1280, H: 800, MinW: 480, MinH: 320,
		DataDir: viewerDataDir(a.opts.DataDir),
		OnClose: func() {
			v.mu.Lock()
			delete(v.open, id)
			v.mu.Unlock()
		},
	})
	if err != nil {
		v.mu.Lock()
		delete(v.pending, key)
		v.mu.Unlock()
		log.Printf("bağlantı penceresi açılamadı: %v", err)
		http.Error(w, "native pencere yok", http.StatusNotImplemented) // arayüz window.open'a düşer
		return
	}
	v.mu.Lock()
	v.open[id] = win
	v.mu.Unlock()
	writeOK(w)
}

// handleViewerTake: bağlantı penceresi açılınca bilgisini bir kez alır.
func (a *App) handleViewerTake(w http.ResponseWriter, r *http.Request) {
	key := r.URL.Query().Get("k")
	v := &a.viewers
	v.mu.Lock()
	p, ok := v.pending[key]
	delete(v.pending, key)
	v.mu.Unlock()
	if !ok || time.Since(p.at) > time.Minute {
		http.Error(w, "bulunamadı", 404)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(p.c)
}
