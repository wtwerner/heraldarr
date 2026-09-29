// Package discord delivers cards to Discord webhooks (domain.Notifier). Each layout is tried in
// order until Discord accepts one; rate limits are honored per webhook.
package discord

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/wtwerner/heraldarr/internal/domain"
)

const (
	// maxAttempts is how many times one layout is sent while Discord answers 429.
	maxAttempts = 3
	// defaultRetryAfter is the wait after a 429 that says nothing about how long.
	defaultRetryAfter = 2 * time.Second
	// retryMargin is added to every wait Discord asks for.
	retryMargin = 500 * time.Millisecond
	// maxRetryAfter: a longer 429 wait is returned as an error for the batcher to retry later.
	maxRetryAfter = 30 * time.Second
	// detailMax is how much of Discord's error body goes into logs and errors.
	detailMax = 500
	// timeout bounds one request when New gets no client.
	timeout = 20 * time.Second
)

// Refusal is Discord's answer to one layout it would not take (a 4xx other than 429).
type Refusal struct {
	Layout string
	Status int
	Detail string // Discord's error body, truncated
}

// RefusedError: Discord refused every layout. Resending the same card won't help.
type RefusedError struct {
	Destination string
	Refusals    []Refusal
}

func (e *RefusedError) Error() string {
	parts := make([]string, len(e.Refusals))
	for i, r := range e.Refusals {
		parts[i] = fmt.Sprintf("%s: %d %s", r.Layout, r.Status, r.Detail)
	}
	return fmt.Sprintf("discord %q refused every layout (%s)", e.Destination, strings.Join(parts, "; "))
}

// Notifier posts to Discord webhooks. It is safe for concurrent use; posts to one webhook are
// serialized so its rate-limit bucket stays accurate.
type Notifier struct {
	client    *http.Client
	clock     domain.Clock
	userAgent string
	sleep     func(context.Context, time.Duration) error
	log       *slog.Logger

	mu      sync.Mutex
	buckets map[string]*bucket // by webhook URL without its query
}

var _ domain.Notifier = (*Notifier)(nil)

// New returns a Notifier. A nil client gets a 20 s timeout.
func New(client *http.Client, clock domain.Clock, userAgent string) *Notifier {
	if client == nil {
		client = &http.Client{Timeout: timeout}
	}
	return &Notifier{
		client: client, clock: clock, userAgent: userAgent, sleep: sleep, log: slog.Default(),
		buckets: map[string]*bucket{},
	}
}

// bucket is one webhook's rate limit, from the X-RateLimit headers of its last response.
type bucket struct {
	turn  chan struct{} // held for the whole of a Post
	until time.Time     // no requests left before this; zero: not limited
}

// Post sends the first layout Discord accepts. A *RefusedError means every layout was refused;
// any other error (5xx, network, rate limited too long) is transient.
func (n *Notifier) Post(ctx context.Context, dest domain.Destination, layouts []domain.Layout) (domain.PostResult, error) {
	if len(layouts) == 0 {
		return domain.PostResult{}, fmt.Errorf("discord %q: no layouts to post", dest.Name)
	}
	u, err := url.Parse(dest.WebhookURL)
	if err != nil || u.Host == "" {
		// url.Parse errors quote the whole URL.
		return domain.PostResult{}, fmt.Errorf("discord %q: invalid webhook URL", dest.Name)
	}
	b := n.bucket(u)
	select {
	case b.turn <- struct{}{}:
		defer func() { <-b.turn }()
	case <-ctx.Done():
		return domain.PostResult{}, ctx.Err()
	}

	if u.RawQuery != "" {
		u.RawQuery += "&"
	}
	u.RawQuery += "wait=true&with_components=true"
	target := u.String()

	refused := &RefusedError{Destination: dest.Name}
	for _, l := range layouts {
		body, err := json.Marshal(withCommon(dest, l.Body))
		if err != nil {
			return domain.PostResult{}, fmt.Errorf("discord %q: encode %s layout: %w", dest.Name, l.Name, err)
		}
		id, r, err := n.send(ctx, b, dest.Name, l.Name, target, body)
		if err != nil {
			return domain.PostResult{}, err
		}
		if r != nil {
			n.log.Warn("discord refused a layout", "destination", dest.Name, "layout", l.Name,
				"status", r.Status, "detail", r.Detail)
			refused.Refusals = append(refused.Refusals, *r)
			continue
		}
		return domain.PostResult{Layout: l.Name, MessageID: id}, nil
	}
	return domain.PostResult{}, refused
}

// send posts one layout, waiting out 429s. It returns the message ID, or the refusal, or a
// transient error.
func (n *Notifier) send(ctx context.Context, b *bucket, destName, layout, target string, body []byte) (string, *Refusal, error) {
	for attempt := 1; ; attempt++ {
		if d := b.until.Sub(n.clock.Now()); d > 0 {
			n.log.Debug("discord rate limit, waiting", "destination", destName, "wait", d)
			if err := n.sleep(ctx, d); err != nil {
				return "", nil, err
			}
		}
		status, header, resp, err := n.do(ctx, target, body)
		if err != nil {
			return "", nil, fmt.Errorf("discord %q: post %s layout: %w", destName, layout, err)
		}
		n.limit(b, header)
		switch {
		case status >= 200 && status < 300:
			var msg struct {
				ID string `json:"id"`
			}
			_ = json.Unmarshal(resp, &msg) // the post went out; only a later edit needs the ID
			return msg.ID, nil, nil
		case status == http.StatusTooManyRequests:
			wait := retryAfter(resp, header)
			if attempt >= maxAttempts || wait > maxRetryAfter {
				return "", nil, fmt.Errorf("discord %q: post %s layout: rate limited (retry after %s, attempt %d)",
					destName, layout, wait, attempt)
			}
			n.log.Info("discord rate limited, retrying", "destination", destName, "layout", layout,
				"wait", wait+retryMargin, "attempt", attempt)
			if err := n.sleep(ctx, wait+retryMargin); err != nil {
				return "", nil, err
			}
		case status >= 400 && status < 500:
			return "", &Refusal{Layout: layout, Status: status, Detail: detail(resp)}, nil
		default:
			return "", nil, fmt.Errorf("discord %q: post %s layout: status %d: %s", destName, layout, status, detail(resp))
		}
	}
}

// do sends one request. Errors never contain the webhook URL.
func (n *Notifier) do(ctx context.Context, target string, body []byte) (int, http.Header, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		return 0, nil, nil, redactErr(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if n.userAgent != "" {
		req.Header.Set("User-Agent", n.userAgent)
	}
	resp, err := n.client.Do(req)
	if err != nil {
		return 0, nil, nil, redactErr(err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return 0, nil, nil, fmt.Errorf("read response: %w", redactErr(err))
	}
	return resp.StatusCode, resp.Header, raw, nil
}

func (n *Notifier) bucket(u *url.URL) *bucket {
	key := (&url.URL{Scheme: u.Scheme, Host: u.Host, Path: u.Path}).String()
	n.mu.Lock()
	defer n.mu.Unlock()
	b := n.buckets[key]
	if b == nil {
		b = &bucket{turn: make(chan struct{}, 1)}
		n.buckets[key] = b
	}
	return b
}

// limit updates the bucket from a response's X-RateLimit headers. Only the holder of b.turn calls it.
func (n *Notifier) limit(b *bucket, h http.Header) {
	remaining, err := strconv.Atoi(h.Get("X-RateLimit-Remaining"))
	if err != nil {
		return
	}
	b.until = time.Time{}
	if remaining > 0 {
		return
	}
	if after, ok := seconds(h.Get("X-RateLimit-Reset-After")); ok {
		b.until = n.clock.Now().Add(after)
	}
}

// retryAfter reads a 429's wait: retry_after from the JSON body, else the Retry-After header.
func retryAfter(body []byte, h http.Header) time.Duration {
	var rl struct {
		RetryAfter *float64 `json:"retry_after"`
	}
	if json.Unmarshal(body, &rl) == nil && rl.RetryAfter != nil {
		if d, ok := toDuration(*rl.RetryAfter); ok {
			return d
		}
	}
	if d, ok := seconds(h.Get("Retry-After")); ok {
		return d
	}
	return defaultRetryAfter
}

func seconds(s string) (time.Duration, bool) {
	f, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil {
		return 0, false
	}
	return toDuration(f)
}

func toDuration(sec float64) (time.Duration, bool) {
	if math.IsNaN(sec) || sec < 0 || sec > 24*3600 {
		return 0, false
	}
	return time.Duration(sec * float64(time.Second)), true
}

// withCommon adds the destination's identity and disables pings; the layout's own keys win.
func withCommon(dest domain.Destination, body map[string]any) map[string]any {
	out := map[string]any{
		"username": dest.Username, "avatar_url": dest.AvatarURL,
		"allowed_mentions": map[string]any{"parse": []any{}},
	}
	for k, v := range body {
		out[k] = v
	}
	return out
}

func detail(body []byte) string {
	s := strings.TrimSpace(strings.ToValidUTF8(string(body), "�"))
	if r := []rune(s); len(r) > detailMax {
		s = string(r[:detailMax])
	}
	return s
}

// redactErr replaces the URL in an *url.Error (net/http's errors quote it in full).
func redactErr(err error) error {
	if ue, ok := errors.AsType[*url.Error](err); ok {
		ue.URL = redact(ue.URL)
	}
	return err
}

// redact hides a webhook URL's token: the path segment after the webhook ID
// (…/webhooks/<id>/REDACTED), or the last segment of any other URL. Query and user info go too.
func redact(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "(webhook URL)"
	}
	segs := strings.Split(strings.Trim(u.Path, "/"), "/")
	cut := len(segs) - 1
	for i, s := range segs {
		if s == "webhooks" && i+2 < len(segs) {
			cut = i + 2
			break
		}
	}
	if cut >= 0 {
		segs = append(segs[:cut], "REDACTED")
	}
	return (&url.URL{Scheme: u.Scheme, Host: u.Host, Path: "/" + strings.Join(segs, "/")}).String()
}

// sleep waits d or until ctx is done.
func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
