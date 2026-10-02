package render

import (
	"strings"
	"testing"
	"time"

	"github.com/wtwerner/heraldarr/internal/domain"
)

func TestEpRanges(t *testing.T) {
	for _, tc := range []struct {
		nums []int
		want string
	}{
		{[]int{1}, "E01"},
		{[]int{1, 2, 3}, "E01–E03"},
		{[]int{3, 1, 2, 7, 8}, "E01–E03, E07–E08"},
		{[]int{1, 3, 5}, "E01, E03, E05"},
		{[]int{2, 4, 5, 6, 9, 11, 12}, "E02, E04–E06, E09, E11–E12"},
		{[]int{9, 10, 100}, "E09–E10, E100"},
		{nil, ""},
	} {
		if got := epRanges(tc.nums); got != tc.want {
			t.Errorf("epRanges(%v) = %q, want %q", tc.nums, got, tc.want)
		}
	}
}

func TestQualityLabel(t *testing.T) {
	for q, want := range map[string]string{
		"":                   "",
		"Bluray-2160p":       "4K Blu-ray",
		"Remux-2160p":        "4K Remux",
		"Bluray-1080p":       "1080p Blu-ray",
		"Remux-1080p":        "1080p Remux",
		"WEBDL-1080p":        "1080p WEB",
		"WEBRip-720p":        "720p WEB",
		"HDTV-720p":          "720p HDTV",
		"HDTV-1080p":         "1080p HDTV",
		"DVD-576p":           "576p DVD",
		"SDTV-480p":          "480p",
		"DVD":                "DVD",
		"webdl-2160p":        "4K WEB",
		"Bluray-480p":        "480p Blu-ray",
		"Raw-HD":             "Raw-HD",
		"Unknown":            "Unknown",
		"Bluray-1080p Remux": "1080p Remux", // Remux is checked before Bluray
		// Parity: the source match ignores case, but the resolution match doesn't (#13).
		"WEBDL-2160P":  "WEB",
		"BLURAY-1080P": "Blu-ray",
	} {
		if got := qualityLabel(q); got != want {
			t.Errorf("qualityLabel(%q) = %q, want %q", q, got, want)
		}
	}
}

func TestTrim(t *testing.T) {
	if got := trim("  short  ", 240); got != "short" {
		t.Errorf("short text: %q", got)
	}
	exact := strings.Repeat("a", 240)
	if got := trim(exact, 240); got != exact {
		t.Errorf("text at the limit was cut")
	}
	// Cut at n-3 runes, then back to the last space, then an ellipsis.
	if got, want := trim("the quick brown fox jumps", 15), "the quick…"; got != want {
		t.Errorf("word boundary: %q, want %q", got, want)
	}
	// No space in the first n-3 runes: hard cut.
	if got, want := trim("abcdefghijklmnop qr", 10), "abcdefg…"; got != want {
		t.Errorf("no space: %q, want %q", got, want)
	}
	// Counts runes, not bytes.
	if got, want := trim("ééé ééé ééé", 10), "ééé…"; got != want {
		t.Errorf("runes: %q, want %q", got, want)
	}
}

func TestTitleYear(t *testing.T) {
	for _, tc := range []struct {
		title string
		year  int
		want  string
	}{
		{"Harbor Lights", 2026, "Harbor Lights (2026)"},
		{"Harbor Lights (2019)", 2026, "Harbor Lights (2019)"},
		{"Harbor Lights", 0, "Harbor Lights"},
		{"Harbor Lights (US)", 2026, "Harbor Lights (US) (2026)"},
	} {
		if got := titleYear(tc.title, tc.year); got != tc.want {
			t.Errorf("titleYear(%q, %d) = %q, want %q", tc.title, tc.year, got, tc.want)
		}
	}
}

func TestRuntime(t *testing.T) {
	for m, want := range map[int]string{0: "", 1: "1m", 59: "59m", 60: "1h 00m", 61: "1h 01m", 124: "2h 04m", 600: "10h 00m"} {
		if got := runtime(m); got != want {
			t.Errorf("runtime(%d) = %q, want %q", m, got, want)
		}
	}
}

func TestMediaLabel(t *testing.T) {
	m := domain.Movie{
		Quality: "Remux-2160p", HDR: "DV HDR10Plus", AudioCodec: "TrueHD Atmos", AudioChannels: 7.1,
		Size: 58_300_000_000, ReleaseGroup: "EXAMPLE",
	}
	if got, want := strings.Join(mediaLabel(m), " · "), "4K Remux · DV HDR10+ · TrueHD Atmos 7.1 · 58.3 GB · EXAMPLE"; got != want {
		t.Errorf("full: %q, want %q", got, want)
	}
	if got := mediaLabel(domain.Movie{AudioChannels: 2}); len(got) != 1 || got[0] != "2.0" {
		t.Errorf("channels only: %q", got)
	}
	if got := mediaLabel(domain.Movie{}); len(got) != 0 {
		t.Errorf("empty: %q", got)
	}
}

func TestMostCommonQualityTieIsFirstInEpisodeOrder(t *testing.T) {
	eps := []domain.Episode{
		{Season: 1, Number: 1, Quality: "HDTV-720p"},
		{Season: 1, Number: 2, Quality: "WEBDL-1080p"},
		{Season: 1, Number: 3, Quality: "WEBDL-1080p"},
		{Season: 1, Number: 4, Quality: "HDTV-720p"},
		{Season: 1, Number: 5, Quality: ""},
	}
	if got := usualQuality(eps); got != "720p HDTV" {
		t.Errorf("tie: %q, want the first in episode order", got)
	}
	// The same 2–2 tie with the other label first.
	reversed := []domain.Episode{eps[1], eps[0], eps[3], eps[2], eps[4]}
	if got := usualQuality(reversed); got != "1080p WEB" {
		t.Errorf("tie, other order: %q, want the first in episode order", got)
	}
	// A 1–1 tie, both orders.
	if got := usualQuality([]domain.Episode{eps[0], eps[1]}); got != "720p HDTV" {
		t.Errorf("1–1 tie: %q", got)
	}
	if got := usualQuality([]domain.Episode{eps[1], eps[0]}); got != "1080p WEB" {
		t.Errorf("1–1 tie, other order: %q", got)
	}
	eps = append(eps, domain.Episode{Quality: "WEBRip-1080p"})
	if got := usualQuality(eps); got != "1080p WEB" {
		t.Errorf("majority: %q", got)
	}
	if got := usualQuality(nil); got != "" {
		t.Errorf("none: %q", got)
	}
}

var now = time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)

func tvBatch(eps ...domain.Episode) *domain.Batch {
	b := &domain.Batch{
		Key: "sonarr:1", Source: "sonarr", Kind: domain.KindTV,
		Series:   &domain.Series{ID: 1, Title: "Example Show", Year: 2020, TVDBID: 9, Genres: []string{"Drama"}},
		Episodes: map[domain.ItemKey]domain.Episode{},
	}
	for i, e := range eps {
		e.ID = i + 1
		b.Episodes[domain.EpisodeKey("sonarr", e.ID)] = e
	}
	return b
}

func TestTVLineCap(t *testing.T) {
	// 13 seasons of two episodes each: one line per episode would be 26 lines; capped to 11 + "…and 15 more".
	var eps []domain.Episode
	for s := 1; s <= 13; s++ {
		eps = append(eps, domain.Episode{Season: s, Number: 1, Title: "A"}, domain.Episode{Season: s, Number: 2, Title: "B"})
	}
	card := TV(TVInput{Common: Common{Now: now}, Batch: tvBatch(eps...)})
	if len(card.Lines) != 12 {
		t.Fatalf("got %d lines, want 12: %q", len(card.Lines), card.Lines)
	}
	if card.Lines[11] != "…and 15 more" {
		t.Errorf("last line %q", card.Lines[11])
	}
	if card.Lines[0] != "`S01E01` · A" {
		t.Errorf("first line %q", card.Lines[0])
	}
}

func TestTVExactlyTwelveLinesNotCapped(t *testing.T) {
	var eps []domain.Episode
	for s := 1; s <= 6; s++ {
		eps = append(eps, domain.Episode{Season: s, Number: 1}, domain.Episode{Season: s, Number: 2})
	}
	card := TV(TVInput{Common: Common{Now: now}, Batch: tvBatch(eps...)})
	if len(card.Lines) != 12 || strings.HasPrefix(card.Lines[11], "…") {
		t.Errorf("lines %q", card.Lines)
	}
	if card.Lines[0] != "`S01E01` · TBA" {
		t.Errorf("untitled episode: %q", card.Lines[0])
	}
}

func TestDigestLineCap(t *testing.T) {
	var movies []MovieInput
	for i := range 17 {
		movies = append(movies, MovieInput{Movie: domain.Movie{ID: i, Title: string(rune('A' + i)), Year: 2000 + i}})
	}
	card := Digest(movies, Common{Now: now})
	if len(card.Lines) != 15 || card.Lines[14] != "…and 3 more" {
		t.Errorf("lines %q", card.Lines)
	}
	if card.Headline != "17 new movies" || card.Lines[0] != "**A (2000)**" {
		t.Errorf("headline %q, first line %q", card.Headline, card.Lines[0])
	}
	movies = movies[:15]
	if card := Digest(movies, Common{Now: now}); len(card.Lines) != 15 || strings.HasPrefix(card.Lines[14], "…") {
		t.Errorf("15 movies were capped: %q", card.Lines)
	}
}

func TestStyle(t *testing.T) {
	m := MovieInput{Movie: domain.Movie{
		ID: 1, Title: "Example", Year: 2024, TMDBID: 5, Quality: "Bluray-2160p", HDR: "HDR10",
		Size: 20_000_000_000, ReleaseGroup: "GRP",
	}}
	plain := Movie(m, Common{ServerName: "Home"})
	if plain.Headline != "New movie" || plain.Color != colorSeries || plain.Footer != "Home · 4K Blu-ray" {
		t.Errorf("zero style: %q %#x %q", plain.Headline, plain.Color, plain.Footer)
	}
	styled := Movie(m, Common{ServerName: "Home", Style: domain.Style{Label: "UHD", Color: 0x123456, TechDetails: true}})
	if styled.Headline != "New in UHD" || styled.Color != 0x123456 || styled.Footer != "Home · 4K Blu-ray · HDR10 · 20.0 GB · GRP" {
		t.Errorf("styled: %q %#x %q", styled.Headline, styled.Color, styled.Footer)
	}

	movies := []MovieInput{m, m, m, m}
	if d := Digest(movies, Common{Style: domain.Style{Label: "4K"}}); d.Headline != "4 new 4K movies" || d.Color != colorSeries {
		t.Errorf("digest: %q %#x", d.Headline, d.Color)
	}
	if d := Digest(movies, Common{Style: domain.Style{Color: 0xABCDEF}}); d.Headline != "4 new movies" || d.Color != 0xABCDEF {
		t.Errorf("digest color: %q %#x", d.Headline, d.Color)
	}

	b := tvBatch(domain.Episode{Season: 1, Number: 1}, domain.Episode{Season: 1, Number: 2})
	tv := TV(TVInput{Common: Common{Style: domain.Style{Label: "4K", Color: 0x654321}}, Batch: b})
	if tv.Headline != "2 new episodes" || tv.Color != 0x654321 { // the reference never labels TV
		t.Errorf("tv: %q %#x", tv.Headline, tv.Color)
	}
	if tv := TV(TVInput{Batch: b}); tv.Headline != "2 new episodes" || tv.Color != colorEpisodes {
		t.Errorf("tv zero style: %q %#x", tv.Headline, tv.Color)
	}
}

func TestCollectionLine(t *testing.T) {
	for collection, want := range map[string]string{
		"Small Hours":             "Part of the Small Hours",
		"The Meridian Collection": "Part of the Meridian Collection",
		// Parity: every "the The " is rewritten, not just the leading article (#13).
		"Tales of the The Endless": "Part of the Tales of the Endless",
	} {
		card := Movie(MovieInput{Movie: domain.Movie{Title: "X"}, Detail: &domain.MovieDetail{Collection: collection}}, Common{})
		if got := card.Lines[len(card.Lines)-1]; got != want {
			t.Errorf("%q: %q, want %q", collection, got, want)
		}
	}
}

func TestTVHeadlines(t *testing.T) {
	one := 1
	d := &domain.SeriesDetail{EpisodeFileCount: &one, Seasons: map[int]domain.SeasonStats{1: {EpisodeFileCount: 1, TotalEpisodeCount: 8}}}
	b := tvBatch(domain.Episode{Season: 1, Number: 1})
	for _, tc := range []struct {
		label, want string
	}{{"", "New series"}, {"4K", "New series"}} {
		if got := TV(TVInput{Common: Common{Style: domain.Style{Label: tc.label}}, Batch: b, Detail: d}).Headline; got != tc.want {
			t.Errorf("series %q: %q", tc.label, got)
		}
	}
	ten := 10
	d = &domain.SeriesDetail{EpisodeFileCount: &ten, Seasons: map[int]domain.SeasonStats{
		1: {EpisodeFileCount: 8, TotalEpisodeCount: 8}, 2: {EpisodeFileCount: 1, TotalEpisodeCount: 8}, 3: {EpisodeFileCount: 1, TotalEpisodeCount: 8},
	}}
	if got := TV(TVInput{Batch: tvBatch(domain.Episode{Season: 2, Number: 1}), Detail: d}).Headline; got != "New season" {
		t.Errorf("season: %q", got)
	}
	b = tvBatch(domain.Episode{Season: 2, Number: 1}, domain.Episode{Season: 3, Number: 1})
	if got := TV(TVInput{Batch: b, Detail: d, Common: Common{Style: domain.Style{Label: "4K"}}}).Headline; got != "New seasons" {
		t.Errorf("seasons: %q", got)
	}
	if got := TV(TVInput{Batch: tvBatch(domain.Episode{Season: 1, Number: 3}), Detail: d}).Headline; got != "New episode" {
		t.Errorf("episode: %q", got)
	}
}

func TestLayoutsWithoutURL(t *testing.T) {
	card := domain.Card{Headline: "2 new movies", Title: "2 new movies", Color: 1}
	ls := Layouts(card, now)
	names := []string{ls[0].Name, ls[1].Name, ls[2].Name}
	if strings.Join(names, "|") != "v2|embed|embed, inline links" {
		t.Errorf("names %q", names)
	}
	parts := ls[0].Body["components"].([]any)[0].(map[string]any)["components"].([]any)
	if got := parts[0].(map[string]any)["content"]; got != "## 2 new movies" {
		t.Errorf("v2 head %q", got)
	}
	if got := parts[2].(map[string]any)["content"]; got != emptyText {
		t.Errorf("v2 empty lines %q", got)
	}
	embed := ls[1].Body["embeds"].([]any)[0].(map[string]any)
	if _, ok := embed["title"]; ok {
		t.Error("embed without url has a title")
	}
	if embed["footer"].(map[string]any)["text"] != "Plex" {
		t.Errorf("footer %v", embed["footer"])
	}
	if embed["timestamp"] != "2026-06-15T12:00:00+00:00" {
		t.Errorf("timestamp %v", embed["timestamp"])
	}
}

func TestLayoutsCapButtonsAndDescription(t *testing.T) {
	card := domain.Card{Title: "T", URL: "https://example.org/", Lines: []string{strings.Repeat("é", 5000)}}
	for i := range 6 {
		card.Buttons = append(card.Buttons, domain.Button{Label: string(rune('a' + i)), URL: "https://example.org/", Emoji: "🎬"})
	}
	ls := Layouts(card, now)
	row := ls[1].Body["components"].([]any)[0].(map[string]any)["components"].([]any)
	if len(row) != 5 {
		t.Errorf("%d buttons, want 5", len(row))
	}
	desc := ls[1].Body["embeds"].([]any)[0].(map[string]any)["description"].(string)
	if n := len([]rune(desc)); n != 4000 {
		t.Errorf("description %d runes, want 4000", n)
	}
	if got := ls[2].Body["components"].([]any); len(got) != 0 {
		t.Errorf("inline layout has components: %v", got)
	}
}

// A back-catalog run still downloading says so on its last line, within the line cap.
func TestTVDownloading(t *testing.T) {
	var eps []domain.Episode
	for s := 1; s <= 13; s++ {
		eps = append(eps, domain.Episode{Season: s, Number: 1, Title: "A"}, domain.Episode{Season: s, Number: 2, Title: "B"})
	}
	card := TV(TVInput{Common: Common{Now: now}, Batch: tvBatch(eps...), Downloading: 9})
	if len(card.Lines) != 12 || card.Lines[11] != "_9 more on the way_" || card.Lines[10] != "…and 16 more" {
		t.Errorf("lines %q", card.Lines)
	}
	card = TV(TVInput{Common: Common{Now: now}, Batch: tvBatch(eps[:1]...)})
	if strings.Contains(strings.Join(card.Lines, "\n"), "on the way") {
		t.Errorf("nothing queued: %q", card.Lines)
	}
}

func TestTVScores(t *testing.T) {
	one := 1
	d := &domain.SeriesDetail{EpisodeFileCount: &one, Rating: 8.3, Certification: "TV-14", Seasons: map[int]domain.SeasonStats{1: {EpisodeFileCount: 1, TotalEpisodeCount: 8}}}
	rating := func(d *domain.SeriesDetail, media domain.Scores) string {
		for _, f := range TV(TVInput{Batch: tvBatch(domain.Episode{Season: 1, Number: 1}), Detail: d, Scores: media}).Facts {
			if f.Name == "Rating" {
				return f.Value
			}
		}
		return ""
	}
	media := domain.Scores{IMDb: 7.9, RTCritic: 96, RTAudience: 82}
	if got := rating(d, media); got != "★ 8.3 IMDb · 🍅 96% · 🍿 82% · TV-14" {
		t.Errorf("both: %q", got)
	}
	d.Rating = 0
	if got := rating(d, media); got != "★ 7.9 IMDb · 🍅 96% · 🍿 82% · TV-14" {
		t.Errorf("media server only: %q", got)
	}
	if got := rating(d, domain.Scores{}); got != "TV-14" {
		t.Errorf("no scores: %q", got)
	}
}

func TestMovieScores(t *testing.T) {
	for _, tc := range []struct {
		detail *domain.MovieDetail
		media  domain.Scores
		want   string
	}{
		{&domain.MovieDetail{IMDbRating: 7.4, RTRating: 91}, domain.Scores{IMDb: 7.3, RTCritic: 90, RTAudience: 85}, "★ 7.4 IMDb · 🍅 91% · 🍿 85%"},
		{&domain.MovieDetail{IMDbRating: 7.4}, domain.Scores{RTCritic: 90}, "★ 7.4 IMDb · 🍅 90%"},
		{nil, domain.Scores{IMDb: 6.2, RTAudience: 40}, "★ 6.2 IMDb · 🍿 40%"},
	} {
		in := MovieInput{Movie: domain.Movie{Title: "Example"}, Detail: tc.detail, Scores: tc.media}
		if got := strings.Join(movieScores(in), " · "); got != tc.want {
			t.Errorf("movieScores(%+v, %+v) = %q, want %q", tc.detail, tc.media, got, tc.want)
		}
		if line := Movie(in, Common{}).Lines[0]; !strings.HasPrefix(line, tc.want) {
			t.Errorf("movie card line %q, want it to start with %q", line, tc.want)
		}
		if line := Digest([]MovieInput{in}, Common{}).Lines[0]; !strings.Contains(line, tc.want) {
			t.Errorf("digest line %q, want %q", line, tc.want)
		}
	}
}
