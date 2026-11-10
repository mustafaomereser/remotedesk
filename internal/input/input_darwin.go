//go:build darwin

package input

import (
	"log"
	"math"
	"sync"
	"time"

	"github.com/ebitengine/purego"

	"remotedesk/internal/desktop"
)

type cgPoint struct{ X, Y float64 }

var (
	loadOnce sync.Once
	loadOK   bool

	cgEventCreateMouseEvent        func(source uintptr, typ uint32, pos cgPoint, button uint32) uintptr
	cgEventCreateKeyboardEvent     func(source uintptr, key uint16, down bool) uintptr
	cgEventCreateScrollWheelEvent2 func(source uintptr, units uint32, count uint32, w1, w2, w3 int32) uintptr
	cgEventSetFlags                func(ev uintptr, flags uint64)
	cgEventSetIntegerValueField    func(ev uintptr, field uint32, v int64)
	cgEventPost                    func(tap uint32, ev uintptr)
	cfRelease                      func(ref uintptr)
)

func load() bool {
	loadOnce.Do(func() {
		cg, err := purego.Dlopen("/System/Library/Frameworks/CoreGraphics.framework/CoreGraphics", purego.RTLD_NOW|purego.RTLD_GLOBAL)
		if err != nil {
			log.Printf("CoreGraphics yüklenemedi: %v", err)
			return
		}
		cf, err := purego.Dlopen("/System/Library/Frameworks/CoreFoundation.framework/CoreFoundation", purego.RTLD_NOW|purego.RTLD_GLOBAL)
		if err != nil {
			log.Printf("CoreFoundation yüklenemedi: %v", err)
			return
		}
		syms := []struct {
			lib  uintptr
			fn   any
			name string
		}{
			{cg, &cgEventCreateMouseEvent, "CGEventCreateMouseEvent"},
			{cg, &cgEventCreateKeyboardEvent, "CGEventCreateKeyboardEvent"},
			{cg, &cgEventSetFlags, "CGEventSetFlags"},
			{cg, &cgEventSetIntegerValueField, "CGEventSetIntegerValueField"},
			{cg, &cgEventPost, "CGEventPost"},
			{cf, &cfRelease, "CFRelease"},
		}
		for _, s := range syms {
			p, err := purego.Dlsym(s.lib, s.name)
			if err != nil {
				log.Printf("%s bulunamadı: %v", s.name, err)
				return
			}
			purego.RegisterFunc(s.fn, p)
		}
		// macOS 13+; yoksa sadece kaydırma çalışmaz.
		if p, err := purego.Dlsym(cg, "CGEventCreateScrollWheelEvent2"); err == nil {
			purego.RegisterFunc(&cgEventCreateScrollWheelEvent2, p)
		} else {
			log.Printf("CGEventCreateScrollWheelEvent2 yok (macOS 13 öncesi), kaydırma devre dışı")
		}
		loadOK = true
	})
	return loadOK
}

const (
	evLeftDown     = 1
	evLeftUp       = 2
	evRightDown    = 3
	evRightUp      = 4
	evMouseMoved   = 5
	evLeftDragged  = 6
	evRightDragged = 7
	evOtherDown    = 25
	evOtherUp      = 26
	evOtherDragged = 27

	fieldClickState = 1
	hidEventTap     = 0
	scrollPixel     = 0

	flagShift   = 0x00020000
	flagControl = 0x00040000
	flagOption  = 0x00080000
	flagCommand = 0x00100000
)

var modifierFlags = map[string]uint64{
	"ShiftLeft": flagShift, "ShiftRight": flagShift,
	"ControlLeft": flagControl, "ControlRight": flagControl,
	"AltLeft": flagOption, "AltRight": flagOption,
	"MetaLeft": flagCommand, "MetaRight": flagCommand,
}

type Injector struct {
	mu      sync.Mutex
	m       Mapping
	keys    map[string]bool
	buttons map[int]bool
	bg      bgState // simülasyon modu durumu
	mods    map[string]uint64

	lastBtn   int
	lastClick time.Time
	lastPt    cgPoint
	clicks    int64
}

func New() *Injector {
	load()
	return &Injector{keys: map[string]bool{}, buttons: map[int]bool{}, mods: map[string]uint64{}}
}

func (in *Injector) SetMapping(m Mapping) {
	in.mu.Lock()
	in.m = m
	in.mu.Unlock()
}

func (in *Injector) flags() uint64 {
	var f uint64
	for _, v := range in.mods {
		f |= v
	}
	return f
}

func (in *Injector) pt(x, y int) cgPoint {
	px, py := in.m.apply(x, y)
	return cgPoint{px, py}
}

func (in *Injector) mouse0(typ uint32, p cgPoint, btn uint32, clicks int64) {
	desktop.MarkPointer(int(p.X), int(p.Y))
	in.mouse(typ, p, btn, clicks)
}

func (in *Injector) post(ev uintptr) {
	if ev == 0 {
		return
	}
	cgEventPost(hidEventTap, ev)
	cfRelease(ev)
}

func (in *Injector) mouse(typ uint32, p cgPoint, btn uint32, clicks int64) {
	ev := cgEventCreateMouseEvent(0, typ, p, btn)
	if ev == 0 {
		return
	}
	if clicks > 0 {
		cgEventSetIntegerValueField(ev, fieldClickState, clicks)
	}
	cgEventSetFlags(ev, in.flags())
	in.post(ev)
}

// Tarayıcı düğmesi -> (CG düğmesi, down tipi, up tipi, drag tipi)
func cgButton(b int) (uint32, uint32, uint32, uint32, bool) {
	switch b {
	case 0:
		return 0, evLeftDown, evLeftUp, evLeftDragged, true
	case 2:
		return 1, evRightDown, evRightUp, evRightDragged, true
	case 1:
		return 2, evOtherDown, evOtherUp, evOtherDragged, true
	case 3, 4:
		return uint32(b), evOtherDown, evOtherUp, evOtherDragged, true
	}
	return 0, 0, 0, 0, false
}

func (in *Injector) Move(x, y int) {
	if !load() {
		return
	}
	in.mu.Lock()
	defer in.mu.Unlock()
	p := in.pt(x, y)
	// Bir düğme basılıysa "dragged" olayı gönderilmeli, yoksa sürükleme çalışmaz.
	for _, b := range []int{0, 2, 1} {
		if in.buttons[b] {
			btn, _, _, drag, _ := cgButton(b)
			in.mouse0(drag, p, btn, in.clicks)
			return
		}
	}
	in.mouse0(evMouseMoved, p, 0, 0)
}

func (in *Injector) Button(b int, down bool, x, y int) {
	if !load() {
		return
	}
	in.mu.Lock()
	defer in.mu.Unlock()
	btn, downT, upT, _, ok := cgButton(b)
	if !ok || (!down && !in.buttons[b]) {
		return
	}
	p := in.pt(x, y)
	if down {
		// macOS çift tıklamayı olaydaki "click state" alanından anlar.
		now := time.Now()
		if b == in.lastBtn && now.Sub(in.lastClick) < 500*time.Millisecond &&
			math.Abs(p.X-in.lastPt.X) < 5 && math.Abs(p.Y-in.lastPt.Y) < 5 {
			in.clicks++
		} else {
			in.clicks = 1
		}
		in.lastBtn, in.lastClick, in.lastPt = b, now, p
		in.buttons[b] = true
		in.mouse0(downT, p, btn, in.clicks)
	} else {
		in.buttons[b] = false
		in.mouse0(upT, p, btn, in.clicks)
	}
}

// Wheel: dy pozitif = aşağı (tarayıcı yönü), 120 = bir çentik.
func (in *Injector) Wheel(dx, dy int) {
	if !load() || cgEventCreateScrollWheelEvent2 == nil {
		return
	}
	in.mu.Lock()
	defer in.mu.Unlock()
	conv := func(v int) int32 {
		if v == 0 {
			return 0
		}
		p := -v / 3
		if p == 0 {
			p = -v / int(math.Abs(float64(v)))
		}
		return int32(p)
	}
	desktop.MarkInjected()
	ev := cgEventCreateScrollWheelEvent2(0, scrollPixel, 2, conv(dy), conv(dx), 0)
	if ev != 0 {
		cgEventSetFlags(ev, in.flags())
		in.post(ev)
	}
}

func (in *Injector) Key(code string, down bool) {
	if !load() {
		return
	}
	in.mu.Lock()
	defer in.mu.Unlock()
	in.key(code, down)
}

func (in *Injector) key(code string, down bool) {
	kc, ok := macKeys[code]
	if !ok {
		return
	}
	if f, isMod := modifierFlags[code]; isMod {
		if down {
			in.mods[code] = f
		} else {
			delete(in.mods, code)
		}
	}
	desktop.MarkInjected()
	ev := cgEventCreateKeyboardEvent(0, kc, down)
	if ev == 0 {
		return
	}
	cgEventSetFlags(ev, in.flags())
	in.post(ev)
	if down {
		in.keys[code] = true
	} else {
		delete(in.keys, code)
	}
}

func (in *Injector) ReleaseAll() {
	if !load() {
		return
	}
	in.mu.Lock()
	defer in.mu.Unlock()
	for code := range in.keys {
		in.key(code, false)
	}
	for b, down := range in.buttons {
		if down {
			btn, _, upT, _, _ := cgButton(b)
			in.buttons[b] = false
			in.mouse(upT, in.lastPt, btn, in.clicks)
		}
	}
	in.mods = map[string]uint64{}
}

// Text, metni klavye düzeninden bağımsız olarak yazar (Unicode karakter olayları).
// Telefon klavyesi gibi tuş kodu olmayan girişler için.
func (in *Injector) Text(s string) {
	if !load() || !loadBg() {
		return
	}
	in.mu.Lock()
	defer in.mu.Unlock()
	desktop.MarkInjected()
	for _, r := range s {
		u := utf16Of(string(r))
		for _, d := range []bool{true, false} {
			ev := cgEventCreateKeyboardEvent(0, 0, d)
			if ev == 0 {
				continue
			}
			cgEventKeyboardSetUnicode(ev, uint(len(u)), &u[0])
			in.post(ev)
		}
	}
}
