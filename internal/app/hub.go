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
	a.mu.Lock()
	a.dev = dev
	a.st.DeviceID = dev.ID
	a.broadcastLocked()
	a.mu.Unlock()
	a.announce()
	go a.tunnelIdleLoop(ctx)

	// Sunucuya tek bağlantı uygulama açık kaldıkça tutulur. "Gelen bağlantılara açık"
	// kapalıyken de açık kalır (cihaz listesinin çevrimiçi bilgisi için) ama sunucuya
	// "müsait değilim" denir: kimse bağlanamaz, başkalarına çevrimdışı görünür.
	// Çevrimiçi bilgisi sorgulanmaz; sunucu değişince kendisi bildirir (kota dostu).
	// ID değişince bağlantı yeni kimlikle yeniden kurulur.
	var stop context.CancelFunc
	var done chan struct{}
	halt := func() {
		if stop != nil {
			stop()
			<-done
			stop = nil
		}
	}
	defer halt()
	var curID string
	for {
		a.mu.Lock()
		on, d := a.st.Online, a.dev
		a.mu.Unlock()
		if d.ID != curID {
			halt()
			curID = d.ID
			cctx, cancel := context.WithCancel(ctx)
			stop, done = cancel, make(chan struct{})
			c := &hub.Client{
				Dev:    d,
				OnOpen: a.openForPeer,
				Hello: func() (bool, []string) {
					return a.isOnline(), a.watchIDs()
				},
				Label:      deviceLabel,
				OnTeams:    a.setTeams,
				OnPresence: a.peers.setPresence,
				OnSignal: func(offer string) (string, error) {
					if !a.isOnline() {
						return "", errors.New("Karşı cihaz bağlantılara kapalı.")
					}
					return a.host.HandleOffer(offer)
				},
				OnState: func(connected bool) {
					a.mu.Lock()
					a.hubConnected = connected
					a.mu.Unlock()
					a.refreshHubState()
				},
			}
			a.mu.Lock()
			a.hubc = c
			a.mu.Unlock()
			go func(d chan struct{}) {
				c.Run(cctx)
				close(d)
			}(done)
		} else {
			a.mu.Lock()
			c := a.hubc
			a.mu.Unlock()
			if c != nil {
				c.SetAvailable(on)
			}
		}
		a.refreshHubState()
		select {
		case <-ctx.Done():
			return
		case <-a.onlineCh:
		}
	}
}

// refreshHubState, sunucu bağlantısı ve "açık/kapalı" durumundan arayüz metnini üretir.
func (a *App) refreshHubState() {
	a.update(func(s *State) {
		switch {
		case !a.hubConnected:
			if s.Hub != "error" {
				s.Hub, s.HubMsg = "connecting", "Sunucuya bağlanılıyor…"
			}
		case !s.Online:
			s.Hub, s.HubMsg = "offline", "Bağlantılara kapalı"
		default:
			s.Hub, s.HubMsg = "online", "Çevrimiçi"
		}
	})
}

// watchPeers, kayıtlı cihaz listesi değişince sunucuya yeni izleme listesini bildirir.
func (a *App) watchPeers() {
	a.mu.Lock()
	c := a.hubc
	a.mu.Unlock()
	if c != nil {
		c.Watch(a.watchIDs())
	}
}

// watchIDs: çevrimiçi durumu izlenen cihazlar = kayıtlı cihazlar + takım üyeleri.
func (a *App) watchIDs() []string {
	seen := map[string]bool{}
	var out []string
	add := func(id string) {
		if id != "" && !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	for _, id := range a.peers.ids() {
		add(id)
	}
	a.mu.Lock()
	self := a.dev.ID
	for _, t := range a.teams {
		for _, m := range t.Members {
			if m.ID != self {
				add(m.ID)
			}
		}
	}
	a.mu.Unlock()
	if len(out) > 100 {
		out = out[:100]
	}
	return out
}

// ---- takımlar ----

func (a *App) setTeams(list []hub.Team) {
	a.mu.Lock()
	a.teams = list
	a.mu.Unlock()
	a.watchPeers()
}

// deviceLabel: takım üyelerine görünen ad (bilgisayarın adı).
func deviceLabel() string {
	h, _ := os.Hostname()
	return cleanLabel(h)
}

// TeamView, arayüze gönderilen takım bilgisidir (üyelerin çevrimiçi durumuyla).
type TeamView struct {
	hub.Team
	Members []TeamMemberView `json:"members"`
}

type TeamMemberView struct {
	hub.TeamMember
	Self   bool   `json:"self"`
	Online bool   `json:"online"`
	Open   bool   `json:"open"`
	Name   string `json:"name"` // kayıtlı cihazlarda verilen ad (varsa)
}

func (a *App) handleTeams(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	teams := append([]hub.Team(nil), a.teams...)
	self := a.dev.ID
	a.mu.Unlock()
	p := &a.peers
	out := make([]TeamView, 0, len(teams))
	for _, t := range teams {
		v := TeamView{Team: t, Members: make([]TeamMemberView, 0, len(t.Members))}
		for _, m := range t.Members {
			mv := TeamMemberView{TeamMember: m, Self: m.ID == self}
			p.mu.Lock()
			mv.Online = p.presence[m.ID]
			if x := p.find(m.ID); x != nil {
				mv.Name = x.Name
			}
			p.mu.Unlock()
			mv.Online = mv.Online || mv.Self
			mv.Open = a.viewers.isOpen(m.ID)
			v.Members = append(v.Members, mv)
		}
		// Çevrimiçi olanlar önce, sonra ada göre.
		sort.SliceStable(v.Members, func(i, j int) bool {
			if v.Members[i].Online != v.Members[j].Online {
				return v.Members[i].Online
			}
			return strings.ToLower(v.Members[i].Label) < strings.ToLower(v.Members[j].Label)
		})
		out = append(out, v)
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(out)
}

func (a *App) handleTeamOp(w http.ResponseWriter, r *http.Request) {
	var body map[string]string
	if err := readJSON(r, &body); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	switch body["op"] {
	case "create", "join", "leave", "kick", "delete", "code":
	default:
		http.Error(w, "geçersiz işlem", 400)
		return
	}
	op := map[string]string{}
	for _, k := range []string{"op", "name", "code", "tid", "id"} {
		if v := body[k]; v != "" {
			op[k] = v
		}
	}
	a.mu.Lock()
	c := a.hubc
	a.mu.Unlock()
	if c == nil {
		http.Error(w, "Sunucuya bağlı değil.", 409)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	if err := c.TeamOp(ctx, op); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	writeOK(w)
}

// openForPeer: biri bu cihaza bağlanmak istedi. Tünel kapalıysa açar, adresini döner.
func (a *App) openForPeer(ctx context.Context) (string, error) {
	if !a.isOnline() {
		return "", errors.New("Karşı cihaz bağlantılara kapalı.")
	}
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
		busy := len(a.st.Sessions) > 0 || a.st.Pending != nil
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
	Open   bool   `json:"open"`            // bu cihaza açık bir bağlantı penceresi var
	Group  string `json:"group,omitempty"` // kullanıcının verdiği grup adı (ör. "Ofis")
}

type peerStore struct {
	mu    sync.Mutex
	path  string
	peers []Peer

	presence map[string]bool // sunucunun bildirdiği çevrimiçi durumu
}

func (p *peerStore) ids() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]string, 0, len(p.peers))
	for _, x := range p.peers {
		out = append(out, x.ID)
	}
	return out
}

func (p *peerStore) setPresence(s map[string]bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.presence == nil {
		p.presence = map[string]bool{}
	}
	for id, on := range s {
		p.presence[id] = on
	}
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
// Liste yeni bir cihaz eklendiyse true döner.
func (p *peerStore) touch(id string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	added := false
	if x := p.find(id); x != nil {
		x.Last = time.Now().UnixMilli()
	} else {
		p.peers = append(p.peers, Peer{ID: id, Last: time.Now().UnixMilli()})
		added = true
	}
	p.saveLocked()
	return added
}

func cleanPeerID(s string) string { return hub.NormalizeID(s) }

func (a *App) handlePeers(w http.ResponseWriter, r *http.Request) {
	// Yalnızca yerel bilgi: çevrimiçi durumu sunucu bağlantısı üzerinden zaten gelir,
	// burada sunucuya istek atılmaz (arayüz bu ucu sık sık okuyabilir).
	p := &a.peers
	p.mu.Lock()
	list := make([]Peer, len(p.peers))
	copy(list, p.peers)
	for i := range list {
		list[i].Online = p.presence[list[i].ID]
	}
	p.mu.Unlock()
	for i := range list {
		list[i].Open = a.viewers.isOpen(list[i].ID)
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
		Group  *string `json:"group"`
		Delete bool    `json:"delete"`
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
		}
		if body.Name != nil {
			x.Name = cleanLabel(*body.Name)
		}
		if body.Fav != nil {
			x.Fav = *body.Fav
		}
		if body.Group != nil {
			x.Group = cleanLabel(*body.Group)
		}
	}
	p.saveLocked()
	go a.watchPeers()
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
	if a.peers.touch(id) {
		go a.watchPeers()
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"url": url})
}
