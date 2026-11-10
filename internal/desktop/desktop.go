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

// RemoteActivity, uzaktaki kişinin (x, y) noktasında işlem yaptığını bildirir;
// ad etiketi orada 1,5 sn görünür kalır.
func RemoteActivity(name string, x, y int) {
	if name == "" {
		return
	}
	mu.Lock()
	labelName, labelX, labelY = name, x, y
	labelUntil = time.Now().Add(1500 * time.Millisecond)
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
	for range t.C {
		mu.Lock()
		name, x, y, until := labelName, labelX, labelY, labelUntil
		if time.Now().After(until) {
			labelLoop = false
			mu.Unlock()
			hideLabel()
			return
		}
		mu.Unlock()
		if name != shownName || x != sx || y != sy {
			showLabel(name, x, y)
			shownName, sx, sy = name, x, y
		}
	}
}
