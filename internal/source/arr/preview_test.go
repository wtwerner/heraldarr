package arr

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestSeriesAndMovieImport(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path + "?" + r.URL.RawQuery {
		case "/api/v3/series/7?":
			_, _ = w.Write([]byte(`{"id":7,"title":"Example","year":2020,"path":"/tv/Example","tvdbId":70,
				"images":[{"coverType":"poster","remoteUrl":"https://images.example.org/p.jpg"}]}`))
		case "/api/v3/episode?seriesId=7":
			_, _ = w.Write([]byte(`[
				{"id":1,"seasonNumber":1,"episodeNumber":1,"title":"A","hasFile":true,"episodeFileId":11},
				{"id":2,"seasonNumber":2,"episodeNumber":1,"title":"B","hasFile":true,"episodeFileId":12},
				{"id":3,"seasonNumber":2,"episodeNumber":2,"title":"C","hasFile":false},
				{"id":4,"seasonNumber":2,"episodeNumber":3,"title":"D","hasFile":true,"episodeFileId":13}]`))
		case "/api/v3/episodefile?seriesId=7":
			_, _ = w.Write([]byte(`[{"id":11,"quality":{"quality":{"name":"HDTV-720p"}}},
				{"id":12,"quality":{"quality":{"name":"WEBDL-1080p"}}},{"id":13,"quality":{"quality":{"name":"WEBDL-1080p"}}}]`))
		case "/api/v3/movie/9?":
			_, _ = w.Write([]byte(`{"id":9,"title":"M","year":2021,"path":"/movies/M","tmdbId":90,
				"movieFile":{"quality":{"quality":{"name":"Bluray-2160p"}},"size":5,"releaseGroup":"G",
				"mediaInfo":{"videoDynamicRangeType":"HDR10","audioCodec":"DTS","audioChannels":5.1}}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	c := NewClient(srv.URL, StaticKey("k"), "test", DefaultTimeout)
	ctx := context.Background()

	for sel, want := range map[string][]int{"": {1, 2, 4}, "S02": {2, 4}, "s02e03,S01E01": {1, 4}} {
		imp, err := c.SeriesImport(ctx, "sonarr", 7, sel)
		if err != nil {
			t.Fatalf("%q: %v", sel, err)
		}
		var got []int
		for _, e := range imp.Episodes {
			got = append(got, e.ID)
		}
		if len(got) != len(want) {
			t.Errorf("%q: got %v, want %v", sel, got, want)
		}
	}
	imp, _ := c.SeriesImport(ctx, "sonarr", 7, "S02E03")
	if imp.Episodes[0].Quality != "WEBDL-1080p" || imp.Series.Poster == "" {
		t.Errorf("unexpected import %+v %+v", imp.Series, imp.Episodes)
	}
	if _, err := c.SeriesImport(ctx, "sonarr", 7, "S09"); err == nil {
		t.Error("S09: want no-match error")
	}
	if _, err := c.SeriesImport(ctx, "sonarr", 7, "season 2"); err == nil {
		t.Error("bad selector: want error")
	}

	m, err := c.MovieImport(ctx, "radarr", 9)
	if err != nil || m.Movie.Quality != "Bluray-2160p" || m.Movie.HDR != "HDR10" || m.Movie.AudioChannels != 5.1 {
		t.Errorf("MovieImport = %+v, %v", m.Movie, err)
	}
}
