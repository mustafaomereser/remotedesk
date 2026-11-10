package app

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"time"

	"remotedesk/internal/sysutil"
	"remotedesk/internal/update"
)

// UpdateInfo, arayüzde gösterilen güncelleme durumudur.
type UpdateInfo struct {
	Version string `json:"version"`
	URL     string `json:"url"`
	Status  string `json:"status"` // available | downloading | installing | error
	Msg     string `json:"msg"`
}

func contextWithTimeout(r *http.Request, d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(r.Context(), d)
}

// updateLoop, açılıştan kısa süre sonra ve 6 saatte bir GitHub'daki son sürüme bakar.
func (a *App) updateLoop(ctx context.Context) {
	if a.opts.UpdateRepo == "" || a.opts.Version == "" {
		return
	}
	if !sleepCtx(ctx, 10*time.Second) {
		return
	}
	for {
		a.checkUpdate(ctx)
		if !sleepCtx(ctx, 6*time.Hour) {
			return
		}
	}
}

func (a *App) checkUpdate(ctx context.Context) error {
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	r, err := update.Latest(cctx, a.opts.UpdateRepo)
	if err != nil {
		log.Printf("güncelleme denetlenemedi: %v", err)
		return err
	}
	if !update.Newer(r.Version, a.opts.Version) {
		return nil
	}
	log.Printf("yeni sürüm var: %s (bu: %s)", r.Version, a.opts.Version)
	a.mu.Lock()
	a.upd = r
	if a.st.Update == nil || a.st.Update.Status != "downloading" {
		a.st.Update = &UpdateInfo{Version: r.Version, URL: r.URL, Status: "available"}
		if !r.Installable() {
			a.st.Update.Msg = "Bu sistem için otomatik kurulum dosyası yok; sürüm sayfasından indir."
		}
	}
	a.broadcastLocked()
	a.mu.Unlock()
	return nil
}

func (a *App) handleUpdateCheck(w http.ResponseWriter, r *http.Request) {
	if a.opts.UpdateRepo == "" {
		http.Error(w, "Güncelleme denetimi kapalı.", 400)
		return
	}
	if err := a.checkUpdate(r.Context()); err != nil {
		http.Error(w, "GitHub'a ulaşılamadı.", 502)
		return
	}
	a.mu.Lock()
	has := a.st.Update != nil
	a.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"update":%t}`, has)
}

func (a *App) handleUpdateInstall(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	rel := a.upd
	busy := a.st.Update != nil && (a.st.Update.Status == "downloading" || a.st.Update.Status == "installing")
	a.mu.Unlock()
	if rel == nil {
		http.Error(w, "Güncelleme yok.", 400)
		return
	}
	if busy {
		writeOK(w)
		return
	}
	setStatus := func(st, msg string) {
		a.update(func(s *State) {
			s.Update = &UpdateInfo{Version: rel.Version, URL: rel.URL, Status: st, Msg: msg}
		})
	}
	setStatus("downloading", "İndiriliyor…")
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()
		last := time.Time{}
		err := rel.Install(ctx, func(done, total int64) {
			if time.Since(last) < 300*time.Millisecond {
				return
			}
			last = time.Now()
			msg := fmt.Sprintf("İndiriliyor… %.1f MB", float64(done)/1e6)
			if total > 0 {
				msg = fmt.Sprintf("İndiriliyor… %%%d", done*100/total)
			}
			setStatus("downloading", msg)
		})
		if err != nil {
			log.Printf("güncelleme kurulamadı: %v", err)
			setStatus("error", "Kurulamadı: "+err.Error())
			return
		}
		log.Printf("güncelleme kuruldu: %s, yeniden başlatılıyor", rel.Version)
		setStatus("installing", "Yeniden başlatılıyor…")
		if err := sysutil.Restart(); err != nil {
			setStatus("error", "Kuruldu; uygulamayı kapatıp yeniden aç.")
			return
		}
		time.Sleep(300 * time.Millisecond)
		a.Quit()
	}()
	writeOK(w)
}
