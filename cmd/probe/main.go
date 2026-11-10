// probe: bir RemoteDesk host'una bağlanıp akışı test eden komut satırı aracı.
//
//	go run ./cmd/probe -url wss://xxx.trycloudflare.com/ws -pw sifre -out ilk.png
//	go run ./cmd/probe -url ws://127.0.0.1:1234/ws -pw sifre -h264 -h264out akis.h264 -dur 5s
package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"flag"
	"fmt"
	"image"
	"image/draw"
	"image/jpeg"
	"image/png"
	"log"
	"math"
	"os"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

func main() {
	url := flag.String("url", "", "ws(s)://.../ws adresi")
	pw := flag.String("pw", "", "şifre")
	n := flag.Int("frames", 20, "alınacak kare sayısı")
	out := flag.String("out", "", "ilk JPEG kareyi bu PNG dosyasına kaydet")
	disp := flag.Int("disp", -1, "bağlanınca bu monitöre geç")
	clip := flag.String("clip", "", "bağlanınca host'un panosuna bu metni yaz")
	dur := flag.Duration("dur", 0, "kare sayısı yerine bu süre boyunca dinle (ör. 6s)")
	h264 := flag.Bool("h264", false, "H.264 iste")
	h264out := flag.String("h264out", "", "H.264 akışını bu dosyaya yaz")
	name := flag.String("name", "", "bağlanırken gönderilecek ad (imleç etiketi)")
	move := flag.Bool("move", false, "bağlanınca fareyi ekranın ortasında daire çizerek gezdir")
	flag.Parse()

	var vf *os.File
	if *h264out != "" {
		var err error
		if vf, err = os.Create(*h264out); err != nil {
			log.Fatal(err)
		}
	}
	frames, rects, total, keys := 0, 0, 0, 0
	start := time.Now()
	var firstFrame time.Duration
	report := func() {
		el := time.Since(start)
		fmt.Printf("ilk kare: %v | %d kare (%d anahtar), %d dikdörtgen, %.1f KB, %.1f fps\n",
			firstFrame, frames, keys, rects, float64(total)/1024, float64(frames)/el.Seconds())
		if vf != nil {
			vf.Close()
		}
	}
	if *dur > 0 {
		*n = 1 << 30
		go func() { time.Sleep(*dur); report(); os.Exit(0) }()
	}

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, *url, nil)
	if err != nil {
		log.Fatal(err)
	}
	defer c.CloseNow()
	c.SetReadLimit(64 << 20)
	wsjson.Write(ctx, c, map[string]any{"t": "auth", "pw": *pw, "h264": *h264, "name": *name})

	var canvas *image.RGBA
	gotFrame := func() {
		frames++
		if frames == 1 {
			firstFrame = time.Since(start)
		}
	}
	for frames < *n {
		typ, data, err := c.Read(ctx)
		if err != nil {
			log.Fatal(err)
		}
		if typ == websocket.MessageText {
			fmt.Println("kontrol:", string(data))
			var m struct {
				T    string
				W, H int
			}
			json.Unmarshal(data, &m)
			if m.T == "init" {
				canvas = image.NewRGBA(image.Rect(0, 0, m.W, m.H))
				start = time.Now()
				if *clip != "" {
					wsjson.Write(ctx, c, map[string]string{"t": "clip", "text": *clip})
					*clip = ""
				}
				if *move {
					*move = false
					go func(w, h int) {
						for i := 0; ctx.Err() == nil; i++ {
							a := float64(i) / 10
							x := w/2 + int(float64(w/6)*math.Cos(a))
							y := h/2 + int(float64(h/6)*math.Sin(a))
							wsjson.Write(ctx, c, map[string]any{"t": "mm", "x": x, "y": y})
							time.Sleep(33 * time.Millisecond)
						}
					}(m.W, m.H)
				}
				if *disp >= 0 {
					wsjson.Write(ctx, c, map[string]any{"t": "disp", "i": *disp})
					*disp = -1
				}
			}
			continue
		}
		total += len(data)
		switch data[0] {
		case 1:
			rects++
			x := int(binary.LittleEndian.Uint16(data[1:]))
			y := int(binary.LittleEndian.Uint16(data[3:]))
			img, err := jpeg.Decode(bytes.NewReader(data[9:]))
			if err != nil {
				log.Fatalf("jpeg: %v", err)
			}
			if canvas != nil {
				draw.Draw(canvas, img.Bounds().Add(image.Pt(x, y)), img, image.Point{}, draw.Src)
			}
		case 2:
			gotFrame()
			if frames == 1 && *out != "" && canvas != nil {
				f, _ := os.Create(*out)
				png.Encode(f, canvas)
				f.Close()
			}
			c.Write(ctx, websocket.MessageText, []byte(`{"t":"ack"}`))
		case 3:
			gotFrame()
			if data[1] == 1 {
				keys++
			}
			if vf != nil {
				vf.Write(data[2:])
			}
			c.Write(ctx, websocket.MessageText, []byte(`{"t":"vack"}`))
		}
	}
	report()
}
