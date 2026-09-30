// Package config loads and validates heraldarr's YAML configuration. See config.example.yaml.
package config

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
)

type Config struct {
	Server       Server        `yaml:"server"`
	Timing       Timing        `yaml:"timing"`
	Backlog      Backlog       `yaml:"backlog"`
	MediaServer  *MediaServer  `yaml:"media_server"` // optional: no deep links, no "wait for the library"
	Sources      []Source      `yaml:"sources"`
	Destinations []Destination `yaml:"destinations"`
	Routes       []Route       `yaml:"routes"`
}

type Server struct {
	Listen    string     `yaml:"listen"`
	DataDir   string     `yaml:"data_dir"`
	PublicURL string     `yaml:"public_url"` // how the *arrs reach heraldarr; `setup` requires it
	Auth      *BasicAuth `yaml:"auth"`       // the *arr webhook connection's Username/Password
	// Called (GET) at most once a minute while the flush loop runs without a store error, for a
	// dead man's switch like healthchecks.io. Often holds a token, so it is a Secret.
	HeartbeatURL Secret `yaml:"heartbeat_url"`
}

type BasicAuth struct {
	Username string `yaml:"username"`
	Password Secret `yaml:"password"`
}

type Timing struct {
	QuietEpisodes   time.Duration `yaml:"quiet_episodes"`   // new episodes people follow
	QuietBacklog    time.Duration `yaml:"quiet_backlog"`    // back-catalog series/seasons
	QuietMovies     time.Duration `yaml:"quiet_movies"`     // per source
	MaxHold         time.Duration `yaml:"max_hold"`         // after the first item, whatever happens
	FollowingWindow time.Duration `yaml:"following_window"` // "aired within" for following
	FollowingEarly  time.Duration `yaml:"following_early"`  // imported this long before its air date: still new
	DigestFrom      int           `yaml:"digest_from"`      // movies in one batch -> one digest card
	Reannounce      time.Duration `yaml:"reannounce_after"` // never announce an item twice within
	RetryInterval   time.Duration `yaml:"retry_interval"`   // Discord or *arr unreachable
	RetryMax        int           `yaml:"retry_max"`        // then drop with a log line
}

// Backlog is how back-catalog TV is announced: episodes that aired more than following_window ago,
// which arrive as many downloads over hours or days.
type Backlog struct {
	// Mode: lead posts a card once the first episodes settle and folds later ones into it;
	// complete waits until the *arr has nothing left queued for the run, then posts one card;
	// quiet is the reference behavior (quiet_backlog windows, no runs).
	Mode    string        `yaml:"mode"`
	Scope   string        `yaml:"scope"`    // series | season: one run (and card) per show, or per season
	Edit    bool          `yaml:"edit"`     // edit the run's card as episodes land; false: fold them in silently
	Settle  time.Duration `yaml:"settle"`   // quiet before a card or an edit (a season pack imports in a minute or two)
	Idle    time.Duration `yaml:"idle"`     // a run ends after this long with no new episode
	MaxHold time.Duration `yaml:"max_hold"` // complete: post anyway this long after the run's first episode
}

// Backlog modes and scopes.
const (
	BacklogLead     = "lead"
	BacklogComplete = "complete"
	BacklogQuiet    = "quiet"
	ScopeSeries     = "series"
	ScopeSeason     = "season"
)

type MediaServer struct {
	Type           string        `yaml:"type"` // plex
	URL            string        `yaml:"url"`
	Token          Secret        `yaml:"token"`
	PreferencesXML string        `yaml:"preferences_xml"` // alternative to token: read PlexOnlineToken
	PathMap        []PathMap     `yaml:"path_map"`        // *arr path prefix -> media server path prefix
	WaitInterval   time.Duration `yaml:"wait_interval"`
	WaitChecks     int           `yaml:"wait_checks"` // then post without the deep link
}

type PathMap struct {
	From string `yaml:"from"`
	To   string `yaml:"to"`
}

type Source struct {
	Name      string `yaml:"name"` // URL path of its webhook: POST /hook/<name>
	Kind      string `yaml:"kind"` // sonarr | radarr
	URL       string `yaml:"url"`
	APIKey    Secret `yaml:"api_key"`
	ConfigXML string `yaml:"config_xml"` // alternative to api_key: read <ApiKey> from the app's config.xml
}

type Destination struct {
	Name       string `yaml:"name"`
	WebhookURL Secret `yaml:"webhook_url"`
	Username   string `yaml:"username"`
	AvatarURL  string `yaml:"avatar_url"`
	Public     bool   `yaml:"public"`
}

type Route struct {
	Sources     []string `yaml:"sources"`
	Destination string   `yaml:"destination"`
	Style       Style    `yaml:"style"`
}

type Style struct {
	Label       string `yaml:"label"`
	Color       Color  `yaml:"color"`
	TechDetails bool   `yaml:"tech_details"`
}

// Defaults match the reference implementation, except backlog runs (mode quiet is the reference)
// and following_early.
func Defaults() Config {
	return Config{
		Server: Server{Listen: ":8790", DataDir: "/config/data"},
		Timing: Timing{
			QuietEpisodes: 5 * time.Minute, QuietBacklog: 30 * time.Minute, QuietMovies: 5 * time.Minute,
			MaxHold: 4 * time.Hour, FollowingWindow: 14 * 24 * time.Hour, DigestFrom: 4,
			Reannounce: 30 * 24 * time.Hour, RetryInterval: 2 * time.Minute, RetryMax: 30,
			FollowingEarly: 24 * time.Hour,
		},
		Backlog: Backlog{
			Mode: BacklogLead, Scope: ScopeSeries, Edit: true,
			Settle: 5 * time.Minute, Idle: 24 * time.Hour, MaxHold: 24 * time.Hour,
		},
	}
}

// Load reads, defaults, resolves secrets and validates.
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Parse(raw)
}

func Parse(raw []byte) (*Config, error) {
	cfg := Defaults()
	dec := yaml.NewDecoder(strings.NewReader(string(raw)))
	dec.KnownFields(true)
	if err := dec.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	if ms := cfg.MediaServer; ms != nil {
		if ms.Type == "" {
			ms.Type = "plex"
		}
		if ms.WaitInterval == 0 {
			ms.WaitInterval = 3 * time.Minute
		}
		if ms.WaitChecks == 0 {
			ms.WaitChecks = 4
		}
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// Validate reports every problem at once.
func (c *Config) Validate() error {
	var errs []error
	bad := func(format string, a ...any) { errs = append(errs, fmt.Errorf(format, a...)) }

	sources := map[string]Source{}
	for i, s := range c.Sources {
		switch {
		case !validName(s.Name):
			bad("sources[%d]: name %q must be lowercase letters, digits, - or _", i, s.Name)
		case sources[s.Name].Name != "":
			bad("sources: duplicate name %q", s.Name)
		}
		sources[s.Name] = s
		if s.Kind != "sonarr" && s.Kind != "radarr" {
			bad("source %q: kind must be sonarr or radarr, not %q", s.Name, s.Kind)
		}
		if s.URL == "" {
			bad("source %q: url is required", s.Name)
		}
		if s.APIKey.IsZero() == (s.ConfigXML == "") {
			bad("source %q: set exactly one of api_key or config_xml", s.Name)
		}
		if err := s.APIKey.Err(); err != nil {
			bad("source %q: api_key: %w", s.Name, err)
		}
	}
	dests := map[string]Destination{}
	for i, d := range c.Destinations {
		switch {
		case !validName(d.Name):
			bad("destinations[%d]: name %q must be lowercase letters, digits, - or _", i, d.Name)
		case dests[d.Name].Name != "":
			bad("destinations: duplicate name %q", d.Name)
		}
		dests[d.Name] = d
		if err := d.WebhookURL.Err(); err != nil {
			bad("destination %q: webhook_url: %w", d.Name, err)
		} else if !strings.HasPrefix(d.WebhookURL.Value(), "https://") {
			bad("destination %q: webhook_url must be an https:// Discord webhook URL", d.Name)
		}
	}
	routed := map[string]bool{}
	for i, r := range c.Routes {
		d, ok := dests[r.Destination]
		if !ok {
			bad("routes[%d]: unknown destination %q", i, r.Destination)
		}
		if len(r.Sources) == 0 {
			bad("routes[%d]: at least one source is required", i)
		}
		for _, s := range r.Sources {
			if _, ok := sources[s]; !ok {
				bad("routes[%d]: unknown source %q", i, s)
			}
			if routed[s] {
				bad("routes[%d]: source %q is already routed (one route per source for now)", i, s)
			}
			routed[s] = true
		}
		if ok && d.Public && r.Style.TechDetails {
			bad("routes[%d]: tech_details would show sizes and release groups on public destination %q", i, d.Name)
		}
	}
	for _, s := range c.Sources {
		if s.Name != "" && !routed[s.Name] {
			bad("source %q: not used by any route", s.Name)
		}
	}
	if ms := c.MediaServer; ms != nil {
		if ms.Type != "plex" {
			bad("media_server: type %q is not supported (plex)", ms.Type)
		}
		if ms.URL == "" {
			bad("media_server: url is required")
		}
		if ms.Token.IsZero() == (ms.PreferencesXML == "") {
			bad("media_server: set exactly one of token or preferences_xml")
		}
		if err := ms.Token.Err(); err != nil {
			bad("media_server: token: %w", err)
		}
	}
	if u := c.Server.PublicURL; u != "" {
		if err := checkPublicURL(u); err != nil {
			bad("server.public_url: %w", err)
		}
	}
	if a := c.Server.Auth; a != nil {
		if a.Username == "" || a.Password.IsZero() {
			bad("server.auth: username and password are both required")
		}
		if err := a.Password.Err(); err != nil {
			bad("server.auth: password: %w", err)
		}
	}
	if hb := c.Server.HeartbeatURL; !hb.IsZero() {
		if err := hb.Err(); err != nil {
			bad("server.heartbeat_url: %w", err)
		} else if u, err := url.Parse(hb.Value()); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			bad("server.heartbeat_url must be an http:// or https:// URL") // no value: it may be secret
		}
	}
	t := c.Timing
	if t.QuietEpisodes <= 0 || t.QuietBacklog <= 0 || t.QuietMovies <= 0 || t.MaxHold <= 0 {
		bad("timing: quiet windows and max_hold must be positive")
	}
	if t.DigestFrom < 2 {
		bad("timing: digest_from must be at least 2")
	}
	if t.FollowingEarly < 0 {
		bad("timing: following_early must not be negative")
	}
	bl := c.Backlog
	switch bl.Mode {
	case BacklogLead, BacklogComplete, BacklogQuiet:
	default:
		bad("backlog: mode must be lead, complete or quiet, not %q", bl.Mode)
	}
	if bl.Scope != ScopeSeries && bl.Scope != ScopeSeason {
		bad("backlog: scope must be series or season, not %q", bl.Scope)
	}
	if bl.Mode != BacklogQuiet && (bl.Settle <= 0 || bl.Idle <= 0 || bl.MaxHold <= 0) {
		bad("backlog: settle, idle and max_hold must be positive")
	}
	return errors.Join(errs...)
}

// RouteFor returns the route a source posts through.
func (c *Config) RouteFor(source string) (Route, bool) {
	for _, r := range c.Routes {
		if slices.Contains(r.Sources, source) {
			return r, true
		}
	}
	return Route{}, false
}

// checkPublicURL accepts an http(s) base URL, with a path if heraldarr sits behind a proxy.
// Credentials belong in server.auth, not in the URL: the *arr would show them. Errors don't quote
// the URL, which may hold such credentials.
func checkPublicURL(raw string) error {
	if lower := strings.ToLower(raw); !strings.HasPrefix(lower, "http://") && !strings.HasPrefix(lower, "https://") {
		return errors.New("must start with http:// or https://")
	}
	u, err := url.Parse(raw)
	var uerr *url.Error
	switch {
	case errors.As(err, &uerr):
		return fmt.Errorf("not a valid URL: %w", uerr.Err) // uerr itself quotes the URL
	case err != nil:
		return errors.New("not a valid URL")
	case u.Hostname() == "":
		return errors.New("has no host")
	case u.User != nil:
		return errors.New("must not contain a username or password (use server.auth)")
	case u.RawQuery != "" || u.Fragment != "":
		return errors.New("must not have a query or fragment")
	}
	return nil
}

var nameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)

func validName(s string) bool { return nameRE.MatchString(s) }

// Color is an accent color written as "#9B59B6" or a number.
type Color int

func (c *Color) UnmarshalYAML(n *yaml.Node) error {
	s := strings.TrimPrefix(strings.TrimSpace(n.Value), "#")
	base := 10
	if strings.HasPrefix(n.Value, "#") || strings.HasPrefix(s, "0x") {
		base = 16
		s = strings.TrimPrefix(s, "0x")
	}
	v, err := strconv.ParseInt(s, base, 32)
	if err != nil || v < 0 || v > 0xFFFFFF {
		return fmt.Errorf("line %d: color %q: want #RRGGBB", n.Line, n.Value)
	}
	*c = Color(v)
	return nil
}
