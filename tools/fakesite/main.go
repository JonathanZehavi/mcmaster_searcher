// fakesite serves the test product pages (testdata/) like McMaster would,
// for smoke-testing the Docker image in CI: go run ./tools/fakesite -addr :8765
package main

import (
	"bytes"
	"encoding/base64"
	"flag"
	"image"
	"image/color"
	"image/png"
	"log"
	"net/http"
	"strings"
)

var png1x1, _ = base64.StdEncoding.DecodeString(
	"iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNk+M9QDwADhgGAWjR9awAAAABJRU5ErkJggg==")

// productPNG draws a simple bolt-like picture, so screenshots show something.
func productPNG() []byte {
	img := image.NewRGBA(image.Rect(0, 0, 200, 200))
	for y := 0; y < 200; y++ {
		for x := 0; x < 200; x++ {
			c := color.RGBA{255, 255, 255, 255}
			if (x > 85 && x < 115 && y > 40 && y < 185) || (x > 55 && x < 145 && y > 20 && y < 50) {
				c = color.RGBA{110, 120, 135, 255}
			}
			img.Set(x, y, c)
		}
	}
	var buf bytes.Buffer
	png.Encode(&buf, img)
	return buf.Bytes()
}

func main() {
	addr := flag.String("addr", ":8765", "listen address")
	dir := flag.String("dir", "testdata", "fixture folder")
	flag.Parse()
	log.Fatal(http.ListenAndServe(*addr, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/img/protected"):
			// Refuses downloads: only an <img> on the page gets it.
			if r.Header.Get("Sec-Fetch-Dest") != "image" {
				http.Error(w, "forbidden", 403)
				return
			}
			w.Header().Set("Content-Type", "image/png")
			w.Write(productPNG())
		case strings.HasPrefix(r.URL.Path, "/img/"):
			w.Header().Set("Content-Type", "image/png")
			w.Write(png1x1)
		case strings.HasPrefix(r.URL.Path, "/91251A540"):
			http.ServeFile(w, r, *dir+"/product.html")
		case strings.HasPrefix(r.URL.Path, "/8336N108"):
			http.ServeFile(w, r, *dir+"/tiered.html")
		case strings.HasPrefix(r.URL.Path, "/94895A031"):
			http.ServeFile(w, r, *dir+"/slow.html")
		default:
			http.ServeFile(w, r, *dir+"/blocked.html")
		}
	})))
}
