package main

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
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
	PartNumber string   `json:"partNumber"`
	Names      []string `json:"names"`
	Unit       string   `json:"unit"`
	Price      string   `json:"price"`
	Image      string   `json:"image"`
	URL        string   `json:"url"`
	Blocked    bool     `json:"blocked"`
	NotFound   bool     `json:"notFound"`
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
}

func NewScraper(dataDir string) *Scraper {
	s := &Scraper{
		BaseURL:     strings.TrimRight(envOr("MCM_BASE_URL", "https://www.mcmaster.com"), "/"),
		BrowserPath: envOr("MCM_BROWSER_PATH", findBrowser()),
		Headless:    os.Getenv("MCM_HEADLESS") != "0",
		MinInterval: envDuration("MCM_MIN_INTERVAL", 4*time.Second),
		Timeout:     envDuration("MCM_TIMEOUT", 25*time.Second),
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
	return s.lookup(pn)
}

func (s *Scraper) lookup(pn string) (*Part, error) {
	pageURL := fmt.Sprintf("%s/%s/", s.BaseURL, pn)

	opts := append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.UserDataDir(s.ProfileDir),
		chromedp.WindowSize(1366, 900),
		chromedp.Flag("enable-automation", false),
		chromedp.Flag("disable-blink-features", "AutomationControlled"),
		chromedp.Flag("lang", "en-US"),
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
	allocCtx, cancelAlloc := chromedp.NewExecAllocator(context.Background(), opts...)
	defer cancelAlloc()
	ctx, cancel := chromedp.NewContext(allocCtx)
	defer cancel()
	ctx, cancelT := context.WithTimeout(ctx, s.Timeout+20*time.Second)
	defer cancelT()

	// Headless browsers announce themselves as "HeadlessChrome"; look like the normal one.
	var ua string
	if err := chromedp.Run(ctx, chromedp.ActionFunc(func(c context.Context) error {
		_, _, _, userAgent, _, err := browser.GetVersion().Do(c)
		ua = strings.Replace(userAgent, "HeadlessChrome", "Chrome", 1)
		return err
	})); err != nil {
		return nil, &LookupError{"לא הצלחתי להפעיל את הדפדפן (Chrome / Edge): " + firstLine(err.Error())}
	}

	navCtx, cancelNav := context.WithTimeout(ctx, s.Timeout)
	_ = chromedp.Run(navCtx,
		emulation.SetUserAgentOverride(ua).WithAcceptLanguage("en-US,en"),
		chromedp.Navigate(pageURL),
	)
	// The page renders client-side; wait for a price or a heading, then let it settle.
	var ready bool
	_ = chromedp.Run(navCtx, chromedp.Poll(
		`/\$\s?[\d,]+\.\d{2}/.test(document.body ? document.body.innerText : "") || !!document.querySelector("h1")`,
		&ready, chromedp.WithPollingInterval(300*time.Millisecond)))
	cancelNav()
	_ = chromedp.Run(ctx, chromedp.Sleep(800*time.Millisecond))

	var data extracted
	js, external := s.Extractor.JS()
	if err := chromedp.Run(ctx, chromedp.Evaluate("("+js+"\n)()", &data)); err != nil {
		debug := s.saveDebug(ctx, pn)
		if external {
			return nil, &LookupError{fmt.Sprintf("קובץ הזיהוי החיצוני (extractor.js) נכשל: %s (נשמר דיבאג: %s).", firstLine(err.Error()), debug)}
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
		Unit: data.Unit, Price: data.Price, ImageURL: data.Image, Source: "auto",
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
