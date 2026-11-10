//go:build windows

package capture

import (
	"sync"
	"time"
)

// Paylaşımlı DXGI kopyalaması: Windows aynı monitör için ikinci bir Desktop Duplication
// açtırmaz (E_INVALIDARG). Toplantıda aynı monitörü izleyen tüm oturumlar tek kopyalamayı
// paylaşır: kareyi kim önce alırsa ortak tampona yazar, diğerleri oradan kopyalar.
// Tek izleyicide kare doğrudan izleyicinin tamponuna yazılır (ek kopya yok).

type sharedDup struct {
	mu     sync.Mutex
	d      Display
	refs   int
	x      *dxgiDup
	retry  time.Time
	buf    []byte // ortak tampon (birden fazla izleyici varken)
	seq    uint64 // her yeni karede artar
	bufSeq uint64 // buf'taki karenin numarası (0: geçersiz)
}

var (
	dupMu sync.Mutex
	dups  = map[Display]*sharedDup{}
)

func acquireDup(d Display) *sharedDup {
	dupMu.Lock()
	defer dupMu.Unlock()
	s := dups[d]
	if s == nil {
		s = &sharedDup{d: d}
		dups[d] = s
	}
	s.mu.Lock()
	s.refs++
	if s.refs == 2 && s.x != nil {
		// Paylaşıma geçiş: kopyalamayı yeniden aç ki ilk kare (tam ekran) ortak tampona gelsin;
		// ekran değişmiyorsa DXGI yeni kare vermez ve yeni izleyici boş kalırdı.
		s.x.close()
		s.x = nil
		s.retry = time.Time{}
	}
	s.mu.Unlock()
	return s
}

func (s *sharedDup) release() {
	dupMu.Lock()
	defer dupMu.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refs--
	if s.refs > 0 {
		return
	}
	if s.x != nil {
		s.x.close()
		s.x = nil
	}
	s.buf = nil
	delete(dups, s.d)
}

// grab, kareyi dst'ye (w×h BGRA) getirir. last, bu izleyicinin en son aldığı kare
// numarasıdır; değişmediyse dxgiSame döner.
func (s *sharedDup) grab(dst []byte, w, h int, wait time.Duration, last *uint64) dxgiResult {
	s.mu.Lock()
	defer s.mu.Unlock()
	shared := s.refs > 1
	need := w * h * 4

	// Başka bir izleyici bu arada daha yeni bir kare aldıysa onu kullan.
	if s.bufSeq != 0 && s.bufSeq == s.seq && *last != s.seq && len(s.buf) >= need {
		copy(dst[:need], s.buf[:need])
		*last = s.seq
		if shared {
			return dxgiNew
		}
	}

	if s.x == nil {
		if time.Now().Before(s.retry) {
			return dxgiFail
		}
		if s.x = openDup(s.d); s.x == nil {
			s.retry = time.Now().Add(2 * time.Second)
			return dxgiFail
		}
	}

	target := dst
	if shared {
		if len(s.buf) < need {
			s.buf = make([]byte, need)
		}
		target = s.buf
	}
	switch s.x.grab(target, w, h, wait) {
	case dxgiNew:
		s.seq++
		if shared {
			s.bufSeq = s.seq
			copy(dst[:need], s.buf[:need])
		} else {
			s.bufSeq = 0 // tek izleyici: ortak tampon güncel değil
		}
		*last = s.seq
		return dxgiNew
	case dxgiSame:
		if *last == s.seq {
			return dxgiSame
		}
		// Bu izleyicinin elinde geçerli kare yok (ekran durgun, paylaşıma yeni katıldı):
		// bu seferlik GDI ile alınır; ekran değişmedikçe o kare geçerli kalır.
		*last = s.seq
		return dxgiStale
	}
	s.x.close()
	s.x = nil
	s.retry = time.Now().Add(time.Second)
	return dxgiFail
}
