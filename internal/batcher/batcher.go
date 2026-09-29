// Package batcher decides what waits and what goes out: it groups imports into batches (one per
// series per source, one per movie source), applies the quiet windows, and tracks delivery
// outcomes. Reference: add_import, tv_following, due, flush and mark_posted in the reference
// implementation. All state is persisted through domain.Store; time comes from domain.Clock.
package batcher

import (
	"context"
	"time"

	"github.com/wtwerner/heraldarr/internal/domain"
)

// implemented gates the parity and timing tests until Wave 1 lands this package (issue: batcher).
const implemented = false

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

type Batcher struct{}

// New creates a batcher. kinds maps source name -> kind; arrs returns the API client used to
// decide "following" for TV (every season in the batch already had files before it).
func New(cfg Config, clock domain.Clock, store domain.Store, kinds map[string]domain.Kind,
	arrs func(source string) domain.ArrClient,
) *Batcher {
	panic("not implemented")
}

// Add queues one import. Upgrades and items posted within Reannounce are skipped. Returns a short
// line for the log ("sonarr Show: +2", "upgrade, ignored", "sonarr Show: already announced").
func (b *Batcher) Add(ctx context.Context, imp domain.Import) (string, error) {
	panic("not implemented")
}

// Due returns snapshots of the batches ready to send now (all non-empty ones when force, ignoring
// NotBefore), ordered by key.
func (b *Batcher) Due(ctx context.Context, force bool) ([]*domain.Batch, error) {
	panic("not implemented")
}

// Done records a delivery attempt of the batch with this key. sent are the keys that went out
// (they are marked posted even when the outcome is Failed part-way through).
func (b *Batcher) Done(ctx context.Context, key string, sent []domain.ItemKey, outcome Outcome) error {
	panic("not implemented")
}
