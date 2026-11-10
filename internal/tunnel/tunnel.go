// Package tunnel, cloudflared quick tunnel (trycloudflare.com) yönetir.
package tunnel

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	"remotedesk/internal/sysutil"
)

var urlRe = regexp.MustCompile(`https://[a-z0-9-]+\.trycloudflare\.com`)

func exeName() string {
	if runtime.GOOS == "windows" {
		return "cloudflared.exe"
	}
	return "cloudflared"
}

// Find, cloudflared'ı PATH'te, bilinen klasörlerde, uygulamanın yanında veya veri klasöründe arar.
func Find(dataDir string) string {
	if p, err := exec.LookPath("cloudflared"); err == nil {
		return p
	}
	var cands []string
	if runtime.GOOS != "windows" {
		// Finder'dan açılan uygulamalarda PATH'te Homebrew olmayabilir.
		cands = append(cands, "/opt/homebrew/bin/cloudflared", "/usr/local/bin/cloudflared")
	}
	if exe, err := os.Executable(); err == nil {
		cands = append(cands, filepath.Join(filepath.Dir(exe), exeName()))
	}
	cands = append(cands, filepath.Join(dataDir, exeName()))
	for _, p := range cands {
		if fileExists(p) {
			return p
		}
	}
	return ""
}

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir() && st.Size() > 0
}

// assets, bu platform için denenecek GitHub sürüm dosyalarıdır.
func assets() []string {
	switch runtime.GOOS {
	case "windows":
		if runtime.GOARCH == "386" {
			return []string{"cloudflared-windows-386.exe"}
		}
		return []string{"cloudflared-windows-amd64.exe"}
	case "darwin":
		if runtime.GOARCH == "arm64" {
			// arm64 sürümü yoksa amd64 (Rosetta) dene.
			return []string{"cloudflared-darwin-arm64.tgz", "cloudflared-darwin-amd64.tgz"}
		}
		return []string{"cloudflared-darwin-amd64.tgz"}
	default:
		return []string{"cloudflared-" + runtime.GOOS + "-" + runtime.GOARCH}
	}
}

// Download, cloudflared'ın son sürümünü GitHub'dan veri klasörüne indirir.
func Download(ctx context.Context, dataDir string, progress func(done, total int64)) (string, error) {
	var lastErr error
	for _, a := range assets() {
		p, err := download(ctx, dataDir, a, progress)
		if err == nil {
			return p, nil
		}
		lastErr = err
		log.Printf("%s indirilemedi: %v", a, err)
	}
	return "", lastErr
}

func download(ctx context.Context, dataDir, asset string, progress func(done, total int64)) (string, error) {
	url := "https://github.com/cloudflare/cloudflared/releases/latest/download/" + asset
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("indirme başarısız: %s", resp.Status)
	}
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return "", err
	}
	dest := filepath.Join(dataDir, exeName())
	tmp := dest + ".part"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return "", err
	}
	var src io.Reader = &progressReader{r: resp.Body, total: resp.ContentLength, fn: progress}
	if strings.HasSuffix(asset, ".tgz") {
		src, err = findInTgz(src, "cloudflared")
		if err != nil {
			f.Close()
			os.Remove(tmp)
			return "", err
		}
	}
	_, err = io.Copy(f, src)
	f.Close()
	if err != nil {
		os.Remove(tmp)
		return "", err
	}
	if err := os.Rename(tmp, dest); err != nil {
		return "", err
	}
	os.Chmod(dest, 0o755)
	return dest, nil
}

func findInTgz(r io.Reader, name string) (io.Reader, error) {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return nil, err
	}
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err != nil {
			return nil, fmt.Errorf("arşivde %s bulunamadı: %w", name, err)
		}
		if h.Typeflag == tar.TypeReg && path.Base(h.Name) == name {
			return tr, nil
		}
	}
}

type progressReader struct {
	r     io.Reader
	done  int64
	total int64
	last  time.Time
	fn    func(done, total int64)
}

func (p *progressReader) Read(b []byte) (int, error) {
	n, err := p.r.Read(b)
	p.done += int64(n)
	if p.fn != nil && (time.Since(p.last) > 250*time.Millisecond || err == io.EOF) {
		p.last = time.Now()
		p.fn(p.done, p.total)
	}
	return n, err
}

type Tunnel struct {
	URL  string
	cmd  *exec.Cmd
	done chan struct{}
	err  error
}

func (t *Tunnel) Done() <-chan struct{} { return t.done }
func (t *Tunnel) Err() error            { return t.err }

// ID, URL'deki alt alan adını döner (ör. "kelime-kelime-kelime").
func (t *Tunnel) ID() string {
	return strings.TrimSuffix(strings.TrimPrefix(t.URL, "https://"), ".trycloudflare.com")
}

// Start, target adresini (ör. http://127.0.0.1:1234) bir quick tunnel ile dışarı açar
// ve trycloudflare URL'si gelene kadar bekler.
func Start(ctx context.Context, bin, target, homeDir string) (*Tunnel, error) {
	cmd := exec.CommandContext(ctx, bin, "tunnel", "--no-autoupdate", "--url", target)
	sysutil.HideWindow(cmd)
	// ~/.cloudflared/config.yml varsa quick tunnel çalışmaz; ayrı bir HOME kullan.
	if homeDir != "" {
		os.MkdirAll(homeDir, 0o755)
		cmd.Env = append(os.Environ(), "HOME="+homeDir, "USERPROFILE="+homeDir)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	job, jerr := sysutil.KillWithParent(cmd.Process)
	if jerr != nil {
		log.Printf("job object: %v", jerr)
	}

	t := &Tunnel{cmd: cmd, done: make(chan struct{})}
	found := make(chan string, 1)
	go func() {
		sc := bufio.NewScanner(stderr)
		for sc.Scan() {
			line := sc.Text()
			log.Printf("[cloudflared] %s", line)
			if u := urlRe.FindString(line); u != "" && u != "https://api.trycloudflare.com" {
				select {
				case found <- u:
				default:
				}
			}
		}
		t.err = cmd.Wait()
		job.Close()
		close(t.done)
	}()

	select {
	case u := <-found:
		t.URL = u
		return t, nil
	case <-t.done:
		return nil, fmt.Errorf("cloudflared kapandı: %v", t.err)
	case <-time.After(60 * time.Second):
		cmd.Process.Kill()
		<-t.done
		return nil, errors.New("tünel adresi alınamadı (zaman aşımı)")
	case <-ctx.Done():
		<-t.done
		return nil, ctx.Err()
	}
}
