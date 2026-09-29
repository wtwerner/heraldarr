package app_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wtwerner/heraldarr/internal/app"
	"github.com/wtwerner/heraldarr/internal/batcher"
	"github.com/wtwerner/heraldarr/internal/config"
	"github.com/wtwerner/heraldarr/internal/domain"
	"github.com/wtwerner/heraldarr/internal/notify/discord"
	"github.com/wtwerner/heraldarr/internal/source/arr"
	"github.com/wtwerner/heraldarr/internal/store"
	"github.com/wtwerner/heraldarr/internal/testkit"
)

type rt func(imdbID, title string) string

func (f rt) RottenTomatoes(_ context.Context, imdbID, title string) string { return f(imdbID, title) }

// capture is a Notifier that records what it was asked to post.
type capture struct {
	mu    sync.Mutex
	posts []post
	fail  error
	after func(n int) // called after the n-th successful post
}

type post struct {
	dest    domain.Destination
	layouts []domain.Layout
}

func (c *capture) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.posts)
}

func (c *capture) Post(ctx context.Context, d domain.Destination, l []domain.Layout) (domain.PostResult, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return domain.PostResult{}, err // like the real notifier's HTTP request
	}
	if c.fail != nil {
		return domain.PostResult{}, c.fail
	}
	c.posts = append(c.posts, post{d, l})
	if c.after != nil {
		c.after(len(c.posts))
	}
	return domain.PostResult{Layout: l[0].Name, MessageID: "1"}, nil
}

var timing = batcher.Config{
	QuietEpisodes: 5 * time.Minute, QuietBacklog: 30 * time.Minute, QuietMovies: 5 * time.Minute,
	MaxHold: 4 * time.Hour, FollowingWindow: 14 * 24 * time.Hour, Reannounce: 30 * 24 * time.Hour,
	RetryInterval: 2 * time.Minute, RetryMax: 30, MediaWait: 3 * time.Minute,
}

type env struct {
	app   *app.App
	srv   http.Handler
	clock *testkit.Clock
	out   *capture
	media *media
	store *recordingStore
	sc    *testkit.Scenario
	exp   *testkit.Expected
}

// recordingStore is the real SQLite store, keeping a copy of the history it's asked to write.
type recordingStore struct {
	domain.Store
	mu      sync.Mutex
	history []domain.HistoryEntry
}

func (r *recordingStore) AppendHistory(ctx context.Context, e domain.HistoryEntry) error {
	r.mu.Lock()
	r.history = append(r.history, e)
	r.mu.Unlock()
	return r.Store.AppendHistory(ctx, e)
}

// media wraps the scenario's media server: it records scan requests and can be made to fail.
type media struct {
	testkit.Media
	mu               sync.Mutex
	scans            []string
	scanErr, findErr error
}

func (m *media) Scan(_ context.Context, path string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.scans = append(m.scans, path)
	return m.scanErr
}

func (m *media) Find(ctx context.Context, guids []string, path string, kind domain.Kind, fresh bool) (*domain.MediaItem, error) {
	if m.findErr != nil {
		return nil, m.findErr
	}
	return m.Media.Find(ctx, guids, path, kind, fresh)
}

func newEnv(t *testing.T, name string, withMedia bool) *env {
	t.Helper()
	sc, exp := testkit.Load(t, name)
	st, err := store.Open(filepath.Join(t.TempDir(), "heraldarr.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ctx := context.Background()
	if err := st.MarkPosted(ctx, sc.Posted, sc.Now); err != nil {
		t.Fatal(err)
	}
	clock, out, rec := testkit.NewClock(sc.Now), &capture{}, &recordingStore{Store: st}
	sources := map[string]app.Source{}
	for n, s := range testkit.Sources {
		sources[n] = app.Source{
			Kind: s.Kind, Arr: testkit.Arr{Sc: sc}, Style: s.Style,
			Destination: domain.Destination{Name: n + "-dest", WebhookURL: "https://discord.invalid/" + n},
		}
	}
	d := app.Deps{
		Clock: clock, Store: rec, Notifier: out, RT: rt(testkit.RottenTomatoes(sc)), Sources: sources,
		Timing: timing, DigestFrom: 4, WaitChecks: 4, Auth: &app.BasicAuth{Username: "u", Password: "p"},
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	var m *media
	if withMedia {
		m = &media{Media: testkit.Media{Sc: sc}}
		d.Media = m
	}
	a := app.New(d)
	return &env{app: a, srv: a.Handler(), clock: clock, out: out, media: m, store: rec, sc: sc, exp: exp}
}

func (e *env) send(t *testing.T) {
	t.Helper()
	for i, ev := range e.sc.Events {
		req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/hook/"+ev.Source, bytes.NewReader(ev.Payload))
		req.SetBasicAuth("u", "p")
		rec := httptest.NewRecorder()
		e.srv.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("event %d: HTTP %d %s", i, rec.Code, rec.Body)
		}
	}
}

// Webhooks in over HTTP, a forced flush through the real batcher, renderer and SQLite store, and
// every post must equal the reference implementation's, layout for layout.
func TestPipelineParity(t *testing.T) {
	for _, name := range testkit.Names(t) {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t, name, true)
			e.send(t)
			e.app.Flush(context.Background(), true)
			if len(e.out.posts) != len(e.exp.Posts) {
				t.Fatalf("got %d posts, want %d", len(e.out.posts), len(e.exp.Posts))
			}
			for i, p := range e.out.posts {
				want := e.exp.Posts[i]
				if p.dest.Name != want.Source+"-dest" {
					t.Errorf("post %d went to %s, want %s", i, p.dest.Name, want.Source+"-dest")
				}
				testkit.EqualJSON(t, "v2", p.layouts[0].Body, want.V2)
				testkit.EqualJSON(t, "embed", p.layouts[1].Body, want.Embed)
				testkit.EqualJSON(t, "embed_inline", p.layouts[2].Body, want.EmbedInline)
			}
			// Everything posted: nothing left, and a replay of the same webhooks queues nothing.
			e.send(t)
			e.app.Flush(context.Background(), true)
			if len(e.out.posts) != len(e.exp.Posts) {
				t.Errorf("replayed webhooks were announced again (%d posts)", len(e.out.posts))
			}
		})
	}
}

// Without force, a batch waits for its quiet window, then for the media server, then posts
// without the deep link once the checks run out.
func TestWaitsForQuietThenMediaServer(t *testing.T) {
	e := newEnv(t, "movie_not_in_plex", true)
	ctx := context.Background()
	e.send(t)
	e.app.Flush(ctx, false)
	if len(e.out.posts) != 0 {
		t.Fatal("posted inside the quiet window")
	}
	e.clock.Advance(timing.QuietMovies)
	for check := 1; check <= 4; check++ {
		e.app.Flush(ctx, false)
		if len(e.out.posts) != 0 {
			t.Fatalf("posted before media check %d ran out", check)
		}
		e.clock.Advance(timing.MediaWait)
	}
	e.app.Flush(ctx, false)
	if len(e.out.posts) != 1 {
		t.Fatalf("got %d posts after the media checks ran out, want 1", len(e.out.posts))
	}
	if len(e.media.scans) != 1 {
		t.Errorf("scan requests = %v, want one (on the first check)", e.media.scans)
	}
	if links := plexLinks(t, e.out.posts); links != 0 {
		t.Errorf("card has %d media-server links, want none", links)
	}
}

// As in the reference's plex_lookup, any media server error means no deep links on the card at
// all, even for items it did find, and the post goes out right away.
func TestMediaServerErrors(t *testing.T) {
	for _, tc := range []struct {
		name string
		fail func(*media)
	}{
		{"scan request fails", func(m *media) { m.scanErr = errors.New("scan refused") }},
		{"lookup fails", func(m *media) { m.findErr = errors.New("plex down") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t, "movie_three", true) // two of the three movies are in the library
			ctx := context.Background()
			e.send(t)
			tc.fail(e.media)
			e.clock.Advance(timing.QuietMovies)
			e.app.Flush(ctx, false)
			if len(e.out.posts) != 3 {
				t.Fatalf("got %d posts, want all 3 right away", len(e.out.posts))
			}
			if links := plexLinks(t, e.out.posts); links != 0 {
				t.Errorf("%d media-server links after an error, want none", links)
			}
		})
	}
}

func plexLinks(t *testing.T, posts []post) int {
	t.Helper()
	n := 0
	for _, p := range posts {
		b, err := json.Marshal(p.layouts[0].Body)
		if err != nil {
			t.Fatal(err)
		}
		n += strings.Count(string(b), "app.plex.tv")
	}
	return n
}

// Discord unreachable: nothing is lost, and it goes out on a later flush.
func TestRetriesAfterFailure(t *testing.T) {
	e := newEnv(t, "movie_single", true)
	ctx := context.Background()
	e.send(t)
	e.out.fail = context.DeadlineExceeded
	e.app.Flush(ctx, true)
	e.out.fail = nil
	e.clock.Advance(timing.QuietMovies) // past both the retry delay and the quiet window
	e.app.Flush(ctx, false)
	if len(e.out.posts) != 1 {
		t.Fatalf("got %d posts after recovery, want 1", len(e.out.posts))
	}
}

func TestAuthAndEndpoints(t *testing.T) {
	e := newEnv(t, "movie_single", false)
	for _, tc := range []struct {
		method, path string
		auth         bool
		want         int
	}{
		{http.MethodPost, "/hook/sonarr", false, http.StatusUnauthorized},
		{http.MethodGet, "/pending", false, http.StatusUnauthorized},
		{http.MethodPost, "/flush", false, http.StatusUnauthorized},
		{http.MethodGet, "/health", false, http.StatusOK},
		{http.MethodGet, "/pending", true, http.StatusOK},
		{http.MethodPost, "/hook/nope", true, http.StatusNotFound},
		{http.MethodPost, "/flush", true, http.StatusAccepted},
	} {
		req := httptest.NewRequestWithContext(context.Background(), tc.method, tc.path, bytes.NewReader([]byte(`{}`)))
		if tc.auth {
			req.SetBasicAuth("u", "p")
		}
		rec := httptest.NewRecorder()
		e.srv.ServeHTTP(rec, req)
		if rec.Code != tc.want {
			t.Errorf("%s %s auth=%v: HTTP %d, want %d", tc.method, tc.path, tc.auth, rec.Code, tc.want)
		}
	}
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/hook/sonarr", bytes.NewReader([]byte(`{"eventType":"Test"}`)))
	req.SetBasicAuth("u", "p")
	rec := httptest.NewRecorder()
	e.srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("Test event: HTTP %d, want 200 (the *arr Test button must succeed)", rec.Code)
	}
}

// A flush that starts after shutdown began posts nothing and loses nothing.
func TestFlushAfterShutdown(t *testing.T) {
	e := newEnv(t, "movie_single", true)
	e.send(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	e.app.Flush(ctx, true)
	e.app.Flush(context.Background(), true)
	if len(e.out.posts) != 1 {
		t.Fatalf("got %d posts after restart, want 1", len(e.out.posts))
	}
}

// A shutdown in the middle of a batch keeps the cards that went out, isn't a failed try, and the
// rest go out on the next flush without repeating the first.
func TestShutdownMidDelivery(t *testing.T) {
	e := newEnv(t, "movie_three", true)
	e.send(t)
	ctx, cancel := context.WithCancel(context.Background())
	e.out.after = func(n int) {
		if n == 1 {
			cancel() // SIGTERM right after the first card went out
		}
	}
	e.app.Flush(ctx, true)
	if len(e.out.posts) != 1 {
		t.Fatalf("got %d posts before the shutdown, want 1", len(e.out.posts))
	}
	assertUntouched(t, e, "radarr:movies", 2)
	e.out.after = nil
	e.app.Flush(context.Background(), true) // restart (forced: one movie isn't in the library)
	if len(e.out.posts) != 3 {
		t.Fatalf("got %d posts in total, want 3 (the first not repeated)", len(e.out.posts))
	}
	titles := map[string]bool{}
	for _, p := range e.out.posts {
		b, _ := json.Marshal(p.layouts[0].Body)
		titles[string(b)] = true
	}
	if len(titles) != 3 {
		t.Error("a card was posted twice")
	}
}

// assertUntouched: the stored batch holds want items and carries no failed try or retry delay.
func assertUntouched(t *testing.T, e *env, key string, want int) {
	t.Helper()
	pending, err := e.app.Store.LoadBatches(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	b := pending[key]
	switch {
	case b == nil:
		t.Fatalf("batch %s is gone", key)
	case b.Len() != want:
		t.Errorf("batch %s holds %d items, want %d", key, b.Len(), want)
	case b.Tries != 0 || !b.NotBefore.IsZero():
		t.Errorf("shutdown counted as a failed try: tries=%d notBefore=%s", b.Tries, b.NotBefore)
	}
}

// Each card is recorded as posted as soon as it goes out, so a crash mid-batch can't repost it.
func TestLedgerPerCard(t *testing.T) {
	e := newEnv(t, "movie_three", true)
	e.send(t)
	checked := false
	e.out.after = func(n int) {
		if n != 2 {
			return
		}
		// Runs during the second post: the first must already be in the ledger.
		first := domain.MovieKey("radarr", 202)
		p, err := e.app.Store.PostedSince(context.Background(), []domain.ItemKey{first}, time.Time{})
		if err == nil {
			checked = true
		}
		if !p[first] {
			t.Error("first card not in the posted ledger before the second went out")
		}
	}
	e.app.Flush(context.Background(), true)
	if !checked || len(e.out.posts) != 3 {
		t.Fatalf("checked=%v posts=%d", checked, len(e.out.posts))
	}
}

// A batch whose source was removed from the config is a failed try, and never recorded as posted.
func TestUnconfiguredSource(t *testing.T) {
	e := newEnv(t, "movie_single", true)
	e.send(t)
	delete(e.app.Sources, "radarr")
	e.app.Flush(context.Background(), true)
	if len(e.out.posts) != 0 {
		t.Fatalf("posted %d cards for an unconfigured source", len(e.out.posts))
	}
	pending, _ := e.app.Store.LoadBatches(context.Background())
	if b := pending["radarr:movies"]; b == nil || b.Tries != 1 {
		t.Fatalf("want the batch kept with one failed try, got %+v", b)
	}
	key := domain.MovieKey("radarr", 201)
	if p, _ := e.app.Store.PostedSince(context.Background(), []domain.ItemKey{key}, time.Time{}); p[key] {
		t.Error("recorded as posted")
	}
}

// As in the reference, a series lookup failing for any reason but "deleted" is a failed try.
func TestSeriesLookupFailureRetries(t *testing.T) {
	e := newEnv(t, "tv_weekly_episode", true)
	e.send(t)
	delete(e.sc.Arr, "series/102") // the fake now answers "unreachable"
	e.app.Flush(context.Background(), true)
	if len(e.out.posts) != 0 {
		t.Fatal("posted although the series lookup failed")
	}
	pending, _ := e.app.Store.LoadBatches(context.Background())
	if b := pending["sonarr:102"]; b == nil || b.Tries != 1 {
		t.Fatalf("want a failed try, got %+v", b)
	}
}

// Every post is written to the history with its message ID.
func TestHistory(t *testing.T) {
	e := newEnv(t, "movie_three", true)
	e.send(t)
	e.app.Flush(context.Background(), true)
	if len(e.store.history) != 3 {
		t.Fatalf("history has %d entries, want 3", len(e.store.history))
	}
	for _, h := range e.store.history {
		if h.MessageID == "" || h.Source != "radarr" || h.Destination != "radarr-dest" || h.Items != 1 {
			t.Errorf("history entry %+v", h)
		}
	}
}

// A shutdown during one batch releases the other due batches untouched; they go out next time.
func TestShutdownReleasesOtherBatches(t *testing.T) {
	e := newEnv(t, "movie_single", true)
	uhd, _ := testkit.Load(t, "movie_4k_private")
	e.sc.Events = append(e.sc.Events, uhd.Events...) // a second batch: radarr4k:movies
	e.send(t)
	ctx, cancel := context.WithCancel(context.Background())
	e.out.after = func(int) { cancel() }
	e.app.Flush(ctx, true)
	if len(e.out.posts) != 1 {
		t.Fatalf("got %d posts before the shutdown, want 1", len(e.out.posts))
	}
	assertUntouched(t, e, "radarr:movies", 1) // batches go out in key order: radarr4k first
	e.out.after = nil
	e.clock.Advance(timing.QuietMovies)
	e.app.Flush(context.Background(), false)
	if len(e.out.posts) != 2 || e.out.posts[1].dest.Name == e.out.posts[0].dest.Name {
		t.Fatalf("the released batch didn't go out on the next flush: %d posts", len(e.out.posts))
	}
}

// Run flushes on start and whenever Kick asks (POST /flush), and returns when its context ends.
func TestRunAndKick(t *testing.T) {
	e := newEnv(t, "movie_single", true)
	e.send(t)
	ctx, cancel := context.WithCancel(context.Background())
	ran := make(chan struct{})
	go func() { e.app.Run(ctx); close(ran) }()
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/flush", http.NoBody)
	req.SetBasicAuth("u", "p")
	e.srv.ServeHTTP(httptest.NewRecorder(), req) // force: inside the quiet window
	deadline := time.Now().Add(5 * time.Second)
	for e.out.count() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if e.out.count() != 1 {
		t.Errorf("POST /flush: got %d posts, want 1", e.out.count())
	}
	cancel()
	select {
	case <-ran:
	case <-time.After(5 * time.Second):
		t.Fatal("Run didn't return after cancel")
	}
}

// As in the reference, a card Discord refuses in every layout is retried, not dropped.
func TestRefusedIsRetried(t *testing.T) {
	e := newEnv(t, "movie_single", true)
	ctx := context.Background()
	e.send(t)
	e.out.fail = &discord.RefusedError{Destination: "radarr-dest"}
	e.app.Flush(ctx, true)
	e.out.fail = nil
	e.clock.Advance(timing.QuietMovies)
	e.app.Flush(ctx, false)
	if len(e.out.posts) != 1 {
		t.Fatalf("got %d posts after Discord accepted again, want 1", len(e.out.posts))
	}
}

func TestPreview(t *testing.T) {
	e := newEnv(t, "movie_4k_private", true)
	ctx := context.Background()
	imp, _, err := arr.ParseWebhook("radarr4k", domain.KindMovie, e.sc.Events[0].Payload)
	if err != nil {
		t.Fatal(err)
	}
	public := &domain.Destination{Name: "friends", Public: true}
	if _, err := e.app.Preview(ctx, "radarr4k", []domain.Import{imp}, public); err == nil {
		t.Error("preview to a public destination must be refused")
	}
	layouts, err := e.app.Preview(ctx, "radarr4k", []domain.Import{imp}, nil)
	if err != nil || len(layouts) != 1 {
		t.Fatalf("preview = %d cards, %v", len(layouts), err)
	}
	testkit.EqualJSON(t, "preview v2", layouts[0][0].Body, e.exp.Posts[0].V2)
	private := &domain.Destination{Name: "private"}
	if _, err := e.app.Preview(ctx, "radarr4k", []domain.Import{imp}, private); err != nil || len(e.out.posts) != 1 {
		t.Fatalf("preview to private: %v, %d posts", err, len(e.out.posts))
	}
	if p, _ := e.app.Store.PostedSince(ctx, []domain.ItemKey{domain.MovieKey("radarr4k", imp.Movie.ID)}, time.Time{}); len(p) != 0 {
		t.Error("a preview was recorded as posted")
	}
}

func TestFromConfig(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	hook := write("hook", "https://discord.invalid/api/webhooks/1/x")
	xml := write("config.xml", "<Config><ApiKey>k</ApiKey></Config>")
	prefs := write("Preferences.xml", `<Preferences PlexOnlineToken="t"/>`)
	cfg, err := config.Parse([]byte(`
server: {data_dir: ` + filepath.Join(dir, "data") + `}
media_server: {url: "http://plex:32400", preferences_xml: ` + prefs + `}
sources:
  - {name: sonarr, kind: sonarr, url: "http://sonarr:8989", config_xml: ` + xml + `}
  - {name: radarr4k, kind: radarr, url: "http://radarr:7878", api_key: k}
destinations:
  - {name: tv, webhook_url: {file: ` + hook + `}, public: true, username: TV}
  - {name: private, webhook_url: {file: ` + hook + `}}
routes:
  - {sources: [sonarr], destination: tv}
  - {sources: [radarr4k], destination: private, style: {label: 4K, color: "#9B59B6", tech_details: true}}
`))
	if err != nil {
		t.Fatal(err)
	}
	svc, err := app.FromConfig(cfg, "test", slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = svc.Close() }()
	tv, uhd := svc.Sources["sonarr"], svc.Sources["radarr4k"]
	if tv.Kind != domain.KindTV || !tv.Destination.Public || tv.Destination.Username != "TV" || tv.Style.TechDetails {
		t.Errorf("sonarr source: %+v", tv)
	}
	if uhd.Kind != domain.KindMovie || uhd.Destination.Public || uhd.Style != (domain.Style{Label: "4K", Color: 0x9B59B6, TechDetails: true}) {
		t.Errorf("radarr4k source: %+v", uhd)
	}
	if svc.Media == nil || svc.WaitChecks != 4 || svc.Timing.MediaWait != 3*time.Minute || svc.DigestFrom != 4 {
		t.Errorf("media/timing not wired: waitChecks=%d mediaWait=%s", svc.WaitChecks, svc.Timing.MediaWait)
	}
	if _, err := os.Stat(filepath.Join(dir, "data", "heraldarr.db")); err != nil {
		t.Errorf("store not created in data_dir: %v", err)
	}
}
