package app_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wtwerner/heraldarr/internal/app"
	"github.com/wtwerner/heraldarr/internal/batcher"
	"github.com/wtwerner/heraldarr/internal/domain"
	"github.com/wtwerner/heraldarr/internal/store"
	"github.com/wtwerner/heraldarr/internal/testkit"
)

// queueArr is a Sonarr whose download queue the test controls. The series itself is gone from
// the API, so cards come from the webhook data alone.
type queueArr struct {
	mu    sync.Mutex
	queue []domain.QueueItem
	err   error
}

func (q *queueArr) set(eps ...int) {
	var items []domain.QueueItem
	for _, id := range eps {
		items = append(items, domain.QueueItem{EpisodeID: id, Season: 3, Aired: aired2004})
	}
	q.setItems(items...)
}

func (q *queueArr) setItems(items ...domain.QueueItem) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.queue = items
}

func (q *queueArr) Series(context.Context, int) (*domain.SeriesDetail, error) {
	return nil, domain.ErrNotFound
}

func (q *queueArr) Movie(context.Context, int) (*domain.MovieDetail, error) {
	return nil, domain.ErrNotFound
}

func (q *queueArr) Queue(context.Context, int) ([]domain.QueueItem, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	return append([]domain.QueueItem(nil), q.queue...), q.err
}

type runEnv struct {
	app   *app.App
	srv   http.Handler
	clock *testkit.Clock
	out   *capture
	arr   *queueArr
}

var runNow = time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)

func newRunEnv(t *testing.T, mode string, edit bool, opts ...func(*batcher.Backlog)) *runEnv {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "heraldarr.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	tm := timing
	tm.FollowingEarly = 24 * time.Hour
	tm.Backlog = batcher.Backlog{
		Mode: mode, Edit: edit, Settle: 5 * time.Minute, Idle: 24 * time.Hour, MaxHold: 24 * time.Hour,
	}
	for _, o := range opts {
		o(&tm.Backlog)
	}
	e := &runEnv{clock: testkit.NewClock(runNow), out: &capture{}, arr: &queueArr{}}
	e.app = app.New(app.Deps{
		Clock: e.clock, Store: st, Notifier: e.out, RT: rt(func(string, string) string { return "" }),
		Sources: map[string]app.Source{"sonarr": {
			Kind: domain.KindTV, Arr: e.arr,
			Destination: domain.Destination{Name: "tv", WebhookURL: "https://discord.invalid/tv", Public: true},
		}},
		Timing: tm, DigestFrom: 4, WaitChecks: 4, Auth: &app.BasicAuth{Username: "u", Password: "p"},
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	e.srv = e.app.Handler()
	return e
}

// imported sends Sonarr's On Import webhook for one episode of season 3 of a show.
func (e *runEnv) imported(t *testing.T, id, number int, aired time.Time) {
	t.Helper()
	e.importedSeason(t, id, 3, number, aired)
}

func (e *runEnv) importedSeason(t *testing.T, id, season, number int, aired time.Time) {
	t.Helper()
	body := fmt.Sprintf(`{"eventType":"Download","series":{"id":7,"title":"The Lantern Keepers","year":2004,
		"path":"/tv/The Lantern Keepers","tvdbId":7007},"episodes":[{"id":%d,"seasonNumber":%d,"episodeNumber":%d,
		"title":"Chapter %d","airDateUtc":%q}],"episodeFile":{"quality":"WEBDL-1080p"}}`,
		id, season, number, number, aired.Format(time.RFC3339))
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/hook/sonarr", bytes.NewReader([]byte(body)))
	req.SetBasicAuth("u", "p")
	rec := httptest.NewRecorder()
	e.srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("webhook: HTTP %d %s", rec.Code, rec.Body)
	}
}

// settle lets the batch go quiet and flushes.
func (e *runEnv) settle() {
	e.clock.Advance(5 * time.Minute)
	e.app.Flush(context.Background(), false)
}

func v2Text(t *testing.T, body map[string]any) string {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

var aired2004 = time.Date(2004, 10, 5, 1, 0, 0, 0, time.UTC)

// The trickle that used to post a card per episode: one card when the first episode lands, then
// silent edits of that card as the rest arrive, saying how many are still on the way.
func TestLeadRunEditsOneCard(t *testing.T) {
	e := newRunEnv(t, batcher.Lead, true)
	e.arr.set(302, 303, 304, 305)
	e.imported(t, 301, 1, aired2004)
	e.settle()
	if len(e.out.posts) != 1 {
		t.Fatalf("%d posts after the first episode, want 1", len(e.out.posts))
	}
	if first := v2Text(t, e.out.posts[0].layouts[0].Body); !strings.Contains(first, "4 more on the way") {
		t.Errorf("first card doesn't say more are coming: %s", first)
	}

	for i, id := range []int{302, 303, 304, 305} {
		// Still listed while they import, and the run's first episode not yet cleared.
		e.arr.set(append([]int{301, id}, queueAfter(id)...)...)
		e.clock.Advance(40 * time.Minute) // longer apart than any quiet window
		e.imported(t, id, i+2, aired2004)
		e.settle()
	}
	if len(e.out.posts) != 1 {
		t.Fatalf("%d posts for one run, want 1", len(e.out.posts))
	}
	if len(e.out.edits) != 4 {
		t.Fatalf("%d edits, want 4", len(e.out.edits))
	}
	last := e.out.edits[3]
	if last.id != "1" || last.layout.Name != "v2" || last.dest.Name != "tv" {
		t.Errorf("edit went to %s %s layout %s", last.dest.Name, last.id, last.layout.Name)
	}
	if text := v2Text(t, last.layout.Body); !strings.Contains(text, "E01–E05") || strings.Contains(text, "on the way") {
		t.Errorf("final card: %s", text)
	}
}

func queueAfter(id int) []int {
	var out []int
	for n := id + 1; n <= 305; n++ {
		out = append(out, n)
	}
	return out
}

// A new episode of the same show while a run is in progress still gets its own card.
func TestLeadRunLeavesNewEpisodesAlone(t *testing.T) {
	e := newRunEnv(t, batcher.Lead, true)
	e.imported(t, 301, 1, aired2004)
	e.settle()
	e.imported(t, 900, 9, e.clock.Now().Add(-3*time.Hour))
	e.settle()
	if len(e.out.posts) != 2 || len(e.out.edits) != 0 {
		t.Fatalf("posts %d, edits %d; want 2 posts", len(e.out.posts), len(e.out.edits))
	}
}

// A card that can't be edited (deleted in Discord) stops being edited; later episodes are folded
// in quietly and never posted on their own. A transient failure is retried instead.
func TestLeadRunEditFailures(t *testing.T) {
	e := newRunEnv(t, batcher.Lead, true)
	e.imported(t, 301, 1, aired2004)
	e.settle()

	e.out.editErr = errors.New("discord: 502")
	e.imported(t, 302, 2, aired2004)
	e.settle()
	e.out.editErr = nil
	e.clock.Advance(timing.RetryInterval)
	e.app.Flush(context.Background(), false)
	if len(e.out.edits) != 2 {
		t.Fatalf("transient failure: %d edit attempts, want 2", len(e.out.edits))
	}

	e.out.editErr = fmt.Errorf("discord: message %w", domain.ErrNotFound)
	e.imported(t, 303, 3, aired2004)
	e.settle()
	e.imported(t, 304, 4, aired2004)
	e.settle()
	if len(e.out.posts) != 1 || len(e.out.edits) != 3 {
		t.Fatalf("after a deleted card: posts %d, edits %d; want 1 and 3", len(e.out.posts), len(e.out.edits))
	}
	// Folded in means announced: a re-import doesn't post either, not even after the run ended.
	e.imported(t, 304, 4, aired2004)
	e.settle()
	e.clock.Advance(25 * time.Hour)
	e.app.Flush(context.Background(), false)
	e.imported(t, 304, 4, aired2004)
	e.settle()
	if len(e.out.posts) != 1 {
		t.Fatalf("re-import posted: %d posts", len(e.out.posts))
	}
}

// edit: false folds later episodes in without touching Discord.
func TestLeadRunWithoutEdits(t *testing.T) {
	e := newRunEnv(t, batcher.Lead, false)
	for i := 1; i <= 3; i++ {
		e.imported(t, 300+i, i, aired2004)
		e.settle()
		e.clock.Advance(time.Hour)
	}
	if len(e.out.posts) != 1 || len(e.out.edits) != 0 {
		t.Fatalf("posts %d, edits %d; want 1 and 0", len(e.out.posts), len(e.out.edits))
	}
}

// Complete holds the card while Sonarr has more of the run queued, then posts it whole. A queue
// that can't be read doesn't hold anything up.
func TestCompleteRunWaitsForQueue(t *testing.T) {
	e := newRunEnv(t, batcher.Complete, true)
	e.arr.set(302, 303)
	e.imported(t, 301, 1, aired2004)
	e.settle()
	e.arr.set(303)
	e.imported(t, 302, 2, aired2004)
	e.settle()
	if len(e.out.posts) != 0 {
		t.Fatalf("posted with episodes still queued")
	}
	e.arr.set()
	e.imported(t, 303, 3, aired2004)
	e.settle()
	if len(e.out.posts) != 1 {
		t.Fatalf("%d posts once the queue emptied, want 1", len(e.out.posts))
	}
	if text := v2Text(t, e.out.posts[0].layouts[0].Body); !strings.Contains(text, "S03E03") || !strings.Contains(text, "S03E01") {
		t.Errorf("card: %s", text)
	}

	e = newRunEnv(t, batcher.Complete, true)
	e.arr.err = errors.New("sonarr down")
	e.imported(t, 301, 1, aired2004)
	e.settle()
	if len(e.out.posts) != 1 {
		t.Fatal("an unreadable queue held the card")
	}
}

// Discord refusing the edit (not a 404) also stops the edits: retrying the same card won't help.
func TestLeadRunEditRefused(t *testing.T) {
	e := newRunEnv(t, batcher.Lead, true)
	e.imported(t, 301, 1, aired2004)
	e.settle()
	e.out.editErr = fmt.Errorf("discord: %w", domain.ErrRefused)
	e.imported(t, 302, 2, aired2004)
	e.settle()
	e.imported(t, 303, 3, aired2004)
	e.settle()
	if len(e.out.posts) != 1 || len(e.out.edits) != 1 {
		t.Fatalf("posts %d, edits %d; want 1 and 1", len(e.out.posts), len(e.out.edits))
	}
}

// The edit uses the layout Discord accepted for the card, not the first one tried.
func TestLeadRunEditKeepsLayout(t *testing.T) {
	e := newRunEnv(t, batcher.Lead, true)
	e.out.layout = "embed"
	e.imported(t, 301, 1, aired2004)
	e.settle()
	e.imported(t, 302, 2, aired2004)
	e.settle()
	if len(e.out.edits) != 1 || e.out.edits[0].layout.Name != "embed" {
		t.Fatalf("edits: %+v", e.out.edits)
	}
}

// A run counts only its own back catalog as "on the way": per season, only its season, and
// never new episodes queued for the same show (they get their own card).
func TestRunQueueScope(t *testing.T) {
	yesterday := runNow.Add(-24 * time.Hour)
	queue := []domain.QueueItem{
		{EpisodeID: 302, Season: 3, Aired: aired2004},
		{EpisodeID: 401, Season: 4, Aired: aired2004},
		{EpisodeID: 402, Season: 4, Aired: aired2004},
		{EpisodeID: 901, Season: 9, Aired: yesterday},
	}
	for _, tc := range []struct {
		name      string
		perSeason bool
		want      string
	}{
		{"series", false, "3 more on the way"},
		{"season", true, "1 more on the way"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newRunEnv(t, batcher.Lead, true, func(b *batcher.Backlog) { b.PerSeason = tc.perSeason })
			e.arr.setItems(queue...)
			e.imported(t, 301, 1, aired2004)
			e.settle()
			if len(e.out.posts) != 1 {
				t.Fatalf("%d posts", len(e.out.posts))
			}
			if text := v2Text(t, e.out.posts[0].layouts[0].Body); !strings.Contains(text, tc.want) {
				t.Errorf("want %q in %s", tc.want, text)
			}
		})
	}
	// Complete per season doesn't wait for another season's downloads.
	e := newRunEnv(t, batcher.Complete, true, func(b *batcher.Backlog) { b.PerSeason = true })
	e.arr.setItems(queue[1:]...)
	e.imported(t, 301, 1, aired2004)
	e.settle()
	if len(e.out.posts) != 1 {
		t.Fatal("season 3 waited for season 4's downloads")
	}
}

// A forced flush posts a complete run's card even while episodes are still queued.
func TestCompleteRunForced(t *testing.T) {
	e := newRunEnv(t, batcher.Complete, true)
	e.arr.set(302)
	e.imported(t, 301, 1, aired2004)
	e.app.Flush(context.Background(), true)
	if len(e.out.posts) != 1 {
		t.Fatal("forced flush held the card")
	}
}

// /health, /pending and /metrics count a run with nothing waiting as a run, not as pending.
func TestRunEndpoints(t *testing.T) {
	e := newRunEnv(t, batcher.Lead, true, func(b *batcher.Backlog) { b.PerSeason = true })
	var health struct{ Pending, Runs int }
	get := func(path string) string {
		req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, path, nil)
		req.SetBasicAuth("u", "p")
		rec := httptest.NewRecorder()
		e.srv.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: HTTP %d", path, rec.Code)
		}
		return rec.Body.String()
	}
	e.imported(t, 301, 1, aired2004)
	if err := json.Unmarshal([]byte(get("/health")), &health); err != nil || health.Pending != 1 || health.Runs != 0 {
		t.Errorf("/health before the run's card: %+v %v", health, err)
	}
	e.settle()                                           // the run's card: nothing pending
	e.imported(t, 900, 9, e.clock.Now().Add(-time.Hour)) // a new episode, waiting
	if err := json.Unmarshal([]byte(get("/health")), &health); err != nil || health.Pending != 1 || health.Runs != 1 {
		t.Errorf("/health: %+v %v", health, err)
	}
	var pending []map[string]any
	if err := json.Unmarshal([]byte(get("/pending")), &pending); err != nil || len(pending) != 2 {
		t.Fatalf("/pending: %v %v", pending, err)
	}
	live, run := pending[0], pending[1] // same show: ordered by key
	if live["key"] != "sonarr:7" || live["backlog"] != nil || live["items"] != float64(1) {
		t.Errorf("live entry: %v", live)
	}
	if run["key"] != "sonarr:7:s3" || run["backlog"] != true || run["season"] != float64(3) ||
		run["announced"] != float64(1) || run["items"] != float64(0) {
		t.Errorf("run entry: %v", run)
	}
	if m := get("/metrics"); !strings.Contains(m, "\nheraldarr_pending_batches 1\n") {
		t.Errorf("/metrics pending:\n%s", m)
	}
}
