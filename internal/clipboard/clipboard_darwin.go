//go:build darwin

// Package clipboard, sistem panosunda düz metin okuma/yazma sağlar.
package clipboard

import (
	"os"
	"os/exec"
	"strings"
	"sync"

	"github.com/ebitengine/purego"
	"github.com/ebitengine/purego/objc"
)

var (
	initOnce sync.Once
	pb       objc.ID
	selCount objc.SEL
)

func setup() {
	initOnce.Do(func() {
		if _, err := purego.Dlopen("/System/Library/Frameworks/AppKit.framework/AppKit", purego.RTLD_NOW|purego.RTLD_GLOBAL); err != nil {
			return
		}
		cls := objc.GetClass("NSPasteboard")
		if cls == 0 {
			return
		}
		pb = objc.ID(cls).Send(objc.RegisterName("generalPasteboard"))
		selCount = objc.RegisterName("changeCount")
	})
}

// Seq, NSPasteboard.changeCount değerini döner (pano değişince artar).
func Seq() uint64 {
	setup()
	if pb == 0 {
		return 0
	}
	return uint64(objc.Send[int](pb, selCount))
}

// pbpaste/pbcopy, metin kodlamasını locale'den aldığı için UTF-8 zorlanır.
func utf8Env() []string {
	return append(os.Environ(), "LANG=en_US.UTF-8", "LC_ALL=en_US.UTF-8")
}

func Read() (string, error) {
	cmd := exec.Command("pbpaste")
	cmd.Env = utf8Env()
	out, err := cmd.Output()
	return string(out), err
}

func Write(text string) error {
	cmd := exec.Command("pbcopy")
	cmd.Env = utf8Env()
	cmd.Stdin = strings.NewReader(text)
	return cmd.Run()
}
