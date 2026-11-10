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
// Kendi sunucunu kurmak için: cd server && npx wrangler deploy, çıkan adresi buraya yaz.
// Çalışırken değiştirmek için: RemoteDesk -server https://... (boş vermek: -server "").
const HubServer = ""
