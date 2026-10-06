package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var png1x1, _ = base64.StdEncoding.DecodeString(
	"iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNk+M9QDwADhgGAWjR9awAAAABJRU5ErkJggg==")

// fakeMcMaster serves a client-rendered product page for 91251A540 and an
// "Access Denied" page for everything else.
func fakeMcMaster(t *testing.T) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/img/"):
			w.Header().Set("Content-Type", "image/png")
			w.Write(png1x1)
		case strings.HasPrefix(r.URL.Path, "/91251A540"):
			http.ServeFile(w, r, "testdata/product.html")
		default:
			http.ServeFile(w, r, "testdata/blocked.html")
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func newTestScraper(t *testing.T, base string) *Scraper {
	t.Setenv("MCM_BASE_URL", base)
	t.Setenv("MCM_MIN_INTERVAL", "0")
	t.Setenv("MCM_TIMEOUT", "8")
	if _, err := os.Stat("/opt/pw-browsers/chromium"); err == nil && os.Getenv("MCM_BROWSER_PATH") == "" {
		t.Setenv("MCM_BROWSER_PATH", "/opt/pw-browsers/chromium")
	}
	s := NewScraper(t.TempDir())
	if s.BrowserPath == "" && os.Getenv("CI") == "" {
		if _, err := os.Stat("/usr/bin/chromium"); err != nil {
			t.Skip("no browser available")
		}
	}
	return s
}

func TestScraperExtractsProduct(t *testing.T) {
	s := newTestScraper(t, fakeMcMaster(t).URL)
	p, err := s.Lookup("91251A540")
	if err != nil {
		t.Fatal(err)
	}
	if p.Name != "Black-Oxide Alloy Steel Socket Head Screw" || p.Unit != "Pack of 100" || p.Price != "12.34" {
		t.Fatalf("bad extract: %+v", p)
	}
	if !contains(p.NameOptions, "M3 x 0.5 mm Thread, 8 mm Long") {
		t.Fatalf("missing spec line in options: %v", p.NameOptions)
	}
	b, err := os.ReadFile(filepath.Join(s.ImagesDir, p.ImageFile))
	if err != nil || !bytes.Equal(b, png1x1) {
		t.Fatalf("image not saved: %q %v", p.ImageFile, err)
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

type countingScraper struct {
	calls int
	part  *Part
}

func (c *countingScraper) Lookup(pn string) (*Part, error) {
	c.calls++
	if c.part == nil {
		return nil, &LookupError{"blocked"}
	}
	p := *c.part
	return &p, nil
}

func newTestServer(t *testing.T, sc interface{ Lookup(string) (*Part, error) }) *httptest.Server {
	store, err := OpenStore(filepath.Join(t.TempDir(), "orders.json"))
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(NewServer(store, sc, t.TempDir(), Extractor{}))
	t.Cleanup(srv.Close)
	return srv
}

func call(t *testing.T, method, url string, body any) (int, map[string]any) {
	var rd *bytes.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	} else {
		rd = bytes.NewReader(nil)
	}
	req, _ := http.NewRequest(method, url, rd)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func TestLookupCachesAndValidates(t *testing.T) {
	sc := &countingScraper{part: &Part{PartNumber: "91251A540", Name: "Screw", Unit: "Each"}}
	srv := newTestServer(t, sc)
	if code, _ := call(t, "GET", srv.URL+"/api/lookup?pn=hello", nil); code != 400 {
		t.Fatalf("garbage pn: %d", code)
	}
	_, a := call(t, "GET", srv.URL+"/api/lookup?pn=91251a540", nil)
	_, b := call(t, "GET", srv.URL+"/api/lookup?pn=91251A540", nil)
	if sc.calls != 1 || a["part"].(map[string]any)["cached"] != false || b["part"].(map[string]any)["cached"] != true {
		t.Fatalf("cache broken: calls=%d a=%v b=%v", sc.calls, a, b)
	}
	call(t, "GET", srv.URL+"/api/lookup?pn=91251A540&refresh=1", nil)
	if sc.calls != 2 {
		t.Fatal("refresh should bypass cache")
	}
}

func TestLookupFailureGivesManualLink(t *testing.T) {
	srv := newTestServer(t, &countingScraper{})
	code, out := call(t, "GET", srv.URL+"/api/lookup?pn=1234K56", nil)
	if code != 502 || out["url"] != "https://www.mcmaster.com/1234K56/" {
		t.Fatalf("got %d %v", code, out)
	}
}

func TestListFlowAndExport(t *testing.T) {
	sc := &countingScraper{}
	srv := newTestServer(t, sc)
	item := map[string]any{"part_number": "91251A540", "name": "Screw", "unit": "Pack of 100", "quantity": "2", "requester": "דני"}
	if code, out := call(t, "POST", srv.URL+"/api/items", item); code != 200 {
		t.Fatalf("add: %d %v", code, out)
	}
	item["quantity"] = 0
	if code, _ := call(t, "POST", srv.URL+"/api/items", item); code != 400 {
		t.Fatal("quantity 0 must be rejected")
	}
	_, list := call(t, "GET", srv.URL+"/api/items", nil)
	items := list["items"].([]any)
	first := items[0].(map[string]any)
	if len(items) != 1 || first["quantity"].(float64) != 2 {
		t.Fatalf("list: %v", items)
	}
	id := int(first["id"].(float64))
	idURL := srv.URL + "/api/items/" + itoa(id)
	call(t, "PATCH", idURL, map[string]any{"quantity": 5})
	if code, _ := call(t, "PATCH", idURL, map[string]any{"quantity": "x"}); code != 400 {
		t.Fatal("bad quantity must be rejected")
	}
	_, list = call(t, "GET", srv.URL+"/api/items", nil)
	if list["items"].([]any)[0].(map[string]any)["quantity"].(float64) != 5 {
		t.Fatal("patch failed")
	}

	resp, err := http.Get(srv.URL + "/api/export.xlsx")
	if err != nil || resp.StatusCode != 200 {
		t.Fatal("export failed")
	}
	head := make([]byte, 2)
	resp.Body.Read(head)
	resp.Body.Close()
	if string(head) != "PK" {
		t.Fatal("export is not an xlsx")
	}

	// manual entry is remembered: next lookup needs no scraping
	_, lk := call(t, "GET", srv.URL+"/api/lookup?pn=91251A540", nil)
	if sc.calls != 0 || lk["part"].(map[string]any)["name"] != "Screw" {
		t.Fatalf("manual entry not cached: %v", lk)
	}

	_, mo := call(t, "POST", srv.URL+"/api/items/mark-ordered", map[string]any{"ids": []int{id}})
	if mo["count"].(float64) != 1 {
		t.Fatalf("mark ordered: %v", mo)
	}
	_, open := call(t, "GET", srv.URL+"/api/items", nil)
	_, hist := call(t, "GET", srv.URL+"/api/items?status=ordered", nil)
	if len(open["items"].([]any)) != 0 || len(hist["items"].([]any)) != 1 {
		t.Fatal("history move failed")
	}
}

func TestStorePersists(t *testing.T) {
	path := filepath.Join(t.TempDir(), "orders.json")
	s, _ := OpenStore(path)
	s.AddItem(Item{PartNumber: "1A1", Name: "x", Quantity: 1})
	s2, err := OpenStore(path)
	if err != nil || len(s2.ListItems("open")) != 1 {
		t.Fatal("data not persisted")
	}
	id, _ := s2.AddItem(Item{PartNumber: "1A2", Name: "y", Quantity: 1})
	if id != 2 {
		t.Fatalf("ids must keep counting after reopen, got %d", id)
	}
}

func TestIndexAndBookmarklet(t *testing.T) {
	srv := newTestServer(t, &countingScraper{})
	for _, p := range []string{"/", "/add?pn=1", "/static/app.js"} {
		resp, err := http.Get(srv.URL + p)
		if err != nil || resp.StatusCode != 200 {
			t.Fatalf("%s: %v %v", p, err, resp)
		}
		resp.Body.Close()
	}
	_, out := call(t, "GET", srv.URL+"/api/bookmarklet", nil)
	href, _ := out["href"].(string)
	if !strings.HasPrefix(href, "javascript:") || !strings.Contains(href, "add%3F") || strings.Contains(href, "#") {
		t.Fatalf("bad bookmarklet: %.80s", href)
	}
}

func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}

func itoa(i int) string { b, _ := json.Marshal(i); return string(b) }

func TestExternalExtractorOverrides(t *testing.T) {
	s := newTestScraper(t, fakeMcMaster(t).URL)
	if _, ext := s.Extractor.JS(); ext {
		t.Fatal("no override file yet")
	}
	override := `() => ({partNumber: "91251A540", names: ["From override"], unit: "Each", price: "", image: "", blocked: false, notFound: false})`
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
