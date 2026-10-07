// McMaster order list: a single-file local web app. Double-click the .exe,
// it opens the page in the browser; other office computers can browse to it.
package main

import (
	"encoding/json"
	"fmt"
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

// dataDir picks where the list, images and debug files live. The folder next
// to the program comes first (easy to find and back up), but only if it can be
// written: macOS keeps Terminal out of Downloads/Desktop/Documents by default.
// Otherwise the per-user app-data folder, which is always writable:
// ~/Library/Application Support/McMasterList on a Mac, %AppData%\McMasterList
// on Windows.
func dataDir() string {
	if d := os.Getenv("MCM_DATA_DIR"); d != "" {
		return d
	}
	var candidates []string
	if exe, err := os.Executable(); err == nil {
		if real, err := filepath.EvalSymlinks(exe); err == nil {
			exe = real
		}
		candidates = append(candidates, filepath.Join(filepath.Dir(exe), "McMasterList-data"))
	}
	if cfg, err := os.UserConfigDir(); err == nil {
		candidates = append(candidates, filepath.Join(cfg, "McMasterList"))
	}
	for _, c := range candidates {
		if writable(c) {
			return c
		}
	}
	return "McMasterList-data"
}

func writable(dir string) bool {
	if os.MkdirAll(dir, 0o755) != nil {
		return false
	}
	probe := filepath.Join(dir, ".write-test")
	if os.WriteFile(probe, []byte("ok"), 0o644) != nil {
		return false
	}
	os.Remove(probe)
	return true
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

// fail shows the error and keeps the window open until Enter, so it can be
// read (and photographed) instead of flashing by.
func fail(msg string) {
	fmt.Println("\n  Error:", msg)
	fmt.Println("\n  Press Enter to close.")
	fmt.Scanln()
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

	defer func() {
		if r := recover(); r != nil {
			fail(fmt.Sprint("unexpected crash: ", r))
		}
	}()

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
	fmt.Println("  If the browser did not open, copy the address above into it.")
	fmt.Println()
	if !noOpen {
		openBrowser(localURL)
	}
	if err := http.Serve(ln, srv); err != nil {
		fail("server stopped: " + err.Error())
	}
}
