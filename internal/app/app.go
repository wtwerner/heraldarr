// Package app wires the packages into the running service: webhook ingest, the flush loop and the
// delivery pipeline (reference: send_batch, plex_lookup and flush).
package app

import (
	"context"
	"log/slog"
	"maps"
	"net/http"
	"slices"
	"time"

	"github.com/wtwerner/heraldarr/internal/batcher"
	"github.com/wtwerner/heraldarr/internal/domain"
)

// RottenTomatoes resolves the Rotten Tomatoes link for a title (enrich.Wikidata in production).
type RottenTomatoes interface {
	RottenTomatoes(ctx context.Context, imdbID, title string) string
}

// Source is one configured *arr instance and where its cards go.
type Source struct {
	Kind        domain.Kind
	Arr         domain.ArrClient
	Destination domain.Destination
	Style       domain.Style
}

// Deps is everything App needs; FromConfig builds it from the configuration.
type Deps struct {
	Clock      domain.Clock
	Store      domain.Store
	Media      domain.MediaServer // nil: no media server configured
	Notifier   domain.Notifier
	RT         RottenTomatoes
	Sources    map[string]Source
	Timing     batcher.Config
	DigestFrom int
	WaitChecks int // media server checks before posting without a deep link
	Auth       *BasicAuth
	Heartbeat  string // GET after each flush without a store error, at most once a minute ("": none)
	Log        *slog.Logger
}

// BasicAuth protects the webhook and control endpoints.
type BasicAuth struct{ Username, Password string }

type App struct {
	Deps
	batcher   *batcher.Batcher
	kick      chan bool // flush now; true = force
	metrics   *metrics
	heartbeat *heartbeat // nil: none configured
}

func New(d Deps) *App {
	kinds := make(map[string]domain.Kind, len(d.Sources))
	for name, s := range d.Sources {
		kinds[name] = s.Kind
	}
	if d.Log == nil {
		d.Log = slog.Default()
	}
	a := &App{Deps: d, kick: make(chan bool, 1), metrics: newMetrics(slices.Collect(maps.Keys(kinds)))}
	if d.Heartbeat != "" {
		a.heartbeat = &heartbeat{url: d.Heartbeat, client: &http.Client{Timeout: 5 * time.Second}} // short: it holds up the flush loop
	}
	a.batcher = batcher.New(d.Timing, d.Clock, d.Store, kinds, func(source string) domain.ArrClient {
		return d.Sources[source].Arr
	})
	return a
}

// FlushInterval is how often due batches are checked.
const FlushInterval = 30 * time.Second

// Run flushes due batches until ctx ends: every FlushInterval, and whenever Kick is called.
func (a *App) Run(ctx context.Context) {
	t := time.NewTicker(FlushInterval)
	defer t.Stop()
	a.Flush(ctx, false) // whatever came due while the service was down
	for {
		force := false
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case force = <-a.kick:
		}
		a.Flush(ctx, force)
	}
}

// Kick asks Run to flush now (force: everything pending, ignoring quiet windows and waits).
func (a *App) Kick(force bool) {
	select {
	case a.kick <- force:
	default: // a flush is already queued
	}
}

// Flush delivers every due batch once, then calls the heartbeat URL if the store worked
// throughout. Run calls it; tests call it directly.
func (a *App) Flush(ctx context.Context, force bool) {
	if a.flush(ctx, force) {
		a.beat(ctx)
	}
}

// flush reports whether it ran to the end without a store error (false when shutting down).
func (a *App) flush(ctx context.Context, force bool) bool {
	due, err := a.batcher.Due(ctx, force)
	if err != nil {
		a.Log.Error("flush: loading due batches", "err", err)
		return false
	}
	ok := true
	for i, b := range due {
		if ctx.Err() != nil {
			// Shutting down: release what Due handed out, untouched, for the next start.
			a.release(ctx, due[i:])
			return false
		}
		sent, outcome, msg, err := a.deliver(ctx, b, force, nil, true)
		if ctx.Err() != nil {
			// Interrupted mid-delivery isn't a failure: keep what went out, retry the rest later.
			a.done(ctx, b, sent, batcher.Aborted, msg)
			a.release(ctx, due[i+1:])
			return false
		}
		switch outcome {
		case batcher.Failed:
			a.metrics.failures.add(1, b.Source)
			a.Log.Warn("delivery failed", "batch", batchName(b), "err", err, "try", b.Tries+1)
		case batcher.Waiting:
			a.metrics.waits.add(1)
		}
		ok = a.done(ctx, b, sent, outcome, msg) && ok
	}
	return ok
}

func (a *App) release(ctx context.Context, batches []*domain.Batch) {
	for _, b := range batches {
		a.done(ctx, b, nil, batcher.Aborted, nil)
	}
}

// done reports an attempt to the batcher; it must happen even while shutting down. It returns
// false when the store couldn't record it.
func (a *App) done(ctx context.Context, b *domain.Batch, sent []domain.ItemKey, outcome batcher.Outcome,
	msg *domain.Message,
) bool {
	if err := a.batcher.DoneMessage(context.WithoutCancel(ctx), b.Key, sent, outcome, msg); err != nil {
		a.Log.Error("recording delivery outcome", "batch", batchName(b), "err", err)
		return false
	}
	return true
}

func batchName(b *domain.Batch) string {
	if b.Series != nil {
		return b.Series.Title
	}
	return b.Source + " movies"
}
