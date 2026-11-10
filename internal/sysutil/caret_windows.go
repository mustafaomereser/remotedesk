//go:build windows

package sysutil

import (
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	procGetGUIThreadInfo = user32.NewProc("GetGUIThreadInfo")
	procGetClassNameW    = user32.NewProc("GetClassNameW")
)

type guiThreadInfo struct {
	Size                                               uint32
	Flags                                              uint32
	Active, Focus, Capture, MenuOwner, MoveSize, Caret uintptr
	CaretL, CaretT, CaretR, CaretB                     int32
}

// TextFocus, odaktaki pencerede yazı imleci (caret) var mı ya da odak bilinen bir yazı
// kutusu mu, onu söyler. Telefondan dokununca klavyeyi kendiliğinden açmak için kullanılır.
// Kesin değildir: kendi imlecini çizen uygulamalarda (bazı tarayıcı alanları) false döner.
func TextFocus() bool {
	gi := guiThreadInfo{}
	gi.Size = uint32(unsafe.Sizeof(gi))
	if r, _, _ := procGetGUIThreadInfo.Call(0, uintptr(unsafe.Pointer(&gi))); r == 0 {
		return false
	}
	if gi.Caret != 0 {
		return true
	}
	if gi.Focus == 0 {
		return false
	}
	buf := make([]uint16, 64)
	n, _, _ := procGetClassNameW.Call(gi.Focus, uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
	cls := strings.ToLower(windows.UTF16ToString(buf[:n]))
	for _, p := range []string{"edit", "richedit", "scintilla", "consolewindowclass", "cascadia_hosting_window_class"} {
		if strings.HasPrefix(cls, p) {
			return true
		}
	}
	return false
}
