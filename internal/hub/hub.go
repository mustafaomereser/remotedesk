// Package hub, buluşma sunucusuyla (server/ altındaki Cloudflare Worker) konuşur:
// kalıcı cihaz ID'si, çevrimiçi bilgisi ve "bana bağlanmak isteyen var, tünel aç" bildirimi.
// Görüntü ve şifre bu sunucudan geçmez.
package hub

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/coder/websocket"
)

var idRe = regexp.MustCompile(`^[1-9]\d{8}$`)

// ValidID, s kalıcı cihaz ID'si biçimindeyse (9 hane) true döner.
func ValidID(s string) bool { return idRe.MatchString(s) }

// Device, bu bilgisayarın sunucudaki kimliğidir. Secret yalnızca bu cihazda durur;
// sunucu özetini saklar.
type Device struct {
	Server string `json:"server"`
	ID     string `json:"id"`
	Secret string `json:"secret"`
}

var httpc = &http.Client{Timeout: 50 * time.Second}

func postJSON(ctx context.Context, url string, in, out any) error {
	b, _ := json.Marshal(in)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	return do(req, out)
}

func do(req *http.Request, out any) error {
	resp, err := httpc.Do(req)
	if err != nil {
		return errors.New("Sunucuya ulaşılamadı.")
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode != http.StatusOK {
		var e struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(body, &e) == nil && e.Error != "" {
			return errors.New(e.Error)
		}
		return fmt.Errorf("Sunucu hatası (%d).", resp.StatusCode)
	}
	return json.Unmarshal(body, out)
}

// LoadDevice, kayıtlı kimliği okur; yoksa (ya da başka sunucuya aitse) yenisini kaydeder.
func LoadDevice(ctx context.Context, dataDir, server string) (Device, error) {
	path := filepath.Join(dataDir, "device.json")
	var d Device
	if b, err := os.ReadFile(path); err == nil {
		if json.Unmarshal(b, &d) == nil && d.Server == server && ValidID(d.ID) && d.Secret != "" {
			return d, nil
		}
	}
	buf := make([]byte, 32)
	rand.Read(buf)
	d = Device{Server: server, Secret: hex.EncodeToString(buf)}
	var out struct {
		ID string `json:"id"`
	}
	if err := postJSON(ctx, server+"/register", map[string]string{"secret": d.Secret}, &out); err != nil {
		return Device{}, err
	}
	if !ValidID(out.ID) {
		return Device{}, errors.New("Sunucu geçersiz ID verdi.")
	}
	d.ID = out.ID
	b, _ := json.MarshalIndent(d, "", "  ")
	if err := os.WriteFile(path, b, 0o600); err != nil {
		return Device{}, err
	}
	return d, nil
}

// Status, verilen ID'lerin çevrimiçi olup olmadığını sorar.
func Status(ctx context.Context, server string, ids []string) (map[string]bool, error) {
	out := map[string]bool{}
	if len(ids) == 0 {
		return out, nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, server+"/status?ids="+strings.Join(ids, ","), nil)
	if err != nil {
		return nil, err
	}
	return out, do(req, &out)
}

// Connect, hedef cihazdan tünel açmasını ister ve tünel adresini döner.
func Connect(ctx context.Context, server, id string) (string, error) {
	var out struct {
		URL string `json:"url"`
	}
	if err := postJSON(ctx, server+"/connect", map[string]string{"id": id}, &out); err != nil {
		return "", err
	}
	return out.URL, nil
}

// Client, cihazın sunucuya açık tuttuğu bağlantıdır.
type Client struct {
	Dev Device
	// OnOpen, biri bağlanmak istediğinde çağrılır; tünel adresini (https://…) dönmelidir.
	OnOpen func(ctx context.Context) (string, error)
	// OnState, sunucu bağlantısı kurulduğunda/koptuğunda çağrılır.
	OnState func(online bool)
}

// Run, ctx bitene kadar bağlı kalır; koparsa artan aralıklarla yeniden bağlanır.
func (c *Client) Run(ctx context.Context) {
	wait := time.Second
	for ctx.Err() == nil {
		start := time.Now()
		err := c.session(ctx)
		c.OnState(false)
		if ctx.Err() != nil {
			return
		}
		if time.Since(start) > 30*time.Second {
			wait = time.Second
		}
		log.Printf("hub: bağlantı koptu (%v), %v sonra yeniden", err, wait)
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		wait = min(wait*2, 30*time.Second)
	}
}

func (c *Client) session(ctx context.Context) error {
	wsURL := "ws" + strings.TrimPrefix(c.Dev.Server, "http") + "/host?id=" + c.Dev.ID
	dctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	conn, _, err := websocket.Dial(dctx, wsURL, &websocket.DialOptions{
		HTTPHeader: http.Header{"Authorization": {"Bearer " + c.Dev.Secret}},
	})
	cancel()
	if err != nil {
		return err
	}
	defer conn.CloseNow()
	conn.SetReadLimit(4096)
	c.OnState(true)

	ctx, stop := context.WithCancel(ctx)
	defer stop()
	pong := make(chan struct{}, 1)
	// Sunucu "ping"e kendiliğinden "pong" döner; 75 sn yanıt gelmezse bağlantı ölü sayılır.
	go func() {
		t := time.NewTicker(30 * time.Second)
		defer t.Stop()
		last := time.Now()
		for {
			select {
			case <-ctx.Done():
				return
			case <-pong:
				last = time.Now()
			case <-t.C:
				if time.Since(last) > 75*time.Second {
					conn.CloseNow()
					return
				}
				wctx, c2 := context.WithTimeout(ctx, 10*time.Second)
				err := conn.Write(wctx, websocket.MessageText, []byte("ping"))
				c2()
				if err != nil {
					conn.CloseNow()
					return
				}
			}
		}
	}()

	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			return err
		}
		if string(data) == "pong" {
			select {
			case pong <- struct{}{}:
			default:
			}
			continue
		}
		var m struct {
			T   string `json:"t"`
			Req string `json:"req"`
		}
		if json.Unmarshal(data, &m) != nil || m.T != "open" || m.Req == "" {
			continue
		}
		go func(req string) {
			octx, c2 := context.WithTimeout(ctx, 35*time.Second)
			defer c2()
			reply := map[string]string{"t": "url", "req": req}
			if url, err := c.OnOpen(octx); err != nil {
				reply["err"] = err.Error()
			} else {
				reply["url"] = url
			}
			b, _ := json.Marshal(reply)
			wctx, c3 := context.WithTimeout(ctx, 10*time.Second)
			defer c3()
			conn.Write(wctx, websocket.MessageText, b)
		}(m.Req)
	}
}
