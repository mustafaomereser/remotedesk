//go:build darwin

// Package sysutil, işletim sistemine özel küçük yardımcıları içerir.
package sysutil

import (
	"os"
	"os/exec"
	"strings"

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

// Permissions, Ekran Kaydı ve Erişilebilirlik izinlerini kontrol eder.
// Eksikse sistemin izin penceresini tetikler ve uyarı metni döner.
func Permissions() []string {
	cg, err := purego.Dlopen("/System/Library/Frameworks/CoreGraphics.framework/CoreGraphics", purego.RTLD_NOW|purego.RTLD_GLOBAL)
	if err != nil {
		return []string{"CoreGraphics yüklenemedi: " + err.Error()}
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
	var warns []string
	if pre, req := get("CGPreflightScreenCaptureAccess"), get("CGRequestScreenCaptureAccess"); pre != nil && !pre() {
		if req != nil {
			req()
		}
		warns = append(warns, "Ekran Kaydı izni yok: Sistem Ayarları › Gizlilik ve Güvenlik › Ekran ve Sistem Ses Kaydı bölümünden izin ver (Terminal'den çalıştırıyorsan Terminal'e), sonra uygulamayı yeniden başlat.")
	}
	if pre, req := get("CGPreflightPostEventAccess"), get("CGRequestPostEventAccess"); pre != nil && !pre() {
		if req != nil {
			req()
		}
		warns = append(warns, "Erişilebilirlik izni yok, fare/klavye kontrolü çalışmaz: Sistem Ayarları › Gizlilik ve Güvenlik › Erişilebilirlik bölümünden izin ver, sonra uygulamayı yeniden başlat.")
	}
	return warns
}
