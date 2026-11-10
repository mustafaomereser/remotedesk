// Package frame, uygulamanın kendi pencerelerini (ana pencere ve bağlantı pencereleri) açar.
//
// Windows: çerçevesiz WebView2 penceresi. Windows'un başlık çubuğu yoktur; üst bar sayfanın
// kendi HTML'idir ve WebView2'nin "non-client region" desteğiyle (CSS app-region: drag)
// başlık çubuğu gibi davranır: sürükle, çift tıkla büyüt, sağ tıkta sistem menüsü, Aero Snap.
// Sayfaya window.__RD_FRAME = {drag: true} eklenir; pencere düğmeleri sayfadan
// window.chrome.webview.postMessage("win:min" | "win:max" | "win:close") ile istenir.
//
// macOS: NSWindow + WKWebView; başlık çubuğu saydam ve koyu (trafik ışıkları yerinde kalır).
package frame

// Options, açılacak pencerenin ayarlarıdır.
type Options struct {
	URL        string
	Title      string
	W, H       int // nokta (DPI'ya göre ölçeklenir)
	MinW, MinH int
	DataDir    string // WebView2 profil klasörü (Windows)
	OnClose    func() // pencere kapandığında (kullanıcı ya da Close)
}

// Window açık bir penceredir.
type Window interface {
	Focus()
	Close()
}
