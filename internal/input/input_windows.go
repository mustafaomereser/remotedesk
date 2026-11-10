//go:build windows

package input

import (
	"sync"
	"unsafe"

	"remotedesk/internal/desktop"

	"golang.org/x/sys/windows"
)

var (
	user32               = windows.NewLazySystemDLL("user32.dll")
	procSendInput        = user32.NewProc("SendInput")
	procGetSystemMetrics = user32.NewProc("GetSystemMetrics")
)

func metric(i uintptr) int {
	r, _, _ := procGetSystemMetrics.Call(i)
	return int(int32(r))
}

const (
	inputMouse    = 0
	inputKeyboard = 1

	mouseMove       = 0x0001
	mouseLeftDown   = 0x0002
	mouseLeftUp     = 0x0004
	mouseRightDown  = 0x0008
	mouseRightUp    = 0x0010
	mouseMiddleDown = 0x0020
	mouseMiddleUp   = 0x0040
	mouseXDown      = 0x0080
	mouseXUp        = 0x0100
	mouseWheel      = 0x0800
	mouseHWheel     = 0x1000
	mouseVirtual    = 0x4000
	mouseAbsolute   = 0x8000

	keyExtended = 0x0001
	keyUp       = 0x0002
	keyScancode = 0x0008
)

type mouseInput struct {
	Dx, Dy    int32
	MouseData uint32
	Flags     uint32
	Time      uint32
	Extra     uintptr
}

type keybdInput struct {
	Vk, Scan uint16
	Flags    uint32
	Time     uint32
	Extra    uintptr
}

// rawInput, Windows INPUT yapısının 64-bit düzenidir (40 bayt).
type rawInput struct {
	Type uint32
	_    uint32
	Mi   mouseInput
}

type rawKeyInput struct {
	Type uint32
	_    uint32
	Ki   keybdInput
	_    [8]byte
}

func send(inputs []rawInput) {
	if len(inputs) == 0 {
		return
	}
	procSendInput.Call(uintptr(len(inputs)), uintptr(unsafe.Pointer(&inputs[0])), unsafe.Sizeof(inputs[0]))
}

func sendKey(ki keybdInput) {
	desktop.MarkInjected()
	in := rawKeyInput{Type: inputKeyboard, Ki: ki}
	procSendInput.Call(1, uintptr(unsafe.Pointer(&in)), unsafe.Sizeof(in))
}

// Injector, bir oturumdaki basılı tuş/düğme durumunu da takip eder,
// böylece bağlantı koparsa hiçbir tuş basılı kalmaz.
type Injector struct {
	mu      sync.Mutex
	m       Mapping
	keys    map[string]bool
	buttons map[int]bool
	bg      bgState // simülasyon modu durumu
}

func New() *Injector {
	return &Injector{keys: map[string]bool{}, buttons: map[int]bool{}}
}

func (in *Injector) SetMapping(m Mapping) {
	in.mu.Lock()
	in.m = m
	in.mu.Unlock()
}

// moveInput, görüntü koordinatını tüm sanal masaüstüne (tüm monitörler) göre normalize eder.
func (in *Injector) moveInput(x, y int) rawInput {
	dx, dy := in.m.apply(x, y)
	vx, vy := metric(76), metric(77) // SM_XVIRTUALSCREEN, SM_YVIRTUALSCREEN
	vw, vh := metric(78), metric(79) // SM_CXVIRTUALSCREEN, SM_CYVIRTUALSCREEN
	if vw < 2 || vh < 2 {
		vw, vh = 2, 2
	}
	desktop.MarkPointer(int(dx), int(dy))
	nx := (dx - float64(vx)) * 65535 / float64(vw-1)
	ny := (dy - float64(vy)) * 65535 / float64(vh-1)
	return rawInput{Type: inputMouse, Mi: mouseInput{
		Dx:    int32(max(0, min(65535, nx))),
		Dy:    int32(max(0, min(65535, ny))),
		Flags: mouseMove | mouseAbsolute | mouseVirtual,
	}}
}

func (in *Injector) Move(x, y int) {
	in.mu.Lock()
	defer in.mu.Unlock()
	send([]rawInput{in.moveInput(x, y)})
}

// Button: b tarayıcının MouseEvent.button değeridir (0 sol, 1 orta, 2 sağ, 3 geri, 4 ileri).
func (in *Injector) Button(b int, down bool, x, y int) {
	in.mu.Lock()
	defer in.mu.Unlock()
	if in.buttons[b] == down {
		// Aynı durumu tekrar gönderme (ör. çift "up").
		if !down {
			return
		}
	}
	ev, ok := buttonInput(b, down)
	if !ok {
		return
	}
	in.buttons[b] = down
	send([]rawInput{in.moveInput(x, y), ev})
}

func buttonInput(b int, down bool) (rawInput, bool) {
	var flags, data uint32
	switch b {
	case 0:
		flags = pick(down, mouseLeftDown, mouseLeftUp)
	case 1:
		flags = pick(down, mouseMiddleDown, mouseMiddleUp)
	case 2:
		flags = pick(down, mouseRightDown, mouseRightUp)
	case 3:
		flags, data = pick(down, mouseXDown, mouseXUp), 1
	case 4:
		flags, data = pick(down, mouseXDown, mouseXUp), 2
	default:
		return rawInput{}, false
	}
	return rawInput{Type: inputMouse, Mi: mouseInput{Flags: flags, MouseData: data}}, true
}

func pick(c bool, a, b uint32) uint32 {
	if c {
		return a
	}
	return b
}

// Wheel: dy pozitif = aşağı kaydırma (tarayıcı yönü), 120 = bir çentik.
func (in *Injector) Wheel(dx, dy int) {
	in.mu.Lock()
	defer in.mu.Unlock()
	desktop.MarkInjected()
	var evs []rawInput
	if dy != 0 {
		evs = append(evs, rawInput{Type: inputMouse, Mi: mouseInput{Flags: mouseWheel, MouseData: uint32(int32(-dy))}})
	}
	if dx != 0 {
		evs = append(evs, rawInput{Type: inputMouse, Mi: mouseInput{Flags: mouseHWheel, MouseData: uint32(int32(dx))}})
	}
	send(evs)
}

// Key: code tarayıcının KeyboardEvent.code değeridir (fiziksel tuş).
// Scancode gönderildiği için karakter, uzak bilgisayarın klavye düzenine göre oluşur.
func (in *Injector) Key(code string, down bool) {
	in.mu.Lock()
	defer in.mu.Unlock()
	in.key(code, down)
}

func (in *Injector) key(code string, down bool) {
	if sc, ok := scanCodes[code]; ok {
		flags := uint32(keyScancode)
		if sc&0xE000 != 0 {
			flags |= keyExtended
		}
		if !down {
			flags |= keyUp
		}
		sendKey(keybdInput{Scan: sc & 0xFF, Flags: flags})
	} else if vk, ok := vkCodes[code]; ok {
		var flags uint32
		if vk.ext {
			flags |= keyExtended
		}
		if !down {
			flags |= keyUp
		}
		sendKey(keybdInput{Vk: vk.vk, Flags: flags})
	} else {
		return
	}
	if down {
		in.keys[code] = true
	} else {
		delete(in.keys, code)
	}
}

// ReleaseAll, basılı kalmış tüm tuş ve düğmeleri bırakır.
func (in *Injector) ReleaseAll() {
	in.mu.Lock()
	defer in.mu.Unlock()
	for code := range in.keys {
		in.key(code, false)
	}
	for b, down := range in.buttons {
		if down {
			if ev, ok := buttonInput(b, false); ok {
				send([]rawInput{ev})
			}
			in.buttons[b] = false
		}
	}
}
