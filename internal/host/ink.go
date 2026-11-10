package host

import (
	"context"
	"fmt"
	"regexp"
	"sync"

	"remotedesk/internal/desktop"
)

// Ekrana çizim (toplantı / anlatım): bağlanan herkes ekranın üstüne kalemle çizebilir ya da
// lazer işaretçi kullanabilir. Çizgiler görüntüye gömülmez: her izleyici kendi ekranında
// çizer (keskin ve anında), bağlanılan bilgisayarda ise ekranda saydam bir katmanda görünür
// (bu katman ekran yakalamaya girmez, yani izleyicilerde iki kez görünmez).
//
// Koordinatlar izlenen monitöre göre 0..10000 arası tamsayıdır (çözünürlükten bağımsız).
//
//	izleyici -> {t:"dr", k, tool, col, wd, p:[x,y,...], e}   çizgi (parça parça gelir, e: bitti)
//	            {t:"drc"} hepsini sil   {t:"dru"} kendi son çizgini geri al
//	herkese  -> {t:"dr", id, tool, col, wd, d, p, e, n}       d: monitör sırası, n: çizen kişi
//	            {t:"drc"}  {t:"dru", id}

const (
	inkMaxStrokes = 400
	inkMaxPoints  = 6000 // çizgi başına (x,y çifti olarak değil, sayı olarak)
	inkMaxMsgPts  = 1000
)

var inkColor = regexp.MustCompile(`^#[0-9a-fA-F]{6}$`)

type inkStroke struct {
	ID    string `json:"id"`
	Owner int    `json:"-"`
	Tool  string `json:"tool"`
	Col   string `json:"col"`
	Wd    int    `json:"wd"`
	Disp  int    `json:"d"`
	Name  string `json:"n"`
	P     []int  `json:"p"`
	E     bool   `json:"e"`

	view viewDisplay
}

type inkStore struct {
	mu      sync.Mutex
	strokes []*inkStroke // kalıcı (kalem) çizgiler, eskiden yeniye
	byID    map[string]*inkStroke
}

func (st *inkStore) replay(ctx context.Context, sess *session) {
	st.mu.Lock()
	list := make([]map[string]any, 0, len(st.strokes))
	for _, k := range st.strokes {
		list = append(list, map[string]any{
			"t": "dr", "id": k.ID, "tool": k.Tool, "col": k.Col, "wd": k.Wd, "d": k.Disp, "n": k.Name,
			"p": append([]int(nil), k.P...), "e": true,
		})
	}
	st.mu.Unlock()
	for _, m := range list {
		if sess.c.WriteJSON(ctx, m) != nil {
			return
		}
	}
}

// handleInk, bir izleyiciden gelen çizim mesajını işler.
func (s *Server) handleInk(sess *session, m *message) {
	switch m.T {
	case "drc":
		s.ClearInk()
	case "dru":
		s.ink.mu.Lock()
		var id string
		for i := len(s.ink.strokes) - 1; i >= 0; i-- {
			if k := s.ink.strokes[i]; k.Owner == sess.sid {
				id = k.ID
				s.ink.strokes = append(s.ink.strokes[:i], s.ink.strokes[i+1:]...)
				delete(s.ink.byID, id)
				break
			}
		}
		s.ink.mu.Unlock()
		if id != "" {
			desktop.InkRemove(id)
			s.broadcast(map[string]any{"t": "dru", "id": id}, 0)
		}
	case "dr":
		v := sess.view.Load()
		if v == nil || (m.Tool != "pen" && m.Tool != "laser") || !inkColor.MatchString(m.Col) ||
			len(m.P)%2 != 0 || len(m.P) > inkMaxMsgPts {
			return
		}
		wd := min(max(m.Wd, 1), 40)
		pts := make([]int, len(m.P))
		for i, p := range m.P {
			pts[i] = min(max(p, 0), 10000)
		}
		id := fmt.Sprintf("%d:%d", sess.sid, m.K)

		s.ink.mu.Lock()
		k := s.ink.byID[id]
		if k == nil {
			k = &inkStroke{ID: id, Owner: sess.sid, Tool: m.Tool, Col: m.Col, Wd: wd, Disp: v.idx, Name: sess.name, view: *v}
			if m.Tool == "pen" {
				if s.ink.byID == nil {
					s.ink.byID = map[string]*inkStroke{}
				}
				if len(s.ink.strokes) >= inkMaxStrokes { // en eski çizgi düşer
					old := s.ink.strokes[0]
					s.ink.strokes = s.ink.strokes[1:]
					delete(s.ink.byID, old.ID)
					go desktop.InkRemove(old.ID)
				}
				s.ink.strokes = append(s.ink.strokes, k)
				s.ink.byID[id] = k
			}
		}
		if len(k.P)+len(pts) > inkMaxPoints {
			s.ink.mu.Unlock()
			return
		}
		if m.Tool == "pen" {
			k.P = append(k.P, pts...)
			k.E = m.E
		}
		disp, tool, col, name, view := k.Disp, k.Tool, k.Col, k.Name, k.view
		s.ink.mu.Unlock()

		desktop.InkAdd(id, tool == "laser", col, wd, view.d.X, view.d.Y, view.d.W, view.d.H, pts, m.E)
		s.broadcast(map[string]any{
			"t": "dr", "id": id, "tool": tool, "col": col, "wd": wd, "d": disp, "n": name, "p": pts, "e": m.E,
		}, sess.sid)
	}
}

// ClearInk, tüm çizimleri siler (izleyicilerde ve bu bilgisayarın ekranında).
func (s *Server) ClearInk() {
	s.ink.mu.Lock()
	had := len(s.ink.strokes) > 0
	s.ink.strokes = nil
	s.ink.byID = nil
	s.ink.mu.Unlock()
	desktop.InkClear()
	if had || len(s.live()) > 0 {
		s.broadcast(map[string]any{"t": "drc"}, 0)
	}
}
