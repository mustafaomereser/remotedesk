//go:build darwin

package frame

import (
	"errors"

	"remotedesk/internal/desktop"
)

// Icon: macOS'ta pencere simgesi uygulama paketinden gelir (kullanılmaz).
var Icon []byte

type macWin struct{ h uintptr }

func (w macWin) Focus() { desktop.FocusWebWindow(w.h) }
func (w macWin) Close() { desktop.CloseWebWindow(w.h) }

// Open, ikincil pencere açar (ana pencere app paketinde desktop.OpenAppWindow ile açılır).
func Open(o Options) (Window, error) {
	h, ok := desktop.OpenWebWindow(o.URL, o.Title, float64(o.W), float64(o.H), o.OnClose)
	if !ok {
		return nil, errors.New("pencere açılamadı")
	}
	return macWin{h}, nil
}
