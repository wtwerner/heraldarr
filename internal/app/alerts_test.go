package app_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wtwerner/heraldarr/internal/alert"
	"github.com/wtwerner/heraldarr/internal/app"
	"github.com/wtwerner/heraldarr/internal/domain"
	"github.com/wtwerner/heraldarr/internal/testkit"
)

// health is a fake HealthReader: the checks the app reports failing, or an error.
type health struct {
	mu      sync.Mutex
	checks  []string
	message string
	err     error
	calls   int
}

func (h *health) Health(context.Context) ([]domain.HealthCheck, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.calls++
	if h.err != nil {
		return nil, h.err
	}
	out := make([]domain.HealthCheck, len(h.checks))
	for i, c := range h.checks {
		out[i] = domain.HealthCheck{Check: c, Level: domain.LevelWarning, Message: h.message}
	}
	return out, nil
}

func (h *health) set(checks ...string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.checks = checks
}

type alertEnv struct {
	app    *app.App
	srv    http.Handler
	clock  *testkit.Clock
	out    *capture
	store  *testkit.MemStore
	health *health
}

var alertsDest = domain.Destination{Name: "server", WebhookURL: "https://discord.invalid/server"}

func newAlertEnv(t *testing.T, opts ...func(*app.Alerts)) *alertEnv {
	t.Helper()
	return buildAlertEnv(t, nil, opts...)
}

// brokenAlerts is a MemStore whose alert calls fail while err is set.
type brokenAlerts struct {
	*testkit.MemStore
	mu  sync.Mutex
	err error
}

func (b *brokenAlerts) fail(err error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.err = err
}

func (b *brokenAlerts) broken() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.err
}

func (b *brokenAlerts) LoadAlerts(ctx context.Context) (map[string]*domain.Alert, error) {
	if err := b.broken(); err != nil {
		return nil, err
	}
	return b.MemStore.LoadAlerts(ctx)
}

func (b *brokenAlerts) SaveAlert(ctx context.Context, a *domain.Alert) error {
	if err := b.broken(); err != nil {
		return err
	}
	return b.MemStore.SaveAlert(ctx, a)
}

// buildAlertEnv: deps, when set, adjusts the App's Deps (whose Store starts as the env's MemStore).
func buildAlertEnv(t *testing.T, deps func(*app.Deps), opts ...func(*app.Alerts)) *alertEnv {
	t.Helper()
	e := &alertEnv{
		clock: testkit.NewClock(time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)), out: &capture{},
		store: testkit.NewMemStore(), health: &health{},
	}
	al := &app.Alerts{
		Destination: alertsDest,
		Sources:     map[string]domain.HealthReader{"prowlarr": e.health, "sonarr": nil},
		Config: alert.Config{
			Hold: 30 * time.Minute, Recheck: 15 * time.Minute, RetryInterval: 2 * time.Minute, RetryMax: 3, Updates: true,
		},
		Restored: "edit",
	}
	for _, o := range opts {
		o(al)
	}
	d := app.Deps{
		Clock: e.clock, Store: e.store, Notifier: e.out, Sources: map[string]app.Source{}, Timing: timing,
		Alerts: al, Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	if deps != nil {
		deps(&d)
	}
	e.app = app.New(d)
	e.srv = e.app.Handler()
	return e
}

func (e *alertEnv) hook(t *testing.T, source, body string) int {
	t.Helper()
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/hook/"+source, strings.NewReader(body))
	rec := httptest.NewRecorder()
	e.srv.ServeHTTP(rec, req)
	return rec.Code
}

func (e *alertEnv) flush() { e.app.Flush(context.Background(), false) }

const (
	indexersDown = `{"eventType":"Health","instanceName":"Prowlarr","level":"warning","type":"IndexerStatusCheck",
		"message":"Indexers unavailable due to failures: Example","wikiUrl":"https://wiki.example.org/indexers"}`
	indexersBack = `{"eventType":"HealthRestored","instanceName":"Prowlarr","level":"warning","type":"IndexerStatusCheck",
		"message":"Indexers unavailable due to failures: Example"}`
)

func v2Content(l domain.Layout) string {
	box := l.Body["components"].([]any)[0].(map[string]any)
	var b strings.Builder
	for _, c := range box["components"].([]any) {
		if s, ok := c.(map[string]any)["content"].(string); ok {
			b.WriteString(s + "\n")
		}
	}
	return b.String()
}

func TestAlertBlipIsNeverPosted(t *testing.T) {
	e := newAlertEnv(t)
	e.health.set("IndexerStatusCheck")
	if code := e.hook(t, "prowlarr", indexersDown); code != http.StatusOK {
		t.Fatalf("HTTP %d", code)
	}
	e.clock.Advance(10 * time.Minute)
	e.flush()
	e.hook(t, "prowlarr", indexersBack)
	e.clock.Advance(time.Hour)
	e.flush()
	if n := e.out.count(); n != 0 {
		t.Fatalf("%d posts for a 10-minute blip", n)
	}
	if all, _ := e.store.LoadAlerts(context.Background()); len(all) != 0 {
		t.Errorf("left in the store: %v", all)
	}
}

func TestAlertLifecycle(t *testing.T) {
	e := newAlertEnv(t)
	e.health.set("IndexerStatusCheck")
	e.hook(t, "prowlarr", indexersDown)
	e.clock.Advance(29 * time.Minute)
	e.flush()
	if e.out.count() != 0 {
		t.Fatal("posted inside the hold")
	}
	e.clock.Advance(time.Minute)
	e.flush()
	if e.out.count() != 1 {
		t.Fatalf("%d posts after the hold, want 1", e.out.count())
	}
	p := e.out.posts[0]
	if p.dest.Name != "server" || !strings.Contains(v2Content(p.layouts[0]), "⚠️ Warning · Prowlarr") ||
		!strings.Contains(v2Content(p.layouts[0]), "Indexers unavailable due to failures: Example") {
		t.Errorf("posted to %s: %s", p.dest.Name, v2Content(p.layouts[0]))
	}
	e.flush()
	if e.out.count() != 1 {
		t.Fatal("posted twice")
	}
	e.clock.Advance(time.Hour)
	e.hook(t, "prowlarr", indexersBack)
	e.flush()
	if len(e.out.edits) != 1 || e.out.edits[0].id != "1" || e.out.edits[0].layout.Name != "v2" ||
		!strings.Contains(v2Content(e.out.edits[0].layout), "✅ Resolved · Prowlarr") {
		t.Fatalf("edits = %+v", e.out.edits)
	}
	if e.out.count() != 1 {
		t.Error("restored posted a message with restored: edit")
	}
	if all, _ := e.store.LoadAlerts(context.Background()); len(all) != 0 {
		t.Errorf("left in the store: %v", all)
	}
}

func TestAlertRestoredPost(t *testing.T) {
	e := newAlertEnv(t, func(a *app.Alerts) { a.Restored = "post" })
	e.health.set("IndexerStatusCheck")
	e.hook(t, "prowlarr", indexersDown)
	e.clock.Advance(30 * time.Minute)
	e.flush()
	e.hook(t, "prowlarr", indexersBack)
	e.flush()
	if e.out.count() != 2 || len(e.out.edits) != 0 || !strings.Contains(v2Content(e.out.posts[1].layouts[0]), "✅ Resolved") {
		t.Fatalf("posts = %d, edits = %d", e.out.count(), len(e.out.edits))
	}
}

func TestAlertDeletedCardStillCloses(t *testing.T) {
	e := newAlertEnv(t)
	e.health.set("IndexerStatusCheck")
	e.hook(t, "prowlarr", indexersDown)
	e.clock.Advance(30 * time.Minute)
	e.flush()
	e.out.editErr = domain.ErrNotFound
	e.hook(t, "prowlarr", indexersBack)
	e.flush()
	if all, _ := e.store.LoadAlerts(context.Background()); len(all) != 0 {
		t.Errorf("an alert whose card was deleted stays: %v", all)
	}
}

func TestAlertGoneFromTheAppIsNotPosted(t *testing.T) {
	e := newAlertEnv(t)
	e.health.set("DownloadClientCheck") // something else fails; the alert's own check passes
	e.hook(t, "prowlarr", indexersDown) // its restore never reached heraldarr
	e.clock.Advance(30 * time.Minute)
	e.flush()
	if e.out.count() != 0 || e.health.calls != 1 {
		t.Errorf("posts = %d, health calls = %d", e.out.count(), e.health.calls)
	}
}

func TestAlertPostsWhenTheAppCantBeAsked(t *testing.T) {
	e := newAlertEnv(t)
	e.health.err = errors.New("connection refused")
	e.hook(t, "prowlarr", indexersDown)
	e.clock.Advance(30 * time.Minute)
	e.flush()
	if e.out.count() != 1 {
		t.Errorf("posts = %d, want 1", e.out.count())
	}
}

func TestAlertRecheckResolvesAMissedRestore(t *testing.T) {
	e := newAlertEnv(t)
	e.health.set("IndexerStatusCheck")
	e.hook(t, "prowlarr", indexersDown)
	e.clock.Advance(30 * time.Minute)
	e.flush()
	e.clock.Advance(15 * time.Minute)
	e.flush() // still failing
	if len(e.out.edits) != 0 {
		t.Fatal("resolved while still failing")
	}
	e.health.set("DownloadClientCheck") // the alert's check passes; the Health Restored event was lost
	e.clock.Advance(15 * time.Minute)
	e.flush() // recheck: gone
	e.flush() // resolve
	if len(e.out.edits) != 1 || !strings.Contains(v2Content(e.out.edits[0].layout), "Lasted 1 h") {
		t.Fatalf("edits = %+v", e.out.edits)
	}
}

func TestAlertErrorsPostAtTheNextFlush(t *testing.T) {
	e := newAlertEnv(t)
	e.hook(t, "sonarr", `{"eventType":"Health","instanceName":"Sonarr","level":"error","type":"DownloadClientCheck",
		"message":"Unable to communicate with the download client"}`)
	e.flush()
	if e.out.count() != 1 || !strings.Contains(v2Content(e.out.posts[0].layouts[0]), "🛑 Error · Sonarr") {
		t.Fatalf("posts = %d", e.out.count())
	}
}

func TestAlertUpdate(t *testing.T) {
	e := newAlertEnv(t)
	e.hook(t, "sonarr", `{"eventType":"ApplicationUpdate","instanceName":"Sonarr","message":"Sonarr updated",
		"previousVersion":"4.0.1","newVersion":"4.0.2"}`)
	e.flush()
	if e.out.count() != 1 || !strings.Contains(v2Content(e.out.posts[0].layouts[0]), "4.0.1 → 4.0.2") {
		t.Fatalf("posts = %d", e.out.count())
	}
}

func TestAlertHookRouting(t *testing.T) {
	e := newAlertEnv(t)
	for _, c := range []struct {
		name, source, body string
		code               int
	}{
		{"unknown source", "lidarr", indexersDown, http.StatusNotFound},
		{"prowlarr test event", "prowlarr", `{"eventType":"Test","instanceName":"Prowlarr"}`, http.StatusOK},
		{"prowlarr grab", "prowlarr", `{"eventType":"Grab"}`, http.StatusOK},
		{"bad alert", "prowlarr", `{"eventType":"Health","level":"warning"}`, http.StatusBadRequest},
	} {
		if code := e.hook(t, c.source, c.body); code != c.code {
			t.Errorf("%s: HTTP %d, want %d", c.name, code, c.code)
		}
	}
	if !strings.Contains(metricsBody(t, e.srv), `heraldarr_alerts_total{source="prowlarr",result="invalid"} 1`) {
		t.Error("bad alert not counted as invalid")
	}
	// A source in neither routes nor alerts is unknown.
	o := newAlertEnv(t, func(a *app.Alerts) { delete(a.Sources, "sonarr") })
	if code := o.hook(t, "sonarr", indexersDown); code != http.StatusNotFound {
		t.Errorf("source in neither routes nor alerts: HTTP %d", code)
	}
	if all, _ := o.store.LoadAlerts(context.Background()); len(all) != 0 {
		t.Errorf("stored %v", all)
	}
}

func TestAlertMetrics(t *testing.T) {
	e := newAlertEnv(t)
	e.health.set("IndexerStatusCheck")
	e.hook(t, "prowlarr", indexersDown)
	e.hook(t, "prowlarr", indexersBack)
	e.clock.Advance(time.Hour)
	e.flush()
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/metrics", nil)
	rec := httptest.NewRecorder()
	e.srv.ServeHTTP(rec, req)
	body := rec.Body.String()
	for _, want := range []string{
		`heraldarr_alerts_total{source="prowlarr",result="held"} 1`,
		`heraldarr_alerts_total{source="prowlarr",result="cleared"} 1`,
		`heraldarr_alerts_dropped_total{source="prowlarr",reason="cleared"} 1`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics lack %s", want)
		}
	}
}

func TestAlertEditUsesTheAcceptedLayout(t *testing.T) {
	e := newAlertEnv(t)
	e.out.layout = "embed" // Discord took the fallback
	e.health.set("IndexerStatusCheck")
	e.hook(t, "prowlarr", indexersDown)
	e.clock.Advance(30 * time.Minute)
	e.flush()
	e.hook(t, "prowlarr", indexersBack)
	e.flush()
	if len(e.out.edits) != 1 || e.out.edits[0].layout.Name != "embed" {
		t.Fatalf("edits = %+v, want the embed layout", e.out.edits)
	}
}

func TestAlertFromASourceNoLongerInAlertsIsDropped(t *testing.T) {
	e := newAlertEnv(t)
	e.hook(t, "sonarr", `{"eventType":"Health","instanceName":"Sonarr","level":"warning","type":"IndexerStatusCheck","message":"m"}`)
	// Restarted with sonarr taken out of alerts.
	later := app.New(app.Deps{
		Clock: e.clock, Store: e.store, Notifier: e.out, Sources: map[string]app.Source{}, Timing: timing,
		Alerts: &app.Alerts{
			Destination: alertsDest, Sources: map[string]domain.HealthReader{"prowlarr": e.health},
			Config: alert.Config{Hold: 30 * time.Minute, RetryMax: 3},
		},
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	e.clock.Advance(30 * time.Minute)
	later.Flush(context.Background(), false)
	if e.out.count() != 0 {
		t.Error("posted an alert from a source no longer in alerts")
	}
	if all, _ := e.store.LoadAlerts(context.Background()); len(all) != 0 {
		t.Errorf("left in the store: %v", all)
	}
}

func (e *alertEnv) stored(t *testing.T) map[string]*domain.Alert {
	t.Helper()
	all, err := e.store.LoadAlerts(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return all
}

const alertKey = "prowlarr:health:IndexerStatusCheck"

// postedAlert runs an indexer warning through its hold and posts it.
func postedAlert(t *testing.T, opts ...func(*app.Alerts)) *alertEnv {
	t.Helper()
	e := newAlertEnv(t, opts...)
	e.health.set("IndexerStatusCheck")
	e.hook(t, "prowlarr", indexersDown)
	e.clock.Advance(30 * time.Minute)
	e.flush()
	if e.out.count() != 1 {
		t.Fatalf("%d posts after the hold", e.out.count())
	}
	return e
}

func TestAlertPostFailureRetriesThenDrops(t *testing.T) {
	e := newAlertEnv(t)
	e.health.set("IndexerStatusCheck")
	e.out.fail = errors.New("discord down")
	e.hook(t, "prowlarr", indexersDown)
	e.clock.Advance(30 * time.Minute)
	for try := 1; try <= 3; try++ {
		e.flush()
		a := e.stored(t)[alertKey]
		if try < 3 && (a == nil || a.Tries != try) {
			t.Fatalf("try %d: stored %+v; want kept for a retry", try, a)
		}
		e.clock.Advance(2 * time.Minute)
	}
	if a := e.stored(t)[alertKey]; a != nil {
		t.Errorf("kept after RetryMax failures: %+v", a)
	}
	body := metricsBody(t, e.srv)
	if !strings.Contains(body, `heraldarr_alerts_dropped_total{source="prowlarr",reason="failed"} 1`) {
		t.Errorf("drop not counted as failed:\n%s", body)
	}
}

func TestAlertResolveFailureRetries(t *testing.T) {
	for name, opt := range map[string]func(*app.Alerts){
		"edit": func(*app.Alerts) {},
		"post": func(a *app.Alerts) { a.Restored = "post" },
	} {
		t.Run(name, func(t *testing.T) {
			e := postedAlert(t, opt)
			e.out.editErr, e.out.fail = errors.New("discord down"), errors.New("discord down")
			e.hook(t, "prowlarr", indexersBack)
			e.flush()
			if a := e.stored(t)[alertKey]; a == nil || a.Tries != 1 || a.Resolved.IsZero() {
				t.Fatalf("stored %+v; want kept for a retry", a)
			}
			e.out.editErr, e.out.fail = nil, nil
			e.clock.Advance(2 * time.Minute)
			e.flush()
			if a := e.stored(t)[alertKey]; a != nil {
				t.Errorf("still stored after the retry worked: %+v", a)
			}
		})
	}
}

func TestAlertRefusedEditCloses(t *testing.T) {
	e := postedAlert(t)
	e.out.editErr = domain.ErrRefused
	e.hook(t, "prowlarr", indexersBack)
	e.flush()
	if all := e.stored(t); len(all) != 0 {
		t.Errorf("an edit Discord refuses is retried: %v", all)
	}
}

func TestAlertRecheckWithTheAppDownStaysOpen(t *testing.T) {
	e := postedAlert(t)
	e.health.err = errors.New("connection refused")
	e.clock.Advance(15 * time.Minute)
	e.flush()
	e.flush()
	if len(e.out.edits) != 0 {
		t.Fatalf("resolved because the app couldn't be asked: %+v", e.out.edits)
	}
	if a := e.stored(t)[alertKey]; a == nil || !a.Resolved.IsZero() || !a.Checked.Equal(e.clock.Now()) {
		t.Errorf("stored %+v; want open, checked now", a)
	}
}

func TestAlertPostUsesTheAppsCurrentMessage(t *testing.T) {
	e := newAlertEnv(t)
	e.health.set("IndexerStatusCheck")
	e.health.message = "Indexers unavailable due to failures: Example, Another"
	e.hook(t, "prowlarr", indexersDown)
	e.clock.Advance(30 * time.Minute)
	e.flush()
	if e.out.count() != 1 || !strings.Contains(v2Content(e.out.posts[0].layouts[0]), "Example, Another") {
		t.Fatalf("posted %s", v2Content(e.out.posts[0].layouts[0]))
	}
	e.hook(t, "prowlarr", indexersBack)
	e.flush()
	if len(e.out.edits) != 1 || !strings.Contains(v2Content(e.out.edits[0].layout), "Example, Another") {
		t.Errorf("the resolved card went back to the webhook's message: %+v", e.out.edits)
	}
}

func TestAlertPostsAreCountedAndRecorded(t *testing.T) {
	e := postedAlert(t)
	e.hook(t, "prowlarr", indexersBack)
	e.flush()
	if !strings.Contains(metricsBody(t, e.srv), `heraldarr_posts_total{source="prowlarr",destination="server",layout="v2"} 1`) {
		t.Error("alert post not in heraldarr_posts_total")
	}
	if len(e.store.History) != 1 || e.store.History[0].Source != "prowlarr" || e.store.History[0].Destination != "server" ||
		e.store.History[0].Headline != "warning" || e.store.History[0].MessageID != "1" {
		t.Errorf("history %+v", e.store.History) // the resolve edit adds nothing
	}
	u := newAlertEnv(t)
	u.hook(t, "sonarr", `{"eventType":"ApplicationUpdate","instanceName":"Sonarr","message":"m","newVersion":"4.0.2"}`)
	u.flush()
	if len(u.store.History) != 1 || u.store.History[0].Headline != "update" {
		t.Errorf("update history %+v", u.store.History)
	}
}

func metricsBody(t *testing.T, srv http.Handler) string {
	t.Helper()
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/metrics", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	return rec.Body.String()
}

func TestAlertResolvedPostIsCountedAndRecorded(t *testing.T) {
	e := postedAlert(t, func(a *app.Alerts) { a.Restored = "post" })
	e.hook(t, "prowlarr", indexersBack)
	e.flush()
	if !strings.Contains(metricsBody(t, e.srv), `heraldarr_posts_total{source="prowlarr",destination="server",layout="v2"} 2`) {
		t.Error("resolved card not in heraldarr_posts_total")
	}
	if len(e.store.History) != 2 || e.store.History[1].Headline != "resolved" {
		t.Errorf("history %+v", e.store.History)
	}
}

func TestAlertStoreErrors(t *testing.T) {
	p := newPinger(t, http.StatusOK)
	st := &brokenAlerts{MemStore: testkit.NewMemStore()}
	e := buildAlertEnv(t, func(d *app.Deps) { d.Store, d.Heartbeat = st, p.URL+pingPath })
	st.fail(errors.New("disk I/O error"))
	if code := e.hook(t, "prowlarr", indexersDown); code != http.StatusInternalServerError {
		t.Errorf("HTTP %d for an alert that couldn't be recorded, want 500", code)
	}
	if !strings.Contains(metricsBody(t, e.srv), `heraldarr_alerts_total{source="prowlarr",result="error"} 1`) {
		t.Error("store error not counted")
	}
	e.flush() // no batch is due, so only the alerts touch the store
	if p.count() != 0 {
		t.Fatal("pinged after a flush whose alerts couldn't be loaded")
	}
	st.fail(nil)
	e.flush()
	if p.count() != 1 {
		t.Errorf("%d pings after the store recovered, want 1", p.count())
	}
}

func TestAlertOutcomeStoreErrorWithholdsTheHeartbeat(t *testing.T) {
	p := newPinger(t, http.StatusOK)
	st := &brokenAlerts{MemStore: testkit.NewMemStore()}
	e := buildAlertEnv(t, func(d *app.Deps) { d.Store, d.Heartbeat = st, p.URL+pingPath })
	e.health.set("IndexerStatusCheck")
	e.hook(t, "prowlarr", indexersDown) // loaded and cached while the store works
	e.clock.Advance(30 * time.Minute)
	st.fail(errors.New("disk I/O error"))
	e.flush() // posts, then can't record it
	if e.out.count() != 1 {
		t.Fatalf("%d posts", e.out.count())
	}
	if p.count() != 0 {
		t.Error("pinged after a flush that couldn't record an alert's outcome")
	}
}
