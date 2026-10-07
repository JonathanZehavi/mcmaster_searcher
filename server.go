package main

import (
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/xuri/excelize/v2"
)

//go:embed static
var staticFS embed.FS

type Lookuper interface{ Lookup(string) (*Part, error) }

type Server struct {
	store   *Store
	scraper Lookuper
	ext     Extractor
	mux     *http.ServeMux
}

func NewServer(store *Store, scraper Lookuper, imagesDir string, ext Extractor) *Server {
	s := &Server{store: store, scraper: scraper, ext: ext, mux: http.NewServeMux()}
	static, _ := fs.Sub(staticFS, "static")
	index := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache")
		http.ServeFileFS(w, r, static, "index.html")
	}
	user, admin := func(h http.HandlerFunc) http.HandlerFunc { return s.requireUser(false, h) },
		func(h http.HandlerFunc) http.HandlerFunc { return s.requireUser(true, h) }

	s.mux.HandleFunc("GET /{$}", index)
	s.mux.HandleFunc("GET /add", index)
	// no-cache: after an update the browser must not mix a new page with old scripts.
	files := http.StripPrefix("/static/", http.FileServerFS(static))
	s.mux.HandleFunc("GET /static/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache")
		files.ServeHTTP(w, r)
	})
	s.mux.Handle("GET /images/", http.StripPrefix("/images/", http.FileServer(http.Dir(imagesDir))))
	s.mux.HandleFunc("GET /api/ping", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, J{"app": appID, "version": version})
	})

	s.mux.HandleFunc("GET /api/me", s.me)
	s.mux.HandleFunc("POST /api/identify", s.identifyAs)
	s.mux.HandleFunc("POST /api/forget", s.forget)
	s.mux.HandleFunc("POST /api/admin/enter", user(s.adminEnter))
	s.mux.HandleFunc("POST /api/admin/leave", user(s.adminLeave))
	s.mux.HandleFunc("POST /api/me/password", admin(s.changePassword))

	s.mux.HandleFunc("GET /api/lookup", user(s.lookup))
	s.mux.HandleFunc("GET /api/items", user(s.listItems))
	s.mux.HandleFunc("POST /api/items", user(s.addItem))
	s.mux.HandleFunc("PATCH /api/items/{id}", user(s.updateItem))
	s.mux.HandleFunc("DELETE /api/items/{id}", user(s.deleteItem))
	s.mux.HandleFunc("GET /api/my-last-order", user(s.myLastOrder))
	s.mux.HandleFunc("GET /api/bookmarklet", user(s.bookmarklet))

	s.mux.HandleFunc("POST /api/orders", admin(s.placeOrder))
	s.mux.HandleFunc("GET /api/orders", admin(s.listOrders))
	s.mux.HandleFunc("GET /api/orders/{id}", admin(s.getOrder))
	s.mux.HandleFunc("GET /api/export.xlsx", admin(s.export))
	s.mux.HandleFunc("GET /api/users", admin(s.listUsers))
	s.mux.HandleFunc("PATCH /api/users/{id}", admin(s.updateUser))
	s.mux.HandleFunc("PUT /api/projects", admin(s.setProjects))
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
	tiers := p.Tiers
	if tiers == nil {
		tiers = []PriceTier{}
	}
	return J{
		"part_number": p.PartNumber, "name": p.Name, "name_options": opts, "unit": p.Unit,
		"tiers": tiers, "image": image, "url": manualURL(p.PartNumber), "cached": cached,
	}
}

// ---------- lookup ----------

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
	log.Printf("lookup %s: ok (%s / %s / %d price tiers)", pn, p.Name, p.Unit, len(p.Tiers))
	s.store.SavePart(*p)
	writeJSON(w, 200, J{"ok": true, "part": partPayload(p, false)})
}

// ---------- items ----------

// flexInt accepts both 3 and "3" (form inputs send strings).
type flexInt struct {
	V        int
	Set, Bad bool
}

func (f *flexInt) UnmarshalJSON(b []byte) error {
	f.Set = true
	n, err := strconv.Atoi(strings.TrimSpace(strings.Trim(string(b), `"`)))
	f.V, f.Bad = n, err != nil
	return nil
}

// flexFloat accepts 12.5, "12.5" and "$12.50".
type flexFloat struct{ V float64 }

func (f *flexFloat) UnmarshalJSON(b []byte) error {
	str := strings.NewReplacer(`"`, "", "$", "", ",", "", " ", "").Replace(string(b))
	f.V, _ = strconv.ParseFloat(str, 64)
	return nil
}

// GET /api/items: the current list. Users see their own lines; admins see all.
func (s *Server) listItems(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	uid := u.ID
	if currentIdentity(r).admin && r.URL.Query().Get("scope") == "all" {
		uid = 0
	}
	writeJSON(w, 200, J{"items": s.store.OpenItems(uid)})
}

func (s *Server) addItem(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	var in struct {
		PartNumber string      `json:"part_number"`
		Name       string      `json:"name"`
		Unit       string      `json:"unit"`
		Image      string      `json:"image"`
		Project    string      `json:"project"`
		Purpose    string      `json:"purpose"`
		Source     string      `json:"source"`
		Quantity   flexInt     `json:"quantity"`
		Tiers      []PriceTier `json:"tiers"`
		UnitPrice  flexFloat   `json:"unit_price"` // manual entry, when there are no tiers
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
	project := strings.TrimSpace(in.Project)
	switch {
	case pn == "" || name == "":
		writeJSON(w, 400, J{"ok": false, "error": "חסר מק״ט או שם מוצר."})
		return
	case in.Quantity.Bad || qty < 1:
		writeJSON(w, 400, J{"ok": false, "error": "כמות לא תקינה."})
		return
	case project == "":
		writeJSON(w, 400, J{"ok": false, "error": "בחר פרויקט."})
		return
	}

	// Prices: the looked-up tiers win; otherwise what the user typed.
	cached := s.store.GetPart(pn)
	tiers := cleanTiers(in.Tiers)
	if cached != nil && cached.Source == "auto" && len(cached.Tiers) > 0 {
		tiers = cached.Tiers
	}
	if len(tiers) == 0 && in.UnitPrice.V > 0 {
		tiers = []PriceTier{{Min: 1, Price: round2(in.UnitPrice.V)}}
	}

	it := Item{
		PartNumber: pn, Name: name, Unit: strings.TrimSpace(in.Unit), Tiers: tiers,
		Quantity: qty, Image: strings.TrimSpace(in.Image),
		UserID: u.ID, Requester: u.Name, Project: project, Purpose: strings.TrimSpace(in.Purpose),
	}
	// Remember manual / bookmarklet details so the next lookup of this part is instant.
	if cached == nil || cached.Name != it.Name || cached.Unit != it.Unit || len(cached.Tiers) == 0 {
		p := Part{PartNumber: pn, Name: it.Name, Unit: it.Unit, Tiers: tiers, Source: in.Source}
		if p.Source == "" {
			p.Source = "manual"
		}
		if f, ok := strings.CutPrefix(it.Image, "/images/"); ok {
			p.ImageFile = f
		} else {
			p.ImageURL = it.Image
			if cached != nil {
				p.ImageFile = cached.ImageFile
			}
		}
		s.store.SavePart(p)
	}
	saved, err := s.store.AddItem(it)
	if err != nil {
		writeJSON(w, 500, J{"ok": false, "error": "שמירה נכשלה: " + err.Error()})
		return
	}
	writeJSON(w, 200, J{"ok": true, "item": saved})
}

// cleanTiers drops nonsense and sorts tiers coming from the browser.
func cleanTiers(in []PriceTier) []PriceTier {
	out := []PriceTier{}
	for _, t := range in {
		if t.Min >= 1 && t.Price > 0 && (t.Max == 0 || t.Max >= t.Min) {
			t.Price = round2(t.Price)
			out = append(out, t)
		}
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j].Min < out[j-1].Min; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

func (s *Server) updateItem(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.Atoi(r.PathValue("id"))
	var in struct {
		Quantity flexInt `json:"quantity"`
		Project  *string `json:"project"`
		Purpose  *string `json:"purpose"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeJSON(w, 400, J{"ok": false, "error": "בקשה לא תקינה."})
		return
	}
	up := ItemUpdate{Project: in.Project, Purpose: in.Purpose}
	if in.Quantity.Set {
		if in.Quantity.Bad || in.Quantity.V < 1 {
			writeJSON(w, 400, J{"ok": false, "error": "כמות לא תקינה."})
			return
		}
		up.Quantity = &in.Quantity.V
	}
	it, err := s.store.UpdateItem(id, currentUser(r), currentIdentity(r).admin, up)
	if !writeStoreErr(w, err) {
		writeJSON(w, 200, J{"ok": true, "item": it})
	}
}

func (s *Server) deleteItem(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.Atoi(r.PathValue("id"))
	if !writeStoreErr(w, s.store.DeleteItem(id, currentUser(r), currentIdentity(r).admin)) {
		writeJSON(w, 200, J{"ok": true})
	}
}

// writeStoreErr answers for a store error and reports whether it did.
func writeStoreErr(w http.ResponseWriter, err error) bool {
	switch {
	case err == nil:
		return false
	case errors.Is(err, ErrForbidden):
		writeJSON(w, 403, J{"ok": false, "error": "אפשר לשנות רק שורות שלך."})
	case errors.Is(err, ErrNotFound):
		writeJSON(w, 404, J{"ok": false, "error": "השורה לא נמצאה (אולי כבר הוזמנה)."})
	default:
		writeJSON(w, 500, J{"ok": false, "error": err.Error()})
	}
	return true
}

// GET /api/my-last-order: the user's own lines in the most recent order that had any.
func (s *Server) myLastOrder(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	o := s.store.LastOrderFor(u.ID)
	if o == nil {
		writeJSON(w, 200, J{"order": nil, "items": []Item{}})
		return
	}
	writeJSON(w, 200, J{"order": o, "items": s.store.OrderItems(o.ID, u.ID)})
}

// ---------- orders (admin) ----------

func (s *Server) placeOrder(w http.ResponseWriter, r *http.Request) {
	var in struct {
		IDs      []int  `json:"ids"`
		PONumber string `json:"po_number"`
		PODate   string `json:"po_date"`
	}
	json.NewDecoder(r.Body).Decode(&in)
	if len(in.IDs) == 0 {
		writeJSON(w, 400, J{"ok": false, "error": "אין פריטים להזמנה."})
		return
	}
	o, err := s.store.PlaceOrder(in.IDs, strings.TrimSpace(in.PONumber), strings.TrimSpace(in.PODate), currentUser(r).Name)
	if !writeStoreErr(w, err) {
		writeJSON(w, 200, J{"ok": true, "order": o})
	}
}

func (s *Server) listOrders(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, J{"orders": s.store.Orders()})
}

func (s *Server) getOrder(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.Atoi(r.PathValue("id"))
	o := s.store.GetOrder(id)
	if o == nil {
		writeJSON(w, 404, J{"ok": false, "error": "הזמנה לא נמצאה."})
		return
	}
	writeJSON(w, 200, J{"order": o, "items": s.store.OrderItems(id, 0)})
}

func (s *Server) setProjects(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Projects []string `json:"projects"`
	}
	json.NewDecoder(r.Body).Decode(&in)
	if err := s.store.SetProjects(in.Projects); err != nil {
		writeJSON(w, 500, J{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, 200, J{"ok": true, "projects": s.store.Projects()})
}

// GET /api/export.xlsx?order=ID, or the current list without it. Columns match
// the purchasing sheet that was kept by hand until now.
func (s *Server) export(w http.ResponseWriter, r *http.Request) {
	var items []Item
	var po, poDate, fname string
	if id, err := strconv.Atoi(r.URL.Query().Get("order")); err == nil {
		o := s.store.GetOrder(id)
		if o == nil {
			http.Error(w, "order not found", 404)
			return
		}
		items, po, poDate = s.store.OrderItems(id, 0), o.PONumber, o.PODate
		fname = fmt.Sprintf("mcmaster-order-%s-%s.xlsx", po, o.PODate)
	} else {
		items = s.store.OpenItems(0)
		fname = fmt.Sprintf("mcmaster-open-list-%s.xlsx", today())
	}

	f := excelize.NewFile()
	defer f.Close()
	sheet := "McMaster"
	f.SetSheetName("Sheet1", sheet)
	headers := []any{"McMaster PO number", "McMaster PO Date", "McMaster Part Number", "Part Description",
		"UOM", "Unit cost", "Qty", "Requestor", "Request Date", "Project", "Purpose", "Total cost"}
	f.SetSheetRow(sheet, "A1", &headers)
	head, _ := f.NewStyle(&excelize.Style{
		Font: &excelize.Font{Bold: true}, Fill: excelize.Fill{Type: "pattern", Pattern: 1, Color: []string{"FFFF00"}},
		Alignment: &excelize.Alignment{Horizontal: "center", Vertical: "center", WrapText: true},
	})
	f.SetCellStyle(sheet, "A1", "L1", head)
	usd := `"USD "#,##0.00`
	money, _ := f.NewStyle(&excelize.Style{CustomNumFmt: &usd})
	var sum float64
	for i, it := range items {
		row := []any{po, excelDate(poDate), it.PartNumber, it.Name, it.Unit, it.UnitPrice, it.Quantity,
			it.Requester, excelDate(it.CreatedAt[:10]), it.Project, it.Purpose, it.Total}
		cell, _ := excelize.CoordinatesToCellName(1, i+2)
		f.SetSheetRow(sheet, cell, &row)
		sum += it.Total
	}
	last := len(items) + 1
	if len(items) > 0 {
		f.SetCellStyle(sheet, "F2", fmt.Sprintf("F%d", last), money)
		f.SetCellStyle(sheet, "L2", fmt.Sprintf("L%d", last+1), money)
		f.SetCellValue(sheet, fmt.Sprintf("K%d", last+1), "Total")
		f.SetCellValue(sheet, fmt.Sprintf("L%d", last+1), round2(sum))
	}
	for i, width := range []float64{12, 12, 14, 60, 12, 12, 6, 14, 12, 16, 30, 13} {
		col, _ := excelize.ColumnNumberToName(i + 1)
		f.SetColWidth(sheet, col, col, width)
	}
	f.AutoFilter(sheet, fmt.Sprintf("A1:L%d", max(last, 2)), nil)
	f.SetPanes(sheet, &excelize.Panes{Freeze: true, YSplit: 1, TopLeftCell: "A2", ActivePane: "bottomLeft"})

	w.Header().Set("Content-Type", "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, fname))
	f.Write(w)
}

// excelDate turns 2026-10-05 into 05/10/2026 (as in the existing sheet).
func excelDate(iso string) string {
	t, err := time.Parse("2006-01-02", iso)
	if err != nil {
		return iso
	}
	return t.Format("02/01/2006")
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
		"unit:d.unit||'',tiers:JSON.stringify(d.tiers||[]),image:d.image||'',src:'bookmarklet'}).toString(),'_blank');})();"
	writeJSON(w, 200, J{"href": "javascript:" + strings.ReplaceAll(url.PathEscape(js), "+", "%2B")})
}
