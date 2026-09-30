package app

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"sync"
	"time"
)

// HeartbeatEvery is the most often the heartbeat URL is called.
const HeartbeatEvery = time.Minute

// heartbeat tells an outside monitor (healthchecks.io style) that the flush loop is alive, so it
// can alert when the pings stop.
type heartbeat struct {
	url    string // may hold a secret token: never logged
	client *http.Client

	mu   sync.Mutex
	last time.Time
}

// beat GETs the URL unless it was called less than HeartbeatEvery ago. Failures are logged at
// debug and otherwise ignored: the monitor noticing the missing pings is the alert.
func (a *App) beat(ctx context.Context) {
	h := a.heartbeat
	if h == nil {
		return
	}
	now := a.Clock.Now()
	h.mu.Lock()
	if !h.last.IsZero() && now.Sub(h.last) < HeartbeatEvery {
		h.mu.Unlock()
		return
	}
	h.last = now // a failed attempt counts too: a broken monitor isn't retried every tick
	h.mu.Unlock()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, h.url, http.NoBody)
	if err != nil {
		a.Log.Debug("heartbeat failed", "err", redact(err))
		return
	}
	resp, err := h.client.Do(req)
	if err != nil {
		a.Log.Debug("heartbeat failed", "err", redact(err))
		return
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10)) // so the connection is reused
	_ = resp.Body.Close()
	if resp.StatusCode >= 300 {
		a.Log.Debug("heartbeat refused", "status", resp.Status)
	}
}

// redact drops the URL an HTTP client error carries.
func redact(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Err
	}
	return err
}
