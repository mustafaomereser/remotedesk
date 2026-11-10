//go:build windows

package input

import (
	"time"
	"unsafe"
)

// Simülasyon modu ("aynı anda kullanım"): gerçek imleci oynatmadan, olayları doğrudan
// hedef pencereye mesaj olarak gönderir (PostMessage). Yerel kullanıcının imleci ve odağı
// değişmez. Klasik Win32 uygulamalarında çalışır; Chrome/Electron, oyunlar, kısayollar,
// sürükle-bırak ve yönetici olarak çalışan pencerelerde çalışmaz.

var (
	procWindowFromPoint = user32.NewProc("WindowFromPoint")
	procScreenToClient  = user32.NewProc("ScreenToClient")
	procPostMessageW    = user32.NewProc("PostMessageW")
	procMapVirtualKeyW  = user32.NewProc("MapVirtualKeyW")
)

const (
	wmMouseMove   = 0x0200
	wmLButtonDown = 0x0201
	wmLButtonUp   = 0x0202
	wmLButtonDbl  = 0x0203
	wmRButtonDown = 0x0204
	wmRButtonUp   = 0x0205
	wmMButtonDown = 0x0207
	wmMButtonUp   = 0x0208
	wmMouseWheel  = 0x020A
	wmKeyDown     = 0x0100
	wmKeyUp       = 0x0101
	wmChar        = 0x0102

	mkLButton = 0x0001
	mkRButton = 0x0002
	mkMButton = 0x0010
)

type bgState struct {
	target  uintptr         // klavyenin gideceği pencere (son tıklanan)
	down    map[int]uintptr // basılı düğme -> o anki pencere (bırakma aynı pencereye)
	lastBtn int
	lastAt  time.Time
	lastX   int32
	lastY   int32
}

type winPoint struct{ X, Y int32 }

func windowAt(x, y int32) uintptr {
	h, _, _ := procWindowFromPoint.Call(uintptr(uint32(x)) | uintptr(uint32(y))<<32)
	return h
}

func clientLParam(hwnd uintptr, x, y int32) uintptr {
	p := winPoint{x, y}
	procScreenToClient.Call(hwnd, uintptr(unsafe.Pointer(&p)))
	return uintptr(uint16(int16(p.X))) | uintptr(uint16(int16(p.Y)))<<16
}

func post(hwnd, m, wp, lp uintptr) {
	procPostMessageW.Call(hwnd, m, wp, lp)
}

func (in *Injector) bgMK() uintptr {
	var mk uintptr
	if _, ok := in.bg.down[0]; ok {
		mk |= mkLButton
	}
	if _, ok := in.bg.down[2]; ok {
		mk |= mkRButton
	}
	if _, ok := in.bg.down[1]; ok {
		mk |= mkMButton
	}
	return mk
}

func (in *Injector) bgPoint(x, y int) (int32, int32) {
	dx, dy := in.m.apply(x, y)
	return int32(dx), int32(dy)
}

// BgMove: basılı düğme varsa (sürükleme) o pencereye, yoksa alttaki pencereye hareket bildirir.
func (in *Injector) BgMove(x, y int) {
	in.mu.Lock()
	defer in.mu.Unlock()
	px, py := in.bgPoint(x, y)
	hwnd := windowAt(px, py)
	for _, h := range in.bg.down {
		hwnd = h
		break
	}
	if hwnd != 0 {
		post(hwnd, wmMouseMove, in.bgMK(), clientLParam(hwnd, px, py))
	}
}

func (in *Injector) BgButton(b int, down bool, x, y int) {
	in.mu.Lock()
	defer in.mu.Unlock()
	if in.bg.down == nil {
		in.bg.down = map[int]uintptr{}
	}
	px, py := in.bgPoint(x, y)
	var downMsg, upMsg uintptr
	switch b {
	case 0:
		downMsg, upMsg = wmLButtonDown, wmLButtonUp
	case 2:
		downMsg, upMsg = wmRButtonDown, wmRButtonUp
	case 1:
		downMsg, upMsg = wmMButtonDown, wmMButtonUp
	default:
		return
	}
	if down {
		hwnd := windowAt(px, py)
		if hwnd == 0 {
			return
		}
		lp := clientLParam(hwnd, px, py)
		post(hwnd, wmMouseMove, in.bgMK(), lp)
		in.bg.down[b] = hwnd
		in.bg.target = hwnd
		msg := downMsg
		// Aynı yere 500 ms içinde ikinci sol tık: çift tıklama mesajı
		now := time.Now()
		if b == 0 && in.bg.lastBtn == 0 && now.Sub(in.bg.lastAt) < 500*time.Millisecond &&
			abs32(px-in.bg.lastX) < 5 && abs32(py-in.bg.lastY) < 5 {
			msg = wmLButtonDbl
			in.bg.lastAt = time.Time{}
		} else {
			in.bg.lastBtn, in.bg.lastAt, in.bg.lastX, in.bg.lastY = b, now, px, py
		}
		post(hwnd, msg, in.bgMK(), lp)
		return
	}
	hwnd, ok := in.bg.down[b]
	if !ok {
		return
	}
	delete(in.bg.down, b)
	post(hwnd, upMsg, in.bgMK(), clientLParam(hwnd, px, py))
}

func abs32(v int32) int32 {
	if v < 0 {
		return -v
	}
	return v
}

// BgWheel: tekerlek mesajı ekran koordinatlarıyla alttaki pencereye gider.
func (in *Injector) BgWheel(dx, dy, x, y int) {
	in.mu.Lock()
	defer in.mu.Unlock()
	if dy == 0 {
		return
	}
	px, py := in.bgPoint(x, y)
	hwnd := windowAt(px, py)
	if hwnd == 0 {
		return
	}
	wp := uintptr(uint16(int16(-dy)))<<16 | in.bgMK()
	lp := uintptr(uint16(int16(px))) | uintptr(uint16(int16(py)))<<16
	post(hwnd, wmMouseWheel, wp, lp)
}

// BgKey: yazılabilir karakterler WM_CHAR olarak (viewer'ın klavye düzeniyle) gider;
// Enter, Backspace, oklar gibi tuşlar WM_KEYDOWN/UP olarak. Kısayollar çalışmaz
// (hedef uygulama Ctrl'ün gerçekten basılı olup olmadığına bakar).
func (in *Injector) BgKey(code, ch string, down bool) {
	in.mu.Lock()
	defer in.mu.Unlock()
	hwnd := in.bg.target
	if hwnd == 0 {
		return
	}
	if ch != "" {
		if down {
			for _, r := range ch {
				if r > 0xFFFF { // UTF-16 vekil çifti
					r -= 0x10000
					post(hwnd, wmChar, uintptr(0xD800+(r>>10)), 1)
					post(hwnd, wmChar, uintptr(0xDC00+(r&0x3FF)), 1)
				} else {
					post(hwnd, wmChar, uintptr(r), 1)
				}
			}
		}
		return
	}
	sc, ok := scanCodes[code]
	if !ok {
		return
	}
	ext := sc&0xE000 != 0
	scan := uintptr(sc & 0xFF)
	mapIn := scan
	if ext {
		mapIn |= 0xE000
	}
	vk, _, _ := procMapVirtualKeyW.Call(mapIn, 3) // MAPVK_VSC_TO_VK_EX
	if vk == 0 {
		return
	}
	lp := uintptr(1) | scan<<16
	if ext {
		lp |= 1 << 24
	}
	if down {
		post(hwnd, wmKeyDown, vk, lp)
	} else {
		post(hwnd, wmKeyUp, vk, lp|1<<30|1<<31)
	}
}
