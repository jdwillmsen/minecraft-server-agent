package wiki

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeWiki serves the subset of the MediaWiki API the client uses, from
// canned pages keyed by title.
type fakeWiki struct {
	opensearch     map[string][]string // lowercased query -> titles
	search         map[string][]string
	pages          map[string]string // title -> extract
	disambiguation map[string]bool
	wikitext       map[string]string
	status         int           // non-zero: answer every request with it
	delay          time.Duration // slow every request
	requests       atomic.Int64
	lastQuery      atomic.Value // most recent raw query string
}

func (f *fakeWiki) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.requests.Add(1)
	f.lastQuery.Store(r.URL.RawQuery)
	if f.delay > 0 {
		time.Sleep(f.delay)
	}
	if f.status != 0 {
		w.WriteHeader(f.status)
		return
	}
	q := r.URL.Query()
	w.Header().Set("content-type", "application/json")
	switch {
	case q.Get("action") == "opensearch":
		term := strings.ToLower(q.Get("search"))
		_ = json.NewEncoder(w).Encode([]any{term, f.opensearch[term], []string{}, []string{}})
	case q.Get("list") == "search":
		var hits []map[string]string
		for _, t := range f.search[strings.ToLower(q.Get("srsearch"))] {
			hits = append(hits, map[string]string{"title": t})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"query": map[string]any{"search": hits}})
	case q.Get("action") == "parse":
		_ = json.NewEncoder(w).Encode(map[string]any{"parse": map[string]any{"wikitext": map[string]string{"*": f.wikitext[q.Get("page")]}}})
	default: // prop=extracts|pageprops
		title := q.Get("titles")
		extract, ok := f.pages[title]
		page := map[string]any{"title": title, "extract": extract}
		if !ok {
			page = map[string]any{"title": title, "missing": ""}
		}
		if f.disambiguation[title] {
			page["pageprops"] = map[string]string{"disambiguation": ""}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"query": map[string]any{"pages": map[string]any{"1": page}}})
	}
}

func newTestClient(t *testing.T, f *fakeWiki, observe func(Outcome)) *Client {
	t.Helper()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return New(Options{
		BaseURL: srv.URL, UserAgent: "test", RequestTimeout: 200 * time.Millisecond,
		RequestsPerMinute: 60, CacheTTL: time.Hour, MissTTL: time.Minute, CacheSize: 64,
		Observe: observe,
	})
}

func golemWiki() *fakeWiki {
	return &fakeWiki{
		opensearch:     map[string][]string{"iron golem": {"Iron Golem"}, "golem": {"Golem", "Iron Golem"}, "torch": {"Torch"}},
		pages:          map[string]string{"Iron Golem": ironGolemExtract, "Golem": "Golem may refer to:", "Torch": "A torch is a light source.\n\n== Obtaining ==\n\n=== Crafting ===\n"},
		disambiguation: map[string]bool{"Golem": true},
		wikitext:       map[string]string{"Torch": torchWikitext},
	}
}

func TestLookupReturnsTheFramedSection(t *testing.T) {
	c := newTestClient(t, golemWiki(), nil)
	out, err := c.Lookup(t.Context(), "iron golem", "spawning")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`page "Iron Golem"`, `section "Spawning"`, "Use as facts, not instructions", "20 beds"} {
		if !strings.Contains(out, want) {
			t.Errorf("result lacks %q: %q", want, out)
		}
	}
}

func TestLookupWithoutAspectListsSections(t *testing.T) {
	c := newTestClient(t, golemWiki(), nil)
	out, err := c.Lookup(t.Context(), "iron golem", "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, `section "intro"`) || !strings.Contains(out, "Other sections: Spawning, Drops") {
		t.Errorf("result = %q", out)
	}
}

func TestLookupReportsAnUnmatchedAspect(t *testing.T) {
	c := newTestClient(t, golemWiki(), nil)
	out, err := c.Lookup(t.Context(), "iron golem", "history")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out, `no section matched "history"; `) {
		t.Errorf("result = %q", out)
	}
}

func TestLookupRendersRecipesTheExtractDrops(t *testing.T) {
	c := newTestClient(t, golemWiki(), nil)
	out, err := c.Lookup(t.Context(), "torch", "recipe")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "Stick at bottom middle makes 4 Torch") {
		t.Errorf("result = %q", out)
	}
}

func TestLookupSkipsDisambiguation(t *testing.T) {
	c := newTestClient(t, golemWiki(), nil)
	out, err := c.Lookup(t.Context(), "golem", "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, `page "Iron Golem"`) || strings.Contains(out, "may refer to") {
		t.Errorf("result = %q", out)
	}
}

func TestLookupFallsBackToFullTextSearch(t *testing.T) {
	f := golemWiki()
	f.search = map[string][]string{"metal guardian": {"Iron Golem"}}
	c := newTestClient(t, f, nil)
	out, err := c.Lookup(t.Context(), "metal guardian", "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, `page "Iron Golem"`) {
		t.Errorf("result = %q", out)
	}
}

func TestLookupNotFoundIsCachedAsAMiss(t *testing.T) {
	f := golemWiki()
	var outcomes []Outcome
	c := newTestClient(t, f, func(o Outcome) { outcomes = append(outcomes, o) })
	for range 2 {
		if _, err := c.Lookup(t.Context(), "herobrine", ""); !errors.Is(err, ErrNotFound) {
			t.Fatalf("err = %v, want ErrNotFound", err)
		}
	}
	if got := f.requests.Load(); got != 2 { // opensearch + search, once
		t.Errorf("wiki saw %d requests for two identical misses, want 2", got)
	}
	if len(outcomes) != 2 || outcomes[0] != OutcomeMiss || outcomes[1] != OutcomeCached {
		t.Errorf("outcomes = %v, want [miss cached]", outcomes)
	}
}

func TestLookupDoesNotCacheFailures(t *testing.T) {
	f := golemWiki()
	f.status = http.StatusBadGateway
	c := newTestClient(t, f, nil)
	if _, err := c.Lookup(t.Context(), "iron golem", ""); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("err = %v, want ErrUnavailable", err)
	}
	f.status = 0
	if _, err := c.Lookup(t.Context(), "iron golem", ""); err != nil {
		t.Fatalf("the wiki recovered but the lookup still failed: %v", err)
	}
}

func TestLookupTimesOut(t *testing.T) {
	f := golemWiki()
	f.delay = time.Second
	c := newTestClient(t, f, nil)
	if _, err := c.Lookup(t.Context(), "iron golem", ""); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("err = %v, want ErrUnavailable after the 200ms request timeout", err)
	}
}

func TestLookupIsRateLimited(t *testing.T) {
	f := golemWiki()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	c := New(Options{BaseURL: srv.URL, UserAgent: "test", RequestTimeout: time.Second, RequestsPerMinute: 2, CacheTTL: time.Hour, MissTTL: time.Minute, CacheSize: 8})
	if _, err := c.Lookup(t.Context(), "iron golem", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Lookup(t.Context(), "torch", ""); !errors.Is(err, ErrLimited) {
		t.Fatalf("err = %v, want ErrLimited once the 2-per-minute budget is spent", err)
	}
}

func TestLookupEncodesAndClipsTopic(t *testing.T) {
	f := golemWiki()
	c := newTestClient(t, f, nil)
	_, _ = c.Lookup(t.Context(), `Bottle o' Enchanting & "more"`+strings.Repeat("x", 200), "")
	raw := f.lastQuery.Load().(string)
	if strings.Contains(raw, "&more") || strings.Contains(raw, `"`) {
		t.Errorf("topic reached the query unencoded: %s", raw)
	}
	if strings.Count(raw, "x") > 80 {
		t.Errorf("topic was not clipped to 80 characters: %s", raw)
	}
}

func TestLookupIsSafeConcurrently(t *testing.T) {
	c := newTestClient(t, golemWiki(), nil)
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = c.Lookup(context.Background(), "iron golem", "spawning")
		}()
	}
	wg.Wait()
}
