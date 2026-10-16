package app

import (
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/wtwerner/heraldarr/internal/alert"
	"github.com/wtwerner/heraldarr/internal/batcher"
	"github.com/wtwerner/heraldarr/internal/config"
	"github.com/wtwerner/heraldarr/internal/domain"
	"github.com/wtwerner/heraldarr/internal/enrich"
	"github.com/wtwerner/heraldarr/internal/mediaserver/plex"
	"github.com/wtwerner/heraldarr/internal/notify/discord"
	"github.com/wtwerner/heraldarr/internal/source/arr"
	"github.com/wtwerner/heraldarr/internal/store"
)

type wallClock struct{}

func (wallClock) Now() time.Time { return time.Now() }

// Service is the App plus what the commands need beside it.
type Service struct {
	*App
	Store        *store.Store
	Arr          map[string]*arr.Client
	Destinations map[string]domain.Destination
}

// Close releases the store.
func (s *Service) Close() error { return s.Store.Close() }

// FromConfig builds the service from a validated configuration.
func FromConfig(cfg *config.Config, version string, log *slog.Logger) (*Service, error) {
	if err := os.MkdirAll(cfg.Server.DataDir, 0o750); err != nil {
		return nil, fmt.Errorf("data_dir: %w", err)
	}
	st, err := store.Open(filepath.Join(cfg.Server.DataDir, "heraldarr.db"))
	if err != nil {
		return nil, err
	}
	clock := wallClock{}
	ua := "heraldarr/" + version
	svc := &Service{Store: st, Arr: map[string]*arr.Client{}, Destinations: map[string]domain.Destination{}}

	for _, d := range cfg.Destinations {
		svc.Destinations[d.Name] = domain.Destination{
			Name: d.Name, WebhookURL: d.WebhookURL.Value(),
			Username: d.Username, AvatarURL: d.AvatarURL, Public: d.Public,
		}
	}
	sources := map[string]Source{}
	clients := map[string]*arr.Client{}
	for _, s := range cfg.Sources {
		key := arr.StaticKey(s.APIKey.Value())
		if s.ConfigXML != "" {
			key = arr.ConfigXMLKey(s.ConfigXML)
		}
		if s.Kind == "prowlarr" {
			clients[s.Name] = arr.NewProwlarrClient(s.URL, key, version, arr.DefaultTimeout)
			continue // alerts only
		}
		client := arr.NewClient(s.URL, key, version, arr.DefaultTimeout)
		clients[s.Name] = client
		svc.Arr[s.Name] = client
		kind := domain.KindTV
		if s.Kind == "radarr" {
			kind = domain.KindMovie
		}
		r, routed := cfg.RouteFor(s.Name)
		if !routed {
			continue // alerts only
		}
		sources[s.Name] = Source{
			Kind: kind, Arr: client, Destination: svc.Destinations[r.Destination],
			Style: domain.Style{Label: r.Style.Label, Color: int(r.Style.Color), TechDetails: r.Style.TechDetails},
		}
	}

	d := Deps{
		Clock: clock, Store: st, Sources: sources, Log: log, DigestFrom: cfg.Timing.DigestFrom,
		Notifier: discord.New(&http.Client{Timeout: 30 * time.Second}, clock, ua),
		Timing: batcher.Config{
			QuietEpisodes: cfg.Timing.QuietEpisodes, QuietBacklog: cfg.Timing.QuietBacklog,
			QuietMovies: cfg.Timing.QuietMovies, MaxHold: cfg.Timing.MaxHold,
			FollowingWindow: cfg.Timing.FollowingWindow, Reannounce: cfg.Timing.Reannounce,
			RetryInterval: cfg.Timing.RetryInterval, RetryMax: cfg.Timing.RetryMax,
			FollowingEarly: cfg.Timing.FollowingEarly,
			Backlog: batcher.Backlog{
				Mode: cfg.Backlog.Mode, PerSeason: cfg.Backlog.Scope == config.ScopeSeason, Edit: cfg.Backlog.Edit,
				Settle: cfg.Backlog.Settle, Idle: cfg.Backlog.Idle, MaxHold: cfg.Backlog.MaxHold,
			},
		},
	}
	wiki := enrich.New(st, clock)
	wiki.UserAgent = ua + " (+https://github.com/wtwerner/heraldarr)"
	d.RT = wiki
	if ms := cfg.MediaServer; ms != nil {
		token := ms.Token.Value()
		if ms.PreferencesXML != "" {
			if token, err = plex.TokenFromPreferences(ms.PreferencesXML); err != nil {
				_ = st.Close()
				return nil, fmt.Errorf("media_server: %w", err)
			}
		}
		pm := make([]plex.PathMap, len(ms.PathMap))
		for i, p := range ms.PathMap {
			pm[i] = plex.PathMap{From: p.From, To: p.To}
		}
		d.Media = plex.New(ms.URL, token, pm, plex.WithClock(clock))
		d.Timing.MediaWait, d.WaitChecks = ms.WaitInterval, ms.WaitChecks
	}
	if al := cfg.Alerts; al.Enabled() {
		readers := map[string]domain.HealthReader{}
		for _, name := range cfg.AlertSources() {
			readers[name] = clients[name]
		}
		d.Alerts = &Alerts{
			Destination: svc.Destinations[al.Destination], Sources: readers, Restored: al.Restored,
			Config: alert.Config{
				Hold: al.Hold, HoldErrors: al.HoldErrors, Recheck: al.Recheck, Updates: al.Updates,
				RetryInterval: cfg.Timing.RetryInterval, RetryMax: cfg.Timing.RetryMax,
			},
		}
	}
	d.Heartbeat = cfg.Server.HeartbeatURL.Value()
	if a := cfg.Server.Auth; a != nil {
		d.Auth = &BasicAuth{Username: a.Username, Password: a.Password.Value()}
	}
	svc.App = New(d)
	return svc, nil
}
