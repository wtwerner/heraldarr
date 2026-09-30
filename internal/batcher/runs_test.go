package batcher_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/wtwerner/heraldarr/internal/batcher"
	"github.com/wtwerner/heraldarr/internal/domain"
	"github.com/wtwerner/heraldarr/internal/testkit"
)

var t0 = time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)

func runCfg(mode string, perSeason bool) batcher.Config {
	c := cfg
	c.FollowingEarly = 24 * time.Hour
	c.Backlog = batcher.Backlog{
		Mode: mode, PerSeason: perSeason, Edit: true,
		Settle: 5 * time.Minute, Idle: 24 * time.Hour, MaxHold: 24 * time.Hour,
	}
	return c
}

func newRuns(t *testing.T, c batcher.Config) (*batcher.Batcher, *testkit.Clock, *testkit.MemStore) {
	t.Helper()
	clock, store := testkit.NewClock(t0), testkit.NewMemStore()
	// Runs decide "following" by air date alone: the *arr is never asked.
	noArr := testkit.Arr{Sc: &testkit.Scenario{}}
	b := batcher.New(c, clock, store, map[string]domain.Kind{"sonarr": domain.KindTV},
		func(string) domain.ArrClient { return noArr })
	return b, clock, store
}

func ep(id, season, number int, aired time.Time) domain.Episode {
	return domain.Episode{ID: id, Season: season, Number: number, Aired: aired}
}

func tvImport(eps ...domain.Episode) domain.Import {
	return domain.Import{Source: "sonarr", Series: &domain.Series{ID: 7, Title: "Example Show"}, Episodes: eps}
}

func add(t *testing.T, b *batcher.Batcher, eps ...domain.Episode) {
	t.Helper()
	if _, err := b.Add(context.Background(), tvImport(eps...)); err != nil {
		t.Fatal(err)
	}
}

func due(t *testing.T, b *batcher.Batcher, force bool) map[string]*domain.Batch {
	t.Helper()
	got, err := b.Due(context.Background(), force)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]*domain.Batch{}
	for _, bt := range got {
		out[bt.Key] = bt
	}
	return out
}

var old = t0.AddDate(-20, 0, 0)

// Each episode goes by its own air date: new ones (including one released ahead of its air date)
// to the series' batch, back catalog to the run's batch, per series or per season.
func TestRunsRouteByAirDate(t *testing.T) {
	b, _, _ := newRuns(t, runCfg(batcher.Lead, false))
	add(t, b, ep(1, 1, 1, old), ep(2, 1, 2, old), ep(50, 5, 3, t0.Add(-24*time.Hour)), ep(51, 5, 4, t0.Add(12*time.Hour)),
		ep(52, 5, 5, t0.Add(48*time.Hour))) // too far ahead of its air date to trust: back catalog
	add(t, b, ep(20, 2, 1, old))
	got := due(t, b, true)
	live, run := got["sonarr:7"], got["sonarr:7:backlog"]
	if len(got) != 2 || live == nil || run == nil {
		t.Fatalf("batches: %v", keysOf(got))
	}
	if !live.Following || live.Backlog || live.Len() != 2 {
		t.Errorf("live batch: following=%v backlog=%v items=%d", live.Following, live.Backlog, live.Len())
	}
	if run.Following || !run.Backlog || run.Season != nil || run.Len() != 4 {
		t.Errorf("run batch: following=%v backlog=%v season=%v items=%d", run.Following, run.Backlog, run.Season, run.Len())
	}

	b, _, _ = newRuns(t, runCfg(batcher.Lead, true))
	add(t, b, ep(1, 1, 1, old), ep(20, 2, 1, old))
	got = due(t, b, true)
	if s1, s2 := got["sonarr:7:s1"], got["sonarr:7:s2"]; len(got) != 2 || s1 == nil || s2 == nil ||
		s1.Season == nil || *s1.Season != 1 || *s2.Season != 2 {
		t.Fatalf("per season: %v", keysOf(got))
	}
}

// A back-catalog season trickling in (one download every 30 min) makes one card: the first
// episodes after Settle, then each later one joins the run and goes out as an edit of that card.
func TestLeadRunTrickle(t *testing.T) {
	b, clock, _ := newRuns(t, runCfg(batcher.Lead, false))
	ctx := context.Background()
	msg := &domain.Message{Destination: "tv", ID: "900", Layout: "v2"}
	firstCards := 0
	for i := 1; i <= 6; i++ {
		add(t, b, ep(100+i, 3, i, old))
		clock.Advance(5*time.Minute - time.Second)
		if len(due(t, b, false)) != 0 {
			t.Fatalf("episode %d: due before Settle", i)
		}
		clock.Advance(time.Second)
		bt := due(t, b, false)["sonarr:7:backlog"]
		if bt == nil || bt.Len() != 1 {
			t.Fatalf("episode %d: not due after Settle", i)
		}
		var m *domain.Message
		if bt.Run == nil {
			firstCards++
			m = msg
		} else if len(bt.Run.Episodes) != i-1 || bt.Run.Message != *msg {
			t.Fatalf("episode %d: run has %d episodes, message %+v", i, len(bt.Run.Episodes), bt.Run.Message)
		}
		if err := b.DoneMessage(ctx, bt.Key, bt.Keys(), batcher.Posted, m); err != nil {
			t.Fatal(err)
		}
		clock.Advance(25 * time.Minute)
	}
	if firstCards != 1 {
		t.Errorf("%d first cards, want 1", firstCards)
	}
}

// A season pack lands as several imports within a minute or two: still one first card.
func TestLeadSeasonPackIsOneCard(t *testing.T) {
	b, clock, _ := newRuns(t, runCfg(batcher.Lead, false))
	for i := 1; i <= 8; i++ {
		add(t, b, ep(100+i, 2, i, old))
		clock.Advance(10 * time.Second)
	}
	clock.Advance(5 * time.Minute)
	if bt := due(t, b, false)["sonarr:7:backlog"]; bt == nil || bt.Len() != 8 || bt.Run != nil {
		t.Fatalf("season pack: %+v", bt)
	}
}

// A run with nothing pending lasts Idle after its last import; then the next back-catalog
// episode starts a new run, with a new card.
func TestRunEndsWhenIdle(t *testing.T) {
	b, clock, store := newRuns(t, runCfg(batcher.Lead, false))
	ctx := context.Background()
	add(t, b, ep(101, 1, 1, old))
	clock.Advance(5 * time.Minute)
	bt := due(t, b, false)["sonarr:7:backlog"]
	if err := b.DoneMessage(ctx, bt.Key, bt.Keys(), batcher.Posted, &domain.Message{ID: "1"}); err != nil {
		t.Fatal(err)
	}
	clock.Advance(24*time.Hour - 5*time.Minute - time.Second)
	due(t, b, false)
	if all, _ := store.LoadBatches(ctx); all["sonarr:7:backlog"] == nil || all["sonarr:7:backlog"].Run == nil {
		t.Fatal("run ended before Idle")
	}
	clock.Advance(time.Second)
	due(t, b, false)
	if all, _ := store.LoadBatches(ctx); len(all) != 0 {
		t.Fatalf("run still stored after Idle: %v", keysOf(all))
	}
	add(t, b, ep(102, 1, 2, old))
	clock.Advance(5 * time.Minute)
	if bt := due(t, b, false)["sonarr:7:backlog"]; bt == nil || bt.Run != nil {
		t.Fatalf("after the run ended: %+v", bt)
	}
}

// Complete: the first card waits while the app reports Queued, rechecking every Settle, until
// Backlog.MaxHold after the run's first episode.
func TestCompleteAwaitsQueue(t *testing.T) {
	c := runCfg(batcher.Complete, false)
	b, clock, _ := newRuns(t, c)
	ctx := context.Background()
	add(t, b, ep(101, 1, 1, old))
	clock.Advance(5 * time.Minute)
	bt := due(t, b, false)["sonarr:7:backlog"]
	if bt == nil || !b.AwaitsQueue(bt) {
		t.Fatalf("not awaiting the queue: %+v", bt)
	}
	if err := b.Done(ctx, bt.Key, nil, batcher.Queued); err != nil {
		t.Fatal(err)
	}
	clock.Advance(5*time.Minute - time.Second)
	if len(due(t, b, false)) != 0 {
		t.Fatal("rechecked before Settle")
	}
	clock.Advance(time.Second)
	bt = due(t, b, false)["sonarr:7:backlog"]
	if bt == nil || bt.Tries != 0 || bt.MediaChecks != 0 {
		t.Fatalf("recheck: %+v", bt)
	}
	_ = b.Done(ctx, bt.Key, nil, batcher.Queued)
	clock.Advance(c.Backlog.MaxHold)
	bt = due(t, b, false)["sonarr:7:backlog"]
	if bt == nil || b.AwaitsQueue(bt) {
		t.Fatal("still awaiting the queue after max_hold")
	}
	// Once the card is out, later episodes are edits: they never wait for the queue.
	_ = b.DoneMessage(ctx, bt.Key, bt.Keys(), batcher.Posted, &domain.Message{ID: "1"})
	add(t, b, ep(102, 1, 2, old))
	clock.Advance(5 * time.Minute)
	if bt := due(t, b, false)["sonarr:7:backlog"]; bt == nil || b.AwaitsQueue(bt) {
		t.Fatal("an update of a complete run awaits the queue")
	}
	// Lead never waits for the queue.
	lb, lclock, _ := newRuns(t, runCfg(batcher.Lead, false))
	add(t, lb, ep(101, 1, 1, old))
	lclock.Advance(5 * time.Minute)
	if bt := due(t, lb, false)["sonarr:7:backlog"]; bt == nil || lb.AwaitsQueue(bt) {
		t.Fatal("lead awaits the queue")
	}
}

// A run in progress doesn't touch new episodes of the same show: they post on their own card
// after QuietEpisodes, as before.
func TestLiveEpisodesBypassRun(t *testing.T) {
	b, clock, _ := newRuns(t, runCfg(batcher.Lead, false))
	ctx := context.Background()
	add(t, b, ep(101, 1, 1, old))
	clock.Advance(5 * time.Minute)
	bt := due(t, b, false)["sonarr:7:backlog"]
	_ = b.DoneMessage(ctx, bt.Key, bt.Keys(), batcher.Posted, &domain.Message{ID: "1"})
	add(t, b, ep(501, 6, 4, clock.Now().Add(-2*time.Hour)), ep(102, 1, 2, old))
	clock.Advance(5 * time.Minute)
	got := due(t, b, false)
	live := got["sonarr:7"]
	if live == nil || !live.Following || live.Run != nil || live.Len() != 1 {
		t.Fatalf("live episode: %+v", live)
	}
	if run := got["sonarr:7:backlog"]; run == nil || run.Run == nil || run.Len() != 1 {
		t.Fatalf("run follow-up: %+v", run)
	}
}

// A zero message clears the run's card (it can't be edited any more); episodes still join the run.
func TestRunMessageCleared(t *testing.T) {
	b, clock, _ := newRuns(t, runCfg(batcher.Lead, false))
	ctx := context.Background()
	add(t, b, ep(101, 1, 1, old))
	clock.Advance(5 * time.Minute)
	bt := due(t, b, false)["sonarr:7:backlog"]
	_ = b.DoneMessage(ctx, bt.Key, bt.Keys(), batcher.Posted, &domain.Message{ID: "1", Layout: "v2"})
	add(t, b, ep(102, 1, 2, old))
	clock.Advance(5 * time.Minute)
	bt = due(t, b, false)["sonarr:7:backlog"]
	_ = b.DoneMessage(ctx, bt.Key, bt.Keys(), batcher.Posted, &domain.Message{})
	add(t, b, ep(103, 1, 3, old))
	clock.Advance(5 * time.Minute)
	bt = due(t, b, false)["sonarr:7:backlog"]
	if bt == nil || bt.Run == nil || bt.Run.Message != (domain.Message{}) || len(bt.Run.Episodes) != 2 {
		t.Fatalf("after clearing: %+v", bt)
	}
}

func keysOf(m map[string]*domain.Batch) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}

// A run's next episode after a long gap still settles first: the hold counts from that
// episode, not from the run's last card.
func TestRunHoldRestarts(t *testing.T) {
	b, clock, _ := newRuns(t, runCfg(batcher.Lead, false))
	ctx := context.Background()
	add(t, b, ep(101, 1, 1, old))
	clock.Advance(5 * time.Minute)
	bt := due(t, b, false)["sonarr:7:backlog"]
	_ = b.DoneMessage(ctx, bt.Key, bt.Keys(), batcher.Posted, &domain.Message{ID: "1"})
	clock.Advance(5 * time.Hour) // past max_hold, within idle
	add(t, b, ep(102, 1, 2, old))
	clock.Advance(5*time.Minute - time.Second)
	if len(due(t, b, false)) != 0 {
		t.Fatal("due before Settle")
	}
	clock.Advance(time.Second)
	if len(due(t, b, false)) != 1 {
		t.Fatal("not due after Settle")
	}
}

// failingLedger is a store whose posted ledger can't be written.
type failingLedger struct{ *testkit.MemStore }

func (failingLedger) MarkPosted(context.Context, []domain.ItemKey, time.Time) error {
	return errors.New("disk full")
}

// The run remembers what it announced even when the ledger write failed: a re-import doesn't
// queue it again.
func TestRunRemembersWithoutLedger(t *testing.T) {
	clock, store := testkit.NewClock(t0), failingLedger{testkit.NewMemStore()}
	b := batcher.New(runCfg(batcher.Lead, false), clock, store, map[string]domain.Kind{"sonarr": domain.KindTV},
		func(string) domain.ArrClient { return testkit.Arr{Sc: &testkit.Scenario{}} })
	ctx := context.Background()
	add(t, b, ep(101, 1, 1, old))
	clock.Advance(5 * time.Minute)
	bt := due(t, b, false)["sonarr:7:backlog"]
	if err := b.DoneMessage(ctx, bt.Key, bt.Keys(), batcher.Posted, &domain.Message{ID: "1"}); err == nil {
		t.Fatal("ledger failure not reported")
	}
	add(t, b, ep(101, 1, 1, old))
	if got := due(t, b, true); len(got) != 0 {
		t.Fatalf("re-import queued again: %v", keysOf(got))
	}
}

// A run that never goes quiet is still due: lead's first card and its updates at timing.MaxHold
// after their first episode, complete's first card at Backlog.MaxHold.
func TestBusyRunHoldCaps(t *testing.T) {
	ctx := context.Background()
	// trickle adds an episode every 4 minutes until the run is due; it returns the due batch.
	trickle := func(t *testing.T, b *batcher.Batcher, clock *testkit.Clock, from int, until time.Duration) (time.Duration, *domain.Batch) {
		t.Helper()
		start := clock.Now()
		for id := from; ; id++ {
			add(t, b, ep(id, 1, id%100, old))
			if bt := due(t, b, false)["sonarr:7:backlog"]; bt != nil {
				return clock.Now().Sub(start), bt
			}
			if clock.Now().Sub(start) > until {
				t.Fatalf("not due within %s of a steady trickle", until)
			}
			clock.Advance(4 * time.Minute) // never quiet for Settle
		}
	}
	b, clock, _ := newRuns(t, runCfg(batcher.Lead, false))
	got, bt := trickle(t, b, clock, 101, 5*time.Hour)
	if got != 4*time.Hour {
		t.Errorf("lead first card due after %s, want 4h", got)
	}
	_ = b.DoneMessage(ctx, bt.Key, bt.Keys(), batcher.Posted, &domain.Message{ID: "1"})
	clock.Advance(4 * time.Minute)
	if got, _ := trickle(t, b, clock, 201, 5*time.Hour); got != 4*time.Hour {
		t.Errorf("lead update due after %s, want 4h", got)
	}

	c := runCfg(batcher.Complete, false)
	b, clock, _ = newRuns(t, c)
	if got, _ := trickle(t, b, clock, 101, 25*time.Hour); got != c.Backlog.MaxHold {
		t.Errorf("complete first card due after %s, want %s", got, c.Backlog.MaxHold)
	}
}

// failingSave is a store whose batch writes can be made to fail.
type failingSave struct {
	*testkit.MemStore
	fail bool
}

func (s *failingSave) SaveBatch(ctx context.Context, bt *domain.Batch) error {
	if s.fail {
		return errors.New("disk full")
	}
	return s.MemStore.SaveBatch(ctx, bt)
}

// A delivery outcome that can't be saved leaves the run as the store has it: memory is never
// changed in place.
func TestRunUnchangedWhenSaveFails(t *testing.T) {
	clock, store := testkit.NewClock(t0), &failingSave{MemStore: testkit.NewMemStore()}
	b := batcher.New(runCfg(batcher.Lead, false), clock, store, map[string]domain.Kind{"sonarr": domain.KindTV},
		func(string) domain.ArrClient { return testkit.Arr{Sc: &testkit.Scenario{}} })
	ctx := context.Background()
	add(t, b, ep(101, 1, 1, old))
	clock.Advance(5 * time.Minute)
	bt := due(t, b, false)["sonarr:7:backlog"]
	_ = b.DoneMessage(ctx, bt.Key, bt.Keys(), batcher.Posted, &domain.Message{ID: "1"})
	add(t, b, ep(102, 1, 2, old))
	clock.Advance(5 * time.Minute)
	bt = due(t, b, false)["sonarr:7:backlog"]
	store.fail = true
	if err := b.DoneMessage(ctx, bt.Key, bt.Keys(), batcher.Posted, nil); err == nil {
		t.Fatal("save failure not reported")
	}
	store.fail = false
	due(t, b, true) // the next write of the batch saves what memory holds
	if all, _ := store.LoadBatches(ctx); all["sonarr:7:backlog"] == nil || len(all["sonarr:7:backlog"].Run.Episodes) != 1 {
		t.Fatalf("run changed in memory without the store: %+v", all["sonarr:7:backlog"])
	}
}

// seriesArr answers Series with fixed season stats.
type seriesArr struct{ testkit.Arr }

func (seriesArr) Series(context.Context, int) (*domain.SeriesDetail, error) {
	one := 1
	return &domain.SeriesDetail{EpisodeFileCount: &one, Seasons: map[int]domain.SeasonStats{1: {EpisodeFileCount: 1}}}, nil
}

// following_early applies without runs too (#15): a premiere imported hours before its air date,
// its season's first file, is following and goes out after QuietEpisodes.
func TestQuietModeEarlyRelease(t *testing.T) {
	c := cfg // backlog mode: the reference
	c.FollowingEarly = 24 * time.Hour
	clock, store := testkit.NewClock(t0), testkit.NewMemStore()
	b := batcher.New(c, clock, store, map[string]domain.Kind{"sonarr": domain.KindTV},
		func(string) domain.ArrClient { return seriesArr{} })
	add(t, b, ep(101, 1, 1, t0.Add(3*time.Hour)))
	clock.Advance(c.QuietEpisodes)
	if bt := due(t, b, false)["sonarr:7"]; bt == nil || !bt.Following {
		t.Fatalf("early premiere not following: %+v", bt)
	}
}
