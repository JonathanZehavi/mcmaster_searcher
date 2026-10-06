package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// Part is a cached lookup result for one part number.
type Part struct {
	PartNumber  string   `json:"part_number"`
	Name        string   `json:"name"`
	NameOptions []string `json:"name_options,omitempty"`
	Unit        string   `json:"unit"`
	Price       string   `json:"price"`
	ImageURL    string   `json:"image_url"`
	ImageFile   string   `json:"image_file"`
	Source      string   `json:"source"`
	FetchedAt   string   `json:"fetched_at"`
}

// Item is one line on the order list.
type Item struct {
	ID         int    `json:"id"`
	PartNumber string `json:"part_number"`
	Name       string `json:"name"`
	Unit       string `json:"unit"`
	Price      string `json:"price"`
	Image      string `json:"image"`
	Quantity   int    `json:"quantity"`
	Requester  string `json:"requester"`
	Note       string `json:"note"`
	Status     string `json:"status"`
	CreatedAt  string `json:"created_at"`
	OrderedAt  string `json:"ordered_at"`
}

type storeData struct {
	NextID int              `json:"next_id"`
	Parts  map[string]*Part `json:"parts"`
	Items  []*Item          `json:"items"`
}

// Store keeps everything in one JSON file. The data is small (hundreds of
// rows a year), so a file rewrite per change is fine and needs no database.
type Store struct {
	path string
	mu   sync.Mutex
	d    storeData
}

func now() string { return time.Now().Format("2006-01-02T15:04:05") }

func OpenStore(path string) (*Store, error) {
	s := &Store{path: path, d: storeData{NextID: 1, Parts: map[string]*Part{}}}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, &s.d); err != nil {
		return nil, err
	}
	if s.d.Parts == nil {
		s.d.Parts = map[string]*Part{}
	}
	return s, nil
}

func (s *Store) save() error {
	b, err := json.MarshalIndent(s.d, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

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

func (s *Store) ListItems(status string) []Item {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []Item{}
	for _, it := range s.d.Items {
		if it.Status == status {
			out = append(out, *it)
		}
	}
	if status == "ordered" {
		sort.SliceStable(out, func(i, j int) bool {
			if out[i].OrderedAt != out[j].OrderedAt {
				return out[i].OrderedAt > out[j].OrderedAt
			}
			return out[i].ID > out[j].ID
		})
		if len(out) > 500 {
			out = out[:500]
		}
	}
	return out
}

func (s *Store) AddItem(it Item) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	it.ID = s.d.NextID
	s.d.NextID++
	it.Status = "open"
	it.CreatedAt = now()
	s.d.Items = append(s.d.Items, &it)
	return it.ID, s.save()
}

// ItemUpdate holds the editable fields; nil means "leave as is".
type ItemUpdate struct {
	Quantity  *int    `json:"quantity"`
	Name      *string `json:"name"`
	Unit      *string `json:"unit"`
	Requester *string `json:"requester"`
	Note      *string `json:"note"`
}

func (s *Store) UpdateItem(id int, u ItemUpdate) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, it := range s.d.Items {
		if it.ID != id || it.Status != "open" {
			continue
		}
		if u.Quantity != nil {
			it.Quantity = *u.Quantity
		}
		if u.Name != nil {
			it.Name = *u.Name
		}
		if u.Unit != nil {
			it.Unit = *u.Unit
		}
		if u.Requester != nil {
			it.Requester = *u.Requester
		}
		if u.Note != nil {
			it.Note = *u.Note
		}
		return s.save()
	}
	return nil
}

func (s *Store) DeleteItem(id int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, it := range s.d.Items {
		if it.ID == id && it.Status == "open" {
			s.d.Items = append(s.d.Items[:i], s.d.Items[i+1:]...)
			return s.save()
		}
	}
	return nil
}

// MarkOrdered moves open items to history. With no ids it moves all of them.
func (s *Store) MarkOrdered(ids []int) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	want := map[int]bool{}
	for _, id := range ids {
		want[id] = true
	}
	n, ts := 0, now()
	for _, it := range s.d.Items {
		if it.Status == "open" && (len(ids) == 0 || want[it.ID]) {
			it.Status, it.OrderedAt = "ordered", ts
			n++
		}
	}
	if n == 0 {
		return 0, nil
	}
	return n, s.save()
}
