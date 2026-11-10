//go:build darwin

package app

import "remotedesk/internal/desktop"

// openNativeWindow, arayüzü uygulamanın kendi penceresinde (WKWebView) açar.
func openNativeWindow(url, dataDir string, onClose func()) bool {
	return desktop.OpenAppWindow(url, "RemoteDesk", onClose)
}
