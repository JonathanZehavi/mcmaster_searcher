package main

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/xuri/excelize/v2"
)

var png1x1, _ = base64.StdEncoding.DecodeString(
	"iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNk+M9QDwADhgGAWjR9awAAAABJRU5ErkJggg==")

// ---------- fake McMaster + real browser ----------

func fakeMcMaster(t *testing.T) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/img/"):
			w.Header().Set("Content-Type", "image/png")
			w.Write(png1x1)
		case strings.HasPrefix(r.URL.Path, "/91251A540"):
			http.ServeFile(w, r, "testdata/product.html")
		case strings.HasPrefix(r.URL.Path, "/8336N108"):
			http.ServeFile(w, r, "testdata/tiered.html")
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
	b, err := os.ReadFile(filepath.Join(s.ImagesDir, p.ImageFile))
	if err != nil || !bytes.Equal(b, png1x1) {
		t.Fatalf("image not saved: %q %v", p.ImageFile, err)
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
	override := `() => ({partNumber: "91251A540", names: ["From override"], unit: "Each", tiers: [], image: "", blocked: false, notFound: false})`
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
