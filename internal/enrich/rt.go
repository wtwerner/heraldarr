// Package enrich adds links that the *arr APIs don't know: the exact Rotten Tomatoes page of a
// movie or series, found through Wikidata. Reference: rotten_tomatoes in the reference
// implementation. Lookups are cached through domain.Store; time comes from domain.Clock.
package enrich

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/wtwerner/heraldarr/internal/domain"
)

const (
	// WikidataEndpoint is the public SPARQL endpoint.
	WikidataEndpoint = "https://query.wikidata.org/sparql"
	// UserAgent follows the Wikimedia policy: name the client and say where to find its operator.
	UserAgent = "heraldarr/1.0 (+https://github.com/wtwerner/heraldarr; Sonarr/Radarr Discord notifier)"

	rtBase    = "https://www.rottentomatoes.com/"
	cacheNS   = "rt"
	missRetry = 7 * 24 * time.Hour // a cached miss is asked again after this; a hit is kept forever
)

// seasonSuffix: some IMDb IDs sit on a season item ("tv/show/s01"); the card links the show.
var seasonSuffix = regexp.MustCompile(`/s\d+$`)

// Wikidata finds Rotten Tomatoes pages by IMDb ID (P345 -> P1258).
type Wikidata struct {
	Endpoint  string
	UserAgent string
	Timeout   time.Duration // per lookup
	HTTP      *http.Client

	store domain.Store
	clock domain.Clock
}

// New returns a lookup against the public Wikidata endpoint, caching in store (namespace "rt").
func New(store domain.Store, clock domain.Clock) *Wikidata {
	return &Wikidata{
		Endpoint:  WikidataEndpoint,
		UserAgent: UserAgent,
		Timeout:   15 * time.Second,
		HTTP:      http.DefaultClient,
		store:     store,
		clock:     clock,
	}
}

// RottenTomatoes returns the exact Rotten Tomatoes page for imdbID, or a search link for title
// when the page is unknown or the lookup fails. It never fails: a missing link must never block
// a post.
func (w *Wikidata) RottenTomatoes(ctx context.Context, imdbID, title string) string {
	search := rtBase + "search?" + url.Values{"search": {title}}.Encode()
	if imdbID == "" {
		return search
	}
	link := func(path string) string {
		if path == "" {
			return search
		}
		return rtBase + path
	}

	val, at, ok, err := w.store.CacheGet(ctx, cacheNS, imdbID)
	switch {
	case err != nil:
		slog.Warn("rotten tomatoes: cache read failed, asking wikidata", "imdb", imdbID, "err", err)
	case ok && (len(val) > 0 || w.clock.Now().Sub(at) < missRetry):
		return link(string(val))
	}

	path, err := w.lookup(ctx, imdbID)
	if err != nil {
		slog.Warn("rotten tomatoes: wikidata lookup failed", "imdb", imdbID, "err", err)
		return search
	}
	if err := w.store.CachePut(ctx, cacheNS, imdbID, []byte(path), w.clock.Now()); err != nil {
		slog.Warn("rotten tomatoes: cache write failed", "imdb", imdbID, "err", err)
	}
	return link(path)
}

// sparqlResult is the part of a SPARQL JSON result the lookup reads.
type sparqlResult struct {
	Results *struct {
		Bindings []struct {
			RT struct {
				Value string `json:"value"`
			} `json:"rt"`
		} `json:"bindings"`
	} `json:"results"`
}

// lookup returns the RT path ("m/example", "tv/example") for imdbID, "" if Wikidata has none.
func (w *Wikidata) lookup(ctx context.Context, imdbID string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, w.Timeout)
	defer cancel()

	query := fmt.Sprintf(`SELECT ?rt WHERE { ?s wdt:P345 %s . ?s wdt:P1258 ?rt } LIMIT 1`, sparqlString(imdbID))
	u := w.Endpoint + "?" + url.Values{"query": {query}, "format": {"json"}}.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, http.NoBody)
	if err != nil {
		return "", fmt.Errorf("wikidata: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", w.UserAgent)
	resp, err := w.HTTP.Do(req)
	if err != nil {
		return "", fmt.Errorf("wikidata: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("wikidata: %s", resp.Status)
	}
	var res sparqlResult
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		return "", fmt.Errorf("wikidata: decoding result: %w", err)
	}
	if res.Results == nil {
		return "", errors.New("wikidata: result has no \"results\"")
	}
	if len(res.Results.Bindings) == 0 {
		return "", nil
	}
	return seasonSuffix.ReplaceAllString(res.Results.Bindings[0].RT.Value, ""), nil
}

// sparqlString quotes s as a SPARQL string literal.
func sparqlString(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`, "\r", `\r`).Replace(s) + `"`
}
