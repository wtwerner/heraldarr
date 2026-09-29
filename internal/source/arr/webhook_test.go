package arr_test

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/wtwerner/heraldarr/internal/domain"
	"github.com/wtwerner/heraldarr/internal/source/arr"
)

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func utc(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t
}

var glassOrchard = &domain.Series{
	ID: 301, Title: "The Glass Orchard", Year: 2024, Path: "/data/media/TV/The Glass Orchard",
	TVDBID: 900301, IMDbID: "tt9000301", Genres: []string{"Drama", "Mystery"},
	Poster: "https://images.example.org/poster/glass-orchard.jpg",
	Fanart: "https://images.example.org/fanart/glass-orchard.jpg",
}

var grafting = []domain.Episode{{
	ID: 3104, Season: 2, Number: 4, Title: "Grafting", Aired: utc("2025-03-02T02:00:00Z"), Quality: "WEBDL-1080p",
}}

func TestParseWebhookVersions(t *testing.T) {
	for _, tc := range []struct {
		fixture, source string
		kind            domain.Kind
		want            domain.Import
	}{
		{
			// v3 series carry no year, genres or images.
			"sonarr_v3_download.json", "sonarr", domain.KindTV,
			domain.Import{Source: "sonarr", Series: &domain.Series{
				ID: 301, Title: "The Glass Orchard", Path: "/data/media/TV/The Glass Orchard",
				TVDBID: 900301, IMDbID: "tt9000301",
			}, Episodes: grafting},
		},
		{
			"sonarr_v4_download.json", "sonarr", domain.KindTV,
			domain.Import{Source: "sonarr", Series: glassOrchard, Episodes: grafting},
		},
		{
			// On Import Complete: one event for the whole download, several files, no episode→file
			// mapping. Every episode gets the files' usual quality (2 of 3 files are WEBDL-1080p).
			"sonarr_v4_import_complete.json", "sonarr", domain.KindTV,
			domain.Import{Source: "sonarr", Series: &domain.Series{
				ID: 302, Title: "Lanterns Over Kettle Bay", Year: 2019, Path: "/data/media/TV/Lanterns Over Kettle Bay",
				TVDBID: 900302, IMDbID: "tt9000302", Genres: []string{"Comedy"},
				Poster: "https://images.example.org/poster/kettle-bay.jpg",
				Fanart: "https://images.example.org/fanart/kettle-bay.jpg",
			}, Episodes: []domain.Episode{
				{ID: 3201, Season: 1, Number: 1, Title: "Low Tide", Aired: utc("2019-09-06T01:00:00Z"), Quality: "WEBDL-1080p"},
				{ID: 3202, Season: 1, Number: 2, Title: "The Ferry Strike", Aired: utc("2019-09-13T01:00:00Z"), Quality: "WEBDL-1080p"},
				{ID: 3203, Season: 1, Number: 3, Title: "Fog Horn (1)", Aired: utc("2019-09-20T01:00:00Z"), Quality: "WEBDL-1080p"},
				{ID: 3204, Season: 1, Number: 4, Title: "Fog Horn (2)", Aired: utc("2019-09-20T01:30:00Z"), Quality: "WEBDL-1080p"},
			}},
		},
		{
			// v4 movies carry no genres or images.
			"radarr_v4_download.json", "radarr", domain.KindMovie,
			domain.Import{Source: "radarr", Movie: &domain.Movie{
				ID: 401, Title: "Harbor of Small Lights", Year: 2023, Path: "/data/media/Movies/Harbor of Small Lights (2023)",
				TMDBID: 700401, IMDbID: "tt9000401",
				Overview: "A lighthouse keeper's daughter maps every boat that never came home.",
				Quality:  "Bluray-1080p", Size: 9876543210, ReleaseGroup: "EXAMPLE",
				AudioCodec: "DTS-HD MA", AudioChannels: 7.1,
			}},
		},
		{
			"radarr_v5_download.json", "radarr4k", domain.KindMovie,
			domain.Import{Source: "radarr4k", Movie: &domain.Movie{
				ID: 402, Title: "The Cartographer's Winter", Year: 2024,
				Path:   "/data/media/Movies 4K/The Cartographer's Winter (2024)",
				TMDBID: 700402, IMDbID: "tt9000402", Genres: []string{"Drama", "History"},
				Overview: "Snowed in at a mountain archive, two rivals redraw a border.",
				Poster:   "https://images.example.org/poster/cartographers-winter.jpg",
				Fanart:   "https://images.example.org/fanart/cartographers-winter.jpg",
				Quality:  "Remux-2160p", Size: 61200000000, ReleaseGroup: "EXAMPLE",
				HDR: "DV HDR10", AudioCodec: "TrueHD Atmos", AudioChannels: 7.1,
			}},
		},
	} {
		t.Run(tc.fixture, func(t *testing.T) {
			got, ok, err := arr.ParseWebhook(tc.source, tc.kind, fixture(t, tc.fixture))
			if err != nil || !ok {
				t.Fatalf("ok=%v err=%v", ok, err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("got  %+v\nwant %+v", describe(got), describe(tc.want))
			}
		})
	}
}

func describe(imp domain.Import) any {
	return struct {
		Import   domain.Import
		Series   *domain.Series
		Movie    *domain.Movie
		Episodes []domain.Episode
	}{imp, imp.Series, imp.Movie, imp.Episodes}
}

func TestParseWebhookIgnoredEvents(t *testing.T) {
	for _, tc := range []struct {
		name string
		kind domain.Kind
		body []byte
	}{
		{"sonarr test", domain.KindTV, fixture(t, "sonarr_v4_test.json")},
		{"sonarr grab", domain.KindTV, fixture(t, "sonarr_v4_grab.json")},
		{"sonarr rename", domain.KindTV, fixture(t, "sonarr_v4_rename.json")},
		{"radarr test", domain.KindMovie, fixture(t, "radarr_v5_test.json")},
		{"radarr grab", domain.KindMovie, fixture(t, "radarr_v5_grab.json")},
		{"radarr rename", domain.KindMovie, fixture(t, "radarr_v5_rename.json")},
		{"series add", domain.KindTV, []byte(`{"eventType":"SeriesAdd","series":{"id":301,"title":"The Glass Orchard"}}`)},
		{"health", domain.KindTV, []byte(`{"eventType":"Health","level":"warning","message":"Indexers unavailable"}`)},
		{"manual interaction", domain.KindMovie, []byte(`{"eventType":"ManualInteractionRequired","movie":{"id":402}}`)},
		{"future event", domain.KindMovie, []byte(`{"eventType":"SomethingNew","movie":"not an object"}`)},
		{"no event type", domain.KindTV, []byte(`{"series":{"id":301}}`)},
		// An ignored event must not fail on fields a Download would need in another shape.
		{"odd test payload", domain.KindTV, []byte(`{"eventType":"Test","series":"Test Title","episodes":7}`)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			imp, ok, err := arr.ParseWebhook("src", tc.kind, tc.body)
			if ok || err != nil {
				t.Errorf("ok=%v err=%v imp=%+v", ok, err, imp)
			}
		})
	}
}

func TestParseWebhookMalformed(t *testing.T) {
	for _, tc := range []struct {
		name string
		kind domain.Kind
		body string
	}{
		{"not json", domain.KindTV, `{`},
		{"not an object", domain.KindTV, `[1,2]`},
		{"download without series", domain.KindTV, `{"eventType":"Download","movie":{"id":1}}`},
		{"download without movie", domain.KindMovie, `{"eventType":"Download","series":{"id":1}}`},
		{"series of the wrong type", domain.KindTV, `{"eventType":"Download","series":"The Glass Orchard"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, ok, err := arr.ParseWebhook("src", tc.kind, []byte(tc.body)); ok || err == nil {
				t.Errorf("ok=%v err=%v, want an error", ok, err)
			}
		})
	}
}

// Air dates are UTC; a timestamp without a zone is read as UTC rather than dropped.
func TestParseWebhookAirDates(t *testing.T) {
	for in, want := range map[string]time.Time{
		"2025-03-02T02:00:00Z":        utc("2025-03-02T02:00:00Z"),
		"2025-03-02T02:00:00.5Z":      utc("2025-03-02T02:00:00.5Z"),
		"2025-03-02T02:00:00":         utc("2025-03-02T02:00:00Z"),
		"2025-03-02T02:00:00.1234567": utc("2025-03-02T02:00:00.1234567Z"),
		"2025-03-01T21:00:00-05:00":   utc("2025-03-02T02:00:00Z"),
		"":                            {},
		"not a date":                  {},
	} {
		body := `{"eventType":"Download","series":{"id":1},"episodes":[{"id":2,"airDateUtc":"` + in + `"}]}`
		imp, _, err := arr.ParseWebhook("sonarr", domain.KindTV, []byte(body))
		if err != nil {
			t.Fatal(err)
		}
		if got := imp.Episodes[0].Aired; !got.Equal(want) || got.Location() != time.UTC {
			t.Errorf("%q: got %v, want %v", in, got, want)
		}
	}
	// null (an episode without an air date) is not an error.
	imp, ok, err := arr.ParseWebhook("sonarr", domain.KindTV,
		[]byte(`{"eventType":"Download","series":{"id":1},"episodes":[{"id":2,"airDateUtc":null}]}`))
	if err != nil || !ok || !imp.Episodes[0].Aired.IsZero() {
		t.Errorf("null air date: ok=%v err=%v aired=%v", ok, err, imp.Episodes[0].Aired)
	}
}
