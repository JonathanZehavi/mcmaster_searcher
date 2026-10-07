package main

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"log"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/chromedp/cdproto/browser"
	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/emulation"
	"github.com/chromedp/cdproto/fetch"
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
	Headings   int         `json:"headings"` // names found in the rendered page, not metadata
	ImageReady bool        `json:"imageReady"`
	BlockText  bool        `json:"blockText"` // the page says access was blocked
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

	siteHost string // "mcmaster.com": requests elsewhere are third-party
	dietOff  bool   // set if refusing third-party requests ever broke a lookup
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
		dietOff:     os.Getenv("MCM_NO_DIET") == "1",
	}
	if u, err := url.Parse(s.BaseURL); err == nil {
		s.siteHost = strings.TrimPrefix(strings.ToLower(u.Hostname()), "www.")
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
	diet := !s.dietOff
	p, kind, err := s.lookup(pn, diet)
	if err != nil && diet && (kind == failLoad || kind == failUnrecognized) {
		// Maybe the diet refused something the page needs: try once without it.
		// If that works, the diet stays off from now on (and the log says why).
		log.Printf("lookup %s failed with the network diet (%v); retrying without it", pn, err)
		if p2, _, err2 := s.lookup(pn, false); err2 == nil {
			s.dietOff = true
			log.Printf("network diet turned off: the page needs something it refused")
			return p2, nil
		}
	}
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
	tab, cancel, _, err := s.openTab(!s.dietOff)
	if err != nil {
		return
	}
	defer cancel()
	ctx, cancelT := context.WithTimeout(tab, s.Timeout)
	defer cancelT()
	_ = chromedp.Run(ctx, navigateNoWait(s.BaseURL+"/"), chromedp.Sleep(3*time.Second))
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

// ---------- network diet ----------

// Always refused: fonts, video, trackers. The page reads fine without them.
var blockedURLs = []string{
	"*.woff", "*.woff2", "*.ttf", "*.otf", "*.mp4", "*.webm",
	"*google-analytics.com*", "*googletagmanager.com*", "*doubleclick.net*",
	"*facebook.net*", "*hotjar*", "*bing.com*", "*clarity.ms*",
}

// tabStats counts what the network diet did during one lookup.
type tabStats struct {
	blocked    atomic.Int32
	mu         sync.Mutex
	thirdParty map[string]bool // hosts whose scripts/requests were refused
	allow      map[string]bool // URLs let through anyway (the product image)
}

func (t *tabStats) allowURL(u string) {
	t.mu.Lock()
	t.allow[u] = true
	t.mu.Unlock()
}

func (t *tabStats) hosts() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]string, 0, len(t.thirdParty))
	for h := range t.thirdParty {
		out = append(out, h)
	}
	sort.Strings(out)
	return out
}

func (s *Scraper) firstParty(host string) bool {
	host = strings.ToLower(host)
	return host == s.siteHost || strings.HasSuffix(host, "."+s.siteHost)
}

// shouldBlock is the diet: other sites' scripts and calls (analytics, chat,
// ads) are refused, McMaster's own page, scripts and all images load. On a
// small server every extra script costs real seconds of CPU.
func (s *Scraper) shouldBlock(rawURL string, rt network.ResourceType, stats *tabStats) bool {
	switch rt {
	case network.ResourceTypeFont, network.ResourceTypeMedia, network.ResourceTypePing,
		network.ResourceTypeCSPViolationReport, network.ResourceTypeTextTrack:
		return true
	case network.ResourceTypeDocument, network.ResourceTypeImage:
		return false
	}
	u, err := url.Parse(rawURL)
	if err != nil || s.firstParty(u.Hostname()) {
		return false
	}
	stats.mu.Lock()
	defer stats.mu.Unlock()
	if stats.allow[rawURL] {
		return false
	}
	stats.thirdParty[u.Hostname()] = true
	return true
}

// openTab creates a tab with the browser identity set and, when diet is on,
// the request filter installed.
func (s *Scraper) openTab(diet bool) (context.Context, context.CancelFunc, *tabStats, error) {
	tab, cancelTab := chromedp.NewContext(s.browserCtx)
	stats := &tabStats{thirdParty: map[string]bool{}, allow: map[string]bool{}}
	if err := chromedp.Run(tab); err != nil { // creates the tab
		cancelTab()
		return nil, nil, nil, err
	}
	if diet {
		chromedp.ListenTarget(tab, func(ev interface{}) {
			e, ok := ev.(*fetch.EventRequestPaused)
			if !ok {
				return
			}
			go func() {
				ectx := cdp.WithExecutor(tab, chromedp.FromContext(tab).Target)
				if s.shouldBlock(e.Request.URL, e.ResourceType, stats) {
					stats.blocked.Add(1)
					_ = fetch.FailRequest(e.RequestID, network.ErrorReasonBlockedByClient).Do(ectx)
					return
				}
				_ = fetch.ContinueRequest(e.RequestID).Do(ectx)
			}()
		})
	}
	err := chromedp.Run(tab, chromedp.ActionFunc(func(ctx context.Context) error {
		if err := emulation.SetUserAgentOverride(s.userAgent).WithAcceptLanguage("en-US,en").Do(ctx); err != nil {
			return err
		}
		if err := network.Enable().Do(ctx); err != nil {
			return err
		}
		if err := network.SetBlockedURLs(blockedURLs).Do(ctx); err != nil {
			return err
		}
		if diet {
			return fetch.Enable().WithPatterns([]*fetch.RequestPattern{{URLPattern: "*"}}).Do(ctx)
		}
		return nil
	}))
	if err != nil {
		cancelTab()
		return nil, nil, nil, err
	}
	return tab, cancelTab, stats, nil
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

// readyJS is a cheap "has the product appeared?" check. textContent does not
// force the browser to lay out the page, unlike the full read, which matters
// when it runs every 200 ms on a small server.
// Bits: 1 = a heading exists, 2 = a price is in the text, 4 = block/404 text.
const readyJS = `(() => {
	const b = document.body; if (!b) return 0;
	const t = b.textContent || "";
	return (document.querySelector("h1, h2, h3") ? 1 : 0) +
		(/\$\s?[\d,]+\.\d{2}/.test(t) ? 2 : 0) +
		(/access denied|unusual traffic|are you a robot|captcha|no (?:products|results) (?:were )?found|not a valid part number/i.test(t) ? 4 : 0);
})()`

// Lookup kinds of failure, used to decide whether a retry without the diet
// could help.
const (
	failLoad         = "load"
	failUnrecognized = "unrecognized"
)

func (s *Scraper) lookup(pn string, diet bool) (*Part, string, error) {
	if err := s.ensureBrowser(); err != nil {
		return nil, "", err
	}
	t0 := time.Now()
	pageURL := fmt.Sprintf("%s/%s/", s.BaseURL, pn)
	tab, cancelTab, stats, err := s.openTab(diet)
	if err != nil {
		if s.browserCtx.Err() != nil { // the browser itself died; restart next time
			s.browserCtx = nil
		}
		return nil, failLoad, &LookupError{Msg: "לא הצלחתי לפתוח לשונית בדפדפן: " + firstLine(err.Error())}
	}
	defer cancelTab()
	ctx, cancelT := context.WithTimeout(tab, s.Timeout+15*time.Second)
	defer cancelT()

	if err := chromedp.Run(ctx, navigateNoWait(pageURL)); err != nil {
		debug := s.saveDebug(ctx, pn)
		return nil, failLoad, &LookupError{Msg: fmt.Sprintf("הדף של McMaster לא נטען: %s (נשמר דיבאג: %s).", firstLine(err.Error()), debug)}
	}

	// Watch the page render: a cheap check often, the full read when the
	// product looks present (and every ~1.5 s in case the cheap check misses).
	js, external := s.Extractor.JS()
	expr := "(" + js + "\n)()"
	var data extracted
	var evalErr error
	var tShown time.Duration
	deadline := time.Now().Add(s.Timeout)
	var namesSince, lastFull time.Time
	for time.Now().Before(deadline) {
		var ready int
		_ = chromedp.Run(ctx, chromedp.Evaluate(readyJS, &ready))
		if ready&3 == 3 || ready&4 != 0 || time.Since(lastFull) > 1500*time.Millisecond {
			lastFull = time.Now()
			var d extracted
			evalErr = chromedp.Run(ctx, chromedp.Evaluate(expr, &d))
			if evalErr == nil {
				data = d
				// Metadata is there from the first moment; only a name in the
				// rendered page means the product itself has appeared.
				complete := d.Headings > 0 && (d.Price != "" || len(d.Tiers) > 0)
				if d.Headings > 0 && namesSince.IsZero() {
					namesSince = time.Now()
				}
				if complete || d.NotFound || d.BlockText ||
					(!namesSince.IsZero() && time.Since(namesSince) > 3*time.Second) {
					tShown = time.Since(t0)
					if complete {
						data = s.settle(ctx, expr, data)
					}
					break
				}
			}
		}
		time.Sleep(200 * time.Millisecond)
	}

	if evalErr != nil && len(data.Names) == 0 {
		debug := s.saveDebug(ctx, pn)
		if external { // a broken script file: retrying without the diet won't help
			return nil, "", &LookupError{Msg: fmt.Sprintf("קובץ הזיהוי החיצוני (extractor.js) נכשל: %s (נשמר דיבאג: %s).", firstLine(evalErr.Error()), debug)}
		}
		return nil, failLoad, &LookupError{Msg: fmt.Sprintf("הדף של McMaster לא נטען (נשמר דיבאג: %s).", debug)}
	}
	rendered := data.Headings > 0 || (len(data.Names) > 0 && (data.Price != "" || len(data.Tiers) > 0))
	// Only a page that says so is a block page; a near-empty page is one that
	// never rendered (maybe something it needs was refused).
	blocked := data.BlockText
	if blocked || data.NotFound || !rendered {
		debug := s.saveDebug(ctx, pn)
		switch {
		case data.NotFound:
			return nil, "", &LookupError{Msg: "McMaster לא מצא את המק״ט הזה."}
		case blocked:
			return nil, "", &LookupError{Msg: fmt.Sprintf("נראה ש-McMaster חסם את הבדיקה האוטומטית (נשמר דיבאג: %s).", debug)}
		default:
			return nil, failUnrecognized, &LookupError{Msg: fmt.Sprintf("הדף נטען אבל לא זיהיתי שם מוצר (נשמר דיבאג: %s).", debug)}
		}
	}

	p := &Part{
		PartNumber: pn, Name: data.Names[0], NameOptions: data.Names,
		Unit: data.Unit, Tiers: data.Tiers, ImageURL: data.Image, Source: "auto",
	}
	if len(p.NameOptions) > 6 {
		p.NameOptions = p.NameOptions[:6]
	}
	how := "none"
	if data.Image != "" {
		stats.allowURL(data.Image)
		p.ImageFile, how = s.saveImage(ctx, pn, data.Image)
	}
	log.Printf("lookup %s: product shown after %.1fs, done %.1fs; diet=%v blocked=%d %v; image=%s",
		pn, tShown.Seconds(), time.Since(t0).Seconds(), diet, stats.blocked.Load(), stats.hosts(), how)
	return p, "", nil
}

// settle takes one more look a moment later (the price table can finish a beat
// after the price) and gives the product image up to 2 s to finish loading,
// so it can be photographed if it cannot be downloaded.
func (s *Scraper) settle(ctx context.Context, expr string, data extracted) extracted {
	time.Sleep(300 * time.Millisecond)
	for end := time.Now().Add(2 * time.Second); ; {
		var d extracted
		if chromedp.Run(ctx, chromedp.Evaluate(expr, &d)) == nil && len(d.Names) > 0 && len(d.Tiers) >= len(data.Tiers) {
			data = d
		}
		if data.Image == "" || data.ImageReady || time.Now().After(end) {
			return data
		}
		time.Sleep(250 * time.Millisecond)
	}
}

// saveImage stores the product image and says how it got it: the file itself
// fetched from inside the page (same cookies as the page), else a picture of
// the image as shown on screen (works even when downloads are refused), else
// a plain download.
func (s *Scraper) saveImage(ctx context.Context, pn, imageURL string) (string, string) {
	var dataURL string
	js := fmt.Sprintf(`fetch(%q).then(r => r.ok ? r.blob() : null).then(b => b ? new Promise(res => {
		const fr = new FileReader(); fr.onload = () => res(fr.result); fr.readAsDataURL(b); }) : "").catch(() => "")`, imageURL)
	fctx, cancel := context.WithTimeout(ctx, 6*time.Second)
	_ = chromedp.Run(fctx, chromedp.Evaluate(js, &dataURL, func(p *runtime.EvaluateParams) *runtime.EvaluateParams {
		return p.WithAwaitPromise(true)
	}))
	cancel()
	var body []byte
	var ctype, how string
	if meta, b64, ok := strings.Cut(dataURL, ";base64,"); ok {
		ctype = strings.TrimPrefix(meta, "data:")
		if strings.HasPrefix(ctype, "image/") {
			body, _ = base64.StdEncoding.DecodeString(b64)
			how = "fetched"
		}
	}
	if len(body) == 0 {
		var visible bool
		_ = chromedp.Run(ctx, chromedp.Evaluate(`(() => { const el = document.querySelector('[data-mcm-img="1"]');
			return !!(el && el.complete && el.naturalWidth > 0); })()`, &visible))
		if visible {
			sctx, cancel := context.WithTimeout(ctx, 6*time.Second)
			var shot []byte
			if chromedp.Run(sctx, chromedp.Screenshot(`[data-mcm-img="1"]`, &shot, chromedp.ByQuery)) == nil && len(shot) > 0 {
				body, ctype, how = shot, "image/png", "screenshot"
			}
			cancel()
		}
	}
	if len(body) == 0 {
		client := &http.Client{Timeout: 10 * time.Second}
		if resp, err := client.Get(imageURL); err == nil {
			if resp.StatusCode == 200 && strings.HasPrefix(resp.Header.Get("Content-Type"), "image/") {
				body, _ = io.ReadAll(io.LimitReader(resp.Body, 10<<20))
				ctype, how = resp.Header.Get("Content-Type"), "downloaded"
			}
			resp.Body.Close()
		}
	}
	if len(body) == 0 {
		return "", "failed"
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
		return "", "failed"
	}
	return name, how
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
