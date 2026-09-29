// Package testkit loads the parity scenarios (testdata/scenarios) and the reference output
// (testdata/expected), and provides in-memory fakes of the domain interfaces. Test-only.
package testkit

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/wtwerner/heraldarr/internal/domain"
)

// Source is how the reference implementation's three sources are configured.
type Source struct {
	Kind  domain.Kind
	Style domain.Style
}

// Sources mirrors the reference SOURCES table: radarr4k is the private 4K route.
var Sources = map[string]Source{
	"sonarr":   {Kind: domain.KindTV},
	"radarr":   {Kind: domain.KindMovie},
	"radarr4k": {Kind: domain.KindMovie, Style: domain.Style{Label: "4K", Color: 0x9B59B6, TechDetails: true}},
}

type Event struct {
	Source  string          `json:"source"`
	Payload json.RawMessage `json:"payload"`
}

type PlexItem struct {
	RatingKey           string  `json:"ratingKey"`
	AudienceRating      float64 `json:"audienceRating"`
	AudienceRatingImage string  `json:"audienceRatingImage"`
	ContentRating       string  `json:"contentRating"`
}

func (p PlexItem) MediaItem() *domain.MediaItem {
	src, _, _ := strings.Cut(p.AudienceRatingImage, ":")
	return &domain.MediaItem{
		RatingKey: p.RatingKey, AudienceRating: p.AudienceRating, RatingSource: src,
		ContentRating: p.ContentRating,
	}
}

type Scenario struct {
	Name        string
	Description string                     `json:"description"`
	Now         time.Time                  `json:"now"`
	PlexServer  struct{ ID, Name string }  `json:"plex_server"`
	Posted      []domain.ItemKey           `json:"posted"`
	Events      []Event                    `json:"events"`
	Arr         map[string]json.RawMessage `json:"arr"`          // "series/101", "movie/5", "credit?movieId=5"; null = 404
	Plex        map[string]PlexItem        `json:"plex"`         // GUID -> item
	PlexSeasons map[string]string          `json:"plex_seasons"` // "<ratingKey>:<season>" -> season ratingKey
	RT          map[string]string          `json:"rt"`           // IMDb ID -> RT path
}

// Post is one Discord message the reference implementation sent.
type Post struct {
	Source      string          `json:"source"`
	Card        json.RawMessage `json:"card"`
	V2          json.RawMessage `json:"v2"`
	Embed       json.RawMessage `json:"embed"`
	EmbedInline json.RawMessage `json:"embed_inline"`
}

type Expected struct {
	AddResults []string                   `json:"add_results"`
	Pending    map[string]json.RawMessage `json:"pending"`
	Posts      []Post                     `json:"posts"`
}

// Root is the repository root.
func Root() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(file), "..", "..")
}

// Names lists every scenario.
func Names(t testing.TB) []string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(Root(), "testdata", "scenarios", "*.json"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no scenarios found: %v", err)
	}
	names := make([]string, len(files))
	for i, f := range files {
		names[i] = strings.TrimSuffix(filepath.Base(f), ".json")
	}
	return names
}

// Load returns a scenario and what the reference implementation did with it.
func Load(t testing.TB, name string) (*Scenario, *Expected) {
	t.Helper()
	sc := &Scenario{Name: name}
	readJSON(t, filepath.Join(Root(), "testdata", "scenarios", name+".json"), sc)
	exp := &Expected{}
	readJSON(t, filepath.Join(Root(), "testdata", "expected", name+".json"), exp)
	return sc, exp
}

func readJSON(t testing.TB, path string, v any) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, v); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
}

// BatchSummary is the part of a pending batch that parity is judged on.
type BatchSummary struct {
	Key       string
	Source    string
	Items     []string
	Following bool
}

// ExpectedBatches summarizes the reference implementation's pending batches, sorted by key.
func (e *Expected) ExpectedBatches(t testing.TB) []BatchSummary {
	t.Helper()
	var out []BatchSummary
	for key, raw := range e.Pending {
		var b struct {
			Source string                     `json:"source"`
			Items  map[string]json.RawMessage `json:"items"`
			Fast   bool                       `json:"fast"`
		}
		if err := json.Unmarshal(raw, &b); err != nil {
			t.Fatal(err)
		}
		s := BatchSummary{Key: key, Source: b.Source, Following: b.Fast}
		for k := range b.Items {
			s.Items = append(s.Items, k)
		}
		sort.Strings(s.Items)
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

// Summarize makes a BatchSummary from a domain batch.
func Summarize(b *domain.Batch) BatchSummary {
	s := BatchSummary{Key: b.Key, Source: b.Source, Following: b.Following}
	for _, k := range b.Keys() {
		s.Items = append(s.Items, string(k))
	}
	return s
}

// ReferenceCard converts a card to the reference implementation's dict shape (facts as
// [name, value] pairs, buttons as [label, url, emoji], null for a missing url or poster) so it can
// be compared with Post.Card.
func ReferenceCard(c domain.Card) any {
	facts := make([][2]string, len(c.Facts))
	for i, f := range c.Facts {
		facts[i] = [2]string{f.Name, f.Value}
	}
	buttons := make([][3]string, len(c.Buttons))
	for i, b := range c.Buttons {
		buttons[i] = [3]string{b.Label, b.URL, b.Emoji}
	}
	orNil := func(s string) any {
		if s == "" {
			return nil
		}
		return s
	}
	orEmpty := func(s []string) []string {
		if s == nil {
			return []string{}
		}
		return s
	}
	return map[string]any{
		"headline": c.Headline, "title": c.Title, "color": c.Color, "url": orNil(c.URL), "overview": c.Overview,
		"lines": orEmpty(c.Lines), "facts": facts, "poster": orNil(c.Poster), "gallery": orEmpty(c.Gallery),
		"footer": c.Footer, "buttons": buttons,
	}
}

// EqualJSON fails the test unless got, marshaled, is semantically equal to want.
func EqualJSON(t testing.TB, what string, got any, want json.RawMessage) {
	t.Helper()
	g, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("%s: marshal: %v", what, err)
	}
	var gv, wv any
	if err := json.Unmarshal(g, &gv); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(want, &wv); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(gv, wv) {
		gi, _ := json.MarshalIndent(gv, "", "  ")
		wi, _ := json.MarshalIndent(wv, "", "  ")
		t.Errorf("%s differs from the reference\n--- got\n%s\n--- want\n%s", what, gi, wi)
	}
}
