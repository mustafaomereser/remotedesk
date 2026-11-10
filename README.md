# RemoteDesk

AnyDesk benzeri, Go ile yazılmış uzak masaüstü (Windows ve macOS). Bilgisayarlar mümkünse **doğrudan** (WebRTC) bağlanır; ağ engeli varsa Cloudflare **quick tunnel** üzerinden. Hesap, domain ya da port açma gerekmez.

## Kullanım

`RemoteDesk.exe`yi (macOS'ta `RemoteDesk.app`) aç. Ana pencerede bu bilgisayarın **ID**'si ve tek kullanımlık **şifresi** görünür.

- **Bağlanmak:** Karşı tarafın ID'sini gir. Şifreyi biliyorsan doğrudan bağlanırsın. Şifre alanını boş bırakırsan karşı tarafa bir **bağlantı isteği** gider; onaylarsa bağlanırsın.
- **Gözetimsiz erişim:** Ayarlar > Sabit şifre. Bu şifreyle gelen kişi onay beklemeden bağlanır. Şifrenin kendisi saklanmaz, yalnızca PBKDF2 özeti tutulur.
- **Tarayıcıdan erişim** (isteğe bağlı, Ayarlar > Erişim): Bağlantı adresi telefon ya da başka bir bilgisayarın tarayıcısında açılabilir. Şifre sorulur; telefonda dokunmatik kontrol ve telefon klavyesi çalışır.

## Özellikler

### Bağlantı
- **Kalıcı ID** (buluşma sunucusu ayarlıysa): 9 haneli numara ya da seçtiğin bir ad (ör. `mustafa-pc`). Kayıtlı cihazlar, favoriler ve kimin çevrimiçi olduğu listede görünür.
- **Doğrudan bağlantı:**
  - Görüntü, klavye/fare ve ses WebRTC veri kanallarıyla iki bilgisayar arasında doğrudan akar.
  - Buluşma sunucusundan yalnızca tek bir el sıkışma mesajı geçer.
  - Kurulamazsa tünel (WebSocket) kullanılır.
  - İzleme penceresinin durum satırında hangisinin kullanıldığı yazar ("Doğrudan" / "Tünel").
- **Birden fazla bağlantı:** Her cihaz ayrı bir pencerede açılır, aralarında geçiş yapılabilir. Bir gruba ya da takıma **toplu bağlanılabilir**.

### Takımlar ve gruplar
- **Takım:** Cihazlar > Takım oluştur. Davet kodunu bilen bilgisayar takıma katılır. Takımdakiler birbirinin cihazlarını ve hangisinin açık olduğunu görür. Bağlanmak için yine şifre ya da onay gerekir. Takımın sahibi üye çıkarabilir, kodu yenileyebilir, takımı silebilir.
- **Grup:** Kendi cihaz listende her cihaza bir grup adı verebilirsin (ör. "Ofis", "Ev"). Liste gruplara ayrılır.

### Toplantı ve sunum
- **Çok kişili bağlantı:**
  - Aynı bilgisayara birden fazla kişi aynı anda bağlanabilir; kişi sayısı Ayarlar'dan seçilir.
  - Ana pencerede bağlı kişiler listelenir.
  - Fare/klavye izni kişi başına kapatılabilir ve her kişi ayrı ayrı çıkarılabilir.
- **Ekrana çizim:**
  - Bağlanan herkes kalemle çizebilir ya da lazer işaretçi kullanabilir; 5 renk, geri al ve hepsini sil var.
  - Çizgiler herkeste ve bağlanılan bilgisayarın ekranında görünür.
  - Windows'ta bağlanılan bilgisayardaki çizim katmanı ekran yakalamaya girmez, yani görüntüde iki kez çıkmaz.
- **Sesli görüşme:**
  - WebRTC ile, mikrofon izni ister.
  - Toplantıda bağlanılan bilgisayar herkesin sesini birleştirir, herkes herkesi duyar.

### Görüntü
- **Ekran yakalama:**
  - Windows'ta **DXGI Desktop Duplication**; aynı monitörü izleyenler tek kopyalamayı paylaşır. Kullanılamazsa GDI'a düşülür.
  - macOS'ta `CGDisplayStream`.
- **H.264 (ffmpeg):**
  - Donanım kodlayıcı sırayla denenir: NVENC → Quick Sync → AMF → x264 → Media Foundation; macOS'ta VideoToolbox → x264.
  - Uyarlamalı bit hızı: 1,5–20 Mbps arası, bağlantıya göre ayarlanır.
  - İzleyici WebCodecs ile çözer; desteklenmiyorsa JPEG karolar kullanılır.
- **Çoklu monitör, pano senkronu, tam ekran** (tam ekranda Win/Alt+Tab da karşıya gider).

### Telefon ve tablet
- **Dokunmatik kontrol:**
  - Dokun = tık, iki parmakla dokun = sağ tık, tek parmakla sürükle = sürükle-bırak.
  - İki parmakla kaydır = tekerlek, sıkıştır = yakınlaştır.
- **Telefon klavyesi:**
  - Sağ alttaki düğmeyle açılır; Türkçe karakterler dahil yazar.
  - Windows'ta bir yazı alanına dokununca Android'de kendiliğinden açılır.
  - iPhone klavyeyi yalnızca dokunuş anında açtırdığı için orada düğme kullanılır.

### Diğer
- **Aynı anda kullanım** (deneysel): Bağlanan kişi kendi hayalet imleciyle çalışır, ana kullanıcının imleci oynamaz. Kapalıyken ana kullanıcı fareye dokununca kontrol ona geçer ve uzaktaki kişinin imleci adıyla gösterilir.
- **UAC:** Windows'ta "Yönetici olarak yeniden başlat" ile UAC pencereleri de kontrol edilebilir. Güvenli masaüstü açıkken izleyiciye uyarı gösterilir.
- **Güncelleme:** GitHub sürümleri denetlenir, tek tıkla kurulur (SHA-256 doğrulamalı).

## Sunuculu / sunucusuz mod

`config.go` içindeki `HubServer` doluysa uygulama **buluşma sunucusunu** kullanır: kalıcı ID, cihaz listesi, takımlar, doğrudan bağlantı el sıkışması. Boşsa **sunucusuz** çalışır: ID tünel adresidir ve her açılışta değişir.

Sunucu adresi repoda tutulmaz; derlemede `-ldflags "-X main.HubServer=..."` ile eklenir. Kaynak sırası: önce `RD_HUB_SERVER` ortam değişkeni, sonra proje kökündeki `.hubserver` dosyası (git'e girmez).

Kendi sunucunu kurmak için (Cloudflare Workers, ücretsiz katman yeterli):

```bash
cd server && npx wrangler deploy     # çıkan adresi .hubserver dosyasına yaz
```

Sunucudan görüntü, klavye/fare ya da şifre geçmez. Sunucu yalnızca kimin çevrimiçi olduğunu bilir, el sıkışmayı iletir ve takım listelerini tutar. Çevrimiçi bilgisi sorgulanmaz; değişince sunucu bildirir. Böylece istek kotası korunur.

### Komut satırı seçenekleri (`RemoteDesk-console.exe`)

| Seçenek | Açıklama |
|---|---|
| `-headless` | Pencere açmaz, ID ve şifreyi konsola yazar |
| `-autoaccept` | Şifresiz istekleri onay sormadan kabul eder (headless modda gerekli) |
| `-password X` | Sabit şifre |
| `-notunnel` | Tünel açmaz, sadece `127.0.0.1:port` (yerel test) |
| `-server URL` | Buluşma sunucusu (boş: sunucusuz mod) |
| `-concurrent` | Aynı anda kullanımı açık başlat |

## Mimari

```
                 buluşma sunucusu (Workers + Durable Object): çevrimiçi durumu, el sıkışma, takımlar
                        ^                                   ^
[İzleme penceresi] ==== WebRTC (doğrudan) =============== [Host: yakalama + kodlama + giriş]
        \--- wss --> xxx.trycloudflare.com --> cloudflared --/   (yedek yol)
Yerel arayüz (127.0.0.1, çerez anahtarlı) tünele ASLA açılmaz.
```

| Paket | İçerik |
|---|---|
| `internal/host` | Oturumlar (bağlantı türünden bağımsız `Conn`), şifre ve onay, çok kişili oturum, çizim, akış kontrolü, WebRTC |
| `internal/capture` | DXGI / GDI / CGDisplayStream yakalama |
| `internal/input` | `SendInput` / CGEvent; klavye scancode ile, telefon metni Unicode olarak |
| `internal/desktop` | Ad etiketi ve çizim katmanı (saydam, tıklamayı geçiren pencereler), WebView |
| `internal/hub` | Buluşma sunucusu istemcisi (tek kalıcı bağlantı, takım işlemleri) |
| `internal/app` | Durum, ayarlar, yerel API, cihaz listesi, güncelleme |
| `server/` | Buluşma sunucusu (Cloudflare Worker) |
| `web/` | Gömülü arayüz: ana pencere ve izleme penceresi |

## Bilinen sınırlar

- Kilit ekranı ve Ctrl+Alt+Del: servis (SYSTEM) olarak çalışmak gerekir; şimdilik yok.
- Dosya transferi yok; pano yalnızca düz metin.
- macOS'ta bağlanılan bilgisayarın ekranında çizim katmanı ve yazı alanı algılama henüz yok. Çizimler izleyicilerde görünür.
- Doğrudan bağlantı STUN ile kurulur; simetrik NAT gibi zor ağlarda tünel kullanılır (TURN yok).

## Derleme

```powershell
./build.ps1                      # geliştirme derlemeleri -> dist/
bash scripts/build-release.sh    # sürüm paketleri -> dist/release/
```

Sürüm betiği, macOS `.app` paketlerini `rcodesign` ile Developer ID'yle imzalar, Apple noterliğine gönderir ve onayı staple eder. Anahtarlar repo dışında, `~/.agentic-signing/apple` klasöründe durur. Hızlı yerel deneme için `MAC_SIGN=1` kullan: ad-hoc imza atılır, noterlik yapılmaz. Sürüm numarası `main.go` içindeki `appVersion`'dan alınır. İkon `go run ./cmd/mkicon` ile üretilir.
