//go:build windows

package frame

import (
	"errors"
	"fmt"
	"os"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"unsafe"

	"github.com/jchv/go-webview2/pkg/edge"
	"golang.org/x/sys/windows"
)

var (
	user32     = windows.NewLazySystemDLL("user32.dll")
	dwmapi     = windows.NewLazySystemDLL("dwmapi.dll")
	shell32    = windows.NewLazySystemDLL("shell32.dll")
	gdi32      = windows.NewLazySystemDLL("gdi32.dll")
	pRegister  = user32.NewProc("RegisterClassExW")
	pCreate    = user32.NewProc("CreateWindowExW")
	pDefProc   = user32.NewProc("DefWindowProcW")
	pShow      = user32.NewProc("ShowWindow")
	pUpdate    = user32.NewProc("UpdateWindow")
	pGetMsg    = user32.NewProc("GetMessageW")
	pTrans     = user32.NewProc("TranslateMessage")
	pDispatch  = user32.NewProc("DispatchMessageW")
	pPostQuit  = user32.NewProc("PostQuitMessage")
	pDestroy   = user32.NewProc("DestroyWindow")
	pClient    = user32.NewProc("GetClientRect")
	pWinRect   = user32.NewProc("GetWindowRect")
	pSetPos    = user32.NewProc("SetWindowPos")
	pZoomed    = user32.NewProc("IsZoomed")
	pIconic    = user32.NewProc("IsIconic")
	pDpiWin    = user32.NewProc("GetDpiForWindow")
	pMetric    = user32.NewProc("GetSystemMetricsForDpi")
	pPostMsg   = user32.NewProc("PostMessageW")
	pSendMsg   = user32.NewProc("SendMessageW")
	pMonitor   = user32.NewProc("MonitorFromWindow")
	pMonInfo   = user32.NewProc("GetMonitorInfoW")
	pLoadCur   = user32.NewProc("LoadCursorW")
	pFgWin     = user32.NewProc("SetForegroundWindow")
	pSetText   = user32.NewProc("SetWindowTextW")
	pIconRes   = user32.NewProc("CreateIconFromResourceEx")
	pDwmAttr   = dwmapi.NewProc("DwmSetWindowAttribute")
	pShellEx   = shell32.NewProc("ShellExecuteW")
	pBrush     = gdi32.NewProc("CreateSolidBrush")
	classOnce  sync.Once
	classErr   error
	className  *uint16
	wndProcPtr uintptr

	winsMu sync.Mutex
	wins   = map[uintptr]*win{}
)

const (
	wmDestroy       = 0x0002
	wmMove          = 0x0003
	wmSize          = 0x0005
	wmActivate      = 0x0006
	wmClose         = 0x0010
	wmGetMinMaxInfo = 0x0024
	wmSetIcon       = 0x0080
	wmNCCalcSize    = 0x0083
	wmNCHitTest     = 0x0084
	wmMoving        = 0x0216
	wmDpiChanged    = 0x02E0
	wmAppFocus      = 0x8000 + 1

	htTop, htTopLeft, htTopRight = 12, 13, 14

	swShowNormal, swShow, swMinimize, swMaximize, swRestore = 1, 5, 6, 3, 9
)

// Arka plan (style.css --bg): WebView2 yüklenirken beyaz parlama olmasın.
const bgR, bgG, bgB = 0x11, 0x11, 0x13

// Icon, pencere simgesi (PNG). Uygulama açılışta ayarlar.
var Icon []byte

type rect struct{ Left, Top, Right, Bottom int32 }
type point struct{ X, Y int32 }
type minMax struct{ Reserved, MaxSize, MaxPosition, MinTrackSize, MaxTrackSize point }
type msg struct {
	Hwnd    uintptr
	Message uint32
	WParam  uintptr
	LParam  uintptr
	Time    uint32
	Pt      point
}
type ncCalc struct {
	Rgrc [3]rect
	Pos  uintptr
}
type monitorInfo struct {
	Size    uint32
	Monitor rect
	Work    rect
	Flags   uint32
}
type wndClass struct {
	Size       uint32
	Style      uint32
	WndProc    uintptr
	ClsExtra   int32
	WndExtra   int32
	Instance   windows.Handle
	Icon       uintptr
	Cursor     uintptr
	Background uintptr
	MenuName   *uint16
	ClassName  *uint16
	IconSm     uintptr
}

type win struct {
	hwnd      uintptr
	chromium  *edge.Chromium
	frameless bool
	opts      Options
	closed    bool
}

func (w *win) Focus() {
	winsMu.Lock()
	h, closed := w.hwnd, w.closed
	winsMu.Unlock()
	if !closed && h != 0 {
		pPostMsg.Call(h, wmAppFocus, 0, 0)
	}
}

func (w *win) Close() {
	winsMu.Lock()
	h, closed := w.hwnd, w.closed
	winsMu.Unlock()
	if !closed && h != 0 {
		pPostMsg.Call(h, wmClose, 0, 0)
	}
}

func ptrAt[T any](p uintptr) *T { return *(**T)(unsafe.Pointer(&p)) }

func comCall(obj uintptr, idx int, args ...uintptr) uintptr {
	vtbl := *ptrAt[uintptr](obj)
	fn := *ptrAt[uintptr](vtbl + uintptr(idx)*unsafe.Sizeof(uintptr(0)))
	r, _, _ := syscall.SyscallN(fn, append([]uintptr{obj}, args...)...)
	return r
}

func dpiScale(hwnd uintptr, v int) int {
	dpi, _, _ := pDpiWin.Call(hwnd)
	if dpi == 0 {
		dpi = 96
	}
	return v * int(dpi) / 96
}

// topInset: büyütülmemiş çerçevesiz pencerede WebView üst kenardan biraz aşağıda başlar;
// o şerit pencereye kalır ve üst kenardan boyutlandırma çalışır.
func (w *win) topInset() int32 {
	if z, _, _ := pZoomed.Call(w.hwnd); z != 0 || !w.frameless {
		return 0
	}
	return int32(dpiScale(w.hwnd, 4))
}

func (w *win) resize() {
	if w.chromium == nil {
		return
	}
	ctrl := w.chromium.GetController()
	if ctrl == nil {
		return
	}
	var r rect
	pClient.Call(w.hwnd, uintptr(unsafe.Pointer(&r)))
	r.Top += w.topInset()
	comCall(uintptr(unsafe.Pointer(ctrl)), 6, uintptr(unsafe.Pointer(&r))) // put_Bounds
}

// IID_ICoreWebView2Settings9 — put_IsNonClientRegionSupportEnabled (app-region: drag)
var iidSettings9 = windows.GUID{Data1: 0x0528A73B, Data2: 0xE92D, Data3: 0x49F4, Data4: [8]byte{0x92, 0x7A, 0xE5, 0x47, 0xDD, 0xDA, 0xA3, 0x7D}}

// enableNonClientRegions: ICoreWebView2Settings9 sanal tablosunda 37 get / 38 put.
func enableNonClientRegions(settings uintptr) bool {
	var s9 uintptr
	if hr := comCall(settings, 0, uintptr(unsafe.Pointer(&iidSettings9)), uintptr(unsafe.Pointer(&s9))); hr != 0 || s9 == 0 {
		return false
	}
	defer comCall(s9, 2)
	if hr := comCall(s9, 38, 1); hr != 0 {
		return false
	}
	var v int32
	return comCall(s9, 37, uintptr(unsafe.Pointer(&v))) == 0 && v != 0
}

func lookup(hwnd uintptr) *win {
	winsMu.Lock()
	defer winsMu.Unlock()
	return wins[hwnd]
}

func wndProc(hwnd, m, wp, lp uintptr) uintptr {
	w := lookup(hwnd)
	if w == nil {
		r, _, _ := pDefProc.Call(hwnd, m, wp, lp)
		return r
	}
	switch m {
	case wmNCCalcSize:
		if wp == 0 || !w.frameless {
			break
		}
		// Başlık çubuğunu kaldır: varsayılan hesaplamadan sonra üst kenarı geri al. Yan/alt
		// kenarlar (görünmez boyutlandırma alanı) ve DWM gölgesi kalır.
		p := ptrAt[ncCalc](lp)
		top := p.Rgrc[0].Top
		pDefProc.Call(hwnd, m, wp, lp)
		p.Rgrc[0].Top = top
		if z, _, _ := pZoomed.Call(hwnd); z != 0 {
			dpi, _, _ := pDpiWin.Call(hwnd)
			fy, _, _ := pMetric.Call(33, dpi) // SM_CYFRAME
			pb, _, _ := pMetric.Call(92, dpi) // SM_CXPADDEDBORDER
			p.Rgrc[0].Top += int32(fy + pb)
		}
		return 0
	case wmNCHitTest:
		r, _, _ := pDefProc.Call(hwnd, m, wp, lp)
		if r != 1 || !w.frameless {
			return r
		}
		var wr rect
		pWinRect.Call(hwnd, uintptr(unsafe.Pointer(&wr)))
		y := int32(int16(lp >> 16))
		x := int32(int16(lp & 0xffff))
		if z, _, _ := pZoomed.Call(hwnd); z == 0 && y < wr.Top+int32(dpiScale(hwnd, 5)) {
			edge := int32(dpiScale(hwnd, 12))
			switch {
			case x < wr.Left+edge:
				return htTopLeft
			case x > wr.Right-edge:
				return htTopRight
			}
			return htTop
		}
		return r
	case wmSize:
		w.resize()
		state := "normal"
		if z, _, _ := pZoomed.Call(hwnd); z != 0 {
			state = "max"
		}
		if w.chromium != nil && w.chromium.GetController() != nil {
			w.chromium.Eval("window.__rdWinState&&window.__rdWinState('" + state + "')")
		}
		return 0
	case wmMove, wmMoving:
		if w.chromium != nil && w.chromium.GetController() != nil {
			_ = w.chromium.NotifyParentWindowPositionChanged()
		}
	case wmActivate:
		if wp&0xffff != 0 && w.chromium != nil && w.chromium.GetController() != nil {
			w.chromium.Focus()
		}
	case wmAppFocus:
		if i, _, _ := pIconic.Call(hwnd); i != 0 {
			pShow.Call(hwnd, swRestore)
		}
		pFgWin.Call(hwnd)
		return 0
	case wmGetMinMaxInfo:
		mm := ptrAt[minMax](lp)
		mm.MinTrackSize = point{int32(dpiScale(hwnd, max(w.opts.MinW, 360))), int32(dpiScale(hwnd, max(w.opts.MinH, 300)))}
		return 0
	case wmDpiChanged:
		r := ptrAt[rect](lp)
		pSetPos.Call(hwnd, 0, uintptr(r.Left), uintptr(r.Top), uintptr(r.Right-r.Left), uintptr(r.Bottom-r.Top), 0x0014)
		return 0
	case wmClose:
		// WebView2'yi önce kapat: sayfadaki bağlantılar (WebSocket, ses) hemen kesilsin.
		if w.chromium != nil {
			if ctrl := w.chromium.GetController(); ctrl != nil {
				comCall(uintptr(unsafe.Pointer(ctrl)), 24) // ICoreWebView2Controller::Close
			}
		}
		pDestroy.Call(hwnd)
		return 0
	case wmDestroy:
		winsMu.Lock()
		w.closed = true
		delete(wins, hwnd)
		winsMu.Unlock()
		pPostQuit.Call(0)
		return 0
	}
	r, _, _ := pDefProc.Call(hwnd, m, wp, lp)
	return r
}

func (w *win) onMessage(m string) {
	h := w.hwnd
	switch {
	case m == "win:min":
		pShow.Call(h, swMinimize)
	case m == "win:max":
		if z, _, _ := pZoomed.Call(h); z != 0 {
			pShow.Call(h, swRestore)
		} else {
			pShow.Call(h, swMaximize)
		}
	case m == "win:close":
		pPostMsg.Call(h, wmClose, 0, 0)
	case strings.HasPrefix(m, "title:"):
		t, _ := windows.UTF16PtrFromString(strings.TrimPrefix(m, "title:"))
		pSetText.Call(h, uintptr(unsafe.Pointer(t)))
	case strings.HasPrefix(m, "open:"):
		// Dış bağlantılar varsayılan tarayıcıda açılır.
		u := strings.TrimPrefix(m, "open:")
		if strings.HasPrefix(u, "https://") {
			op, _ := windows.UTF16PtrFromString("open")
			up, _ := windows.UTF16PtrFromString(u)
			pShellEx.Call(0, uintptr(unsafe.Pointer(op)), uintptr(unsafe.Pointer(up)), 0, 0, 1)
		}
	}
}

func registerClass() error {
	classOnce.Do(func() {
		var inst windows.Handle
		_ = windows.GetModuleHandleEx(0, nil, &inst)
		className, _ = windows.UTF16PtrFromString("RemoteDeskWindow")
		cursor, _, _ := pLoadCur.Call(0, 32512) // IDC_ARROW
		brush, _, _ := pBrush.Call(uintptr(bgR) | uintptr(bgG)<<8 | uintptr(bgB)<<16)
		wndProcPtr = syscall.NewCallback(wndProc)
		wc := wndClass{
			Size: uint32(unsafe.Sizeof(wndClass{})), Style: 0x0003, // CS_HREDRAW|CS_VREDRAW
			WndProc: wndProcPtr, Instance: inst, Cursor: cursor, Background: brush, ClassName: className,
		}
		if r, _, err := pRegister.Call(uintptr(unsafe.Pointer(&wc))); r == 0 {
			classErr = fmt.Errorf("RegisterClassEx: %v", err)
		}
	})
	return classErr
}

// iconHandle, PNG baytlarından pencere simgesi üretir (Vista+ PNG simgeleri destekler).
func iconHandle(size int) uintptr {
	if len(Icon) == 0 {
		return 0
	}
	h, _, _ := pIconRes.Call(uintptr(unsafe.Pointer(&Icon[0])), uintptr(len(Icon)), 1, 0x00030000, uintptr(size), uintptr(size), 0)
	return h
}

// Open, pencereyi kendi iş parçacığında açar ve hazır olunca döner. WebView2 yoksa hata verir.
func Open(o Options) (Window, error) {
	if o.URL == "" {
		return nil, errors.New("adres yok")
	}
	type result struct {
		w   *win
		err error
	}
	res := make(chan result, 1)
	go func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		_ = windows.CoInitializeEx(0, windows.COINIT_APARTMENTTHREADED)
		w, err := create(o)
		res <- result{w, err}
		if err != nil {
			return
		}
		var m msg
		for {
			r, _, _ := pGetMsg.Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0)
			if r == 0 || int32(r) == -1 {
				break
			}
			pTrans.Call(uintptr(unsafe.Pointer(&m)))
			pDispatch.Call(uintptr(unsafe.Pointer(&m)))
		}
		if o.OnClose != nil {
			o.OnClose()
		}
	}()
	r := <-res
	if r.err != nil {
		return nil, r.err
	}
	return r.w, nil
}

func create(o Options) (*win, error) {
	if err := registerClass(); err != nil {
		return nil, err
	}
	var inst windows.Handle
	_ = windows.GetModuleHandleEx(0, nil, &inst)
	title, _ := windows.UTF16PtrFromString(o.Title)
	hwnd, _, err := pCreate.Call(0, uintptr(unsafe.Pointer(className)), uintptr(unsafe.Pointer(title)),
		0x00CF0000, // WS_OVERLAPPEDWINDOW (Aero Snap, gölge, sistem menüsü kalsın)
		0x80000000, 0x80000000, uintptr(o.W), uintptr(o.H), 0, 0, uintptr(inst), 0)
	if hwnd == 0 {
		return nil, fmt.Errorf("CreateWindowEx: %v", err)
	}
	w := &win{hwnd: hwnd, opts: o}
	winsMu.Lock()
	wins[hwnd] = w
	winsMu.Unlock()

	if big := iconHandle(32); big != 0 {
		pSendMsg.Call(hwnd, wmSetIcon, 1, big)
	}
	if small := iconHandle(16); small != 0 {
		pSendMsg.Call(hwnd, wmSetIcon, 0, small)
	}
	place(hwnd, o.W, o.H)
	dark := int32(1)
	pDwmAttr.Call(hwnd, 20, uintptr(unsafe.Pointer(&dark)), 4) // DWMWA_USE_IMMERSIVE_DARK_MODE

	c := edge.NewChromium()
	c.DataPath = o.DataDir
	c.MessageCallback = w.onMessage
	c.SetPermission(edge.CoreWebView2PermissionKindClipboardRead, edge.CoreWebView2PermissionStateAllow)
	w.chromium = c
	if !c.Embed(hwnd) {
		winsMu.Lock()
		delete(wins, hwnd)
		winsMu.Unlock()
		pDestroy.Call(hwnd)
		return nil, errors.New("WebView2 başlatılamadı")
	}
	if ctrl := c.GetController(); ctrl != nil {
		if c2 := ctrl.GetICoreWebView2Controller2(); c2 != nil {
			_ = c2.PutDefaultBackgroundColor(edge.COREWEBVIEW2_COLOR{A: 255, R: bgR, G: bgG, B: bgB})
		}
	}
	nonClient := false
	if s, err := c.GetSettings(); err == nil && s != nil {
		_ = s.PutAreDefaultContextMenusEnabled(true) // giriş kutularında kes/kopyala/yapıştır
		_ = s.PutAreDevToolsEnabled(os.Getenv("RD_DEVTOOLS") == "1")
		_ = s.PutIsStatusBarEnabled(false)
		nonClient = enableNonClientRegions(uintptr(unsafe.Pointer(s)))
	}
	// Sürükleme desteği varsa başlık çubuğunu kaldır (yoksa Windows çerçevesi kalır).
	w.frameless = nonClient
	pSetPos.Call(hwnd, 0, 0, 0, 0, 0, 0x0027) // FRAMECHANGED|NOMOVE|NOSIZE|NOZORDER
	c.Init(fmt.Sprintf("window.__RD_FRAME={host:'webview2',drag:%t};", nonClient))
	w.resize()
	c.Navigate(o.URL)
	pShow.Call(hwnd, swShow)
	pShow.Call(hwnd, swShowNormal)
	pUpdate.Call(hwnd)
	_ = c.Show()
	pFgWin.Call(hwnd)
	return w, nil
}

// place, pencereyi açıldığı ekranın çalışma alanında ortalar (DPI ölçekli, sığmazsa küçültür).
func place(hwnd uintptr, w, h int) {
	w, h = dpiScale(hwnd, w), dpiScale(hwnd, h)
	mon, _, _ := pMonitor.Call(hwnd, 2)
	mi := monitorInfo{Size: uint32(unsafe.Sizeof(monitorInfo{}))}
	pMonInfo.Call(mon, uintptr(unsafe.Pointer(&mi)))
	ww, wh := int(mi.Work.Right-mi.Work.Left), int(mi.Work.Bottom-mi.Work.Top)
	w, h = min(w, ww-40), min(h, wh-40)
	x := int(mi.Work.Left) + (ww-w)/2
	y := int(mi.Work.Top) + (wh-h)/2
	pSetPos.Call(hwnd, 0, uintptr(x), uintptr(y), uintptr(w), uintptr(h), 0x0014)
}
