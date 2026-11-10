package capture

import (
	"runtime"
	"sync"
)

// Half, src'nin 2x2 kutu filtresiyle yarım boyutlu kopyasını dst'ye yazar.
// Retina ekranlarda görüntüyü "point" çözünürlüğüne indirir: 4 kat daha az veri,
// metin hâlâ net. İş satır gruplarına bölünüp tüm çekirdeklerde yapılır.
func Half(src, dst *Frame) {
	w, h := src.W/2, src.H/2
	need := w * h * 4
	if cap(dst.Pix) < need {
		dst.Pix = make([]byte, need)
	}
	dst.Pix = dst.Pix[:need]
	dst.W, dst.H, dst.Stride = w, h, w*4

	workers := max(1, min(runtime.NumCPU(), h/16))
	var wg sync.WaitGroup
	for k := range workers {
		y0, y1 := h*k/workers, h*(k+1)/workers
		wg.Add(1)
		go func() {
			defer wg.Done()
			for y := y0; y < y1; y++ {
				s0 := src.Pix[2*y*src.Stride:]
				s1 := src.Pix[(2*y+1)*src.Stride:]
				d := dst.Pix[y*dst.Stride : y*dst.Stride+w*4]
				for x := 0; x < w; x++ {
					i, o := x*8, x*4
					d[o] = byte((uint(s0[i]) + uint(s0[i+4]) + uint(s1[i]) + uint(s1[i+4]) + 2) >> 2)
					d[o+1] = byte((uint(s0[i+1]) + uint(s0[i+5]) + uint(s1[i+1]) + uint(s1[i+5]) + 2) >> 2)
					d[o+2] = byte((uint(s0[i+2]) + uint(s0[i+6]) + uint(s1[i+2]) + uint(s1[i+6]) + 2) >> 2)
					d[o+3] = 255
				}
			}
		}()
	}
	wg.Wait()
}
