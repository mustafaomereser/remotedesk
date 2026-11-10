//go:build darwin

package capture

import (
	"log"
	"sync"
	"time"
	"unsafe"

	"github.com/ebitengine/purego"
)

// CGDisplayStream: ekran sunucusu kareleri değiştikçe IOSurface olarak verir ve
// istenen boyuta GPU'da ölçekler. CGDisplayCreateImage'a göre çok daha hızlıdır
// (tam Retina görüntüyü her karede CPU'ya kopyalayıp küçültmek gerekmez) ve imleci de içerir.
// Kare bildirimi ana dispatch kuyruğunda (AppKit'in çalıştığı Go iş parçacığı) gelir.

var (
	streamLoadOnce sync.Once
	streamOK       bool

	cgStreamCreate  func(display uint32, w, h uintptr, format int32, props, queue, handler uintptr) uintptr
	cgStreamStart   func(s uintptr) int32
	cgStreamStop    func(s uintptr) int32
	cgCopyMode      func(display uint32) uintptr
	cgModePixelsW   func(mode uintptr) uintptr
	cgModePixelsH   func(mode uintptr) uintptr
	cgModeRelease   func(mode uintptr)
	cfRetain        func(ref uintptr) uintptr
	cfDictCreate    func(alloc uintptr, keys, values unsafe.Pointer, n int, kcb, vcb uintptr) uintptr
	ioLock          func(s uintptr, opts uint32, seed uintptr) int32
	ioUnlock        func(s uintptr, opts uint32, seed uintptr) int32
	ioBase          func(s uintptr) unsafe.Pointer
	ioStride        func(s uintptr) uintptr
	ioWidth         func(s uintptr) uintptr
	ioHeight        func(s uintptr) uintptr
	ioIncUse        func(s uintptr)
	ioDecUse        func(s uintptr)
	streamProps     uintptr
	streamQueue     uintptr
	streamBlockAddr uintptr

	// Global blok (Block ABI): yakalama yok, kopyalanınca kendisi döner.
	streamBlock struct {
		isa      uintptr
		flags    int32
		reserved int32
		invoke   uintptr
		desc     uintptr
	}
	streamBlockDesc struct{ reserved, size uintptr }

	stMu     sync.Mutex
	stLatest uintptr // son tamamlanan karenin IOSurface'i (retain edilmiş)
	stSeq    uint64
	stNotify = make(chan struct{}, 1)
)

func loadStream() bool {
	streamLoadOnce.Do(func() {
		if load() != nil {
			return
		}
		cg, _ := purego.Dlopen("/System/Library/Frameworks/CoreGraphics.framework/CoreGraphics", purego.RTLD_NOW|purego.RTLD_GLOBAL)
		cf, _ := purego.Dlopen("/System/Library/Frameworks/CoreFoundation.framework/CoreFoundation", purego.RTLD_NOW|purego.RTLD_GLOBAL)
		io, err := purego.Dlopen("/System/Library/Frameworks/IOSurface.framework/IOSurface", purego.RTLD_NOW|purego.RTLD_GLOBAL)
		if err != nil {
			log.Printf("capture: IOSurface yok: %v", err)
			return
		}
		sys, err := purego.Dlopen("/usr/lib/libSystem.B.dylib", purego.RTLD_NOW|purego.RTLD_GLOBAL)
		if err != nil {
			return
		}
		fns := []struct {
			lib  uintptr
			fn   any
			name string
		}{
			{cg, &cgStreamCreate, "CGDisplayStreamCreateWithDispatchQueue"},
			{cg, &cgStreamStart, "CGDisplayStreamStart"},
			{cg, &cgStreamStop, "CGDisplayStreamStop"},
			{cg, &cgCopyMode, "CGDisplayCopyDisplayMode"},
			{cg, &cgModePixelsW, "CGDisplayModeGetPixelWidth"},
			{cg, &cgModePixelsH, "CGDisplayModeGetPixelHeight"},
			{cg, &cgModeRelease, "CGDisplayModeRelease"},
			{cf, &cfRetain, "CFRetain"},
			{cf, &cfDictCreate, "CFDictionaryCreate"},
			{io, &ioLock, "IOSurfaceLock"},
			{io, &ioUnlock, "IOSurfaceUnlock"},
			{io, &ioBase, "IOSurfaceGetBaseAddress"},
			{io, &ioStride, "IOSurfaceGetBytesPerRow"},
			{io, &ioWidth, "IOSurfaceGetWidth"},
			{io, &ioHeight, "IOSurfaceGetHeight"},
			{io, &ioIncUse, "IOSurfaceIncrementUseCount"},
			{io, &ioDecUse, "IOSurfaceDecrementUseCount"},
		}
		for _, f := range fns {
			p, err := purego.Dlsym(f.lib, f.name)
			if err != nil {
				log.Printf("capture: %s yok, CGDisplayStream kullanılmayacak", f.name)
				return
			}
			purego.RegisterFunc(f.fn, p)
		}
		sym := func(lib uintptr, name string) uintptr {
			p, err := purego.Dlsym(lib, name)
			if err != nil {
				return 0
			}
			return p
		}
		mainQ := sym(sys, "_dispatch_main_q")
		globalBlock := sym(sys, "_NSConcreteGlobalBlock")
		showCursor := sym(cg, "kCGDisplayStreamShowCursor")
		boolTrue := sym(cf, "kCFBooleanTrue")
		keyCB := sym(cf, "kCFTypeDictionaryKeyCallBacks")
		valCB := sym(cf, "kCFTypeDictionaryValueCallBacks")
		if mainQ == 0 || globalBlock == 0 || showCursor == 0 || boolTrue == 0 || keyCB == 0 || valCB == 0 {
			log.Printf("capture: CGDisplayStream sembolleri eksik")
			return
		}
		keys := [1]uintptr{**(**uintptr)(unsafe.Pointer(&showCursor))}
		vals := [1]uintptr{**(**uintptr)(unsafe.Pointer(&boolTrue))}
		streamProps = cfDictCreate(0, unsafe.Pointer(&keys[0]), unsafe.Pointer(&vals[0]), 1, keyCB, valCB)
		streamQueue = mainQ

		streamBlockDesc.size = unsafe.Sizeof(streamBlock)
		streamBlock.isa = globalBlock
		streamBlock.flags = 1 << 28 // BLOCK_IS_GLOBAL
		streamBlock.invoke = purego.NewCallback(onStreamFrame)
		streamBlock.desc = uintptr(unsafe.Pointer(&streamBlockDesc))
		streamBlockAddr = uintptr(unsafe.Pointer(&streamBlock))
		streamOK = true
	})
	return streamOK
}

// onStreamFrame: ^(CGDisplayStreamFrameStatus status, uint64 time, IOSurfaceRef surface, CGDisplayStreamUpdateRef update)
func onStreamFrame(block, status, t, surf, update uintptr) uintptr {
	if uint32(status) != 0 || surf == 0 { // 0 = kCGDisplayStreamFrameStatusFrameComplete
		return 0
	}
	cfRetain(surf)
	ioIncUse(surf)
	stMu.Lock()
	old := stLatest
	stLatest = surf
	stSeq++
	stMu.Unlock()
	if old != 0 {
		ioDecUse(old)
		cfRelease(old)
	}
	select {
	case stNotify <- struct{}{}:
	default:
	}
	return 0
}

type displayStream struct {
	ref     uintptr
	display int
	full    bool
	started time.Time
	gotAny  bool
	lastSeq uint64
}

func (c *Capturer) stopStream() {
	if c.stream == nil {
		return
	}
	cgStreamStop(c.stream.ref)
	cfRelease(c.stream.ref)
	c.stream = nil
	stMu.Lock()
	old := stLatest
	stLatest = 0
	stMu.Unlock()
	if old != 0 {
		ioDecUse(old)
		cfRelease(old)
	}
}

// streamCapture, CGDisplayStream'den en yeni kareyi döner. ok=false ise eski yönteme düşülmeli.
func (c *Capturer) streamCapture(d Display) (*Frame, bool) {
	if c.streamBroken[d.ID] || !loadStream() {
		return nil, false
	}
	if c.stream != nil && (c.stream.display != d.ID || c.stream.full != c.Full) {
		c.stopStream()
	}
	if c.stream == nil {
		w, h := d.W, d.H // point boyutu: Retina'da yarı çözünürlük
		if c.Full {
			if m := cgCopyMode(uint32(d.ID)); m != 0 {
				w, h = int(cgModePixelsW(m)), int(cgModePixelsH(m))
				cgModeRelease(m)
			}
		}
		ref := cgStreamCreate(uint32(d.ID), uintptr(w), uintptr(h), 0x42475241, streamProps, streamQueue, streamBlockAddr)
		if ref == 0 || cgStreamStart(ref) != 0 {
			if ref != 0 {
				cfRelease(ref)
			}
			log.Printf("capture: CGDisplayStream başlatılamadı, CGDisplayCreateImage kullanılıyor")
			c.streamBroken[d.ID] = true
			return nil, false
		}
		c.stream = &displayStream{ref: ref, display: d.ID, full: c.Full, started: time.Now()}
		log.Printf("capture: CGDisplayStream %dx%d", w, h)
	}
	s := c.stream

	// Yeni kare gelene kadar kısa süre bekle (ekran değişmiyorsa sonuncusunu tekrar ver).
	deadline := time.After(100 * time.Millisecond)
	for {
		stMu.Lock()
		seq := stSeq
		stMu.Unlock()
		if seq != s.lastSeq && seq != 0 {
			break
		}
		if s.gotAny {
			select {
			case <-stNotify:
				continue
			case <-deadline:
			}
			break
		}
		// Henüz hiç kare yok: ilk kare gelmezse eski yönteme düş.
		if time.Since(s.started) > 3*time.Second {
			log.Printf("capture: CGDisplayStream kare vermiyor, CGDisplayCreateImage kullanılıyor")
			c.stopStream()
			c.streamBroken[d.ID] = true
			return nil, false
		}
		select {
		case <-stNotify:
			continue
		case <-deadline:
			return nil, false // bu kareyi eski yöntemle al
		}
	}

	stMu.Lock()
	surf := stLatest
	seq := stSeq
	if surf != 0 {
		cfRetain(surf)
	}
	stMu.Unlock()
	if surf == 0 {
		if c.frame.Pix != nil && s.gotAny {
			return &c.frame, true
		}
		return nil, false
	}
	defer cfRelease(surf)

	const readOnly = 1
	if ioLock(surf, readOnly, 0) != 0 {
		return nil, false
	}
	w, h, stride := int(ioWidth(surf)), int(ioHeight(surf)), int(ioStride(surf))
	need := stride * h
	if cap(c.buf) < need {
		c.buf = make([]byte, need)
	}
	c.buf = c.buf[:need]
	copy(c.buf, unsafe.Slice((*byte)(ioBase(surf)), need))
	ioUnlock(surf, readOnly, 0)

	s.gotAny = true
	s.lastSeq = seq
	c.releaseData()
	c.frame = Frame{W: w, H: h, Stride: stride, Pix: c.buf}
	return &c.frame, true
}
