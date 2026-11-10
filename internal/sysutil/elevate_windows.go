//go:build windows

package sysutil

import (
	"os"
	"strconv"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	procOpenInputDesktop  = user32.NewProc("OpenInputDesktop")
	procCloseDesktop      = user32.NewProc("CloseDesktop")
	procGetUserObjectInfo = user32.NewProc("GetUserObjectInformationW")
	shell32               = windows.NewLazySystemDLL("shell32.dll")
	procShellExecuteW     = shell32.NewProc("ShellExecuteW")
)

// SecureDesktopActive, UAC onay ekranı ya da kilit ekranı (güvenli masaüstü) açıksa
// true döner. Bu sırada normal kullanıcı süreci ekranı göremez ve fare/klavye gönderemez.
func SecureDesktopActive() bool {
	const desktopReadObjects = 0x0001
	h, _, _ := procOpenInputDesktop.Call(0, 0, desktopReadObjects)
	if h == 0 {
		return true // girdi masaüstüne erişim yok: Winlogon / güvenli masaüstü
	}
	defer procCloseDesktop.Call(h)
	var buf [64]uint16
	var need uint32
	const uoiName = 2
	if r, _, _ := procGetUserObjectInfo.Call(h, uoiName, uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)*2), uintptr(unsafe.Pointer(&need))); r == 0 {
		return false
	}
	return !strings.EqualFold(windows.UTF16ToString(buf[:]), "Default")
}

// IsElevated, süreç yönetici olarak çalışıyorsa true döner.
func IsElevated() bool { return windows.GetCurrentProcessToken().IsElevated() }

// CanElevate, "Yönetici olarak yeniden başlat" seçeneğinin sunulup sunulmayacağını söyler.
func CanElevate() bool { return !IsElevated() }

// CanRestart: Windows'ta uygulama kendini yeniden başlatabilir.
func CanRestart() bool { return true }

// Restart, uygulamayı aynı argümanlarla yeniden açar; yeni süreç bu süreç kapanana
// kadar bekler (-waitpid). Çağıran ardından kendini kapatmalıdır.
func Restart() error { return restart("open", "") }

// RestartElevated, uygulamayı yönetici olarak (UAC onayıyla) yeniden açar.
func RestartElevated() error { return restart("runas", "") }

// RestartExe, verilen exe'yi (güncelleme sonrası yeni dosya) aynı argümanlarla açar.
func RestartExe(exe string) error { return restart("open", exe) }

func restart(verb, exe string) error {
	if exe == "" {
		var err error
		if exe, err = os.Executable(); err != nil {
			return err
		}
	}
	args := []string{"-waitpid", strconv.Itoa(os.Getpid())}
	for i := 1; i < len(os.Args); i++ {
		if os.Args[i] == "-waitpid" || os.Args[i] == "--waitpid" {
			i++ // önceki yeniden başlatmanın argümanı
			continue
		}
		args = append(args, os.Args[i])
	}
	params := windows.ComposeCommandLine(args)
	v, _ := windows.UTF16PtrFromString(verb)
	f, _ := windows.UTF16PtrFromString(exe)
	p, _ := windows.UTF16PtrFromString(params)
	const swShowNormal = 1
	r, _, err := procShellExecuteW.Call(0, uintptr(unsafe.Pointer(v)), uintptr(unsafe.Pointer(f)), uintptr(unsafe.Pointer(p)), 0, swShowNormal)
	if r <= 32 { // ShellExecute hata kodları <= 32 (ör. kullanıcı UAC'yi reddetti)
		return err
	}
	return nil
}

// WaitPID, verilen süreç kapanana kadar (en çok 15 sn) bekler.
func WaitPID(pid int) {
	h, err := windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(pid))
	if err != nil {
		return
	}
	defer windows.CloseHandle(h)
	windows.WaitForSingleObject(h, 15000)
}
