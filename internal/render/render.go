// Package render turns batches into cards and cards into Discord payloads. Pure: no I/O, no clock;
// everything comes in through the inputs. Reference: render_tv, render_movie, render_digest, to_v2
// and to_embed in the reference implementation (see testdata/harness).
package render

import (
	"time"

	"github.com/wtwerner/heraldarr/internal/domain"
)

// implemented gates the parity tests until Wave 1 lands this package (issue: render).
const implemented = false

// Common is what every card needs.
type Common struct {
	ServerName string // media server name for the footer; "" when there is none
	Style      domain.Style
	Now        time.Time
}

// TVInput is one series batch plus everything looked up for it at send time.
type TVInput struct {
	Common
	Batch  *domain.Batch
	Detail *domain.SeriesDetail // nil: the series was deleted since the import
	Show   *domain.MediaItem    // nil: not in the media server
	// ShowURL links the show; SeasonURL links the season and is set only when the batch holds a
	// single season. Both "" without a media server.
	ShowURL        string
	SeasonURL      string
	RottenTomatoes string // exact page or search link
}

// MovieInput is one movie plus everything looked up for it at send time.
type MovieInput struct {
	Key            domain.ItemKey
	Movie          domain.Movie
	Detail         *domain.MovieDetail // nil: deleted or unreachable
	URL            string              // media server deep link; "" if not found
	RottenTomatoes string
}

// TV renders one card for a series batch: "New series", "New season(s)" or "N new episodes".
func TV(in TVInput) domain.Card {
	panic("not implemented")
}

// Movie renders one card for a single movie.
func Movie(in MovieInput, c Common) domain.Card {
	panic("not implemented")
}

// Digest renders one card for many movies (the batch has at least digest_from).
func Digest(movies []MovieInput, c Common) domain.Card {
	panic("not implemented")
}

// Layouts encodes a card for Discord, in the order to try them: "v2" (Components V2), "embed"
// (classic embed + link buttons) and "embed, inline links". now stamps the embeds.
func Layouts(card domain.Card, now time.Time) []domain.Layout {
	panic("not implemented")
}

// Implemented reports whether the package is past its Wave 0 stub (used to skip parity tests).
func Implemented() bool { return implemented }
