// McMaster order list: a single-file local web app. Double-click the .exe,
// it opens the page in the browser; other office computers can browse to it.
package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"time"
)

// ---------- startup ----------

func dataDir() string {
	if d := os.Getenv("MCM_DATA_DIR"); d != "" {
		return d
	}
	exe, err := os.Executable()
	if err != nil {
		return "McMasterList-data"
	}
	return filepath.Join(filepath.Dir(exe), "McMasterList-data")
}

func openBrowser(u string) {
	switch runtime.GOOS {
	case "windows":
		exec.Command("rundll32", "url.dll,FileProtocolHandler", u).Start()
	case "darwin":
		exec.Command("open", u).Start()
	default:
		exec.Command("xdg-open", u).Start()
	}
}

func lanAddresses() []string {
	var out []string
	addrs, _ := net.InterfaceAddrs()
	for _, a := range addrs {
		if ipn, ok := a.(*net.IPNet); ok && !ipn.IP.IsLoopback() && ipn.IP.To4() != nil && !ipn.IP.IsLinkLocalUnicast() {
			out = append(out, ipn.IP.String())
		}
	}
	return out
}

func fail(msg string) {
	fmt.Println("\n  שגיאה / Error:", msg)
	if runtime.GOOS == "windows" {
		fmt.Println("\n  Press Enter to close.")
		fmt.Scanln()
	}
	os.Exit(1)
}

var version = "dev" // set at build time

const appID = "mcmaster-order-list"

// alreadyRunning reports whether the program on this port is this app.
func alreadyRunning(port int) bool {
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get(fmt.Sprintf("http://127.0.0.1:%d/api/ping", port))
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	var out struct{ App string }
	return json.NewDecoder(resp.Body).Decode(&out) == nil && out.App == appID
}

func main() {
	startPort, _ := strconv.Atoi(envOr("MCM_PORT", "8642"))
	host := envOr("MCM_HOST", "0.0.0.0")
	noOpen := os.Getenv("MCM_NO_OPEN") == "1"

	// Find a free port. A busy port is either this app already running (then
	// just open it) or another program, e.g. macOS AirPlay on 5000 (then move on).
	var ln net.Listener
	port := startPort
	for ; port < startPort+20; port++ {
		var err error
		if ln, err = net.Listen("tcp", net.JoinHostPort(host, strconv.Itoa(port))); err == nil {
			break
		}
		if alreadyRunning(port) {
			fmt.Printf("\n  Already running at http://localhost:%d - opening it.\n", port)
			if !noOpen {
				openBrowser(fmt.Sprintf("http://localhost:%d", port))
			}
			return
		}
	}
	if ln == nil {
		fail(fmt.Sprintf("no free port between %d and %d", startPort, startPort+19))
	}
	localURL := fmt.Sprintf("http://localhost:%d", port)

	dir := dataDir()
	store, err := OpenStore(filepath.Join(dir, "orders.json"))
	if err != nil {
		fail("cannot open data file: " + err.Error())
	}
	scraper := NewScraper(dir)
	srv := NewServer(store, scraper, scraper.ImagesDir, scraper.Extractor)

	fmt.Println()
	fmt.Println("  McMaster order list " + version + " is running.  Keep this window open; close it to stop.")
	fmt.Println()
	fmt.Println("    On this computer:    " + localURL)
	if host == "0.0.0.0" {
		for _, ip := range lanAddresses() {
			fmt.Printf("    Other computers:     http://%s:%d\n", ip, port)
		}
	}
	fmt.Println("    Data folder:         " + dir)
	if _, external := scraper.Extractor.JS(); external {
		fmt.Println("    Detection:           external extractor.js from the data folder")
	}
	fmt.Println()
	if !noOpen {
		openBrowser(localURL)
	}
	log.Fatal(http.Serve(ln, srv))
}
