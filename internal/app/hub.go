package app

// Sunuculu mod: buluşma sunucusu (config.go'daki HubServer) ayarlıysa kullanılır.
// Kalıcı cihaz ID'si, çevrimiçi durumu, istek üzerine tünel açma ve kayıtlı cihazlar
// burada. Sunucusuz modda (Options.Server boş) hubLoop hemen döner, bu dosyadaki API
// uçları boş liste / "sunucu ayarlı değil" yanıtı verir ve uygulama yalnızca
// app.go'daki "Tüneli aç" yoluyla çalışır.

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"remotedesk/internal/hub"
)

// tunnelIdle: sunucu isteğiyle açılan tünel, bu kadar süre oturum olmazsa kapatılır.
const tunnelIdle = 3 * time.Minute

// ---- cihazın sunucu bağlantısı ----

func (a *App) hubLoop(ctx context.Context) {
	if a.opts.Server == "" {
		return
	}
	a.update(func(s *State) { s.Hub, s.HubMsg = "connecting", "Sunucuya bağlanılıyor…" })
	var dev hub.Device
	for wait := 2 * time.Second; ; wait = min(wait*2, time.Minute) {
		rctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		d, err := hub.LoadDevice(rctx, a.opts.DataDir, a.opts.Server)
		cancel()
		if err == nil {
			dev = d
			break
		}
		log.Printf("hub: kimlik alınamadı: %v", err)
		a.update(func(s *State) { s.Hub, s.HubMsg = "error", "Sunucuya ulaşılamıyor" })
		if !sleepCtx(ctx, wait) {
			return
		}
	}
	log.Printf("hub: cihaz ID %s", dev.ID)
	a.update(func(s *State) { s.DeviceID = dev.ID })
	a.announce()
	go a.tunnelIdleLoop(ctx)

	c := &hub.Client{
		Dev:    dev,
		OnOpen: a.openForPeer,
		OnState: func(online bool) {
			a.update(func(s *State) {
				if online {
					s.Hub, s.HubMsg = "online", "Çevrimiçi"
				} else {
					s.Hub, s.HubMsg = "connecting", "Sunucuya yeniden bağlanılıyor…"
				}
			})
		},
	}
	c.Run(ctx)
}

// openForPeer: biri bu cihaza bağlanmak istedi. Tünel kapalıysa açar, adresini döner.
func (a *App) openForPeer(ctx context.Context) (string, error) {
	a.tunnelNeeded.Store(time.Now().UnixNano())
	if a.opts.NoTunnel {
		return "http://" + a.hostAddr, nil
	}
	a.mu.Lock()
	off := a.st.Tunnel == "off"
	a.mu.Unlock()
	if off {
		a.tunnelAuto.Store(true)
		select {
		case a.tunnelReq <- true:
		default:
		}
	}
	t := time.NewTicker(200 * time.Millisecond)
	defer t.Stop()
	for {
		a.mu.Lock()
		st, id := a.st.Tunnel, a.st.ID
		a.mu.Unlock()
		if st == "on" && id != "" {
			return "https://" + id + ".trycloudflare.com", nil
		}
		select {
		case <-ctx.Done():
			return "", errors.New("Karşı cihaz tüneli zamanında açamadı.")
		case <-t.C:
		}
	}
}

// tunnelIdleLoop, kendiliğinden açılmış tüneli kullanılmıyorsa kapatır.
func (a *App) tunnelIdleLoop(ctx context.Context) {
	t := time.NewTicker(15 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if !a.tunnelAuto.Load() {
			continue
		}
		a.mu.Lock()
		busy := a.st.Session != nil || a.st.Pending != nil
		on := a.st.Tunnel != "off"
		a.mu.Unlock()
		if busy {
			a.tunnelNeeded.Store(time.Now().UnixNano())
			continue
		}
		if on && time.Since(time.Unix(0, a.tunnelNeeded.Load())) > tunnelIdle {
			log.Printf("tünel boşta, kapatılıyor")
			a.tunnelAuto.Store(false)
			select {
			case a.tunnelReq <- false:
			default:
			}
		}
	}
}

// ---- kayıtlı cihazlar ----

// Peer, bağlanılan/kaydedilen bir uzak cihazdır. Şifre saklanmaz.
type Peer struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Fav    bool   `json:"fav"`
	Last   int64  `json:"last"` // son bağlantı (unix ms)
	Online bool   `json:"online"`
}

type peerStore struct {
	mu    sync.Mutex
	path  string
	peers []Peer

	statAt time.Time
	stat   map[string]bool
}

func (p *peerStore) load(dataDir string) {
	p.path = filepath.Join(dataDir, "peers.json")
	if b, err := os.ReadFile(p.path); err == nil {
		json.Unmarshal(b, &p.peers)
	}
}

func (p *peerStore) saveLocked() {
	b, _ := json.MarshalIndent(p.peers, "", "  ")
	if err := os.WriteFile(p.path, b, 0o600); err != nil {
		log.Printf("cihaz listesi kaydedilemedi: %v", err)
	}
}

func (p *peerStore) find(id string) *Peer {
	for i := range p.peers {
		if p.peers[i].ID == id {
			return &p.peers[i]
		}
	}
	return nil
}

// touch, cihazı listeye ekler (yoksa) ve son bağlantı zamanını günceller.
func (p *peerStore) touch(id string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if x := p.find(id); x != nil {
		x.Last = time.Now().UnixMilli()
	} else {
		p.peers = append(p.peers, Peer{ID: id, Last: time.Now().UnixMilli()})
	}
	p.saveLocked()
}

func cleanPeerID(s string) string {
	return strings.Map(func(r rune) rune {
		if r >= '0' && r <= '9' {
			return r
		}
		return -1
	}, s)
}

func (a *App) handlePeers(w http.ResponseWriter, r *http.Request) {
	p := &a.peers
	p.mu.Lock()
	list := make([]Peer, len(p.peers))
	copy(list, p.peers)
	fresh := time.Since(p.statAt) < 4*time.Second
	stat := p.stat
	p.mu.Unlock()

	if !fresh && a.opts.Server != "" && len(list) > 0 {
		ids := make([]string, 0, len(list))
		for _, x := range list {
			ids = append(ids, x.ID)
		}
		ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
		if s, err := hub.Status(ctx, a.opts.Server, ids); err == nil {
			stat = s
			p.mu.Lock()
			p.stat, p.statAt = s, time.Now()
			p.mu.Unlock()
		}
		cancel()
	}
	for i := range list {
		list[i].Online = stat[list[i].ID]
	}
	// Favoriler önce, sonra en son bağlanılan.
	sort.SliceStable(list, func(i, j int) bool {
		if list[i].Fav != list[j].Fav {
			return list[i].Fav
		}
		return list[i].Last > list[j].Last
	})
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(list)
}

func (a *App) handlePeerSave(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ID     string  `json:"id"`
		Name   *string `json:"name"`
		Fav    *bool   `json:"fav"`
		Delete bool    `json:"delete"`
	}
	if err := readJSON(r, &body); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	id := cleanPeerID(body.ID)
	if !hub.ValidID(id) {
		http.Error(w, "Geçersiz ID (9 haneli olmalı).", 400)
		return
	}
	p := &a.peers
	p.mu.Lock()
	defer p.mu.Unlock()
	if body.Delete {
		out := p.peers[:0]
		for _, x := range p.peers {
			if x.ID != id {
				out = append(out, x)
			}
		}
		p.peers = out
	} else {
		x := p.find(id)
		if x == nil {
			if len(p.peers) >= 200 {
				http.Error(w, "Cihaz listesi dolu.", 400)
				return
			}
			p.peers = append(p.peers, Peer{ID: id})
			x = &p.peers[len(p.peers)-1]
			p.statAt = time.Time{} // yeni cihazın durumu hemen sorulsun
		}
		if body.Name != nil {
			x.Name = cleanLabel(*body.Name)
		}
		if body.Fav != nil {
			x.Fav = *body.Fav
		}
	}
	p.saveLocked()
	writeOK(w)
}

func cleanLabel(s string) string {
	s = strings.TrimSpace(strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, s))
	if r := []rune(s); len(r) > 40 {
		s = string(r[:40])
	}
	return s
}

// handleResolve: kalıcı ID'yi, hedef cihazın o an açtığı tünel adresine çevirir.
func (a *App) handleResolve(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ID string `json:"id"`
	}
	if err := readJSON(r, &body); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	id := cleanPeerID(body.ID)
	if !hub.ValidID(id) {
		http.Error(w, "Geçersiz ID.", 400)
		return
	}
	if a.opts.Server == "" {
		http.Error(w, "Sunucu ayarlı değil.", 400)
		return
	}
	a.mu.Lock()
	self := a.st.DeviceID == id
	a.mu.Unlock()
	if self {
		http.Error(w, "Bu, bu bilgisayarın kendi ID'si.", 400)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
	defer cancel()
	url, err := hub.Connect(ctx, a.opts.Server, id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	a.peers.touch(id)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"url": url})
}
