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
