# RemoteDesk

AnyDesk benzeri, Go ile yazılmış uzak masaüstü. Bağlantı, Cloudflare **quick tunnel** (trycloudflare.com) üzerinden kurulur: hesap, domain ya da port açma gerekmez.

## Kullanım

`RemoteDesk.exe` dosyasını çalıştır. Uygulama:

1. Ekranı yayınlayan host sunucusunu başlatır (sadece 127.0.0.1).
2. `cloudflared` ile bir quick tunnel açar (cloudflared yoksa otomatik indirir).
3. Tünel adresinin alt alan adını **ID** olarak gösterir, rastgele bir **şifre** üretir.

Bağlanacak kişi kendi RemoteDesk'inde bu ID'yi ve şifreyi girip **Bağlan**'a basar. Host tarafında onay penceresi çıkar.

> Quick tunnel ID'si uygulama her açıldığında (ve tünel koparsa) değişir.

### Komut satırı seçenekleri (`RemoteDesk-console.exe`)

| Seçenek | Açıklama |
|---|---|
| `-headless` | Pencere açmaz, ID ve şifreyi konsola yazar |
| `-autoaccept` | Onay sormadan kabul eder (headless modda gerekli) |
| `-password X` | Sabit şifre |
| `-notunnel` | Tünel açmaz, sadece `127.0.0.1:port` (yerel test) |

## Mimari

```
[Viewer penceresi] --wss--> xxx.trycloudflare.com --> cloudflared --> 127.0.0.1:host --> yakalama + SendInput
       ^
  Yerel UI (127.0.0.1:47800, Edge --app penceresi) — tünele ASLA açılmaz
```

- `internal/capture`: GDI BitBlt ile ekran yakalama, imleç görüntüye çizilir.
- `internal/host`: WebSocket oturumu, şifre doğrulama (5 hatalı denemede 1 dk kilit), 64×64 karo farkı + paralel JPEG kodlama, ack tabanlı akış kontrolü (en fazla 30 fps).
- `internal/input`: `SendInput` ile fare ve klavye. Klavye **scancode** gönderir, yani karakter karşı tarafın klavye düzenine göre oluşur.
- `internal/tunnel`: cloudflared'ı bulma/indirme/başlatma. `~/.cloudflared/config.yml` çakışmasın diye ayrı bir HOME kullanır. Job object sayesinde uygulama kapanınca cloudflared da kapanır.
- `internal/app`: durum yönetimi, SSE, onay akışı, Edge/Chrome uygulama penceresi.
- `web/`: gömülü arayüz (ana pencere + viewer).

### Protokol (WebSocket `/ws`)

Viewer'dan host'a (JSON): `auth{pw,h264,v}`, `ack`, `vack`, `kf`, `codec{codec}`, `disp{i}`, `clip{text}`, `mm{x,y}`, `mb{b,d,x,y}`, `wh{dx,dy}`, `k{c,d}`, `q{v}`, `ping{ts}`
Host'tan viewer'a (JSON): `wait`, `mode{v,enc}`, `displays{list,cur}`, `init{w,h}`, `clip{text}`, `notice{msg}`, `err{msg}`, `pong{ts}`
Host'tan viewer'a (binary): `[1][x][y][w][h][jpeg]` dikdörtgen, `[2]` kare sonu, `[3][anahtar?][H.264 Annex B]` video karesi.

## macOS

```bash
chmod +x RemoteDesk-macos-arm64          # Intel Mac için: RemoteDesk-macos-amd64
xattr -dr com.apple.quarantine RemoteDesk-macos-arm64
./RemoteDesk-macos-arm64
```

- İlk açılışta macOS **Ekran Kaydı** ve **Erişilebilirlik** izinlerini ister. Terminal'den çalıştırıyorsan izinler Terminal'e verilir. İzinleri verdikten sonra uygulamayı yeniden başlat. Eksik izinler arayüzde sarı uyarı olarak görünür.
- Pencere Chrome/Edge/Brave varsa uygulama modunda açılır, yoksa Safari'de açılır.
- Ekran yakalama `CGDisplayCreateImage` ile yapılır (cgo yok, `purego` kullanılıyor). macOS'ta imleç görüntüye çizilmez.
- Klavye fiziksel tuş eşlemesiyle çalışır: Windows'tan bağlanınca Win tuşu ⌘ (Command) olarak gider.

## Özellikler

- **H.264 (ffmpeg):** Uygulama açılışta ffmpeg'i arar (PATH, Homebrew, uygulama klasörü). Bulamazsa indirir: Windows'ta gyan.dev (SHA-256 doğrulamalı), macOS'ta martin-riedl.de / evermeet.cx. Ardından sırayla NVENC → Quick Sync → AMF → x264 → Media Foundation (macOS'ta VideoToolbox → x264) denenir ve çalışan ilk kodlayıcı seçilir.
  - Ekranı ffmpeg yakalamaz. Kareler uygulamanın kendi yakalama kodundan ffmpeg'e ham BGRA olarak verilir. Bu sayede monitör seçimi ve imleç çizimi aynı kalır, ekran değişmezken kare de gönderilmez.
  - Ekran durduktan sonra 12 kare daha gönderilir, böylece görüntü kalitesi netleşir.
  - Akış kontrolü: viewer'ın çözmediği kare sayısı 3'ü geçemez (`vack`).
  - Viewer, H.264'ü tarayıcının WebCodecs `VideoDecoder`'ı ile çözer. Desteklenmiyorsa ya da ffmpeg hazır değilse JPEG kullanılır. Araç çubuğundan elle de geçilebilir.

- **Çoklu monitör:** Viewer araç çubuğunda monitör seçici var (birden fazla ekran olduğunda görünür).
- **Pano:** Metin iki yönde de senkronize olur ("Pano" kutusu). Viewer kendi bilgisayarının panosuna yerel uygulama üzerinden erişir, tarayıcı izni gerekmez.

## Bilinen sınırlar / yapılacaklar

- UAC ve kilit ekranı (secure desktop) görünmez. Bunun için servis olarak (SYSTEM) çalışmak gerekiyor.
- Ctrl+Alt+Del gönderilemiyor.
- Pano sadece düz metin; dosya transferi ve ses yok.
- Ekran yakalama hâlâ GDI / CGDisplayCreateImage. DXGI Desktop Duplication / ScreenCaptureKit ile CPU kullanımı düşürülebilir.
- Trafik cloudflared ile TLS'li, ama uçtan uca ek bir şifreleme katmanı yok (Cloudflare trafiği görebilir).

## Derleme

```powershell
./build.ps1                      # geliştirme derlemeleri -> dist/
bash scripts/build-release.sh    # sürüm paketleri -> dist/release/
```

Sürüm betiği, macOS `.app` paketlerini `rcodesign` ile Developer ID'yle imzalar, Apple noterliğine gönderir ve onayı staple eder. Anahtarlar repo dışında, `~/.agentic-signing/apple` klasöründe durur. Hızlı yerel deneme için `MAC_SIGN=1` kullan: ad-hoc imza atılır, noterlik yapılmaz. Sürüm numarası `main.go` içindeki `appVersion`'dan alınır. İkon `go run ./cmd/mkicon` ile üretilir.
