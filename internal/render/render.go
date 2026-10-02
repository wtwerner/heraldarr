// Package render turns batches into cards and cards into Discord payloads. Pure: no I/O, no clock;
// everything comes in through the inputs. Reference: render_tv, render_movie, render_digest, to_v2
// and to_embed in the reference implementation (see testdata/harness).
package render

import (
	"cmp"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/wtwerner/heraldarr/internal/domain"
)

// implemented gates the parity tests until Wave 1 lands this package (issue: render).
const implemented = true

// Accent colors by card type; Style.Color overrides them.
const (
	colorSeries   = 0xE5A00D // new series, new movie, digest
	colorSeason   = 0x1ABC9C
	colorEpisodes = 0x5865F2
)

const (
	maxTVLines     = 12
	maxDigestLines = 15
	maxGallery     = 10
	maxTitle       = 200
	maxOverview    = 240
	recentlyAired  = 14 * 24 * time.Hour // a single episode this fresh shows "aired <t:…:R>"
)

// Common is what every card needs.
type Common struct {
	ServerName string // media server name for the footer; "" when there is none
	Style      domain.Style
	Now        time.Time
}

func (c Common) color(def int) int {
	if c.Style.Color != 0 {
		return c.Style.Color
	}
	return def
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
	Scores         domain.Scores // the show's, from the media server
	RottenTomatoes string        // exact page or search link
	// Downloading is how many more episodes of a back-catalog run the *arr has queued: the card
	// says they're on the way (it is edited as they land).
	Downloading int
}

// MovieInput is one movie plus everything looked up for it at send time.
type MovieInput struct {
	Key            domain.ItemKey
	Movie          domain.Movie
	Detail         *domain.MovieDetail // nil: deleted or unreachable
	URL            string              // media server deep link; "" if not found
	Scores         domain.Scores       // from the media server
	RottenTomatoes string
}

// TV renders one card for a series batch: "New series", "New season(s)" or "N new episodes".
func TV(in TVInput) domain.Card {
	s, d := in.Batch.Series, in.Detail
	eps := slices.Collect(maps.Values(in.Batch.Episodes))
	slices.SortFunc(eps, func(a, b domain.Episode) int {
		return cmp.Or(cmp.Compare(a.Season, b.Season), cmp.Compare(a.Number, b.Number))
	})
	bySeason := map[int][]domain.Episode{}
	var seasonNums, regular []int // ascending
	for _, e := range eps {
		if _, ok := bySeason[e.Season]; !ok {
			seasonNums = append(seasonNums, e.Season)
			if e.Season > 0 {
				regular = append(regular, e.Season)
			}
		}
		bySeason[e.Season] = append(bySeason[e.Season], e)
	}
	stats := func(n int) (domain.SeasonStats, bool) {
		if d == nil {
			return domain.SeasonStats{}, false
		}
		st, ok := d.Seasons[n]
		return st, ok
	}

	// What kind of news is this? Counts are taken after the import, so "had nothing before" means
	// every file the *arr has for the series (or season) arrived in this batch.
	newSeries := d != nil && d.EpisodeFileCount != nil && *d.EpisodeFileCount <= len(eps)
	newSeasons := 0
	for _, n := range regular {
		if st, ok := stats(n); ok && st.EpisodeFileCount <= len(bySeason[n]) {
			newSeasons++
		}
	}
	const (
		kindSeries = iota
		kindSeason
		kindEpisodes
	)
	// Style.Label is not applied: the reference never labels TV headlines (see the divergence issue).
	var kind, color int
	var headline string
	switch {
	case newSeries:
		kind, color, headline = kindSeries, colorSeries, "New series"
	case newSeasons > 1:
		kind, color, headline = kindSeason, colorSeason, "New seasons"
	case newSeasons == 1:
		kind, color, headline = kindSeason, colorSeason, "New season"
	case len(eps) == 1:
		kind, color, headline = kindEpisodes, colorEpisodes, "New episode"
	default:
		kind, color, headline = kindEpisodes, colorEpisodes, fmt.Sprintf("%d new episodes", len(eps))
	}

	var lines []string
	for _, n := range seasonNums {
		seasonEps := bySeason[n]
		st, _ := stats(n)
		total := st.TotalEpisodeCount
		complete := total > 0 && st.EpisodeFileCount >= total
		switch {
		case complete && len(seasonEps) >= total:
			lines = append(lines, fmt.Sprintf("**%s** · all %s", seasonName(n), plural(total, "episode")))
		case len(seasonEps) <= 3:
			for _, e := range seasonEps {
				title := e.Title
				if title == "" {
					title = "TBA"
				}
				line := fmt.Sprintf("`S%02dE%02d` · %s", n, e.Number, title)
				if age := in.Now.Sub(e.Aired); len(eps) == 1 && !e.Aired.IsZero() && e.Aired.Unix() != 0 &&
					age > 0 && age < recentlyAired {
					line += fmt.Sprintf(" · aired <t:%d:R>", e.Aired.Unix())
				}
				lines = append(lines, line)
			}
		default:
			nums := make([]int, len(seasonEps))
			for i, e := range seasonEps {
				nums[i] = e.Number
			}
			line := fmt.Sprintf("**%s** · %s (%d)", seasonName(n), epRanges(nums), len(nums))
			if complete {
				line += " · season complete"
			}
			lines = append(lines, line)
		}
	}
	// Several whole, consecutive seasons (and nothing else) read better as one line.
	var whole []int
	for _, n := range regular {
		if st, _ := stats(n); st.TotalEpisodeCount > 0 && len(bySeason[n]) >= st.TotalEpisodeCount {
			whole = append(whole, n)
		}
	}
	if _, specials := bySeason[0]; len(whole) > 1 && slices.Equal(whole, regular) &&
		whole[len(whole)-1]-whole[0] == len(whole)-1 && !specials {
		lines = []string{fmt.Sprintf("**Seasons %d–%d** · all %s", whole[0], whole[len(whole)-1], plural(len(eps), "episode"))}
	}
	if in.Downloading > 0 {
		lines = capLines(lines, maxTVLines-1)
		lines = append(lines, fmt.Sprintf("_%d more on the way_", in.Downloading))
	} else {
		lines = capLines(lines, maxTVLines)
	}

	mediaURL := ""
	if in.Show != nil {
		mediaURL = in.ShowURL
		if kind != kindSeries && len(seasonNums) == 1 && in.SeasonURL != "" {
			mediaURL = in.SeasonURL
		}
	}

	var facts []domain.Fact
	if kind != kindEpisodes {
		if d != nil && d.Network != "" {
			facts = append(facts, domain.Fact{Name: "Network", Value: d.Network})
		}
		var arrScores domain.Scores
		if d != nil {
			arrScores.IMDb = d.Rating
		}
		rating := strings.Join(scores(arrScores, in.Scores), " · ")
		cert := ""
		if d != nil {
			cert = d.Certification
		}
		if cert == "" && in.Show != nil {
			cert = in.Show.ContentRating
		}
		if r := joinNonEmpty(" · ", rating, cert); r != "" {
			facts = append(facts, domain.Fact{Name: "Rating", Value: r})
		}
		if len(s.Genres) > 0 {
			facts = append(facts, domain.Fact{Name: "Genres", Value: strings.Join(first(s.Genres, 3), ", ")})
		}
	}
	if d != nil {
		switch {
		case d.Status == "continuing" && !d.NextAiring.IsZero():
			facts = append(facts, domain.Fact{Name: "Next episode", Value: fmt.Sprintf("Next episode <t:%d:R>", d.NextAiring.Unix())})
		case d.Status == "ended" && kind == kindSeries:
			ended := "Ended"
			if !d.LastAired.IsZero() {
				ended += " " + strconv.Itoa(d.LastAired.Year())
			}
			facts = append(facts, domain.Fact{Name: "Status", Value: ended})
		case d.Status == "upcoming" && kind == kindSeries:
			facts = append(facts, domain.Fact{Name: "Status", Value: "Upcoming"})
		}
	}

	card := domain.Card{
		Headline: headline,
		Title:    truncate(titleYear(s.Title, s.Year), maxTitle),
		Color:    in.color(color),
		URL:      cmp.Or(mediaURL, imdbURL(s.IMDbID), "https://www.thetvdb.com/dereferrer/series/"+strconv.Itoa(s.TVDBID)),
		Lines:    lines,
		Facts:    facts,
		Poster:   s.Poster,
		Footer:   joinNonEmpty(" · ", in.ServerName, usualQuality(eps)),
		Buttons:  linkButtons(mediaURL, "", s.IMDbID, in.RottenTomatoes),
	}
	if kind == kindSeries {
		if d != nil {
			card.Overview = trim(d.Overview, maxOverview)
		}
		if s.Fanart != "" {
			card.Gallery = []string{s.Fanart}
		}
	}
	return card
}

// Movie renders one card for a single movie.
func Movie(in MovieInput, c Common) domain.Card {
	m, d := in.Movie, in.Detail
	if d == nil {
		d = &domain.MovieDetail{}
	}
	facts := movieScores(in)
	for _, x := range []string{d.Certification, runtime(d.RuntimeMin), strings.Join(first(m.Genres, 3), ", ")} {
		if x != "" {
			facts = append(facts, x)
		}
	}
	var lines, credits []string
	if len(facts) > 0 {
		lines = append(lines, strings.Join(facts, " · "))
	}
	if len(d.Directors) > 0 {
		credits = append(credits, "Directed by "+strings.Join(first(d.Directors, 2), " & "))
	}
	if len(d.Cast) > 0 {
		credits = append(credits, "Starring "+strings.Join(first(d.Cast, 3), ", "))
	}
	if len(credits) > 0 {
		lines = append(lines, strings.Join(credits, " · "))
	}
	if d.Collection != "" {
		lines = append(lines, strings.ReplaceAll("Part of the "+d.Collection, "the The ", "the "))
	}

	footer := []string{c.ServerName}
	if c.Style.TechDetails {
		footer = append(footer, mediaLabel(m)...)
	} else {
		footer = append(footer, qualityLabel(m.Quality))
	}
	headline := "New movie"
	if c.Style.Label != "" {
		headline = "New in " + c.Style.Label
	}
	trailer := ""
	if d.TrailerID != "" {
		trailer = "https://www.youtube.com/watch?v=" + d.TrailerID
	}
	card := domain.Card{
		Headline: headline,
		Title:    truncate(titleYear(m.Title, m.Year), maxTitle),
		Color:    c.color(colorSeries),
		URL:      cmp.Or(in.URL, imdbURL(m.IMDbID), "https://www.themoviedb.org/movie/"+strconv.Itoa(m.TMDBID)),
		Overview: trim(cmp.Or(m.Overview, d.Overview), maxOverview),
		Lines:    lines,
		Poster:   m.Poster,
		Footer:   joinNonEmpty(" · ", footer...),
		Buttons:  linkButtons(in.URL, trailer, m.IMDbID, in.RottenTomatoes),
	}
	if m.Fanart != "" {
		card.Gallery = []string{m.Fanart}
	}
	return card
}

// Digest renders one card for many movies (the batch has at least digest_from).
func Digest(movies []MovieInput, c Common) domain.Card {
	movies = slices.Clone(movies)
	slices.SortStableFunc(movies, func(a, b MovieInput) int {
		return strings.Compare(strings.ToLower(a.Movie.Title), strings.ToLower(b.Movie.Title))
	})
	var lines, gallery []string
	for _, in := range movies {
		m := in.Movie
		name := titleYear(m.Title, m.Year)
		line := "**" + name + "**"
		if url := cmp.Or(in.URL, imdbURL(m.IMDbID)); url != "" {
			line = fmt.Sprintf("**[%s](%s)**", name, url)
		}
		d := in.Detail
		if d == nil {
			d = &domain.MovieDetail{}
		}
		bits := movieScores(in)
		if r := runtime(d.RuntimeMin); r != "" {
			bits = append(bits, r)
		}
		if len(bits) > 0 {
			line += " · " + strings.Join(bits, " · ")
		}
		lines = append(lines, line)
		// Gallery tiles are landscape: fanart fits, posters get their tops and bottoms cropped.
		if img := cmp.Or(m.Fanart, m.Poster); img != "" {
			gallery = append(gallery, img)
		}
	}
	title := fmt.Sprintf("%d new movies", len(movies))
	if c.Style.Label != "" {
		title = fmt.Sprintf("%d new %s movies", len(movies), c.Style.Label)
	}
	return domain.Card{
		Headline: title,
		Title:    title,
		Color:    c.color(colorSeries),
		Lines:    capLines(lines, maxDigestLines),
		Gallery:  first(gallery, maxGallery),
		Footer:   c.ServerName,
	}
}

// Implemented reports whether the package is past its Wave 0 stub (used to skip parity tests).
func Implemented() bool { return implemented }
