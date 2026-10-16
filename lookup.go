package main

import (
	"context"
	"fmt"
	"log"
	"net/url"
	"os"
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

	siteHost     string // "mcmaster.com": requests elsewhere are third-party
	freshProfile string // empty profile used after a session reset (see resetSession)
	dietOff      bool   // set if refusing third-party requests ever broke a lookup

	// The fast path (see direct.go): an open McMaster tab from which a lookup
	// asks the site's data calls directly, and their version segment.
	apiTab      context.Context
	apiCancel   func()
	mv          string
	infoTmpl    string // the data call URLs as the page last sent them, part number as {PN}
	contentTmpl string
	lastHow     string // "direct" or "page": how the last lookup got its data (tests, log)
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
	p, err := s.lookupAny(pn)
	if err == nil && len(p.components) > 0 {
		s.fillVariants(p)
	}
	return p, err
}

// lookupAny gets the product details: the fast path when it can, else the page.
func (s *Scraper) lookupAny(pn string) (*Part, error) {
	// Fast path first: ask McMaster's data calls from the open tab.
	p, err := s.direct(pn)
	switch {
	case err == nil:
		return p, nil
	case err == errWall:
		// The session expired or was refused. Cheapest first: reload the
		// kept tab so McMaster authorizes the same visitor again; then a new
		// visitor warmed until McMaster answers; then a full page load.
		log.Printf("lookup %s: McMaster refused the data call; renewing the session", pn)
		if s.reauth() {
			if p, err := s.direct(pn); err == nil {
				return p, nil
			}
		}
		s.resetSession()
		if s.warm() {
			if p, err := s.direct(pn); err == nil {
				return p, nil
			}
		}
	case err == errIncomplete:
		log.Printf("lookup %s: the data call left out the price and choices; loading the page", pn)
	case err != errNoFastPath && err != errNotProduct:
		log.Printf("lookup %s: direct data call failed (%v); loading the page instead", pn, err)
		s.closeAPITab()
	}
	diet := !s.dietOff
	p, kind, err := s.lookup(pn, diet)
	if kind == failWall {
		// The anonymous session hit McMaster's page limit: forget it (cookies
		// and all) and try once more as a new visitor.
		log.Printf("lookup %s: McMaster's login wall; starting a fresh session and retrying once", pn)
		s.resetSession()
		if p, kind, err = s.lookup(pn, diet); kind == failWall {
			return nil, &LookupError{Msg: "McMaster מבקשים להתחבר כדי להמשיך לגלוש, גם אחרי פתיחת חיבור חדש. כנראה חסימה זמנית לפי כתובת השרת – נסה שוב בעוד כמה דקות, או הזן את הפרטים ידנית."}
		}
		return p, err
	}
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

// Warm starts the browser and opens McMaster's home page in a tab that stays
// open, so lookups can use the fast path from the very first search. If the
// session is refused, it starts over once as a new visitor.
func (s *Scraper) Warm() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.warm() {
		s.resetSession()
		s.warm()
	}
}

// warm opens the fast-path tab and waits until McMaster actually answers the
// data call from it. Finding the site version is not enough: McMaster
// authorizes a new visitor a few seconds after the page starts loading, and
// until then refuses the calls (403), which made the first search after a
// restart fall back to slow page loads.
func (s *Scraper) warm() bool {
	if err := s.ensureBrowser(); err != nil {
		log.Printf("browser warm-up: %v", err)
		return false
	}
	tab, cancel, _, err := s.openTab(!s.dietOff)
	if err != nil {
		return false
	}
	t0 := time.Now()
	ctx, cancelT := context.WithTimeout(tab, s.Timeout)
	defer cancelT()
	if chromedp.Run(ctx, navigateNoWait(s.BaseURL+"/")) != nil {
		cancel()
		return false
	}
	// The site version segment shows up in the page's own script/data URLs.
	for end := time.Now().Add(s.Timeout); time.Now().Before(end); time.Sleep(300 * time.Millisecond) {
		var mv string
		if chromedp.Run(ctx, chromedp.Evaluate(mvFromPageJS, &mv)) == nil && mv != "" {
			s.keepAPITab(tab, cancel, mv)
			break
		}
	}
	if s.apiTab != tab {
		cancel()
		log.Printf("warm-up: no site version found; lookups will load pages")
		return false
	}
	status := 0
	for end := time.Now().Add(15 * time.Second); time.Now().Before(end); time.Sleep(time.Second) {
		if status = s.probe(); status == 200 {
			log.Printf("warm-up: fast path ready (%s) after %.1fs", s.mv, time.Since(t0).Seconds())
			return true
		}
	}
	log.Printf("warm-up: McMaster keeps refusing the data call (status %d)", status)
	s.closeAPITab()
	return false
}

// reauth reloads McMaster's page in the kept tab (same browser, same
// cookies) and waits until the data call answers again: McMaster's page
// renews the visitor's authorization as it loads. Much faster than starting
// a new browser on a small server.
func (s *Scraper) reauth() bool {
	if s.apiTab == nil || s.apiTab.Err() != nil {
		return false
	}
	t0 := time.Now()
	// A real reload, past the browser cache, so the page asks McMaster again.
	ctx, cancel := context.WithTimeout(s.apiTab, 5*time.Second)
	err := chromedp.Run(ctx, chromedp.ActionFunc(func(c context.Context) error {
		return page.Reload().WithIgnoreCache(true).Do(c)
	}))
	cancel()
	if err != nil {
		return false
	}
	for end := time.Now().Add(12 * time.Second); time.Now().Before(end); time.Sleep(time.Second) {
		var mv string
		if chromedp.Run(s.apiTab, chromedp.Evaluate(mvFromPageJS, &mv)) == nil && mv != "" && mv != s.mv {
			s.mv = mv // McMaster deployed meanwhile
		}
		if s.probe() == 200 {
			log.Printf("session renewed in the same browser after %.1fs", time.Since(t0).Seconds())
			return true
		}
	}
	return false
}

// probePN is any real part number; asking for it tells whether McMaster
// still answers this session's data calls.
const probePN = "91251A540"

// probe asks the data call once from the fast-path tab and returns the HTTP
// status (0 if it could not ask).
func (s *Scraper) probe() int {
	if s.apiTab == nil || s.apiTab.Err() != nil || s.mv == "" {
		return 0
	}
	ctx, cancel := context.WithTimeout(s.apiTab, 10*time.Second)
	defer cancel()
	info, content := s.callURLs(probePN)
	var res struct{ Info struct{ Status int } }
	if chromedp.Run(ctx, chromedp.Evaluate(fmt.Sprintf(directJS, jsString(info), jsString(content)), &res,
		func(p *runtime.EvaluateParams) *runtime.EvaluateParams { return p.WithAwaitPromise(true) })) != nil {
		return 0
	}
	return res.Info.Status
}

// KeepWarm checks the session now and then while nobody is searching (work
// hours only) and renews it when McMaster stopped answering, so the next
// search finds a working fast path instead of paying for the recovery.
// MCM_KEEPWARM=0 turns it off, =always ignores the hours.
func (s *Scraper) KeepWarm(every time.Duration) {
	mode := os.Getenv("MCM_KEEPWARM")
	if mode == "0" {
		return
	}
	for range time.Tick(every) {
		if mode != "always" && !workHours(time.Now()) {
			continue
		}
		s.mu.Lock()
		if time.Since(s.last) >= every {
			s.refreshSession()
		}
		s.mu.Unlock()
	}
}

// refreshSession renews the session if McMaster refuses its data calls.
func (s *Scraper) refreshSession() {
	st := s.probe()
	if st == 200 {
		return
	}
	log.Printf("keep-warm: data call answered %d; renewing the McMaster session", st)
	if s.reauth() {
		return
	}
	s.resetSession()
	if !s.warm() {
		s.resetSession()
		s.warm()
	}
}

// workHours: every day 6:00-23:00 (server time; render.yaml sets
// Asia/Jerusalem). People search on Fridays and evenings too; at night one
// slow first search is fine.
func workHours(t time.Time) bool {
	h := t.Hour()
	return h >= 6 && h < 23
}

// resetSession makes the next request come from a new anonymous visitor: the
// browser restarts with an empty profile (no cookies, storage or cache from
// any McMaster address). Clearing cookies alone was not always enough.
func (s *Scraper) resetSession() {
	s.closeAPITab()
	if s.stopBrowser != nil {
		s.stopBrowser()
	}
	s.browserCtx, s.stopBrowser = nil, nil
	if s.freshProfile != "" {
		os.RemoveAll(s.freshProfile)
	}
	s.freshProfile, _ = os.MkdirTemp(filepath.Dir(s.ProfileDir), "browser-fresh-")
}

// Close shuts the browser down (on exit, so no Chrome is left running).
func (s *Scraper) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closeAPITab()
	if s.stopBrowser != nil {
		s.stopBrowser()
		s.browserCtx, s.stopBrowser = nil, nil
	}
	if s.freshProfile != "" {
		os.RemoveAll(s.freshProfile)
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
	profile := s.ProfileDir
	if s.freshProfile != "" {
		profile = s.freshProfile
	}
	err := try(profile)
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
		network.ResourceTypeCSPViolationReport, network.ResourceTypeTextTrack,
		network.ResourceTypeImage: // no product picture any more: skip all images
		return true
	case network.ResourceTypeDocument:
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
// Bits: 1 = a heading exists, 2 = a price is in the text, 4 = block/404 text,
// 8 = McMaster's "please log in to continue browsing" wall.
const readyJS = `(() => {
	const b = document.body; if (!b) return 0;
	const t = b.textContent || "";
	return (document.querySelector("h1, h2, h3") ? 1 : 0) +
		(/\$\s?[\d,]+\.\d{2}/.test(t) ? 2 : 0) +
		(/access denied|unusual traffic|are you a robot|captcha|no (?:products|results) (?:were )?found|not a valid part number/i.test(t) ? 4 : 0) +
		(/continue browsing/i.test(t) ? 8 : 0);
})()`

// Lookup kinds of failure, used to decide whether a retry without the diet
// could help.
const (
	failLoad         = "load"
	failUnrecognized = "unrecognized"
	failWall         = "wall" // McMaster's login wall: a fresh session may help
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
	keepTab := false // a lookup that got its data from the site's calls keeps the tab for the fast path
	defer func() {
		if !keepTab {
			cancelTab()
		}
	}()
	ctx, cancelT := context.WithTimeout(tab, s.Timeout+15*time.Second)
	defer cancelT()
	// Note the page's data requests (saved before the tab closes; see recorder.go).
	rec := startRecorder(tab, pn)
	outcome := "failed"
	defer func() { rec.save(s.DebugDir, pageURL, outcome) }()

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
		// Best source: the site's own product data call, which answers even
		// when the page shows "Log in to view Product Detail".
		if body, u, ok := rec.find(orderInfoPath); ok {
			if p, ok := parseOrderInfo(body, pn); ok {
				outcome, keepTab, s.lastHow = "ok", true, "page"
				if m := mvSegmentRe.FindStringSubmatch(u); m != nil {
					s.keepAPITab(tab, cancelTab, m[1])
					_, cu, _ := rec.find(contentPath)
					s.rememberCallForms(pn, u, cu)
				} else {
					keepTab = false
				}
				log.Printf("lookup %s: product data after %.1fs (from the site's data call); %d price tiers",
					pn, time.Since(t0).Seconds(), len(p.Tiers))
				return p, "", nil
			}
		}
		var ready int
		_ = chromedp.Run(ctx, chromedp.Evaluate(readyJS, &ready))
		if ready&8 != 0 {
			s.saveDebug(ctx, pn)
			return nil, failWall, &LookupError{Msg: "McMaster מבקשים להתחבר כדי להמשיך לגלוש."}
		}
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
		Unit: data.Unit, Tiers: data.Tiers, Source: "auto",
	}
	if len(p.NameOptions) > 6 {
		p.NameOptions = p.NameOptions[:6]
	}
	outcome, s.lastHow = "ok", "page"
	log.Printf("lookup %s: product shown after %.1fs, done %.1fs; diet=%v blocked=%d %v",
		pn, tShown.Seconds(), time.Since(t0).Seconds(), diet, stats.blocked.Load(), stats.hosts())
	return p, "", nil
}

// settle takes one more look a moment later: the price table can finish a
// beat after the price.
func (s *Scraper) settle(ctx context.Context, expr string, data extracted) extracted {
	time.Sleep(300 * time.Millisecond)
	var d extracted
	if chromedp.Run(ctx, chromedp.Evaluate(expr, &d)) == nil && len(d.Names) > 0 && len(d.Tiers) >= len(data.Tiers) {
		data = d
	}
	return data
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
