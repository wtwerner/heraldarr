// Package batcher decides what waits and what goes out: it groups imports into batches (one per
// series per source, one per movie source), applies the quiet windows, and tracks delivery
// outcomes. Reference: add_import, tv_following, due, flush and mark_posted in the reference
// implementation. All state is persisted through domain.Store; time comes from domain.Clock.
package batcher

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/wtwerner/heraldarr/internal/domain"
)

// implemented gates the parity and timing tests until Wave 1 lands this package (issue: batcher).
const implemented = true

// Implemented reports whether the package is past its Wave 0 stub.
func Implemented() bool { return implemented }

// Config is the timing part of the configuration (config.Timing plus the media server wait).
type Config struct {
	QuietEpisodes   time.Duration // following: new episodes people watch as they air
	QuietBacklog    time.Duration // back-catalog series/seasons
	QuietMovies     time.Duration
	MaxHold         time.Duration // after the batch's first item, due regardless of quiet
	FollowingWindow time.Duration // every episode aired within this -> following
	Reannounce      time.Duration // an item posted within this is never queued again
	RetryInterval   time.Duration
	RetryMax        int           // failed sends before the batch is dropped
	MediaWait       time.Duration // between media server checks
}

// Outcome of one delivery attempt.
type Outcome int

const (
	// Posted: the sent keys went out; items that arrived meanwhile start a fresh batch window.
	Posted Outcome = iota
	// Waiting: the media server doesn't have the items yet; check again after MediaWait.
	Waiting
	// Failed: a transient error; retry after RetryInterval, drop after RetryMax tries.
	Failed
)

// pruneEvery is how often Done trims the posted ledger to the reannounce window.
const pruneEvery = time.Hour

type Batcher struct {
	cfg   Config
	clock domain.Clock
	store domain.Store
	kinds map[string]domain.Kind
	arrs  func(source string) domain.ArrClient

	mu        sync.Mutex                  // serializes each read-modify-write of the store's batches
	inflight  map[string][]domain.ItemKey // batch key -> the item keys Due last handed out
	lastPrune time.Time
}

// New creates a batcher. kinds maps source name -> kind; arrs returns the API client used to
// decide "following" for TV (every season in the batch already had files before it).
func New(cfg Config, clock domain.Clock, store domain.Store, kinds map[string]domain.Kind,
	arrs func(source string) domain.ArrClient,
) *Batcher {
	return &Batcher{
		cfg: cfg, clock: clock, store: store, kinds: kinds, arrs: arrs,
		inflight: map[string][]domain.ItemKey{},
	}
}

// Add queues one import. Upgrades and items posted within Reannounce are skipped. Returns a short
// line for the log ("sonarr Show: +2", "upgrade, ignored", "sonarr Show: already announced").
func (b *Batcher) Add(ctx context.Context, imp domain.Import) (string, error) {
	if imp.Upgrade {
		return "upgrade, ignored", nil
	}
	kind, ok := b.kinds[imp.Source]
	if !ok {
		return "", fmt.Errorf("add: unknown source %q", imp.Source)
	}
	var key, name string
	var keys []domain.ItemKey
	switch {
	case kind == domain.KindTV && imp.Series != nil:
		key, name = fmt.Sprintf("%s:%d", imp.Source, imp.Series.ID), imp.Series.Title
		for _, e := range imp.Episodes {
			keys = append(keys, domain.EpisodeKey(imp.Source, e.ID))
		}
	case kind == domain.KindMovie && imp.Movie != nil:
		key, name = imp.Source+":movies", imp.Movie.Title
		keys = []domain.ItemKey{domain.MovieKey(imp.Source, imp.Movie.ID)}
	default:
		return "", fmt.Errorf("add: %s import has no %s subject", imp.Source, kind)
	}

	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.clock.Now()
	posted, err := b.store.PostedSince(ctx, keys, now.Add(-b.cfg.Reannounce))
	if err != nil {
		return "", fmt.Errorf("add: %w", err)
	}
	batches, err := b.store.LoadBatches(ctx)
	if err != nil {
		return "", fmt.Errorf("add: %w", err)
	}
	bt, existed := batches[key]
	if !existed {
		bt = &domain.Batch{Key: key, Source: imp.Source, Kind: kind, First: now}
		if kind == domain.KindTV {
			s := *imp.Series
			bt.Series = &s
		}
	}

	added := 0
	for i, k := range keys {
		if posted[k] {
			continue
		}
		added++
		if kind == domain.KindTV {
			if bt.Episodes == nil {
				bt.Episodes = map[domain.ItemKey]domain.Episode{}
			}
			bt.Episodes[k] = imp.Episodes[i]
		} else {
			if bt.Movies == nil {
				bt.Movies = map[domain.ItemKey]domain.Movie{}
			}
			bt.Movies[k] = *imp.Movie
		}
	}
	// Like the reference, even an import that adds nothing counts as activity on the batch.
	bt.Last = now
	if kind == domain.KindTV {
		bt.Following = b.following(ctx, bt, now)
	}

	switch {
	case bt.Len() > 0:
		err = b.store.SaveBatch(ctx, bt)
	case existed:
		err = b.store.DeleteBatch(ctx, key)
	}
	if err != nil {
		return "", fmt.Errorf("add: %w", err)
	}
	if added == 0 {
		return fmt.Sprintf("%s %s: already announced", imp.Source, name), nil
	}
	return fmt.Sprintf("%s %s: +%d", imp.Source, name, added), nil
}

// following: every episode aired within FollowingWindow (weekly episodes, a premiere, a same-day
// season drop), or every season in the batch already had files before it. An *arr that can't
// answer means not following: the longer quiet window is the patient choice.
func (b *Batcher) following(ctx context.Context, bt *domain.Batch, now time.Time) bool {
	recent := true
	perSeason := map[int]int{}
	for _, e := range bt.Episodes {
		if age := now.Sub(e.Aired); e.Aired.IsZero() || age < 0 || age >= b.cfg.FollowingWindow {
			recent = false
		}
		perSeason[e.Season]++
	}
	if recent {
		return true
	}
	detail, err := b.arrs(bt.Source).Series(ctx, bt.Series.ID)
	if err != nil {
		slog.Warn("following: series lookup failed, treating as back catalog",
			"source", bt.Source, "series", bt.Series.Title, "err", err)
		return false
	}
	// Counts are after the import, so more files than this batch brings means some were there before.
	for season, n := range perSeason {
		if detail.Seasons[season].EpisodeFileCount <= n {
			return false
		}
	}
	return true
}

// Due returns snapshots of the batches ready to send now (all non-empty ones when force, ignoring
// NotBefore), ordered by key.
func (b *Batcher) Due(ctx context.Context, force bool) ([]*domain.Batch, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.clock.Now()
	batches, err := b.store.LoadBatches(ctx)
	if err != nil {
		return nil, fmt.Errorf("due: %w", err)
	}
	var out []*domain.Batch
	for _, bt := range batches {
		if bt.Len() > 0 && (force || b.due(bt, now)) {
			out = append(out, bt)
			b.inflight[bt.Key] = bt.Keys()
		}
	}
	slices.SortFunc(out, func(x, y *domain.Batch) int { return strings.Compare(x.Key, y.Key) })
	return out, nil
}

func (b *Batcher) due(bt *domain.Batch, now time.Time) bool {
	if now.Before(bt.NotBefore) {
		return false
	}
	quiet := b.cfg.QuietBacklog
	switch {
	case bt.Kind == domain.KindMovie:
		quiet = b.cfg.QuietMovies
	case bt.Following:
		quiet = b.cfg.QuietEpisodes
	}
	return now.Sub(bt.Last) >= quiet || now.Sub(bt.First) >= b.cfg.MaxHold
}

// Done records a delivery attempt of the batch with this key. sent are the keys that went out
// (they are marked posted even when the outcome is Failed part-way through).
func (b *Batcher) Done(ctx context.Context, key string, sent []domain.ItemKey, outcome Outcome) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.clock.Now()
	handed, ok := b.inflight[key]
	delete(b.inflight, key)

	if len(sent) > 0 {
		if err := b.store.MarkPosted(ctx, sent, now); err != nil {
			return fmt.Errorf("done %s: %w", key, err)
		}
		if now.Sub(b.lastPrune) >= pruneEvery {
			if _, err := b.store.PrunePosted(ctx, now.Add(-b.cfg.Reannounce)); err != nil {
				return fmt.Errorf("done %s: prune: %w", key, err)
			}
			b.lastPrune = now
		}
	}

	// Re-read: new items may have arrived while the batch was being sent.
	batches, err := b.store.LoadBatches(ctx)
	if err != nil {
		return fmt.Errorf("done %s: %w", key, err)
	}
	bt := batches[key]
	if bt == nil {
		return nil
	}
	gone := slices.Clone(sent)
	switch outcome {
	case Waiting:
		bt.MediaChecks++
		bt.NotBefore = now.Add(b.cfg.MediaWait)
	case Failed:
		bt.Tries++
		if bt.Tries < b.cfg.RetryMax {
			slog.Warn("send failed, retrying", "batch", key, "tries", bt.Tries, "in", b.cfg.RetryInterval)
			bt.NotBefore = now.Add(b.cfg.RetryInterval)
			break
		}
		slog.Error("send failed, giving up", "batch", key, "tries", bt.Tries)
		if !ok { // no Due snapshot (not expected): drop everything
			handed = bt.Keys()
		}
		gone = append(gone, handed...)
		fallthrough // as in the reference, what arrived during the last try starts a fresh window
	case Posted:
		bt.First, bt.MediaChecks, bt.Tries, bt.NotBefore = now, 0, 0, time.Time{}
	}
	for _, k := range gone {
		delete(bt.Episodes, k)
		delete(bt.Movies, k)
	}
	if bt.Len() == 0 {
		err = b.store.DeleteBatch(ctx, key)
	} else {
		err = b.store.SaveBatch(ctx, bt)
	}
	if err != nil {
		return fmt.Errorf("done %s: %w", key, err)
	}
	return nil
}
