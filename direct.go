package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/chromedp"
)

// The fast path. Loading a McMaster page takes seconds; the two data calls
// that carry the product details (see mcmdata.go) answer in a fraction of
// that. Once a McMaster tab is open (from the warm-up, or kept after a page
// lookup), a lookup asks those calls from inside it: same site, same cookies,
// no page to render. If anything is off it falls back to loading the page,
// which also re-learns the calls' current form.

var (
	errNoFastPath = errors.New("fast path not ready")
	errNotProduct = errors.New("the data call did not return product details")
)

// mvFromPageJS finds the site version segment in the page's own URLs.
const mvFromPageJS = `(() => {
	const urls = performance.getEntriesByType("resource").map(e => e.name)
		.concat([...document.scripts].map(s => s.src));
	const m = urls.join(" ").match(/\/(mv\d+)\//);
	return m ? m[1] : "";
})()`

// directJS asks both data calls and returns their status and text.
// %s: the two URLs (JSON strings).
const directJS = `(async (infoURL, contentURL) => {
	const get = async (u) => {
		try {
			const r = await fetch(u, { credentials: "include" });
			return { status: r.status, text: await r.text() };
		} catch (e) { return { status: 0, text: String(e) }; }
	};
	const [info, content] = await Promise.all([get(infoURL), get(contentURL)]);
	return { info, content };
})(%s, %s)`

const pnPlaceholder = "{PN}"

func jsString(s string) string { b, _ := json.Marshal(s); return string(b) }

// keepAPITab makes tab the fast-path tab (closing a previous one).
func (s *Scraper) keepAPITab(tab context.Context, cancel func(), mv string) {
	if s.apiCancel != nil && s.apiTab != tab {
		s.apiCancel()
	}
	s.apiTab, s.apiCancel, s.mv = tab, cancel, mv
}

func (s *Scraper) closeAPITab() {
	if s.apiCancel != nil {
		s.apiCancel()
	}
	s.apiTab, s.apiCancel, s.mv = nil, nil, ""
	s.infoTmpl, s.contentTmpl = "", ""
}

// rememberCallForms stores the exact URLs the page used, with the part number
// replaced by a placeholder, so the fast path sends the same parameters.
func (s *Scraper) rememberCallForms(pn, infoURL, contentURL string) {
	tmpl := func(u string) string {
		if u == "" {
			return ""
		}
		return strings.ReplaceAll(strings.ReplaceAll(u, pn, pnPlaceholder), strings.ToLower(pn), pnPlaceholder)
	}
	s.infoTmpl, s.contentTmpl = tmpl(infoURL), tmpl(contentURL)
}

// callURLs gives the two data call URLs for pn.
func (s *Scraper) callURLs(pn string) (string, string) {
	if s.infoTmpl != "" && s.contentTmpl != "" {
		return strings.ReplaceAll(s.infoTmpl, pnPlaceholder, pn), strings.ReplaceAll(s.contentTmpl, pnPlaceholder, pn)
	}
	// Not seen yet (fresh warm-up): the form McMaster's page uses.
	events, _ := json.Marshal([]map[string]any{{
		"type": "ORDERINGMASTERPARTNUMBER", "selectionType": "SELECTION",
		"values": map[string]string{"entities": pn, "preselectedComponentPartNumber": ""},
	}})
	q := "?partNumber=" + pn + "&clientNavigationEvents=" + string(events)
	base := s.BaseURL + "/" + s.mv
	return base + orderInfoPath + q, base + contentPath + q
}

func (s *Scraper) absURL(p string) string {
	if strings.HasPrefix(p, "http://") || strings.HasPrefix(p, "https://") {
		return p
	}
	return s.BaseURL + "/" + strings.TrimPrefix(p, "/")
}

// direct is the fast path. errNoFastPath: no open tab yet; errNotProduct: the
// call answered but not with a product (e.g. an unknown part number).
func (s *Scraper) direct(pn string) (*Part, error) {
	if s.apiTab == nil || s.mv == "" {
		return nil, errNoFastPath
	}
	if s.apiTab.Err() != nil || s.browserCtx == nil || s.browserCtx.Err() != nil {
		s.closeAPITab()
		return nil, errNoFastPath
	}
	t0 := time.Now()
	ctx, cancel := context.WithTimeout(s.apiTab, 15*time.Second)
	defer cancel()
	infoURL, contentURL := s.callURLs(pn)
	var res struct {
		Info, Content struct {
			Status int
			Text   string
		}
	}
	err := chromedp.Run(ctx, chromedp.Evaluate(fmt.Sprintf(directJS, jsString(infoURL), jsString(contentURL)), &res,
		func(p *runtime.EvaluateParams) *runtime.EvaluateParams { return p.WithAwaitPromise(true) }))
	if err != nil {
		return nil, err
	}
	if res.Info.Status != 200 {
		return nil, fmt.Errorf("data call answered %d", res.Info.Status)
	}
	p, ok := parseOrderInfo(res.Info.Text, pn)
	if !ok {
		return nil, errNotProduct
	}
	p.ImageNote = "no-path"
	if res.Content.Status == 200 {
		if path := parseImagePath(res.Content.Text); path != "" {
			p.ImageURL = s.absURL(path)
			p.ImageFile, _ = s.saveImage(ctx, pn, p.ImageURL)
			p.ImageNote = imageNote(p)
		}
	}
	s.lastHow = "direct"
	log.Printf("lookup %s: %.2fs via the site's data call (no page load); %d price tiers; image=%v",
		pn, time.Since(t0).Seconds(), len(p.Tiers), p.ImageFile != "")
	return p, nil
}

// fillImageFromContent adds the image from the page's own ProductContent
// call, waiting briefly for it to arrive.
func (s *Scraper) fillImageFromContent(ctx context.Context, rec *netRecorder, p *Part) {
	p.ImageNote = "no-path"
	for end := time.Now().Add(2 * time.Second); time.Now().Before(end); time.Sleep(150 * time.Millisecond) {
		if body, _, ok := rec.find(contentPath); ok {
			if path := parseImagePath(body); path != "" {
				p.ImageURL = s.absURL(path)
				p.ImageFile, _ = s.saveImage(ctx, p.PartNumber, p.ImageURL)
				p.ImageNote = imageNote(p)
			}
			return
		}
	}
}

// imageNote says why a part with an image address has no stored copy.
// The browser can still show it straight from McMaster.
func imageNote(p *Part) string {
	if p.ImageFile == "" {
		return "download-failed"
	}
	return ""
}
