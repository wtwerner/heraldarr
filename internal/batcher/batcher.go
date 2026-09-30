// Package batcher decides what waits and what goes out: it groups imports into batches (one per
// series per source, one per movie source), applies the quiet windows, and tracks delivery
// outcomes. Reference: add_import, tv_following, due, flush and mark_posted in the reference
// implementation. All state is persisted through domain.Store; time comes from domain.Clock.
package batcher

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
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
	FollowingEarly  time.Duration // an episode imported up to this long before its air date is recent too
	Reannounce      time.Duration // an item posted within this is never queued again
	RetryInterval   time.Duration
	RetryMax        int           // failed sends before the batch is dropped
	MediaWait       time.Duration // between media server checks
	Backlog         Backlog
}

// Backlog modes. The zero value is the reference behavior: no runs.
const (
	Lead     = "lead"     // post once the first episodes settle, fold later ones into that card
	Complete = "complete" // post once the *arr has nothing left queued for the run
)

// Backlog is how back-catalog TV (episodes that aired before FollowingWindow) is grouped into runs.
type Backlog struct {
	Mode      string // Lead, Complete; anything else: the reference's quiet windows
	PerSeason bool   // one run per season instead of one per series
	Edit      bool   // the app edits the run's card as episodes land (else they're folded in silently)
	Settle    time.Duration
	Idle      time.Duration // a run with nothing pending ends after this long without an import
	MaxHold   time.Duration // Complete: due this long after the run's first item, queue or not
}

func (c Config) runs() bool { return c.Backlog.Mode == Lead || c.Backlog.Mode == Complete }

// Outcome of one delivery attempt.
type Outcome int

const (
	// Posted: the sent keys went out; items that arrived meanwhile start a fresh batch window.
	Posted Outcome = iota
	// Waiting: the media server doesn't have the items yet; check again after MediaWait.
	Waiting
	// Failed: a transient error; retry after RetryInterval, drop after RetryMax tries.
	Failed
	// Aborted: the attempt was interrupted (shutdown). Sent keys are recorded; the batch is
	// otherwise left as it was, and not counted as a try.
	Aborted
	// Queued: the *arr still has episodes of the run downloading (Complete); check again after Settle.
	Queued
)

// pruneEvery is how often Done trims the posted ledger to the reannounce window.
const pruneEvery = time.Hour

// Batcher is the only writer of batches: it keeps them in memory, loaded from the store on first
// use, and writes every change through. One Batcher per store.
type Batcher struct {
	cfg   Config
	clock domain.Clock
	store domain.Store
	kinds map[string]domain.Kind
	arrs  func(source string) domain.ArrClient

	mu        sync.Mutex
	batches   map[string]*domain.Batch    // what the store holds; nil until loaded. Never mutated in place.
	inflight  map[string][]domain.ItemKey // handed out by Due, not yet reported by Done: key -> item keys
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
	if kind == domain.KindTV && imp.Series != nil && b.cfg.runs() {
		return b.addRuns(ctx, imp)
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

	added, lookup, err := b.merge(ctx, imp, kind, key, keys, imp.Episodes, nil)
	if err != nil {
		return "", fmt.Errorf("add: %w", err)
	}
	if lookup {
		// Outside the lock: a slow or unreachable *arr must not hold up other webhooks or the flush.
		detail, lookupErr := b.arrs(imp.Source).Series(ctx, imp.Series.ID)
		if err := b.settleFollowing(ctx, key, detail, lookupErr); err != nil {
			return "", fmt.Errorf("add: %w", err)
		}
	}
	if added == 0 {
		return fmt.Sprintf("%s %s: already announced", imp.Source, name), nil
	}
	return fmt.Sprintf("%s %s: +%d", imp.Source, name, added), nil
}

// addRuns queues a TV import when back-catalog runs are on. Each episode goes by its air date:
// recent ones to the series' batch (following), older ones to the run's batch.
func (b *Batcher) addRuns(ctx context.Context, imp domain.Import) (string, error) {
	type group struct {
		keys    []domain.ItemKey
		eps     []domain.Episode
		backlog bool
		season  *int
	}
	now := b.clock.Now()
	groups := map[string]*group{}
	for _, e := range imp.Episodes {
		key, g := fmt.Sprintf("%s:%d", imp.Source, imp.Series.ID), group{}
		if !b.recentEpisode(e, now) {
			g.backlog = true
			if b.cfg.Backlog.PerSeason {
				key, g.season = fmt.Sprintf("%s:%d:s%d", imp.Source, imp.Series.ID, e.Season), &e.Season
			} else {
				key += ":backlog"
			}
		}
		if groups[key] == nil {
			groups[key] = &g
		}
		groups[key].keys = append(groups[key].keys, domain.EpisodeKey(imp.Source, e.ID))
		groups[key].eps = append(groups[key].eps, e)
	}
	added := 0
	for _, key := range slices.Sorted(maps.Keys(groups)) {
		g := groups[key]
		n, _, err := b.merge(ctx, imp, domain.KindTV, key, g.keys, g.eps, func(bt *domain.Batch) {
			bt.Backlog, bt.Season = g.backlog, g.season
		})
		if err != nil {
			return "", fmt.Errorf("add: %w", err)
		}
		added += n
	}
	if added == 0 {
		return fmt.Sprintf("%s %s: already announced", imp.Source, imp.Series.Title), nil
	}
	return fmt.Sprintf("%s %s: +%d", imp.Source, imp.Series.Title, added), nil
}

// merge puts the import's unposted items (eps for TV, aligned with keys) into their batch. init
// sets up a batch merge creates. lookup reports that "following" needs the *arr: until
// settleFollowing runs, the batch keeps its previous value.
func (b *Batcher) merge(ctx context.Context, imp domain.Import, kind domain.Kind, key string,
	keys []domain.ItemKey, eps []domain.Episode, init func(*domain.Batch),
) (added int, lookup bool, err error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.clock.Now()
	posted, err := b.store.PostedSince(ctx, keys, now.Add(-b.cfg.Reannounce))
	if err != nil {
		return 0, false, err
	}
	batches, err := b.loaded(ctx)
	if err != nil {
		return 0, false, err
	}
	var bt *domain.Batch
	if cur, ok := batches[key]; ok {
		bt = clone(cur)
	} else {
		bt = &domain.Batch{Key: key, Source: imp.Source, Kind: kind, First: now}
		if kind == domain.KindTV {
			s := *imp.Series
			bt.Series = &s
		}
		if init != nil {
			init(bt)
		}
	}

	for i, k := range keys {
		if _, announced := runEpisodes(bt)[k]; posted[k] || announced {
			continue
		}
		added++
		if kind == domain.KindTV {
			if bt.Episodes == nil {
				bt.Episodes = map[domain.ItemKey]domain.Episode{}
			}
			bt.Episodes[k] = eps[i]
		} else {
			if bt.Movies == nil {
				bt.Movies = map[domain.ItemKey]domain.Movie{}
			}
			bt.Movies[k] = *imp.Movie
		}
	}
	// Like the reference, even an import that adds nothing counts as activity on the batch.
	bt.Last = now
	switch {
	case kind != domain.KindTV || bt.Len() == 0:
	case b.cfg.runs():
		bt.Following = !bt.Backlog // by air date, decided in addRuns
	default:
		if recent, _ := b.recent(bt, now); recent {
			bt.Following = true
		} else {
			lookup = true
		}
	}
	return added, lookup, b.put(ctx, bt)
}

// runEpisodes are the episodes bt's run announced (nil without a run): the ledger normally
// filters them too, but the run holds them even when a ledger write failed.
func runEpisodes(bt *domain.Batch) map[domain.ItemKey]domain.Episode {
	if bt.Run == nil {
		return nil
	}
	return bt.Run.Episodes
}

// settleFollowing decides "following" for the batch as it is now, given the *arr's series.
func (b *Batcher) settleFollowing(ctx context.Context, key string, detail *domain.SeriesDetail, lookupErr error) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	cur := b.batches[key]
	if cur == nil { // sent and gone meanwhile
		return nil
	}
	recent, perSeason := b.recent(cur, b.clock.Now())
	following := recent
	switch {
	case recent:
	case lookupErr != nil:
		// The patient choice: the longer quiet window.
		slog.Warn("following: series lookup failed, treating as back catalog",
			"source", cur.Source, "series", cur.Series.Title, "err", lookupErr)
	default:
		// Counts are after the import, so more files than the batch brings means some were there before.
		following = true
		for season, n := range perSeason {
			if detail.Seasons[season].EpisodeFileCount <= n {
				following = false
			}
		}
	}
	if following == cur.Following {
		return nil
	}
	bt := clone(cur)
	bt.Following = following
	return b.put(ctx, bt)
}

// recent reports whether every episode in the batch aired within FollowingWindow (weekly
// episodes, a premiere, a same-day season drop), and counts its episodes per season.
func (b *Batcher) recent(bt *domain.Batch, now time.Time) (bool, map[int]int) {
	recent := true
	perSeason := map[int]int{}
	for _, e := range bt.Episodes {
		if !b.recentEpisode(e, now) {
			recent = false
		}
		perSeason[e.Season]++
	}
	return recent, perSeason
}

// recentEpisode: aired within FollowingWindow, or due to air within FollowingEarly (streaming
// premieres often land hours before the air date the *arr has).
func (b *Batcher) recentEpisode(e domain.Episode, now time.Time) bool {
	age := now.Sub(e.Aired)
	return !e.Aired.IsZero() && age >= -b.cfg.FollowingEarly && age < b.cfg.FollowingWindow
}

// Due returns snapshots of the batches ready to send now (all non-empty ones when force, ignoring
// NotBefore), ordered by key. Each batch it returns is handed out: it isn't returned again, even
// when forced, until Done reports on it, so the caller must call Done for every one. On error it
// hands out nothing.
func (b *Batcher) Due(ctx context.Context, force bool) ([]*domain.Batch, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.clock.Now()
	batches, err := b.loaded(ctx)
	if err != nil {
		return nil, fmt.Errorf("due: %w", err)
	}
	var out []*domain.Batch
	for _, key := range slices.Sorted(maps.Keys(batches)) {
		bt := batches[key]
		if _, sending := b.inflight[key]; sending {
			continue
		}
		if bt.Len() == 0 {
			b.endRun(ctx, bt, now)
			continue
		}
		if !force && !b.due(bt, now) {
			continue
		}
		if bt, err = b.dropPosted(ctx, bt, now); err != nil {
			return nil, fmt.Errorf("due: %w", err)
		}
		if bt.Len() == 0 {
			continue
		}
		out = append(out, clone(bt))
	}
	// Only now that nothing can fail: a batch marked in flight but never returned would be
	// skipped by every later Due, with no Done to release it.
	for _, bt := range out {
		b.inflight[bt.Key] = bt.Keys()
	}
	return out, nil
}

func (b *Batcher) due(bt *domain.Batch, now time.Time) bool {
	if now.Before(bt.NotBefore) {
		return false
	}
	if bt.Backlog {
		settled := now.Sub(bt.Last) >= b.cfg.Backlog.Settle
		if bt.Run == nil && b.cfg.Backlog.Mode == Complete {
			// The app holds it while the *arr has more queued (AwaitsQueue), up to Backlog.MaxHold.
			return settled || now.Sub(bt.First) >= b.cfg.Backlog.MaxHold
		}
		return settled || now.Sub(bt.First) >= b.cfg.MaxHold
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

// AwaitsQueue reports that bt's first card waits for the *arr's queue to empty: a Complete run
// not yet announced and within Backlog.MaxHold. The caller checks the queue and reports Queued.
func (b *Batcher) AwaitsQueue(bt *domain.Batch) bool {
	return bt.Backlog && bt.Run == nil && b.cfg.Backlog.Mode == Complete &&
		b.clock.Now().Sub(bt.First) < b.cfg.Backlog.MaxHold
}

// endRun deletes a run with nothing pending once it has been idle for Backlog.Idle: the next
// back-catalog episode of the series starts a new run and a new card. Callers hold b.mu.
func (b *Batcher) endRun(ctx context.Context, bt *domain.Batch, now time.Time) {
	if bt.Run == nil || now.Sub(bt.Last) < b.cfg.Backlog.Idle {
		return
	}
	if err := b.store.DeleteBatch(ctx, bt.Key); err != nil {
		slog.Warn("ending a run failed; trying again later", "batch", bt.Key, "err", err)
		return
	}
	delete(b.batches, bt.Key)
}

// dropPosted removes items the ledger says went out: left behind when a Done marked them posted
// but couldn't save the batch. The ledger is the authority, so they are never sent twice.
func (b *Batcher) dropPosted(ctx context.Context, bt *domain.Batch, now time.Time) (*domain.Batch, error) {
	posted, err := b.store.PostedSince(ctx, bt.Keys(), now.Add(-b.cfg.Reannounce))
	if err != nil || len(posted) == 0 {
		return bt, err
	}
	slog.Warn("dropping items already posted", "batch", bt.Key, "items", len(posted))
	bt = clone(bt)
	for k := range posted {
		delete(bt.Episodes, k)
		delete(bt.Movies, k)
	}
	return bt, b.put(ctx, bt)
}

// Done records a delivery attempt of a batch Due handed out. sent are the keys that went out
// (they are marked posted even when the outcome is Failed part-way through).
func (b *Batcher) Done(ctx context.Context, key string, sent []domain.ItemKey, outcome Outcome) error {
	return b.DoneMessage(ctx, key, sent, outcome, nil)
}

// DoneMessage is Done for a back-catalog batch: sent episodes join its run, and msg (when not
// nil) becomes the card the run edits; a zero msg means the card can't be edited any more.
func (b *Batcher) DoneMessage(ctx context.Context, key string, sent []domain.ItemKey, outcome Outcome,
	msg *domain.Message,
) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.clock.Now()
	handed, ok := b.inflight[key]
	if !ok {
		return fmt.Errorf("done %s: not handed out by Due", key)
	}
	delete(b.inflight, key)

	// Two writes that can't share a transaction; each one alone keeps sent items from going out
	// again: the ledger through dropPosted, the batch by no longer holding them.
	var errs []error
	if len(sent) > 0 {
		if err := b.store.MarkPosted(ctx, sent, now); err != nil {
			errs = append(errs, fmt.Errorf("mark posted: %w", err))
		}
		b.prune(ctx, now)
	}
	if cur := b.batches[key]; cur != nil {
		if err := b.put(ctx, b.outcome(cur, handed, sent, outcome, msg, now)); err != nil {
			errs = append(errs, err)
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("done %s: %w", key, errors.Join(errs...))
	}
	return nil
}

// outcome returns the batch after a delivery attempt of the handed-out items. Items that arrived
// during the attempt are in cur but not in handed.
func (b *Batcher) outcome(cur *domain.Batch, handed, sent []domain.ItemKey, outcome Outcome,
	msg *domain.Message, now time.Time,
) *domain.Batch {
	bt := clone(cur)
	gone := slices.Clone(sent)
	if bt.Backlog && (len(sent) > 0 || msg != nil) {
		if bt.Run == nil {
			bt.Run = &domain.Run{}
		}
		if bt.Run.Episodes == nil {
			bt.Run.Episodes = map[domain.ItemKey]domain.Episode{}
		}
		for _, k := range sent {
			if e, ok := bt.Episodes[k]; ok {
				bt.Run.Episodes[k] = e
			}
		}
		if msg != nil {
			bt.Run.Message = *msg
		}
	}
	switch outcome {
	case Queued:
		bt.NotBefore = now.Add(b.cfg.Backlog.Settle)
	case Waiting:
		bt.MediaChecks++
		bt.NotBefore = now.Add(b.cfg.MediaWait)
	case Failed:
		bt.Tries++
		if bt.Tries < b.cfg.RetryMax {
			slog.Warn("send failed, retrying", "batch", bt.Key, "tries", bt.Tries, "in", b.cfg.RetryInterval)
			bt.NotBefore = now.Add(b.cfg.RetryInterval)
			break
		}
		slog.Error("send failed, giving up", "batch", bt.Key, "tries", bt.Tries)
		gone = append(gone, handed...)
		fallthrough // as in the reference, what arrived during the last try starts a fresh window
	case Posted:
		bt.First, bt.MediaChecks, bt.Tries, bt.NotBefore = now, 0, 0, time.Time{}
	case Aborted:
	}
	for _, k := range gone {
		delete(bt.Episodes, k)
		delete(bt.Movies, k)
	}
	return bt
}

// prune trims the ledger to the reannounce window, at most once per pruneEvery. It is
// housekeeping: a failure is logged and tried again next time.
func (b *Batcher) prune(ctx context.Context, now time.Time) {
	if now.Sub(b.lastPrune) < pruneEvery {
		return
	}
	b.lastPrune = now
	if _, err := b.store.PrunePosted(ctx, now.Add(-b.cfg.Reannounce)); err != nil {
		slog.Warn("pruning the posted ledger failed", "err", err)
	}
}

// loaded returns the batches, loading them from the store on first use. Callers hold b.mu.
func (b *Batcher) loaded(ctx context.Context) (map[string]*domain.Batch, error) {
	if b.batches == nil {
		m, err := b.store.LoadBatches(ctx)
		if err != nil {
			return nil, err
		}
		if m == nil {
			m = map[string]*domain.Batch{}
		}
		b.batches = m
	}
	return b.batches, nil
}

// put writes bt through to the store (deleting it when empty and not in a run), then to memory,
// so memory never holds what the store doesn't. Callers hold b.mu and have loaded the batches.
func (b *Batcher) put(ctx context.Context, bt *domain.Batch) error {
	if bt.Len() == 0 && bt.Run == nil {
		if _, ok := b.batches[bt.Key]; !ok {
			return nil
		}
		if err := b.store.DeleteBatch(ctx, bt.Key); err != nil {
			return err
		}
		delete(b.batches, bt.Key)
		return nil
	}
	if err := b.store.SaveBatch(ctx, bt); err != nil {
		return err
	}
	b.batches[bt.Key] = bt
	return nil
}

func clone(bt *domain.Batch) *domain.Batch {
	c := *bt
	if bt.Series != nil {
		s := *bt.Series
		c.Series = &s
	}
	c.Episodes = maps.Clone(bt.Episodes)
	c.Movies = maps.Clone(bt.Movies)
	if bt.Run != nil {
		r := *bt.Run
		r.Episodes = maps.Clone(bt.Run.Episodes)
		c.Run = &r
	}
	return &c
}
