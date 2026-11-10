// Package update, GitHub'daki son sürümü denetler ve uygulamayı tek tıkla günceller.
//
// Windows: yeni exe indirilir, sha256 doğrulanır, çalışan exe ".old" olarak yeniden
// adlandırılıp yenisi aynı yola konur (çalışan dosya silinemez ama adı değiştirilebilir),
// sonra uygulama yeniden başlatılır. Eski dosya bir sonraki açılışta silinir.
//
// macOS: zip indirilir, doğrulanır, açılır; .app paketi yenisiyle değiştirilip
// uygulama yeniden açılır.
package update

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// Release, GitHub'daki son sürümdür.
type Release struct {
	Version string `json:"version"` // "0.3.1"
	URL     string `json:"url"`     // sürüm sayfası
	Notes   string `json:"notes"`
	asset   string // indirilecek dosyanın adresi
	sums    string // checksums.txt adresi
	name    string // indirilecek dosyanın adı
}

var httpc = &http.Client{Timeout: 5 * time.Minute}

// Newer, a sürümü b'den yeniyse true döner ("0.10.0" > "0.9.3").
func Newer(a, b string) bool {
	pa, pb := parts(a), parts(b)
	for i := 0; i < 3; i++ {
		if pa[i] != pb[i] {
			return pa[i] > pb[i]
		}
	}
	return false
}

func parts(v string) [3]int {
	var out [3]int
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	if i := strings.IndexAny(v, "-+"); i >= 0 {
		v = v[:i]
	}
	for i, p := range strings.SplitN(v, ".", 3) {
		out[i], _ = strconv.Atoi(p)
	}
	return out
}

func assetName(version string) string {
	if runtime.GOOS == "darwin" {
		return "RemoteDesk-" + version + "-macos.zip"
	}
	return "RemoteDesk-" + version + "-windows-" + runtime.GOARCH + ".exe"
}

// Latest, deponun son (taslak/ön sürüm olmayan) sürümünü döner.
func Latest(ctx context.Context, repo string) (*Release, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.github.com/repos/"+repo+"/releases/latest", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "RemoteDesk-updater")
	resp, err := httpc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GitHub yanıtı: %s", resp.Status)
	}
	var gh struct {
		Tag    string `json:"tag_name"`
		URL    string `json:"html_url"`
		Body   string `json:"body"`
		Assets []struct {
			Name string `json:"name"`
			URL  string `json:"browser_download_url"`
		} `json:"assets"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&gh); err != nil {
		return nil, err
	}
	r := &Release{Version: strings.TrimPrefix(gh.Tag, "v"), URL: gh.URL, Notes: gh.Body}
	r.name = assetName(r.Version)
	for _, a := range gh.Assets {
		switch a.Name {
		case r.name:
			r.asset = a.URL
		case "checksums.txt":
			r.sums = a.URL
		}
	}
	return r, nil
}

// Installable, bu platform için indirilebilir dosya ve sağlama özeti varsa true döner.
func (r *Release) Installable() bool { return r.asset != "" && r.sums != "" }

func download(ctx context.Context, url, dst string, progress func(done, total int64)) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "RemoteDesk-updater")
	resp, err := httpc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("indirme: %s", resp.Status)
	}
	f, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return err
	}
	var done int64
	buf := make([]byte, 256<<10)
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			if _, err := f.Write(buf[:n]); err != nil {
				f.Close()
				return err
			}
			done += int64(n)
			if progress != nil {
				progress(done, resp.ContentLength)
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			f.Close()
			return rerr
		}
	}
	return f.Close()
}

func fetchText(ctx context.Context, url string) (string, error) {
	tmp, err := os.CreateTemp("", "rd-sums-*")
	if err != nil {
		return "", err
	}
	tmp.Close()
	defer os.Remove(tmp.Name())
	if err := download(ctx, url, tmp.Name(), nil); err != nil {
		return "", err
	}
	b, err := os.ReadFile(tmp.Name())
	return string(b), err
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
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

// fetchVerified, sürüm dosyasını dst'ye indirir ve checksums.txt ile doğrular.
func (r *Release) fetchVerified(ctx context.Context, dst string, progress func(done, total int64)) error {
	sums, err := fetchText(ctx, r.sums)
	if err != nil {
		return fmt.Errorf("sağlama özeti alınamadı: %w", err)
	}
	var want string
	for _, line := range strings.Split(sums, "\n") {
		f := strings.Fields(line)
		if len(f) == 2 && strings.TrimPrefix(f[1], "*") == r.name {
			want = strings.ToLower(f[0])
		}
	}
	if len(want) != 64 {
		return errors.New("sağlama özetinde bu dosya yok")
	}
	if err := download(ctx, r.asset, dst, progress); err != nil {
		return err
	}
	got, err := fileSHA256(dst)
	if err != nil {
		return err
	}
	if got != want {
		os.Remove(dst)
		return errors.New("indirilen dosya doğrulanamadı (sha256 uyuşmuyor)")
	}
	return nil
}

// Install, sürümü indirip doğrular ve çalışan uygulamanın yerine koyar.
// Başarılı olursa çağıran uygulamayı yeniden başlatıp kapanmalıdır.
func (r *Release) Install(ctx context.Context, progress func(done, total int64)) error {
	if !r.Installable() {
		return errors.New("bu sistem için güncelleme dosyası yok")
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	exe, _ = filepath.EvalSymlinks(exe)
	if runtime.GOOS == "darwin" {
		return r.installMac(ctx, exe, progress)
	}
	return r.installWindows(ctx, exe, progress)
}

func (r *Release) installWindows(ctx context.Context, exe string, progress func(done, total int64)) error {
	dir := filepath.Dir(exe)
	tmp := exe + ".new"
	if err := r.fetchVerified(ctx, tmp, progress); err != nil {
		os.Remove(tmp)
		if os.IsPermission(err) || errors.Is(err, os.ErrPermission) {
			return fmt.Errorf("%s klasörüne yazılamıyor; uygulamayı yazılabilir bir klasöre taşı ya da yönetici olarak çalıştır", dir)
		}
		return err
	}
	old := exe + ".old"
	os.Remove(old)
	if err := os.Rename(exe, old); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("eski sürüm kenara alınamadı: %w", err)
	}
	if err := os.Rename(tmp, exe); err != nil {
		os.Rename(old, exe) // geri al
		return fmt.Errorf("yeni sürüm yerleştirilemedi: %w", err)
	}
	return nil
}

func (r *Release) installMac(ctx context.Context, exe string, progress func(done, total int64)) error {
	// exe: .../RemoteDesk.app/Contents/MacOS/RemoteDesk
	app := filepath.Dir(filepath.Dir(filepath.Dir(exe)))
	if !strings.HasSuffix(app, ".app") {
		return errors.New("uygulama paketi bulunamadı (.app içinden çalışmıyor)")
	}
	work, err := os.MkdirTemp("", "rd-update-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(work)
	zip := filepath.Join(work, r.name)
	if err := r.fetchVerified(ctx, zip, progress); err != nil {
		return err
	}
	out := filepath.Join(work, "x")
	if b, err := exec.CommandContext(ctx, "/usr/bin/ditto", "-x", "-k", zip, out).CombinedOutput(); err != nil {
		return fmt.Errorf("zip açılamadı: %v %s", err, b)
	}
	newApp := filepath.Join(out, "RemoteDesk.app")
	if _, err := os.Stat(filepath.Join(newApp, "Contents", "MacOS", "RemoteDesk")); err != nil {
		return errors.New("güncelleme paketinde RemoteDesk.app yok")
	}
	// İndirilen dosyada karantina işareti olmaz; yine de temizle (Gatekeeper sorusu çıkmasın).
	exec.Command("/usr/bin/xattr", "-dr", "com.apple.quarantine", newApp).Run()

	backup := app + ".old"
	os.RemoveAll(backup)
	if err := os.Rename(app, backup); err != nil {
		return fmt.Errorf("%s değiştirilemiyor (izin yok): %w", app, err)
	}
	if b, err := exec.CommandContext(ctx, "/usr/bin/ditto", newApp, app).CombinedOutput(); err != nil {
		os.RemoveAll(app)
		os.Rename(backup, app) // geri al
		return fmt.Errorf("yeni sürüm kopyalanamadı: %v %s", err, b)
	}
	os.RemoveAll(backup)
	return nil
}

// Cleanup, önceki güncellemeden kalan dosyayı siler (Windows'ta çalışan eski exe).
func Cleanup() {
	if exe, err := os.Executable(); err == nil {
		os.Remove(exe + ".old")
		os.Remove(exe + ".new")
	}
}
