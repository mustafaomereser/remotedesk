package host

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/jpeg"
	"runtime"
	"sync"

	"remotedesk/internal/capture"
)

const (
	tileSize     = 64
	maxMergeRows = 4 // dikey birleştirmede en fazla 4 karo (paralel kodlama için)

	msgRect     = 1
	msgFrameEnd = 2
	msgVideo    = 3 // [3][1=anahtar kare][H.264 Annex B]
)

type rect struct{ x, y, w, h int }

// diffRects, önceki kareye göre değişen 64x64 karoları bulur ve
// yatay/dikey komşuları dikdörtgenlerde birleştirir. prev nil ise tüm ekran döner.
func diffRects(prev []byte, f *capture.Frame) []rect {
	var out []rect
	for ty := 0; ty < f.H; ty += tileSize {
		th := min(tileSize, f.H-ty)
		start := -1
		flush := func(end int) {
			r := rect{start, ty, end - start, th}
			// Bir üst satırdaki aynı genişlikteki dikdörtgenle birleştir.
			for i := len(out) - 1; i >= 0; i-- {
				o := &out[i]
				if o.y+o.h < ty {
					break
				}
				if o.x == r.x && o.w == r.w && o.y+o.h == ty && o.h < tileSize*maxMergeRows {
					o.h += th
					return
				}
			}
			out = append(out, r)
		}
		for tx := 0; tx < f.W; tx += tileSize {
			tw := min(tileSize, f.W-tx)
			if prev == nil || tileChanged(prev, f.Pix, f.Stride, tx, ty, tw, th) {
				if start < 0 {
					start = tx
				}
			} else if start >= 0 {
				flush(tx)
				start = -1
			}
		}
		if start >= 0 {
			flush(f.W)
		}
	}
	return out
}

func tileChanged(a, b []byte, stride, x, y, w, h int) bool {
	for row := y; row < y+h; row++ {
		o := row*stride + x*4
		if !bytes.Equal(a[o:o+w*4], b[o:o+w*4]) {
			return true
		}
	}
	return false
}

// encodeRects her dikdörtgeni paralel olarak JPEG'e çevirir.
// Mesaj biçimi: [1][x u16][y u16][w u16][h u16][jpeg...] (little endian)
func encodeRects(f *capture.Frame, rects []rect, quality int) [][]byte {
	out := make([][]byte, len(rects))
	var wg sync.WaitGroup
	sem := make(chan struct{}, runtime.NumCPU())
	for i, r := range rects {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			out[i] = encodeRect(f, r, quality)
		}()
	}
	wg.Wait()
	return out
}

var rgbaPool sync.Pool

func encodeRect(f *capture.Frame, r rect, quality int) []byte {
	need := r.w * r.h * 4
	var pix []byte
	if p, ok := rgbaPool.Get().(*[]byte); ok && cap(*p) >= need {
		pix = (*p)[:need]
	} else {
		pix = make([]byte, need)
	}
	defer rgbaPool.Put(&pix)

	img := &image.RGBA{Pix: pix, Stride: r.w * 4, Rect: image.Rect(0, 0, r.w, r.h)}
	for y := 0; y < r.h; y++ {
		src := f.Pix[(r.y+y)*f.Stride+r.x*4:][:r.w*4]
		dst := pix[y*img.Stride:][:r.w*4]
		for i := 0; i < len(src); i += 4 {
			dst[i] = src[i+2]
			dst[i+1] = src[i+1]
			dst[i+2] = src[i]
			dst[i+3] = 255
		}
	}

	var buf bytes.Buffer
	buf.Grow(9 + need/10)
	var hdr [9]byte
	hdr[0] = msgRect
	binary.LittleEndian.PutUint16(hdr[1:], uint16(r.x))
	binary.LittleEndian.PutUint16(hdr[3:], uint16(r.y))
	binary.LittleEndian.PutUint16(hdr[5:], uint16(r.w))
	binary.LittleEndian.PutUint16(hdr[7:], uint16(r.h))
	buf.Write(hdr[:])
	_ = jpeg.Encode(&buf, img, &jpeg.Options{Quality: quality})
	return buf.Bytes()
}
