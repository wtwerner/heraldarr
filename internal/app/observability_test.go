package app_test

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"testing/iotest"
	"time"

	"github.com/wtwerner/heraldarr/internal/app"
	"github.com/wtwerner/heraldarr/internal/domain"
	"github.com/wtwerner/heraldarr/internal/source/arr"
	"github.com/wtwerner/heraldarr/internal/testkit"
)

// scrape returns GET /metrics as {`name{labels}`: value}.
func scrape(t *testing.T, e *env) map[string]float64 {
	t.Helper()
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/metrics", http.NoBody)
	req.SetBasicAuth("u", "p")
	rec := httptest.NewRecorder()
	e.srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/plain; version=0.0.4") {
		t.Fatalf("GET /metrics: HTTP %d, %s", rec.Code, rec.Header().Get("Content-Type"))
	}
	out := map[string]float64{}
	sc := bufio.NewScanner(rec.Body)
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "#") {
			continue
		}
		i := strings.LastIndexByte(line, ' ')
		v, err := strconv.ParseFloat(line[i+1:], 64)
		if i < 0 || err != nil {
			t.Fatalf("bad sample line %q", line)
		}
		out[line[:i]] = v
	}
	return out
}

func (e *env) hook(t *testing.T, source, body string) {
	t.Helper()
	e.hookBody(t, source, strings.NewReader(body))
}

func (e *env) hookBody(t *testing.T, source string, body io.Reader) {
	t.Helper()
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/hook/"+source, body)
	req.SetBasicAuth("u", "p")
	e.srv.ServeHTTP(httptest.NewRecorder(), req)
}

func expectMetrics(t *testing.T, step string, got map[string]float64, want map[string]float64) {
	t.Helper()
	for k, v := range want {
		g, ok := got[k]
		if !ok {
			t.Errorf("%s: %s missing", step, k)
		} else if g != v {
			t.Errorf("%s: %s = %v, want %v", step, k, g, v)
		}
	}
}

const (
	queued    = `heraldarr_imports_total{source="radarr",result="queued"}`
	upgrade   = `heraldarr_imports_total{source="radarr",result="upgrade"}`
	announced = `heraldarr_imports_total{source="radarr",result="already_announced"}`
	ignored   = `heraldarr_imports_total{source="radarr",result="ignored"}`
	invalid   = `heraldarr_imports_total{source="radarr",result="invalid"}`
	queueErr  = `heraldarr_imports_total{source="radarr",result="error"}`
	posts     = `heraldarr_posts_total{source="radarr",destination="radarr-dest",layout="v2"}`
	failures  = `heraldarr_delivery_failures_total{source="radarr"}`
	pending   = `heraldarr_pending_batches`
	waits     = `heraldarr_media_server_waits_total`
	lastPost  = `heraldarr_last_post_timestamp_seconds`
)

// Every counter moves on its own event, and only on it: each step checks every series.
func TestMetricsCountEachEvent(t *testing.T) {
	e := newEnv(t, "movie_three", true) // one of the three movies isn't in the library
	ctx := context.Background()
	want := map[string]float64{
		queued: 0, upgrade: 0, announced: 0, ignored: 0, invalid: 0, queueErr: 0,
		failures: 0, pending: 0, waits: 0, lastPost: 0,
	}
	step := func(name string, changes map[string]float64) {
		t.Helper()
		for k, v := range changes {
			want[k] = v
		}
		got := scrape(t, e)
		expectMetrics(t, name, got, want)
		for k, v := range got {
			if _, ok := want[k]; !ok && v != 0 {
				t.Errorf("%s: %s = %v, want 0", name, k, v) // another source, or a series that shouldn't move
			}
		}
	}
	step("at start", nil)

	e.send(t)
	step("3 imports", map[string]float64{queued: 3, pending: 1})

	up, _ := testkit.Load(t, "movie_upgrade_ignored")
	e.hook(t, "radarr", string(up.Events[0].Payload))
	step("upgrade", map[string]float64{upgrade: 1})
	e.hook(t, "radarr", `{"eventType":"Test"}`)
	step("test event", map[string]float64{ignored: 1})
	e.hook(t, "radarr", `{"eventType":"Download","movie":`)
	step("garbage", map[string]float64{invalid: 1})
	e.hookBody(t, "radarr", io.LimitReader(zeros{}, 5<<20)) // over the 4 MiB limit
	step("oversized body", map[string]float64{invalid: 2})
	e.hookBody(t, "radarr", iotest.ErrReader(errors.New("connection reset")))
	step("unreadable body", map[string]float64{invalid: 3})
	e.hook(t, "nope", `{"eventType":"Test"}`)
	step("unknown source", nil)
	for k := range scrape(t, e) {
		if strings.Contains(k, "nope") {
			t.Errorf("unknown source counted: %s", k)
		}
	}

	e.clock.Advance(timing.QuietMovies)
	e.app.Flush(ctx, false)
	step("waiting for the library", map[string]float64{waits: 1})

	e.out.fail = errors.New("discord down")
	e.app.Flush(ctx, true)
	step("failed delivery", map[string]float64{failures: 1})

	e.out.fail = nil
	e.clock.Advance(time.Minute)
	e.app.Flush(ctx, true)
	step("posted", map[string]float64{posts: 3, pending: 0, lastPost: float64(e.clock.Now().Unix())})

	e.send(t)
	step("replayed", map[string]float64{announced: 3})

	e.store.saveErr = errors.New("disk full")
	single, _ := testkit.Load(t, "movie_single")
	e.hook(t, "radarr", string(single.Events[0].Payload))
	step("store failure", map[string]float64{queueErr: 1})
}

type zeros struct{}

func (zeros) Read(p []byte) (int, error) { clear(p); return len(p), nil }

// A scrape that can't read the store fails, rather than serving a stale pending count.
func TestMetricsStoreUnavailable(t *testing.T) {
	e := newEnv(t, "movie_single", false)
	e.store.loadErr = errors.New("database is locked")
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/metrics", http.NoBody)
	req.SetBasicAuth("u", "p")
	rec := httptest.NewRecorder()
	e.srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("GET /metrics with the store down: HTTP %d, want 503", rec.Code)
	}
}

// A preview posted to a private destination isn't an announcement.
func TestMetricsIgnorePreviews(t *testing.T) {
	e := newEnv(t, "movie_4k_private", true)
	imp, _, err := arr.ParseWebhook("radarr4k", domain.KindMovie, e.sc.Events[0].Payload)
	if err != nil {
		t.Fatal(err)
	}
	private := &domain.Destination{Name: "private"}
	if _, err := e.app.Preview(context.Background(), "radarr4k", []domain.Import{imp}, private); err != nil || e.out.count() != 1 {
		t.Fatalf("preview: %v, %d posts", err, e.out.count())
	}
	for k, v := range scrape(t, e) {
		if strings.HasPrefix(k, "heraldarr_posts_total") || k == lastPost && v != 0 {
			t.Errorf("preview counted as a post: %s %v", k, v)
		}
	}
}

// pinger is a fake heartbeat monitor.
type pinger struct {
	*httptest.Server
	mu     sync.Mutex
	hits   []string
	status int
}

func newPinger(t *testing.T, status int) *pinger {
	t.Helper()
	p := &pinger{status: status}
	p.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p.mu.Lock()
		p.hits = append(p.hits, r.Method+" "+r.URL.Path)
		p.mu.Unlock()
		w.WriteHeader(p.status)
	}))
	t.Cleanup(p.Close)
	return p
}

func (p *pinger) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.hits)
}

const pingPath = "/ping/secret-token"

func withHeartbeat(url string) func(*app.Deps) {
	return func(d *app.Deps) { d.Heartbeat = url }
}

// Each flush pings the monitor, at most once a minute, whether or not the monitor accepts.
func TestHeartbeatRateLimited(t *testing.T) {
	for _, status := range []int{http.StatusOK, http.StatusServiceUnavailable} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			p := newPinger(t, status)
			e := newEnv(t, "movie_single", false, withHeartbeat(p.URL+pingPath))
			ctx := context.Background()
			for _, step := range []struct {
				advance time.Duration
				want    int
			}{
				{0, 1},                                // first flush
				{0, 1},                                // same instant
				{app.HeartbeatEvery - time.Second, 1}, // not yet a minute
				{time.Second, 2},                      // a minute after the first ping
				{30 * time.Second, 2},
				{30 * time.Second, 3},
			} {
				e.clock.Advance(step.advance)
				e.app.Flush(ctx, false)
				if got := p.count(); got != step.want {
					t.Fatalf("at +%s: %d pings, want %d", e.clock.Now().Sub(e.sc.Now), got, step.want)
				}
			}
			if p.hits[0] != "GET "+pingPath {
				t.Errorf("ping was %q, want GET %s", p.hits[0], pingPath)
			}
		})
	}
}

// A flush that hit a store error doesn't ping: the monitor should alert on a service that runs
// but can't keep its state. The next clean flush pings again right away.
func TestHeartbeatSkippedOnStoreError(t *testing.T) {
	t.Run("loading batches", func(t *testing.T) {
		p := newPinger(t, http.StatusOK)
		e := newEnv(t, "movie_single", false, withHeartbeat(p.URL+pingPath))
		e.store.loadErr = errors.New("database is locked")
		e.app.Flush(context.Background(), false)
		if p.count() != 0 {
			t.Fatal("pinged after a flush that couldn't load its batches")
		}
		e.store.loadErr = nil
		e.app.Flush(context.Background(), false)
		if p.count() != 1 {
			t.Fatalf("%d pings after the store recovered, want 1", p.count())
		}
	})
	t.Run("recording one of the outcomes", func(t *testing.T) {
		p := newPinger(t, http.StatusOK)
		e := newEnv(t, "movie_single", false, withHeartbeat(p.URL+pingPath))
		uhd, _ := testkit.Load(t, "movie_4k_private")
		e.sc.Events = append(e.sc.Events, uhd.Events...) // two batches: radarr4k:movies goes first
		e.send(t)
		e.store.delErr, e.store.delKey = errors.New("disk I/O error"), "radarr4k:movies"
		e.app.Flush(context.Background(), true)
		if e.out.count() != 2 {
			t.Fatalf("%d posts, want 2", e.out.count())
		}
		if p.count() != 0 {
			t.Fatal("pinged after a flush that couldn't record what it posted")
		}
		// One failed Done doesn't skip the others: the second batch was recorded as sent.
		if pending, _ := e.app.Store.LoadBatches(context.Background()); pending["radarr:movies"] != nil {
			t.Error("the batch after the failed one was never reported done")
		}
	})
}

// A monitor that is down or refuses is logged at debug, without the URL (it may hold a token),
// and changes nothing else.
func TestHeartbeatFailureIsQuiet(t *testing.T) {
	down := newPinger(t, http.StatusOK)
	down.Close()
	for _, tc := range []struct {
		name, url, msg string
	}{
		{"refused", newPinger(t, http.StatusInternalServerError).URL + pingPath, "heartbeat refused"},
		{"not found", newPinger(t, http.StatusNotFound).URL + pingPath, "heartbeat refused"},
		{"unreachable", down.URL + pingPath, "heartbeat failed"},
		{"bad URL", "http://example.org" + pingPath + "\x7f", "heartbeat failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var logs bytes.Buffer
			e := newEnv(t, "movie_single", false, withHeartbeat(tc.url), func(d *app.Deps) {
				d.Log = slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
			})
			e.send(t)
			e.app.Flush(context.Background(), true)
			if e.out.count() != 1 {
				t.Fatalf("%d posts, want 1", e.out.count())
			}
			var line string
			for _, l := range strings.Split(logs.String(), "\n") {
				if strings.Contains(l, "heartbeat") {
					line = l
				}
			}
			if !strings.Contains(line, "level=DEBUG") || !strings.Contains(line, `msg="`+tc.msg+`"`) {
				t.Errorf("want a debug line %q, got %q", tc.msg, line)
			}
			if strings.Contains(logs.String(), "secret-token") {
				t.Errorf("the heartbeat URL was logged: %s", logs.String())
			}
		})
	}
}
