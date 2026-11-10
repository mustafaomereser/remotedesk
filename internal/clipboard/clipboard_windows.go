//go:build windows

// Package clipboard, sistem panosunda düz metin okuma/yazma sağlar.
package clipboard

import (
	"errors"
	"runtime"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	user32   = windows.NewLazySystemDLL("user32.dll")
	kernel32 = windows.NewLazySystemDLL("kernel32.dll")

	procOpenClipboard              = user32.NewProc("OpenClipboard")
	procCloseClipboard             = user32.NewProc("CloseClipboard")
	procEmptyClipboard             = user32.NewProc("EmptyClipboard")
	procGetClipboardData           = user32.NewProc("GetClipboardData")
	procSetClipboardData           = user32.NewProc("SetClipboardData")
	procGetClipboardSequenceNumber = user32.NewProc("GetClipboardSequenceNumber")

	procGlobalAlloc  = kernel32.NewProc("GlobalAlloc")
	procGlobalFree   = kernel32.NewProc("GlobalFree")
	procGlobalLock   = kernel32.NewProc("GlobalLock")
	procGlobalUnlock = kernel32.NewProc("GlobalUnlock")
)

const (
	cfUnicodeText = 13
	gmemMoveable  = 0x0002
)

var errBusy = errors.New("pano meşgul")

// Seq, pano her değiştiğinde artan bir sayı döner.
func Seq() uint64 {
	r, _, _ := procGetClipboardSequenceNumber.Call()
	return uint64(r)
}

func open() bool {
	for range 10 {
		if r, _, _ := procOpenClipboard.Call(0); r != 0 {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}

func toPtr(p uintptr) unsafe.Pointer {
	return *(*unsafe.Pointer)(unsafe.Pointer(&p))
}

func Read() (string, error) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	if !open() {
		return "", errBusy
	}
	defer procCloseClipboard.Call()
	h, _, _ := procGetClipboardData.Call(cfUnicodeText)
	if h == 0 {
		return "", nil // metin yok
	}
	p, _, _ := procGlobalLock.Call(h)
	if p == 0 {
		return "", errors.New("GlobalLock başarısız")
	}
	defer procGlobalUnlock.Call(h)
	return windows.UTF16PtrToString((*uint16)(toPtr(p))), nil
}

func Write(text string) error {
	u16, err := windows.UTF16FromString(text)
	if err != nil {
		return err
	}
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	h, _, _ := procGlobalAlloc.Call(gmemMoveable, uintptr(len(u16)*2))
	if h == 0 {
		return errors.New("GlobalAlloc başarısız")
	}
	p, _, _ := procGlobalLock.Call(h)
	if p == 0 {
		procGlobalFree.Call(h)
		return errors.New("GlobalLock başarısız")
	}
	copy(unsafe.Slice((*uint16)(toPtr(p)), len(u16)), u16)
	procGlobalUnlock.Call(h)

	if !open() {
		procGlobalFree.Call(h)
		return errBusy
	}
	defer procCloseClipboard.Call()
	procEmptyClipboard.Call()
	if r, _, _ := procSetClipboardData.Call(cfUnicodeText, h); r == 0 {
		procGlobalFree.Call(h)
		return errors.New("SetClipboardData başarısız")
	}
	return nil // h artık sistemin
}
