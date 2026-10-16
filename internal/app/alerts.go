package app

import (
	"context"
	"errors"
	"slices"

	"github.com/wtwerner/heraldarr/internal/alert"
	"github.com/wtwerner/heraldarr/internal/config"
	"github.com/wtwerner/heraldarr/internal/domain"
	"github.com/wtwerner/heraldarr/internal/render"
)

// Alerts posts the sources' health alerts and application updates to one destination.
type Alerts struct {
	Destination domain.Destination
	// Sources whose alerts are accepted, with the reader that asks the app whether a check still
	// fails (nil: never asked).
	Sources  map[string]domain.HealthReader
	Config   alert.Config
	Restored string // config.RestoredEdit (default) or config.RestoredPost
}

// Alert results counted by heraldarr_alerts_total besides alert.Result.
const (
	alertInvalid = "invalid" // a payload that couldn't be decoded
	alertError   = "error"   // could not be recorded (store failure)
)

func (a *App) alertSources() []string {
	if a.Alerts == nil {
		return nil
	}
	var out []string
	for s := range a.Alerts.Sources {
		out = append(out, s)
	}
	slices.Sort(out)
	return out
}

func (a *App) alerting(source string) bool {
	if a.Alerts == nil {
		return false
	}
	_, ok := a.Alerts.Sources[source]
	return ok
}

// addAlert records an alert event from the webhook; it reports the HTTP status to reply with.
func (a *App) addAlert(ctx context.Context, e domain.AlertEvent) (int, string) {
	res, err := a.alerts.Add(ctx, e)
	if err != nil {
		a.metrics.alerts.add(1, e.Source, alertError)
		a.Log.Error("recording alert", "source", e.Source, "err", err)
		return 500, "could not record the alert"
	}
	a.metrics.alerts.add(1, e.Source, string(res))
	a.Log.Info("alert: "+string(res), "source", e.Source, "kind", e.Kind, "check", e.Check, "level", e.Level,
		"message", e.Message)
	if res == alert.Restored || res == alert.Update || (res == alert.Held && e.Level == domain.LevelError) {
		a.Kick(false)
	}
	return 200, ""
}

// flushAlerts delivers every alert with something to do. It reports false on a store error or
// when shutting down.
func (a *App) flushAlerts(ctx context.Context) bool {
	if a.alerts == nil {
		return true
	}
	work, err := a.alerts.Due(ctx)
	if err != nil {
		a.Log.Error("flush: loading due alerts", "err", err)
		return false
	}
	ok := true
	for i, w := range work {
		outcome, msg := alert.Aborted, (*domain.Message)(nil)
		if ctx.Err() == nil {
			outcome, msg = a.deliverAlert(ctx, w)
		}
		if ctx.Err() != nil && outcome == alert.Failed {
			outcome = alert.Aborted // interrupted, not failed
		}
		if err := a.alerts.Done(context.WithoutCancel(ctx), w.Alert.Key, outcome, msg); err != nil {
			a.Log.Error("recording alert outcome", "alert", w.Alert.Key, "err", err)
			ok = false
		}
		if ctx.Err() != nil {
			for _, rest := range work[i+1:] {
				_ = a.alerts.Done(context.WithoutCancel(ctx), rest.Alert.Key, alert.Aborted, nil)
			}
			return false
		}
	}
	return ok
}

func (a *App) deliverAlert(ctx context.Context, w alert.Work) (alert.Outcome, *domain.Message) {
	al, dest := w.Alert, a.Alerts.Destination
	if !a.alerting(al.Source) {
		a.Log.Info("alert dropped: source no longer in alerts", "alert", al.Key)
		return alert.Closed, nil
	}
	switch w.Action {
	case alert.Recheck:
		if now, known := a.current(ctx, al); known && now == nil {
			a.Log.Info("alert resolved: the app no longer reports it", "alert", al.Key)
			return alert.Gone, nil
		}
		return alert.Open, nil

	case alert.Post:
		if al.Kind == domain.AlertHealth {
			now, known := a.current(ctx, al)
			if known && now == nil {
				a.Log.Info("alert cleared before posting (the app no longer reports it)", "alert", al.Key)
				return alert.Closed, nil
			}
			if now != nil && now.Message != "" && now.Message != al.Message {
				al.Message = now.Message // the app's latest (e.g. more indexers down by now)
				if err := a.alerts.Refresh(context.WithoutCancel(ctx), al.Key, al.Message); err != nil {
					a.Log.Warn("recording the app's current alert message", "alert", al.Key, "err", err)
				}
			}
		}
		res, err := a.Notifier.Post(ctx, dest, render.Alert(al, a.Clock.Now()))
		if err != nil {
			a.Log.Warn("alert delivery failed", "alert", al.Key, "err", err, "try", al.Tries+1)
			return alert.Failed, nil
		}
		a.recordAlert(ctx, al, dest, res)
		return alert.Sent, &domain.Message{Destination: dest.Name, ID: res.MessageID, Layout: res.Layout}

	case alert.Resolve:
		layouts := render.Alert(al, a.Clock.Now())
		if a.Alerts.Restored == config.RestoredPost {
			res, err := a.Notifier.Post(ctx, dest, layouts)
			if err != nil {
				a.Log.Warn("alert delivery failed", "alert", al.Key, "err", err, "try", al.Tries+1)
				return alert.Failed, nil
			}
			a.recordAlert(ctx, al, dest, res)
			return alert.Closed, nil
		}
		layout := layouts[0]
		for _, l := range layouts {
			if l.Name == al.Posted.Layout {
				layout = l
			}
		}
		switch err := a.Notifier.Edit(ctx, dest, al.Posted.ID, layout); {
		case errors.Is(err, domain.ErrNotFound), errors.Is(err, domain.ErrRefused):
			a.Log.Info("alert card can't be edited; closing it", "alert", al.Key, "err", err)
		case err != nil:
			a.Log.Warn("alert edit failed", "alert", al.Key, "err", err, "try", al.Tries+1)
			return alert.Failed, nil
		default:
			a.Log.Info("alert resolved", "alert", al.Key, "lasted", al.Resolved.Sub(al.First).String())
		}
		return alert.Closed, nil
	}
	return alert.Aborted, nil
}

// current asks the alert's app for its check: nil when the app reports it passing. known is false
// when the app can't be asked (no reader, or an error): the caller goes ahead as if it still fails.
func (a *App) current(ctx context.Context, al *domain.Alert) (check *domain.HealthCheck, known bool) {
	r := a.Alerts.Sources[al.Source]
	if r == nil {
		return nil, false
	}
	checks, err := r.Health(ctx)
	if err != nil {
		a.Log.Warn("app health unavailable", "source", al.Source, "err", err)
		return nil, false
	}
	if i := slices.IndexFunc(checks, func(c domain.HealthCheck) bool { return c.Check == al.Check }); i >= 0 {
		return &checks[i], true
	}
	return nil, true
}

func (a *App) recordAlert(ctx context.Context, al *domain.Alert, dest domain.Destination, res domain.PostResult) {
	now := a.Clock.Now()
	a.metrics.posted(al.Source, dest.Name, res.Layout, now)
	headline := al.Level
	switch {
	case al.Kind == domain.AlertUpdate:
		headline = "update"
	case !al.Resolved.IsZero():
		headline = "resolved"
	}
	a.Log.Info("posted alert", "alert", al.Key, "headline", headline, "layout", res.Layout)
	if err := a.Store.AppendHistory(context.WithoutCancel(ctx), domain.HistoryEntry{
		At: now, Source: al.Source, Destination: dest.Name, Title: al.Message, Headline: headline,
		MessageID: res.MessageID,
	}); err != nil {
		a.Log.Warn("recording post history", "err", err)
	}
}
