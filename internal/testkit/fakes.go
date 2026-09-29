package testkit

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/wtwerner/heraldarr/internal/domain"
	"github.com/wtwerner/heraldarr/internal/source/arr"
)

// Clock is a settable clock.
type Clock struct {
	mu sync.Mutex
	t  time.Time
}

func NewClock(t time.Time) *Clock { return &Clock{t: t} }

func (c *Clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *Clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// ErrUnreachable is what the fakes return for data the scenario doesn't have.
var ErrUnreachable = errors.New("unreachable (no stub)")

// Arr answers from a scenario's "arr" map.
type Arr struct{ Sc *Scenario }

func (a Arr) body(path string) ([]byte, error) {
	raw, ok := a.Sc.Arr[path]
	switch {
	case !ok:
		return nil, fmt.Errorf("%s: %w", path, ErrUnreachable)
	case string(raw) == "null":
		return nil, fmt.Errorf("%s: %w", path, domain.ErrNotFound)
	}
	return raw, nil
}

func (a Arr) Series(_ context.Context, id int) (*domain.SeriesDetail, error) {
	b, err := a.body("series/" + strconv.Itoa(id))
	if err != nil {
		return nil, err
	}
	return arr.DecodeSeries(b)
}

func (a Arr) Movie(_ context.Context, id int) (*domain.MovieDetail, error) {
	b, err := a.body("movie/" + strconv.Itoa(id))
	if err != nil {
		return nil, err
	}
	credits, _ := a.body("credit?movieId=" + strconv.Itoa(id))
	return arr.DecodeMovie(b, credits)
}

// Media answers from a scenario's "plex" and "plex_seasons" maps.
type Media struct{ Sc *Scenario }

func (m Media) Name() string { return m.Sc.PlexServer.Name }

func (m Media) Find(_ context.Context, guids []string, _ string, _ domain.Kind, _ bool) (*domain.MediaItem, error) {
	for _, g := range guids {
		if it, ok := m.Sc.Plex[g]; ok && g != "" {
			return it.MediaItem(), nil
		}
	}
	return nil, nil
}

func (m Media) SeasonKey(_ context.Context, show *domain.MediaItem, season int) (string, error) {
	return m.Sc.PlexSeasons[show.RatingKey+":"+strconv.Itoa(season)], nil
}

// URL matches the Plex adapter's deep link format.
func (m Media) URL(ratingKey string) string {
	return "https://app.plex.tv/desktop/#!/server/" + m.Sc.PlexServer.ID + "/details?key=" +
		url.QueryEscape("/library/metadata/"+ratingKey)
}

func (m Media) Scan(context.Context, string) error { return nil }

// RottenTomatoes answers like the enrich package: the exact page when known, else a search link.
func RottenTomatoes(sc *Scenario) func(imdbID, title string) string {
	return func(imdbID, title string) string {
		if p, ok := sc.RT[imdbID]; ok && imdbID != "" {
			return "https://www.rottentomatoes.com/" + p
		}
		return "https://www.rottentomatoes.com/search?" + url.Values{"search": {title}}.Encode()
	}
}

// MemStore is an in-memory domain.Store.
type MemStore struct {
	mu      sync.Mutex
	batches map[string]*domain.Batch
	posted  map[domain.ItemKey]time.Time
	History []domain.HistoryEntry
	cache   map[string]cached
}

type cached struct {
	val []byte
	at  time.Time
}

func NewMemStore() *MemStore {
	return &MemStore{batches: map[string]*domain.Batch{}, posted: map[domain.ItemKey]time.Time{}, cache: map[string]cached{}}
}

func clone(b *domain.Batch) *domain.Batch {
	c := *b
	if b.Series != nil {
		s := *b.Series
		c.Series = &s
	}
	c.Episodes = make(map[domain.ItemKey]domain.Episode, len(b.Episodes))
	for k, v := range b.Episodes {
		c.Episodes[k] = v
	}
	c.Movies = make(map[domain.ItemKey]domain.Movie, len(b.Movies))
	for k, v := range b.Movies {
		c.Movies[k] = v
	}
	return &c
}

func (s *MemStore) LoadBatches(context.Context) (map[string]*domain.Batch, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]*domain.Batch, len(s.batches))
	for k, b := range s.batches {
		out[k] = clone(b)
	}
	return out, nil
}

func (s *MemStore) SaveBatch(_ context.Context, b *domain.Batch) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.batches[b.Key] = clone(b)
	return nil
}

func (s *MemStore) DeleteBatch(_ context.Context, key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.batches, key)
	return nil
}

func (s *MemStore) PostedSince(_ context.Context, keys []domain.ItemKey, since time.Time) (map[domain.ItemKey]bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[domain.ItemKey]bool{}
	for _, k := range keys {
		if at, ok := s.posted[k]; ok && !at.Before(since) {
			out[k] = true
		}
	}
	return out, nil
}

func (s *MemStore) MarkPosted(_ context.Context, keys []domain.ItemKey, at time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, k := range keys {
		s.posted[k] = at
	}
	return nil
}

func (s *MemStore) PrunePosted(_ context.Context, before time.Time) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for k, at := range s.posted {
		if at.Before(before) {
			delete(s.posted, k)
			n++
		}
	}
	return n, nil
}

func (s *MemStore) AppendHistory(_ context.Context, e domain.HistoryEntry) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.History = append(s.History, e)
	return nil
}

func (s *MemStore) CacheGet(_ context.Context, ns, key string) ([]byte, time.Time, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.cache[ns+"\x00"+key]
	return c.val, c.at, ok, nil
}

func (s *MemStore) CachePut(_ context.Context, ns, key string, val []byte, at time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cache[ns+"\x00"+key] = cached{val: val, at: at}
	return nil
}

func (s *MemStore) Close() error { return nil }

// Compile-time checks.
var (
	_ domain.Clock       = (*Clock)(nil)
	_ domain.ArrClient   = Arr{}
	_ domain.MediaServer = Media{}
	_ domain.Store       = (*MemStore)(nil)
)

// SourceOf returns the source name of an item key ("radarr4k:movie:3" -> "radarr4k").
func SourceOf(k domain.ItemKey) string {
	s, _, _ := strings.Cut(string(k), ":")
	return s
}
