// Package desktop; yerel kullanıcının etkinliğini algılar ve uzaktan kontrol edenin adını
// ekranda (imlecin/işlem noktasının yanında) gösterir.
//
// Yerel etkinlik nasıl ayırt edilir: işletim sistemi bizim gönderdiğimiz sahte olayları da
// "son giriş" olarak sayar. İki ipucu kullanılır:
//  1. İmleci en son nereye koyduğumuzu biliyoruz; imleç oradan kaymışsa yerel kullanıcı oynatmıştır.
//  2. Son giriş zamanı, bizim son gönderimimizden belirgin şekilde sonraysa yereldir.
package desktop

import (
	"sync"
	"time"
)

var (
	mu         sync.Mutex
	lastInject time.Time
	lastLocal  time.Time
	expX, expY int // imleci en son koyduğumuz yer (masaüstü koordinatı)
	hasExp     bool

	labelName  string
	labelX     int
	labelY     int
	labelUntil time.Time
	labelGhost bool
	labelLoop  bool
)

// MarkInjected, sahte bir klavye/tekerlek olayı gönderilmeden hemen önce çağrılır.
func MarkInjected() {
	mu.Lock()
	lastInject = time.Now()
	mu.Unlock()
}

// MarkPointer, imleç gerçekten (x, y)'ye taşınmadan hemen önce çağrılır.
func MarkPointer(x, y int) {
	mu.Lock()
	lastInject = time.Now()
	expX, expY, hasExp = x, y, true
	mu.Unlock()
}

// LocalActive, yerel kullanıcı son `within` süresi içinde fare/klavye kullandıysa true döner.
func LocalActive(within time.Duration) bool {
	age := lastInputAge()
	cx, cy, cok := cursorPos()
	mu.Lock()
	defer mu.Unlock()
	now := time.Now()

	if ev := now.Add(-age); ev.After(lastInject.Add(60*time.Millisecond)) && ev.After(lastLocal) {
		lastLocal = ev
	}
	if cok && hasExp && now.Sub(lastInject) > 40*time.Millisecond {
		if abs(cx-expX) > 4 || abs(cy-expY) > 4 {
			lastLocal = now
			expX, expY = cx, cy // aynı kaymayı tekrar tekrar saymamak için
		}
	}
	return now.Sub(lastLocal) < within
}

func abs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

// RemoteActivity, uzaktaki kişinin (x, y) noktasında işlem yaptığını bildirir; ad etiketi
// orada görünür kalır (normal modda 1,5 sn). ghost=true: gerçek imleç orada değil
// (simülasyon modu), etiketle birlikte bir "hayalet" ok imleci çizilir.
func RemoteActivity(name string, x, y int, ghost bool) {
	if name == "" {
		return
	}
	mu.Lock()
	labelName, labelX, labelY, labelGhost = name, x, y, ghost
	// Hayalet imleç, gerçek imleç gibi yerinde kalmalı; normal etiket kısa süre sonra kaybolur.
	if ghost {
		labelUntil = time.Now().Add(30 * time.Second)
	} else {
		labelUntil = time.Now().Add(1500 * time.Millisecond)
	}
	start := !labelLoop
	labelLoop = true
	mu.Unlock()
	if start {
		go runLabel()
	}
}

// HideLabel, etiketi hemen gizler (oturum bittiğinde).
func HideLabel() {
	mu.Lock()
	labelUntil = time.Time{}
	mu.Unlock()
}

func runLabel() {
	t := time.NewTicker(33 * time.Millisecond)
	defer t.Stop()
	var shownName string
	var sx, sy int
	var sg bool
	for range t.C {
		mu.Lock()
		name, x, y, ghost, until := labelName, labelX, labelY, labelGhost, labelUntil
		if time.Now().After(until) {
			labelLoop = false
			mu.Unlock()
			hideLabel()
			return
		}
		mu.Unlock()
		if name != shownName || x != sx || y != sy || ghost != sg {
			showLabel(name, x, y, ghost)
			shownName, sx, sy, sg = name, x, y, ghost
		}
	}
}

// InkAdd, ekrana çizim katmanına bir çizginin yeni noktalarını ekler. Noktalar (x,y çiftleri)
// masaüstündeki (dx,dy,dw,dh) monitörüne göre 0..10000 arasıdır. laser=true ise çizgi
// bırakıldıktan kısa süre sonra kaybolur.
func InkAdd(id string, laser bool, color string, width int, dx, dy, dw, dh int, pts []int, end bool) {
	inkAdd(id, laser, color, width, dx, dy, dw, dh, pts, end)
}

// InkRemove, bir çizgiyi siler (geri al).
func InkRemove(id string) { inkRemove(id) }

// InkClear, tüm çizgileri siler.
func InkClear() { inkClear() }
