//go:build darwin

package app

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
)

func findBrowser() string {
	home, _ := os.UserHomeDir()
	apps := []string{
		"Google Chrome.app/Contents/MacOS/Google Chrome",
		"Microsoft Edge.app/Contents/MacOS/Microsoft Edge",
		"Brave Browser.app/Contents/MacOS/Brave Browser",
		"Chromium.app/Contents/MacOS/Chromium",
	}
	for _, base := range []string{"/Applications", filepath.Join(home, "Applications")} {
		for _, a := range apps {
			p := filepath.Join(base, a)
			if st, err := os.Stat(p); err == nil && !st.IsDir() {
				return p
			}
		}
	}
	return ""
}

// openAppWindow, Chromium tabanlı bir tarayıcıyı adres çubuğu olmayan "uygulama" modunda açar.
// Hiçbiri yoksa çağıran Safari'ye (varsayılan tarayıcı) düşer.
func openAppWindow(url, profileDir string) (*exec.Cmd, error) {
	bin := findBrowser()
	if bin == "" {
		return nil, errors.New("Chrome/Edge/Brave bulunamadı")
	}
	os.MkdirAll(profileDir, 0o755)
	cmd := exec.Command(bin,
		"--app="+url,
		"--user-data-dir="+profileDir,
		"--no-first-run",
		"--no-default-browser-check",
		"--disable-features=Translate",
		"--window-size=980,700",
	)
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return cmd, nil
}

// closeBrowser, uygulamaya özel profille açılan tarayıcıyı düzgünce kapatır.
func closeBrowser(cmd *exec.Cmd) {
	if cmd.Process != nil {
		cmd.Process.Signal(syscall.SIGTERM)
	}
}

func openDefaultBrowser(url string) {
	exec.Command("open", url).Start()
}
