package domain

import (
	"context"
	"errors"
	"time"
)

// ErrNotFound: the *arr or media server doesn't have the item (deleted since the import).
var ErrNotFound = errors.New("not found")

// ErrRefused: the destination refused the message; sending the same again won't help.
var ErrRefused = errors.New("refused")

// Clock is injected everywhere time matters, so batching is testable.
type Clock interface {
	Now() time.Time
}

// ArrClient reads details from one Sonarr or Radarr instance.
type ArrClient interface {
	// Series returns ErrNotFound if the series was deleted.
	Series(ctx context.Context, id int) (*SeriesDetail, error)
	// Movie includes credits; a credits failure leaves Directors/Cast empty rather than failing.
	Movie(ctx context.Context, id int) (*MovieDetail, error)
	// Queue lists a series' episodes that are grabbed and still expected to import (Sonarr only):
	// failed or blocked downloads are left out.
	Queue(ctx context.Context, seriesID int) ([]QueueItem, error)
}

// MediaServer finds imported items in the library and links to them.
type MediaServer interface {
	// Name is the server's friendly name for card footers ("" until known).
	Name() string
	// Find looks up an item by any GUID ("tvdb://1", "tmdb://2", "imdb://tt3") in the library
	// whose folder holds path (so a 4K copy links to the 4K library). nil, nil: not there yet.
	// fresh bypasses caches.
	Find(ctx context.Context, guids []string, path string, kind Kind, fresh bool) (*MediaItem, error)
	// SeasonKey returns the rating key of a show's season, "" if unknown.
	SeasonKey(ctx context.Context, show *MediaItem, season int) (string, error)
	// URL is a deep link that works for every user the library is shared with (no token).
	URL(ratingKey string) string
	// Scan asks the server to scan the library folder holding path.
	Scan(ctx context.Context, path string) error
}

// Notifier delivers one card, trying each layout until one is accepted.
type Notifier interface {
	Post(ctx context.Context, dest Destination, layouts []Layout) (PostResult, error)
	// Edit replaces a posted message with layout (the one Discord accepted for it). An error
	// matching ErrRefused or ErrNotFound (the message was deleted) won't go away on a retry.
	Edit(ctx context.Context, dest Destination, messageID string, layout Layout) error
}

// Store persists batching and delivery state. Implementations must be safe for concurrent use.
type Store interface {
	LoadBatches(ctx context.Context) (map[string]*Batch, error)
	SaveBatch(ctx context.Context, b *Batch) error
	DeleteBatch(ctx context.Context, key string) error

	// PostedSince reports which keys were announced at or after since.
	PostedSince(ctx context.Context, keys []ItemKey, since time.Time) (map[ItemKey]bool, error)
	MarkPosted(ctx context.Context, keys []ItemKey, at time.Time) error
	PrunePosted(ctx context.Context, before time.Time) (int, error)

	AppendHistory(ctx context.Context, e HistoryEntry) error

	// Cache is a small namespaced key/value store (e.g. ns "rt": IMDb ID -> RT path).
	CacheGet(ctx context.Context, ns, key string) (val []byte, at time.Time, ok bool, err error)
	CachePut(ctx context.Context, ns, key string, val []byte, at time.Time) error

	Close() error
}
