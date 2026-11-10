// Package capture, ekran yakalamayı platformdan bağımsız bir arayüzle sunar.
package capture

// Frame, BGRA formatında üstten-alta bir ekran karesidir.
// Pix, bir sonraki Capture çağrısında üzerine yazılır.
type Frame struct {
	W, H, Stride int
	Pix          []byte
}

// Display bir monitördür. X/Y/W/H, işletim sisteminin masaüstü koordinatlarıdır
// (Windows'ta fiziksel piksel, macOS'ta "point"). Yakalanan karenin boyutu farklı olabilir
// (ör. Retina ekranlarda 2 katı).
type Display struct {
	ID      int  `json:"id"`
	X       int  `json:"x"`
	Y       int  `json:"y"`
	W       int  `json:"w"`
	H       int  `json:"h"`
	Primary bool `json:"primary"`
}

// PrimaryIndex, listedeki ana monitörün sırasını döner.
func PrimaryIndex(ds []Display) int {
	for i, d := range ds {
		if d.Primary {
			return i
		}
	}
	return 0
}

// SameDisplays, iki listenin aynı olup olmadığını söyler.
func SameDisplays(a, b []Display) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
