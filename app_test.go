package main

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"image"
	"image/png"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chromedp/cdproto/network"
	"github.com/xuri/excelize/v2"
)

var png1x1, _ = base64.StdEncoding.DecodeString(
	"iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNk+M9QDwADhgGAWjR9awAAAABJRU5ErkJggg==")

// ---------- fake McMaster + real browser ----------

// redPNG is a visible 40x40 image, so a screenshot of it is not empty.
func redPNG() []byte {
	img := image.NewRGBA(image.Rect(0, 0, 40, 40))
	for i := range img.Pix {
		img.Pix[i] = []byte{220, 30, 30, 255}[i%4]
	}
	var buf bytes.Buffer
	png.Encode(&buf, img)
	return buf.Bytes()
}

// fakeCatalog answers the fake site's ProductOrderInfo call, in the shape the
// real site uses.
var fakeCatalog = map[string]string{
	"7129K21": `{"partNumber":"7129K21","unitOfMeasure":"Pack of 10","parentDescription":"Brass Hex Nut","suffixDescription":"1/4\"-20 Thread Size","pricingData":{"price":"$5.12 per pack of 10","priceLevels":[{"QuantityRangeMessage":"1-9 packs","PriceMessage":"$5.12","SellPrice":5.12},{"QuantityRangeMessage":"10 or more","PriceMessage":"$4.60","SellPrice":4.6}],"shouldShowPrice":true}}`,
	"5555W1":  `{"partNumber":"5555W1","unitOfMeasure":"Each","parentDescription":"Steel Washer","suffixDescription":"M6","pricingData":{"priceLevels":[{"QuantityRangeMessage":"","PriceMessage":"$0.40","SellPrice":0.4}],"shouldShowPrice":true}}`,
	"5555W2":  `{"partNumber":"5555W2","unitOfMeasure":"Each","parentDescription":"Steel Washer","suffixDescription":"M8","pricingData":{"priceLevels":[{"QuantityRangeMessage":"","PriceMessage":"$0.55","SellPrice":0.55}],"shouldShowPrice":true}}`,
	"7129K22": `{"partNumber":"7129K22","unitOfMeasure":"Each","parentDescription":"Brass Hex Nut","suffixDescription":"5/16\"-18 Thread Size","pricingData":{"price":"$0.88 each","priceLevels":[{"QuantityRangeMessage":"","PriceMessage":"$0.88","SellPrice":0.88}],"shouldShowPrice":true}}`,
}

const wallHTML = `<!doctype html><html><head><title>McMaster-Carr</title></head><body>
<h1>To continue browsing, please log in.</h1><a href="/login">Log in</a></body></html>`

// seenOther: this session already viewed a product other than pn.
func seenOther(r *http.Request, pn string) bool {
	c, err := r.Cookie("seen")
	return err == nil && c.Value != pn
}

// The 18 hidden part numbers behind 7856K41's choices (from the real answer):
// 3 lengths x 6 colors. Half answer with the option ids they stand for, half
// only say it in their description, to cover both ways of matching.
var (
	heatShrinkComps   = strings.Fields("7856K188 7856K189 7856K199 7856K202 7856K203 7856K311 7856K511 7856K512 7856K514 7856K515 7856K516 7856K517 7856K934 7856K936 7856K937 7856K939 7856K947 7856K94")
	heatShrinkLengths = [][2]string{{"25", "10000001612988"}, {"100", "10000001613252"}, {"1,000", "10000001604658"}}
	heatShrinkColors  = [][2]string{{"Black", "10000001549922"}, {"Blue", "10000001527696"}, {"Green", "10000001523318"},
		{"Red", "10000001522000"}, {"White", "10000001536223"}, {"Yellow", "10000001536684"}}
)

func heatShrinkPrice(li, ci int) float64 { return []float64{10, 30, 250}[li] + float64(ci) }

func fakeVariant(pn string) string {
	i := slices.Index(heatShrinkComps, pn)
	if i < 0 {
		return ""
	}
	li, ci := i/6, i%6
	l, c := heatShrinkLengths[li], heatShrinkColors[ci]
	ids := "[]"
	if i%2 == 0 {
		ids = fmt.Sprintf(`["%s","%s"]`, l[1], c[1])
	}
	return fmt.Sprintf(`{"partNumber":"%s","unitOfMeasure":"Each","parentDescription":"Heat-Shrink Tubing","suffixDescription":"0.06\" ID Before Shrinking, %s ft. Long, %s","easyToOrderModel":{"implicitlySelectedAttrValueIds":%s,"componentSpecSpecChoices":[],"terminalComponentPartNumbersInScope":[]},"pricingData":{"price":"$%.2f each","priceLevels":[{"QuantityRangeMessage":"","PriceMessage":"$%.2f","SellPrice":%.2f}],"shouldShowPrice":true}}`,
		pn, l[0], c[0], ids, heatShrinkPrice(li, ci), heatShrinkPrice(li, ci), heatShrinkPrice(li, ci))
}

// pageHits counts product page loads per part number (to prove the fast path skips them).
var pageHits sync.Map

func fakeMcMaster(t *testing.T) *httptest.Server {
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/":
			http.ServeFile(w, r, "testdata/home.html")
		case r.URL.Path == "/mv123/app.js":
			w.Header().Set("Content-Type", "text/javascript")
			w.Write([]byte("// site bundle"))
		case r.URL.Path == "/mv123"+orderInfoPath && strings.HasPrefix(r.URL.Query().Get("partNumber"), "5555W") && seenOther(r, r.URL.Query().Get("partNumber")):
			// The session already used up its views: McMaster's hard wall.
			w.Header().Set("Content-Type", "text/html")
			w.Write([]byte(wallHTML))
		case r.URL.Path == "/mv123"+orderInfoPath && r.URL.Query().Get("partNumber") == "7856K41":
			// Recorded from the real site; without the page's feature flags
			// the choices are left out (as the real site seems to do).
			b, _ := os.ReadFile("testdata/7856K41-orderinfo.json")
			if !strings.Contains(r.URL.Query().Get("features"), "enablemnitpns") {
				var m map[string]any
				json.Unmarshal(b, &m)
				delete(m, "easyToOrderModel")
				b, _ = json.Marshal(m)
			}
			w.Write(b)
		case r.URL.Path == "/mv123"+orderInfoPath && fakeVariant(r.URL.Query().Get("partNumber")) != "":
			w.Write([]byte(fakeVariant(r.URL.Query().Get("partNumber"))))
		case r.URL.Path == "/mv123"+orderInfoPath:
			body, ok := fakeCatalog[r.URL.Query().Get("partNumber")]
			if !ok {
				body = `{"partNumber":"","unitOfMeasure":""}`
			}
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(body))
		case r.URL.Path == "/mv123"+contentPath:
			pn := strings.ToLower(r.URL.Query().Get("partNumber"))
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"displayImage":{"sourcePath":"/mvD/Contents/gfx/ImageCache/712/%s-@100p.png?ver=1","height":100,"width":100}}`, pn)
		case strings.HasPrefix(r.URL.Path, "/mvD/Contents/gfx/ImageCache/"):
			w.Header().Set("Content-Type", "image/png")
			w.Write(redPNG())
		case strings.HasPrefix(r.URL.Path, "/7129K2"):
			pn := strings.Trim(r.URL.Path, "/")
			n, _ := pageHits.LoadOrStore(pn, new(atomic.Int32))
			n.(*atomic.Int32).Add(1)
			http.ServeFile(w, r, "testdata/login-wall.html")
		case strings.HasPrefix(r.URL.Path, "/5555W"):
			// One product per anonymous session, then the hard wall.
			pn := strings.Trim(r.URL.Path, "/")
			if seenOther(r, pn) {
				w.Write([]byte(wallHTML))
				return
			}
			http.SetCookie(w, &http.Cookie{Name: "seen", Value: pn, Path: "/"})
			http.ServeFile(w, r, "testdata/login-wall.html")
		case strings.HasPrefix(r.URL.Path, "/6666W"):
			w.Write([]byte(wallHTML)) // walled whatever the session (e.g. by address)
		case strings.HasPrefix(r.URL.Path, "/img/protected"):
			// Like a site that refuses downloads: only an <img> on the page gets it.
			if r.Header.Get("Sec-Fetch-Dest") != "image" {
				http.Error(w, "forbidden", 403)
				return
			}
			w.Header().Set("Content-Type", "image/png")
			w.Write(redPNG())
		case strings.HasPrefix(r.URL.Path, "/img/"):
			w.Header().Set("Content-Type", "image/png")
			w.Write(png1x1)
		case r.URL.Path == "/data/product":
			body, _ := io.ReadAll(r.Body)
			if !strings.Contains(string(body), "6040K12") {
				http.Error(w, "unknown part", 404)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"partNumber":"6040K12","name":"Recorded Spacer","price":"4.20","unit":"Each"}`))
		case strings.HasPrefix(r.URL.Path, "/6040K12"):
			http.ServeFile(w, r, "testdata/xhr.html")
		case strings.HasPrefix(r.URL.Path, "/cdn/app.js"):
			w.Header().Set("Content-Type", "text/javascript")
			w.Write([]byte(`document.getElementById("app").innerHTML = "<h1>CDN Rendered Washer</h1><div>$1.25 Each</div>";`))
		case strings.HasPrefix(r.URL.Path, "/91251A540"):
			http.ServeFile(w, r, "testdata/product.html")
		case strings.HasPrefix(r.URL.Path, "/8336N108"):
			http.ServeFile(w, r, "testdata/tiered.html")
		case strings.HasPrefix(r.URL.Path, "/94895A031"):
			http.ServeFile(w, r, "testdata/slow.html")
		case strings.HasPrefix(r.URL.Path, "/90107A010"):
			b, _ := os.ReadFile("testdata/needs-cdn.html")
			port := srv.URL[strings.LastIndex(srv.URL, ":")+1:]
			w.Header().Set("Content-Type", "text/html")
			w.Write([]byte(strings.ReplaceAll(string(b), "__PORT__", port)))
		default:
			http.ServeFile(w, r, "testdata/blocked.html")
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// browserTempDir is like t.TempDir, but tolerates Chrome's helper processes
// still writing to the profile for a moment after the browser closes.
func browserTempDir(t *testing.T) string {
	dir, err := os.MkdirTemp("", "mcm-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		for i := 0; i < 20; i++ {
			if os.RemoveAll(dir) == nil {
				return
			}
			time.Sleep(250 * time.Millisecond)
		}
	})
	return dir
}

func newTestScraper(t *testing.T, base string) *Scraper {
	t.Setenv("MCM_BASE_URL", base)
	t.Setenv("MCM_MIN_INTERVAL", "0")
	t.Setenv("MCM_TIMEOUT", "8")
	if _, err := os.Stat("/opt/pw-browsers/chromium"); err == nil && os.Getenv("MCM_BROWSER_PATH") == "" {
		t.Setenv("MCM_BROWSER_PATH", "/opt/pw-browsers/chromium")
	}
	s := NewScraper(browserTempDir(t))
	t.Cleanup(s.Close)
	return s
}

func TestScraperExtractsProduct(t *testing.T) {
	s := newTestScraper(t, fakeMcMaster(t).URL)
	p, err := s.Lookup("91251A540")
	if err != nil {
		t.Fatal(err)
	}
	if p.Name != "Black-Oxide Alloy Steel Socket Head Screw" || p.Unit != "Pack of 100" {
		t.Fatalf("bad extract: %+v", p)
	}
	if len(p.Tiers) != 1 || p.Tiers[0] != (PriceTier{Min: 1, Max: 0, Price: 12.34}) {
		t.Fatalf("single price should become one tier: %+v", p.Tiers)
	}
	if !contains(p.NameOptions, "M3 x 0.5 mm Thread, 8 mm Long") {
		t.Fatalf("missing spec line in options: %v", p.NameOptions)
	}
}

func TestScraperExtractsQuantityTiers(t *testing.T) {
	s := newTestScraper(t, fakeMcMaster(t).URL)
	p, err := s.Lookup("8336N108")
	if err != nil {
		t.Fatal(err)
	}
	want := []PriceTier{{Min: 1, Max: 11, Price: 28.46}, {Min: 12, Max: 0, Price: 25.93}}
	if fmt.Sprint(p.Tiers) != fmt.Sprint(want) {
		t.Fatalf("tiers = %+v, want %+v (the 8-32 thread size must not count)", p.Tiers, want)
	}
	if p.Unit != "Pair" {
		t.Fatalf("unit from the tier rows should win over a stray 'Each': %q", p.Unit)
	}
}

func TestNetworkDietRules(t *testing.T) {
	s := &Scraper{siteHost: "mcmaster.com"}
	st := &tabStats{thirdParty: map[string]bool{}, allow: map[string]bool{}}
	for _, c := range []struct {
		url   string
		rt    network.ResourceType
		block bool
	}{
		{"https://www.mcmaster.com/static/app.js", network.ResourceTypeScript, false},
		{"https://www.mcmaster.com/7129K21/", network.ResourceTypeDocument, false},
		{"https://images.mcmaster.com/x.png", network.ResourceTypeImage, true}, // no product picture: skip images
		{"https://www.mcmaster.com/font.woff2", network.ResourceTypeFont, true},
		{"https://chat.widget.io/loader.js", network.ResourceTypeScript, true},
		{"https://stats.example/collect", network.ResourceTypeXHR, true},
		{"https://evilmcmaster.com/x.js", network.ResourceTypeScript, true}, // not a subdomain
	} {
		if got := s.shouldBlock(c.url, c.rt, st); got != c.block {
			t.Errorf("%s (%s): block=%v, want %v", c.url, c.rt, got, c.block)
		}
	}
	st.allowURL("https://cdn.other.com/fetch-me.png")
	if s.shouldBlock("https://cdn.other.com/fetch-me.png", network.ResourceTypeFetch, st) {
		t.Error("the product image URL must be fetchable even from another host")
	}
}

// A page that only renders with a script from another host: the diet breaks
// it, so the lookup retries without the diet, succeeds, and keeps it off.
func TestNetworkDietFallsBack(t *testing.T) {
	s := newTestScraper(t, fakeMcMaster(t).URL)
	s.Timeout = 4 * time.Second
	p, err := s.Lookup("90107A010")
	if err != nil || p.Name != "CDN Rendered Washer" {
		t.Fatalf("got %+v, %v", p, err)
	}
	if !s.dietOff {
		t.Fatal("diet should be off after it broke a page")
	}
}

func hits(pn string) int32 {
	if n, ok := pageHits.Load(pn); ok {
		return n.(*atomic.Int32).Load()
	}
	return 0
}

// The real site shows "Log in to view Product Detail" but its data calls still
// answer: the lookup takes the details from them, keeps the tab, and the next
// lookup asks the calls directly without loading a page.
func TestLoginWallUsesDataCallsThenFastPath(t *testing.T) {
	s := newTestScraper(t, fakeMcMaster(t).URL)
	p, err := s.Lookup("7129K21")
	if err != nil {
		t.Fatal(err)
	}
	want := []PriceTier{{Min: 1, Max: 9, Price: 5.12}, {Min: 10, Max: 0, Price: 4.6}}
	if p.Name != `Brass Hex Nut, 1/4"-20 Thread Size` || p.Unit != "Pack of 10" || fmt.Sprint(p.Tiers) != fmt.Sprint(want) {
		t.Fatalf("got %+v", p)
	}
	if s.lastHow != "page" || s.mv != "mv123" || !strings.Contains(s.infoTmpl, "features=enablemnitpns") {
		t.Fatalf("fast path not prepared: how=%s mv=%q tmpl=%q", s.lastHow, s.mv, s.infoTmpl)
	}

	p, err = s.Lookup("7129K22")
	if err != nil || s.lastHow != "direct" || hits("7129K22") != 0 {
		t.Fatalf("second lookup should skip the page: how=%s hits=%d err=%v", s.lastHow, hits("7129K22"), err)
	}
	if p.Unit != "Each" || len(p.Tiers) != 1 || p.Tiers[0] != (PriceTier{Min: 1, Max: 0, Price: 0.88}) {
		t.Fatalf("direct result: %+v", p)
	}
	if rec, _ := filepath.Glob(filepath.Join(s.DebugDir, "7129K22-*-network.json")); len(rec) != 1 {
		t.Fatalf("the fast path must save both answers for the debug files: %v", rec)
	} else if b, _ := os.ReadFile(rec[0]); !strings.Contains(string(b), "displayImage") || !strings.Contains(string(b), "Brass Hex Nut") {
		t.Fatalf("recording lacks an answer: %s", b)
	}

	// An unknown part number falls back to the page (and fails) but keeps the fast path.
	if _, err := s.Lookup("1111A11"); err == nil {
		t.Fatal("unknown part should fail")
	}
	if s.apiTab == nil {
		t.Fatal("an unknown part number must not close the fast path")
	}
}

// After a few anonymous views McMaster shows "To continue browsing, please
// log in" everywhere. That text must never become a product name: the lookup
// forgets the session (cookies), comes back as a new visitor and succeeds.
func TestHardLoginWallStartsFreshSession(t *testing.T) {
	s := newTestScraper(t, fakeMcMaster(t).URL)
	if p, err := s.Lookup("5555W1"); err != nil || p.Name != "Steel Washer, M6" {
		t.Fatalf("first lookup: %+v %v", p, err)
	}
	p, err := s.Lookup("5555W2") // the fast path hits the wall; a fresh session recovers
	if err != nil || p.Name != "Steel Washer, M8" || p.Tiers[0].Price != 0.55 {
		t.Fatalf("after the wall: %+v %v", p, err)
	}
}

// A wall that stays (not tied to the session) fails fast with a clear message.
func TestPersistentLoginWallFailsClearly(t *testing.T) {
	s := newTestScraper(t, fakeMcMaster(t).URL)
	t0 := time.Now()
	p, err := s.Lookup("6666W1")
	if err == nil || !strings.Contains(err.Error(), "להתחבר") {
		t.Fatalf("want a login-wall error, got %+v %v", p, err)
	}
	if d := time.Since(t0); d > 6*time.Second {
		t.Fatalf("took %v: the wall should be recognized at once", d)
	}
}

// The real answer for 7856K41: two choices (length, color), no price yet,
// and 18 hidden part numbers.
func TestParseChoices(t *testing.T) {
	body, _ := os.ReadFile("testdata/7856K41-orderinfo.json")
	p, ok := parseOrderInfo(string(body), "7856K41")
	if !ok || p.Name != `Heat-Shrink Tubing, 0.06" ID Before Shrinking` || len(p.Tiers) != 0 {
		t.Fatalf("got %+v ok=%v", p, ok)
	}
	if len(p.Choices) != 2 || p.Choices[0].Name != "Length" || p.Choices[0].Label != "Choose a Length, ft." ||
		len(p.Choices[0].Options) != 3 || p.Choices[0].Options[2].Text != "1,000" ||
		p.Choices[1].Name != "Color" || len(p.Choices[1].Options) != 6 || p.Choices[1].Options[5].Text != "Yellow" {
		t.Fatalf("choices: %+v", p.Choices)
	}
	if len(p.components) != 18 || p.components[17] != "7856K94" {
		t.Fatalf("hidden part numbers: %v", p.components)
	}
	// A plain product has no choices.
	plain, _ := parseOrderInfo(fakeCatalog["7129K22"], "7129K22")
	if len(plain.Choices) != 0 || len(plain.components) != 0 {
		t.Fatalf("plain product got choices: %+v", plain)
	}
}

func TestContainsWord(t *testing.T) {
	for _, c := range []struct {
		s, w string
		want bool
	}{
		{"100 ft. long, black", "100", true},
		{"1000 ft. long, black", "100", false},
		{"0.100 in. thick", "100", false},
		{"colored", "red", false},
		{"25 ft. long, red", "red", true},
		{"0.25 in.", "25", false},
	} {
		if got := containsWord(c.s, c.w); got != c.want {
			t.Errorf("containsWord(%q, %q) = %v", c.s, c.w, got)
		}
	}
}

// A product with choices: every combination gets its hidden part number and
// price, through the fast path, in one lookup.
func TestChoicesArePricedPerCombination(t *testing.T) {
	s := newTestScraper(t, fakeMcMaster(t).URL)
	s.Warm()
	p, err := s.Lookup("7856K41")
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Choices) != 2 || len(p.Variants) != 18 {
		t.Fatalf("want 2 choices and 18 priced combinations, got %d / %d", len(p.Choices), len(p.Variants))
	}
	for _, v := range p.Variants {
		li := slices.IndexFunc(heatShrinkLengths, func(x [2]string) bool { return x[0] == v.Values["Length"] })
		ci := slices.IndexFunc(heatShrinkColors, func(x [2]string) bool { return x[0] == v.Values["Color"] })
		if li < 0 || ci < 0 || heatShrinkComps[li*6+ci] != v.PartNumber || v.Tiers[0].Price != heatShrinkPrice(li, ci) {
			t.Fatalf("wrong combination: %+v", v)
		}
	}
	if rec, _ := filepath.Glob(filepath.Join(s.DebugDir, "7856K41-*-network.json")); len(rec) < 2 {
		t.Fatalf("the combinations' answers should be saved for the debug files: %v", rec)
	}
}

func TestWarmUpEnablesFastPath(t *testing.T) {
	s := newTestScraper(t, fakeMcMaster(t).URL)
	s.Warm()
	if s.mv != "mv123" {
		t.Fatalf("warm-up did not find the site version: %q", s.mv)
	}
	before := hits("7129K21")
	p, err := s.Lookup("7129K21")
	if err != nil || s.lastHow != "direct" || hits("7129K21") != before {
		t.Fatalf("first lookup after warm-up should be direct: how=%s err=%v", s.lastHow, err)
	}
	if p.Name != `Brass Hex Nut, 1/4"-20 Thread Size` {
		t.Fatalf("got %+v", p)
	}
}

func TestParseRealOrderInfo(t *testing.T) {
	// The answer the real site gave for 8336N108 (recorded 2026-10-11).
	body := `{"partNumber":"8336N108","unitOfMeasure":"Pair","parentDescription":"Welding Mitts","suffixDescription":"Cowhide Leather, Size L","deliveryMessage":"Delivers Tuesday","pricingData":{"price":"$28.46 per pair","priceLevelUnitOfMeasureColumnHeader":"Per Pair","priceLevels":[{"QuantityRangeMessage":"1-11 pairs","PriceMessage":"$28.46","SellPrice":28.46},{"QuantityRangeMessage":"12 or more","PriceMessage":"$25.93","SellPrice":25.93}],"shouldShowPrice":true,"shouldShowComponentPrice":false,"isPRUP":false,"perPiecePrice":null},"showProductDetailLinkInd":true}`
	p, ok := parseOrderInfo(body, "8336N108")
	want := []PriceTier{{Min: 1, Max: 11, Price: 28.46}, {Min: 12, Max: 0, Price: 25.93}}
	if !ok || p.Name != "Welding Mitts, Cowhide Leather, Size L" || p.Unit != "Pair" || fmt.Sprint(p.Tiers) != fmt.Sprint(want) {
		t.Fatalf("got %+v ok=%v", p, ok)
	}
	if _, ok := parseOrderInfo(`{"partNumber":"","unitOfMeasure":""}`, "1111A11"); ok {
		t.Fatal("empty answer must not count as a product")
	}
	img := parseImagePath(`{"displayImage":{"id":1,"sourcePath":"/mvD/Contents/gfx/ImageCache/833/8336n108-@100p_oh4yb_1755197483.png?ver=ImageNotFound","height":100,"width":100}}`)
	if !strings.HasPrefix(img, "/mvD/Contents/gfx/ImageCache/833/8336n108") {
		t.Fatalf("image path: %q", img)
	}
}

// The recorder keeps the page's data request (method, body, answer), which is
// what a browser-free lookup would call directly.
func TestRecorderCapturesDataRequests(t *testing.T) {
	s := newTestScraper(t, fakeMcMaster(t).URL)
	p, err := s.Lookup("6040K12")
	if err != nil || p.Name != "Recorded Spacer" {
		t.Fatalf("lookup: %+v %v", p, err)
	}
	files, _ := filepath.Glob(filepath.Join(s.DebugDir, "6040K12-*-network.json"))
	if len(files) != 1 {
		t.Fatalf("want one recording, got %v", files)
	}
	b, _ := os.ReadFile(files[0])
	var rec struct {
		Outcome  string
		Requests []netEntry
	}
	if err := json.Unmarshal(b, &rec); err != nil {
		t.Fatal(err)
	}
	var found *netEntry
	for i, e := range rec.Requests {
		if strings.HasSuffix(e.URL, "/data/product") {
			found = &rec.Requests[i]
		}
	}
	if rec.Outcome != "ok" || found == nil {
		t.Fatalf("data request not recorded: %s", b)
	}
	if found.Method != "POST" || !strings.Contains(found.PostData, "6040K12") || !found.HasPart ||
		!strings.Contains(found.Body, "Recorded Spacer") || found.Status != 200 {
		t.Fatalf("incomplete entry: %+v", found)
	}
}

// The site's marketing text is in the page metadata from the first moment;
// it must neither become the name nor end the wait before the product renders.
func TestScraperIgnoresSiteDescription(t *testing.T) {
	s := newTestScraper(t, fakeMcMaster(t).URL)
	p, err := s.Lookup("94895A031")
	if err != nil {
		t.Fatal(err)
	}
	if p.Name != "Slow Loading Hex Nut" || p.Unit != "Pack of 50" {
		t.Fatalf("got %+v", p)
	}
	for _, n := range p.NameOptions {
		if strings.Contains(n, "complete source") {
			t.Fatalf("site description offered as a name: %v", p.NameOptions)
		}
	}
}

func TestScraperReportsBlockAndSavesDebug(t *testing.T) {
	s := newTestScraper(t, fakeMcMaster(t).URL)
	_, err := s.Lookup("1234K56")
	if err == nil || !strings.Contains(err.Error(), "חסם") {
		t.Fatalf("want block error, got %v", err)
	}
	if m, _ := filepath.Glob(filepath.Join(s.DebugDir, "1234K56-*.html")); len(m) == 0 {
		t.Fatal("debug html not saved")
	}
}

func TestExternalExtractorOverrides(t *testing.T) {
	s := newTestScraper(t, fakeMcMaster(t).URL)
	override := `() => ({partNumber: "91251A540", names: ["From override"], headings: 1, unit: "Each", tiers: [], image: "", blocked: false, notFound: false})`
	os.WriteFile(s.Extractor.OverridePath, []byte(override), 0o644)
	p, err := s.Lookup("91251A540")
	if err != nil || p.Name != "From override" {
		t.Fatalf("override not used: %+v %v", p, err)
	}
	os.WriteFile(s.Extractor.OverridePath, []byte("() => { syntax error"), 0o644)
	if _, err := s.Lookup("91251A540"); err == nil || !strings.Contains(err.Error(), "extractor.js") {
		t.Fatalf("broken override should be named in the error, got %v", err)
	}
}

// ---------- pricing ----------

func TestUnitPriceFor(t *testing.T) {
	tiers := []PriceTier{{1, 11, 28.46}, {12, 0, 25.93}}
	for qty, want := range map[int]float64{1: 28.46, 11: 28.46, 12: 25.93, 500: 25.93} {
		if got := unitPriceFor(tiers, qty); got != want {
			t.Errorf("qty %d: got %v want %v", qty, got, want)
		}
	}
	if unitPriceFor(nil, 3) != 0 {
		t.Error("no tiers means no price")
	}
}

// ---------- API ----------

type stubScraper struct {
	calls int
	parts map[string]*Part
}

func (c *stubScraper) Lookup(pn string) (*Part, error) {
	c.calls++
	p, ok := c.parts[pn]
	if !ok {
		return nil, &LookupError{"blocked"}
	}
	cp := *p
	return &cp, nil
}

type client struct {
	t    *testing.T
	base string
	hc   *http.Client
}

func newClient(t *testing.T, base string) *client {
	jar, _ := cookiejar.New(nil)
	return &client{t, base, &http.Client{Jar: jar}}
}

func (c *client) do(method, path string, body any) (int, map[string]any) {
	c.t.Helper()
	var b []byte
	if body != nil {
		b, _ = json.Marshal(body)
	}
	req, _ := http.NewRequest(method, c.base+path, bytes.NewReader(b))
	resp, err := c.hc.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func (c *client) mustOK(method, path string, body any) map[string]any {
	c.t.Helper()
	code, out := c.do(method, path, body)
	if code != 200 {
		c.t.Fatalf("%s %s: %d %v", method, path, code, out)
	}
	return out
}

func items(out map[string]any) []map[string]any {
	var res []map[string]any
	for _, x := range out["items"].([]any) {
		res = append(res, x.(map[string]any))
	}
	return res
}

// world: a server where Rachel_Levi is the manager (in manager mode) and
// Dana_Cohen and Yossi_Mizrahi are regular users; two projects.
func world(t *testing.T, sc Lookuper) (srvURL string, admin, dana, yossi *client) {
	store, err := OpenStore(filepath.Join(t.TempDir(), "orders.json"))
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(NewServer(store, sc, t.TempDir(), Extractor{}))
	t.Cleanup(srv.Close)
	admin = newClient(t, srv.URL)
	admin.mustOK("POST", "/api/identify", map[string]any{"name": "Rachel_Levi"})
	admin.mustOK("POST", "/api/admin/enter", map[string]any{"password": testPW("rachel")}) // first manager
	admin.mustOK("PUT", "/api/projects", map[string]any{"projects": []string{"CWC", "Lab (Hanoch)"}})
	dana, yossi = newClient(t, srv.URL), newClient(t, srv.URL)
	dana.mustOK("POST", "/api/identify", map[string]any{"name": " dana cohen "}) // normalized to Dana_Cohen
	yossi.mustOK("POST", "/api/identify", map[string]any{"name": "Yossi_Mizrahi"})
	return srv.URL, admin, dana, yossi
}

func TestNormalizeName(t *testing.T) {
	for in, want := range map[string]string{"John_Doe": "John_Doe", " john doe ": "John_Doe", "JOHN__DOE": "JOHN_DOE", "dana": "Dana"} {
		if got := normalizeName(in); got != want {
			t.Errorf("normalizeName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestIdentityAndManagerMode(t *testing.T) {
	url, admin, dana, _ := world(t, &stubScraper{})
	anon := newClient(t, url)
	me := anon.mustOK("GET", "/api/me", nil)
	if me["user"] != nil || me["has_admin"] != true || len(me["names"].([]any)) != 3 {
		t.Fatalf("anonymous /api/me: %v", me)
	}
	if code, _ := anon.do("GET", "/api/items", nil); code != 401 {
		t.Fatal("items must need a name")
	}
	if code, _ := anon.do("POST", "/api/identify", map[string]any{"name": "<script>"}); code != 400 {
		t.Fatal("bad name accepted")
	}

	// the same name in another browser is the same user, and stays remembered
	again := newClient(t, url)
	u := again.mustOK("POST", "/api/identify", map[string]any{"name": "DANA_COHEN"})["user"].(map[string]any)
	if u["name"] != "Dana_Cohen" {
		t.Fatalf("name lookup should ignore case: %v", u)
	}
	if again.mustOK("GET", "/api/me", nil)["user"].(map[string]any)["name"] != "Dana_Cohen" {
		t.Fatal("browser forgot who it is")
	}

	// regular users get no manager screens, and cannot become manager
	for _, p := range []string{"/api/orders", "/api/users"} {
		if code, _ := dana.do("GET", p, nil); code != 403 {
			t.Fatalf("non-manager reached %s", p)
		}
	}
	if code, _ := dana.do("POST", "/api/admin/enter", map[string]any{"password": testPW("rachel")}); code != 401 {
		t.Fatal("non-manager entered manager mode with the manager's password")
	}
	// typing the manager's name is not enough without the password
	fake := newClient(t, url)
	fake.mustOK("POST", "/api/identify", map[string]any{"name": "Rachel_Levi"})
	if code, _ := fake.do("GET", "/api/orders", nil); code != 403 {
		t.Fatal("manager name without password reached history")
	}
	if code, _ := fake.do("POST", "/api/admin/enter", map[string]any{"password": "nope"}); code != 401 {
		t.Fatal("wrong manager password accepted")
	}
	// leaving manager mode
	admin.mustOK("POST", "/api/admin/leave", nil)
	if code, _ := admin.do("GET", "/api/orders", nil); code != 403 {
		t.Fatal("still in manager mode after leaving")
	}
	admin.mustOK("POST", "/api/admin/enter", map[string]any{"password": testPW("rachel")})

	// promoting needs a password; disabling a user forgets their browsers
	users := admin.mustOK("GET", "/api/users", nil)["users"].([]any)
	ids := map[string]int{}
	for _, x := range users {
		ids[x.(map[string]any)["name"].(string)] = int(x.(map[string]any)["id"].(float64))
	}
	if code, _ := admin.do("PATCH", fmt.Sprintf("/api/users/%d", ids["Yossi_Mizrahi"]), map[string]any{"role": "admin"}); code != 400 {
		t.Fatal("manager without password created")
	}
	admin.mustOK("PATCH", fmt.Sprintf("/api/users/%d", ids["Dana_Cohen"]), map[string]any{"active": false})
	if code, _ := dana.do("GET", "/api/items", nil); code != 401 {
		t.Fatal("disabled user still recognized")
	}
	if code, _ := newClient(t, url).do("POST", "/api/identify", map[string]any{"name": "Dana_Cohen"}); code != 403 {
		t.Fatal("disabled user could pick their name again")
	}
	if code, _ := admin.do("PATCH", fmt.Sprintf("/api/users/%d", ids["Rachel_Levi"]), map[string]any{"role": "user"}); code != 400 {
		t.Fatal("last manager was demoted")
	}

	// password change
	admin.mustOK("POST", "/api/me/password", map[string]any{"old": testPW("rachel"), "new": testPW("rachel2")})
	other := newClient(t, url)
	other.mustOK("POST", "/api/identify", map[string]any{"name": "Rachel_Levi"})
	other.mustOK("POST", "/api/admin/enter", map[string]any{"password": testPW("rachel2")})
}

func TestOrderFlow(t *testing.T) {
	sc := &stubScraper{parts: map[string]*Part{
		"8336N108": {PartNumber: "8336N108", Name: "Shoulder Bolt", Unit: "Pair", Source: "auto",
			Tiers: []PriceTier{{1, 11, 28.46}, {12, 0, 25.93}}},
	}}
	_, admin, dana, yossi := world(t, sc)

	// lookup + add with tiered price: 12 pairs -> $25.93 each
	dana.mustOK("GET", "/api/lookup?pn=8336n108", nil)
	if code, _ := dana.do("POST", "/api/items", map[string]any{"part_number": "8336N108", "name": "Shoulder Bolt", "quantity": 12}); code != 400 {
		t.Fatal("project must be required")
	}
	added := dana.mustOK("POST", "/api/items", map[string]any{
		"part_number": "8336N108", "name": "Shoulder Bolt", "unit": "Pair", "quantity": "12",
		"project": "CWC", "purpose": "fixture",
		"tiers": []PriceTier{{1, 0, 1.00}}, // a tampered price from the browser is ignored for looked-up parts
	})["item"].(map[string]any)
	if added["unit_price"] != 25.93 || added["total"] != 311.16 || added["requester"] != "Dana_Cohen" {
		t.Fatalf("pricing/requester wrong: %v", added)
	}
	danaItem := int(added["id"].(float64))

	// lowering qty to 11 moves to the higher tier
	upd := dana.mustOK("PATCH", fmt.Sprintf("/api/items/%d", danaItem), map[string]any{"quantity": 11})["item"].(map[string]any)
	if upd["unit_price"] != 28.46 || upd["total"] != 313.06 {
		t.Fatalf("re-pricing on qty change: %v", upd)
	}

	// manual part: unit price typed by the user
	yossiItem := yossi.mustOK("POST", "/api/items", map[string]any{
		"part_number": "1234K56", "name": "Manual Washer", "unit": "Pack of 25", "quantity": 2,
		"unit_price": "$10.35", "project": "Lab (Hanoch)",
	})["item"].(map[string]any)
	if yossiItem["total"] != 20.7 {
		t.Fatalf("manual price: %v", yossiItem)
	}

	// each user sees only their own lines; admin sees all
	if n := len(items(dana.mustOK("GET", "/api/items", nil))); n != 1 {
		t.Fatalf("dana sees %d lines", n)
	}
	if n := len(items(dana.mustOK("GET", "/api/items?scope=all", nil))); n != 1 {
		t.Fatal("scope=all must be ignored for non-admins")
	}
	if n := len(items(admin.mustOK("GET", "/api/items?scope=all", nil))); n != 2 {
		t.Fatalf("admin sees %d lines", n)
	}
	// users cannot touch each other's lines
	if code, _ := yossi.do("PATCH", fmt.Sprintf("/api/items/%d", danaItem), map[string]any{"quantity": 99}); code != 403 {
		t.Fatal("yossi edited dana's line")
	}
	if code, _ := yossi.do("DELETE", fmt.Sprintf("/api/items/%d", danaItem), nil); code != 403 {
		t.Fatal("yossi deleted dana's line")
	}
	if code, _ := dana.do("POST", "/api/orders", map[string]any{"ids": []int{danaItem}}); code != 403 {
		t.Fatal("non-admin placed an order")
	}

	// export of the open list
	resp, err := admin.hc.Get(admin.base + "/api/export.xlsx")
	if err != nil || resp.StatusCode != 200 {
		t.Fatal("export failed")
	}
	f, err := excelize.OpenReader(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	rows, _ := f.GetRows("McMaster")
	if rows[0][2] != "McMaster Part Number" || rows[0][11] != "Total cost" || len(rows) != 3 {
		t.Fatalf("export layout: %v", rows)
	}

	// purchasing places the order
	order := admin.mustOK("POST", "/api/orders", map[string]any{
		"ids": []int{danaItem, int(yossiItem["id"].(float64))}, "po_number": "984826", "po_date": "2026-10-05",
	})["order"].(map[string]any)
	if order["items"].(float64) != 2 || order["total"] != 333.76 {
		t.Fatalf("order: %v", order)
	}
	if n := len(items(admin.mustOK("GET", "/api/items?scope=all", nil))); n != 0 {
		t.Fatal("open list not cleared")
	}

	// dana sees only her lines of the last order; history is admin-only
	last := dana.mustOK("GET", "/api/my-last-order", nil)
	if its := items(last); len(its) != 1 || its[0]["part_number"] != "8336N108" {
		t.Fatalf("my last order: %v", last)
	}
	if last["order"].(map[string]any)["po_number"] != "984826" {
		t.Fatal("po number missing")
	}
	if code, _ := dana.do("GET", "/api/orders/1", nil); code != 403 {
		t.Fatal("non-admin read history")
	}
	hist := admin.mustOK("GET", "/api/orders", nil)["orders"].([]any)
	if len(hist) != 1 || len(items(admin.mustOK("GET", "/api/orders/1", nil))) != 2 {
		t.Fatal("history wrong")
	}
	// ordered lines are frozen
	if code, _ := dana.do("PATCH", fmt.Sprintf("/api/items/%d", danaItem), map[string]any{"quantity": 3}); code != 404 {
		t.Fatal("ordered line was edited")
	}

	// a manual entry is remembered for the next lookup
	before := sc.calls
	if p := yossi.mustOK("GET", "/api/lookup?pn=1234K56", nil)["part"].(map[string]any); p["name"] != "Manual Washer" || sc.calls != before {
		t.Fatalf("manual part not cached: %v", p)
	}
}

func TestLookupValidationAndFailure(t *testing.T) {
	_, _, dana, _ := world(t, &stubScraper{})
	if code, _ := dana.do("GET", "/api/lookup?pn=hello", nil); code != 400 {
		t.Fatal("garbage part number accepted")
	}
	code, out := dana.do("GET", "/api/lookup?pn=1234K56", nil)
	if code != 502 || out["url"] != "https://www.mcmaster.com/1234K56/" {
		t.Fatalf("failure should give a manual link: %d %v", code, out)
	}
}

func TestStorePersists(t *testing.T) {
	path := filepath.Join(t.TempDir(), "orders.json")
	s, _ := OpenStore(path)
	s.AddUser(User{Username: "a", Name: "A", Role: "admin", Active: true})
	s.AddItem(Item{PartNumber: "1A1", Name: "x", Quantity: 2, Tiers: []PriceTier{{1, 0, 1.5}}})
	s2, err := OpenStore(path)
	if err != nil || len(s2.OpenItems(0)) != 1 || s2.OpenItems(0)[0].Total != 3 || s2.UserCount() != 1 {
		t.Fatal("data not persisted")
	}
	it, _ := s2.AddItem(Item{PartNumber: "1A2", Name: "y", Quantity: 1})
	if it.ID != 2 {
		t.Fatalf("ids must keep counting after reopen, got %d", it.ID)
	}
}

func TestPagesAndBookmarklet(t *testing.T) {
	url, _, dana, _ := world(t, &stubScraper{})
	for _, p := range []string{"/", "/add?pn=1", "/static/app.js", "/api/ping"} {
		resp, err := http.Get(url + p)
		if err != nil || resp.StatusCode != 200 {
			t.Fatalf("%s: %v %v", p, err, resp)
		}
		resp.Body.Close()
	}
	if code, _ := dana.do("GET", "/api/debug.zip", nil); code != 403 {
		t.Fatal("non-manager downloaded debug files")
	}
	href, _ := dana.mustOK("GET", "/api/bookmarklet", nil)["href"].(string)
	if !strings.HasPrefix(href, "javascript:") || !strings.Contains(href, "add%3F") || strings.Contains(href, "#") {
		t.Fatalf("bad bookmarklet: %.80s", href)
	}
}

// testPW makes a throwaway password per test user at run time, so no
// credential-looking literals live in the repository.
var testPWs = map[string]string{}

func testPW(user string) string {
	if pw, ok := testPWs[user]; ok {
		return pw
	}
	b := make([]byte, 8)
	rand.Read(b)
	testPWs[user] = hex.EncodeToString(b)
	return testPWs[user]
}

func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}

func TestOfficeAccessCode(t *testing.T) {
	t.Setenv("MCM_ACCESS_CODE", "office-"+testPW("code"))
	url, _, _, _ := func() (string, *client, *client, *client) {
		store, _ := OpenStore(filepath.Join(t.TempDir(), "orders.json"))
		srv := httptest.NewServer(NewServer(store, &stubScraper{}, t.TempDir(), Extractor{}))
		t.Cleanup(srv.Close)
		return srv.URL, nil, nil, nil
	}()
	c := newClient(t, url)
	me := c.mustOK("GET", "/api/me", nil)
	if me["needs_code"] != true || len(me["names"].([]any)) != 0 {
		t.Fatalf("code-protected /api/me leaks or misses the flag: %v", me)
	}
	if code, _ := c.do("POST", "/api/identify", map[string]any{"name": "John_Doe", "code": "wrong"}); code != 401 {
		t.Fatal("wrong office code accepted")
	}
	c.mustOK("POST", "/api/identify", map[string]any{"name": "John_Doe", "code": "office-" + testPW("code")})
	c.mustOK("GET", "/api/items", nil)
}

type slowStub struct{ calls atomic.Int32 }

func (s *slowStub) Lookup(pn string) (*Part, error) {
	s.calls.Add(1)
	time.Sleep(300 * time.Millisecond)
	return &Part{PartNumber: pn, Name: "Slow Part", Source: "auto"}, nil
}

// A paste and a click on "search" for the same part share one lookup.
func TestConcurrentLookupsShareOne(t *testing.T) {
	sc := &slowStub{}
	_, _, dana, _ := world(t, sc)
	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			dana.mustOK("GET", "/api/lookup?pn=5000N12", nil)
		}()
	}
	wg.Wait()
	if n := sc.calls.Load(); n != 1 {
		t.Fatalf("%d lookups for one part number", n)
	}
}

// ---------- storage and the fixed manager ----------

// With DATABASE_URL the data lives in Postgres and survives a restart.
// Runs only when MCM_TEST_DATABASE_URL points at a disposable database.
func TestPostgresStoreSurvivesRestart(t *testing.T) {
	dsn := os.Getenv("MCM_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("MCM_TEST_DATABASE_URL not set")
	}
	s, err := OpenPostgresStore(dsn)
	if err != nil {
		t.Fatal(err)
	}
	s.back.(pgBackend).db.Exec(`DELETE FROM app_state`)
	if s, err = OpenPostgresStore(dsn); err != nil {
		t.Fatal(err)
	}
	u, _ := s.FindOrCreateUser("Dana_Cohen")
	if _, err := s.AddItem(Item{PartNumber: "91251A540", Name: "Screw", Quantity: 3, UserID: u.ID, Requester: u.Name, Project: "CWC"}); err != nil {
		t.Fatal(err)
	}
	s2, err := OpenPostgresStore(dsn)
	if err != nil {
		t.Fatal(err)
	}
	if got := s2.OpenItems(0); len(got) != 1 || got[0].Requester != "Dana_Cohen" || s2.UserByName("dana_cohen") == nil {
		t.Fatalf("data lost on restart: %+v", got)
	}
}

// MCM_ADMIN_NAME/PASSWORD make a fixed manager on every start, so a data
// wipe can never hand manager mode to whoever enters a password first.
func TestManagerFromEnvironment(t *testing.T) {
	store, _ := OpenStore(filepath.Join(t.TempDir(), "orders.json"))
	t.Setenv("MCM_ADMIN_NAME", "jonathan zehavi")
	t.Setenv("MCM_ADMIN_PASSWORD", testPW("boss"))
	if err := ensureAdminFromEnv(store); err != nil {
		t.Fatal(err)
	}
	if err := ensureAdminFromEnv(store); err != nil { // every start: idempotent
		t.Fatal(err)
	}
	srv := httptest.NewServer(NewServer(store, &stubScraper{}, t.TempDir(), Extractor{}))
	t.Cleanup(srv.Close)

	other := newClient(t, srv.URL)
	other.mustOK("POST", "/api/identify", map[string]any{"name": "Someone_Else"})
	if code, _ := other.do("POST", "/api/admin/enter", map[string]any{"password": testPW("other")}); code != 401 {
		t.Fatalf("another user became manager: %d", code)
	}
	boss := newClient(t, srv.URL)
	boss.mustOK("POST", "/api/identify", map[string]any{"name": "Jonathan_Zehavi"})
	boss.mustOK("POST", "/api/admin/enter", map[string]any{"password": testPW("boss")})

	t.Setenv("MCM_ADMIN_PASSWORD", "x")
	if ensureAdminFromEnv(store) == nil {
		t.Fatal("a too-short password must be refused")
	}
}

// A line for a product with choices needs every choice made; it then gets
// the combination's price, unit and hidden part number, and the Excel file
// shows the choices in the description.
func TestAddItemWithChoices(t *testing.T) {
	sc := &stubScraper{parts: map[string]*Part{"7856K41": {
		PartNumber: "7856K41", Name: "Heat-Shrink Tubing", Unit: "Each", Source: "auto",
		Choices: []Choice{
			{Name: "Length", Label: "Choose a Length, ft.", Options: []ChoiceOption{{Text: "25"}, {Text: "100"}}},
			{Name: "Color", Label: "Choose a Color", Options: []ChoiceOption{{Text: "Black"}, {Text: "Red"}}},
		},
		Variants: []Variant{
			{PartNumber: "7856K188", Values: map[string]string{"Length": "25", "Color": "Black"}, Unit: "Each", Tiers: []PriceTier{{1, 0, 10}}},
			{PartNumber: "7856K311", Values: map[string]string{"Length": "100", "Color": "Red"}, Unit: "Each", Tiers: []PriceTier{{1, 0, 33.5}}},
		},
	}}}
	_, admin, dana, _ := world(t, sc)
	look := dana.mustOK("GET", "/api/lookup?pn=7856K41", nil)
	if part := look["part"].(map[string]any); len(part["choices"].([]any)) != 2 || len(part["variants"].([]any)) != 2 {
		t.Fatalf("lookup should send choices and combinations: %v", part)
	}
	base := map[string]any{"part_number": "7856K41", "name": "Heat-Shrink Tubing", "quantity": 2, "project": "CWC"}
	with := func(sel ...string) map[string]any {
		m := map[string]any{}
		for k, v := range base {
			m[k] = v
		}
		var list []map[string]string
		for i := 0; i+1 < len(sel); i += 2 {
			list = append(list, map[string]string{"name": sel[i], "value": sel[i+1]})
		}
		m["selections"] = list
		return m
	}
	if code, out := dana.do("POST", "/api/items", with("Length", "100")); code != 400 || !strings.Contains(out["error"].(string), "Color") {
		t.Fatalf("a missing choice must be refused: %d %v", code, out)
	}
	if code, _ := dana.do("POST", "/api/items", with("Length", "100", "Color", "Purple")); code != 400 {
		t.Fatal("an option the product does not have must be refused")
	}
	it := dana.mustOK("POST", "/api/items", with("Color", "Red", "Length", "100"))["item"].(map[string]any)
	if it["variant_pn"] != "7856K311" || it["unit_price"] != 33.5 || it["total"] != 67.0 {
		t.Fatalf("priced by the wrong combination: %v", it)
	}
	if sel := it["selections"].([]any); len(sel) != 2 || sel[0].(map[string]any)["name"] != "Length" {
		t.Fatalf("selections should follow the product's order: %v", sel)
	}
	// The cached part keeps its choices after the line is added.
	if look := dana.mustOK("GET", "/api/lookup?pn=7856K41", nil); len(look["part"].(map[string]any)["choices"].([]any)) != 2 {
		t.Fatal("adding a line must not drop the part's choices")
	}

	resp, err := admin.hc.Get(admin.base + "/api/export.xlsx")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	f, err := excelize.OpenReader(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	desc, _ := f.GetCellValue("McMaster", "D2")
	if desc != "Heat-Shrink Tubing — Length: 100 · Color: Red" {
		t.Fatalf("Excel description: %q", desc)
	}
}
