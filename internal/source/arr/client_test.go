package arr_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wtwerner/heraldarr/internal/domain"
	"github.com/wtwerner/heraldarr/internal/source/arr"
	"github.com/wtwerner/heraldarr/internal/testkit"
)

// fakeArr serves canned bodies by request URI and records what it was asked.
type fakeArr struct {
	mu     sync.Mutex
	bodies map[string]string // "/api/v3/series/102" -> body
	status map[string]int    // overrides 200
	reqs   []*http.Request
}

func (f *fakeArr) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.reqs = append(f.reqs, r)
	body, ok := f.bodies[r.URL.RequestURI()]
	code, set := f.status[r.URL.RequestURI()]
	f.mu.Unlock()
	switch {
	case set:
		w.WriteHeader(code)
		_, _ = w.Write([]byte(`{"message":"canned"}`))
	case !ok:
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"message":"NotFound"}`))
	default:
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}
}

func (f *fakeArr) requests() []*http.Request {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.reqs
}

func serve(t *testing.T, f *fakeArr) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return srv
}

func newClient(url string) *arr.Client {
	return arr.NewClient(url, arr.StaticKey("k3y"), "1.2.3", time.Second)
}

func TestClientSeries(t *testing.T) {
	sc, _ := testkit.Load(t, "tv_weekly_episode")
	f := &fakeArr{bodies: map[string]string{"/api/v3/series/102": string(sc.Arr["series/102"])}}
	srv := serve(t, f)

	got, err := newClient(srv.URL).Series(context.Background(), 102)
	if err != nil {
		t.Fatal(err)
	}
	want, _ := arr.DecodeSeries(sc.Arr["series/102"])
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v, want %+v", got, want)
	}

	r := f.requests()[0]
	for h, want := range map[string]string{
		"X-Api-Key": "k3y", "User-Agent": "heraldarr/1.2.3", "Accept": "application/json",
	} {
		if got := r.Header.Get(h); got != want {
			t.Errorf("%s: got %q, want %q", h, got, want)
		}
	}
	if r.Method != http.MethodGet {
		t.Errorf("method %s", r.Method)
	}
	if r.URL.Query().Get("apikey") != "" {
		t.Error("the key belongs in the header, never the URL")
	}
}

func TestClientMovie(t *testing.T) {
	sc, _ := testkit.Load(t, "movie_single")
	f := &fakeArr{bodies: map[string]string{
		"/api/v3/movie/201":          string(sc.Arr["movie/201"]),
		"/api/v3/credit?movieId=201": string(sc.Arr["credit?movieId=201"]),
	}}
	srv := serve(t, f)

	got, err := newClient(srv.URL).Movie(context.Background(), 201)
	if err != nil {
		t.Fatal(err)
	}
	want, _ := arr.DecodeMovie(sc.Arr["movie/201"], sc.Arr["credit?movieId=201"])
	if !reflect.DeepEqual(got, want) || len(got.Cast) == 0 || len(got.Directors) == 0 {
		t.Errorf("got %+v, want %+v", got, want)
	}
	for _, r := range f.requests() {
		if r.Header.Get("X-Api-Key") != "k3y" {
			t.Errorf("%s: no key", r.URL)
		}
	}
}

// Credits are decoration: whatever goes wrong fetching them, the movie still comes back.
func TestClientMovieCreditsFail(t *testing.T) {
	sc, _ := testkit.Load(t, "movie_single")
	movie := string(sc.Arr["movie/201"])
	for name, f := range map[string]*fakeArr{
		"500": {
			bodies: map[string]string{"/api/v3/movie/201": movie},
			status: map[string]int{"/api/v3/credit?movieId=201": http.StatusInternalServerError},
		},
		"404":      {bodies: map[string]string{"/api/v3/movie/201": movie}},
		"bad json": {bodies: map[string]string{"/api/v3/movie/201": movie, "/api/v3/credit?movieId=201": `{"oops"`}},
		"not a list": {bodies: map[string]string{
			"/api/v3/movie/201": movie, "/api/v3/credit?movieId=201": `{"personName":"Lena Marsh"}`,
		}},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := newClient(serve(t, f).URL).Movie(context.Background(), 201)
			if err != nil {
				t.Fatal(err)
			}
			if got.Directors != nil || got.Cast != nil {
				t.Errorf("want no credits, got %v / %v", got.Directors, got.Cast)
			}
			if got.IMDbRating != 7.4 || got.RuntimeMin != 124 {
				t.Errorf("movie details lost: %+v", got)
			}
		})
	}
}

func TestClientNotFound(t *testing.T) {
	srv := serve(t, &fakeArr{})
	c := newClient(srv.URL)
	if _, err := c.Series(context.Background(), 7); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("series: got %v, want ErrNotFound", err)
	}
	if _, err := c.Movie(context.Background(), 7); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("movie: got %v, want ErrNotFound", err)
	}
}

func TestClientHTTPErrors(t *testing.T) {
	for _, code := range []int{http.StatusUnauthorized, http.StatusInternalServerError, http.StatusBadGateway} {
		f := &fakeArr{status: map[string]int{"/api/v3/series/7": code, "/api/v3/movie/7": code}}
		c := newClient(serve(t, f).URL)
		for name, call := range map[string]func() error{
			"series": func() error { _, err := c.Series(context.Background(), 7); return err },
			"movie":  func() error { _, err := c.Movie(context.Background(), 7); return err },
		} {
			err := call()
			var se *arr.StatusError
			if !errors.As(err, &se) || se.Code != code {
				t.Errorf("%s %d: got %v, want a StatusError", name, code, err)
			}
			if errors.Is(err, domain.ErrNotFound) {
				t.Errorf("%s %d: must not be ErrNotFound", name, code)
			}
			if !strings.Contains(err.Error(), http.StatusText(code)) || strings.Contains(err.Error(), "k3y") {
				t.Errorf("%s %d: error %q should name the status and hide the key", name, code, err)
			}
		}
	}
}

func TestClientBadBody(t *testing.T) {
	f := &fakeArr{bodies: map[string]string{"/api/v3/series/7": `<html>login</html>`, "/api/v3/movie/7": `[`}}
	c := newClient(serve(t, f).URL)
	if _, err := c.Series(context.Background(), 7); err == nil {
		t.Error("series: want a decode error")
	}
	if _, err := c.Movie(context.Background(), 7); err == nil {
		t.Error("movie: want a decode error")
	}
}

func TestClientUnreachable(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close()
	if _, err := newClient(url).Series(context.Background(), 1); err == nil || errors.Is(err, domain.ErrNotFound) {
		t.Errorf("got %v, want a connection error", err)
	}
}

func TestClientTimeout(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(release) })

	c := arr.NewClient(srv.URL, arr.StaticKey("k"), "dev", 50*time.Millisecond)
	start := time.Now()
	if _, err := c.Series(context.Background(), 1); err == nil {
		t.Fatal("want a timeout error")
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("timeout not applied: took %v", d)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	c = arr.NewClient(srv.URL, arr.StaticKey("k"), "dev", time.Minute)
	if _, err := c.Series(ctx, 1); !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled context: got %v", err)
	}
}

// The base URL may carry a URL base (reverse proxy) and a trailing slash.
func TestClientBaseURL(t *testing.T) {
	sc, _ := testkit.Load(t, "tv_weekly_episode")
	f := &fakeArr{bodies: map[string]string{"/sonarr/api/v3/series/102": string(sc.Arr["series/102"])}}
	srv := serve(t, f)
	for _, base := range []string{srv.URL + "/sonarr", srv.URL + "/sonarr/"} {
		if _, err := newClient(base).Series(context.Background(), 102); err != nil {
			t.Errorf("%s: %v", base, err)
		}
	}
}

const configXML = `<Config>
  <BindAddress>*</BindAddress>
  <Port>8989</Port>
  <ApiKey>%s</ApiKey>
  <AuthenticationMethod>Forms</AuthenticationMethod>
</Config>
`

func writeConfig(t *testing.T, path, key string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(strings.Replace(configXML, "%s", key, 1)), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestAPIKeyFromConfigXML(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.xml")
	writeConfig(t, path, "0123456789abcdef0123456789abcdef")
	if got, err := arr.APIKeyFromConfigXML(path); err != nil || got != "0123456789abcdef0123456789abcdef" {
		t.Errorf("got %q, %v", got, err)
	}

	writeConfig(t, path, "  padded  ")
	if got, err := arr.APIKeyFromConfigXML(path); err != nil || got != "padded" {
		t.Errorf("padded: got %q, %v", got, err)
	}

	for name, body := range map[string]string{
		"no key":    "<Config><Port>8989</Port></Config>",
		"empty key": "<Config><ApiKey>  </ApiKey></Config>",
		"not xml":   "ApiKey=abc",
	} {
		p := filepath.Join(dir, strings.ReplaceAll(name, " ", "_")+".xml")
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if got, err := arr.APIKeyFromConfigXML(p); err == nil {
			t.Errorf("%s: got %q, want an error", name, got)
		}
	}
	if _, err := arr.APIKeyFromConfigXML(filepath.Join(dir, "missing.xml")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("missing file: got %v", err)
	}
}

// config_xml sources read the key on every request, so rotating it in Sonarr needs no restart.
func TestClientConfigXMLKeyRotation(t *testing.T) {
	sc, _ := testkit.Load(t, "tv_weekly_episode")
	f := &fakeArr{bodies: map[string]string{"/api/v3/series/102": string(sc.Arr["series/102"])}}
	srv := serve(t, f)
	path := filepath.Join(t.TempDir(), "config.xml")
	c := arr.NewClient(srv.URL, arr.ConfigXMLKey(path), "dev", time.Second)

	writeConfig(t, path, "first")
	if _, err := c.Series(context.Background(), 102); err != nil {
		t.Fatal(err)
	}
	writeConfig(t, path, "second")
	if _, err := c.Series(context.Background(), 102); err != nil {
		t.Fatal(err)
	}
	reqs := f.requests()
	if reqs[0].Header.Get("X-Api-Key") != "first" || reqs[1].Header.Get("X-Api-Key") != "second" {
		t.Errorf("keys sent: %q, %q", reqs[0].Header.Get("X-Api-Key"), reqs[1].Header.Get("X-Api-Key"))
	}

	// An unreadable key fails the request before anything is sent.
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Series(context.Background(), 102); err == nil || errors.Is(err, domain.ErrNotFound) {
		t.Errorf("missing config.xml: got %v", err)
	}
	if len(f.requests()) != 2 {
		t.Errorf("sent %d requests, want 2", len(f.requests()))
	}
}

// A redirect (a proxy's login page, another host) is never followed, so the key never leaves.
func TestClientRedirectNotFollowed(t *testing.T) {
	elsewhere := &fakeArr{}
	target := serve(t, elsewhere)
	for _, code := range []int{http.StatusFound, http.StatusTemporaryRedirect} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, target.URL+"/login", code)
		}))
		t.Cleanup(srv.Close)
		_, err := newClient(srv.URL).Series(context.Background(), 1)
		var se *arr.StatusError
		if !errors.As(err, &se) || se.Code != code {
			t.Errorf("%d: got %v, want a StatusError", code, err)
		}
	}
	if n := len(elsewhere.requests()); n != 0 {
		t.Errorf("redirect followed %d times", n)
	}
}

// Cancelled while fetching credits: the caller is going away, so Movie fails rather than
// returning a movie without credits.
func TestClientMovieCancelledDuringCredits(t *testing.T) {
	sc, _ := testkit.Load(t, "movie_single")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v3/movie/201" {
			_, _ = w.Write(sc.Arr["movie/201"])
			return
		}
		cancel()
		<-r.Context().Done()
	}))
	t.Cleanup(srv.Close)
	if _, err := newClient(srv.URL).Movie(ctx, 201); !errors.Is(err, context.Canceled) {
		t.Errorf("got %v, want context.Canceled", err)
	}
}

var _ domain.ArrClient = (*arr.Client)(nil)

// The queue is one record per grabbed episode (a season pack is one per episode, same download);
// downloads that won't import on their own are left out.
func TestClientQueue(t *testing.T) {
	f := &fakeArr{bodies: map[string]string{"/api/v3/queue/details?seriesId=102&includeEpisode=true": `[
		{"id":1,"seriesId":102,"episodeId":501,"seasonNumber":2,"status":"queued","trackedDownloadState":"downloading","downloadId":"A",
		 "episode":{"id":501,"airDateUtc":"2011-04-18T01:00:00Z"}},
		{"id":2,"seriesId":102,"episodeId":502,"seasonNumber":2,"status":"queued","trackedDownloadState":"importPending","downloadId":"A"},
		{"id":3,"seriesId":102,"seasonNumber":3,"status":"queued","downloadId":"B"},
		{"id":4,"seriesId":102,"episodeId":601,"seasonNumber":3,"status":"warning","trackedDownloadState":"downloading","downloadId":"C"},
		{"id":5,"seriesId":102,"episodeId":602,"seasonNumber":3,"status":"completed","trackedDownloadState":"importBlocked","downloadId":"D"},
		{"id":6,"seriesId":102,"episodeId":603,"seasonNumber":3,"status":"failed","trackedDownloadState":"downloading","downloadId":"E"},
		{"id":7,"seriesId":102,"episodeId":604,"seasonNumber":3,"status":"completed","trackedDownloadState":"failedPending","downloadId":"F"},
		{"id":8,"seriesId":102,"episodeId":605,"seasonNumber":3,"status":"completed","trackedDownloadState":"ignored","downloadId":"G"}
	]`}}
	srv := serve(t, f)
	got, err := newClient(srv.URL).Queue(context.Background(), 102)
	if err != nil {
		t.Fatal(err)
	}
	want := []domain.QueueItem{
		{EpisodeID: 501, Season: 2, Aired: time.Date(2011, 4, 18, 1, 0, 0, 0, time.UTC)},
		{EpisodeID: 502, Season: 2},
		{EpisodeID: 601, Season: 3},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v, want %+v", got, want)
	}
	if _, err := newClient(srv.URL).Queue(context.Background(), 7); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("unknown series: %v, want ErrNotFound", err)
	}
}
