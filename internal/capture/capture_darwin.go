//go:build darwin

package capture

import (
	"errors"
	"fmt"
	"sync"
	"unsafe"

	"github.com/ebitengine/purego"
)

type cgPoint struct{ X, Y float64 }
type cgSize struct{ W, H float64 }
type cgRect struct {
	Origin cgPoint
	Size   cgSize
}

var (
	loadOnce sync.Once
	loadErr  error

	cgGetActiveDisplayList func(max uint32, ids *uint32, count *uint32) int32
	cgMainDisplayID        func() uint32
	cgDisplayBounds        func(id uint32) cgRect
	cgDisplayCreateImage   func(id uint32) uintptr
	cgImageGetWidth        func(img uintptr) uintptr
	cgImageGetHeight       func(img uintptr) uintptr
	cgImageGetBytesPerRow  func(img uintptr) uintptr
	cgImageGetBitsPerPixel func(img uintptr) uintptr
	cgImageGetBitmapInfo   func(img uintptr) uint32
	cgImageGetDataProvider func(img uintptr) uintptr
	cgDataProviderCopyData func(p uintptr) uintptr
	cfDataGetBytePtr       func(d uintptr) unsafe.Pointer
	cfDataGetLength        func(d uintptr) int
	cfRelease              func(ref uintptr)
)

func load() error {
	loadOnce.Do(func() {
		cg, err := purego.Dlopen("/System/Library/Frameworks/CoreGraphics.framework/CoreGraphics", purego.RTLD_NOW|purego.RTLD_GLOBAL)
		if err != nil {
			loadErr = err
			return
		}
		cf, err := purego.Dlopen("/System/Library/Frameworks/CoreFoundation.framework/CoreFoundation", purego.RTLD_NOW|purego.RTLD_GLOBAL)
		if err != nil {
			loadErr = err
			return
		}
		syms := []struct {
			lib  uintptr
			fn   any
			name string
		}{
			{cg, &cgGetActiveDisplayList, "CGGetActiveDisplayList"},
			{cg, &cgMainDisplayID, "CGMainDisplayID"},
			{cg, &cgDisplayBounds, "CGDisplayBounds"},
			{cg, &cgDisplayCreateImage, "CGDisplayCreateImage"},
			{cg, &cgImageGetWidth, "CGImageGetWidth"},
			{cg, &cgImageGetHeight, "CGImageGetHeight"},
			{cg, &cgImageGetBytesPerRow, "CGImageGetBytesPerRow"},
			{cg, &cgImageGetBitsPerPixel, "CGImageGetBitsPerPixel"},
			{cg, &cgImageGetBitmapInfo, "CGImageGetBitmapInfo"},
			{cg, &cgImageGetDataProvider, "CGImageGetDataProvider"},
			{cg, &cgDataProviderCopyData, "CGDataProviderCopyData"},
			{cf, &cfDataGetBytePtr, "CFDataGetBytePtr"},
			{cf, &cfDataGetLength, "CFDataGetLength"},
			{cf, &cfRelease, "CFRelease"},
		}
		for _, s := range syms {
			p, err := purego.Dlsym(s.lib, s.name)
			if err != nil {
				loadErr = fmt.Errorf("%s bulunamadı: %w", s.name, err)
				return
			}
			purego.RegisterFunc(s.fn, p)
		}
	})
	return loadErr
}

// Displays, aktif monitörleri döner (koordinatlar "point" cinsinden).
func Displays() ([]Display, error) {
	if err := load(); err != nil {
		return nil, err
	}
	var ids [16]uint32
	var n uint32
	if cgGetActiveDisplayList(uint32(len(ids)), &ids[0], &n) != 0 || n == 0 {
		return nil, errors.New("monitör bulunamadı")
	}
	main := cgMainDisplayID()
	out := make([]Display, 0, n)
	for _, id := range ids[:n] {
		b := cgDisplayBounds(id)
		out = append(out, Display{
			ID:      int(id),
			X:       int(b.Origin.X),
			Y:       int(b.Origin.Y),
			W:       int(b.Size.W),
			H:       int(b.Size.H),
			Primary: id == main,
		})
	}
	return out, nil
}

type Capturer struct {
	// Full: Retina ekranı tam çözünürlükte yakala (yoksa point boyutunda).
	Full bool

	stream       *displayStream
	streamBroken map[int]bool

	buf   []byte
	data  uintptr // son karenin CFData'sı; Pix doğrudan onu gösterir (kopya yok)
	frame Frame
}

func (c *Capturer) releaseData() {
	if c.data != 0 {
		cfRelease(c.data)
		c.data = 0
	}
}

func New() (*Capturer, error) {
	if err := load(); err != nil {
		return nil, err
	}
	return &Capturer{streamBroken: map[int]bool{}}, nil
}

// Capture monitörü fiziksel piksel çözünürlüğünde yakalar.
// Not: macOS'ta imleç görüntüye dahil edilmez.
func (c *Capturer) Capture(d Display) (*Frame, error) {
	if f, ok := c.streamCapture(d); ok {
		return f, nil
	}
	img := cgDisplayCreateImage(uint32(d.ID))
	if img == 0 {
		return nil, errors.New("ekran yakalanamadı (Ekran Kaydı izni verildi mi?)")
	}
	defer cfRelease(img)
	if bpp := cgImageGetBitsPerPixel(img); bpp != 32 {
		return nil, fmt.Errorf("desteklenmeyen piksel biçimi: %d bit", bpp)
	}
	w, h := int(cgImageGetWidth(img)), int(cgImageGetHeight(img))
	stride := int(cgImageGetBytesPerRow(img))
	data := cgDataProviderCopyData(cgImageGetDataProvider(img))
	if data == 0 {
		return nil, errors.New("görüntü verisi alınamadı")
	}
	need := stride * h
	if cfDataGetLength(data) < need {
		cfRelease(data)
		return nil, errors.New("görüntü verisi eksik")
	}
	// Önceki karenin verisi artık kullanılmıyor (Frame.Pix bir sonraki Capture'a kadar geçerli).
	c.releaseData()
	c.data = data
	pix := unsafe.Slice((*byte)(cfDataGetBytePtr(data)), need)

	// Genelde BGRA (32Little + alpha first) gelir: kopyalamadan kullan.
	// Değilse kendi tamponumuza kopyalayıp BGRA'ya çevir (CFData salt okunurdur).
	info := cgImageGetBitmapInfo(img)
	if info&0x7000 == 0x2000 {
		c.frame = Frame{W: w, H: h, Stride: stride, Pix: pix}
		return &c.frame, nil
	}
	if cap(c.buf) < need {
		c.buf = make([]byte, need)
	}
	c.buf = c.buf[:need]
	copy(c.buf, pix)
	{
		alpha := info & 0x1F
		alphaFirst := alpha == 2 || alpha == 4 || alpha == 6
		p := c.buf
		for y := 0; y < h; y++ {
			row := p[y*stride : y*stride+w*4]
			for i := 0; i < len(row); i += 4 {
				if alphaFirst { // ARGB -> BGRA
					row[i], row[i+1], row[i+2], row[i+3] = row[i+3], row[i+2], row[i+1], row[i]
				} else { // RGBA -> BGRA
					row[i], row[i+2] = row[i+2], row[i]
				}
			}
		}
	}
	c.frame = Frame{W: w, H: h, Stride: stride, Pix: c.buf}
	return &c.frame, nil
}

func (c *Capturer) Close() {
	c.stopStream()
	c.releaseData()
}
