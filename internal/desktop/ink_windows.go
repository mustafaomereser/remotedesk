//go:build windows

package desktop

import (
	"runtime"
	"strconv"
	"sync"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Ekrana çizim katmanı: tüm masaüstünü kaplayan, tıklamaları geçiren, her zaman üstte,
// saydam bir pencere. Ekran yakalamaya girmez (WDA_EXCLUDEFROMCAPTURE): izleyiciler
// çizgileri kendi ekranlarında zaten çiziyor, görüntüde ikinci kez görünmesin.

var (
	procGetSystemMetrics         = user32.NewProc("GetSystemMetrics")
	procSetWindowDisplayAffinity = user32.NewProc("SetWindowDisplayAffinity")
	procSetTimer                 = user32.NewProc("SetTimer")
	procKillTimer                = user32.NewProc("KillTimer")
	procExtCreatePen             = gdi32.NewProc("ExtCreatePen")
	procDeleteObject             = gdi32.NewProc("DeleteObject")
	procPolyline                 = gdi32.NewProc("Polyline")
	procCreateCompatibleDC       = gdi32.NewProc("CreateCompatibleDC")
	procCreateCompatibleBitmap   = gdi32.NewProc("CreateCompatibleBitmap")
	procDeleteDC                 = gdi32.NewProc("DeleteDC")
	procBitBlt                   = gdi32.NewProc("BitBlt")
	procEllipse                  = gdi32.NewProc("Ellipse")
)

const (
	wmTimer      = 0x0113
	wmAppInkSync = 0x8010
	inkLaserLife = 1500 * time.Millisecond
	inkKey       = 0x00010203 // saydam renk (çizgilerde kullanılmayacak kadar özel)
)

type inkLine struct {
	laser bool
	color uint32 // BGR
	width int
	pts   []point // pencere koordinatı
	ended time.Time
	done  bool
}

var (
	inkOnce  sync.Once
	inkHwnd  uintptr
	inkMu    sync.Mutex
	inkLines = map[string]*inkLine{}
	inkOrder []string
	inkVis   bool
)

func ensureInk() bool {
	inkOnce.Do(func() {
		ready := make(chan struct{})
		go func() {
			runtime.LockOSThread()
			createInk()
			close(ready)
			if inkHwnd == 0 {
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
	return inkHwnd != 0
}

func createInk() {
	inst, _, _ := procGetModuleHandleW.Call(0)
	cls, _ := windows.UTF16PtrFromString("RemoteDeskInk")
	wc := wndClassEx{WndProc: windows.NewCallback(inkProc), Instance: inst, ClassName: cls}
	wc.Size = uint32(unsafe.Sizeof(wc))
	procRegisterClassExW.Call(uintptr(unsafe.Pointer(&wc)))
	const (
		wsPopup         = 0x80000000
		wsExLayered     = 0x00080000
		wsExTransparent = 0x00000020
		wsExTopmost     = 0x00000008
		wsExToolWindow  = 0x00000080
		wsExNoActivate  = 0x08000000
	)
	h, _, _ := procCreateWindowExW.Call(
		wsExLayered|wsExTransparent|wsExTopmost|wsExToolWindow|wsExNoActivate,
		uintptr(unsafe.Pointer(cls)), 0, wsPopup, 0, 0, 10, 10, 0, 0, inst, 0)
	if h == 0 {
		return
	}
	procSetLayeredWindowAttributes.Call(h, inkKey, 255, 1) // LWA_COLORKEY
	if procSetWindowDisplayAffinity.Find() == nil {
		procSetWindowDisplayAffinity.Call(h, 0x11) // WDA_EXCLUDEFROMCAPTURE (Windows 10 2004+)
	}
	inkHwnd = h
}

func virtualScreen() (x, y, w, h int32) {
	m := func(i uintptr) int32 { r, _, _ := procGetSystemMetrics.Call(i); return int32(r) }
	return m(76), m(77), m(78), m(79) // SM_XVIRTUALSCREEN, SM_YVIRTUALSCREEN, SM_CXVIRTUALSCREEN, SM_CYVIRTUALSCREEN
}

func inkProc(hwnd, m, wp, lp uintptr) uintptr {
	switch m {
	case wmAppInkSync:
		inkMu.Lock()
		n := len(inkLines)
		laser := false
		for _, l := range inkLines {
			laser = laser || l.laser
		}
		inkMu.Unlock()
		if n == 0 {
			procShowWindow.Call(hwnd, 0)
			procKillTimer.Call(hwnd, 1)
			inkMu.Lock()
			inkVis = false
			inkMu.Unlock()
			return 0
		}
		inkMu.Lock()
		show := !inkVis
		inkVis = true
		inkMu.Unlock()
		if show {
			x, y, w, h := virtualScreen()
			const hwndTopmost = ^uintptr(0)
			procSetWindowPos.Call(hwnd, hwndTopmost, uintptr(x), uintptr(y), uintptr(w), uintptr(h), 0x10|0x40)
		}
		if laser {
			procSetTimer.Call(hwnd, 1, 50, 0)
		}
		procInvalidateRect.Call(hwnd, 0, 0)
		return 0
	case wmTimer:
		// Lazer izleri: bırakıldıktan bir süre sonra silinir.
		now := time.Now()
		inkMu.Lock()
		changed, laser := false, false
		for id, l := range inkLines {
			if l.laser && l.done && now.Sub(l.ended) > inkLaserLife {
				delete(inkLines, id)
				inkOrder = removeID(inkOrder, id)
				changed = true
			} else if l.laser {
				laser = true
			}
		}
		inkMu.Unlock()
		if !laser {
			procKillTimer.Call(hwnd, 1)
		}
		if changed {
			procPostMessageW.Call(hwnd, wmAppInkSync, 0, 0)
		}
		return 0
	case wmPaint:
		var ps paintStruct
		hdc, _, _ := procBeginPaint.Call(hwnd, uintptr(unsafe.Pointer(&ps)))
		var rc rect
		procGetClientRect.Call(hwnd, uintptr(unsafe.Pointer(&rc)))
		// Titremesin diye önce bellekte çizilir.
		mdc, _, _ := procCreateCompatibleDC.Call(hdc)
		bmp, _, _ := procCreateCompatibleBitmap.Call(hdc, uintptr(rc.R), uintptr(rc.B))
		oldBmp, _, _ := procSelectObject.Call(mdc, bmp)
		key, _, _ := procCreateSolidBrush.Call(inkKey)
		procFillRect.Call(mdc, uintptr(unsafe.Pointer(&rc)), key)
		procDeleteObject.Call(key)
		inkMu.Lock()
		for _, id := range inkOrder {
			l := inkLines[id]
			if l == nil || len(l.pts) == 0 {
				continue
			}
			drawInkLine(mdc, l)
		}
		inkMu.Unlock()
		procBitBlt.Call(hdc, 0, 0, uintptr(rc.R), uintptr(rc.B), mdc, 0, 0, 0x00CC0020) // SRCCOPY
		procSelectObject.Call(mdc, oldBmp)
		procDeleteObject.Call(bmp)
		procDeleteDC.Call(mdc)
		procEndPaint.Call(hwnd, uintptr(unsafe.Pointer(&ps)))
		return 0
	}
	r, _, _ := procDefWindowProcW.Call(hwnd, m, wp, lp)
	return r
}

func drawInkLine(dc uintptr, l *inkLine) {
	type logBrush struct {
		Style uint32
		Color uint32
		Hatch uintptr
	}
	lb := logBrush{Style: 0, Color: l.color}
	const psGeometric, psEndcapRound, psJoinRound = 0x00010000, 0, 0
	pen, _, _ := procExtCreatePen.Call(psGeometric|psEndcapRound|psJoinRound, uintptr(l.width), uintptr(unsafe.Pointer(&lb)), 0, 0)
	old, _, _ := procSelectObject.Call(dc, pen)
	if len(l.pts) == 1 {
		// Tek nokta: küçük daire
		br, _, _ := procCreateSolidBrush.Call(uintptr(l.color))
		ob, _, _ := procSelectObject.Call(dc, br)
		p, r := l.pts[0], int32(max(l.width/2, 1))
		procEllipse.Call(dc, uintptr(p.X-r), uintptr(p.Y-r), uintptr(p.X+r+1), uintptr(p.Y+r+1))
		procSelectObject.Call(dc, ob)
		procDeleteObject.Call(br)
	} else {
		procPolyline.Call(dc, uintptr(unsafe.Pointer(&l.pts[0])), uintptr(len(l.pts)))
	}
	procSelectObject.Call(dc, old)
	procDeleteObject.Call(pen)
}

func parseColor(c string) uint32 {
	if len(c) != 7 {
		return 0x003F4BEF
	}
	v, err := strconv.ParseUint(c[1:], 16, 32)
	if err != nil {
		return 0x003F4BEF
	}
	r, g, b := uint32(v>>16)&0xFF, uint32(v>>8)&0xFF, uint32(v)&0xFF
	bgr := b<<16 | g<<8 | r
	if bgr == inkKey {
		bgr++
	}
	return bgr
}

func inkAdd(id string, laser bool, color string, width int, dx, dy, dw, dh int, pts []int, end bool) {
	if !ensureInk() {
		return
	}
	vx, vy, _, _ := virtualScreen()
	inkMu.Lock()
	l := inkLines[id]
	if l == nil {
		// Kalınlık izleyicinin ekranına göre verilir (CSS pikseli); bu ekranda monitör
		// genişliğine oranla ölçeklenir.
		w := max(1, width*dw/1600)
		if laser {
			w = max(w, 3)
		}
		l = &inkLine{laser: laser, color: parseColor(color), width: w}
		inkLines[id] = l
		inkOrder = append(inkOrder, id)
	}
	for i := 0; i+1 < len(pts); i += 2 {
		x := int32(dx+pts[i]*dw/10000) - vx
		y := int32(dy+pts[i+1]*dh/10000) - vy
		l.pts = append(l.pts, point{x, y})
	}
	if end {
		l.done, l.ended = true, time.Now()
	}
	inkMu.Unlock()
	procPostMessageW.Call(inkHwnd, wmAppInkSync, 0, 0)
}

func inkRemove(id string) {
	if inkHwnd == 0 {
		return
	}
	inkMu.Lock()
	delete(inkLines, id)
	inkOrder = removeID(inkOrder, id)
	inkMu.Unlock()
	procPostMessageW.Call(inkHwnd, wmAppInkSync, 0, 0)
}

func inkClear() {
	if inkHwnd == 0 {
		return
	}
	inkMu.Lock()
	inkLines = map[string]*inkLine{}
	inkOrder = nil
	inkMu.Unlock()
	procPostMessageW.Call(inkHwnd, wmAppInkSync, 0, 0)
}

func removeID(list []string, id string) []string {
	for i, v := range list {
		if v == id {
			return append(list[:i], list[i+1:]...)
		}
	}
	return list
}
