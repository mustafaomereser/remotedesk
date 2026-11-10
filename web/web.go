// Package web, uygulamanın gömülü arayüz dosyalarını içerir.
package web

import "embed"

//go:embed *.html *.css *.js
var FS embed.FS
