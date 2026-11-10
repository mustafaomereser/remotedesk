// RemoteDesk: cloudflared quick tunnel üzerinden çalışan, AnyDesk benzeri uzak masaüstü.
package main

import (
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"remotedesk/internal/app"
	"remotedesk/internal/desktop"
	"remotedesk/internal/sysutil"
)

// appVersion, sürüm paketlerinde (scripts/build-release.sh) kullanılır.
// hubServer, kalıcı ID ve çevrimiçi bilgisi için buluşma sunucusudur (server/ klasörü).
const hubServer = ""

const appVersion = "0.2.1"

func dataDir() string {
	if runtime.GOOS == "windows" {
		if d := os.Getenv("LocalAppData"); d != "" {
			return filepath.Join(d, "RemoteDesk")
		}
	} else if d, err := os.UserConfigDir(); err == nil {
		return filepath.Join(d, "RemoteDesk") // macOS: ~/Library/Application Support/RemoteDesk
	}
	return filepath.Join(os.TempDir(), "RemoteDesk")
}

func main() {
	// macOS'ta ana iş parçacığı AppKit olay döngüsüne ayrılır (ad etiketi penceresi için);
	// uygulama ayrı bir goroutine'de çalışır. Windows'ta run doğrudan çağrılır.
	desktop.RunMain(run)
}

func run() {
	headless := flag.Bool("headless", false, "Pencere açmadan çalış (ID ve şifre konsola yazılır)")
	autoAccept := flag.Bool("autoaccept", false, "Gelen bağlantıları onay sormadan kabul et")
	password := flag.String("password", "", "Sabit şifre (boşsa rastgele üretilir)")
	noTunnel := flag.Bool("notunnel", false, "Tünel açma, sadece 127.0.0.1 üzerinde dinle (test için)")
	server := flag.String("server", hubServer, "Buluşma sunucusu adresi (boşsa kalıcı ID kapalı)")
	concurrent := flag.Bool("concurrent", false, "Aynı anda kullanımı (simülasyon modu) açık başlat")
	flag.Parse()

	dir := dataDir()
	os.MkdirAll(dir, 0o755)

	if lf, err := os.OpenFile(filepath.Join(dir, "remotedesk.log"), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644); err == nil {
		defer lf.Close()
		log.SetOutput(io.MultiWriter(lf, os.Stderr))
	}
	log.SetFlags(log.Ltime | log.Lmicroseconds)
	log.Printf("RemoteDesk %s (%s/%s)", appVersion, runtime.GOOS, runtime.GOARCH)

	// Çift tıkla açılınca konsol yok: başlangıçta bir şey ters giderse kullanıcı görsün.
	fail := func(msg string) {
		log.Printf("HATA: %s", msg)
		if !*headless {
			sysutil.Alert("RemoteDesk başlatılamadı", msg+"\n\nAyrıntılar: "+filepath.Join(dir, "remotedesk.log"))
		}
		os.Exit(1)
	}
	defer func() {
		if r := recover(); r != nil {
			fail(fmt.Sprint(r))
		}
	}()

	sysutil.SetDPIAware()
	sysutil.ConsoleUTF8()

	a := app.New(app.Options{
		DataDir:    dir,
		Headless:   *headless,
		AutoAccept: *autoAccept,
		NoTunnel:   *noTunnel,
		Password:   *password,
		Concurrent: *concurrent,
		Server:     strings.TrimRight(*server, "/"),
	})
	if err := a.Run(); err != nil {
		fail(err.Error())
	}
}
