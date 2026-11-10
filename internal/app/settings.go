package app

import (
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"remotedesk/internal/host"
	"remotedesk/internal/hub"
	"remotedesk/internal/sysutil"
)

// Settings, kullanıcının kalıcı ayarlarıdır (veri klasöründe settings.json).
type Settings struct {
	OnlineAtStart  bool `json:"onlineAtStart"`  // uygulama açılınca gelen bağlantılara açık
	AutoAccept     bool `json:"autoAccept"`     // tek kullanımlık şifreyle gelene de onay sorma
	Concurrent     bool `json:"concurrent"`     // aynı anda kullanım
	AllowControl   bool `json:"allowControl"`   // bağlanan kişi fare/klavye kullanabilir
	AllowClipboard bool `json:"allowClipboard"` // pano paylaşımı
	AllowBrowser   bool `json:"allowBrowser"`   // tünel adresi tarayıcıda açılabilir (şifreyle)

	// Sabit şifre (gözetimsiz erişim): yalnızca PBKDF2 özeti saklanır.
	FixedHash string `json:"fixedHash,omitempty"`
	FixedSalt string `json:"fixedSalt,omitempty"`
}

func defaultSettings() Settings {
	return Settings{OnlineAtStart: true, AllowControl: true, AllowClipboard: true}
}

func (a *App) settingsPath() string { return filepath.Join(a.opts.DataDir, "settings.json") }

func (a *App) loadSettings() Settings {
	s := defaultSettings()
	if b, err := os.ReadFile(a.settingsPath()); err == nil {
		if err := json.Unmarshal(b, &s); err != nil {
			log.Printf("ayarlar okunamadı, varsayılanlar kullanılıyor: %v", err)
			s = defaultSettings()
		}
	}
	return s
}

// saveSettingsLocked, a.mu tutulurken çağrılır.
func (a *App) saveSettingsLocked() {
	b, _ := json.MarshalIndent(a.set, "", "  ")
	if err := os.WriteFile(a.settingsPath(), b, 0o600); err != nil {
		log.Printf("ayarlar kaydedilemedi: %v", err)
	}
}

// syncStateLocked, ayarların arayüze gösterilen kısmını State'e yansıtır.
func (a *App) syncStateLocked() {
	a.st.OnlineAtStart = a.set.OnlineAtStart
	a.st.AutoAccept = a.set.AutoAccept
	a.st.Concurrent = a.set.Concurrent
	a.st.AllowControl = a.set.AllowControl
	a.st.AllowClipboard = a.set.AllowClipboard
	a.st.FixedPassword = a.set.FixedHash != ""
	a.st.AllowBrowser = a.set.AllowBrowser
}

func fixedHash(pw, saltHex string) string {
	salt, _ := hex.DecodeString(saltHex)
	k, _ := pbkdf2.Key(sha256.New, pw, salt, 120_000, 32)
	return hex.EncodeToString(k)
}

// checkPassword: önce tek kullanımlık şifre, sonra sabit şifre (gözetimsiz erişim).
func (a *App) checkPassword(pw string) (ok, unattended bool) {
	a.mu.Lock()
	want := a.st.Password
	h, salt := a.set.FixedHash, a.set.FixedSalt
	a.mu.Unlock()
	if subtle.ConstantTimeCompare([]byte(pw), []byte(want)) == 1 {
		return true, false
	}
	if h != "" && pw != "" && subtle.ConstantTimeCompare([]byte(fixedHash(pw, salt)), []byte(h)) == 1 {
		return true, true
	}
	return false, false
}

func (a *App) browserAllowed() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.set.AllowBrowser
}

func (a *App) permissions() host.Perms {
	a.mu.Lock()
	defer a.mu.Unlock()
	return host.Perms{Control: a.set.AllowControl, Clipboard: a.set.AllowClipboard}
}

// setOnline, gelen bağlantılara açık/kapalı durumunu değiştirir.
// Sunuculu modda sunucu bağlantısını, sunucusuz modda tüneli açar/kapatır.
func (a *App) setOnline(on bool) {
	a.mu.Lock()
	changed := a.st.Online != on
	a.st.Online = on
	a.broadcastLocked()
	a.mu.Unlock()
	if !changed {
		return
	}
	select {
	case a.onlineCh <- struct{}{}:
	default:
	}
	if a.opts.Server == "" || !on {
		a.tunnelAuto.Store(false)
		select {
		case a.tunnelReq <- on:
		default:
		}
	}
	if !on {
		a.host.Kick()
	}
}

func (a *App) isOnline() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.st.Online
}

// ---- API ----

func (a *App) handleSettings(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Online         *bool `json:"online"`
		OnlineAtStart  *bool `json:"onlineAtStart"`
		AutoAccept     *bool `json:"autoAccept"`
		Concurrent     *bool `json:"concurrent"`
		AllowControl   *bool `json:"allowControl"`
		AllowClipboard *bool `json:"allowClipboard"`
		AllowBrowser   *bool `json:"allowBrowser"`
	}
	if err := readJSON(r, &body); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	a.mu.Lock()
	set := func(dst *bool, v *bool) {
		if v != nil {
			*dst = *v
		}
	}
	set(&a.set.OnlineAtStart, body.OnlineAtStart)
	set(&a.set.AutoAccept, body.AutoAccept)
	set(&a.set.Concurrent, body.Concurrent)
	set(&a.set.AllowControl, body.AllowControl)
	set(&a.set.AllowClipboard, body.AllowClipboard)
	set(&a.set.AllowBrowser, body.AllowBrowser)
	a.saveSettingsLocked()
	a.syncStateLocked()
	a.broadcastLocked()
	a.mu.Unlock()
	if body.Online != nil {
		a.setOnline(*body.Online)
	}
	writeOK(w)
}

func (a *App) handleFixedPassword(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Password string `json:"password"` // boş: sabit şifreyi kaldır
	}
	if err := readJSON(r, &body); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	if body.Password != "" && len([]rune(body.Password)) < 8 {
		http.Error(w, "Sabit şifre en az 8 karakter olmalı.", 400)
		return
	}
	var h, saltHex string
	if body.Password != "" {
		salt := make([]byte, 16)
		rand.Read(salt)
		saltHex = hex.EncodeToString(salt)
		h = fixedHash(body.Password, saltHex)
	}
	a.mu.Lock()
	a.set.FixedHash, a.set.FixedSalt = h, saltHex
	a.saveSettingsLocked()
	a.syncStateLocked()
	a.broadcastLocked()
	a.mu.Unlock()
	writeOK(w)
}

func (a *App) handleRename(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ID string `json:"id"` // boş: rastgele yeni ID
	}
	if err := readJSON(r, &body); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	newID := hub.NormalizeID(body.ID)
	if newID != "" && !hub.ValidID(newID) {
		http.Error(w, "ID harfle başlamalı; küçük harf, rakam ve tire içerebilir (4-24 karakter).", 400)
		return
	}
	a.mu.Lock()
	dev := a.dev
	a.mu.Unlock()
	if dev.ID == "" {
		http.Error(w, "Sunucuya bağlı değil.", 400)
		return
	}
	ctx, cancel := contextWithTimeout(r, 20*time.Second)
	defer cancel()
	id, err := hub.Rename(ctx, dev, newID)
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	dev.ID = id
	if err := dev.Save(a.opts.DataDir); err != nil {
		log.Printf("yeni ID kaydedilemedi: %v", err)
	}
	a.mu.Lock()
	a.dev = dev
	a.st.DeviceID = id
	a.broadcastLocked()
	a.mu.Unlock()
	log.Printf("cihaz ID değişti: %s", id)
	select {
	case a.onlineCh <- struct{}{}: // sunucu bağlantısını yeni ID ile yeniden kur
	default:
	}
	writeOK(w)
}

func (a *App) handleElevate(w http.ResponseWriter, r *http.Request) {
	if !sysutil.CanElevate() {
		http.Error(w, "Zaten yönetici olarak çalışıyor.", 400)
		return
	}
	if err := sysutil.RestartElevated(); err != nil {
		http.Error(w, errors.New("Yönetici izni verilmedi.").Error(), 400)
		return
	}
	writeOK(w)
	go func() { time.Sleep(300 * time.Millisecond); a.Quit() }()
}
