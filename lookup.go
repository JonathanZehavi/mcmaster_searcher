package main

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"log"
	"mime"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/chromedp/cdproto/browser"
	"github.com/chromedp/cdproto/emulation"
	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/chromedp"
)

var partRe = regexp.MustCompile(`^[0-9]{1,6}[A-Z][0-9]{1,6}$`)
var nonAlnum = regexp.MustCompile(`[^0-9A-Za-z]`)

func normalizePartNumber(raw string) string {
	return strings.ToUpper(nonAlnum.ReplaceAllString(raw, ""))
}
func looksLikePartNumber(pn string) bool { return partRe.MatchString(pn) }

// LookupError is a failed lookup; its message is shown to the user.
type LookupError struct{ Msg string }

func (e *LookupError) Error() string { return e.Msg }

type extracted struct {
	PartNumber string      `json:"partNumber"`
	Names      []string    `json:"names"`
	Unit       string      `json:"unit"`
	Price      string      `json:"price"`
	Tiers      []PriceTier `json:"tiers"`
	Image      string      `json:"image"`
	URL        string      `json:"url"`
	Blocked    bool        `json:"blocked"`
	NotFound   bool        `json:"notFound"`
	TextLength int         `json:"textLength"`
}

// Scraper opens a McMaster-Carr product page in the computer's own Chrome or
// Edge (hidden) and reads the details off the page. One lookup at a time, with
// a gap between lookups, so traffic looks like one person browsing.
type Scraper struct {
	BaseURL     string
	BrowserPath string
	Headless    bool
	MinInterval time.Duration
	Timeout     time.Duration
	ProfileDir  string
	ImagesDir   string
	DebugDir    string
	Extractor   Extractor

	mu   sync.Mutex
	last time.Time

	// One browser stays open between lookups (each lookup gets a fresh tab):
	// starting Chrome every time cost seconds, and a warm cache makes McMaster's
	// own scripts load much faster.
	browserCtx  context.Context
	stopBrowser func()
	userAgent   string
}

func NewScraper(dataDir string) *Scraper {
	s := &Scraper{
		BaseURL:     strings.TrimRight(envOr("MCM_BASE_URL", "https://www.mcmaster.com"), "/"),
		BrowserPath: envOr("MCM_BROWSER_PATH", findBrowser()),
		Headless:    os.Getenv("MCM_HEADLESS") != "0",
		MinInterval: envDuration("MCM_MIN_INTERVAL", 2*time.Second),
		Timeout:     envDuration("MCM_TIMEOUT", 20*time.Second),
		ProfileDir:  filepath.Join(dataDir, "browser-profile"),
		ImagesDir:   filepath.Join(dataDir, "images"),
		DebugDir:    filepath.Join(dataDir, "debug"),
		Extractor:   Extractor{OverridePath: filepath.Join(dataDir, "extractor.js")},
	}
	for _, d := range []string{s.ProfileDir, s.ImagesDir, s.DebugDir} {
		os.MkdirAll(d, 0o755)
	}
	return s
}

// findBrowser returns Chrome if installed, else Edge (which ships with Windows).
// Elsewhere chromedp's own search covers Chromium.
func findBrowser() string {
	var candidates []string
	for _, env := range []string{"ProgramFiles", "ProgramFiles(x86)", "LocalAppData"} {
		if base := os.Getenv(env); base != "" {
			candidates = append(candidates, filepath.Join(base, `Google\Chrome\Application\chrome.exe`))
		}
	}
	for _, env := range []string{"ProgramFiles(x86)", "ProgramFiles"} {
		if base := os.Getenv(env); base != "" {
			candidates = append(candidates, filepath.Join(base, `Microsoft\Edge\Application\msedge.exe`))
		}
	}
	candidates = append(candidates,
		"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
		"/Applications/Microsoft Edge.app/Contents/MacOS/Microsoft Edge")
	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			return c
		}
	}
	return "" // let chromedp search the usual places (Linux/macOS)
}

func (s *Scraper) Lookup(pn string) (*Part, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if wait := s.MinInterval - time.Since(s.last); wait > 0 {
		time.Sleep(wait)
	}
	defer func() { s.last = time.Now() }()
	start := time.Now()
	p, err := s.lookup(pn)
	log.Printf("lookup %s took %.1fs", pn, time.Since(start).Seconds())
	return p, err
}

// Warm starts the browser and loads McMaster's home page once, so the first
// real lookup finds the browser running and the site's scripts cached.
func (s *Scraper) Warm() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ensureBrowser(); err != nil {
		log.Printf("browser warm-up: %v", err)
		return
	}
	tab, cancel := chromedp.NewContext(s.browserCtx)
	defer cancel()
	ctx, cancelT := context.WithTimeout(tab, s.Timeout)
	defer cancelT()
	_ = chromedp.Run(ctx, s.prepareTab(), navigateNoWait(s.BaseURL+"/"), chromedp.Sleep(3*time.Second))
}

// Close shuts the browser down (on exit, so no Chrome is left running).
func (s *Scraper) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopBrowser != nil {
		s.stopBrowser()
		s.browserCtx, s.stopBrowser = nil, nil
	}
}

func (s *Scraper) allocatorOptions(profile string) []chromedp.ExecAllocatorOption {
	opts := append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.UserDataDir(profile),
		chromedp.WindowSize(1366, 900),
		chromedp.Flag("enable-automation", false),
		chromedp.Flag("disable-blink-features", "AutomationControlled"),
		chromedp.Flag("lang", "en-US"),
		chromedp.Flag("disable-dev-shm-usage", true), // containers have a tiny /dev/shm
	)
	if s.Headless {
		opts = append(opts, chromedp.Flag("headless", "new"))
	} else {
		opts = append(opts, chromedp.Flag("headless", false))
	}
	if os.Getenv("MCM_NO_SANDBOX") == "1" { // CI containers only
		opts = append(opts, chromedp.NoSandbox)
	}
	if s.BrowserPath != "" {
		opts = append(opts, chromedp.ExecPath(s.BrowserPath))
	}
	return opts
}

// ensureBrowser starts the shared browser if it is not running (first use,
// or it crashed / was closed).
func (s *Scraper) ensureBrowser() error {
	if s.browserCtx != nil && s.browserCtx.Err() == nil {
		return nil
	}
	if s.stopBrowser != nil {
		s.stopBrowser()
	}
	try := func(profile string) error {
		allocCtx, cancelAlloc := chromedp.NewExecAllocator(context.Background(), s.allocatorOptions(profile)...)
		bctx, cancelB := chromedp.NewContext(allocCtx)
		var ua string
		err := chromedp.Run(bctx, chromedp.ActionFunc(func(c context.Context) error {
			_, _, _, userAgent, _, err := browser.GetVersion().Do(c)
			ua = userAgent
			return err
		}))
		if err != nil {
			cancelB()
			cancelAlloc()
			return err
		}
		s.browserCtx, s.userAgent = bctx, strings.Replace(ua, "HeadlessChrome", "Chrome", 1)
		s.stopBrowser = func() { cancelB(); cancelAlloc() }
		return nil
	}
	err := try(s.ProfileDir)
	if err != nil {
		// The saved profile can be locked by a Chrome left over from a crash;
		// a throwaway profile still works (only cookies are lost).
		if tmp, terr := os.MkdirTemp("", "mcm-profile-"); terr == nil {
			err = try(tmp)
		}
	}
	if err != nil {
		return &LookupError{"לא הצלחתי להפעיל את הדפדפן (Chrome / Edge): " + firstLine(err.Error())}
	}
	return nil
}

// Things the page does not need for us to read it: fonts, video, trackers.
var blockedURLs = []string{
	"*.woff", "*.woff2", "*.ttf", "*.otf", "*.mp4", "*.webm",
	"*google-analytics.com*", "*googletagmanager.com*", "*doubleclick.net*",
	"*facebook.net*", "*hotjar*", "*bing.com*", "*clarity.ms*",
}

func (s *Scraper) prepareTab() chromedp.Action {
	return chromedp.ActionFunc(func(ctx context.Context) error {
		if err := emulation.SetUserAgentOverride(s.userAgent).WithAcceptLanguage("en-US,en").Do(ctx); err != nil {
			return err
		}
		if err := network.Enable().Do(ctx); err != nil {
			return err
		}
		return network.SetBlockedURLs(blockedURLs).Do(ctx)
	})
}

// navigateNoWait starts loading a page without waiting for its load event;
// McMaster keeps loading extras long after the product details are on screen.
func navigateNoWait(url string) chromedp.Action {
	return chromedp.ActionFunc(func(ctx context.Context) error {
		_, _, errText, _, err := page.Navigate(url).Do(ctx)
		if err == nil && errText != "" {
			err = fmt.Errorf("%s", errText)
		}
		return err
	})
}

func (s *Scraper) lookup(pn string) (*Part, error) {
	if err := s.ensureBrowser(); err != nil {
		return nil, err
	}
	pageURL := fmt.Sprintf("%s/%s/", s.BaseURL, pn)
	tab, cancelTab := chromedp.NewContext(s.browserCtx)
	defer cancelTab()
	ctx, cancelT := context.WithTimeout(tab, s.Timeout+15*time.Second)
	defer cancelT()

	if err := chromedp.Run(ctx, s.prepareTab(), navigateNoWait(pageURL)); err != nil {
		if s.browserCtx.Err() != nil { // the browser itself died; restart next time
			s.browserCtx = nil
		}
		debug := s.saveDebug(ctx, pn)
		return nil, &LookupError{fmt.Sprintf("הדף של McMaster לא נטען: %s (נשמר דיבאג: %s).", firstLine(err.Error()), debug)}
	}

	// Read the page as it renders and stop as soon as the details are there,
	// instead of waiting for everything to finish loading.
	js, external := s.Extractor.JS()
	expr := "(" + js + "\n)()"
	var data extracted
	var evalErr error
	deadline := time.Now().Add(s.Timeout)
	var namesSince time.Time
	for time.Now().Before(deadline) {
		var d extracted
		evalErr = chromedp.Run(ctx, chromedp.Evaluate(expr, &d))
		if evalErr == nil {
			data = d
			complete := len(d.Names) > 0 && (d.Price != "" || len(d.Tiers) > 0)
			if len(d.Names) > 0 && namesSince.IsZero() {
				namesSince = time.Now()
			}
			// Done: details are complete, the product has a name but shows no
			// price after a few seconds, or the page is clearly a block/404
			// (a near-empty page early on is just still rendering).
			if complete || d.NotFound || (d.Blocked && d.TextLength >= 40) ||
				(!namesSince.IsZero() && time.Since(namesSince) > 3*time.Second) {
				if complete {
					// One more look a moment later: the price table can finish a beat after the price.
					time.Sleep(400 * time.Millisecond)
					if chromedp.Run(ctx, chromedp.Evaluate(expr, &d)) == nil && len(d.Tiers) >= len(data.Tiers) {
						data = d
					}
				}
				break
			}
		}
		time.Sleep(250 * time.Millisecond)
	}

	if evalErr != nil && len(data.Names) == 0 {
		debug := s.saveDebug(ctx, pn)
		if external {
			return nil, &LookupError{fmt.Sprintf("קובץ הזיהוי החיצוני (extractor.js) נכשל: %s (נשמר דיבאג: %s).", firstLine(evalErr.Error()), debug)}
		}
		return nil, &LookupError{fmt.Sprintf("הדף של McMaster לא נטען (נשמר דיבאג: %s).", debug)}
	}
	if data.Blocked || data.NotFound || len(data.Names) == 0 {
		debug := s.saveDebug(ctx, pn)
		switch {
		case data.NotFound:
			return nil, &LookupError{"McMaster לא מצא את המק״ט הזה."}
		case data.Blocked:
			return nil, &LookupError{fmt.Sprintf("נראה ש-McMaster חסם את הבדיקה האוטומטית (נשמר דיבאג: %s).", debug)}
		default:
			return nil, &LookupError{fmt.Sprintf("הדף נטען אבל לא זיהיתי שם מוצר (נשמר דיבאג: %s).", debug)}
		}
	}

	p := &Part{
		PartNumber: pn, Name: data.Names[0], NameOptions: data.Names,
		Unit: data.Unit, Tiers: data.Tiers, ImageURL: data.Image, Source: "auto",
	}
	if len(p.NameOptions) > 6 {
		p.NameOptions = p.NameOptions[:6]
	}
	if data.Image != "" {
		p.ImageFile = s.saveImage(ctx, pn, data.Image)
	}
	return p, nil
}

// saveImage downloads the product image, from inside the page first (same
// cookies as the page), falling back to a plain request.
func (s *Scraper) saveImage(ctx context.Context, pn, imageURL string) string {
	var dataURL string
	js := fmt.Sprintf(`fetch(%q).then(r => r.ok ? r.blob() : null).then(b => b ? new Promise(res => {
		const fr = new FileReader(); fr.onload = () => res(fr.result); fr.readAsDataURL(b); }) : "").catch(() => "")`, imageURL)
	_ = chromedp.Run(ctx, chromedp.Evaluate(js, &dataURL, func(p *runtime.EvaluateParams) *runtime.EvaluateParams {
		return p.WithAwaitPromise(true)
	}))
	var body []byte
	var ctype string
	if meta, b64, ok := strings.Cut(dataURL, ";base64,"); ok {
		ctype = strings.TrimPrefix(meta, "data:")
		body, _ = base64.StdEncoding.DecodeString(b64)
	}
	if len(body) == 0 {
		client := &http.Client{Timeout: 15 * time.Second}
		resp, err := client.Get(imageURL)
		if err != nil {
			return ""
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			return ""
		}
		body, _ = io.ReadAll(io.LimitReader(resp.Body, 10<<20))
		ctype = resp.Header.Get("Content-Type")
	}
	if len(body) == 0 {
		return ""
	}
	ext := path.Ext(strings.SplitN(imageURL, "?", 2)[0])
	if exts, _ := mime.ExtensionsByType(strings.Split(ctype, ";")[0]); len(exts) > 0 {
		ext = exts[0]
		for _, e := range exts {
			if e == ".jpg" || e == ".png" || e == ".gif" || e == ".webp" {
				ext = e
			}
		}
	}
	if ext == "" {
		ext = ".img"
	}
	name := pn + ext
	if err := os.WriteFile(filepath.Join(s.ImagesDir, name), body, 0o644); err != nil {
		return ""
	}
	return name
}

func (s *Scraper) saveDebug(ctx context.Context, pn string) string {
	base := fmt.Sprintf("%s-%s", pn, time.Now().Format("20060102-150405"))
	var html string
	var shot []byte
	if err := chromedp.Run(ctx, chromedp.OuterHTML("html", &html, chromedp.ByQuery)); err == nil {
		os.WriteFile(filepath.Join(s.DebugDir, base+".html"), []byte(html), 0o644)
	}
	if err := chromedp.Run(ctx, chromedp.FullScreenshot(&shot, 80)); err == nil {
		os.WriteFile(filepath.Join(s.DebugDir, base+".jpg"), shot, 0o644)
	}
	return base
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	return line
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envDuration(key string, def time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		var secs float64
		if _, err := fmt.Sscan(v, &secs); err == nil {
			return time.Duration(secs * float64(time.Second))
		}
	}
	return def
}
