// fakesite serves the test product pages (testdata/) like McMaster would,
// for smoke-testing the Docker image in CI: go run ./tools/fakesite -addr :8765
package main

import (
	"encoding/base64"
	"flag"
	"log"
	"net/http"
	"strings"
)

var png1x1, _ = base64.StdEncoding.DecodeString(
	"iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNk+M9QDwADhgGAWjR9awAAAABJRU5ErkJggg==")

func main() {
	addr := flag.String("addr", ":8765", "listen address")
	dir := flag.String("dir", "testdata", "fixture folder")
	flag.Parse()
	log.Fatal(http.ListenAndServe(*addr, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/img/"):
			w.Header().Set("Content-Type", "image/png")
			w.Write(png1x1)
		case strings.HasPrefix(r.URL.Path, "/91251A540"):
			http.ServeFile(w, r, *dir+"/product.html")
		case strings.HasPrefix(r.URL.Path, "/8336N108"):
			http.ServeFile(w, r, *dir+"/tiered.html")
		default:
			http.ServeFile(w, r, *dir+"/blocked.html")
		}
	})))
}
