package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// PriceTier is one row of McMaster's quantity pricing ("1-11 pairs $28.46",
// "12 or more $25.93"). Max 0 means "and up". Quantities count sale units
// (packs, pairs, each).
type PriceTier struct {
	Min   int     `json:"min"`
	Max   int     `json:"max"`
	Price float64 `json:"price"`
}

// unitPriceFor returns the per-unit price that applies to qty.
func unitPriceFor(tiers []PriceTier, qty int) float64 {
	if len(tiers) == 0 {
		return 0
	}
	for _, t := range tiers {
		if qty >= t.Min && (t.Max == 0 || qty <= t.Max) {
			return t.Price
		}
	}
	return tiers[0].Price
}

func round2(f float64) float64 { return math.Round(f*100) / 100 }

// Part is a cached lookup result for one part number.
type Part struct {
	PartNumber  string      `json:"part_number"`
	Name        string      `json:"name"`
	NameOptions []string    `json:"name_options,omitempty"`
	Unit        string      `json:"unit"`
	Tiers       []PriceTier `json:"tiers"`
	ImageURL    string      `json:"image_url"`
	ImageFile   string      `json:"image_file"`
	Source      string      `json:"source"`
	FetchedAt   string      `json:"fetched_at"`
}

// Item is one line on the order list.
type Item struct {
	ID         int         `json:"id"`
	PartNumber string      `json:"part_number"`
	Name       string      `json:"name"`
	Unit       string      `json:"unit"`
	Tiers      []PriceTier `json:"tiers"`
	UnitPrice  float64     `json:"unit_price"`
	Quantity   int         `json:"quantity"`
	Total      float64     `json:"total"`
	Image      string      `json:"image"`
	UserID     int         `json:"user_id"`
	Requester  string      `json:"requester"`
	Project    string      `json:"project"`
	Purpose    string      `json:"purpose"`
	Status     string      `json:"status"` // open | ordered
	CreatedAt  string      `json:"created_at"`
	OrderID    int         `json:"order_id"`
}

func (it *Item) reprice() {
	it.UnitPrice = unitPriceFor(it.Tiers, it.Quantity)
	it.Total = round2(it.UnitPrice * float64(it.Quantity))
}

// Order is one placed McMaster order (what used to be one weekly sheet).
type Order struct {
	ID        int     `json:"id"`
	PONumber  string  `json:"po_number"`
	PODate    string  `json:"po_date"`
	OrderedBy string  `json:"ordered_by"`
	CreatedAt string  `json:"created_at"`
	Items     int     `json:"items"`
	Total     float64 `json:"total"`
}

type User struct {
	ID        int    `json:"id"`
	Username  string `json:"username"`
	Name      string `json:"name"`
	PassHash  string `json:"pass_hash"`
	Role      string `json:"role"` // admin | user
	Active    bool   `json:"active"`
	CreatedAt string `json:"created_at"`
}

func (u *User) IsAdmin() bool { return u.Role == "admin" }

// session is one browser. Admin means the manager password was entered in
// this browser; it only counts while the user still has the admin role.
type session struct {
	UserID  int       `json:"user_id"`
	Expires time.Time `json:"expires"`
	Admin   bool      `json:"admin"`
}

type storeData struct {
	NextID      int                 `json:"next_id"`
	NextOrderID int                 `json:"next_order_id"`
	NextUserID  int                 `json:"next_user_id"`
	Parts       map[string]*Part    `json:"parts"`
	Items       []*Item             `json:"items"`
	Orders      []*Order            `json:"orders"`
	Users       []*User             `json:"users"`
	Sessions    map[string]*session `json:"sessions"`
	Projects    []string            `json:"projects"`
}

// Store keeps everything in one JSON file. The data is small (a few thousand
// rows a year), so a file rewrite per change is fine and needs no database.
type Store struct {
	path string
	mu   sync.Mutex
	d    storeData
}

var (
	ErrNotFound  = errors.New("not found")
	ErrForbidden = errors.New("forbidden")
	ErrTaken     = errors.New("username taken")
	ErrLastAdmin = errors.New("last active admin")
)

func now() string   { return time.Now().Format("2006-01-02T15:04:05") }
func today() string { return time.Now().Format("2006-01-02") }

func OpenStore(path string) (*Store, error) {
	s := &Store{path: path}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	b, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if err == nil {
		if err := json.Unmarshal(b, &s.d); err != nil {
			return nil, err
		}
	}
	if s.d.Parts == nil {
		s.d.Parts = map[string]*Part{}
	}
	if s.d.Sessions == nil {
		s.d.Sessions = map[string]*session{}
	}
	for _, c := range []*int{&s.d.NextID, &s.d.NextOrderID, &s.d.NextUserID} {
		if *c < 1 {
			*c = 1
		}
	}
	return s, nil
}

func (s *Store) save() error {
	b, err := json.MarshalIndent(s.d, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

// ---------- parts ----------

func (s *Store) GetPart(pn string) *Part {
	s.mu.Lock()
	defer s.mu.Unlock()
	if p, ok := s.d.Parts[pn]; ok {
		cp := *p
		return &cp
	}
	return nil
}

func (s *Store) SavePart(p Part) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	p.FetchedAt = now()
	s.d.Parts[p.PartNumber] = &p
	return s.save()
}

// ---------- items ----------

// OpenItems returns the current (not yet ordered) list; userID 0 means everyone's.
func (s *Store) OpenItems(userID int) []Item {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []Item{}
	for _, it := range s.d.Items {
		if it.Status == "open" && (userID == 0 || it.UserID == userID) {
			out = append(out, *it)
		}
	}
	return out
}

func (s *Store) OrderItems(orderID int, userID int) []Item {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []Item{}
	for _, it := range s.d.Items {
		if it.Status == "ordered" && it.OrderID == orderID && (userID == 0 || it.UserID == userID) {
			out = append(out, *it)
		}
	}
	return out
}

func (s *Store) AddItem(it Item) (Item, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	it.ID = s.d.NextID
	s.d.NextID++
	it.Status = "open"
	it.CreatedAt = now()
	it.reprice()
	s.d.Items = append(s.d.Items, &it)
	return it, s.save()
}

// ItemUpdate holds the editable fields; nil means "leave as is".
type ItemUpdate struct {
	Quantity *int
	Project  *string
	Purpose  *string
}

// UpdateItem edits an open item. Without manager mode only your own.
func (s *Store) UpdateItem(id int, u *User, manager bool, up ItemUpdate) (Item, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	it := s.findOpen(id)
	if it == nil {
		return Item{}, ErrNotFound
	}
	if !manager && it.UserID != u.ID {
		return Item{}, ErrForbidden
	}
	if up.Quantity != nil {
		it.Quantity = *up.Quantity
	}
	if up.Project != nil {
		it.Project = *up.Project
	}
	if up.Purpose != nil {
		it.Purpose = *up.Purpose
	}
	it.reprice()
	return *it, s.save()
}

func (s *Store) DeleteItem(id int, u *User, manager bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, it := range s.d.Items {
		if it.ID == id && it.Status == "open" {
			if !manager && it.UserID != u.ID {
				return ErrForbidden
			}
			s.d.Items = append(s.d.Items[:i], s.d.Items[i+1:]...)
			return s.save()
		}
	}
	return ErrNotFound
}

func (s *Store) findOpen(id int) *Item {
	for _, it := range s.d.Items {
		if it.ID == id && it.Status == "open" {
			return it
		}
	}
	return nil
}

// PlaceOrder closes the given open items (all open items if ids is empty)
// into one Order with the PO number / date purchasing entered.
func (s *Store) PlaceOrder(ids []int, poNumber, poDate, orderedBy string) (*Order, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	want := map[int]bool{}
	for _, id := range ids {
		want[id] = true
	}
	o := &Order{ID: s.d.NextOrderID, PONumber: poNumber, PODate: poDate, OrderedBy: orderedBy, CreatedAt: now()}
	if o.PODate == "" {
		o.PODate = today()
	}
	for _, it := range s.d.Items {
		if it.Status == "open" && (len(ids) == 0 || want[it.ID]) {
			it.Status, it.OrderID = "ordered", o.ID
			o.Items++
			o.Total += it.Total
		}
	}
	if o.Items == 0 {
		return nil, ErrNotFound
	}
	o.Total = round2(o.Total)
	s.d.NextOrderID++
	s.d.Orders = append(s.d.Orders, o)
	return o, s.save()
}

// Orders lists placed orders, newest first.
func (s *Store) Orders() []Order {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Order, 0, len(s.d.Orders))
	for _, o := range s.d.Orders {
		out = append(out, *o)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID > out[j].ID })
	return out
}

func (s *Store) GetOrder(id int) *Order {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, o := range s.d.Orders {
		if o.ID == id {
			cp := *o
			return &cp
		}
	}
	return nil
}

// LastOrderFor returns the newest order containing items of this user.
func (s *Store) LastOrderFor(userID int) *Order {
	s.mu.Lock()
	defer s.mu.Unlock()
	best := 0
	for _, it := range s.d.Items {
		if it.Status == "ordered" && it.UserID == userID && it.OrderID > best {
			best = it.OrderID
		}
	}
	for _, o := range s.d.Orders {
		if o.ID == best {
			cp := *o
			return &cp
		}
	}
	return nil
}

// ---------- projects ----------

func (s *Store) Projects() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string{}, s.d.Projects...)
}

func (s *Store) SetProjects(list []string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	seen := map[string]bool{}
	out := []string{}
	for _, p := range list {
		p = strings.TrimSpace(p)
		if p != "" && !seen[strings.ToLower(p)] {
			seen[strings.ToLower(p)] = true
			out = append(out, p)
		}
	}
	s.d.Projects = out
	return s.save()
}

// ---------- users & sessions ----------

func (s *Store) UserCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.d.Users)
}

func (s *Store) Users() []User {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]User, 0, len(s.d.Users))
	for _, u := range s.d.Users {
		out = append(out, *u)
	}
	return out
}

func (s *Store) UserByName(username string) *User {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, u := range s.d.Users {
		if strings.EqualFold(u.Username, username) {
			cp := *u
			return &cp
		}
	}
	return nil
}

func (s *Store) AddUser(u User) (User, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, x := range s.d.Users {
		if strings.EqualFold(x.Username, u.Username) {
			return User{}, ErrTaken
		}
	}
	u.ID = s.d.NextUserID
	s.d.NextUserID++
	u.CreatedAt = now()
	s.d.Users = append(s.d.Users, &u)
	return u, s.save()
}

// UpdateUser applies fn to a copy of the user and stores it, unless that
// would leave no active admin.
func (s *Store) UpdateUser(id int, fn func(*User)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, u := range s.d.Users {
		if u.ID != id {
			continue
		}
		next := *u
		fn(&next)
		if u.Active && u.IsAdmin() && !(next.Active && next.IsAdmin()) && s.activeAdmins() <= 1 {
			return ErrLastAdmin
		}
		*u = next
		if !u.Active {
			s.dropSessions(u.ID)
		}
		return s.save()
	}
	return ErrNotFound
}

func (s *Store) activeAdmins() int {
	n := 0
	for _, u := range s.d.Users {
		if u.Active && u.IsAdmin() {
			n++
		}
	}
	return n
}

func (s *Store) NewSession(userID int, ttl time.Duration) (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	tok := hex.EncodeToString(b)
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, v := range s.d.Sessions {
		if time.Now().After(v.Expires) {
			delete(s.d.Sessions, k)
		}
	}
	s.d.Sessions[tok] = &session{UserID: userID, Expires: time.Now().Add(ttl)}
	return tok, s.save()
}

// SessionUser returns the active user behind a session token (or nil), and
// whether this browser is in manager mode.
func (s *Store) SessionUser(tok string) (*User, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ses, ok := s.d.Sessions[tok]
	if !ok || time.Now().After(ses.Expires) {
		return nil, false
	}
	for _, u := range s.d.Users {
		if u.ID == ses.UserID && u.Active {
			cp := *u
			return &cp, ses.Admin && u.IsAdmin()
		}
	}
	return nil, false
}

// SetSessionAdmin turns manager mode on or off for one browser.
func (s *Store) SetSessionAdmin(tok string, on bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	ses, ok := s.d.Sessions[tok]
	if !ok {
		return ErrNotFound
	}
	ses.Admin = on
	return s.save()
}

// FindOrCreateUser returns the user with this name (any letter case),
// creating a regular user the first time a name is used.
func (s *Store) FindOrCreateUser(name string) (User, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, u := range s.d.Users {
		if strings.EqualFold(u.Username, name) {
			return *u, nil
		}
	}
	u := &User{ID: s.d.NextUserID, Username: name, Name: name, Role: "user", Active: true, CreatedAt: now()}
	s.d.NextUserID++
	s.d.Users = append(s.d.Users, u)
	return *u, s.save()
}

// HasAdmin reports whether any active manager with a password exists.
func (s *Store) HasAdmin() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, u := range s.d.Users {
		if u.Active && u.IsAdmin() && u.PassHash != "" {
			return true
		}
	}
	return false
}

// MakeFirstAdmin promotes a user to manager, but only while there is none.
func (s *Store) MakeFirstAdmin(userID int, passHash string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, u := range s.d.Users {
		if u.Active && u.IsAdmin() && u.PassHash != "" {
			return ErrTaken
		}
	}
	for _, u := range s.d.Users {
		if u.ID == userID {
			u.Role, u.PassHash = "admin", passHash
			return s.save()
		}
	}
	return ErrNotFound
}

func (s *Store) DeleteSession(tok string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.d.Sessions, tok)
	s.save()
}

func (s *Store) dropSessions(userID int) {
	for k, v := range s.d.Sessions {
		if v.UserID == userID {
			delete(s.d.Sessions, k)
		}
	}
}
