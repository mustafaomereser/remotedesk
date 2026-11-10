// Package input, uzaktan gelen fare/klavye olaylarını işletim sistemine uygular.
package input

// Mapping, viewer'daki görüntü pikselini masaüstü koordinatına çevirir:
// masaüstü = O + piksel * S
type Mapping struct {
	OX, OY float64
	SX, SY float64
}

func (m Mapping) apply(x, y int) (float64, float64) {
	sx, sy := m.SX, m.SY
	if sx == 0 {
		sx = 1
	}
	if sy == 0 {
		sy = 1
	}
	return m.OX + float64(x)*sx, m.OY + float64(y)*sy
}

// Desktop, görüntü pikselini masaüstü koordinatına çevirir (ad etiketinin konumu için).
func (in *Injector) Desktop(x, y int) (int, int) {
	in.mu.Lock()
	defer in.mu.Unlock()
	dx, dy := in.m.apply(x, y)
	return int(dx), int(dy)
}

// ButtonDown / KeyDown: olay normal yoldan basılmışsa bırakma da normal yoldan gitmeli.
func (in *Injector) ButtonDown(b int) bool {
	in.mu.Lock()
	defer in.mu.Unlock()
	return in.buttons[b]
}

func (in *Injector) KeyDown(code string) bool {
	in.mu.Lock()
	defer in.mu.Unlock()
	return in.keys[code]
}
