//go:build windows

package app

import (
	"log"
	"path/filepath"

	"remotedesk/internal/frame"
	"remotedesk/web"
)

func init() {
	frame.Icon, _ = web.FS.ReadFile("icon.png")
}

// openNativeWindow, arayüzü uygulamanın kendi çerçevesiz penceresinde (WebView2) açar.
// WebView2 çalışma zamanı yoksa false döner (çağıran tarayıcıya düşer).
func openNativeWindow(url, dataDir string, onClose func()) bool {
	_, err := frame.Open(frame.Options{
		URL: url, Title: "RemoteDesk", W: 1000, H: 720, MinW: 420, MinH: 480,
		DataDir: filepath.Join(dataDir, "webview"), OnClose: onClose,
	})
	if err != nil {
		log.Printf("uygulama penceresi açılamadı: %v", err)
		return false
	}
	return true
}

func viewerDataDir(dataDir string) string { return filepath.Join(dataDir, "webview") }
