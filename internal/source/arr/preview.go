package arr

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/wtwerner/heraldarr/internal/domain"
)

// These build imports from what is already on disk, as if it had just arrived, for previews.

type quality struct {
	Quality struct {
		Name string `json:"name"`
	} `json:"quality"`
}

// SeriesImport returns the episodes of a series that have files, optionally narrowed by selector:
// "S02" (a season) or "S02E05,S02E06" (episodes); "" means all.
func (c *Client) SeriesImport(ctx context.Context, source string, id int, selector string) (domain.Import, error) {
	var s struct {
		ID     int      `json:"id"`
		Title  string   `json:"title"`
		Year   int      `json:"year"`
		Path   string   `json:"path"`
		TVDBID int      `json:"tvdbId"`
		IMDbID string   `json:"imdbId"`
		Genres []string `json:"genres"`
		Images []image  `json:"images"`
	}
	var eps []struct {
		ID            int    `json:"id"`
		SeasonNumber  int    `json:"seasonNumber"`
		EpisodeNumber int    `json:"episodeNumber"`
		Title         string `json:"title"`
		AirDateUTC    string `json:"airDateUtc"`
		HasFile       bool   `json:"hasFile"`
		EpisodeFileID int    `json:"episodeFileId"`
	}
	var files []struct {
		ID      int     `json:"id"`
		Quality quality `json:"quality"`
	}
	want, err := parseSelector(selector) // before any request: a typo shouldn't cost three calls
	if err != nil {
		return domain.Import{}, err
	}
	q := "?seriesId=" + strconv.Itoa(id)
	for path, v := range map[string]any{"series/" + strconv.Itoa(id): &s, "episode" + q: &eps, "episodefile" + q: &files} {
		if err := c.getJSON(ctx, path, v); err != nil {
			return domain.Import{}, err
		}
	}
	qualities := map[int]string{}
	for _, f := range files {
		qualities[f.ID] = f.Quality.Quality.Name
	}
	imp := domain.Import{Source: source, Series: &domain.Series{
		ID: s.ID, Title: s.Title, Year: s.Year, Path: s.Path,
		TVDBID: s.TVDBID, IMDbID: s.IMDbID, Genres: s.Genres, Poster: pickImage(s.Images, "poster"),
		Fanart: pickImage(s.Images, "fanart"),
	}}
	for _, e := range eps {
		if !e.HasFile || !want(e.SeasonNumber, e.EpisodeNumber) {
			continue
		}
		imp.Episodes = append(imp.Episodes, domain.Episode{
			ID: e.ID, Season: e.SeasonNumber, Number: e.EpisodeNumber,
			Title: e.Title, Aired: parseTime(e.AirDateUTC), Quality: qualities[e.EpisodeFileID],
		})
	}
	if len(imp.Episodes) == 0 {
		return imp, fmt.Errorf("series %d: no episodes with files match %q", id, selector)
	}
	return imp, nil
}

// MovieImport returns a movie that has a file.
func (c *Client) MovieImport(ctx context.Context, source string, id int) (domain.Import, error) {
	var m struct {
		ID        int      `json:"id"`
		Title     string   `json:"title"`
		Year      int      `json:"year"`
		Path      string   `json:"path"`
		TMDBID    int      `json:"tmdbId"`
		IMDbID    string   `json:"imdbId"`
		Genres    []string `json:"genres"`
		Overview  string   `json:"overview"`
		Images    []image  `json:"images"`
		MovieFile *struct {
			Quality      quality `json:"quality"`
			Size         int64   `json:"size"`
			ReleaseGroup string  `json:"releaseGroup"`
			MediaInfo    *struct {
				VideoDynamicRangeType string  `json:"videoDynamicRangeType"`
				AudioCodec            string  `json:"audioCodec"`
				AudioChannels         float64 `json:"audioChannels"`
			} `json:"mediaInfo"`
		} `json:"movieFile"`
	}
	if err := c.getJSON(ctx, "movie/"+strconv.Itoa(id), &m); err != nil {
		return domain.Import{}, err
	}
	if m.MovieFile == nil {
		return domain.Import{}, fmt.Errorf("movie %d has no file", id)
	}
	mv := &domain.Movie{
		ID: m.ID, Title: m.Title, Year: m.Year, Path: m.Path, TMDBID: m.TMDBID, IMDbID: m.IMDbID,
		Genres: m.Genres, Overview: m.Overview, Poster: pickImage(m.Images, "poster"), Fanart: pickImage(m.Images, "fanart"),
		Quality: m.MovieFile.Quality.Quality.Name, Size: m.MovieFile.Size, ReleaseGroup: m.MovieFile.ReleaseGroup,
	}
	if mi := m.MovieFile.MediaInfo; mi != nil {
		mv.HDR, mv.AudioCodec, mv.AudioChannels = mi.VideoDynamicRangeType, mi.AudioCodec, mi.AudioChannels
	}
	return domain.Import{Source: source, Movie: mv}, nil
}

func (c *Client) getJSON(ctx context.Context, path string, v any) error {
	body, err := c.get(ctx, path)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(body, v); err != nil {
		return fmt.Errorf("GET %s: %w", path, err)
	}
	return nil
}

var selectorRE = regexp.MustCompile(`^S(\d+)(?:E(\d+))?$`)

func parseSelector(sel string) (func(season, episode int) bool, error) {
	if strings.TrimSpace(sel) == "" {
		return func(int, int) bool { return true }, nil
	}
	type pick struct{ season, episode int } // episode -1: whole season
	var picks []pick
	for _, part := range strings.Split(strings.ToUpper(sel), ",") {
		m := selectorRE.FindStringSubmatch(strings.TrimSpace(part))
		if m == nil {
			return nil, fmt.Errorf("selector %q: want S02 or S02E05,S02E06", sel)
		}
		p := pick{episode: -1}
		p.season, _ = strconv.Atoi(m[1])
		if m[2] != "" {
			p.episode, _ = strconv.Atoi(m[2])
		}
		picks = append(picks, p)
	}
	return func(season, episode int) bool {
		for _, p := range picks {
			if p.season == season && (p.episode == -1 || p.episode == episode) {
				return true
			}
		}
		return false
	}, nil
}
