package hub

import (
	"context"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// Canlı sunucuda anlık çevrimiçi bildirimi (yalnızca RD_HUB ayarlıysa çalışır).
func TestPresenceLive(t *testing.T) {
	server := os.Getenv("RD_HUB")
	if server == "" {
		t.Skip("RD_HUB yok")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	a, err := LoadDevice(ctx, t.TempDir(), server)
	if err != nil {
		t.Fatal(err)
	}
	b, err := LoadDevice(ctx, t.TempDir(), server)
	if err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	var events []string
	got := make(chan struct{}, 16)
	bc := &Client{Dev: b, OnState: func(bool) {}, OnOpen: nil,
		Hello: func() (bool, []string) { return false, []string{a.ID} },
		OnPresence: func(s map[string]bool) {
			mu.Lock()
			for id, on := range s {
				if id == a.ID {
					if on {
						events = append(events, "açık")
					} else {
						events = append(events, "kapalı")
					}
				}
			}
			mu.Unlock()
			got <- struct{}{}
		}}
	bctx, bstop := context.WithCancel(ctx)
	defer bstop()
	go bc.Run(bctx)
	wait := func(what string) {
		select {
		case <-got:
		case <-time.After(15 * time.Second):
			t.Fatalf("%s bildirimi gelmedi", what)
		}
	}
	wait("ilk durum")

	avail := true
	ac := &Client{Dev: a, OnState: func(bool) {}, Hello: func() (bool, []string) { return avail, nil }}
	actx, astop := context.WithCancel(ctx)
	go ac.Run(actx)
	wait("A açıldı")
	time.Sleep(500 * time.Millisecond)
	ac.SetAvailable(false)
	wait("A bağlantılara kapandı")
	ac.SetAvailable(true)
	wait("A tekrar açıldı")
	astop()
	wait("A kapandı")

	mu.Lock()
	defer mu.Unlock()
	t.Logf("B'nin gördüğü A durumları: %s", strings.Join(events, " -> "))
	want := "kapalı -> açık -> kapalı -> açık -> kapalı"
	if strings.Join(events, " -> ") != want {
		t.Errorf("beklenen %s", want)
	}
}
