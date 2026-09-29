// Package arr reads Sonarr and Radarr: webhook payloads (decode.go) and the v3 API (client.go).
package arr

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/wtwerner/heraldarr/internal/domain"
)

// EventDownload is the eventType of "On Import" (and Sonarr's "On Import Complete").
const EventDownload = "Download"

type image struct {
	CoverType string `json:"coverType"`
	RemoteURL string `json:"remoteUrl"`
}

func pickImage(images []image, cover string) string {
	for _, i := range images {
		if i.CoverType == cover && strings.HasPrefix(i.RemoteURL, "http") {
			return i.RemoteURL
		}
	}
	return ""
}

type episodeFile struct {
	Quality string `json:"quality"`
}

type webhook struct {
	EventType    string            `json:"eventType"`
	IsUpgrade    bool              `json:"isUpgrade"`
	DeletedFiles []json.RawMessage `json:"deletedFiles"`
	Series       *struct {
		ID     int      `json:"id"`
		Title  string   `json:"title"`
		Year   int      `json:"year"`
		Path   string   `json:"path"`
		TVDBID int      `json:"tvdbId"`
		IMDbID string   `json:"imdbId"`
		Genres []string `json:"genres"`
		Images []image  `json:"images"`
	} `json:"series"`
	Episodes []struct {
		ID            int    `json:"id"`
		SeasonNumber  int    `json:"seasonNumber"`
		EpisodeNumber int    `json:"episodeNumber"`
		Title         string `json:"title"`
		AirDateUTC    string `json:"airDateUtc"`
	} `json:"episodes"`
	EpisodeFile  *episodeFile  `json:"episodeFile"`
	EpisodeFiles []episodeFile `json:"episodeFiles"` // Sonarr v4 On Import Complete
	Movie        *struct {
		ID         int      `json:"id"`
		Title      string   `json:"title"`
		Year       int      `json:"year"`
		FolderPath string   `json:"folderPath"`
		Path       string   `json:"path"`
		TMDBID     int      `json:"tmdbId"`
		IMDbID     string   `json:"imdbId"`
		Genres     []string `json:"genres"`
		Overview   string   `json:"overview"`
		Images     []image  `json:"images"`
	} `json:"movie"`
	MovieFile *struct {
		Quality      string `json:"quality"`
		Size         int64  `json:"size"`
		ReleaseGroup string `json:"releaseGroup"`
		MediaInfo    *struct {
			VideoDynamicRangeType string  `json:"videoDynamicRangeType"`
			AudioCodec            string  `json:"audioCodec"`
			AudioChannels         float64 `json:"audioChannels"`
		} `json:"mediaInfo"`
	} `json:"movieFile"`
}

// ParseWebhook decodes a webhook body: Sonarr v3/v4 On Import and v4 On Import Complete, Radarr
// v4/v5 On Import. ok is false for events other than Download (Test, Grab, Rename…), which
// callers acknowledge and ignore whatever the rest of their body holds.
//
// On Import Complete carries no isUpgrade or deletedFiles, so its upgrades can't be told apart.
func ParseWebhook(source string, kind domain.Kind, body []byte) (imp domain.Import, ok bool, err error) {
	var event struct {
		EventType string `json:"eventType"`
	}
	if err := json.Unmarshal(body, &event); err != nil {
		return imp, false, fmt.Errorf("bad json: %w", err)
	}
	if event.EventType != EventDownload {
		return imp, false, nil
	}
	var w webhook
	if err := json.Unmarshal(body, &w); err != nil {
		return imp, false, fmt.Errorf("%s: unexpected Download payload: %w", source, err)
	}
	imp = domain.Import{Source: source, Upgrade: w.IsUpgrade || len(w.DeletedFiles) > 0}
	switch kind {
	case domain.KindTV:
		if w.Series == nil {
			return imp, false, fmt.Errorf("%s: Download event without series", source)
		}
		s := w.Series
		imp.Series = &domain.Series{
			ID: s.ID, Title: s.Title, Year: s.Year, Path: s.Path, TVDBID: s.TVDBID,
			IMDbID: s.IMDbID, Genres: s.Genres, Poster: pickImage(s.Images, "poster"), Fanart: pickImage(s.Images, "fanart"),
		}
		quality := usualQuality(w.EpisodeFiles)
		if w.EpisodeFile != nil {
			quality = w.EpisodeFile.Quality
		}
		for _, e := range w.Episodes {
			imp.Episodes = append(imp.Episodes, domain.Episode{
				ID: e.ID, Season: e.SeasonNumber, Number: e.EpisodeNumber,
				Title: e.Title, Aired: parseTime(e.AirDateUTC), Quality: quality,
			})
		}
	case domain.KindMovie:
		if w.Movie == nil {
			return imp, false, fmt.Errorf("%s: Download event without movie", source)
		}
		m := w.Movie
		path := m.FolderPath
		if path == "" {
			path = m.Path
		}
		mv := &domain.Movie{
			ID: m.ID, Title: m.Title, Year: m.Year, Path: path, TMDBID: m.TMDBID, IMDbID: m.IMDbID,
			Genres: m.Genres, Overview: m.Overview, Poster: pickImage(m.Images, "poster"), Fanart: pickImage(m.Images, "fanart"),
		}
		if f := w.MovieFile; f != nil {
			mv.Quality, mv.Size, mv.ReleaseGroup = f.Quality, f.Size, f.ReleaseGroup
			if mi := f.MediaInfo; mi != nil {
				mv.HDR, mv.AudioCodec, mv.AudioChannels = mi.VideoDynamicRangeType, mi.AudioCodec, mi.AudioChannels
			}
		}
		imp.Movie = mv
	default:
		return imp, false, fmt.Errorf("unknown kind %q", kind)
	}
	return imp, true, nil
}

// usualQuality is the most common quality of an On Import Complete's files (the first on a tie).
// The payload doesn't say which file holds which episode, and the card shows only the usual one.
func usualQuality(files []episodeFile) string {
	counts := map[string]int{}
	best := ""
	for _, f := range files {
		if f.Quality == "" {
			continue
		}
		counts[f.Quality]++
		if counts[f.Quality] > counts[best] {
			best = f.Quality
		}
	}
	return best
}

type seriesResource struct {
	Network       string `json:"network"`
	Overview      string `json:"overview"`
	Certification string `json:"certification"`
	Status        string `json:"status"`
	NextAiring    string `json:"nextAiring"`
	LastAired     string `json:"lastAired"`
	Ratings       struct {
		Value float64 `json:"value"`
	} `json:"ratings"`
	Statistics *struct {
		EpisodeFileCount *int `json:"episodeFileCount"`
	} `json:"statistics"`
	Seasons []struct {
		SeasonNumber int `json:"seasonNumber"`
		Statistics   struct {
			EpisodeFileCount  int `json:"episodeFileCount"`
			TotalEpisodeCount int `json:"totalEpisodeCount"`
		} `json:"statistics"`
	} `json:"seasons"`
}

// DecodeSeries decodes GET /api/v3/series/{id}.
func DecodeSeries(body []byte) (*domain.SeriesDetail, error) {
	var r seriesResource
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, err
	}
	d := &domain.SeriesDetail{
		Network: r.Network, Overview: r.Overview, Certification: r.Certification,
		Status: r.Status, Rating: r.Ratings.Value, NextAiring: parseTime(r.NextAiring), LastAired: parseTime(r.LastAired),
		Seasons: map[int]domain.SeasonStats{},
	}
	if r.Statistics != nil {
		d.EpisodeFileCount = r.Statistics.EpisodeFileCount
	}
	for _, s := range r.Seasons {
		d.Seasons[s.SeasonNumber] = domain.SeasonStats{
			EpisodeFileCount:  s.Statistics.EpisodeFileCount,
			TotalEpisodeCount: s.Statistics.TotalEpisodeCount,
		}
	}
	return d, nil
}

type movieResource struct {
	Overview         string `json:"overview"`
	Certification    string `json:"certification"`
	Runtime          int    `json:"runtime"`
	YouTubeTrailerID string `json:"youTubeTrailerId"`
	Collection       *struct {
		Title string `json:"title"`
	} `json:"collection"`
	Ratings struct {
		IMDb *struct {
			Value float64 `json:"value"`
		} `json:"imdb"`
		RottenTomatoes *struct {
			Value float64 `json:"value"`
		} `json:"rottenTomatoes"`
	} `json:"ratings"`
}

type credit struct {
	PersonName string `json:"personName"`
	Type       string `json:"type"` // cast | crew
	Job        string `json:"job"`
	Order      *int   `json:"order"`
}

// DecodeMovie decodes GET /api/v3/movie/{id} and, if non-nil, GET /api/v3/credit?movieId={id}.
func DecodeMovie(movieBody, creditsBody []byte) (*domain.MovieDetail, error) {
	var r movieResource
	if err := json.Unmarshal(movieBody, &r); err != nil {
		return nil, err
	}
	d := &domain.MovieDetail{
		Overview: r.Overview, Certification: r.Certification, RuntimeMin: r.Runtime,
		TrailerID: r.YouTubeTrailerID,
	}
	if r.Collection != nil {
		d.Collection = r.Collection.Title
	}
	if r.Ratings.IMDb != nil {
		d.IMDbRating = r.Ratings.IMDb.Value
	}
	if r.Ratings.RottenTomatoes != nil {
		d.RTRating = r.Ratings.RottenTomatoes.Value
	}
	if creditsBody != nil {
		var people []credit
		if err := json.Unmarshal(creditsBody, &people); err != nil {
			return d, nil //nolint:nilerr // credits are decoration: a bad body leaves them empty
		}
		var cast []credit
		for _, p := range people {
			switch {
			case p.Type == "crew" && p.Job == "Director":
				d.Directors = append(d.Directors, p.PersonName)
			case p.Type == "cast":
				cast = append(cast, p)
			}
		}
		order := func(c credit) int {
			if c.Order == nil {
				return 99
			}
			return *c.Order
		}
		slices.SortStableFunc(cast, func(a, b credit) int { return order(a) - order(b) })
		for _, c := range cast {
			d.Cast = append(d.Cast, c.PersonName)
		}
	}
	return d, nil
}

// parseTime reads an *arr timestamp. They are UTC; one without a zone is read as UTC. Zero: none.
func parseTime(s string) time.Time {
	for _, layout := range []string{time.RFC3339, "2006-01-02T15:04:05"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC()
		}
	}
	return time.Time{}
}
