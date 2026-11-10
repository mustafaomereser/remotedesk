//go:build windows

package app

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
)

func findBrowser() string {
	var cands []string
	for _, env := range []string{"ProgramFiles(x86)", "ProgramFiles", "LocalAppData"} {
		base := os.Getenv(env)
		if base == "" {
			continue
		}
		cands = append(cands,
			filepath.Join(base, `Microsoft\Edge\Application\msedge.exe`),
			filepath.Join(base, `Google\Chrome\Application\chrome.exe`),
		)
	}
	for _, c := range cands {
		if st, err := os.Stat(c); err == nil && !st.IsDir() {
			return c
		}
	}
	return ""
}

// openAppWindow, Edge/Chrome'u adres çubuğu olmayan "uygulama" modunda açar.
// Ayrı profil klasörü sayesinde süreç pencere kapanana kadar yaşar.
func openAppWindow(url, profileDir string) (*exec.Cmd, error) {
	bin := findBrowser()
	if bin == "" {
		return nil, errors.New("Edge veya Chrome bulunamadı")
	}
	os.MkdirAll(profileDir, 0o755)
	cmd := exec.Command(bin,
		"--app="+url,
		"--user-data-dir="+profileDir,
		"--no-first-run",
		"--no-default-browser-check",
		"--disable-features=Translate,msEdgeSidebarV2",
		"--window-size=980,700",
	)
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return cmd, nil
}

// Windows'ta Edge penceresi kapanınca süreç zaten biter.
func closeBrowser(cmd *exec.Cmd) {}

func openDefaultBrowser(url string) {
	exec.Command("rundll32", "url.dll,FileProtocolHandler", url).Start()
}
