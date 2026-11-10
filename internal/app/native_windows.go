//go:build windows

package app

import (
	"log"
	"path/filepath"
	"runtime"

	webview2 "github.com/jchv/go-webview2"
)

// openNativeWindow, arayüzü uygulamanın kendi penceresinde (WebView2) açar.
// WebView2 çalışma zamanı yoksa false döner (çağıran tarayıcıya düşer).
// window.open ile açılan viewer pencerelerini WebView2 kendisi oluşturur.
func openNativeWindow(url, dataDir string, onClose func()) bool {
	res := make(chan bool, 1)
	go func() {
		runtime.LockOSThread()
		w := webview2.NewWithOptions(webview2.WebViewOptions{
			DataPath:  filepath.Join(dataDir, "webview"),
			AutoFocus: true,
			WindowOptions: webview2.WindowOptions{
				Title:  "RemoteDesk",
				Width:  980,
				Height: 700,
				Center: true,
			},
		})
		if w == nil {
			log.Printf("WebView2 başlatılamadı")
			res <- false
			return
		}
		w.Navigate(url)
		res <- true
		w.Run()
		onClose()
	}()
	return <-res
}
