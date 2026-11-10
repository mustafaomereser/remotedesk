//go:build windows

// Package sysutil, işletim sistemine özel küçük yardımcıları içerir.
package sysutil

import (
	"os"
	"os/exec"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	user32                  = windows.NewLazySystemDLL("user32.dll")
	procSetDpiCtx           = user32.NewProc("SetProcessDpiAwarenessContext")
	procSetDPIAware         = user32.NewProc("SetProcessDPIAware")
	procFindWindowW         = user32.NewProc("FindWindowW")
	procIsIconic            = user32.NewProc("IsIconic")
	procShowWindow          = user32.NewProc("ShowWindow")
	procSetForegroundWindow = user32.NewProc("SetForegroundWindow")
	procFlashWindowEx       = user32.NewProc("FlashWindowEx")
)

// SetDPIAware, ekran yakalamanın ölçeklenmemiş gerçek piksellerle yapılmasını sağlar.
func SetDPIAware() {
	if procSetDpiCtx.Find() == nil {
		// DPI_AWARENESS_CONTEXT_PER_MONITOR_AWARE_V2 = -4
		if r, _, _ := procSetDpiCtx.Call(^uintptr(3)); r != 0 {
			return
		}
	}
	procSetDPIAware.Call()
}

// Alert, kullanıcıya bir hata penceresi gösterir (konsolsuz derlemede tek geri bildirim yolu).
func Alert(title, msg string) {
	t, _ := windows.UTF16PtrFromString(title)
	m, _ := windows.UTF16PtrFromString(msg)
	windows.MessageBox(0, m, t, windows.MB_OK|windows.MB_ICONERROR)
}

// HideWindow, alt sürecin konsol penceresi açmasını engeller.
func HideWindow(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000} // CREATE_NO_WINDOW
}

// Permissions, eksik sistem izinleri için uyarı metinleri döner (Windows'ta gerekmez).
func Permissions() []string { return nil }

// ConsoleUTF8, konsol çıktısını UTF-8 yapar (Türkçe karakterler için).
func ConsoleUTF8() {
	windows.SetConsoleOutputCP(65001)
}

// Job, bir alt sürecin bu süreçle birlikte ölmesini sağlayan job object'tir.
type Job struct{ h windows.Handle }

func (j *Job) Close() {
	if j != nil && j.h != 0 {
		windows.CloseHandle(j.h)
		j.h = 0
	}
}

// KillWithParent, p sürecini bu uygulama kapandığında (çökse bile) otomatik sonlandırır.
func KillWithParent(p *os.Process) (*Job, error) {
	h, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return nil, err
	}
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{
		BasicLimitInformation: windows.JOBOBJECT_BASIC_LIMIT_INFORMATION{
			LimitFlags: windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE,
		},
	}
	if _, err := windows.SetInformationJobObject(h, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil {
		windows.CloseHandle(h)
		return nil, err
	}
	ph, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(p.Pid))
	if err != nil {
		windows.CloseHandle(h)
		return nil, err
	}
	defer windows.CloseHandle(ph)
	if err := windows.AssignProcessToJobObject(h, ph); err != nil {
		windows.CloseHandle(h)
		return nil, err
	}
	return &Job{h: h}, nil
}

type flashInfo struct {
	Size    uint32
	Hwnd    uintptr
	Flags   uint32
	Count   uint32
	Timeout uint32
}

// Attention, verilen başlıktaki pencereyi öne getirip görev çubuğunda yanıp söndürür.
func Attention(title string) {
	t, err := windows.UTF16PtrFromString(title)
	if err != nil {
		return
	}
	hwnd, _, _ := procFindWindowW.Call(0, uintptr(unsafe.Pointer(t)))
	if hwnd == 0 {
		return
	}
	if r, _, _ := procIsIconic.Call(hwnd); r != 0 {
		procShowWindow.Call(hwnd, 9) // SW_RESTORE
	}
	procSetForegroundWindow.Call(hwnd)
	fi := flashInfo{Hwnd: hwnd, Flags: 0x3 | 0xC} // FLASHW_ALL | FLASHW_TIMERNOFG
	fi.Size = uint32(unsafe.Sizeof(fi))
	procFlashWindowEx.Call(uintptr(unsafe.Pointer(&fi)))
}
