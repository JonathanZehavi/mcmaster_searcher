package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/chromedp"
)

// The network recorder notes the data requests a McMaster product page makes
// while it renders (its XHR/fetch calls and their answers), and saves them
// next to the debug snapshots. They show where the page gets the product
// details from, which is what a faster, browser-free lookup would call
// directly. No cookies or request headers are kept.

const (
	recMaxEntries  = 60
	recMaxBody     = 150 << 10
	recKeepFiles   = 30
	recFileSuffix  = "-network.json"
	recBodyTimeout = 2 * time.Second
)

type netEntry struct {
	URL       string `json:"url"`
	Method    string `json:"method"`
	Type      string `json:"type"`
	Status    int64  `json:"status"`
	MIME      string `json:"mime"`
	PostData  string `json:"post_data,omitempty"`
	Size      int    `json:"size"`
	HasPart   bool   `json:"mentions_part_number"`
	Body      string `json:"body,omitempty"`
	Truncated bool   `json:"body_truncated,omitempty"`
	At        string `json:"at"`
}

type netRecorder struct {
	tab   context.Context
	pn    string
	start time.Time
	mu    sync.Mutex
	byID  map[network.RequestID]*netEntry
	order []network.RequestID
	wg    sync.WaitGroup
}

// recordable: the page's own data calls, plus the page itself.
func recordable(t network.ResourceType) bool {
	return t == network.ResourceTypeXHR || t == network.ResourceTypeFetch || t == network.ResourceTypeDocument
}

func startRecorder(tab context.Context, pn string) *netRecorder {
	r := &netRecorder{tab: tab, pn: pn, start: time.Now(), byID: map[network.RequestID]*netEntry{}}
	chromedp.ListenTarget(tab, func(ev interface{}) {
		switch e := ev.(type) {
		case *network.EventRequestWillBeSent:
			if !recordable(e.Type) || e.Request == nil {
				return
			}
			r.mu.Lock()
			defer r.mu.Unlock()
			if len(r.order) >= recMaxEntries {
				return
			}
			r.byID[e.RequestID] = &netEntry{
				URL: e.Request.URL, Method: e.Request.Method, Type: string(e.Type),
				At: fmt.Sprintf("+%.2fs", time.Since(r.start).Seconds()),
			}
			r.order = append(r.order, e.RequestID)
			if e.Request.HasPostData {
				r.fetchAsync(e.RequestID, func(ctx context.Context, n *netEntry) {
					if pd, err := network.GetRequestPostData(e.RequestID).Do(ctx); err == nil {
						r.mu.Lock()
						n.PostData = truncate(pd, 20<<10)
						r.mu.Unlock()
					}
				})
			}
		case *network.EventResponseReceived:
			r.mu.Lock()
			if n, ok := r.byID[e.RequestID]; ok && e.Response != nil {
				n.Status, n.MIME = e.Response.Status, e.Response.MimeType
			}
			r.mu.Unlock()
		case *network.EventLoadingFinished:
			r.mu.Lock()
			_, ok := r.byID[e.RequestID]
			r.mu.Unlock()
			if ok {
				r.fetchAsync(e.RequestID, func(ctx context.Context, n *netEntry) {
					body, err := network.GetResponseBody(e.RequestID).Do(ctx)
					if err != nil {
						return
					}
					r.mu.Lock()
					defer r.mu.Unlock()
					n.Size = len(body)
					text := string(body)
					n.HasPart = strings.Contains(strings.ToUpper(text), r.pn)
					if len(text) > recMaxBody {
						text, n.Truncated = text[:recMaxBody], true
					}
					n.Body = text
				})
			}
		}
	})
	return r
}

// fetchAsync runs a browser call for one request outside the event loop
// (calling the browser from inside a listener would deadlock).
func (r *netRecorder) fetchAsync(id network.RequestID, fn func(context.Context, *netEntry)) {
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		r.mu.Lock()
		n := r.byID[id]
		r.mu.Unlock()
		if n == nil {
			return
		}
		c := chromedp.FromContext(r.tab)
		if c == nil || c.Target == nil {
			return
		}
		ctx, cancel := context.WithTimeout(cdp.WithExecutor(r.tab, c.Target), recBodyTimeout)
		defer cancel()
		fn(ctx, n)
	}()
}

// save writes what was recorded to <dir>/<pn>-<time>-network.json and keeps
// only the newest recordings. Called before the tab closes.
func (r *netRecorder) save(dir, pageURL, outcome string) string {
	done := make(chan struct{})
	go func() { r.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(recBodyTimeout + 500*time.Millisecond):
	}
	r.mu.Lock()
	entries := make([]*netEntry, 0, len(r.order))
	for _, id := range r.order {
		entries = append(entries, r.byID[id])
	}
	b, err := json.MarshalIndent(map[string]any{
		"part_number": r.pn, "page": pageURL, "outcome": outcome,
		"recorded_at": time.Now().Format(time.RFC3339), "requests": entries,
	}, "", "  ")
	r.mu.Unlock()
	if err != nil {
		return ""
	}
	name := fmt.Sprintf("%s-%s%s", r.pn, time.Now().Format("20060102-150405"), recFileSuffix)
	if os.WriteFile(filepath.Join(dir, name), b, 0o644) != nil {
		return ""
	}
	pruneRecordings(dir)
	return name
}

func pruneRecordings(dir string) {
	matches, _ := filepath.Glob(filepath.Join(dir, "*"+recFileSuffix))
	if len(matches) <= recKeepFiles {
		return
	}
	sort.Strings(matches) // names end in a timestamp, so per part number they sort by age
	sort.SliceStable(matches, func(i, j int) bool {
		a, _ := os.Stat(matches[i])
		b, _ := os.Stat(matches[j])
		return a != nil && b != nil && a.ModTime().Before(b.ModTime())
	})
	for _, m := range matches[:len(matches)-recKeepFiles] {
		os.Remove(m)
	}
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}
