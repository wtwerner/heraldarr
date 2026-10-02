package plex

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/wtwerner/heraldarr/internal/domain"
	"github.com/wtwerner/heraldarr/internal/testkit"
)

var start = time.Date(2026, 9, 29, 20, 0, 0, 0, time.UTC)

var arrToPlex = []PathMap{{From: "/data/media/", To: "/media/"}}

func setup(t *testing.T) (*Plex, *fakePlex, *testkit.Clock) {
	t.Helper()
	f := newFakePlex(t, libraryFixtures()...)
	clock := testkit.NewClock(start)
	return New(f.URL+"/", fakeToken, arrToPlex, WithClock(clock)), f, clock
}

func find(t *testing.T, p *Plex, guids []string, path string, kind domain.Kind, fresh bool) *domain.MediaItem {
	t.Helper()
	it, err := p.Find(t.Context(), guids, path, kind, fresh)
	if err != nil {
		t.Fatalf("Find(%v, %q): %v", guids, path, err)
	}
	return it
}

func ratingKey(it *domain.MediaItem) string {
	if it == nil {
		return "<nil>"
	}
	return it.RatingKey
}

func TestTokenFromPreferences(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	ok := write("Preferences.xml", `<?xml version="1.0" encoding="utf-8"?>
<Preferences MachineIdentifier="abc" FriendlyName="Example Server" PlexOnlineToken="tok&amp;en-123" PlexOnlineUsername="someone"/>`)
	got, err := TokenFromPreferences(ok)
	if err != nil || got != "tok&en-123" {
		t.Errorf("TokenFromPreferences = %q, %v; want tok&en-123", got, err)
	}

	for name, path := range map[string]string{
		"no token":  write("NoToken.xml", `<Preferences MachineIdentifier="abc"/>`),
		"not xml":   write("Broken.xml", `PlexOnlineToken="abc`),
		"not there": filepath.Join(dir, "missing.xml"),
	} {
		if got, err := TokenFromPreferences(path); err == nil {
			t.Errorf("%s: got %q, want an error", name, got)
		}
	}
}

func TestNameAndURLMatchTheFake(t *testing.T) {
	p, _, _ := setup(t)
	if p.Name() != "" || p.URL("6201") != "" {
		t.Fatalf("before any request: Name %q, URL %q; want both empty", p.Name(), p.URL("6201"))
	}
	find(t, p, []string{"tmdb://800201"}, "/data/media/Movies/Film (2026)", domain.KindMovie, false)
	if p.Name() != fakeName {
		t.Errorf("Name = %q, want %q", p.Name(), fakeName)
	}
	sc := &testkit.Scenario{}
	sc.PlexServer.ID = fakeMachine
	want := testkit.Media{Sc: sc}.URL("6201")
	if got := p.URL("6201"); got != want {
		t.Errorf("URL = %s\nwant  %s", got, want)
	}
	if want := "https://app.plex.tv/desktop/#!/server/" + fakeMachine + "/details?key=%2Flibrary%2Fmetadata%2F6201"; p.URL("6201") != want {
		t.Errorf("URL = %s\nwant  %s", p.URL("6201"), want)
	}
}

func TestFindLinksTheLibraryHoldingThePath(t *testing.T) {
	p, _, _ := setup(t)
	guids := []string{"tmdb://800301", "imdb://tt9800301"}
	for _, tc := range []struct{ path, want string }{
		{"/data/media/Movies 4K/Film (2025)", "7301"},
		{"/data/media/Movies/Film (2025)", "7300"},
	} {
		if got := ratingKey(find(t, p, guids, tc.path, domain.KindMovie, false)); got != tc.want {
			t.Errorf("Find in %s = %s, want %s", tc.path, got, tc.want)
		}
	}
}

func TestFindWithoutAHoldingLibraryTriesEveryLibraryOfTheKind(t *testing.T) {
	p, _, _ := setup(t)
	for _, path := range []string{"", "/elsewhere/Film (2025)"} {
		if got := ratingKey(find(t, p, []string{"tmdb://800301"}, path, domain.KindMovie, false)); got != "7300" {
			t.Errorf("Find at %q = %s, want the first movie library's 7300", path, got)
		}
	}
	// Found only in the second movie library.
	p, f, _ := setup(t)
	f.add("2", fakeItem{RatingKey: "7400", GUIDs: []string{"tmdb://800400"}, AddedAt: 5000})
	if got := ratingKey(find(t, p, []string{"tmdb://800400"}, "", domain.KindMovie, false)); got != "7400" {
		t.Errorf("Find = %s, want 7400", got)
	}
}

func TestFindHoldingLibraryMissDoesNotBorrowAnotherLibrary(t *testing.T) {
	// The 4K copy hasn't been scanned yet but the 1080p one exists: wait for the 4K library rather
	// than linking the wrong copy.
	p, _, _ := setup(t)
	if got := find(t, p, []string{"tmdb://800201"}, "/data/media/Movies 4K/Film (2026)", domain.KindMovie, false); got != nil {
		t.Errorf("Find = %s, want nil", got.RatingKey)
	}
}

func TestFindMatchesAnyGUIDAndOnlyTheKind(t *testing.T) {
	p, _, _ := setup(t)
	show := find(t, p, []string{"", "tvdb://999", "imdb://tt9900102"}, "/data/media/TV/Glass Orchard (2022)", domain.KindTV, false)
	if ratingKey(show) != "5201" {
		t.Errorf("Find show by imdb = %s, want 5201", ratingKey(show))
	}
	if got := find(t, p, []string{"tmdb://800201"}, "", domain.KindTV, false); got != nil {
		t.Errorf("a movie GUID found a show: %s", got.RatingKey)
	}
	if got := find(t, p, []string{"tvdb://900102"}, "", domain.KindMovie, false); got != nil {
		t.Errorf("a show GUID found a movie: %s", got.RatingKey)
	}
	if got := find(t, p, []string{"", "tmdb://1"}, "", domain.KindMovie, false); got != nil {
		t.Errorf("unknown GUIDs found %s", got.RatingKey)
	}
}

func TestFindReturnsContentRating(t *testing.T) {
	p, _, _ := setup(t)
	for _, tc := range []struct {
		guid, path string
		kind       domain.Kind
		want       domain.MediaItem
	}{
		{
			"tvdb://900102", "/data/media/TV/Glass Orchard (2022)", domain.KindTV,
			domain.MediaItem{RatingKey: "5201", ContentRating: "TV-MA"},
		},
		{
			"tmdb://800301", "/data/media/Movies 4K/Film", domain.KindMovie,
			domain.MediaItem{RatingKey: "7301", ContentRating: "R"},
		},
		{
			"tvdb://900101", "/data/media/TV/Harbor Lights (2026)", domain.KindTV,
			domain.MediaItem{RatingKey: "5101"},
		},
	} {
		got := find(t, p, []string{tc.guid}, tc.path, tc.kind, false)
		if got == nil || *got != tc.want {
			t.Errorf("Find(%s) = %+v, want %+v", tc.guid, got, tc.want)
		}
	}
}

func TestScoresReadEverySourceWhateverTheLibraryShows(t *testing.T) {
	p, f, _ := setup(t)
	ctx := t.Context()
	got, err := p.Scores(ctx, "5201") // the library shows TMDB
	if err != nil {
		t.Fatal(err)
	}
	if want := (domain.Scores{IMDb: 7.9, RTCritic: 96, RTAudience: 82}); got != want {
		t.Errorf("Scores(5201) = %+v, want %+v", got, want)
	}
	if reqs := f.Requests(); !slices.Contains(reqs, "/library/metadata/5201?") {
		t.Errorf("want the item's metadata, got:\n%s", strings.Join(reqs, "\n"))
	}
	if got, err := p.Scores(ctx, "7301"); err != nil || got != (domain.Scores{}) { // no Rating list
		t.Errorf("Scores(7301) = %+v, %v; want none", got, err)
	}
	if _, err := p.Scores(ctx, "404"); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("unknown item: err = %v, want ErrNotFound", err)
	}
}

func TestFindHitMakesNoFullLibraryRequest(t *testing.T) {
	p, f, _ := setup(t)
	for _, tc := range []struct {
		guid, path string
		kind       domain.Kind
	}{
		{"tmdb://800201", "/data/media/Movies/Film (2026)", domain.KindMovie},
		{"tmdb://800301", "/data/media/Movies 4K/Film (2025)", domain.KindMovie},
		{"tvdb://900102", "/data/media/TV/Glass Orchard (2022)", domain.KindTV},
		{"tvdb://900101", "", domain.KindTV},
	} {
		if find(t, p, []string{tc.guid}, tc.path, tc.kind, true) == nil {
			t.Fatalf("Find(%s) missed", tc.guid)
		}
	}
	reqs := f.Requests()
	if n := countFull(reqs); n != 0 {
		t.Errorf("hits made %d full library requests:\n%s", n, strings.Join(reqs, "\n"))
	}
}

func TestFindFallsBackToTheFullListingAndCachesIt(t *testing.T) {
	p, f, clock := setup(t)
	old := []string{"tmdb://800100"} // added long ago: not among the recently added
	if got := ratingKey(find(t, p, old, "/data/media/Movies/Old (1999)", domain.KindMovie, false)); got != "6100" {
		t.Fatalf("Find old = %s, want 6100", got)
	}
	reqs := f.Requests()
	if countFull(reqs) != 1 || !slices.ContainsFunc(reqs, func(r string) bool {
		return r == "/library/sections/1/all?includeGuids=1"
	}) {
		t.Errorf("want one full listing of section 1, got:\n%s", strings.Join(reqs, "\n"))
	}

	clock.Advance(30 * time.Second)
	find(t, p, old, "/data/media/Movies/Old (1999)", domain.KindMovie, false)
	if reqs := f.Requests(); len(reqs) != 0 {
		t.Errorf("within every TTL: want no requests, got:\n%s", strings.Join(reqs, "\n"))
	}

	clock.Advance(2 * time.Minute) // recent listing stale, full listing still good
	find(t, p, old, "/data/media/Movies/Old (1999)", domain.KindMovie, false)
	if reqs := f.Requests(); len(reqs) != 1 || countFull(reqs) != 0 {
		t.Errorf("want only the recent listing again, got:\n%s", strings.Join(reqs, "\n"))
	}

	clock.Advance(10 * time.Minute)
	find(t, p, old, "/data/media/Movies/Old (1999)", domain.KindMovie, false)
	if reqs := f.Requests(); countFull(reqs) != 1 {
		t.Errorf("after the TTL: want the full listing again, got:\n%s", strings.Join(reqs, "\n"))
	}

	find(t, p, old, "/data/media/Movies/Old (1999)", domain.KindMovie, true)
	if reqs := f.Requests(); countFull(reqs) != 1 {
		t.Errorf("fresh: want the full listing again, got:\n%s", strings.Join(reqs, "\n"))
	}
}

func TestFindSeesANewImportOnTheNextCheck(t *testing.T) {
	p, f, _ := setup(t)
	guids := []string{"tmdb://800500", "imdb://tt9800500"}
	path := "/data/media/Movies/New Film (2026)"
	if got := find(t, p, guids, path, domain.KindMovie, false); got != nil {
		t.Fatalf("before the scan: found %s", got.RatingKey)
	}
	f.Requests()
	f.add("1", fakeItem{RatingKey: "6500", GUIDs: []string{"tmdb://800500"}, AddedAt: 9000})

	if got := find(t, p, guids, path, domain.KindMovie, false); got != nil {
		t.Errorf("cached miss: found %s before the recent TTL passed", got.RatingKey)
	}
	if got := ratingKey(find(t, p, guids, path, domain.KindMovie, true)); got != "6500" {
		t.Errorf("fresh: Find = %s, want 6500", got)
	}

	// Without fresh, the next check a wait interval later finds it in the recent listing.
	p, f, clock := setup(t)
	find(t, p, guids, path, domain.KindMovie, false)
	f.add("1", fakeItem{RatingKey: "6500", GUIDs: []string{"tmdb://800500"}, AddedAt: 9000})
	f.Requests()
	clock.Advance(3 * time.Minute)
	if got := ratingKey(find(t, p, guids, path, domain.KindMovie, false)); got != "6500" {
		t.Errorf("next check: Find = %s, want 6500", got)
	}
	if reqs := f.Requests(); countFull(reqs) != 0 {
		t.Errorf("next check made a full listing:\n%s", strings.Join(reqs, "\n"))
	}
}

func TestSeasonKey(t *testing.T) {
	p, _, _ := setup(t)
	ctx := t.Context()
	show := &domain.MediaItem{RatingKey: "5201"}
	for season, want := range map[int]string{3: "5203", 1: "5202", 2: ""} {
		got, err := p.SeasonKey(ctx, show, season)
		if err != nil || got != want {
			t.Errorf("SeasonKey(5201, %d) = %q, %v; want %q", season, got, err, want)
		}
	}
	if got, err := p.SeasonKey(ctx, &domain.MediaItem{RatingKey: "5101"}, 0); err != nil || got != "5100" {
		t.Errorf("SeasonKey specials = %q, %v; want 5100", got, err)
	}
	if _, err := p.SeasonKey(ctx, &domain.MediaItem{RatingKey: "404"}, 1); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("deleted show: err = %v, want ErrNotFound", err)
	}
	if got, err := p.SeasonKey(ctx, nil, 1); err != nil || got != "" {
		t.Errorf("nil show: %q, %v", got, err)
	}
}

func TestScanRefreshesTheLibraryHoldingThePath(t *testing.T) {
	p, f, _ := setup(t)
	ctx := t.Context()
	if err := p.Scan(ctx, "/data/media/Movies 4K/Film & Co (2025)"); err != nil {
		t.Fatal(err)
	}
	if err := p.Scan(ctx, "/data/media/TV/Glass Orchard (2022)"); err != nil {
		t.Fatal(err)
	}
	var refreshes []string
	for _, r := range f.Requests() {
		if strings.Contains(r, "/refresh") {
			refreshes = append(refreshes, r)
		}
		if isFullListing(r) {
			t.Errorf("Scan listed a library: %s", r)
		}
	}
	want := []string{
		"/library/sections/2/refresh?path=%2Fmedia%2FMovies+4K%2FFilm+%26+Co+%282025%29",
		"/library/sections/3/refresh?path=%2Fmedia%2FTV%2FGlass+Orchard+%282022%29",
	}
	if !slices.Equal(refreshes, want) {
		t.Errorf("refreshes:\n%s\nwant:\n%s", strings.Join(refreshes, "\n"), strings.Join(want, "\n"))
	}

	if err := p.Scan(ctx, "/elsewhere/Film (2025)"); err != nil {
		t.Errorf("path no library holds: %v", err)
	}
	for _, r := range f.Requests() {
		if strings.Contains(r, "/refresh") {
			t.Errorf("scanned a path no library holds: %s", r)
		}
	}
}

func TestPathMapFirstMatchWins(t *testing.T) {
	p := New("http://plex.example.org", "", []PathMap{
		{From: "/data/media/TV/", To: "/tv/"},
		{From: "/data/media/", To: "/media/"},
	})
	for in, want := range map[string]string{
		"/data/media/TV/Show":     "/tv/Show",
		"/data/media/Movies/Film": "/media/Movies/Film",
		"/other/Film":             "/other/Film",
		"":                        "",
	} {
		if got := p.mapPath(in); got != want {
			t.Errorf("mapPath(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestErrors(t *testing.T) {
	f := newFakePlex(t, libraryFixtures()...)
	ctx := t.Context()

	bad := New(f.URL, "wrong-token", arrToPlex)
	if _, err := bad.Find(ctx, []string{"tmdb://800201"}, "", domain.KindMovie, false); err == nil {
		t.Error("wrong token: Find succeeded")
	}
	if err := bad.Scan(ctx, "/data/media/Movies/Film"); err == nil {
		t.Error("wrong token: Scan succeeded")
	}

	p := New(f.URL, fakeToken, arrToPlex)
	find(t, p, []string{"tmdb://800201"}, "", domain.KindMovie, false)
	f.mu.Lock()
	f.fail = 500
	f.mu.Unlock()
	if _, err := p.Find(ctx, []string{"tmdb://800201"}, "", domain.KindMovie, true); err == nil || strings.Contains(err.Error(), fakeToken) {
		t.Errorf("server error: Find err = %v, want an error without the token", err)
	}
	if _, err := p.SeasonKey(ctx, &domain.MediaItem{RatingKey: "5201"}, 1); err == nil {
		t.Error("server error: SeasonKey succeeded")
	}
}

// TestParity serves each scenario's Plex data from the fake and checks the adapter answers what
// testkit.Media (the fake the parity tests use) answers.
func TestParity(t *testing.T) {
	for _, name := range testkit.Names(t) {
		t.Run(name, func(t *testing.T) {
			sc, _ := testkit.Load(t, name)
			movies := &fakeSection{Key: "1", Type: "movie", Title: "Movies", Locations: []string{"/media/Movies"}}
			tv := &fakeSection{Key: "2", Type: "show", Title: "TV", Locations: []string{"/media/TV"}}
			for guid, it := range sc.Plex {
				s := movies
				if strings.HasPrefix(guid, "tvdb://") {
					s = tv
				}
				fi := fakeItem{
					RatingKey: it.RatingKey, GUIDs: []string{guid}, AddedAt: 1, Seasons: map[int]string{},
					ContentRating: it.ContentRating, Rating: it.Rating,
				}
				for k, season := range sc.PlexSeasons {
					show, idx, _ := strings.Cut(k, ":")
					if n, err := strconv.Atoi(idx); err == nil && show == it.RatingKey {
						fi.Seasons[n] = season
					}
				}
				s.Items = append(s.Items, fi)
			}
			f := newFakePlex(t, movies, tv)
			f.machine, f.name = sc.PlexServer.ID, sc.PlexServer.Name
			p := New(f.URL, fakeToken, arrToPlex)
			want := testkit.Media{Sc: sc}
			ctx := t.Context()

			guids := slices.Sorted(func(yield func(string) bool) {
				for g := range sc.Plex {
					if !yield(g) {
						return
					}
				}
			})
			for _, g := range append(guids, "tmdb://1", "tvdb://1") {
				kind := domain.KindMovie
				if strings.HasPrefix(g, "tvdb://") {
					kind = domain.KindTV
				}
				got, err := p.Find(ctx, []string{g}, "", kind, false)
				if err != nil {
					t.Fatal(err)
				}
				exp, _ := want.Find(ctx, []string{g}, "", kind, false)
				if (got == nil) != (exp == nil) || got != nil && *got != *exp {
					t.Errorf("Find(%s) = %+v, want %+v", g, got, exp)
					continue
				}
				if got == nil || kind != domain.KindTV {
					continue
				}
				for season := range 6 {
					gk, err := p.SeasonKey(ctx, got, season)
					ek, _ := want.SeasonKey(ctx, exp, season)
					if err != nil || gk != ek {
						t.Errorf("SeasonKey(%s, %d) = %q, %v; want %q", got.RatingKey, season, gk, err, ek)
					}
				}
			}
			for _, it := range sc.Plex {
				gs, err := p.Scores(ctx, it.RatingKey)
				es, _ := want.Scores(ctx, it.RatingKey)
				if err != nil || gs != es {
					t.Errorf("Scores(%s) = %+v, %v; want %+v", it.RatingKey, gs, err, es)
				}
				if p.URL(it.RatingKey) != want.URL(it.RatingKey) {
					t.Errorf("URL(%s) = %s, want %s", it.RatingKey, p.URL(it.RatingKey), want.URL(it.RatingKey))
				}
			}
			if p.Name() != want.Name() {
				t.Errorf("Name = %q, want %q", p.Name(), want.Name())
			}
		})
	}
}
