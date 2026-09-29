package app_test

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/wtwerner/heraldarr/internal/app"
	"github.com/wtwerner/heraldarr/internal/batcher"
	"github.com/wtwerner/heraldarr/internal/domain"
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
}

type post struct {
	dest    domain.Destination
	layouts []domain.Layout
}

func (c *capture) Post(_ context.Context, d domain.Destination, l []domain.Layout) (domain.PostResult, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.fail != nil {
		return domain.PostResult{}, c.fail
	}
	c.posts = append(c.posts, post{d, l})
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
			Name: n, Kind: s.Kind, Arr: testkit.Arr{Sc: sc}, Style: s.Style,
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

// A shutdown during delivery neither counts as a failed try nor loses the batch.
func TestShutdownMidDelivery(t *testing.T) {
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
