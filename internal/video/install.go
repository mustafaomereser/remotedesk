// Package video, ffmpeg ile H.264 kodlama yapar: ffmpeg'i bulur/indirir,
// uygun (tercihen donanımsal) kodlayıcıyı seçer ve ham kareleri H.264'e çevirir.
package video

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"remotedesk/internal/sysutil"
)

func binName() string {
	if runtime.GOOS == "windows" {
		return "ffmpeg.exe"
	}
	return "ffmpeg"
}

// Find, ffmpeg'i PATH'te, bilinen klasörlerde, uygulamanın yanında veya veri klasöründe arar.
func Find(dataDir string) string {
	if p, err := exec.LookPath("ffmpeg"); err == nil {
		return p
	}
	var cands []string
	if runtime.GOOS != "windows" {
		cands = append(cands, "/opt/homebrew/bin/ffmpeg", "/usr/local/bin/ffmpeg")
	}
	if exe, err := os.Executable(); err == nil {
		cands = append(cands, filepath.Join(filepath.Dir(exe), binName()))
	}
	cands = append(cands, filepath.Join(dataDir, binName()))
	for _, p := range cands {
		if st, err := os.Stat(p); err == nil && !st.IsDir() && st.Size() > 0 {
			return p
		}
	}
	return ""
}

type source struct {
	url    string
	sha256 string // içinde SHA-256 olan metin dosyasının adresi (opsiyonel)
}

func sources() []source {
	switch runtime.GOOS {
	case "windows":
		return []source{
			{"https://www.gyan.dev/ffmpeg/builds/ffmpeg-release-essentials.zip", "https://www.gyan.dev/ffmpeg/builds/ffmpeg-release-essentials.zip.sha256"},
			{"https://github.com/BtbN/FFmpeg-Builds/releases/download/latest/ffmpeg-master-latest-win64-gpl.zip", ""},
		}
	case "darwin":
		var s []source
		if runtime.GOARCH == "arm64" {
			s = append(s, source{"https://ffmpeg.martin-riedl.de/redirect/latest/macos/arm64/release/ffmpeg.zip", ""})
		}
		// Intel derlemesi; Apple Silicon'da Rosetta ile çalışır.
		return append(s, source{"https://evermeet.cx/ffmpeg/getrelease/zip", ""})
	}
	return nil
}

// Download, ffmpeg'i indirip veri klasörüne çıkarır.
func Download(ctx context.Context, dataDir string, progress func(done, total int64)) (string, error) {
	srcs := sources()
	if len(srcs) == 0 {
		return "", errors.New("bu platform için ffmpeg indirme adresi yok")
	}
	var lastErr error
	for _, s := range srcs {
		p, err := downloadZip(ctx, dataDir, s, progress)
		if err == nil {
			return p, nil
		}
		lastErr = err
		log.Printf("ffmpeg indirilemedi (%s): %v", s.url, err)
	}
	return "", lastErr
}

func downloadZip(ctx context.Context, dataDir string, s source, progress func(done, total int64)) (string, error) {
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return "", err
	}
	zipPath := filepath.Join(dataDir, "ffmpeg-download.zip")
	defer os.Remove(zipPath)
	if err := fetchFile(ctx, s.url, zipPath, progress); err != nil {
		return "", err
	}
	if s.sha256 != "" {
		if want, err := fetchText(ctx, s.sha256); err == nil && len(strings.Fields(want)) > 0 {
			got, err := fileSHA256(zipPath)
			if err != nil {
				return "", err
			}
			if !strings.EqualFold(strings.Fields(want)[0], got) {
				return "", errors.New("indirilen dosyanın SHA-256 değeri tutmuyor")
			}
		}
	}

	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		return "", err
	}
	defer zr.Close()
	for _, f := range zr.File {
		if f.FileInfo().IsDir() || path.Base(f.Name) != binName() {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return "", err
		}
		dest := filepath.Join(dataDir, binName())
		tmp := dest + ".part"
		out, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
		if err != nil {
			rc.Close()
			return "", err
		}
		_, err = io.Copy(out, rc)
		rc.Close()
		out.Close()
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
	return "", fmt.Errorf("arşivde %s bulunamadı", binName())
}

func fetchFile(ctx context.Context, url, dest string, progress func(done, total int64)) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("indirme başarısız: %s", resp.Status)
	}
	f, err := os.Create(dest)
	if err != nil {
		return err
	}
	_, err = io.Copy(f, &progressReader{r: resp.Body, total: resp.ContentLength, fn: progress})
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

func fetchText(ctx context.Context, url string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
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
		return "", errors.New(resp.Status)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 4096))
	return string(b), err
}

func fileSHA256(p string) (string, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
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

// Detect, sırayla aday kodlayıcıları kısa bir test videosuyla dener ve çalışan ilkini döner.
// Donanım kodlayıcıları (NVENC, Quick Sync, AMF, VideoToolbox) önce denenir.
func Detect(ctx context.Context, bin string) (string, error) {
	for _, enc := range candidates() {
		args := []string{"-hide_banner", "-loglevel", "error",
			"-f", "lavfi", "-i", "testsrc2=size=1280x720:rate=30", "-frames:v", "10",
			"-vf", "format=nv12", "-c:v", enc}
		args = append(args, encoderArgs(enc, 4_000_000)...)
		args = append(args, "-f", "null", "-")
		cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
		cmd := exec.CommandContext(cctx, bin, args...)
		sysutil.HideWindow(cmd)
		out, err := cmd.CombinedOutput()
		cancel()
		if err == nil {
			return enc, nil
		}
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		log.Printf("kodlayıcı %s kullanılamıyor: %v %s", enc, err, firstLine(string(out)))
	}
	return "", errors.New("uygun H.264 kodlayıcı bulunamadı")
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return s
}
