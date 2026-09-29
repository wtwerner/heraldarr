package app_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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
	sc    *testkit.Scenario
	exp   *testkit.Expected
}

func newEnv(t *testing.T, name string, media bool) *env {
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
	clock, out := testkit.NewClock(sc.Now), &capture{}
	sources := map[string]app.Source{}
	for n, s := range testkit.Sources {
		sources[n] = app.Source{
			Kind: s.Kind, Arr: testkit.Arr{Sc: sc}, Style: s.Style,
			Destination: domain.Destination{Name: n + "-dest", WebhookURL: "https://discord.invalid/" + n},
		}
	}
	d := app.Deps{
		Clock: clock, Store: st, Notifier: out, RT: rt(testkit.RottenTomatoes(sc)), Sources: sources,
		Timing: timing, DigestFrom: 4, WaitChecks: 4, Auth: &app.BasicAuth{Username: "u", Password: "p"},
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	if media {
		d.Media = testkit.Media{Sc: sc}
	}
	a := app.New(d)
	return &env{app: a, srv: a.Handler(), clock: clock, out: out, sc: sc, exp: exp}
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

// A shutdown before a flush starts releases every due batch untouched.
func TestShutdownBeforeFlush(t *testing.T) {
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
	e.out.after = nil
	e.app.Flush(context.Background(), true)
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
	pending, _ := e.app.Store.LoadBatches(context.Background())
	for _, b := range pending {
		if b.Tries != 0 {
			t.Errorf("shutdown counted as a failed try: %+v", b)
		}
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
