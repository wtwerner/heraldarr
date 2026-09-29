// Package app wires the packages into the running service: webhook ingest, the flush loop and the
// delivery pipeline (reference: send_batch, plex_lookup and flush).
package app

import (
	"context"
	"log/slog"
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
	Log        *slog.Logger
}

// BasicAuth protects the webhook and control endpoints.
type BasicAuth struct{ Username, Password string }

type App struct {
	Deps
	batcher *batcher.Batcher
	kick    chan bool // flush now; true = force
}

func New(d Deps) *App {
	kinds := make(map[string]domain.Kind, len(d.Sources))
	for name, s := range d.Sources {
		kinds[name] = s.Kind
	}
	if d.Log == nil {
		d.Log = slog.Default()
	}
	a := &App{Deps: d, kick: make(chan bool, 1)}
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

// Flush delivers every due batch once. Run calls it; tests call it directly.
func (a *App) Flush(ctx context.Context, force bool) {
	due, err := a.batcher.Due(ctx, force)
	if err != nil {
		a.Log.Error("flush: loading due batches", "err", err)
		return
	}
	for i, b := range due {
		if ctx.Err() != nil {
			// Shutting down: release what Due handed out, untouched, for the next start.
			a.release(ctx, due[i:])
			return
		}
		sent, outcome, err := a.deliver(ctx, b, force, nil, true)
		if ctx.Err() != nil {
			// Interrupted mid-delivery isn't a failure: keep what went out, retry the rest later.
			a.done(ctx, b, sent, batcher.Aborted)
			a.release(ctx, due[i+1:])
			return
		}
		if err != nil {
			a.Log.Warn("delivery failed", "batch", batchName(b), "err", err, "try", b.Tries+1)
		}
		a.done(ctx, b, sent, outcome)
	}
}

func (a *App) release(ctx context.Context, batches []*domain.Batch) {
	for _, b := range batches {
		a.done(ctx, b, nil, batcher.Aborted)
	}
}

// done reports an attempt to the batcher; it must happen even while shutting down.
func (a *App) done(ctx context.Context, b *domain.Batch, sent []domain.ItemKey, outcome batcher.Outcome) {
	if err := a.batcher.Done(context.WithoutCancel(ctx), b.Key, sent, outcome); err != nil {
		a.Log.Error("recording delivery outcome", "batch", batchName(b), "err", err)
	}
}

func batchName(b *domain.Batch) string {
	if b.Series != nil {
		return b.Series.Title
	}
	return b.Source + " movies"
}
