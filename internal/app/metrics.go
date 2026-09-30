package app

import (
	"fmt"
	"io"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Import results counted by heraldarr_imports_total.
const (
	importQueued           = "queued"            // at least one item went into a batch
	importUpgrade          = "upgrade"           // never announced
	importAlreadyAnnounced = "already_announced" // every item posted within the reannounce window
	importIgnored          = "ignored"           // an event other than On Import (Test, Grab…)
	importInvalid          = "invalid"           // a payload that couldn't be read or decoded
	importError            = "error"             // could not be queued (store failure)
)

// metrics is what GET /metrics reports, in the Prometheus text format. A few counters don't need
// the client library.
type metrics struct {
	imports, posts, failures, waits, lastPost *family
	pending                                   *family // set from the store at each scrape
}

func newMetrics(sources []string) *metrics {
	m := &metrics{
		imports:  newFamily("heraldarr_imports_total", "counter", "Webhook imports received, by result.", "source", "result"),
		posts:    newFamily("heraldarr_posts_total", "counter", "Cards posted, by the layout Discord accepted.", "source", "destination", "layout"),
		failures: newFamily("heraldarr_delivery_failures_total", "counter", "Batch deliveries that failed and will be retried or dropped.", "source"),
		pending:  newFamily("heraldarr_pending_batches", "gauge", "Batches waiting to be posted."),
		waits:    newFamily("heraldarr_media_server_waits_total", "counter", "Deliveries postponed because the media server didn't have the items yet."),
		lastPost: newFamily("heraldarr_last_post_timestamp_seconds", "gauge", "Unix time of the last card posted since start (0: none yet)."),
	}
	// Every known series starts at 0, so rates and alerts work before the first event.
	for _, s := range sources {
		for _, r := range []string{importQueued, importUpgrade, importAlreadyAnnounced, importIgnored, importInvalid, importError} {
			m.imports.add(0, s, r)
		}
		m.failures.add(0, s)
	}
	m.waits.add(0)
	m.lastPost.set(0)
	return m
}

func (m *metrics) write(w io.Writer) {
	for _, f := range []*family{m.imports, m.posts, m.failures, m.pending, m.waits, m.lastPost} {
		f.write(w)
	}
}

func (a *App) metricsHandler(w http.ResponseWriter, r *http.Request) {
	batches, err := a.Store.LoadBatches(r.Context())
	if err != nil {
		reply(w, http.StatusServiceUnavailable, map[string]any{"error": "store unavailable"})
		return
	}
	pending := 0
	for _, b := range batches {
		if b.Len() > 0 { // a back-catalog run with nothing waiting isn't pending
			pending++
		}
	}
	a.metrics.pending.set(float64(pending))
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	a.metrics.write(w)
}

func (m *metrics) posted(source, destination, layout string, at time.Time) {
	m.posts.add(1, source, destination, layout)
	m.lastPost.set(float64(at.Unix()))
}

// family is one metric name with its samples, keyed by their rendered labels (`{source="x"}`).
type family struct {
	name, kind, help string
	labels           []string

	mu      sync.Mutex
	samples map[string]float64
}

func newFamily(name, kind, help string, labels ...string) *family {
	return &family{name: name, kind: kind, help: help, labels: labels, samples: map[string]float64{}}
}

func (f *family) add(v float64, values ...string) {
	k := f.key(values)
	f.mu.Lock()
	f.samples[k] += v
	f.mu.Unlock()
}

func (f *family) set(v float64, values ...string) {
	k := f.key(values)
	f.mu.Lock()
	f.samples[k] = v
	f.mu.Unlock()
}

func (f *family) key(values []string) string {
	if len(values) != len(f.labels) {
		panic(fmt.Sprintf("%s: %d label values for %d labels", f.name, len(values), len(f.labels)))
	}
	if len(values) == 0 {
		return ""
	}
	parts := make([]string, len(values))
	for i, v := range values {
		parts[i] = f.labels[i] + `="` + labelEscaper.Replace(v) + `"`
	}
	return "{" + strings.Join(parts, ",") + "}"
}

var labelEscaper = strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)

func (f *family) write(w io.Writer) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.samples) == 0 {
		return
	}
	_, _ = fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s %s\n", f.name, f.help, f.name, f.kind)
	for _, k := range slices.Sorted(maps.Keys(f.samples)) {
		_, _ = fmt.Fprintf(w, "%s%s %s\n", f.name, k, strconv.FormatFloat(f.samples[k], 'f', -1, 64))
	}
}
