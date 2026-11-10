//go:build windows

package capture

import (
	"errors"
	"fmt"
	"log"
	"sync"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	user32 = windows.NewLazySystemDLL("user32.dll")
	gdi32  = windows.NewLazySystemDLL("gdi32.dll")

	procGetDC               = user32.NewProc("GetDC")
	procReleaseDC           = user32.NewProc("ReleaseDC")
	procGetCursorInfo       = user32.NewProc("GetCursorInfo")
	procGetIconInfo         = user32.NewProc("GetIconInfo")
	procDrawIconEx          = user32.NewProc("DrawIconEx")
	procEnumDisplayMonitors = user32.NewProc("EnumDisplayMonitors")
	procGetMonitorInfoW     = user32.NewProc("GetMonitorInfoW")

	procCreateCompatibleDC = gdi32.NewProc("CreateCompatibleDC")
	procCreateDIBSection   = gdi32.NewProc("CreateDIBSection")
	procSelectObject       = gdi32.NewProc("SelectObject")
	procBitBlt             = gdi32.NewProc("BitBlt")
	procDeleteDC           = gdi32.NewProc("DeleteDC")
	procDeleteObject       = gdi32.NewProc("DeleteObject")
	procGdiFlush           = gdi32.NewProc("GdiFlush")
)

const (
	srcCopy       = 0x00CC0020
	cursorShowing = 0x1
	diNormal      = 0x3
)

type bitmapInfoHeader struct {
	Size          uint32
	Width         int32
	Height        int32
	Planes        uint16
	BitCount      uint16
	Compression   uint32
	SizeImage     uint32
	XPelsPerMeter int32
	YPelsPerMeter int32
	ClrUsed       uint32
	ClrImportant  uint32
}

type bitmapInfo struct {
	Header bitmapInfoHeader
	Colors [1]uint32
}

type cursorInfo struct {
	Size   uint32
	Flags  uint32
	Cursor uintptr
	X, Y   int32
}

type iconInfo struct {
	IsIcon   int32
	XHotspot uint32
	YHotspot uint32
	HbmMask  uintptr
	HbmColor uintptr
}

type rectW struct{ Left, Top, Right, Bottom int32 }

type monitorInfo struct {
	Size    uint32
	Monitor rectW
	Work    rectW
	Flags   uint32
}

// ---- monitör listesi ----

var (
	enumMu   sync.Mutex
	enumList []Display
	enumCB   = windows.NewCallback(func(hmon, hdc, lprc, lparam uintptr) uintptr {
		var mi monitorInfo
		mi.Size = uint32(unsafe.Sizeof(mi))
		if r, _, _ := procGetMonitorInfoW.Call(hmon, uintptr(unsafe.Pointer(&mi))); r != 0 {
			enumList = append(enumList, Display{
				ID:      len(enumList),
				X:       int(mi.Monitor.Left),
				Y:       int(mi.Monitor.Top),
				W:       int(mi.Monitor.Right - mi.Monitor.Left),
				H:       int(mi.Monitor.Bottom - mi.Monitor.Top),
				Primary: mi.Flags&1 != 0,
			})
		}
		return 1
	})
)

// Displays, bağlı monitörleri döner (koordinatlar fiziksel piksel; süreç DPI-aware olmalı).
func Displays() ([]Display, error) {
	enumMu.Lock()
	defer enumMu.Unlock()
	enumList = nil
	procEnumDisplayMonitors.Call(0, 0, enumCB, 0)
	if len(enumList) == 0 {
		return nil, errors.New("monitör bulunamadı")
	}
	out := make([]Display, len(enumList))
	copy(out, enumList)
	return out, nil
}

// ---- yakalama ----

// Capturer tek bir goroutine'den kullanılmalıdır.
type Capturer struct {
	Full bool // Windows'ta kullanılmaz (her zaman fiziksel çözünürlük)

	screenDC uintptr
	memDC    uintptr
	bmp      uintptr
	oldBmp   uintptr
	w, h     int
	frame    Frame

	dx      *dxgiDup
	dxRetry time.Time
	dxLog   bool
}

func New() (*Capturer, error) {
	dc, _, _ := procGetDC.Call(0)
	if dc == 0 {
		return nil, errors.New("GetDC başarısız")
	}
	mem, _, _ := procCreateCompatibleDC.Call(dc)
	if mem == 0 {
		procReleaseDC.Call(0, dc)
		return nil, errors.New("CreateCompatibleDC başarısız")
	}
	return &Capturer{screenDC: dc, memDC: mem}, nil
}

func (c *Capturer) ensure(w, h int) error {
	if c.bmp != 0 && w == c.w && h == c.h {
		return nil
	}
	c.freeBitmap()
	bi := bitmapInfo{Header: bitmapInfoHeader{
		Width:    int32(w),
		Height:   -int32(h), // negatif = üstten alta
		Planes:   1,
		BitCount: 32,
	}}
	bi.Header.Size = uint32(unsafe.Sizeof(bi.Header))
	var bits unsafe.Pointer
	bmp, _, err := procCreateDIBSection.Call(c.memDC, uintptr(unsafe.Pointer(&bi)), 0, uintptr(unsafe.Pointer(&bits)), 0, 0)
	if bmp == 0 || bits == nil {
		return fmt.Errorf("CreateDIBSection: %v", err)
	}
	c.oldBmp, _, _ = procSelectObject.Call(c.memDC, bmp)
	c.bmp, c.w, c.h = bmp, w, h
	c.frame = Frame{W: w, H: h, Stride: w * 4, Pix: unsafe.Slice((*byte)(bits), w*h*4)}
	return nil
}

func (c *Capturer) freeBitmap() {
	if c.bmp != 0 {
		procSelectObject.Call(c.memDC, c.oldBmp)
		procDeleteObject.Call(c.bmp)
		c.bmp = 0
	}
}

// Capture verilen monitörü yakalar ve imleci görüntünün üzerine çizer.
func (c *Capturer) Capture(d Display) (*Frame, error) {
	if d.W <= 0 || d.H <= 0 {
		return nil, errors.New("geçersiz monitör boyutu")
	}
	if err := c.ensure(d.W, d.H); err != nil {
		return nil, err
	}
	if c.dxgiCapture(d) {
		return &c.frame, nil
	}
	r, _, err := procBitBlt.Call(c.memDC, 0, 0, uintptr(d.W), uintptr(d.H), c.screenDC, uintptr(d.X), uintptr(d.Y), srcCopy)
	if r == 0 {
		return nil, fmt.Errorf("BitBlt: %v", err)
	}
	c.drawCursor(d)
	procGdiFlush.Call()
	return &c.frame, nil
}

// dxgiCapture, kareyi DXGI ile DIB'e alır. false dönerse çağıran GDI ile devam eder.
func (c *Capturer) dxgiCapture(d Display) bool {
	if c.dx != nil && c.dx.display != d {
		c.dx.close()
		c.dx = nil
	}
	if c.dx == nil {
		if time.Now().Before(c.dxRetry) {
			return false
		}
		if c.dx = openDup(d); c.dx == nil {
			c.dxRetry = time.Now().Add(2 * time.Second)
			return false
		}
		if !c.dxLog {
			c.dxLog = true
			log.Printf("capture: DXGI Desktop Duplication %dx%d", d.W, d.H)
		}
	}
	switch c.dx.grab(c.frame.Pix, d.W, d.H, 40*time.Millisecond) {
	case dxgiNew:
		c.drawCursor(d)
		procGdiFlush.Call()
		return true
	case dxgiSame:
		return true // ekran ve imleç değişmedi: DIB'deki kare hâlâ geçerli
	}
	c.dx.close()
	c.dx = nil
	c.dxRetry = time.Now().Add(time.Second)
	return false
}

func (c *Capturer) drawCursor(d Display) {
	var ci cursorInfo
	ci.Size = uint32(unsafe.Sizeof(ci))
	if r, _, _ := procGetCursorInfo.Call(uintptr(unsafe.Pointer(&ci))); r == 0 || ci.Flags&cursorShowing == 0 || ci.Cursor == 0 {
		return
	}
	var hx, hy int
	var ii iconInfo
	if r, _, _ := procGetIconInfo.Call(ci.Cursor, uintptr(unsafe.Pointer(&ii))); r != 0 {
		hx, hy = int(ii.XHotspot), int(ii.YHotspot)
		if ii.HbmMask != 0 {
			procDeleteObject.Call(ii.HbmMask)
		}
		if ii.HbmColor != 0 {
			procDeleteObject.Call(ii.HbmColor)
		}
	}
	x, y := int(ci.X)-d.X-hx, int(ci.Y)-d.Y-hy
	if x < -64 || y < -64 || x > d.W || y > d.H {
		return // imleç başka monitörde
	}
	procDrawIconEx.Call(c.memDC, uintptr(x), uintptr(y), ci.Cursor, 0, 0, 0, 0, diNormal)
}

func (c *Capturer) Close() {
	if c.dx != nil {
		c.dx.close()
		c.dx = nil
	}
	c.freeBitmap()
	if c.memDC != 0 {
		procDeleteDC.Call(c.memDC)
		c.memDC = 0
	}
	if c.screenDC != 0 {
		procReleaseDC.Call(0, c.screenDC)
		c.screenDC = 0
	}
}
