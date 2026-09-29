package batcher_test

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/wtwerner/heraldarr/internal/batcher"
	"github.com/wtwerner/heraldarr/internal/domain"
	"github.com/wtwerner/heraldarr/internal/source/arr"
	"github.com/wtwerner/heraldarr/internal/testkit"
)

// Add's log lines match the reference's add_import results, event by event.
func TestParityAddResults(t *testing.T) {
	for _, name := range testkit.Names(t) {
		t.Run(name, func(t *testing.T) {
			b, _, _, sc, exp := setup(t, name)
			var got []string
			for i, ev := range sc.Events {
				imp, _, err := arr.ParseWebhook(ev.Source, testkit.Sources[ev.Source].Kind, ev.Payload)
				if err != nil {
					t.Fatalf("event %d: %v", i, err)
				}
				line, err := b.Add(context.Background(), imp)
				if err != nil {
					t.Fatalf("event %d: %v", i, err)
				}
				got = append(got, line)
			}
			if !slices.Equal(got, exp.AddResults) {
				t.Errorf("got %q\nwant %q", got, exp.AddResults)
			}
		})
	}
}

// movie is a fictional Radarr import for tests that need more items than a scenario has.
func movie(id int) domain.Import {
	return domain.Import{Source: "radarr", Movie: &domain.Movie{
		ID: id, Title: fmt.Sprintf("Placeholder %d", id), Path: fmt.Sprintf("/movies/Placeholder %d", id), TMDBID: 90000 + id,
	}}
}

func mustAdd(t *testing.T, b *batcher.Batcher, imp domain.Import) {
	t.Helper()
	if _, err := b.Add(context.Background(), imp); err != nil {
		t.Fatal(err)
	}
}

// Items that arrive while a batch is being sent stay, and start a fresh window from the send.
func TestPostedKeepsItemsThatArrivedMeanwhile(t *testing.T) {
	b, clock, _, sc, _ := setup(t, "movie_single")
	ctx := context.Background()
	addAll(t, b, sc)
	clock.Advance(5 * time.Minute)
	due, _ := b.Due(ctx, false)
	if len(due) != 1 {
		t.Fatal("want one due batch")
	}
	mustAdd(t, b, movie(9001)) // arrives mid-send
	clock.Advance(10 * time.Second)
	sentAt := clock.Now()
	if err := b.Done(ctx, due[0].Key, due[0].Keys(), batcher.Posted); err != nil {
		t.Fatal(err)
	}
	left, _ := b.Due(ctx, true)
	if len(left) != 1 || !slices.Equal(left[0].Keys(), []domain.ItemKey{"radarr:movie:9001"}) {
		t.Fatalf("left: %+v", left)
	}
	if l := left[0]; !l.First.Equal(sentAt) || l.Tries != 0 || l.MediaChecks != 0 || !l.NotBefore.IsZero() {
		t.Errorf("fresh window: first %s (want %s), tries %d, checks %d, notBefore %s",
			l.First, sentAt, l.Tries, l.MediaChecks, l.NotBefore)
	}
}

// A send that fails part-way keeps what didn't go out; what did go out is marked posted.
func TestFailedMarksSentKeysPosted(t *testing.T) {
	b, clock, store, sc, _ := setup(t, "movie_three")
	ctx := context.Background()
	addAll(t, b, sc)
	clock.Advance(5 * time.Minute)
	due, _ := b.Due(ctx, false)
	keys := due[0].Keys()
	if err := b.Done(ctx, due[0].Key, keys[:1], batcher.Failed); err != nil {
		t.Fatal(err)
	}
	if p, _ := store.PostedSince(ctx, keys, clock.Now()); !p[keys[0]] || len(p) != 1 {
		t.Fatalf("posted after a partial send: %v", p)
	}
	left, _ := b.Due(ctx, true)
	if len(left) != 1 || !slices.Equal(left[0].Keys(), keys[1:]) || left[0].Tries != 1 {
		t.Fatalf("left: %+v", left)
	}
	if want := clock.Now().Add(cfg.RetryInterval); !left[0].NotBefore.Equal(want) {
		t.Errorf("notBefore %s, want %s", left[0].NotBefore, want)
	}
}

// Giving up drops what was due; items that arrived during the last try stay, in a fresh window.
func TestGiveUpKeepsItemsThatArrivedMeanwhile(t *testing.T) {
	b, clock, _, sc, _ := setup(t, "movie_single")
	ctx := context.Background()
	addAll(t, b, sc)
	clock.Advance(5 * time.Minute)
	var due []*domain.Batch
	for i := 1; i <= cfg.RetryMax; i++ {
		due, _ = b.Due(ctx, false)
		if len(due) != 1 {
			t.Fatalf("try %d: not due", i)
		}
		if i == cfg.RetryMax {
			mustAdd(t, b, movie(9002))
		}
		if err := b.Done(ctx, due[0].Key, nil, batcher.Failed); err != nil {
			t.Fatal(err)
		}
		clock.Advance(cfg.RetryInterval)
	}
	left, _ := b.Due(ctx, true)
	if len(left) != 1 || !slices.Equal(left[0].Keys(), []domain.ItemKey{"radarr:movie:9002"}) || left[0].Tries != 0 {
		t.Fatalf("left: %+v", left)
	}
}

// The posted ledger forgets entries older than Reannounce, checking at most once an hour.
func TestDonePrunesLedgerHourly(t *testing.T) {
	b, clock, store, sc, _ := setup(t, "movie_three")
	ctx := context.Background()
	addAll(t, b, sc)
	clock.Advance(5 * time.Minute)
	due, _ := b.Due(ctx, false)
	keys := due[0].Keys()
	stale := func(k domain.ItemKey) {
		if err := store.MarkPosted(ctx, []domain.ItemKey{k}, clock.Now().Add(-cfg.Reannounce-time.Hour)); err != nil {
			t.Fatal(err)
		}
	}
	inLedger := func(k domain.ItemKey) bool {
		p, _ := store.PostedSince(ctx, []domain.ItemKey{k}, time.Time{})
		return p[k]
	}

	send := func(sent []domain.ItemKey, outcome batcher.Outcome) {
		t.Helper()
		if due, _ := b.Due(ctx, true); len(due) != 1 {
			t.Fatal("want the batch handed out")
		}
		if err := b.Done(ctx, due[0].Key, sent, outcome); err != nil {
			t.Fatal(err)
		}
	}

	stale("radarr:movie:1")
	if err := b.Done(ctx, due[0].Key, keys[:1], batcher.Failed); err != nil {
		t.Fatal(err)
	}
	if inLedger("radarr:movie:1") {
		t.Fatal("first Done didn't prune")
	}
	stale("radarr:movie:2")
	clock.Advance(59 * time.Minute)
	send(keys[1:2], batcher.Failed)
	if !inLedger("radarr:movie:2") {
		t.Fatal("pruned again within the hour")
	}
	clock.Advance(time.Minute)
	send(keys[2:], batcher.Posted)
	if inLedger("radarr:movie:2") {
		t.Fatal("not pruned after an hour")
	}
}

// An *arr that can't be reached (or no longer has the series) means not following: be patient.
func TestFollowingFallsBackWhenArrUnreachable(t *testing.T) {
	for _, stub := range []string{"unreachable", "deleted"} {
		t.Run(stub, func(t *testing.T) {
			if !batcher.Implemented() {
				t.Skip("batcher not implemented yet (Wave 1)")
			}
			// tv_partial_season is following only because the *arr says season 4 already had files.
			sc, _ := testkit.Load(t, "tv_partial_season")
			bare := *sc
			bare.Arr = map[string]json.RawMessage{}
			if stub == "deleted" {
				bare.Arr["series/105"] = json.RawMessage("null")
			}
			a := testkit.Arr{Sc: &bare}
			clock := testkit.NewClock(sc.Now)
			b := batcher.New(cfg, clock, testkit.NewMemStore(), kinds(), func(string) domain.ArrClient { return a })
			addAll(t, b, sc)
			got, _ := b.Due(context.Background(), true)
			if len(got) != 1 || got[0].Following {
				t.Fatalf("got %+v, want one batch, not following", got)
			}
		})
	}
}

// Concurrent webhooks and flushes lose nothing: every item ends up posted or still pending.
func TestConcurrentAddAndFlush(t *testing.T) {
	if !batcher.Implemented() {
		t.Skip("batcher not implemented yet (Wave 1)")
	}
	// The flusher advances the clock a minute a loop with no bound: keep the ledger out of its reach.
	long := cfg
	long.Reannounce = 100 * 365 * 24 * time.Hour
	clock, store := testkit.NewClock(time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)), testkit.NewMemStore()
	b := batcher.New(long, clock, store, kinds(), nil)
	ctx := context.Background()
	const writers, each = 4, 25
	var wg sync.WaitGroup
	for w := range writers {
		wg.Go(func() {
			for i := range each {
				if _, err := b.Add(ctx, movie(1000+w*each+i)); err != nil {
					t.Error(err)
				}
			}
		})
	}
	stop := make(chan struct{})
	flushed := make(chan struct{})
	go func() {
		defer close(flushed)
		for {
			select {
			case <-stop:
				return
			default:
			}
			clock.Advance(time.Minute)
			due, err := b.Due(ctx, false)
			if err != nil {
				t.Error(err)
				return
			}
			for _, d := range due {
				if err := b.Done(ctx, d.Key, d.Keys(), batcher.Posted); err != nil {
					t.Error(err)
					return
				}
			}
		}
	}()
	wg.Wait()
	close(stop)
	<-flushed

	var all []domain.ItemKey
	for i := range writers * each {
		all = append(all, domain.MovieKey("radarr", 1000+i))
	}
	posted, _ := store.PostedSince(ctx, all, time.Time{})
	pending, _ := b.Due(ctx, true)
	for _, p := range pending {
		for _, k := range p.Keys() {
			if posted[k] {
				t.Errorf("%s both posted and pending", k)
			}
			posted[k] = true
		}
	}
	for _, k := range all {
		if !posted[k] {
			t.Errorf("%s lost", k)
		}
	}
}
