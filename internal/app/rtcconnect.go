package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"remotedesk/internal/hub"
)

// handleRTCConnect: izleme penceresinin WebRTC teklifini hedef cihaza iletir, yanıtı döner.
//   - Kalıcı ID (sunuculu mod): buluşma sunucusu üzerinden (tek istek; tünel açılmaz).
//   - Tünel adresi / yerel adres: doğrudan o adresin /rtc ucuna.
//
// Kurulamazsa izleme penceresi bugünkü tünel yoluna düşer.
func (a *App) handleRTCConnect(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ID  string `json:"id"`
		SDP string `json:"sdp"`
	}
	if err := readJSON(r, &body); err != nil || body.SDP == "" || len(body.SDP) > 16000 {
		http.Error(w, "geçersiz istek", 400)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	var ans string
	var err error
	if id := hub.NormalizeID(body.ID); hub.ValidID(id) {
		if a.opts.Server == "" {
			http.Error(w, "Sunucu ayarlı değil.", 400)
			return
		}
		ans, err = hub.Signal(ctx, a.opts.Server, id, body.SDP)
		// Özel ID'ler tünel adresine (kelime-kelime-kelime) benzer: sunucuda yoksa tünel dene.
		if err != nil && strings.Contains(id, "-") {
			if a2, err2 := rtcViaTunnel(ctx, body.ID, body.SDP); err2 == nil {
				ans, err = a2, nil
			}
		}
	} else {
		ans, err = rtcViaTunnel(ctx, body.ID, body.SDP)
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"sdp": ans})
}

// rtcViaTunnel: "kelime-kelime-kelime" (trycloudflare) ya da "127.0.0.1:port" (yerel test).
func rtcViaTunnel(ctx context.Context, id, offer string) (string, error) {
	id = strings.TrimSpace(id)
	var base string
	switch {
	case strings.HasPrefix(id, "https://") || strings.HasPrefix(id, "http://"):
		base = strings.TrimRight(id, "/")
	case strings.HasPrefix(id, "127.0.0.1:") || strings.HasPrefix(id, "localhost:"):
		base = "http://" + id
	case strings.Contains(id, "."):
		base = "https://" + id
	default:
		base = "https://" + id + ".trycloudflare.com"
	}
	b, _ := json.Marshal(map[string]string{"sdp": offer})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/rtc", bytes.NewReader(b))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", errors.New("karşı cihaza ulaşılamadı")
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode != http.StatusOK {
		return "", errors.New("doğrudan bağlantı desteklenmiyor")
	}
	var out struct {
		SDP string `json:"sdp"`
	}
	if json.Unmarshal(data, &out) != nil || out.SDP == "" {
		return "", errors.New("geçersiz yanıt")
	}
	return out.SDP, nil
}
