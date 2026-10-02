// Package domain holds the types every other package shares. No I/O lives here. The glossary is
// CONTEXT.md; the reference behavior is testdata/expected (see testdata/harness).
package domain

import (
	"fmt"
	"slices"
	"time"
)

// Kind is what a source produces.
type Kind string

const (
	KindTV    Kind = "tv"
	KindMovie Kind = "movie"
)

// ItemKey identifies one announceable unit across restarts and sources:
// "<source>:ep:<episodeID>" or "<source>:movie:<movieID>".
type ItemKey string

func EpisodeKey(source string, episodeID int) ItemKey {
	return ItemKey(fmt.Sprintf("%s:ep:%d", source, episodeID))
}

func MovieKey(source string, movieID int) ItemKey {
	return ItemKey(fmt.Sprintf("%s:movie:%d", source, movieID))
}

// Series is the show an episode import belongs to, as the webhook describes it.
type Series struct {
	ID     int      `json:"id"`
	Title  string   `json:"title"`
	Year   int      `json:"year,omitempty"`
	Path   string   `json:"path"`
	TVDBID int      `json:"tvdbId"`
	IMDbID string   `json:"imdbId,omitempty"`
	Genres []string `json:"genres,omitempty"`
	Poster string   `json:"poster,omitempty"`
	Fanart string   `json:"fanart,omitempty"`
}

// Episode is one imported episode. Quality is the *arr quality name ("WEBDL-1080p").
type Episode struct {
	ID      int       `json:"id"`
	Season  int       `json:"season"`
	Number  int       `json:"number"`
	Title   string    `json:"title,omitempty"`
	Aired   time.Time `json:"aired,omitzero"` // zero: unknown
	Quality string    `json:"quality,omitempty"`
}

// Movie is one imported movie with the file facts the 4K footer shows.
type Movie struct {
	ID            int      `json:"id"`
	Title         string   `json:"title"`
	Year          int      `json:"year,omitempty"`
	Path          string   `json:"path"`
	TMDBID        int      `json:"tmdbId"`
	IMDbID        string   `json:"imdbId,omitempty"`
	Genres        []string `json:"genres,omitempty"`
	Overview      string   `json:"overview,omitempty"`
	Poster        string   `json:"poster,omitempty"`
	Fanart        string   `json:"fanart,omitempty"`
	Quality       string   `json:"quality,omitempty"`
	Size          int64    `json:"size,omitempty"`
	ReleaseGroup  string   `json:"group,omitempty"`
	HDR           string   `json:"hdr,omitempty"` // videoDynamicRangeType, e.g. "DV HDR10Plus"
	AudioCodec    string   `json:"audio,omitempty"`
	AudioChannels float64  `json:"channels,omitempty"`
}

// Import is one normalized "On Import" webhook event. Exactly one of Series or Movie is set.
type Import struct {
	Source   string
	Upgrade  bool // isUpgrade, or the file replaced another: never announced
	Series   *Series
	Episodes []Episode
	Movie    *Movie
}

// Batch is what waits for its quiet window: every episode of one series from one source, or every
// movie from one source.
type Batch struct {
	Key       string              `json:"key"` // "<source>:<seriesID>" or "<source>:movies"
	Source    string              `json:"source"`
	Kind      Kind                `json:"kind"`
	Series    *Series             `json:"series,omitempty"`
	Episodes  map[ItemKey]Episode `json:"episodes,omitempty"`
	Movies    map[ItemKey]Movie   `json:"movies,omitempty"`
	First     time.Time           `json:"first"`
	Last      time.Time           `json:"last"`
	Following bool                `json:"following"` // new episodes people follow: short quiet window
	// Backlog: episodes that aired long ago, grouped into a run (backlog modes lead and complete).
	// Season is the one season the batch holds when runs are per season; nil: the whole series.
	Backlog bool `json:"backlog,omitempty"`
	Season  *int `json:"season,omitempty"`
	// Run is what the batch already announced; set after its first card, kept while the run lasts.
	Run *Run `json:"run,omitempty"`
	// Delivery state, owned by the batcher.
	MediaChecks int       `json:"mediaChecks"` // times the media server was asked and didn't have it yet
	Tries       int       `json:"tries"`       // failed sends
	NotBefore   time.Time `json:"notBefore,omitzero"`
}

// Keys returns the batch's item keys in a stable order.
func (b *Batch) Keys() []ItemKey {
	keys := make([]ItemKey, 0, len(b.Episodes)+len(b.Movies))
	for k := range b.Episodes {
		keys = append(keys, k)
	}
	for k := range b.Movies {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

// Len is the number of items waiting.
func (b *Batch) Len() int { return len(b.Episodes) + len(b.Movies) }

// Run is a back-catalog run: episodes of one series (or season) that arrive over hours or days.
// Its first card is posted once; later episodes are folded into that card, not posted again.
type Run struct {
	Episodes map[ItemKey]Episode `json:"episodes"` // announced so far
	// Message is the card to edit as more episodes land; zero when it can't be edited.
	Message Message `json:"message,omitzero"`
}

// Message is a posted card: where it went, its ID and the layout Discord accepted.
type Message struct {
	Destination string `json:"destination"`
	ID          string `json:"id"`
	Layout      string `json:"layout"`
}

// QueueItem is one episode the *arr has grabbed and is still expected to import.
type QueueItem struct {
	EpisodeID int
	Season    int
	Aired     time.Time // zero: unknown
}

// SeasonStats counts files after the import, so "had nothing before" means every file arrived in
// this batch.
type SeasonStats struct {
	EpisodeFileCount  int
	TotalEpisodeCount int
}

// SeriesDetail is what the *arr API adds to a series at send time.
type SeriesDetail struct {
	Network          string
	Overview         string
	Certification    string
	Status           string  // continuing | ended | upcoming
	Rating           float64 // IMDb's rating (Sonarr reports it); 0: none
	NextAiring       time.Time
	LastAired        time.Time
	EpisodeFileCount *int // nil: the API didn't report statistics
	Seasons          map[int]SeasonStats
}

// MovieDetail is what the *arr API adds to a movie at send time. Zero values mean "none".
type MovieDetail struct {
	IMDbRating    float64
	RTRating      float64 // percent
	Certification string
	RuntimeMin    int
	Collection    string
	TrailerID     string // YouTube
	Overview      string
	Directors     []string
	Cast          []string // billing order
}

// MediaItem is a library item on the media server.
type MediaItem struct {
	RatingKey     string
	ContentRating string
}

// Scores are a title's review scores. Zero values mean "none".
type Scores struct {
	IMDb       float64 // out of 10
	RTCritic   float64 // Rotten Tomatoes Tomatometer, percent
	RTAudience float64 // Rotten Tomatoes Popcornmeter, percent
}

// Style changes how a route's cards look. The zero value is the standard public card.
type Style struct {
	Label       string // e.g. "4K": "New in 4K", "4 new 4K movies"
	Color       int    // accent override; 0: by card type
	TechDetails bool   // footer shows quality, HDR, audio, size, release group; never on public routes
}

// Destination is a Discord webhook.
type Destination struct {
	Name       string
	WebhookURL string
	Username   string
	AvatarURL  string
	Public     bool
}

// Fact is one "name: value" line element; Discord V2 cards show only the value.
type Fact struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// Button is a link button.
type Button struct {
	Label string `json:"label"`
	URL   string `json:"url"`
	Emoji string `json:"emoji"`
}

// Card is the renderer's output, independent of Discord's wire format.
type Card struct {
	Headline string   `json:"headline"`
	Title    string   `json:"title"`
	Color    int      `json:"color"`
	URL      string   `json:"url,omitempty"`
	Overview string   `json:"overview,omitempty"`
	Lines    []string `json:"lines,omitempty"`
	Facts    []Fact   `json:"facts,omitempty"`
	Poster   string   `json:"poster,omitempty"`
	Gallery  []string `json:"gallery,omitempty"`
	Footer   string   `json:"footer,omitempty"`
	Buttons  []Button `json:"buttons,omitempty"`
}

// Layout is one encoding of a card for the notifier to try, in order (V2, then fallbacks).
type Layout struct {
	Name string
	Body map[string]any
}

// PostResult says which layout Discord accepted and the message it created.
type PostResult struct {
	Layout    string
	MessageID string
}

// HistoryEntry is one line of the post log.
type HistoryEntry struct {
	At          time.Time
	Source      string
	Destination string
	Title       string
	Headline    string
	Items       int
	Following   bool
	MessageID   string
}
