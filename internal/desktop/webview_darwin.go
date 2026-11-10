//go:build darwin

package desktop

import (
	"log"
	"sync"
	"time"

	"github.com/ebitengine/purego"
	"github.com/ebitengine/purego/objc"
)

// Uygulamanın kendi penceresi: NSWindow + WKWebView (tarayıcı gerekmez).

var (
	wvOnce     sync.Once
	wvOK       bool
	wvDelegate uintptr
	wvQuit     func()

	msgInitWV func(id, sel uintptr, r cgRect, cfg uintptr) uintptr
	msgID3    func(id, sel, a, b, c uintptr) uintptr

	wvMu      sync.Mutex
	wvWindows = map[uintptr]wvEntry{} // NSWindow -> pencere bilgisi
)

type wvEntry struct {
	view uintptr
	main bool
}

func loadWebKit() bool {
	wvOnce.Do(func() {
		if _, err := purego.Dlopen("/System/Library/Frameworks/WebKit.framework/WebKit", purego.RTLD_NOW|purego.RTLD_GLOBAL); err != nil {
			log.Printf("webview: WebKit yüklenemedi: %v", err)
			return
		}
		if class("WKWebView") == 0 || msgSendAddr == 0 {
			return
		}
		purego.RegisterFunc(&msgInitWV, msgSendAddr)
		purego.RegisterFunc(&msgID3, msgSendAddr)

		cls, err := objc.RegisterClass("RDWebDelegate", objc.GetClass("NSObject"), nil, nil, []objc.MethodDef{
			{Cmd: objc.RegisterName("windowWillClose:"), Fn: wvWindowWillClose},
			{Cmd: objc.RegisterName("webView:createWebViewWithConfiguration:forNavigationAction:windowFeatures:"), Fn: wvCreatePopup},
			{Cmd: objc.RegisterName("webViewDidClose:"), Fn: wvDidClose},
			{Cmd: objc.RegisterName("rdQuit:"), Fn: wvQuitAction},
		})
		if err != nil {
			log.Printf("webview: %v", err)
			return
		}
		wvDelegate = msgID(msgID(uintptr(cls), sel("alloc")), sel("init"))
		setupMenu()
		wvOK = true
	})
	return wvOK
}

// OpenAppWindow, url'yi uygulamanın kendi penceresinde açar. Pencere kapanınca onClose çağrılır.
// WebKit/AppKit kullanılamıyorsa false döner (çağıran tarayıcıya düşer).
func OpenAppWindow(url, title string, onClose func()) bool {
	// RunMain, AppKit'i ana iş parçacığında fn ile aynı anda hazırlar: hazır olmasını bekle.
	select {
	case <-mainReadyCh:
	case <-time.After(5 * time.Second):
		log.Printf("webview: AppKit hazır değil")
		return false
	}
	res := make(chan bool, 1)
	onMain(func() {
		if !loadWebKit() {
			log.Printf("webview: WebKit kullanılamıyor")
			res <- false
			return
		}
		wvQuit = onClose
		app := msgID(class("NSApplication"), sel("sharedApplication"))
		msgInt(app, sel("setActivationPolicy:"), 0) // normal uygulama: Dock ikonu + menü
		cfg := msgID(msgID(class("WKWebViewConfiguration"), sel("alloc")), sel("init"))
		win, _ := newWebWindow(cfg, title, 980, 700, true)
		msgID1(win, sel("center"), 0)
		u := msgID1(class("NSURL"), sel("URLWithString:"), nsString(url))
		view := wvWindows[win].view
		msgID1(view, sel("loadRequest:"), msgID1(class("NSURLRequest"), sel("requestWithURL:"), u))
		msgID1(win, sel("makeKeyAndOrderFront:"), 0)
		msgBool(app, sel("activateIgnoringOtherApps:"), true)
		res <- true
	})
	select {
	case ok := <-res:
		return ok
	case <-time.After(5 * time.Second):
		log.Printf("webview: pencere zamanında açılamadı")
		return false
	}
}

// newWebWindow ana iş parçacığında çağrılır.
func newWebWindow(cfg uintptr, title string, w, h float64, main bool) (win, view uintptr) {
	const (
		titled, closable, miniaturizable, resizable = 1, 2, 4, 8
		buffered                                    = 2
	)
	r := cgRect{Size: cgSize{w, h}}
	win = msgInitWin(msgID(class("NSWindow"), sel("alloc")), sel("initWithContentRect:styleMask:backing:defer:"),
		r, titled|closable|miniaturizable|resizable, buffered, false)
	msgBool(win, sel("setReleasedWhenClosed:"), false)
	msgID1(win, sel("setTitle:"), nsString(title))
	msgID1(win, sel("setDelegate:"), wvDelegate)

	prefs := msgID(cfg, sel("preferences"))
	if prefs != 0 {
		msgBool(prefs, sel("setJavaScriptCanOpenWindowsAutomatically:"), true)
	}
	view = msgInitWV(msgID(class("WKWebView"), sel("alloc")), sel("initWithFrame:configuration:"), r, cfg)
	msgInt(view, sel("setAutoresizingMask:"), 2|16) // genişlik + yükseklik esnek
	msgID1(view, sel("setUIDelegate:"), wvDelegate)
	msgID1(win, sel("setContentView:"), view)

	wvMu.Lock()
	wvWindows[win] = wvEntry{view: view, main: main}
	wvMu.Unlock()
	return win, view
}

// window.open: aynı yapılandırmayla yeni pencere (window.opener korunur).
func wvCreatePopup(self objc.ID, _ objc.SEL, webView, cfg, action, features objc.ID) objc.ID {
	win, view := newWebWindow(uintptr(cfg), "RemoteDesk · Bağlantı", 1280, 800, false)
	msgID1(win, sel("center"), 0)
	msgID1(win, sel("makeKeyAndOrderFront:"), 0)
	return objc.ID(view)
}

// JS'den window.close()
func wvDidClose(self objc.ID, _ objc.SEL, webView objc.ID) {
	wvMu.Lock()
	var target uintptr
	for win, e := range wvWindows {
		if e.view == uintptr(webView) {
			target = win
		}
	}
	wvMu.Unlock()
	if target != 0 {
		msgID1(target, sel("performClose:"), 0)
	}
}

func wvWindowWillClose(self objc.ID, _ objc.SEL, note objc.ID) {
	win := msgID(uintptr(note), sel("object"))
	wvMu.Lock()
	e, ok := wvWindows[win]
	delete(wvWindows, win)
	wvMu.Unlock()
	if !ok {
		return
	}
	if e.main {
		if wvQuit != nil {
			go wvQuit()
		}
		return
	}
	// Viewer penceresi: web görünümünü bırak ki bağlantı (WebSocket) hemen kapansın.
	msgID1(win, sel("setContentView:"), 0)
	msgID(e.view, sel("release"))
}

func wvQuitAction(self objc.ID, _ objc.SEL, sender objc.ID) {
	if wvQuit != nil {
		go wvQuit()
	}
}

// setupMenu, Cmd+C/V/X/A/Z/Q/W kısayollarının web görünümünde çalışması için ana menüyü kurar.
func setupMenu() {
	app := msgID(class("NSApplication"), sel("sharedApplication"))
	newMenu := func(title string) uintptr {
		return msgID1(msgID(class("NSMenu"), sel("alloc")), sel("initWithTitle:"), nsString(title))
	}
	item := func(title, action, key string) uintptr {
		return msgID3(msgID(class("NSMenuItem"), sel("alloc")), sel("initWithTitle:action:keyEquivalent:"),
			nsString(title), sel(action), nsString(key))
	}
	sub := func(parent, menu uintptr) {
		it := msgID(msgID(class("NSMenuItem"), sel("alloc")), sel("init"))
		msgID1(it, sel("setSubmenu:"), menu)
		msgID1(parent, sel("addItem:"), it)
	}

	bar := newMenu("")
	appMenu := newMenu("RemoteDesk")
	msgID1(appMenu, sel("addItem:"), item("Pencereyi Kapat", "performClose:", "w"))
	quit := item("RemoteDesk'ten Çık", "rdQuit:", "q")
	msgID1(quit, sel("setTarget:"), wvDelegate)
	msgID1(appMenu, sel("addItem:"), quit)
	sub(bar, appMenu)

	edit := newMenu("Düzen")
	for _, it := range [][3]string{
		{"Geri Al", "undo:", "z"},
		{"Kes", "cut:", "x"},
		{"Kopyala", "copy:", "c"},
		{"Yapıştır", "paste:", "v"},
		{"Tümünü Seç", "selectAll:", "a"},
	} {
		msgID1(edit, sel("addItem:"), item(it[0], it[1], it[2]))
	}
	sub(bar, edit)
	msgID1(app, sel("setMainMenu:"), bar)
}
