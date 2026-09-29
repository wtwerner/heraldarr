package batcher_test

import (
	"context"
	"errors"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wtwerner/heraldarr/internal/batcher"
	"github.com/wtwerner/heraldarr/internal/domain"
	"github.com/wtwerner/heraldarr/internal/source/arr"
	"github.com/wtwerner/heraldarr/internal/testkit"
)

var errFlaky = errors.New("store unavailable")

// flakyStore fails the operations whose switch is on, and counts full batch loads.
type flakyStore struct {
	*testkit.MemStore
	failPrune, failSave, failMark atomic.Bool
	loads                         atomic.Int32
	postedFailsIn                 atomic.Int32 // n > 0: the nth PostedSince call from now fails, once
}

func (s *flakyStore) PostedSince(ctx context.Context, keys []domain.ItemKey, since time.Time) (map[domain.ItemKey]bool, error) {
	if s.postedFailsIn.Load() > 0 && s.postedFailsIn.Add(-1) == 0 {
		return nil, errFlaky
	}
	return s.MemStore.PostedSince(ctx, keys, since)
}

func (s *flakyStore) LoadBatches(ctx context.Context) (map[string]*domain.Batch, error) {
	s.loads.Add(1)
	return s.MemStore.LoadBatches(ctx)
}

func (s *flakyStore) SaveBatch(ctx context.Context, bt *domain.Batch) error {
	if s.failSave.Load() {
		return errFlaky
	}
	return s.MemStore.SaveBatch(ctx, bt)
}

func (s *flakyStore) DeleteBatch(ctx context.Context, key string) error {
	if s.failSave.Load() {
		return errFlaky
	}
	return s.MemStore.DeleteBatch(ctx, key)
}

func (s *flakyStore) MarkPosted(ctx context.Context, keys []domain.ItemKey, at time.Time) error {
	if s.failMark.Load() {
		return errFlaky
	}
	return s.MemStore.MarkPosted(ctx, keys, at)
}

func (s *flakyStore) PrunePosted(ctx context.Context, before time.Time) (int, error) {
	if s.failPrune.Load() {
		return 0, errFlaky
	}
	return s.MemStore.PrunePosted(ctx, before)
}

// flaky sets up movie_three (three movies, one batch) on a flakyStore, due now.
func flaky(t *testing.T) (*batcher.Batcher, *flakyStore) {
	t.Helper()
	if !batcher.Implemented() {
		t.Skip("batcher not implemented yet (Wave 1)")
	}
	sc, _ := testkit.Load(t, "movie_three")
	clock, store := testkit.NewClock(sc.Now), &flakyStore{MemStore: testkit.NewMemStore()}
	b := batcher.New(cfg, clock, store, kinds(), nil)
	addAll(t, b, sc)
	clock.Advance(cfg.QuietMovies)
	return b, store
}

// handOut returns the one batch Due hands out.
func handOut(t *testing.T, b *batcher.Batcher, force bool) *domain.Batch {
	t.Helper()
	due, err := b.Due(context.Background(), force)
	if err != nil || len(due) != 1 {
		t.Fatalf("want one batch handed out, got %d (%v)", len(due), err)
	}
	return due[0]
}

// Pruning is housekeeping: its failure must not stop the batch update, or the items post twice.
func TestPruneFailureDoesNotAbortDone(t *testing.T) {
	b, store := flaky(t)
	ctx := context.Background()
	d := handOut(t, b, false)
	store.failPrune.Store(true)
	if err := b.Done(ctx, d.Key, d.Keys(), batcher.Posted); err != nil {
		t.Fatalf("Done failed on a prune error: %v", err)
	}
	if left, _ := b.Due(ctx, true); len(left) != 0 {
		t.Fatalf("posted items still pending: %+v", left)
	}
}

// Sent items never go out again, whichever half of Done's bookkeeping the store drops.
func TestStoreFailureInDoneDoesNotResend(t *testing.T) {
	for _, fail := range []string{"mark", "save"} {
		t.Run(fail, func(t *testing.T) {
			b, store := flaky(t)
			ctx := context.Background()
			d := handOut(t, b, false)
			sent := d.Keys()[:2]
			sw := &store.failSave
			if fail == "mark" {
				sw = &store.failMark
			}
			sw.Store(true)
			if err := b.Done(ctx, d.Key, sent, batcher.Posted); !errors.Is(err, errFlaky) {
				t.Fatalf("Done: got %v, want the store error", err)
			}
			sw.Store(false)
			again := handOut(t, b, true)
			if !slices.Equal(again.Keys(), d.Keys()[2:]) {
				t.Fatalf("handed out again: %v, want only %v", again.Keys(), d.Keys()[2:])
			}
		})
	}
}

// A batch being sent isn't handed out again, even by a forced Due, until Done reports on it.
func TestDueHandsOutOnce(t *testing.T) {
	b, _ := flaky(t)
	ctx := context.Background()
	d := handOut(t, b, false)
	mustAdd(t, b, movie(9003)) // arrives mid-send
	if again, _ := b.Due(ctx, true); len(again) != 0 {
		t.Fatalf("handed out twice: %+v", again)
	}
	if err := b.Done(ctx, d.Key, d.Keys(), batcher.Posted); err != nil {
		t.Fatal(err)
	}
	if left := handOut(t, b, true); !slices.Equal(left.Keys(), []domain.ItemKey{"radarr:movie:9003"}) {
		t.Fatalf("left: %v", left.Keys())
	}
}

// A Due that fails partway hands out nothing, so it leaves nothing in flight: once the store
// recovers, every batch goes out.
func TestDueFailurePartwayStrandsNothing(t *testing.T) {
	b, store := flaky(t)
	ctx := context.Background()
	mustAdd(t, b, domain.Import{Source: "radarr4k", Movie: &domain.Movie{ID: 9006, Title: "Placeholder 9006"}})
	store.postedFailsIn.Store(2) // radarr4k:movies is checked first, then radarr:movies fails
	if due, err := b.Due(ctx, true); !errors.Is(err, errFlaky) || len(due) != 0 {
		t.Fatalf("Due: got %d batches, %v; want none and the store error", len(due), err)
	}
	var keys []string
	for _, d := range mustDue(t, b) {
		keys = append(keys, d.Key)
	}
	if want := []string{"radarr4k:movies", "radarr:movies"}; !slices.Equal(keys, want) {
		t.Fatalf("after recovery Due handed out %v, want %v", keys, want)
	}
}

// Done reports on a batch Due handed out; anything else is a caller bug, and changes nothing.
func TestDoneWithoutDue(t *testing.T) {
	b, _ := flaky(t)
	ctx := context.Background()
	if err := b.Done(ctx, "radarr:movies", nil, batcher.Failed); err == nil {
		t.Fatal("Done without Due succeeded")
	}
	if d := handOut(t, b, false); d.Tries != 0 || d.Len() != 3 {
		t.Fatalf("batch changed: tries %d, %d items", d.Tries, d.Len())
	}
}

// The batcher keeps its own copy of the batches: one store load, not one per webhook or flush.
func TestBatchesLoadedOnce(t *testing.T) {
	b, store := flaky(t)
	ctx := context.Background()
	d := handOut(t, b, false)
	if err := b.Done(ctx, d.Key, d.Keys(), batcher.Posted); err != nil {
		t.Fatal(err)
	}
	mustAdd(t, b, movie(9004))
	if n := store.loads.Load(); n != 1 {
		t.Fatalf("LoadBatches called %d times", n)
	}
}

// blockingArr answers like testkit.Arr, but only once released.
type blockingArr struct {
	testkit.Arr
	entered, release chan struct{}
}

func (a blockingArr) Series(ctx context.Context, id int) (*domain.SeriesDetail, error) {
	close(a.entered)
	<-a.release
	return a.Arr.Series(ctx, id)
}

// The *arr lookup for "following" happens outside the lock: a slow Sonarr holds up nothing else.
func TestFollowingLookupDoesNotBlock(t *testing.T) {
	if !batcher.Implemented() {
		t.Skip("batcher not implemented yet (Wave 1)")
	}
	sc, _ := testkit.Load(t, "tv_partial_season") // aired long ago: needs the lookup
	a := blockingArr{Arr: testkit.Arr{Sc: sc}, entered: make(chan struct{}), release: make(chan struct{})}
	b := batcher.New(cfg, testkit.NewClock(sc.Now), testkit.NewMemStore(), kinds(),
		func(string) domain.ArrClient { return a })
	ctx := context.Background()
	imp, _, _ := arr.ParseWebhook("sonarr", domain.KindTV, sc.Events[0].Payload)
	added := make(chan error)
	go func() {
		_, err := b.Add(ctx, imp)
		added <- err
	}()
	<-a.entered

	listed := make(chan struct{})
	go func() {
		if _, err := b.Add(ctx, movie(9005)); err != nil {
			t.Error(err)
		}
		close(listed)
	}()
	select {
	case <-listed:
	case <-time.After(5 * time.Second):
		t.Fatal("Add blocked behind another Add's *arr lookup")
	}

	close(a.release)
	if err := <-added; err != nil {
		t.Fatal(err)
	}
	for _, bt := range mustDue(t, b) {
		if bt.Kind == domain.KindTV && !bt.Following {
			t.Error("lookup result not applied: season 4 already had files")
		}
	}
}

func mustDue(t *testing.T, b *batcher.Batcher) []*domain.Batch {
	t.Helper()
	due, err := b.Due(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	return due
}
