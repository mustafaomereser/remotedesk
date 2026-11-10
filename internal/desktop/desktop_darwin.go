//go:build darwin

package desktop

import (
	"log"
	"os"
	"runtime"
	"sync"
	"time"
	"unsafe"

	"github.com/ebitengine/purego"
	"github.com/ebitengine/purego/objc"
)

type cgPoint struct{ X, Y float64 }
type cgSize struct{ W, H float64 }
type cgRect struct {
	Origin cgPoint
	Size   cgSize
}

var (
	cgLoad sync.Once

	cgEventCreate      func(src uintptr) uintptr
	cgEventGetLocation func(ev uintptr) cgPoint
	cgSecondsSince     func(state int32, typ uint32) float64
	cgMainDisplayID    func() uint32
	cgDisplayBounds    func(id uint32) cgRect
	cfRelease          func(ref uintptr)
	dispatchAsyncF     func(queue, ctx, work uintptr)
	dispatchMainQ      uintptr
	mainCallback       uintptr
	mainReady          bool
	msgSendAddr        uintptr
	mainJobs           = make(chan func(), 256)

	// objc_msgSend'in kullandığımız imzaları (struct/float argümanları için tipli kayıt gerekir)
	msgID      func(id, sel uintptr) uintptr
	msgID1     func(id, sel, a uintptr) uintptr
	msgBool    func(id, sel uintptr, b bool) uintptr
	msgInt     func(id, sel uintptr, n int) uintptr
	msgFloat   func(id, sel uintptr, f float64) uintptr
	msgColor   func(id, sel uintptr, r, g, b, a float64) uintptr
	msgInitWin func(id, sel uintptr, r cgRect, style, backing uint, deferFlag bool) uintptr
	msgSetRect func(id, sel uintptr, r cgRect, display bool) uintptr
	msgFrame   func(id, sel uintptr, r cgRect) uintptr // initWithFrame: / setFrame:
	msgSize    func(id, sel uintptr) cgSize
)

func loadCG() {
	cgLoad.Do(func() {
		cg, err := purego.Dlopen("/System/Library/Frameworks/CoreGraphics.framework/CoreGraphics", purego.RTLD_NOW|purego.RTLD_GLOBAL)
		if err != nil {
			log.Printf("desktop: %v", err)
			return
		}
		cf, _ := purego.Dlopen("/System/Library/Frameworks/CoreFoundation.framework/CoreFoundation", purego.RTLD_NOW|purego.RTLD_GLOBAL)
		reg := func(lib uintptr, fn any, name string) {
			if p, err := purego.Dlsym(lib, name); err == nil {
				purego.RegisterFunc(fn, p)
			} else {
				log.Printf("desktop: %s yok", name)
			}
		}
		reg(cg, &cgEventCreate, "CGEventCreate")
		reg(cg, &cgEventGetLocation, "CGEventGetLocation")
		reg(cg, &cgSecondsSince, "CGEventSourceSecondsSinceLastEventType")
		reg(cg, &cgMainDisplayID, "CGMainDisplayID")
		reg(cg, &cgDisplayBounds, "CGDisplayBounds")
		reg(cf, &cfRelease, "CFRelease")
	})
}

func cursorPos() (int, int, bool) {
	loadCG()
	if cgEventCreate == nil || cgEventGetLocation == nil {
		return 0, 0, false
	}
	ev := cgEventCreate(0)
	if ev == 0 {
		return 0, 0, false
	}
	p := cgEventGetLocation(ev)
	cfRelease(ev)
	return int(p.X), int(p.Y), true
}

func lastInputAge() time.Duration {
	loadCG()
	if cgSecondsSince == nil {
		return time.Hour
	}
	// kCGEventSourceStateHIDSystemState = 1, kCGAnyInputEventType = ~0
	s := cgSecondsSince(1, ^uint32(0))
	return time.Duration(s * float64(time.Second))
}

// RunMain, fn'i ayrı bir goroutine'de çalıştırır, ana iş parçacığında AppKit olay döngüsünü
// başlatır (pencereler ancak böyle çizilebilir). fn bitince süreç kapanır.
// Ana iş parçacığında çağrılmalıdır (main paketinde init'te runtime.LockOSThread).
func RunMain(fn func()) {
	go func() {
		fn()
		os.Exit(0)
	}()
	app := setupApp()
	if app == 0 {
		select {} // AppKit yoksa etiket olmadan devam
	}
	msgID(app, sel("run")) // dönmez
}

func sel(name string) uintptr   { return uintptr(objc.RegisterName(name)) }
func class(name string) uintptr { return uintptr(objc.GetClass(name)) }

func setupApp() uintptr {
	if _, err := purego.Dlopen("/System/Library/Frameworks/AppKit.framework/AppKit", purego.RTLD_NOW|purego.RTLD_GLOBAL); err != nil {
		log.Printf("desktop: AppKit yüklenemedi: %v", err)
		return 0
	}
	objcLib, err := purego.Dlopen("/usr/lib/libobjc.A.dylib", purego.RTLD_NOW|purego.RTLD_GLOBAL)
	if err != nil {
		return 0
	}
	send, err := purego.Dlsym(objcLib, "objc_msgSend")
	if err != nil {
		return 0
	}
	msgSendAddr = send
	for _, fn := range []any{&msgID, &msgID1, &msgBool, &msgInt, &msgFloat, &msgColor, &msgInitWin, &msgSetRect, &msgFrame, &msgSize} {
		purego.RegisterFunc(fn, send)
	}

	sys, err := purego.Dlopen("/usr/lib/libSystem.B.dylib", purego.RTLD_NOW|purego.RTLD_GLOBAL)
	if err != nil {
		return 0
	}
	q, err1 := purego.Dlsym(sys, "_dispatch_main_q")
	da, err2 := purego.Dlsym(sys, "dispatch_async_f")
	if err1 != nil || err2 != nil {
		return 0
	}
	dispatchMainQ = q
	purego.RegisterFunc(&dispatchAsyncF, da)
	mainCallback = purego.NewCallback(func(ctx uintptr) uintptr {
		for {
			select {
			case f := <-mainJobs:
				f()
			default:
				return 0
			}
		}
	})

	app := msgID(class("NSApplication"), sel("sharedApplication"))
	// Accessory: Dock'ta ikon yok; arayüz tarayıcı penceresinde.
	msgInt(app, sel("setActivationPolicy:"), 1)
	mainReady = true
	return app
}

// onMain, f'i ana iş parçacığında çalıştırır (AppKit yalnızca orada kullanılabilir).
func onMain(f func()) {
	if !mainReady {
		return
	}
	select {
	case mainJobs <- f:
		dispatchAsyncF(dispatchMainQ, 0, mainCallback)
	default: // kuyruk dolu: bu güncellemeyi atla
	}
}

func nsString(s string) uintptr {
	b := append([]byte(s), 0)
	r := msgID1(class("NSString"), sel("stringWithUTF8String:"), uintptr(unsafe.Pointer(&b[0])))
	runtime.KeepAlive(b)
	return r
}

var (
	lblWin   uintptr
	lblView  uintptr
	lblField uintptr
	lblArrow uintptr
	arrowW   float64
	arrowH   float64
	lblName  string
)

func ensureLabelWindow() bool {
	if lblWin != 0 {
		return true
	}
	const (
		borderless = 0
		buffered   = 2
	)
	frame := cgRect{Size: cgSize{160, 50}}
	w := msgID(class("NSWindow"), sel("alloc"))
	w = msgInitWin(w, sel("initWithContentRect:styleMask:backing:defer:"), frame, borderless, buffered, false)
	if w == 0 {
		return false
	}
	msgBool(w, sel("setOpaque:"), false)
	msgID1(w, sel("setBackgroundColor:"), msgID(class("NSColor"), sel("clearColor")))
	msgInt(w, sel("setLevel:"), 1000) // NSScreenSaverWindowLevel: her şeyin üstünde
	msgBool(w, sel("setIgnoresMouseEvents:"), true)
	msgBool(w, sel("setHasShadow:"), false)
	msgBool(w, sel("setReleasedWhenClosed:"), false)
	msgInt(w, sel("setCollectionBehavior:"), 1|16|256) // tüm masaüstleri + tam ekran

	view := msgFrame(msgID(class("NSView"), sel("alloc")), sel("initWithFrame:"), frame)
	msgID1(w, sel("setContentView:"), view)

	// Hayalet imleç: sistemin ok imlecinin resmi
	img := msgID(msgID(class("NSCursor"), sel("arrowCursor")), sel("image"))
	sz := msgSize(img, sel("size"))
	arrowW, arrowH = sz.W, sz.H
	arrow := msgID1(class("NSImageView"), sel("imageViewWithImage:"), img)
	msgID1(view, sel("addSubview:"), arrow)

	f := msgID1(class("NSTextField"), sel("labelWithString:"), nsString(""))
	msgID1(f, sel("setTextColor:"), msgID(class("NSColor"), sel("whiteColor")))
	msgID1(f, sel("setFont:"), msgFloat(class("NSFont"), sel("boldSystemFontOfSize:"), 12))
	msgInt(f, sel("setAlignment:"), 2) // NSTextAlignmentCenter (macOS)
	msgBool(f, sel("setDrawsBackground:"), true)
	msgID1(f, sel("setBackgroundColor:"), msgColor(class("NSColor"), sel("colorWithSRGBRed:green:blue:alpha:"), 0.937, 0.294, 0.247, 0.95))
	msgID1(view, sel("addSubview:"), f)

	lblWin, lblView, lblField, lblArrow = w, view, f, arrow
	return true
}

func showLabel(name string, x, y int, ghost bool) {
	loadCG()
	onMain(func() {
		if !ensureLabelWindow() {
			return
		}
		if name != lblName {
			msgID1(lblField, sel("setStringValue:"), nsString(name))
			lblName = name
		}
		ts := msgSize(lblField, sel("fittingSize"))
		lw, lh := ts.W+20, ts.H+4 // yazı kesilmesin diye pay

		// Pencere içi yerleşim (Cocoa'da orijin sol alt)
		var W, H, left, top float64
		if ghost {
			const lx, ly = 10, 20 // ad kutusu, okun ucunun sağ altında
			W = max(arrowW, lx+lw)
			H = max(arrowH, ly+lh)
			msgFrame(lblArrow, sel("setFrame:"), cgRect{Origin: cgPoint{0, H - arrowH}, Size: cgSize{arrowW, arrowH}})
			msgFrame(lblField, sel("setFrame:"), cgRect{Origin: cgPoint{lx, H - ly - lh}, Size: cgSize{lw, lh}})
			left, top = float64(x), float64(y) // okun ucu = işlem noktası
		} else {
			W, H = lw, lh
			msgFrame(lblField, sel("setFrame:"), cgRect{Size: cgSize{lw, lh}})
			left, top = float64(x)+12, float64(y)+22 // gerçek imlecin sağ altı
		}
		msgBool(lblArrow, sel("setHidden:"), !ghost)

		// Bizim koordinatlarımız ana ekranın sol üstünden; Cocoa'nınki sol altından.
		mainH := 0.0
		if cgDisplayBounds != nil && cgMainDisplayID != nil {
			mainH = cgDisplayBounds(cgMainDisplayID()).Size.H
		}
		r := cgRect{Origin: cgPoint{left, mainH - top - H}, Size: cgSize{W, H}}
		msgSetRect(lblWin, sel("setFrame:display:"), r, true)
		msgID1(lblWin, sel("orderFrontRegardless"), 0)
	})
}

func hideLabel() {
	onMain(func() {
		if lblWin != 0 {
			msgID1(lblWin, sel("orderOut:"), 0)
		}
	})
}
