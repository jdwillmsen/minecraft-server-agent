package wiki

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
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
	redirects      map[string]string // requested title -> the title it redirects to
	status         int               // non-zero: answer every request with it
	// override answers a request itself when it returns true, for the
	// malformed and erroring responses no canned page produces.
	override  func(w http.ResponseWriter, q url.Values) bool
	delay     time.Duration // slow every request
	requests  atomic.Int64
	lastQuery atomic.Value // most recent raw query string
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
	if f.override != nil && f.override(w, q) {
		return
	}
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
		if to, ok := f.redirects[title]; ok {
			title = to
		}
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

func TestLookupDoesNotMatchAnAspectOnFillerWords(t *testing.T) {
	f := &fakeWiki{
		opensearch: map[string][]string{"emerald": {"Emerald"}},
		pages:      map[string]string{"Emerald": "An emerald is a rare mineral.\n\n== Trading to villagers ==\nVillagers buy and sell for emeralds."},
	}
	c := newTestClient(t, f, nil)
	out, err := c.Lookup(t.Context(), "emerald", "how to get")
	if err != nil {
		t.Fatal(err)
	}
	want := `no section matched "how to get"; ` + Format("Emerald", "intro", "An emerald is a rare mineral.", []string{"Trading to villagers"})
	if out != want {
		t.Errorf("result = %q, want %q", out, want)
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

func TestLookupDoesNotFollowAnHTTPRedirect(t *testing.T) {
	elsewhere := golemWiki()
	other := httptest.NewServer(elsewhere)
	t.Cleanup(other.Close)
	f := golemWiki()
	f.override = func(w http.ResponseWriter, _ url.Values) bool {
		w.Header().Set("Location", other.URL+"/api.php")
		w.WriteHeader(http.StatusFound)
		return true
	}
	c := newTestClient(t, f, nil)
	if _, err := c.Lookup(t.Context(), "iron golem", ""); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("err = %v, want ErrUnavailable for a redirect", err)
	}
	if got := elsewhere.requests.Load(); got != 0 {
		t.Errorf("the redirect target saw %d requests, want 0", got)
	}
}

// failsOnce answers the first page request with bad, then behaves.
func failsOnce(bad func(w http.ResponseWriter)) func(http.ResponseWriter, url.Values) bool {
	var done atomic.Bool
	return func(w http.ResponseWriter, q url.Values) bool {
		if q.Get("prop") != "extracts|pageprops" || done.Swap(true) {
			return false
		}
		bad(w)
		return true
	}
}

func TestLookupTreatsAnAPIErrorAsUnavailableAndDoesNotCacheIt(t *testing.T) {
	for name, bad := range map[string]func(w http.ResponseWriter){
		"error body": func(w http.ResponseWriter) {
			_, _ = w.Write([]byte(`{"error":{"code":"maxlag","info":"Waiting for a database server"}}`))
		},
		"error header": func(w http.ResponseWriter) {
			w.Header().Set("MediaWiki-API-Error", "readonly")
			_, _ = w.Write([]byte(`{}`))
		},
		"no pages": func(w http.ResponseWriter) {
			_, _ = w.Write([]byte(`{"query":{"pages":{}}}`))
		},
		"no query": func(w http.ResponseWriter) {
			_, _ = w.Write([]byte(`{"batchcomplete":""}`))
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := golemWiki()
			f.override = failsOnce(bad)
			var outcomes []Outcome
			c := newTestClient(t, f, func(o Outcome) { outcomes = append(outcomes, o) })
			if _, err := c.Lookup(t.Context(), "iron golem", ""); !errors.Is(err, ErrUnavailable) {
				t.Fatalf("err = %v, want ErrUnavailable", err)
			}
			out, err := c.Lookup(t.Context(), "iron golem", "")
			if err != nil || !strings.Contains(out, `page "Iron Golem"`) {
				t.Fatalf("the wiki recovered but the lookup returned %q, %v", out, err)
			}
			if len(outcomes) != 2 || outcomes[0] != OutcomeError || outcomes[1] != OutcomeHit {
				t.Errorf("outcomes = %v, want [error hit]", outcomes)
			}
		})
	}
}

func TestLookupFallsBackToSearchOnAShortOpensearchAnswer(t *testing.T) {
	f := golemWiki()
	f.search = map[string][]string{"iron golem": {"Iron Golem"}}
	f.override = func(w http.ResponseWriter, q url.Values) bool {
		if q.Get("action") != "opensearch" {
			return false
		}
		_, _ = w.Write([]byte(`["iron golem"]`))
		return true
	}
	c := newTestClient(t, f, nil)
	out, err := c.Lookup(t.Context(), "iron golem", "")
	if err != nil || !strings.Contains(out, `page "Iron Golem"`) {
		t.Fatalf("result = %q, %v", out, err)
	}
}

func TestLookupFallsBackToTheIntroWhenASectionIsEmpty(t *testing.T) {
	for name, parse := range map[string]string{
		"no recipe in the wikitext": `{"parse":{"wikitext":{"*":"== Obtaining ==\n=== Crafting ===\nSee elsewhere."}}}`,
		"no parse key":              `{}`,
	} {
		t.Run(name, func(t *testing.T) {
			f := golemWiki()
			f.override = func(w http.ResponseWriter, q url.Values) bool {
				if q.Get("action") != "parse" {
					return false
				}
				_, _ = w.Write([]byte(parse))
				return true
			}
			c := newTestClient(t, f, nil)
			out, err := c.Lookup(t.Context(), "torch", "recipe")
			if err != nil {
				t.Fatal(err)
			}
			want := `no section matched "recipe"; ` + Format("Torch", "intro", "A torch is a light source.", []string{"Obtaining"})
			if out != want {
				t.Errorf("result = %q\nwant     %q", out, want)
			}
		})
	}
}

func TestLookupReportsTheLimitOnHTTP429(t *testing.T) {
	f := golemWiki()
	f.status = http.StatusTooManyRequests
	var outcomes []Outcome
	c := newTestClient(t, f, func(o Outcome) { outcomes = append(outcomes, o) })
	if _, err := c.Lookup(t.Context(), "iron golem", ""); !errors.Is(err, ErrLimited) {
		t.Fatalf("err = %v, want ErrLimited", err)
	}
	if len(outcomes) != 1 || outcomes[0] != OutcomeLimited {
		t.Errorf("outcomes = %v, want [limited]", outcomes)
	}
}

func TestLookupObservesEachOutcome(t *testing.T) {
	f := golemWiki()
	var outcomes []Outcome
	c := newTestClient(t, f, func(o Outcome) { outcomes = append(outcomes, o) })
	if _, err := c.Lookup(t.Context(), "iron golem", ""); err != nil {
		t.Fatal(err)
	}
	f.status = http.StatusBadGateway
	_, _ = c.Lookup(t.Context(), "torch", "")
	f.status = http.StatusTooManyRequests
	_, _ = c.Lookup(t.Context(), "golem", "")
	want := []Outcome{OutcomeHit, OutcomeError, OutcomeLimited}
	if !slices.Equal(outcomes, want) {
		t.Errorf("outcomes = %v, want %v", outcomes, want)
	}
}

func TestLookupFollowsAPageRedirect(t *testing.T) {
	f := golemWiki()
	f.opensearch["villager golem"] = []string{"Villager Golem"}
	f.redirects = map[string]string{"Villager Golem": "Iron Golem"}
	c := newTestClient(t, f, nil)
	out, err := c.Lookup(t.Context(), "villager golem", "spawning")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, `page "Iron Golem"`) || !strings.Contains(out, "20 beds") {
		t.Errorf("result = %q, want the page the redirect resolved to", out)
	}
}
