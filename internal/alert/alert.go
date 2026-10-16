// Package alert decides when health alerts and application updates from the sources are posted.
// A health issue is held: it posts only if the check is still failing once its hold is over, so
// the blips that clear on their own never reach the channel. A posted issue is shown resolved when
// the check passes again. Pure logic over domain.Store and domain.Clock; the app delivers.
package alert

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/wtwerner/heraldarr/internal/domain"
)

// Config holds the alert timings.
type Config struct {
	Hold       time.Duration // a warning or notice must last this long before it posts
	HoldErrors time.Duration // the same for errors (0: at the next flush)
	// Recheck is how often a posted, still-open issue is checked against the app, so a missed
	// Health Restored doesn't leave it open for good (0: never).
	Recheck       time.Duration
	RetryInterval time.Duration // after a failed delivery
	RetryMax      int           // failed deliveries before the alert is dropped
	Updates       bool          // post Application Update events
}

// Result says what Add did, for logs and metrics.
type Result string

const (
	Held     Result = "held"     // a new issue, waiting out its hold
	Repeat   Result = "repeat"   // an issue already known: updated, or reopened within a hold of clearing
	Cleared  Result = "cleared"  // restored before it was posted
	Restored Result = "restored" // restored after it was posted: its card will show it resolved
	Update   Result = "update"   // an application update, posted at the next flush
	Ignored  Result = "ignored"  // nothing to do: a restore of an unknown issue, a known update, updates off
)

// Action is what Due hands out an alert for.
type Action int

const (
	Post    Action = iota + 1 // post its card: the hold is over (or it is an update)
	Resolve                   // a posted issue passed: show it resolved
	Recheck                   // a posted issue is still open: ask the app whether it still fails
)

func (a Action) String() string {
	switch a {
	case Post:
		return "post"
	case Resolve:
		return "resolve"
	case Recheck:
		return "recheck"
	}
	return fmt.Sprintf("Action(%d)", int(a))
}

// Work is one alert handed out by Due. Alert is a copy.
type Work struct {
	Alert  *domain.Alert
	Action Action
}

// Outcome is what came of a Work, reported to Done.
type Outcome int

const (
	Sent    Outcome = iota + 1 // Post: the card went out
	Closed                     // finished: shown resolved, or no longer failing before it posted, or the card is gone
	Open                       // Recheck: the app still reports the issue
	Gone                       // Recheck: the app no longer reports it; it is resolved now
	Failed                     // delivery failed: retried, then dropped
	Aborted                    // shutting down: nothing happened
)

// Drop reasons passed to Manager.Dropped.
const (
	DropCleared = "cleared" // passed within its hold: never posted
	DropFailed  = "failed"  // delivery failed RetryMax times
)

// Manager holds alerts between their events and their delivery. Safe for concurrent use.
type Manager struct {
	cfg   Config
	clock domain.Clock
	store domain.Store
	// Dropped, when set, is called for every alert dropped without being posted.
	Dropped func(a *domain.Alert, reason string)

	mu       sync.Mutex
	alerts   map[string]*domain.Alert // loaded on first use; the store is written through
	rev      map[string]int           // bumped by every Add that changes an alert
	inflight map[string]flight        // handed out by Due, not yet reported by Done
}

type flight struct {
	action Action
	rev    int
}

func New(cfg Config, clock domain.Clock, store domain.Store) *Manager {
	return &Manager{cfg: cfg, clock: clock, store: store, rev: map[string]int{}, inflight: map[string]flight{}}
}

func healthKey(source, check string) string { return source + ":health:" + check }

// Add records one alert event.
func (m *Manager) Add(ctx context.Context, e domain.AlertEvent) (Result, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	all, err := m.loaded(ctx)
	if err != nil {
		return "", fmt.Errorf("alert: %w", err)
	}
	now := m.clock.Now()
	switch e.Kind {
	case domain.AlertUpdate:
		if !m.cfg.Updates {
			return Ignored, nil
		}
		k := e.Source + ":update:" + e.To
		if all[k] != nil {
			return Ignored, nil
		}
		return Update, m.put(ctx, &domain.Alert{
			Key: k, Source: e.Source, Kind: domain.AlertUpdate, App: e.App, AppURL: e.AppURL,
			Message: e.Message, From: e.From, To: e.To, First: now,
		})

	case domain.AlertHealth:
		k := healthKey(e.Source, e.Check)
		cur := all[k]
		if cur == nil {
			return Held, m.put(ctx, &domain.Alert{
				Key: k, Source: e.Source, Kind: domain.AlertHealth, App: e.App, AppURL: e.AppURL,
				Check: e.Check, Level: e.Level, Message: e.Message, WikiURL: e.WikiURL, First: now,
			})
		}
		a := *cur
		a.App, a.AppURL, a.Level, a.Message, a.WikiURL = e.App, e.AppURL, e.Level, e.Message, e.WikiURL
		// Failing again within a hold of clearing: one issue, so a flapping check still posts once
		// it has been failing on and off for a hold. A posted card stays and shows it open again.
		a.Resolved = time.Time{}
		return Repeat, m.put(ctx, &a)

	case domain.AlertRestored:
		cur := all[healthKey(e.Source, e.Check)]
		if cur == nil || !cur.Resolved.IsZero() {
			return Ignored, nil
		}
		a := *cur
		a.Resolved = now
		if a.Posted == nil {
			return Cleared, m.put(ctx, &a)
		}
		return Restored, m.put(ctx, &a)
	}
	return "", fmt.Errorf("alert: unknown event kind %q", e.Kind)
}

// hold is how long an issue at level must last before it posts.
func (m *Manager) hold(level string) time.Duration {
	if level == domain.LevelError {
		return m.cfg.HoldErrors
	}
	return m.cfg.Hold
}

// Due hands out every alert with something to do, and drops the ones that cleared within their
// hold. Each one handed out must be reported to Done.
func (m *Manager) Due(ctx context.Context) ([]Work, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	all, err := m.loaded(ctx)
	if err != nil {
		return nil, fmt.Errorf("alert: due: %w", err)
	}
	now := m.clock.Now()
	var out []Work
	for _, k := range slices.Sorted(maps.Keys(all)) {
		a := all[k]
		if _, sending := m.inflight[k]; sending || now.Before(a.NotBefore) {
			continue
		}
		var action Action
		switch {
		case a.Posted == nil && a.Kind == domain.AlertUpdate:
			action = Post
		case a.Posted == nil && a.Resolved.IsZero():
			if now.Before(a.First.Add(m.hold(a.Level))) {
				continue
			}
			action = Post
		case a.Posted == nil: // cleared: kept for a hold in case it fails again
			if now.Before(a.Resolved.Add(m.hold(a.Level))) {
				continue
			}
			if err := m.drop(ctx, a, DropCleared); err != nil {
				return nil, fmt.Errorf("alert: due: %w", err)
			}
			continue
		case !a.Resolved.IsZero():
			action = Resolve
		default:
			last := a.PostedAt
			if a.Checked.After(last) {
				last = a.Checked
			}
			if m.cfg.Recheck <= 0 || now.Before(last.Add(m.cfg.Recheck)) {
				continue
			}
			action = Recheck
		}
		c := clone(a)
		out = append(out, Work{Alert: c, Action: action})
	}
	for _, w := range out {
		m.inflight[w.Alert.Key] = flight{action: w.Action, rev: m.rev[w.Alert.Key]}
	}
	return out, nil
}

// Done reports what came of an alert Due handed out. msg is the posted card (Sent only).
func (m *Manager) Done(ctx context.Context, key string, outcome Outcome, msg *domain.Message) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	f, ok := m.inflight[key]
	if !ok {
		return fmt.Errorf("alert: done %s: not handed out by Due", key)
	}
	delete(m.inflight, key)
	cur := m.alerts[key]
	if cur == nil || outcome == Aborted {
		return nil
	}
	now := m.clock.Now()
	changed := m.rev[key] != f.rev // an event arrived while it was out
	a := *cur
	switch outcome {
	case Sent:
		if msg == nil {
			return fmt.Errorf("alert: done %s: sent without a message", key)
		}
		if a.Kind == domain.AlertUpdate {
			return m.remove(ctx, key)
		}
		posted := *msg
		a.Posted, a.PostedAt, a.Checked, a.Tries, a.NotBefore = &posted, now, now, 0, time.Time{}
	case Closed:
		switch {
		case f.action == Resolve && changed && a.Resolved.IsZero():
			// It failed again while its card was being marked resolved: a new issue.
			a.Posted, a.PostedAt, a.Checked, a.First, a.Tries, a.NotBefore = nil, time.Time{}, time.Time{}, now, 0, time.Time{}
		case f.action == Post && changed:
			// Restored or reported again while being checked: keep it for the next Due.
		case f.action == Post:
			return m.drop(ctx, &a, DropCleared)
		default:
			return m.remove(ctx, key)
		}
	case Open:
		a.Checked = now
	case Gone:
		// Unless it was reported failing again while the app was being asked.
		if a.Resolved.IsZero() && !changed {
			a.Resolved = now
		}
		a.Checked = now
	case Failed:
		a.Tries++
		if a.Tries >= m.cfg.RetryMax {
			if a.Posted == nil {
				return m.drop(ctx, &a, DropFailed)
			}
			return m.remove(ctx, key) // its card stays as it is
		}
		a.NotBefore = now.Add(m.cfg.RetryInterval)
	default:
		return fmt.Errorf("alert: done %s: unknown outcome %d", key, outcome)
	}
	return m.save(ctx, &a)
}

// Refresh replaces the message of an alert Due handed out with the app's latest, so its later
// cards (resolved) show what was posted. It doesn't count as an event.
func (m *Manager) Refresh(ctx context.Context, key, message string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	cur := m.alerts[key]
	if _, out := m.inflight[key]; !out || cur == nil || message == "" || cur.Message == message {
		return nil
	}
	a := *cur
	a.Message = message
	return m.save(ctx, &a)
}

func (m *Manager) loaded(ctx context.Context) (map[string]*domain.Alert, error) {
	if m.alerts == nil {
		all, err := m.store.LoadAlerts(ctx)
		if err != nil {
			return nil, err
		}
		m.alerts = all
	}
	return m.alerts, nil
}

// put saves a changed alert from Add.
func (m *Manager) put(ctx context.Context, a *domain.Alert) error {
	if err := m.save(ctx, a); err != nil {
		return err
	}
	m.rev[a.Key]++
	return nil
}

func (m *Manager) save(ctx context.Context, a *domain.Alert) error {
	if err := m.store.SaveAlert(ctx, a); err != nil {
		return err
	}
	m.alerts[a.Key] = clone(a)
	return nil
}

func (m *Manager) remove(ctx context.Context, key string) error {
	if err := m.store.DeleteAlert(ctx, key); err != nil {
		return err
	}
	delete(m.alerts, key)
	delete(m.rev, key)
	return nil
}

func (m *Manager) drop(ctx context.Context, a *domain.Alert, reason string) error {
	if err := m.remove(ctx, a.Key); err != nil {
		return err
	}
	if m.Dropped != nil {
		m.Dropped(clone(a), reason)
	}
	return nil
}

func clone(a *domain.Alert) *domain.Alert {
	c := *a
	if a.Posted != nil {
		p := *a.Posted
		c.Posted = &p
	}
	return &c
}
