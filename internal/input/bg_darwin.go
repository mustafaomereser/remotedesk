//go:build darwin

package input

import (
	"log"
	"os"
	"sync"
	"unsafe"

	"github.com/ebitengine/purego"
)

// Simülasyon modu ("aynı anda kullanım"): gerçek imleci oynatmadan olayları doğrudan
// tıklanan penceredeki uygulamaya gönderir (CGEventPostToPid). Yazı ve basit tıklamalar
// standart Cocoa uygulamalarında çoğunlukla çalışır; arka planda kalan pencerelerin bir kısmı
// ilk tıklamayı yok sayar, Chrome/Electron ve oyunlarda güvenilir değildir.

var (
	bgOnce sync.Once
	bgOK   bool

	cgWindowListCopyWindowInfo   func(option uint32, rel uint32) uintptr
	cgRectMakeWithDictionaryRepr func(dict uintptr, r *cgRectI) bool
	cgEventPostToPid             func(pid int32, ev uintptr)
	cgEventKeyboardSetUnicode    func(ev uintptr, n uint, s *uint16)
	cgEventSetLocation           func(ev uintptr, p cgPoint)
	cfArrayGetCount              func(a uintptr) int
	cfArrayGetValueAtIndex       func(a uintptr, i int) uintptr
	cfDictionaryGetValue         func(d, k uintptr) uintptr
	cfNumberGetValue             func(n uintptr, typ int, out *int64) bool

	keyOwnerPID, keyWindowNumber, keyLayer, keyBounds uintptr
)

type cgRectI struct{ X, Y, W, H float64 }

func derefPtr(p uintptr) uintptr { return **(**uintptr)(unsafe.Pointer(&p)) }

func loadBg() bool {
	bgOnce.Do(func() {
		cg, err := purego.Dlopen("/System/Library/Frameworks/CoreGraphics.framework/CoreGraphics", purego.RTLD_NOW|purego.RTLD_GLOBAL)
		if err != nil {
			return
		}
		cf, err := purego.Dlopen("/System/Library/Frameworks/CoreFoundation.framework/CoreFoundation", purego.RTLD_NOW|purego.RTLD_GLOBAL)
		if err != nil {
			return
		}
		fns := []struct {
			lib  uintptr
			fn   any
			name string
		}{
			{cg, &cgWindowListCopyWindowInfo, "CGWindowListCopyWindowInfo"},
			{cg, &cgRectMakeWithDictionaryRepr, "CGRectMakeWithDictionaryRepresentation"},
			{cg, &cgEventPostToPid, "CGEventPostToPid"},
			{cg, &cgEventKeyboardSetUnicode, "CGEventKeyboardSetUnicodeString"},
			{cg, &cgEventSetLocation, "CGEventSetLocation"},
			{cf, &cfArrayGetCount, "CFArrayGetCount"},
			{cf, &cfArrayGetValueAtIndex, "CFArrayGetValueAtIndex"},
			{cf, &cfDictionaryGetValue, "CFDictionaryGetValue"},
			{cf, &cfNumberGetValue, "CFNumberGetValue"},
		}
		for _, f := range fns {
			p, err := purego.Dlsym(f.lib, f.name)
			if err != nil {
				log.Printf("simülasyon modu: %s yok", f.name)
				return
			}
			purego.RegisterFunc(f.fn, p)
		}
		for _, k := range []struct {
			dst  *uintptr
			name string
		}{{&keyOwnerPID, "kCGWindowOwnerPID"}, {&keyWindowNumber, "kCGWindowNumber"}, {&keyLayer, "kCGWindowLayer"}, {&keyBounds, "kCGWindowBounds"}} {
			p, err := purego.Dlsym(cg, k.name)
			if err != nil {
				return
			}
			*k.dst = derefPtr(p) // sembol, CFStringRef'in adresidir
		}
		bgOK = true
	})
	return bgOK && load()
}

func cfInt(dict, key uintptr) (int64, bool) {
	v := cfDictionaryGetValue(dict, key)
	if v == 0 {
		return 0, false
	}
	var n int64
	ok := cfNumberGetValue(v, 4, &n) // kCFNumberSInt64Type
	return n, ok
}

// windowAt, (x, y) noktasındaki en öndeki normal pencerenin sahibi PID'i ve pencere numarasını döner.
func windowAt(x, y float64) (pid int32, wnum int64) {
	list := cgWindowListCopyWindowInfo(1|16, 0) // OnScreenOnly | ExcludeDesktopElements
	if list == 0 {
		return 0, 0
	}
	defer cfRelease(list)
	self := int64(os.Getpid())
	for i, n := 0, cfArrayGetCount(list); i < n; i++ {
		d := cfArrayGetValueAtIndex(list, i)
		if layer, _ := cfInt(d, keyLayer); layer != 0 {
			continue // menü çubuğu, Dock, bizim etiketimiz vb.
		}
		p, ok := cfInt(d, keyOwnerPID)
		if !ok || p == self {
			continue
		}
		b := cfDictionaryGetValue(d, keyBounds)
		var r cgRectI
		if b == 0 || !cgRectMakeWithDictionaryRepr(b, &r) {
			continue
		}
		if x >= r.X && x < r.X+r.W && y >= r.Y && y < r.Y+r.H {
			num, _ := cfInt(d, keyWindowNumber)
			return int32(p), num
		}
	}
	return 0, 0
}

type bgState struct {
	pid    int32         // klavyenin gideceği uygulama (son tıklanan)
	down   map[int]int32 // basılı düğme -> PID
	wnum   int64
	clicks int64
}

func (in *Injector) postMouseTo(pid int32, wnum int64, typ uint32, p cgPoint, btn uint32, clicks int64) {
	ev := cgEventCreateMouseEvent(0, typ, p, btn)
	if ev == 0 {
		return
	}
	cgEventSetIntegerValueField(ev, fieldClickState, max(clicks, 1))
	if wnum != 0 {
		cgEventSetIntegerValueField(ev, 91, wnum) // kCGMouseEventWindowUnderMousePointer
		cgEventSetIntegerValueField(ev, 92, wnum) // ...ThatCanHandleThisEvent
	}
	cgEventPostToPid(pid, ev)
	cfRelease(ev)
}

func (in *Injector) BgMove(x, y int) {
	if !loadBg() {
		return
	}
	in.mu.Lock()
	defer in.mu.Unlock()
	p := in.pt(x, y)
	for b, pid := range in.bg.down { // sadece sürüklemede gönder
		btn, _, _, drag, _ := cgButton(b)
		in.postMouseTo(pid, in.bg.wnum, drag, p, btn, in.bg.clicks)
		return
	}
}

func (in *Injector) BgButton(b int, down bool, x, y int) {
	if !loadBg() {
		return
	}
	in.mu.Lock()
	defer in.mu.Unlock()
	if in.bg.down == nil {
		in.bg.down = map[int]int32{}
	}
	btn, downT, upT, _, ok := cgButton(b)
	if !ok {
		return
	}
	p := in.pt(x, y)
	if down {
		pid, wnum := windowAt(p.X, p.Y)
		if pid == 0 {
			return
		}
		in.bg.pid, in.bg.wnum = pid, wnum
		in.bg.down[b] = pid
		in.bg.clicks = 1
		in.postMouseTo(pid, wnum, downT, p, btn, 1)
		return
	}
	pid, ok := in.bg.down[b]
	if !ok {
		return
	}
	delete(in.bg.down, b)
	in.postMouseTo(pid, in.bg.wnum, upT, p, btn, in.bg.clicks)
}

func (in *Injector) BgWheel(dx, dy, x, y int) {
	if !loadBg() || cgEventCreateScrollWheelEvent2 == nil {
		return
	}
	in.mu.Lock()
	defer in.mu.Unlock()
	p := in.pt(x, y)
	pid, _ := windowAt(p.X, p.Y)
	if pid == 0 {
		return
	}
	ev := cgEventCreateScrollWheelEvent2(0, scrollPixel, 2, int32(-dy/3), int32(-dx/3), 0)
	if ev == 0 {
		return
	}
	cgEventSetLocation(ev, p)
	cgEventPostToPid(pid, ev)
	cfRelease(ev)
}

// BgKey: yazılabilir karakterler Unicode olarak (viewer'ın klavye düzeniyle), diğer tuşlar
// (Enter, Delete, oklar…) sanal tuş kodu olarak son tıklanan uygulamaya gider.
func (in *Injector) BgKey(code, ch string, down bool) {
	if !loadBg() {
		return
	}
	in.mu.Lock()
	defer in.mu.Unlock()
	pid := in.bg.pid
	if pid == 0 {
		return
	}
	var kc uint16
	if ch == "" {
		k, ok := macKeys[code]
		if !ok {
			return
		}
		kc = k
	} else if !down {
		return // karakter tek seferde (basma+bırakma) gönderiliyor
	}
	send := func(d bool) {
		ev := cgEventCreateKeyboardEvent(0, kc, d)
		if ev == 0 {
			return
		}
		if ch != "" {
			u := utf16Of(ch)
			cgEventKeyboardSetUnicode(ev, uint(len(u)), &u[0])
		}
		cgEventPostToPid(pid, ev)
		cfRelease(ev)
	}
	if ch != "" {
		send(true)
		send(false)
	} else {
		send(down)
	}
}

func utf16Of(s string) []uint16 {
	var u []uint16
	for _, r := range s {
		if r > 0xFFFF {
			r -= 0x10000
			u = append(u, uint16(0xD800+(r>>10)), uint16(0xDC00+(r&0x3FF)))
		} else {
			u = append(u, uint16(r))
		}
	}
	return u
}
