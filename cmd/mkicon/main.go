// mkicon: uygulama ikonunu (packaging/remotedesk.png ve .icns) üretir.
//
//	go run ./cmd/mkicon
package main

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/color"
	"image/png"
	"log"
	"math"
	"os"
	"path/filepath"
)

// sdRoundRect: merkezi (cx,cy), yarı boyutu (hx,hy), köşe yarıçapı r olan
// yuvarlak dikdörtgene işaretli uzaklık (içeride negatif).
func sdRoundRect(x, y, cx, cy, hx, hy, r float64) float64 {
	qx := math.Abs(x-cx) - hx + r
	qy := math.Abs(y-cy) - hy + r
	ox, oy := math.Max(qx, 0), math.Max(qy, 0)
	return math.Hypot(ox, oy) + math.Min(math.Max(qx, qy), 0) - r
}

func coverage(d, px float64) float64 { // kenar yumuşatma
	return math.Max(0, math.Min(1, 0.5-d/px))
}

func lerp(a, b, t float64) float64 { return a + (b-a)*t }

// render, 1024'lük tasarım ızgarasını size×size piksele çizer.
func render(size int) *image.NRGBA {
	img := image.NewNRGBA(image.Rect(0, 0, size, size))
	s := 1024 / float64(size) // piksel başına tasarım birimi
	for py := 0; py < size; py++ {
		for px := 0; px < size; px++ {
			x, y := (float64(px)+0.5)*s, (float64(py)+0.5)*s

			// Zemin: Apple ikon ızgarası (824x824, kenarlarda 100 boşluk)
			bg := coverage(sdRoundRect(x, y, 512, 512, 412, 412, 185), s)
			if bg == 0 {
				continue
			}
			t := math.Max(0, math.Min(1, (x+y)/2048))
			r, g, b := lerp(0xEF, 0xFF, t), lerp(0x4B, 0x8A, t), lerp(0x3F, 0x5C, t)

			// Monitör: çerçeve (halka) + ayak + taban
			screen := math.Abs(sdRoundRect(x, y, 512, 455, 270, 185, 44)) - 24
			neck := sdRoundRect(x, y, 512, 690, 34, 52, 8)
			base := sdRoundRect(x, y, 512, 742, 130, 22, 22)
			// Ekranın içinde imleç benzeri bir ok: üçgen yerine küçük yuvarlak kare + çizgi yerine nokta
			dot := math.Hypot(x-512, y-455) - 46
			glyph := math.Min(math.Min(screen, neck), math.Min(base, dot))
			w := coverage(glyph, s)

			r, g, b = lerp(r, 255, w), lerp(g, 255, w), lerp(b, 255, w)
			img.SetNRGBA(px, py, color.NRGBA{uint8(r), uint8(g), uint8(b), uint8(bg * 255)})
		}
	}
	return img
}

func pngBytes(size int) []byte {
	var buf bytes.Buffer
	if err := png.Encode(&buf, render(size)); err != nil {
		log.Fatal(err)
	}
	return buf.Bytes()
}

func main() {
	dir := "packaging"
	os.MkdirAll(dir, 0o755)
	if err := os.WriteFile(filepath.Join(dir, "remotedesk.png"), pngBytes(1024), 0o644); err != nil {
		log.Fatal(err)
	}

	// ICNS: PNG gömülü girdiler.
	entries := []struct {
		typ  string
		size int
	}{
		{"ic11", 32}, {"ic12", 64}, {"ic07", 128}, {"ic13", 256},
		{"ic08", 256}, {"ic14", 512}, {"ic09", 512}, {"ic10", 1024},
	}
	var body bytes.Buffer
	for _, e := range entries {
		data := pngBytes(e.size)
		body.WriteString(e.typ)
		binary.Write(&body, binary.BigEndian, uint32(8+len(data)))
		body.Write(data)
	}
	var out bytes.Buffer
	out.WriteString("icns")
	binary.Write(&out, binary.BigEndian, uint32(8+body.Len()))
	out.Write(body.Bytes())
	if err := os.WriteFile(filepath.Join(dir, "remotedesk.icns"), out.Bytes(), 0o644); err != nil {
		log.Fatal(err)
	}
	log.Printf("yazıldı: %s/remotedesk.png, %s/remotedesk.icns", dir, dir)
}
