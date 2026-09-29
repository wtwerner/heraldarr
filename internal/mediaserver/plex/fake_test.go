package plex

import (
	"cmp"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
)

const (
	fakeToken   = "test-token"
	fakeMachine = "0123456789abcdef0123456789abcdef01234567"
	fakeName    = "Example Server"
)

// fakeItem is a library item as the fake Plex stores it.
type fakeItem struct {
	RatingKey           string
	GUIDs               []string
	AddedAt             int64 // movies: the item; shows: the newest episode (episode.addedAt)
	AudienceRating      float64
	AudienceRatingImage string
	ContentRating       string
	Seasons             map[int]string // index -> season rating key (shows)
}

type fakeSection struct {
	Key, Type, Title string
	Locations        []string
	Items            []fakeItem
}

// fakePlex is an httptest Plex Media Server: enough of /, /library/sections, …/all (with sort,
// type and container paging), …/refresh and /library/metadata/<key>/children.
type fakePlex struct {
	*httptest.Server
	t testing.TB

	mu       sync.Mutex
	sections []*fakeSection
	requests []string // path?query, in order
	fail     int      // status to answer everything with; 0: behave
	machine  string   // machineIdentifier
	name     string   // friendlyName
}

func newFakePlex(t testing.TB, sections ...*fakeSection) *fakePlex {
	f := &fakePlex{t: t, sections: sections, machine: fakeMachine, name: fakeName}
	f.Server = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.Close)
	return f
}

// libraryFixtures: Movies and Movies 4K share a title (with different rating keys), 60 older
// filler movies push the oldest ones out of the recently added listing, and a music library that
// Find must ignore.
func libraryFixtures() []*fakeSection {
	movies := &fakeSection{Key: "1", Type: "movie", Title: "Movies", Locations: []string{"/media/Movies"}, Items: []fakeItem{
		{RatingKey: "6201", GUIDs: []string{"plex://movie/aaa", "imdb://tt9800201", "tmdb://800201"}, AddedAt: 2000, AudienceRating: 7.9, AudienceRatingImage: "themoviedb://image.rating", ContentRating: "PG-13"},
		{RatingKey: "7300", GUIDs: []string{"imdb://tt9800301", "tmdb://800301"}, AddedAt: 1990},
		{RatingKey: "6100", GUIDs: []string{"imdb://tt9800100", "tmdb://800100"}, AddedAt: 1}, // oldest: only in the full listing
	}}
	for i := range 60 {
		movies.Items = append(movies.Items, fakeItem{RatingKey: strconv.Itoa(9000 + i), GUIDs: []string{"tmdb://" + strconv.Itoa(700000+i)}, AddedAt: int64(100 + i)})
	}
	movies4k := &fakeSection{Key: "2", Type: "movie", Title: "Movies 4K", Locations: []string{"/media/Movies 4K/"}, Items: []fakeItem{
		{RatingKey: "7301", GUIDs: []string{"imdb://tt9800301", "tmdb://800301"}, AddedAt: 2100, AudienceRating: 8.2, AudienceRatingImage: "rottentomatoes://image.rating.upright", ContentRating: "R"},
	}}
	tv := &fakeSection{Key: "3", Type: "show", Title: "TV Shows", Locations: []string{"/media/TV"}, Items: []fakeItem{
		{RatingKey: "5201", GUIDs: []string{"plex://show/bbb", "imdb://tt9900102", "tmdb://99102", "tvdb://900102"}, AddedAt: 3000, AudienceRating: 8.4, AudienceRatingImage: "themoviedb://image.rating", ContentRating: "TV-MA", Seasons: map[int]string{1: "5202", 3: "5203"}},
		{RatingKey: "5101", GUIDs: []string{"tvdb://900101"}, AddedAt: 2500, Seasons: map[int]string{0: "5100", 1: "5102"}},
	}}
	music := &fakeSection{Key: "4", Type: "artist", Title: "Music", Locations: []string{"/media/Music"}, Items: []fakeItem{
		{RatingKey: "8001", GUIDs: []string{"tmdb://800201"}}, // same GUID as a movie: must never match
	}}
	return []*fakeSection{movies, movies4k, tv, music}
}

func (f *fakePlex) add(sectionKey string, it fakeItem) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, s := range f.sections {
		if s.Key == sectionKey {
			s.Items = append(s.Items, it)
			return
		}
	}
	f.t.Fatalf("no section %s", sectionKey)
}

// Requests returns every request so far and forgets them.
func (f *fakePlex) Requests() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	r := f.requests
	f.requests = nil
	return r
}

// isFullListing: a …/all request that isn't the paged, sorted recently added one.
func isFullListing(req string) bool {
	path, query, _ := strings.Cut(req, "?")
	return strings.HasSuffix(path, "/all") && !strings.Contains(query, "sort=")
}

func countFull(reqs []string) int {
	n := 0
	for _, r := range reqs {
		if isFullListing(r) {
			n++
		}
	}
	return n
}

func (f *fakePlex) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, r.URL.Path+"?"+r.URL.RawQuery)
	if r.Header.Get("X-Plex-Token") != fakeToken {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if f.fail != 0 {
		http.Error(w, "broken", f.fail)
		return
	}
	if r.Header.Get("Accept") != "application/json" {
		http.Error(w, "fake only speaks JSON", http.StatusNotAcceptable)
		return
	}
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	switch {
	case r.URL.Path == "/":
		f.reply(w, map[string]any{"machineIdentifier": f.machine, "friendlyName": f.name, "size": 0})
	case r.URL.Path == "/library/sections":
		dirs := []map[string]any{}
		for _, s := range f.sections {
			locs := []map[string]any{}
			for i, l := range s.Locations {
				locs = append(locs, map[string]any{"id": i + 1, "path": l})
			}
			dirs = append(dirs, map[string]any{"key": s.Key, "type": s.Type, "title": s.Title, "Location": locs})
		}
		f.reply(w, map[string]any{"size": len(dirs), "Directory": dirs})
	case len(parts) == 4 && parts[1] == "sections" && parts[3] == "all":
		f.all(w, r, f.section(parts[2]))
	case len(parts) == 4 && parts[1] == "sections" && parts[3] == "refresh":
		if f.section(parts[2]) == nil || r.URL.Query().Get("path") == "" {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(http.StatusOK)
	case len(parts) == 4 && parts[1] == "metadata" && parts[3] == "children":
		it := f.item(parts[2])
		if it == nil {
			http.NotFound(w, r)
			return
		}
		seasons := []map[string]any{}
		for idx, key := range it.Seasons {
			seasons = append(seasons, map[string]any{"ratingKey": key, "index": idx, "type": "season", "parentRatingKey": it.RatingKey})
		}
		slices.SortFunc(seasons, func(a, b map[string]any) int { return cmp.Compare(a["index"].(int), b["index"].(int)) })
		f.reply(w, map[string]any{"size": len(seasons), "Metadata": seasons})
	default:
		http.NotFound(w, r)
	}
}

func (f *fakePlex) section(key string) *fakeSection {
	for _, s := range f.sections {
		if s.Key == key {
			return s
		}
	}
	return nil
}

func (f *fakePlex) item(ratingKey string) *fakeItem {
	for _, s := range f.sections {
		for i := range s.Items {
			if s.Items[i].RatingKey == ratingKey {
				return &s.Items[i]
			}
		}
	}
	return nil
}

func (f *fakePlex) all(w http.ResponseWriter, r *http.Request, s *fakeSection) {
	if s == nil {
		http.NotFound(w, r)
		return
	}
	q := r.URL.Query()
	items := slices.Clone(s.Items)
	switch sort := q.Get("sort"); sort {
	case "":
	case "addedAt:desc", "episode.addedAt:desc":
		if (sort == "addedAt:desc") != (s.Type == "movie") {
			f.t.Errorf("sort %s on a %s library", sort, s.Type)
		}
		slices.SortStableFunc(items, func(a, b fakeItem) int { return cmp.Compare(b.AddedAt, a.AddedAt) })
	default:
		f.t.Errorf("unexpected sort %q", sort)
	}
	if typ := q.Get("type"); typ != "" && typ != map[string]string{"movie": "1", "show": "2"}[s.Type] {
		f.t.Errorf("type %s on a %s library", typ, s.Type)
	}
	if size := q.Get("X-Plex-Container-Size"); size != "" {
		start, _ := strconv.Atoi(q.Get("X-Plex-Container-Start"))
		n, _ := strconv.Atoi(size)
		items = items[min(start, len(items)):min(start+n, len(items))]
	}
	meta := []map[string]any{}
	for _, it := range items {
		m := map[string]any{
			"ratingKey": it.RatingKey, "key": "/library/metadata/" + it.RatingKey, "type": s.Type,
			"guid": "plex://" + s.Type + "/" + it.RatingKey, "addedAt": it.AddedAt,
		}
		if it.AudienceRating != 0 {
			m["audienceRating"] = it.AudienceRating
			m["audienceRatingImage"] = it.AudienceRatingImage
		}
		if it.ContentRating != "" {
			m["contentRating"] = it.ContentRating
		}
		if q.Get("includeGuids") == "1" {
			guids := []map[string]string{}
			for _, g := range it.GUIDs {
				guids = append(guids, map[string]string{"id": g})
			}
			m["Guid"] = guids
		}
		meta = append(meta, m)
	}
	f.reply(w, map[string]any{"size": len(meta), "totalSize": len(s.Items), "Metadata": meta})
}

func (f *fakePlex) reply(w http.ResponseWriter, container map[string]any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(map[string]any{"MediaContainer": container}); err != nil {
		f.t.Error(err)
	}
}
