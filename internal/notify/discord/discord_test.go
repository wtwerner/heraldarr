package discord

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wtwerner/heraldarr/internal/domain"
	"github.com/wtwerner/heraldarr/internal/testkit"
)

const token = "fictional-webhook-token-0123456789"

// reply is one scripted response.
type reply struct {
	status  int
	body    string
	headers map[string]string
}

type request struct {
	path, rawQuery string
	header         http.Header
	body           map[string]any
}

// discordStub answers each request with the next scripted reply (the last one repeats) and
// records what it received.
type discordStub struct {
	*httptest.Server
	mu      sync.Mutex
	replies []reply
	got     []request
}

func newStub(t *testing.T, replies ...reply) *discordStub {
	t.Helper()
	s := &discordStub{replies: replies}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Errorf("request body is not JSON: %v", err)
		}
		s.mu.Lock()
		s.got = append(s.got, request{r.URL.Path, r.URL.RawQuery, r.Header.Clone(), body})
		rep := s.replies[min(len(s.got), len(s.replies))-1]
		s.mu.Unlock()
		for k, v := range rep.headers {
			w.Header().Set(k, v)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(rep.status)
		_, _ = io.WriteString(w, rep.body)
	}))
	t.Cleanup(s.Close)
	return s
}

func (s *discordStub) requests() []request {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]request(nil), s.got...)
}

func (s *discordStub) webhook(id string) string {
	return s.URL + "/api/webhooks/" + id + "/" + token
}

// harness is a Notifier with a fake clock, a sleep that advances it, and captured logs.
type harness struct {
	*Notifier
	clock  *testkit.Clock
	slept  []time.Duration
	logs   bytes.Buffer
	sleepF func(context.Context, time.Duration) error // overrides the recording sleep
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	h := &harness{clock: testkit.NewClock(time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC))}
	h.Notifier = New(nil, h.clock, "heraldarr-test")
	h.log = slog.New(slog.NewTextHandler(&h.logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	h.sleep = func(ctx context.Context, d time.Duration) error {
		if h.sleepF != nil {
			return h.sleepF(ctx, d)
		}
		h.slept = append(h.slept, d)
		h.clock.Advance(d)
		return nil
	}
	return h
}

func dest(webhook string) domain.Destination {
	return domain.Destination{
		Name: "tv", WebhookURL: webhook, Username: "Heraldarr",
		AvatarURL: "https://example.org/avatar.png", Public: true,
	}
}

var layouts = []domain.Layout{
	{Name: "v2", Body: map[string]any{"flags": 32768, "components": []any{}}},
	{Name: "embed", Body: map[string]any{"embeds": []any{map[string]any{"title": "Example Show"}}}},
	{Name: "embed, inline links", Body: map[string]any{"embeds": []any{map[string]any{"title": "Example Show (links)"}}}},
}

func ok(id string) reply { return reply{status: 200, body: `{"id":"` + id + `","channel_id":"42"}`} }

func TestPostsFirstLayoutWithCommonFields(t *testing.T) {
	s := newStub(t, ok("1001"))
	h := newHarness(t)

	res, err := h.Post(context.Background(), dest(s.webhook("1")), layouts)
	if err != nil {
		t.Fatal(err)
	}
	if want := (domain.PostResult{Layout: "v2", MessageID: "1001"}); res != want {
		t.Errorf("result = %+v, want %+v", res, want)
	}
	got := s.requests()
	if len(got) != 1 {
		t.Fatalf("%d requests, want 1", len(got))
	}
	r := got[0]
	if r.path != "/api/webhooks/1/"+token {
		t.Errorf("path = %q", r.path)
	}
	if r.rawQuery != "wait=true&with_components=true" {
		t.Errorf("query = %q, want wait=true&with_components=true", r.rawQuery)
	}
	if ct := r.header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q", ct)
	}
	if ua := r.header.Get("User-Agent"); ua != "heraldarr-test" {
		t.Errorf("User-Agent = %q", ua)
	}
	want := map[string]any{
		"username": "Heraldarr", "avatar_url": "https://example.org/avatar.png",
		"allowed_mentions": map[string]any{"parse": []any{}},
		"flags":            float64(32768), "components": []any{},
	}
	assertJSON(t, r.body, want)
	if len(h.slept) != 0 {
		t.Errorf("slept %v on a clean post", h.slept)
	}
}

func TestAppendsToExistingQuery(t *testing.T) {
	s := newStub(t, ok("1"))
	h := newHarness(t)
	if _, err := h.Post(context.Background(), dest(s.webhook("1")+"?thread_id=77"), layouts[:1]); err != nil {
		t.Fatal(err)
	}
	if q := s.requests()[0].rawQuery; q != "thread_id=77&wait=true&with_components=true" {
		t.Errorf("query = %q", q)
	}
}

func TestLayoutBodyWinsOverCommonFields(t *testing.T) {
	s := newStub(t, ok("1"))
	h := newHarness(t)
	l := []domain.Layout{{Name: "v2", Body: map[string]any{"username": "Override"}}}
	if _, err := h.Post(context.Background(), dest(s.webhook("1")), l); err != nil {
		t.Fatal(err)
	}
	if u := s.requests()[0].body["username"]; u != "Override" {
		t.Errorf("username = %v, want the layout's", u)
	}
}

func TestRefusedLayoutFallsBackToNext(t *testing.T) {
	s := newStub(t,
		reply{status: 400, body: `{"message":"Invalid Form Body","code":50035}`},
		ok("2002"),
	)
	h := newHarness(t)

	res, err := h.Post(context.Background(), dest(s.webhook("1")), layouts)
	if err != nil {
		t.Fatal(err)
	}
	if want := (domain.PostResult{Layout: "embed", MessageID: "2002"}); res != want {
		t.Errorf("result = %+v, want %+v", res, want)
	}
	got := s.requests()
	if len(got) != 2 {
		t.Fatalf("%d requests, want 2", len(got))
	}
	if _, ok := got[1].body["embeds"]; !ok {
		t.Errorf("second request is not the embed layout: %v", got[1].body)
	}
	logs := h.logs.String()
	for _, want := range []string{"v2", "400", "Invalid Form Body"} {
		if !strings.Contains(logs, want) {
			t.Errorf("log lacks %q:\n%s", want, logs)
		}
	}
}

func TestEveryLayoutRefused(t *testing.T) {
	s := newStub(t,
		reply{status: 400, body: `{"message":"bad v2"}`},
		reply{status: 403, body: `{"message":"bad embed"}`},
		reply{status: 404, body: `{"message":"Unknown Webhook"}`},
	)
	h := newHarness(t)

	_, err := h.Post(context.Background(), dest(s.webhook("1")), layouts)
	var refused *RefusedError
	if !errors.As(err, &refused) {
		t.Fatalf("err = %v, want a *RefusedError", err)
	}
	if refused.Destination != "tv" {
		t.Errorf("Destination = %q", refused.Destination)
	}
	var statuses []int
	var names []string
	for _, r := range refused.Refusals {
		statuses = append(statuses, r.Status)
		names = append(names, r.Layout)
	}
	if want := []int{400, 403, 404}; !slices.Equal(statuses, want) {
		t.Errorf("statuses = %v, want %v", statuses, want)
	}
	if want := []string{"v2", "embed", "embed, inline links"}; !slices.Equal(names, want) {
		t.Errorf("layouts = %v, want %v", names, want)
	}
	if !strings.Contains(err.Error(), "Unknown Webhook") {
		t.Errorf("error lacks Discord's reason: %v", err)
	}
	if n := len(s.requests()); n != 3 {
		t.Errorf("%d requests, want 3", n)
	}
}

func TestNoLayouts(t *testing.T) {
	h := newHarness(t)
	if _, err := h.Post(context.Background(), dest("https://example.org/api/webhooks/1/"+token), nil); err == nil {
		t.Error("no layouts: want an error")
	}
}

func TestRateLimitedWaitsRetryAfterFromBody(t *testing.T) {
	s := newStub(t,
		reply{status: 429, body: `{"message":"You are being rate limited.","retry_after":1.25,"global":false}`},
		ok("3003"),
	)
	h := newHarness(t)

	res, err := h.Post(context.Background(), dest(s.webhook("1")), layouts)
	if err != nil {
		t.Fatal(err)
	}
	if res.Layout != "v2" || res.MessageID != "3003" {
		t.Errorf("result = %+v, want v2 retried", res)
	}
	if want := []time.Duration{1250*time.Millisecond + retryMargin}; !slices.Equal(h.slept, want) {
		t.Errorf("slept %v, want %v", h.slept, want)
	}
	got := s.requests()
	if len(got) != 2 {
		t.Fatalf("%d requests, want 2", len(got))
	}
	if _, ok := got[1].body["flags"]; !ok {
		t.Errorf("retry is not the v2 layout: %v", got[1].body)
	}
}

func TestRateLimitedWaitsRetryAfterHeader(t *testing.T) {
	s := newStub(t,
		reply{status: 429, body: `rate limited`, headers: map[string]string{"Retry-After": "3"}},
		ok("1"),
	)
	h := newHarness(t)
	if _, err := h.Post(context.Background(), dest(s.webhook("1")), layouts); err != nil {
		t.Fatal(err)
	}
	if want := []time.Duration{3*time.Second + retryMargin}; !slices.Equal(h.slept, want) {
		t.Errorf("slept %v, want %v", h.slept, want)
	}
}

func TestRateLimitedWithoutHintWaitsDefault(t *testing.T) {
	s := newStub(t, reply{status: 429, body: `{}`}, ok("1"))
	h := newHarness(t)
	if _, err := h.Post(context.Background(), dest(s.webhook("1")), layouts); err != nil {
		t.Fatal(err)
	}
	if want := []time.Duration{defaultRetryAfter + retryMargin}; !slices.Equal(h.slept, want) {
		t.Errorf("slept %v, want %v", h.slept, want)
	}
}

func TestRateLimitedThreeTimesIsTransient(t *testing.T) {
	s := newStub(t, reply{status: 429, body: `{"retry_after":0.5}`})
	h := newHarness(t)

	_, err := h.Post(context.Background(), dest(s.webhook("1")), layouts)
	if err == nil {
		t.Fatal("want an error")
	}
	var refused *RefusedError
	if errors.As(err, &refused) {
		t.Errorf("rate limiting is transient, not a refusal: %v", err)
	}
	if n := len(s.requests()); n != maxAttempts {
		t.Errorf("%d requests, want %d (one layout, no fallback)", n, maxAttempts)
	}
	if len(h.slept) != maxAttempts-1 {
		t.Errorf("slept %v, want %d waits", h.slept, maxAttempts-1)
	}
}

func TestRateLimitedTooLongIsTransient(t *testing.T) {
	s := newStub(t, reply{status: 429, body: `{"retry_after":3600,"global":true}`})
	h := newHarness(t)
	if _, err := h.Post(context.Background(), dest(s.webhook("1")), layouts); err == nil {
		t.Fatal("want an error")
	}
	if len(h.slept) != 0 {
		t.Errorf("slept %v; an hour-long wait should go back to the batcher", h.slept)
	}
	if n := len(s.requests()); n != 1 {
		t.Errorf("%d requests, want 1", n)
	}
}

func TestServerErrorIsTransient(t *testing.T) {
	s := newStub(t, reply{status: 502, body: `bad gateway`})
	h := newHarness(t)

	_, err := h.Post(context.Background(), dest(s.webhook("1")), layouts)
	if err == nil {
		t.Fatal("want an error")
	}
	var refused *RefusedError
	if errors.As(err, &refused) {
		t.Errorf("5xx is transient, not a refusal: %v", err)
	}
	if !strings.Contains(err.Error(), "502") {
		t.Errorf("error lacks the status: %v", err)
	}
	if n := len(s.requests()); n != 1 {
		t.Errorf("%d requests, want 1 (the batcher retries later)", n)
	}
}

func TestNetworkErrorIsTransient(t *testing.T) {
	s := newStub(t, ok("1"))
	webhook := s.webhook("1")
	s.Close()
	h := newHarness(t)
	if _, err := h.Post(context.Background(), dest(webhook), layouts); err == nil {
		t.Fatal("want an error")
	}
}

func TestContextCancelledDuringWait(t *testing.T) {
	s := newStub(t, reply{status: 429, body: `{"retry_after":1}`})
	h := newHarness(t)
	h.sleepF = func(context.Context, time.Duration) error { return context.Canceled }
	_, err := h.Post(context.Background(), dest(s.webhook("1")), layouts)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
}

func TestRealSleepHonorsContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := sleep(ctx, time.Hour); !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
	if err := sleep(context.Background(), time.Millisecond); err != nil {
		t.Error(err)
	}
}

func TestBucketWaitsWhenExhausted(t *testing.T) {
	exhausted := map[string]string{"X-RateLimit-Remaining": "0", "X-RateLimit-Reset-After": "1.5"}
	s := newStub(t, reply{status: 200, body: `{"id":"1"}`, headers: exhausted}, ok("2"))
	h := newHarness(t)
	ctx := context.Background()

	if _, err := h.Post(ctx, dest(s.webhook("1")), layouts); err != nil {
		t.Fatal(err)
	}
	if len(h.slept) != 0 {
		t.Fatalf("slept %v before the first post", h.slept)
	}
	// Another webhook has its own bucket.
	if _, err := h.Post(ctx, dest(s.webhook("2")), layouts); err != nil {
		t.Fatal(err)
	}
	if len(h.slept) != 0 {
		t.Fatalf("slept %v for a different webhook", h.slept)
	}
	// Same webhook, even with a different query: waits for the reset.
	h.clock.Advance(500 * time.Millisecond)
	if _, err := h.Post(ctx, dest(s.webhook("1")+"?thread_id=5"), layouts); err != nil {
		t.Fatal(err)
	}
	if want := []time.Duration{time.Second}; !slices.Equal(h.slept, want) {
		t.Errorf("slept %v, want %v", h.slept, want)
	}
}

func TestBucketDoesNotWaitWithRemaining(t *testing.T) {
	headers := map[string]string{"X-RateLimit-Remaining": "4", "X-RateLimit-Reset-After": "2"}
	s := newStub(t, reply{status: 200, body: `{"id":"1"}`, headers: headers})
	h := newHarness(t)
	for range 2 {
		if _, err := h.Post(context.Background(), dest(s.webhook("1")), layouts); err != nil {
			t.Fatal(err)
		}
	}
	if len(h.slept) != 0 {
		t.Errorf("slept %v with requests remaining", h.slept)
	}
}

func TestBucketDoesNotWaitAfterReset(t *testing.T) {
	exhausted := map[string]string{"X-RateLimit-Remaining": "0", "X-RateLimit-Reset-After": "2"}
	s := newStub(t, reply{status: 200, body: `{"id":"1"}`, headers: exhausted})
	h := newHarness(t)
	ctx := context.Background()
	if _, err := h.Post(ctx, dest(s.webhook("1")), layouts); err != nil {
		t.Fatal(err)
	}
	h.clock.Advance(2 * time.Second)
	if _, err := h.Post(ctx, dest(s.webhook("1")), layouts); err != nil {
		t.Fatal(err)
	}
	if len(h.slept) != 0 {
		t.Errorf("slept %v after the bucket reset", h.slept)
	}
}

func TestConcurrentPostsToOneWebhookShareTheBucket(t *testing.T) {
	exhausted := map[string]string{"X-RateLimit-Remaining": "0", "X-RateLimit-Reset-After": "1"}
	s := newStub(t, reply{status: 200, body: `{"id":"1"}`, headers: exhausted})
	h := newHarness(t)
	var mu sync.Mutex
	h.sleepF = func(_ context.Context, d time.Duration) error {
		mu.Lock()
		defer mu.Unlock()
		h.slept = append(h.slept, d)
		h.clock.Advance(d)
		return nil
	}
	var wg sync.WaitGroup
	for range 3 {
		wg.Go(func() {
			if _, err := h.Post(context.Background(), dest(s.webhook("1")), layouts); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	// Serialized: every post after the first waits for the reset the previous one reported.
	if want := []time.Duration{time.Second, time.Second}; !slices.Equal(h.slept, want) {
		t.Errorf("slept %v, want %v", h.slept, want)
	}
}

// The webhook token is a credential: it must not reach logs or errors on any path.
func TestWebhookTokenNeverLeaks(t *testing.T) {
	cases := map[string][]reply{
		"refused":      {{status: 400, body: `{"message":"bad"}`}},
		"server error": {{status: 500, body: `oops`}},
		"rate limited": {{status: 429, body: `{"retry_after":0.1}`}},
		"too long":     {{status: 429, body: `{"retry_after":999}`}},
		"success":      {ok("1")},
	}
	for name, replies := range cases {
		t.Run(name, func(t *testing.T) {
			s := newStub(t, replies...)
			h := newHarness(t)
			_, err := h.Post(context.Background(), dest(s.webhook("1")), layouts)
			assertNoToken(t, err, h.logs.String())
		})
	}
	t.Run("network error", func(t *testing.T) {
		s := newStub(t, ok("1"))
		webhook := s.webhook("1")
		s.Close()
		h := newHarness(t)
		_, err := h.Post(context.Background(), dest(webhook), layouts)
		if err == nil {
			t.Fatal("want an error")
		}
		assertNoToken(t, err, h.logs.String())
	})
	t.Run("invalid URL", func(t *testing.T) {
		h := newHarness(t)
		_, err := h.Post(context.Background(), dest("https://exa mple.org/api/webhooks/1/"+token), layouts)
		if err == nil {
			t.Fatal("want an error")
		}
		assertNoToken(t, err, h.logs.String())
	})
}

func TestRedact(t *testing.T) {
	for in, want := range map[string]string{
		"https://discord.com/api/webhooks/123/" + token:                    "https://discord.com/api/webhooks/123/REDACTED",
		"https://discord.com/api/webhooks/123/" + token + "?thread_id=9":   "https://discord.com/api/webhooks/123/REDACTED",
		"https://discord.com/api/webhooks/123/" + token + "/github":        "https://discord.com/api/webhooks/123/REDACTED",
		"https://example.org/hooks/" + token:                               "https://example.org/hooks/REDACTED",
		"https://user:" + token + "@example.org/api/webhooks/1/" + token:   "https://example.org/api/webhooks/1/REDACTED",
		"https://discord.com/api/v10/webhooks/123/" + token + "?wait=true": "https://discord.com/api/v10/webhooks/123/REDACTED",
		"::not a url " + token: "(webhook URL)",
	} {
		if got := redact(in); got != want {
			t.Errorf("redact(%q) = %q, want %q", in, got, want)
		}
	}
}

func assertNoToken(t *testing.T, err error, logs string) {
	t.Helper()
	if err != nil && strings.Contains(err.Error(), token) {
		t.Errorf("error leaks the webhook token: %v", err)
	}
	if strings.Contains(logs, token) {
		t.Errorf("logs leak the webhook token:\n%s", logs)
	}
}

func assertJSON(t *testing.T, got, want map[string]any) {
	t.Helper()
	g, _ := json.Marshal(got)
	w, _ := json.Marshal(want)
	if !bytes.Equal(g, w) {
		t.Errorf("body = %s\nwant   %s", g, w)
	}
}
