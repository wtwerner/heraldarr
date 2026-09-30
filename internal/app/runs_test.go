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
	q.mu.Lock()
	defer q.mu.Unlock()
	q.queue = nil
	for _, id := range eps {
		q.queue = append(q.queue, domain.QueueItem{EpisodeID: id, Season: 3})
	}
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

func newRunEnv(t *testing.T, mode string, edit bool) *runEnv {
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
	body := fmt.Sprintf(`{"eventType":"Download","series":{"id":7,"title":"The Lantern Keepers","year":2004,
		"path":"/tv/The Lantern Keepers","tvdbId":7007},"episodes":[{"id":%d,"seasonNumber":3,"episodeNumber":%d,
		"title":"Chapter %d","airDateUtc":%q}],"episodeFile":{"quality":"WEBDL-1080p"}}`,
		id, number, number, aired.Format(time.RFC3339))
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
		e.arr.set(append([]int{id}, queueAfter(id)...)...) // still listed while it imports
		e.clock.Advance(40 * time.Minute)                  // longer apart than any quiet window
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
