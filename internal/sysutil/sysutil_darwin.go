//go:build darwin

// Package sysutil, işletim sistemine özel küçük yardımcıları içerir.
package sysutil

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/ebitengine/purego"
)

func HideWindow(cmd *exec.Cmd) {}

// Alert, kullanıcıya bir hata penceresi gösterir (çift tıkla açılan uygulamada konsol yoktur).
func Alert(title, msg string) {
	esc := func(s string) string { return strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) }
	exec.Command("osascript", "-e",
		`display alert "`+esc(title)+`" message "`+esc(msg)+`" as critical`).Run()
}

func SetDPIAware() {}
func ConsoleUTF8() {}

// Attention macOS'ta şimdilik bir şey yapmaz (onay penceresi arayüzde gösterilir).
func Attention(title string) {}

// Job, Windows'taki job object'in macOS karşılığıdır (no-op).
type Job struct{}

func (j *Job) Close() {}

func KillWithParent(p *os.Process) (*Job, error) { return &Job{}, nil }

// ---- Ekran Kaydı / Erişilebilirlik izinleri ----

var (
	permOnce                   sync.Once
	preScreen, reqScreen       func() bool
	prePostEvent, reqPostEvent func() bool
)

func loadPerms() {
	permOnce.Do(func() {
		cg, err := purego.Dlopen("/System/Library/Frameworks/CoreGraphics.framework/CoreGraphics", purego.RTLD_NOW|purego.RTLD_GLOBAL)
		if err != nil {
			return
		}
		get := func(name string) func() bool {
			p, err := purego.Dlsym(cg, name)
			if err != nil {
				return nil
			}
			var f func() bool
			purego.RegisterFunc(&f, p)
			return f
		}
		preScreen, reqScreen = get("CGPreflightScreenCaptureAccess"), get("CGRequestScreenCaptureAccess")
		prePostEvent, reqPostEvent = get("CGPreflightPostEventAccess"), get("CGRequestPostEventAccess")
	})
}

// PermissionsNeeded, bu platformda izin verilmesi gerekip gerekmediğini söyler.
func PermissionsNeeded() bool { return true }

// PermissionStatus, Ekran Kaydı ve Erişilebilirlik (olay gönderme) izinlerinin durumunu döner.
// Not: macOS, izin verildikten sonra çoğu zaman uygulamanın yeniden başlatılmasını ister.
func PermissionStatus() (screen, input bool) {
	loadPerms()
	screen, input = true, true
	if preScreen != nil {
		screen = preScreen()
	}
	if prePostEvent != nil {
		input = prePostEvent()
	}
	return
}

// RequestPermissions, eksik izinler için macOS'un izin penceresini tetikler
// (uygulamayı Sistem Ayarları'ndaki listeye ekler).
func RequestPermissions() {
	screen, input := PermissionStatus()
	if !screen && reqScreen != nil {
		reqScreen()
	}
	if !input && reqPostEvent != nil {
		reqPostEvent()
	}
}

// OpenPermissionSettings, ilgili Sistem Ayarları sayfasını açar ("screen" veya "input").
func OpenPermissionSettings(kind string) error {
	pane := "Privacy_ScreenCapture"
	if kind == "input" {
		pane = "Privacy_Accessibility"
	}
	return exec.Command("open", "x-apple.systempreferences:com.apple.preference.security?"+pane).Start()
}

// appBundle, çalışan .app paketinin yolunu döner (paket dışından çalışıyorsa boş).
func appBundle() string {
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	exe, _ = filepath.EvalSymlinks(exe)
	app := filepath.Dir(filepath.Dir(filepath.Dir(exe))) // .../X.app/Contents/MacOS/X
	if strings.HasSuffix(app, ".app") {
		return app
	}
	return ""
}

// CanRestart, uygulamanın kendini yeniden başlatıp başlatamayacağını söyler.
func CanRestart() bool { return appBundle() != "" }

// Restart, bu süreç kapandıktan kısa süre sonra uygulamayı yeniden açar.
// Çağıran taraf ardından kendini kapatmalıdır.
func Restart() error {
	app := appBundle()
	if app == "" {
		return errors.New("uygulama paketi bulunamadı")
	}
	// Eski süreç tamamen bitmeden açmak LaunchServices'te -47 (dosya meşgul) hatası verebiliyor:
	// süreç çıkana kadar bekle, sonra aç.
	script := `i=0; while kill -0 "$1" 2>/dev/null && [ $i -lt 100 ]; do sleep 0.2; i=$((i+1)); done; sleep 1; open "$0"`
	return exec.Command("/bin/sh", "-c", script, app, strconv.Itoa(os.Getpid())).Start()
}
