// McMaster order list: a single-file local web app. Double-click the .exe,
// it opens the page in the browser; other office computers can browse to it.
package main

import (
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/xuri/excelize/v2"
)

//go:embed static
var staticFS embed.FS

type Server struct {
	store   *Store
	scraper interface{ Lookup(string) (*Part, error) }
	images  string
	ext     Extractor
	mux     *http.ServeMux
}

func NewServer(store *Store, scraper interface{ Lookup(string) (*Part, error) }, imagesDir string, ext Extractor) *Server {
	s := &Server{store: store, scraper: scraper, images: imagesDir, ext: ext, mux: http.NewServeMux()}
	static, _ := fs.Sub(staticFS, "static")
	index := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache")
		http.ServeFileFS(w, r, static, "index.html")
	}
	s.mux.HandleFunc("GET /{$}", index)
	s.mux.HandleFunc("GET /add", index)
	s.mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServerFS(static)))
	s.mux.Handle("GET /images/", http.StripPrefix("/images/", http.FileServer(http.Dir(imagesDir))))
	s.mux.HandleFunc("GET /api/ping", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, J{"app": appID, "version": version})
	})
	s.mux.HandleFunc("GET /api/lookup", s.lookup)
	s.mux.HandleFunc("GET /api/items", s.listItems)
	s.mux.HandleFunc("POST /api/items", s.addItem)
	s.mux.HandleFunc("POST /api/items/mark-ordered", s.markOrdered)
	s.mux.HandleFunc("PATCH /api/items/{id}", s.updateItem)
	s.mux.HandleFunc("DELETE /api/items/{id}", s.deleteItem)
	s.mux.HandleFunc("GET /api/export.xlsx", s.export)
	s.mux.HandleFunc("GET /api/bookmarklet", s.bookmarklet)
	return s
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.mux.ServeHTTP(w, r) }

type J map[string]any

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func manualURL(pn string) string { return "https://www.mcmaster.com/" + pn + "/" }

func partPayload(p *Part, cached bool) J {
	image := p.ImageURL
	if p.ImageFile != "" {
		image = "/images/" + p.ImageFile
	}
	opts := p.NameOptions
	if len(opts) == 0 {
		opts = []string{p.Name}
	}
	return J{
		"part_number": p.PartNumber, "name": p.Name, "name_options": opts, "unit": p.Unit,
		"price": p.Price, "image": image, "url": manualURL(p.PartNumber), "cached": cached,
	}
}

func (s *Server) lookup(w http.ResponseWriter, r *http.Request) {
	pn := normalizePartNumber(r.URL.Query().Get("pn"))
	if pn == "" {
		writeJSON(w, 400, J{"ok": false, "error": "הכנס מק״ט."})
		return
	}
	if !looksLikePartNumber(pn) {
		writeJSON(w, 400, J{"ok": false, "url": manualURL(pn),
			"error": fmt.Sprintf("'%s' לא נראה כמו מק״ט של McMaster (למשל 91251A540).", pn)})
		return
	}
	if r.URL.Query().Get("refresh") != "1" {
		if p := s.store.GetPart(pn); p != nil && p.Name != "" {
			writeJSON(w, 200, J{"ok": true, "part": partPayload(p, true)})
			return
		}
	}
	p, err := s.scraper.Lookup(pn)
	if err != nil {
		log.Printf("lookup %s: %v", pn, err)
		msg := "שגיאה בחיפוש: " + err.Error()
		var le *LookupError
		if errors.As(err, &le) {
			msg = le.Msg
		}
		writeJSON(w, 502, J{"ok": false, "error": msg, "url": manualURL(pn)})
		return
	}
	log.Printf("lookup %s: ok (%s / %s)", pn, p.Name, p.Unit)
	s.store.SavePart(*p)
	writeJSON(w, 200, J{"ok": true, "part": partPayload(p, false)})
}

func (s *Server) listItems(w http.ResponseWriter, r *http.Request) {
	status := r.URL.Query().Get("status")
	if status == "" {
		status = "open"
	}
	writeJSON(w, 200, J{"items": s.store.ListItems(status)})
}

// flexInt accepts both 3 and "3" (the form sends input values as strings).
type flexInt struct {
	V   int
	Set bool
	Bad bool
}

func (f *flexInt) UnmarshalJSON(b []byte) error {
	f.Set = true
	str := strings.Trim(string(b), `"`)
	n, err := strconv.Atoi(strings.TrimSpace(str))
	if err != nil {
		f.Bad = true
		return nil
	}
	f.V = n
	return nil
}

func (s *Server) addItem(w http.ResponseWriter, r *http.Request) {
	var in struct {
		PartNumber                                        string `json:"part_number"`
		Name, Unit, Price, Image, Requester, Note, Source string
		Quantity                                          flexInt
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeJSON(w, 400, J{"ok": false, "error": "בקשה לא תקינה."})
		return
	}
	pn := normalizePartNumber(in.PartNumber)
	qty := in.Quantity.V
	if !in.Quantity.Set {
		qty = 1
	}
	name := strings.TrimSpace(in.Name)
	if pn == "" || name == "" || in.Quantity.Bad || qty < 1 {
		writeJSON(w, 400, J{"ok": false, "error": "חסר מק״ט, שם או כמות תקינה."})
		return
	}
	it := Item{
		PartNumber: pn, Name: name, Unit: strings.TrimSpace(in.Unit), Price: strings.TrimSpace(in.Price),
		Image: strings.TrimSpace(in.Image), Quantity: qty,
		Requester: strings.TrimSpace(in.Requester), Note: strings.TrimSpace(in.Note),
	}
	// Remember manual / bookmarklet details so the next lookup of this part is instant.
	if ex := s.store.GetPart(pn); ex == nil || ex.Name != it.Name || ex.Unit != it.Unit {
		p := Part{PartNumber: pn, Name: it.Name, Unit: it.Unit, Price: it.Price, Source: in.Source}
		if p.Source == "" {
			p.Source = "manual"
		}
		if f, ok := strings.CutPrefix(it.Image, "/images/"); ok {
			p.ImageFile = f
		} else {
			p.ImageURL = it.Image
			if ex != nil {
				p.ImageFile = ex.ImageFile
			}
		}
		s.store.SavePart(p)
	}
	id, err := s.store.AddItem(it)
	if err != nil {
		writeJSON(w, 500, J{"ok": false, "error": "שמירה נכשלה: " + err.Error()})
		return
	}
	writeJSON(w, 200, J{"ok": true, "id": id})
}

func (s *Server) updateItem(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.Atoi(r.PathValue("id"))
	var in struct {
		Quantity                    flexInt
		Name, Unit, Requester, Note *string
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeJSON(w, 400, J{"ok": false, "error": "בקשה לא תקינה."})
		return
	}
	u := ItemUpdate{Name: in.Name, Unit: in.Unit, Requester: in.Requester, Note: in.Note}
	if in.Quantity.Set {
		if in.Quantity.Bad || in.Quantity.V < 1 {
			writeJSON(w, 400, J{"ok": false, "error": "כמות לא תקינה."})
			return
		}
		u.Quantity = &in.Quantity.V
	}
	s.store.UpdateItem(id, u)
	writeJSON(w, 200, J{"ok": true})
}

func (s *Server) deleteItem(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.Atoi(r.PathValue("id"))
	s.store.DeleteItem(id)
	writeJSON(w, 200, J{"ok": true})
}

func (s *Server) markOrdered(w http.ResponseWriter, r *http.Request) {
	var in struct{ IDs []int }
	json.NewDecoder(r.Body).Decode(&in)
	n, err := s.store.MarkOrdered(in.IDs)
	if err != nil {
		writeJSON(w, 500, J{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, 200, J{"ok": true, "count": n})
}

func (s *Server) export(w http.ResponseWriter, r *http.Request) {
	status := r.URL.Query().Get("status")
	if status == "" {
		status = "open"
	}
	items := s.store.ListItems(status)
	f := excelize.NewFile()
	defer f.Close()
	sheet := "הזמנה"
	if status == "ordered" {
		sheet = "הוזמן"
	}
	f.SetSheetName("Sheet1", sheet)
	rtl := true
	f.SetSheetView(sheet, 0, &excelize.ViewOptions{RightToLeft: &rtl})
	headers := []any{"מק״ט", "שם מוצר", "יחידת מכירה", "כמות", "מחיר ליחידה ($)", "מבקש", "הערה", "נוסף ב", "קישור"}
	f.SetSheetRow(sheet, "A1", &headers)
	bold, _ := f.NewStyle(&excelize.Style{Font: &excelize.Font{Bold: true}})
	f.SetRowStyle(sheet, 1, 1, bold)
	for i, it := range items {
		row := []any{it.PartNumber, it.Name, it.Unit, it.Quantity, it.Price, it.Requester, it.Note,
			strings.Replace(it.CreatedAt, "T", " ", 1), manualURL(it.PartNumber)}
		cell, _ := excelize.CoordinatesToCellName(1, i+2)
		f.SetSheetRow(sheet, cell, &row)
	}
	for i, width := range []float64{14, 60, 16, 8, 14, 14, 30, 18, 40} {
		col, _ := excelize.ColumnNumberToName(i + 1)
		f.SetColWidth(sheet, col, col, width)
	}
	w.Header().Set("Content-Type", "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet")
	w.Header().Set("Content-Disposition",
		fmt.Sprintf(`attachment; filename="mcmaster-order-%s.xlsx"`, time.Now().Format("2006-01-02")))
	f.Write(w)
}

func (s *Server) bookmarklet(w http.ResponseWriter, r *http.Request) {
	src, _ := s.ext.JS()
	var lines []string
	for _, l := range strings.Split(src, "\n") {
		if !strings.HasPrefix(strings.TrimSpace(l), "//") {
			lines = append(lines, strings.TrimRight(l, "\r"))
		}
	}
	target := "http://" + r.Host + "/add?"
	js := "(()=>{const d=(" + strings.TrimSpace(strings.Join(lines, "\n")) + ")();" +
		"const pn=d.partNumber||prompt('Part number?');if(!pn)return;" +
		"window.open('" + target + "'+new URLSearchParams({pn:pn,name:d.names[0]||''," +
		"unit:d.unit||'',price:d.price||'',image:d.image||'',src:'bookmarklet'}).toString(),'_blank');})();"
	writeJSON(w, 200, J{"href": "javascript:" + strings.ReplaceAll(url.PathEscape(js), "+", "%2B")})
}

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
