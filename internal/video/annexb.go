package video

// Frame, tek bir kareye ait H.264 erişim birimidir (Annex B, 4 baytlık başlangıç kodlarıyla).
type Frame struct {
	Data []byte
	Key  bool
}

// auParser, ffmpeg'in ham H.264 çıktısını NAL birimlerine, onları da karelere böler.
type auParser struct {
	buf    []byte // henüz tamamlanmamış veri (bir başlangıç koduyla başlar)
	au     []byte
	hasVCL bool
	key    bool
}

type startCode struct{ code, payload int }

func findStarts(b []byte) []startCode {
	var out []startCode
	for i := 0; i+2 < len(b); i++ {
		if b[i] == 0 && b[i+1] == 0 && b[i+2] == 1 {
			code := i
			if i > 0 && b[i-1] == 0 {
				code = i - 1
			}
			out = append(out, startCode{code, i + 3})
			i += 2
		}
	}
	return out
}

// feed, gelen veriyi işler; tamamlanan kareleri emit ile bildirir.
// Son NAL birimi, bir sonraki başlangıç kodu görülene (veya flush çağrılana) kadar bekletilir.
func (p *auParser) feed(b []byte, emit func(Frame)) {
	p.buf = append(p.buf, b...)
	starts := findStarts(p.buf)
	if len(starts) == 0 {
		return
	}
	for i := 0; i+1 < len(starts); i++ {
		p.addNAL(p.buf[starts[i].payload:starts[i+1].code], emit)
	}
	last := starts[len(starts)-1].code
	p.buf = append(p.buf[:0], p.buf[last:]...)
}

// flush, bekleyen son NAL'ı da tamamlanmış sayar ve kareyi gönderir.
// ffmpeg bir kareyi tek seferde yazdığı için kısa bir sessizlik "kare bitti" demektir.
func (p *auParser) flush(emit func(Frame)) {
	if starts := findStarts(p.buf); len(starts) > 0 {
		p.addNAL(p.buf[starts[len(starts)-1].payload:], emit)
	}
	p.buf = p.buf[:0]
	p.emitAU(emit)
}

func (p *auParser) addNAL(nal []byte, emit func(Frame)) {
	if len(nal) == 0 {
		return
	}
	switch typ := nal[0] & 0x1F; typ {
	case 1, 5: // dilim (slice)
		// first_mb_in_slice == 0 ise yeni bir kare başlıyor.
		if p.hasVCL && len(nal) > 1 && nal[1]&0x80 != 0 {
			p.emitAU(emit)
		}
		p.hasVCL = true
		if typ == 5 {
			p.key = true
		}
	case 6, 7, 8, 9: // SEI, SPS, PPS, AUD yeni karenin başıdır
		if p.hasVCL {
			p.emitAU(emit)
		}
	}
	p.au = append(p.au, 0, 0, 0, 1)
	p.au = append(p.au, nal...)
}

func (p *auParser) emitAU(emit func(Frame)) {
	if !p.hasVCL {
		return // SPS/PPS bir sonraki kareyle birlikte gidecek
	}
	emit(Frame{Data: p.au, Key: p.key})
	p.au = nil
	p.hasVCL = false
	p.key = false
}
