package main

// HubServer, buluşma sunucusunun (server/ klasöründeki Cloudflare Worker) adresidir.
//
// Sunuculu mod (adres dolu):
//   - Her cihaz kalıcı 9 haneli bir ID alır; uygulama kapanıp açılınca değişmez.
//   - Cihaz listesi, favoriler ve çevrimiçi/çevrimdışı durumu çalışır.
//   - Tünel yalnızca biri bağlanmak istediğinde açılır, boşta kalınca kapanır.
//   - Sunucuya ulaşılamazsa uygulama "Tüneli aç" düğmesiyle sunucusuz moda döner.
//
// Sunucusuz mod (adres boş ""):
//   - Kalıcı ID yoktur; "Tüneli aç" ile her seferinde yeni bir geçici adres
//     (kelime-kelime-kelime) alınır ve karşı tarafa elle verilir.
//   - Cihaz listesi ve çevrimiçi durumu çalışmaz.
//
// Adres repoda tutulmaz: derleme sırasında exe'ye eklenir. Kaynak sırasıyla:
//   1. RD_HUB_SERVER ortam değişkeni
//   2. proje kökündeki .hubserver dosyası (git'e girmez, .gitignore'da)
// scripts/build-release.sh ve build.ps1 bunu -ldflags "-X main.HubServer=..." ile yapar.
// Elle derlerken: go build -ldflags "-X main.HubServer=$(cat .hubserver)" .
// Kendi sunucunu kurmak için: cd server && npx wrangler deploy, çıkan adresi .hubserver'a yaz.
// Çalışırken değiştirmek için: RemoteDesk -server https://... (boş vermek: -server "").
var HubServer = ""

// UpdateRepo, güncellemelerin denetlendiği GitHub deposudur ("sahip/ad").
// Uygulama açılınca ve 6 saatte bir son sürüme bakar; yeni sürüm varsa arayüzde
// "Güncelle" düğmesi çıkar. Boş bırakılırsa güncelleme denetimi kapalıdır.
const UpdateRepo = "mustafaomereser/remotedesk"
