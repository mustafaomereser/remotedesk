//go:build windows

package desktop

import (
	"runtime"
	"sync"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	user32   = windows.NewLazySystemDLL("user32.dll")
	gdi32    = windows.NewLazySystemDLL("gdi32.dll")
	kernel32 = windows.NewLazySystemDLL("kernel32.dll")

	procGetCursorPos               = user32.NewProc("GetCursorPos")
	procGetLastInputInfo           = user32.NewProc("GetLastInputInfo")
	procRegisterClassExW           = user32.NewProc("RegisterClassExW")
	procCreateWindowExW            = user32.NewProc("CreateWindowExW")
	procDefWindowProcW             = user32.NewProc("DefWindowProcW")
	procGetMessageW                = user32.NewProc("GetMessageW")
	procTranslateMessage           = user32.NewProc("TranslateMessage")
	procDispatchMessageW           = user32.NewProc("DispatchMessageW")
	procPostMessageW               = user32.NewProc("PostMessageW")
	procSetWindowPos               = user32.NewProc("SetWindowPos")
	procShowWindow                 = user32.NewProc("ShowWindow")
	procBeginPaint                 = user32.NewProc("BeginPaint")
	procEndPaint                   = user32.NewProc("EndPaint")
	procGetClientRect              = user32.NewProc("GetClientRect")
	procFillRect                   = user32.NewProc("FillRect")
	procDrawTextW                  = user32.NewProc("DrawTextW")
	procInvalidateRect             = user32.NewProc("InvalidateRect")
	procGetDC                      = user32.NewProc("GetDC")
	procReleaseDC                  = user32.NewProc("ReleaseDC")
	procSetLayeredWindowAttributes = user32.NewProc("SetLayeredWindowAttributes")
	procGetDpiForSystem            = user32.NewProc("GetDpiForSystem")

	procCreateSolidBrush      = gdi32.NewProc("CreateSolidBrush")
	procCreateFontW           = gdi32.NewProc("CreateFontW")
	procSelectObject          = gdi32.NewProc("SelectObject")
	procSetTextColor          = gdi32.NewProc("SetTextColor")
	procSetBkMode             = gdi32.NewProc("SetBkMode")
	procGetTextExtentPoint32W = gdi32.NewProc("GetTextExtentPoint32W")
	procGetTickCount          = kernel32.NewProc("GetTickCount")
	procGetModuleHandleW      = kernel32.NewProc("GetModuleHandleW")
)

// RunMain, Windows'ta özel bir şey gerektirmez.
func RunMain(fn func()) { fn() }

type point struct{ X, Y int32 }
type rect struct{ L, T, R, B int32 }

func cursorPos() (int, int, bool) {
	var p point
	if r, _, _ := procGetCursorPos.Call(uintptr(unsafe.Pointer(&p))); r == 0 {
		return 0, 0, false
	}
	return int(p.X), int(p.Y), true
}

func lastInputAge() time.Duration {
	var li struct{ Size, Time uint32 }
	li.Size = 8
	if r, _, _ := procGetLastInputInfo.Call(uintptr(unsafe.Pointer(&li))); r == 0 {
		return time.Hour
	}
	now, _, _ := procGetTickCount.Call()
	return time.Duration(uint32(now)-li.Time) * time.Millisecond
}

// ---- ad etiketi: tıklamaları geçiren, her zaman üstte, odak almayan katmanlı pencere ----

const (
	wmPaint   = 0x000F
	wmAppShow = 0x8000
	wmAppHide = 0x8001
)

var (
	ovOnce  sync.Once
	ovHwnd  uintptr
	ovMu    sync.Mutex
	ovText  []uint16
	ovFont  uintptr
	ovBrush uintptr
)

type wndClassEx struct {
	Size       uint32
	Style      uint32
	WndProc    uintptr
	ClsExtra   int32
	WndExtra   int32
	Instance   uintptr
	Icon       uintptr
	Cursor     uintptr
	Background uintptr
	MenuName   *uint16
	ClassName  *uint16
	IconSm     uintptr
}

type msg struct {
	Hwnd    uintptr
	Message uint32
	WParam  uintptr
	LParam  uintptr
	Time    uint32
	Pt      point
	Private uint32
}

type paintStruct struct {
	Hdc      uintptr
	Erase    int32
	Paint    rect
	Restore  int32
	IncUpd   int32
	Reserved [32]byte
}

func ensureOverlay() bool {
	ovOnce.Do(func() {
		ready := make(chan struct{})
		go func() {
			runtime.LockOSThread() // pencere ve mesaj döngüsü aynı iş parçacığında olmalı
			createOverlay()
			close(ready)
			if ovHwnd == 0 {
				return
			}
			var m msg
			for {
				r, _, _ := procGetMessageW.Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0)
				if int32(r) <= 0 {
					return
				}
				procTranslateMessage.Call(uintptr(unsafe.Pointer(&m)))
				procDispatchMessageW.Call(uintptr(unsafe.Pointer(&m)))
			}
		}()
		<-ready
	})
	return ovHwnd != 0
}

func createOverlay() {
	inst, _, _ := procGetModuleHandleW.Call(0)
	cls, _ := windows.UTF16PtrFromString("RemoteDeskLabel")
	wc := wndClassEx{WndProc: windows.NewCallback(overlayProc), Instance: inst, ClassName: cls}
	wc.Size = uint32(unsafe.Sizeof(wc))
	procRegisterClassExW.Call(uintptr(unsafe.Pointer(&wc)))

	const (
		wsPopup         = 0x80000000
		wsExLayered     = 0x00080000
		wsExTransparent = 0x00000020 // tıklamalar alttaki pencereye geçer
		wsExTopmost     = 0x00000008
		wsExToolWindow  = 0x00000080 // görev çubuğunda görünmez
		wsExNoActivate  = 0x08000000 // odak çalmaz
	)
	h, _, _ := procCreateWindowExW.Call(
		wsExLayered|wsExTransparent|wsExTopmost|wsExToolWindow|wsExNoActivate,
		uintptr(unsafe.Pointer(cls)), 0, wsPopup, 0, 0, 10, 10, 0, 0, inst, 0)
	if h == 0 {
		return
	}
	procSetLayeredWindowAttributes.Call(h, 0, 235, 2) // LWA_ALPHA

	dpi := uintptr(96)
	if procGetDpiForSystem.Find() == nil {
		if d, _, _ := procGetDpiForSystem.Call(); d > 0 {
			dpi = d
		}
	}
	face, _ := windows.UTF16PtrFromString("Segoe UI")
	height := -int32(10 * dpi / 72) // 10pt
	ovFont, _, _ = procCreateFontW.Call(uintptr(height), 0, 0, 0, 700, 0, 0, 0, 1, 0, 0, 5, 0, uintptr(unsafe.Pointer(face)))
	ovBrush, _, _ = procCreateSolidBrush.Call(0x003F4BEF) // #EF4B3F (BGR)
	ovHwnd = h
}

func overlayProc(hwnd, m, wp, lp uintptr) uintptr {
	switch m {
	case wmPaint:
		var ps paintStruct
		hdc, _, _ := procBeginPaint.Call(hwnd, uintptr(unsafe.Pointer(&ps)))
		var rc rect
		procGetClientRect.Call(hwnd, uintptr(unsafe.Pointer(&rc)))
		procFillRect.Call(hdc, uintptr(unsafe.Pointer(&rc)), ovBrush)
		procSelectObject.Call(hdc, ovFont)
		procSetBkMode.Call(hdc, 1) // TRANSPARENT
		procSetTextColor.Call(hdc, 0x00FFFFFF)
		ovMu.Lock()
		if len(ovText) > 1 {
			procDrawTextW.Call(hdc, uintptr(unsafe.Pointer(&ovText[0])), uintptr(len(ovText)-1),
				uintptr(unsafe.Pointer(&rc)), 0x1|0x4|0x20) // CENTER|VCENTER|SINGLELINE
		}
		ovMu.Unlock()
		procEndPaint.Call(hwnd, uintptr(unsafe.Pointer(&ps)))
		return 0
	case wmAppShow:
		x, y := int32(int64(wp)), int32(int64(lp))
		w, h := textSize(hwnd)
		const hwndTopmost = ^uintptr(0)                                                                     // -1
		procSetWindowPos.Call(hwnd, hwndTopmost, uintptr(x), uintptr(y), uintptr(w), uintptr(h), 0x10|0x40) // NOACTIVATE|SHOWWINDOW
		procInvalidateRect.Call(hwnd, 0, 1)
		return 0
	case wmAppHide:
		procShowWindow.Call(hwnd, 0)
		return 0
	}
	r, _, _ := procDefWindowProcW.Call(hwnd, m, wp, lp)
	return r
}

func textSize(hwnd uintptr) (int32, int32) {
	hdc, _, _ := procGetDC.Call(hwnd)
	defer procReleaseDC.Call(hwnd, hdc)
	procSelectObject.Call(hdc, ovFont)
	var sz struct{ CX, CY int32 }
	ovMu.Lock()
	if len(ovText) > 1 {
		procGetTextExtentPoint32W.Call(hdc, uintptr(unsafe.Pointer(&ovText[0])), uintptr(len(ovText)-1), uintptr(unsafe.Pointer(&sz)))
	}
	ovMu.Unlock()
	return sz.CX + sz.CY, sz.CY + sz.CY/2 // yatay/dikey boşluk yazı yüksekliğiyle orantılı
}

func showLabel(name string, x, y int) {
	if !ensureOverlay() {
		return
	}
	t, _ := windows.UTF16FromString(name)
	ovMu.Lock()
	ovText = t
	ovMu.Unlock()
	// İmlecin sağ altı (imleç ucu kutunun üstünde kalsın)
	procPostMessageW.Call(ovHwnd, wmAppShow, uintptr(int64(x+14)), uintptr(int64(y+22)))
}

func hideLabel() {
	if ovHwnd != 0 {
		procPostMessageW.Call(ovHwnd, wmAppHide, 0, 0)
	}
}
