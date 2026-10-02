// Package plex is the Plex implementation of domain.MediaServer.
//
// Find avoids listing whole libraries: it asks each library for its most recently added items
// first (that is where a fresh import is) and only falls back to a full listing on a miss. Both
// listings are cached per library, the recent one briefly so a scan's results show up on the next
// check, the full one for longer.
package plex

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/wtwerner/heraldarr/internal/domain"
)

const (
	sectionsTTL = 10 * time.Minute // the library list rarely changes
	recentTTL   = time.Minute      // shorter than the batcher's wait interval, so each check re-asks
	allTTL      = 10 * time.Minute // a full listing is only needed for items added long ago
	recentSize  = 50               // items per library in the recently added listing
	httpTimeout = 20 * time.Second
	userAgent   = "heraldarr (+https://github.com/wtwerner/heraldarr)"
)

// PathMap rewrites an *arr path prefix to how the same folder looks to Plex.
type PathMap struct {
	From string
	To   string
}

// Option configures a Plex client.
type Option func(*Plex)

// WithClock sets the clock the caches expire by.
func WithClock(c domain.Clock) Option { return func(p *Plex) { p.clock = c } }

// WithHTTPClient replaces the default client (20 s timeout).
func WithHTTPClient(c *http.Client) Option { return func(p *Plex) { p.client = c } }

// Plex talks to one Plex Media Server. It is safe for concurrent use.
type Plex struct {
	base    string
	token   string
	pathMap []PathMap
	client  *http.Client
	clock   domain.Clock

	server atomic.Pointer[identity] // nil until the first request that needs it

	mu         sync.Mutex // guards the fields below; held across requests so callers share one fetch
	sections   []*section
	sectionsAt time.Time
	byKey      map[string]*section // listing caches survive a refresh of the library list
}

type identity struct {
	ID   string // machineIdentifier
	Name string // friendlyName
}

type section struct {
	key       string
	typ       string   // "movie" | "show"
	locations []string // with a trailing slash, so /media/Movies/ doesn't hold /media/Movies 4K/
	recent    listing
	all       listing
}

type listing struct {
	items map[string]*metadata // GUID -> item
	at    time.Time
}

// metadata is the part of a Plex item heraldarr reads.
type metadata struct {
	RatingKey     string `json:"ratingKey"`
	Index         int    `json:"index"`
	ContentRating string `json:"contentRating"`
	// Plex sends both "guid" (its own plex:// ID) and "Guid" (the external IDs Find matches).
	// encoding/json matches keys case-insensitively, so "guid" needs a field of its own.
	PlexGUID string `json:"guid"`
	GUIDs    []struct {
		ID string `json:"id"`
	} `json:"Guid"`
}

// New returns a client for the server at baseURL ("http://plex:32400"). pathMap is tried in order;
// the first From that prefixes an *arr path wins.
func New(baseURL, token string, pathMap []PathMap, opts ...Option) *Plex {
	p := &Plex{
		base:    strings.TrimRight(baseURL, "/"),
		token:   token,
		pathMap: pathMap,
		client:  &http.Client{Timeout: httpTimeout},
		clock:   systemClock{},
		byKey:   map[string]*section{},
	}
	for _, o := range opts {
		o(p)
	}
	return p
}

// TokenFromPreferences reads PlexOnlineToken from Plex's Preferences.xml.
func TokenFromPreferences(path string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("plex preferences: %w", err)
	}
	var prefs struct {
		Token string `xml:"PlexOnlineToken,attr"`
	}
	if err := xml.Unmarshal(raw, &prefs); err != nil {
		return "", fmt.Errorf("plex preferences %s: %w", path, err)
	}
	if prefs.Token == "" {
		return "", fmt.Errorf("plex preferences %s: no PlexOnlineToken", path)
	}
	return prefs.Token, nil
}

// Name is the server's friendly name, "" until the server has been asked.
func (p *Plex) Name() string {
	if id := p.server.Load(); id != nil {
		return id.Name
	}
	return ""
}

// URL links to the item on app.plex.tv, which works for everyone the library is shared with.
// It is "" until the server has been asked (Find does).
func (p *Plex) URL(ratingKey string) string {
	id := p.server.Load()
	if id == nil {
		return ""
	}
	return "https://app.plex.tv/desktop/#!/server/" + id.ID + "/details?key=" +
		url.QueryEscape("/library/metadata/"+ratingKey)
}

// Find looks for the item in the libraries whose folders hold path (Movies vs Movies 4K), or, when
// none does, in every library of the kind. Each library's recently added items are checked before
// its full listing.
func (p *Plex) Find(ctx context.Context, guids []string, path string, kind domain.Kind, fresh bool) (*domain.MediaItem, error) {
	if err := p.identify(ctx); err != nil {
		return nil, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.loadSections(ctx, fresh); err != nil {
		return nil, err
	}
	typ := "movie"
	if kind == domain.KindTV {
		typ = "show"
	}
	var candidates, holding []*section
	for _, s := range p.sections {
		if s.typ == typ {
			candidates = append(candidates, s)
			if s.holds(p.mapPath(path)) {
				holding = append(holding, s)
			}
		}
	}
	if len(holding) > 0 {
		candidates = holding
	}
	for _, s := range candidates {
		for _, full := range []bool{false, true} {
			items, err := p.listing(ctx, s, full, fresh)
			if err != nil {
				return nil, err
			}
			for _, g := range guids {
				if m, ok := items[g]; ok && g != "" {
					return m.item(), nil
				}
			}
		}
	}
	return nil, nil
}

// SeasonKey returns the rating key of the show's season, "" if Plex doesn't have it.
func (p *Plex) SeasonKey(ctx context.Context, show *domain.MediaItem, season int) (string, error) {
	if show == nil || show.RatingKey == "" {
		return "", nil
	}
	var c struct {
		Metadata []metadata `json:"Metadata"`
	}
	if err := p.get(ctx, "/library/metadata/"+url.PathEscape(show.RatingKey)+"/children", nil, &c); err != nil {
		return "", err
	}
	for _, m := range c.Metadata {
		if m.Index == season {
			return m.RatingKey, nil
		}
	}
	return "", nil
}

// Scores reads the item's Rating list, which holds every source Plex has (IMDb, Rotten Tomatoes
// critic and audience, TMDB) whichever one the library shows. Library listings leave it out.
func (p *Plex) Scores(ctx context.Context, ratingKey string) (domain.Scores, error) {
	var c struct {
		Metadata []struct {
			Rating []struct {
				Image string  `json:"image"` // "imdb://image.rating", "rottentomatoes://image.rating.ripe", …
				Type  string  `json:"type"`  // critic | audience
				Value float64 `json:"value"` // out of 10
			} `json:"Rating"`
		} `json:"Metadata"`
	}
	var s domain.Scores
	if err := p.get(ctx, "/library/metadata/"+url.PathEscape(ratingKey), nil, &c); err != nil {
		return s, err
	}
	if len(c.Metadata) == 0 {
		return s, nil
	}
	for _, r := range c.Metadata[0].Rating {
		src, _, _ := strings.Cut(r.Image, ":")
		switch {
		case src == "imdb":
			s.IMDb = r.Value
		case src == "rottentomatoes" && r.Type == "critic":
			s.RTCritic = math.Round(r.Value * 10)
		case src == "rottentomatoes" && r.Type == "audience":
			s.RTAudience = math.Round(r.Value * 10)
		}
	}
	return s, nil
}

// Scan requests a partial scan of path in the library that holds it. A path no library holds is
// logged and ignored.
func (p *Plex) Scan(ctx context.Context, path string) error {
	mapped := p.mapPath(path)
	p.mu.Lock()
	err := p.loadSections(ctx, false)
	var target *section
	for _, s := range p.sections {
		if target == nil && s.holds(mapped) {
			target = s
		}
	}
	p.mu.Unlock()
	if err != nil {
		return err
	}
	if target == nil {
		slog.Warn("plex: no library holds path, not scanning", "path", mapped)
		return nil
	}
	if err := p.get(ctx, "/library/sections/"+target.key+"/refresh", url.Values{"path": {mapped}}, nil); err != nil {
		return err
	}
	slog.Info("plex: partial scan requested", "section", target.key, "path", mapped)
	return nil
}

func (p *Plex) mapPath(path string) string {
	for _, m := range p.pathMap {
		if m.From != "" && strings.HasPrefix(path, m.From) {
			return m.To + path[len(m.From):]
		}
	}
	return path
}

func (s *section) holds(path string) bool {
	if path == "" {
		return false
	}
	for _, loc := range s.locations {
		if strings.HasPrefix(path, loc) {
			return true
		}
	}
	return false
}

func (m *metadata) item() *domain.MediaItem {
	return &domain.MediaItem{RatingKey: m.RatingKey, ContentRating: m.ContentRating}
}

func (p *Plex) identify(ctx context.Context) error {
	if p.server.Load() != nil {
		return nil
	}
	var root struct {
		MachineIdentifier string `json:"machineIdentifier"`
		FriendlyName      string `json:"friendlyName"`
	}
	if err := p.get(ctx, "/", nil, &root); err != nil {
		return err
	}
	if root.MachineIdentifier == "" {
		return errors.New("plex: server reported no machineIdentifier")
	}
	if root.FriendlyName == "" {
		root.FriendlyName = "Plex"
	}
	p.server.Store(&identity{ID: root.MachineIdentifier, Name: root.FriendlyName})
	return nil
}

// loadSections refreshes the library list when stale. Callers hold p.mu.
func (p *Plex) loadSections(ctx context.Context, fresh bool) error {
	if !fresh && p.sections != nil && p.clock.Now().Sub(p.sectionsAt) < sectionsTTL {
		return nil
	}
	var c struct {
		Directory []struct {
			Key      string `json:"key"`
			Type     string `json:"type"`
			Location []struct {
				Path string `json:"path"`
			} `json:"Location"`
		} `json:"Directory"`
	}
	if err := p.get(ctx, "/library/sections", nil, &c); err != nil {
		return err
	}
	sections := make([]*section, 0, len(c.Directory))
	for _, d := range c.Directory {
		if d.Type != "movie" && d.Type != "show" {
			continue
		}
		s := p.byKey[d.Key]
		if s == nil || s.typ != d.Type {
			s = &section{key: d.Key, typ: d.Type}
			p.byKey[d.Key] = s
		}
		s.locations = s.locations[:0]
		for _, l := range d.Location {
			s.locations = append(s.locations, strings.TrimRight(l.Path, "/")+"/")
		}
		sections = append(sections, s)
	}
	p.sections, p.sectionsAt = sections, p.clock.Now()
	return nil
}

// listing returns a library's recently added (full=false) or every item by GUID, from the cache
// unless stale or fresh. Callers hold p.mu.
func (p *Plex) listing(ctx context.Context, s *section, full, fresh bool) (map[string]*metadata, error) {
	l, ttl := &s.recent, recentTTL
	if full {
		l, ttl = &s.all, allTTL
	}
	now := p.clock.Now()
	if !fresh && l.items != nil && now.Sub(l.at) < ttl {
		return l.items, nil
	}
	q := url.Values{"includeGuids": {"1"}}
	if !full {
		// A show's addedAt is when the show first arrived; episode.addedAt is its newest episode.
		sort, typ := "addedAt:desc", "1"
		if s.typ == "show" {
			sort, typ = "episode.addedAt:desc", "2"
		}
		q.Set("type", typ)
		q.Set("sort", sort)
		q.Set("X-Plex-Container-Start", "0")
		q.Set("X-Plex-Container-Size", strconv.Itoa(recentSize))
	}
	var c struct {
		Metadata []*metadata `json:"Metadata"`
	}
	if err := p.get(ctx, "/library/sections/"+s.key+"/all", q, &c); err != nil {
		return nil, err
	}
	items := make(map[string]*metadata, len(c.Metadata))
	for _, m := range c.Metadata {
		for _, g := range m.GUIDs {
			items[g.ID] = m
		}
	}
	if full {
		slog.Debug("plex: listed library", "section", s.key, "items", len(c.Metadata))
	}
	*l = listing{items: items, at: now}
	return items, nil
}

// get fetches a Plex endpoint and decodes its MediaContainer into out (nil: ignore the body).
// A 404 is domain.ErrNotFound.
func (p *Plex) get(ctx context.Context, path string, q url.Values, out any) error {
	u := p.base + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return fmt.Errorf("plex %s: %w", path, err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("X-Plex-Token", p.token)
	resp, err := p.client.Do(req)
	if err != nil {
		return fmt.Errorf("plex %s: %w", path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return fmt.Errorf("plex %s: %w", path, domain.ErrNotFound)
	case resp.StatusCode >= 300:
		return fmt.Errorf("plex %s: %s", path, resp.Status)
	case out == nil:
		return nil
	}
	var env struct {
		MediaContainer json.RawMessage `json:"MediaContainer"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		return fmt.Errorf("plex %s: %w", path, err)
	}
	if len(env.MediaContainer) == 0 {
		return fmt.Errorf("plex %s: no MediaContainer in response", path)
	}
	if err := json.Unmarshal(env.MediaContainer, out); err != nil {
		return fmt.Errorf("plex %s: %w", path, err)
	}
	return nil
}

type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now() }

var _ domain.MediaServer = (*Plex)(nil)
