package enrich_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wtwerner/heraldarr/internal/domain"
	"github.com/wtwerner/heraldarr/internal/enrich"
	"github.com/wtwerner/heraldarr/internal/testkit"
)

var t0 = time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)

const day = 24 * time.Hour

// fakeWikidata answers SPARQL queries from a map of IMDb ID -> RT path. Change its behavior between
// requests with set.
type fakeWikidata struct {
	srv *httptest.Server

	mu      sync.Mutex
	pages   map[string]string
	status  int    // 0: 200
	body    string // "": a SPARQL result built from pages
	delay   time.Duration
	calls   int
	lastReq *http.Request
}

func newFake(t *testing.T, pages map[string]string) *fakeWikidata {
	t.Helper()
	f := &fakeWikidata{pages: pages}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeWikidata) set(change func(f *fakeWikidata)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	change(f)
}

func (f *fakeWikidata) requests() (int, *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls, f.lastReq
}

func (f *fakeWikidata) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.calls++
	f.lastReq = r.Clone(context.Background())
	pages, status, body, delay := f.pages, f.status, f.body, f.delay
	f.mu.Unlock()

	if delay > 0 {
		select {
		case <-time.After(delay):
		case <-r.Context().Done():
			return
		}
	}
	if status != 0 {
		w.WriteHeader(status)
	}
	if body != "" {
		_, _ = w.Write([]byte(body))
		return
	}
	q := r.URL.Query().Get("query")
	bindings := "[]"
	for id, p := range pages {
		if strings.Contains(q, `wdt:P345 "`+id+`"`) {
			bindings = fmt.Sprintf(`[{"rt": {"type": "literal", "value": %q}}]`, p)
		}
	}
	w.Header().Set("Content-Type", "application/sparql-results+json")
	_, _ = fmt.Fprintf(w, `{"head": {"vars": ["rt"]}, "results": {"bindings": %s}}`, bindings)
}

type rig struct {
	wd    *enrich.Wikidata
	fake  *fakeWikidata
	clock *testkit.Clock
	store *testkit.MemStore
}

func newRig(t *testing.T, pages map[string]string) *rig {
	t.Helper()
	r := &rig{fake: newFake(t, pages), clock: testkit.NewClock(t0), store: testkit.NewMemStore()}
	r.wd = enrich.New(r.store, r.clock)
	r.wd.Endpoint = r.fake.srv.URL + "/sparql"
	return r
}

func (r *rig) rt(t *testing.T, imdbID, title string) string {
	t.Helper()
	return r.wd.RottenTomatoes(t.Context(), imdbID, title)
}

func search(title string) string {
	return testkit.RottenTomatoes(&testkit.Scenario{})("", title)
}

func TestExactPage(t *testing.T) {
	r := newRig(t, map[string]string{"tt0000001": "m/example_movie"})
	if got, want := r.rt(t, "tt0000001", "Example Movie"), "https://www.rottentomatoes.com/m/example_movie"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}

	_, req := r.fake.requests()
	if req.Method != http.MethodGet || req.URL.Path != "/sparql" {
		t.Errorf("request %s %s, want GET /sparql", req.Method, req.URL.Path)
	}
	q := req.URL.Query()
	if want := `SELECT ?rt WHERE { ?s wdt:P345 "tt0000001" . ?s wdt:P1258 ?rt } LIMIT 1`; q.Get("query") != want {
		t.Errorf("query %q, want %q", q.Get("query"), want)
	}
	if q.Get("format") != "json" {
		t.Errorf("format %q, want json", q.Get("format"))
	}
	if ua := req.Header.Get("User-Agent"); !strings.HasPrefix(ua, "heraldarr") || !strings.Contains(ua, "https://") {
		t.Errorf("User-Agent %q: want a descriptive one naming heraldarr with a contact URL", ua)
	}
}

func TestSeasonTrimmedToShow(t *testing.T) {
	r := newRig(t, map[string]string{"tt0000002": "tv/example_show/s01", "tt0000003": "tv/example_show/s12"})
	for _, id := range []string{"tt0000002", "tt0000003"} {
		if got, want := r.rt(t, id, "Example Show"), "https://www.rottentomatoes.com/tv/example_show"; got != want {
			t.Errorf("%s: got %q, want %q", id, got, want)
		}
	}
}

func TestSeasonLookalikeKept(t *testing.T) {
	r := newRig(t, map[string]string{"tt0000004": "m/s01_the_movie", "tt0000005": "tv/example_show/s01e02"})
	for id, want := range map[string]string{
		"tt0000004": "https://www.rottentomatoes.com/m/s01_the_movie",
		"tt0000005": "https://www.rottentomatoes.com/tv/example_show/s01e02",
	} {
		if got := r.rt(t, id, "X"); got != want {
			t.Errorf("%s: got %q, want %q", id, got, want)
		}
	}
}

func TestNoIMDbIDIsSearchWithoutLookup(t *testing.T) {
	r := newRig(t, nil)
	if got, want := r.rt(t, "", "Example Show"), search("Example Show"); got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
	if n, _ := r.fake.requests(); n != 0 {
		t.Errorf("%d Wikidata requests, want 0", n)
	}
}

func TestSearchEncodingMatchesTestkit(t *testing.T) {
	r := newRig(t, nil)
	for _, title := range []string{"Example Show", "Café & Friends: Part 2", "100% Fictional?", "Ünïcode/Title #1"} {
		got := r.rt(t, "tt0000009", title)
		if want := search(title); got != want {
			t.Errorf("%q: got %q, want %q", title, got, want)
		}
	}
	if got, want := search("A B&C"), "https://www.rottentomatoes.com/search?search=A+B%26C"; got != want {
		t.Errorf("testkit search %q, want %q", got, want)
	}
}

func TestHitCachedForever(t *testing.T) {
	r := newRig(t, map[string]string{"tt0000001": "m/example_movie"})
	want := "https://www.rottentomatoes.com/m/example_movie"
	r.rt(t, "tt0000001", "Example Movie")
	r.fake.set(func(f *fakeWikidata) { f.pages = nil }) // Wikidata forgets it; the cache must not
	r.clock.Advance(3 * 365 * day)
	if got := r.rt(t, "tt0000001", "Example Movie"); got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	if n, _ := r.fake.requests(); n != 1 {
		t.Errorf("%d Wikidata requests, want 1", n)
	}

	val, at, ok, err := r.store.CacheGet(t.Context(), "rt", "tt0000001")
	if err != nil || !ok || string(val) != "m/example_movie" || !at.Equal(t0) {
		t.Errorf("cache entry = %q %v %v %v, want m/example_movie at %v", val, at, ok, err, t0)
	}
}

func TestSeasonTrimmedBeforeCaching(t *testing.T) {
	r := newRig(t, map[string]string{"tt0000002": "tv/example_show/s03"})
	r.rt(t, "tt0000002", "Example Show")
	val, _, ok, err := r.store.CacheGet(t.Context(), "rt", "tt0000002")
	if err != nil || !ok || string(val) != "tv/example_show" {
		t.Errorf("cache entry = %q %v %v, want tv/example_show", val, ok, err)
	}
}

func TestMissRetriedAfterSevenDays(t *testing.T) {
	r := newRig(t, nil)
	want := search("Example Movie")
	if got := r.rt(t, "tt0000007", "Example Movie"); got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
	val, _, ok, err := r.store.CacheGet(t.Context(), "rt", "tt0000007")
	if err != nil || !ok || len(val) != 0 {
		t.Fatalf("cache entry = %q %v %v, want an empty (miss) entry", val, ok, err)
	}

	r.fake.set(func(f *fakeWikidata) { f.pages = map[string]string{"tt0000007": "m/example_movie"} })
	r.clock.Advance(7*day - time.Second)
	if got := r.rt(t, "tt0000007", "Example Movie"); got != want {
		t.Errorf("within 7 days: got %q, want the cached miss %q", got, want)
	}
	if n, _ := r.fake.requests(); n != 1 {
		t.Errorf("within 7 days: %d Wikidata requests, want 1", n)
	}

	r.clock.Advance(time.Second)
	if got, want := r.rt(t, "tt0000007", "Example Movie"), "https://www.rottentomatoes.com/m/example_movie"; got != want {
		t.Errorf("after 7 days: got %q, want %q", got, want)
	}
	if n, _ := r.fake.requests(); n != 2 {
		t.Errorf("after 7 days: %d Wikidata requests, want 2", n)
	}
}

func TestErrorsReturnSearchAndAreNotCached(t *testing.T) {
	for name, set := range map[string]func(f *fakeWikidata){
		"server error": func(f *fakeWikidata) { f.status = http.StatusInternalServerError },
		"rate limited": func(f *fakeWikidata) { f.status = http.StatusTooManyRequests },
		"bad json":     func(f *fakeWikidata) { f.body = "<html>oops</html>" },
		"no results":   func(f *fakeWikidata) { f.body = `{"head": {}}` },
	} {
		t.Run(name, func(t *testing.T) {
			r := newRig(t, map[string]string{"tt0000001": "m/example_movie"})
			r.fake.set(set)
			if got, want := r.rt(t, "tt0000001", "Example Movie"), search("Example Movie"); got != want {
				t.Fatalf("got %q, want %q", got, want)
			}
			if _, _, ok, _ := r.store.CacheGet(t.Context(), "rt", "tt0000001"); ok {
				t.Fatal("error was cached")
			}
			r.fake.set(func(f *fakeWikidata) { f.status, f.body = 0, "" })
			if got, want := r.rt(t, "tt0000001", "Example Movie"), "https://www.rottentomatoes.com/m/example_movie"; got != want {
				t.Errorf("after recovery: got %q, want %q", got, want)
			}
		})
	}
}

func TestUnreachable(t *testing.T) {
	r := newRig(t, nil)
	r.fake.srv.Close()
	if got, want := r.rt(t, "tt0000001", "Example Movie"), search("Example Movie"); got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
	if _, _, ok, _ := r.store.CacheGet(t.Context(), "rt", "tt0000001"); ok {
		t.Error("error was cached")
	}
}

func TestTimeout(t *testing.T) {
	if got := enrich.New(testkit.NewMemStore(), testkit.NewClock(t0)).Timeout; got != 15*time.Second {
		t.Errorf("default timeout %v, want 15s", got)
	}
	r := newRig(t, map[string]string{"tt0000001": "m/example_movie"})
	r.fake.set(func(f *fakeWikidata) { f.delay = time.Minute })
	r.wd.Timeout = 50 * time.Millisecond
	start := time.Now()
	if got, want := r.rt(t, "tt0000001", "Example Movie"), search("Example Movie"); got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
	if el := time.Since(start); el > 5*time.Second {
		t.Errorf("took %v; the timeout wasn't applied", el)
	}
	if _, _, ok, _ := r.store.CacheGet(t.Context(), "rt", "tt0000001"); ok {
		t.Error("timeout was cached")
	}
}

func TestCanceledContext(t *testing.T) {
	r := newRig(t, map[string]string{"tt0000001": "m/example_movie"})
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if got, want := r.wd.RottenTomatoes(ctx, "tt0000001", "Example Movie"), search("Example Movie"); got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
	if _, _, ok, _ := r.store.CacheGet(t.Context(), "rt", "tt0000001"); ok {
		t.Error("cancellation was cached")
	}
}

func TestQuotesInIMDbIDAreEscaped(t *testing.T) {
	r := newRig(t, nil)
	r.rt(t, `tt1" } UNION { ?s ?p ?o`, "X")
	_, req := r.fake.requests()
	q := req.URL.Query().Get("query")
	if want := `wdt:P345 "tt1\" } UNION { ?s ?p ?o" .`; !strings.Contains(q, want) {
		t.Errorf("query %q: want the ID as one escaped literal", q)
	}
}

// brokenStore fails every cache call.
type brokenStore struct{ *testkit.MemStore }

var errDisk = errors.New("disk on fire")

func (brokenStore) CacheGet(context.Context, string, string) ([]byte, time.Time, bool, error) {
	return nil, time.Time{}, false, errDisk
}

func (brokenStore) CachePut(context.Context, string, string, []byte, time.Time) error {
	return errDisk
}

func TestStoreErrorsDontBlock(t *testing.T) {
	f := newFake(t, map[string]string{"tt0000001": "m/example_movie"})
	var store domain.Store = brokenStore{testkit.NewMemStore()}
	wd := enrich.New(store, testkit.NewClock(t0))
	wd.Endpoint = f.srv.URL
	if got, want := wd.RottenTomatoes(t.Context(), "tt0000001", "Example Movie"), "https://www.rottentomatoes.com/m/example_movie"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}
