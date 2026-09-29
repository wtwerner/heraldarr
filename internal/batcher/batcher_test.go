package batcher_test

import (
	"context"
	"testing"
	"time"

	"github.com/wtwerner/heraldarr/internal/batcher"
	"github.com/wtwerner/heraldarr/internal/domain"
	"github.com/wtwerner/heraldarr/internal/source/arr"
	"github.com/wtwerner/heraldarr/internal/testkit"
)

var cfg = batcher.Config{
	QuietEpisodes: 5 * time.Minute, QuietBacklog: 30 * time.Minute, QuietMovies: 5 * time.Minute,
	MaxHold: 4 * time.Hour, FollowingWindow: 14 * 24 * time.Hour, Reannounce: 30 * 24 * time.Hour,
	RetryInterval: 2 * time.Minute, RetryMax: 30, MediaWait: 3 * time.Minute,
}

func kinds() map[string]domain.Kind {
	k := map[string]domain.Kind{}
	for name, s := range testkit.Sources {
		k[name] = s.Kind
	}
	return k
}

func setup(t *testing.T, name string) (*batcher.Batcher, *testkit.Clock, *testkit.MemStore, *testkit.Scenario, *testkit.Expected) {
	t.Helper()
	if !batcher.Implemented() {
		t.Skip("batcher not implemented yet (Wave 1)")
	}
	sc, exp := testkit.Load(t, name)
	clock, store := testkit.NewClock(sc.Now), testkit.NewMemStore()
	if err := store.MarkPosted(context.Background(), sc.Posted, sc.Now); err != nil {
		t.Fatal(err)
	}
	a := testkit.Arr{Sc: sc}
	b := batcher.New(cfg, clock, store, kinds(), func(string) domain.ArrClient { return a })
	return b, clock, store, sc, exp
}

func addAll(t *testing.T, b *batcher.Batcher, sc *testkit.Scenario) {
	t.Helper()
	for i, ev := range sc.Events {
		imp, ok, err := arr.ParseWebhook(ev.Source, testkit.Sources[ev.Source].Kind, ev.Payload)
		if err != nil || !ok {
			t.Fatalf("event %d: %v", i, err)
		}
		if _, err := b.Add(context.Background(), imp); err != nil {
			t.Fatalf("event %d: %v", i, err)
		}
	}
}

// After all events, the pending batches match the reference: same keys, items and following flag.
func TestParityPending(t *testing.T) {
	for _, name := range testkit.Names(t) {
		t.Run(name, func(t *testing.T) {
			b, _, _, sc, exp := setup(t, name)
			addAll(t, b, sc)
			got, err := b.Due(context.Background(), true)
			if err != nil {
				t.Fatal(err)
			}
			want := exp.ExpectedBatches(t)
			if len(got) != len(want) {
				t.Fatalf("got %d batches, want %d", len(got), len(want))
			}
			for i := range got {
				g, w := testkit.Summarize(got[i]), want[i]
				if g.Key != w.Key || g.Source != w.Source || g.Following != w.Following || len(g.Items) != len(w.Items) {
					t.Errorf("batch %d: got %+v, want %+v", i, g, w)
					continue
				}
				for j := range g.Items {
					if g.Items[j] != w.Items[j] {
						t.Errorf("batch %d item %d: got %s, want %s", i, j, g.Items[j], w.Items[j])
					}
				}
			}
		})
	}
}

// Quiet windows: when does each scenario's batch first become due?
func TestQuietWindows(t *testing.T) {
	for _, tc := range []struct {
		scenario string
		due      time.Duration // first due after the last event
	}{
		{"tv_weekly_episode", 5 * time.Minute},           // following: aired yesterday
		{"tv_new_series_pilot", 5 * time.Minute},         // following: aired 2 days ago
		{"tv_season_pack", 30 * time.Minute},             // back catalog, season 2 had nothing
		{"tv_backlog_complete_series", 30 * time.Minute}, // back catalog
		{"tv_partial_season", 5 * time.Minute},           // aired long ago, but season 4 already had files
		{"movie_single", 5 * time.Minute},
		{"movie_digest", 5 * time.Minute},
	} {
		t.Run(tc.scenario, func(t *testing.T) {
			b, clock, _, sc, _ := setup(t, tc.scenario)
			addAll(t, b, sc)
			clock.Advance(tc.due - time.Second)
			if due, _ := b.Due(context.Background(), false); len(due) != 0 {
				t.Fatalf("due %s early", time.Second)
			}
			clock.Advance(time.Second)
			if due, _ := b.Due(context.Background(), false); len(due) != 1 {
				t.Fatalf("not due after %s (got %d batches)", tc.due, len(due))
			}
		})
	}
}

// A steady trickle keeps a back-catalog batch waiting, but never past MaxHold after the first item.
func TestMaxHold(t *testing.T) {
	b, clock, _, sc, _ := setup(t, "tv_backlog_complete_series")
	ctx := context.Background()
	start := clock.Now()
	for i, ev := range sc.Events {
		imp, _, _ := arr.ParseWebhook(ev.Source, domain.KindTV, ev.Payload)
		if _, err := b.Add(ctx, imp); err != nil {
			t.Fatal(err)
		}
		if due, _ := b.Due(ctx, false); len(due) != 0 && clock.Now().Sub(start) < cfg.MaxHold {
			t.Fatalf("event %d: due after %s, before max hold", i, clock.Now().Sub(start))
		}
		clock.Advance(15 * time.Minute) // 24 events x 15 min = 6 h of trickle, never 30 min quiet
		if clock.Now().Sub(start) >= cfg.MaxHold {
			if due, _ := b.Due(ctx, false); len(due) != 1 {
				t.Fatalf("not due at max hold (%s)", clock.Now().Sub(start))
			}
			return
		}
	}
	t.Fatal("trickle ended before max hold")
}

// Posted items are never queued again within Reannounce; items that arrived during a send stay.
func TestDoneAndReannounce(t *testing.T) {
	b, clock, store, sc, _ := setup(t, "tv_weekly_episode")
	ctx := context.Background()
	addAll(t, b, sc)
	clock.Advance(5 * time.Minute)
	due, _ := b.Due(ctx, false)
	if len(due) != 1 {
		t.Fatal("want one due batch")
	}
	sent := due[0].Keys()
	if err := b.Done(ctx, due[0].Key, sent, batcher.Posted); err != nil {
		t.Fatal(err)
	}
	if left, _ := b.Due(ctx, true); len(left) != 0 {
		t.Fatalf("batch still pending after Posted: %+v", left)
	}
	if p, _ := store.PostedSince(ctx, sent, sc.Now); len(p) != len(sent) {
		t.Fatal("sent keys not marked posted")
	}
	clock.Advance(24 * time.Hour)
	addAll(t, b, sc) // re-import the same episode
	if left, _ := b.Due(ctx, true); len(left) != 0 {
		t.Fatal("re-import within 30 days was queued again")
	}
}

// Waiting defers by MediaWait; Failed retries after RetryInterval and gives up after RetryMax.
func TestWaitingAndFailed(t *testing.T) {
	b, clock, _, sc, _ := setup(t, "movie_single")
	ctx := context.Background()
	addAll(t, b, sc)
	clock.Advance(5 * time.Minute)
	due, _ := b.Due(ctx, false)
	key := due[0].Key
	if err := b.Done(ctx, key, nil, batcher.Waiting); err != nil {
		t.Fatal(err)
	}
	if d, _ := b.Due(ctx, false); len(d) != 0 {
		t.Fatal("due again before MediaWait")
	}
	clock.Advance(cfg.MediaWait)
	d, _ := b.Due(ctx, false)
	if len(d) != 1 || d[0].MediaChecks != 1 {
		t.Fatalf("after MediaWait: %+v", d)
	}
	for i := 1; i <= cfg.RetryMax; i++ {
		if err := b.Done(ctx, key, nil, batcher.Failed); err != nil {
			t.Fatal(err)
		}
		clock.Advance(cfg.RetryInterval)
		d, _ := b.Due(ctx, false)
		if i < cfg.RetryMax && len(d) != 1 {
			t.Fatalf("try %d: batch dropped early", i)
		}
		if i == cfg.RetryMax && len(d) != 0 {
			t.Fatalf("still pending after %d failures", i)
		}
	}
}

// Aborted releases the batch unchanged: not a try, sent keys recorded, due again right away.
func TestAborted(t *testing.T) {
	b, clock, store, sc, _ := setup(t, "movie_three")
	ctx := context.Background()
	addAll(t, b, sc)
	clock.Advance(5 * time.Minute)
	due, _ := b.Due(ctx, false)
	keys := due[0].Keys()
	if err := b.Done(ctx, due[0].Key, keys[:1], batcher.Aborted); err != nil {
		t.Fatal(err)
	}
	again, _ := b.Due(ctx, false)
	if len(again) != 1 || again[0].Tries != 0 || again[0].Len() != len(keys)-1 {
		t.Fatalf("after Aborted: %+v", again)
	}
	if p, _ := store.PostedSince(ctx, keys[:1], sc.Now); !p[keys[0]] {
		t.Error("sent key not recorded")
	}
}
