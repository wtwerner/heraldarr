package arr

import (
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/wtwerner/heraldarr/internal/domain"
)

// DefaultTimeout bounds one API request when NewClient is given none.
const DefaultTimeout = 20 * time.Second

// maxBody caps a response; the largest real ones (credits of an ensemble film) are a few hundred KB.
const maxBody = 16 << 20

// KeyFunc returns the API key. The client calls it for every request, so a rotated key is picked up.
type KeyFunc func() (string, error)

// StaticKey is a KeyFunc for an api_key from the config.
func StaticKey(key string) KeyFunc {
	return func() (string, error) { return key, nil }
}

// ConfigXMLKey is a KeyFunc for a config_xml source: the key is read from the file on every request.
func ConfigXMLKey(path string) KeyFunc {
	return func() (string, error) { return APIKeyFromConfigXML(path) }
}

// APIKeyFromConfigXML reads <ApiKey> from a Sonarr or Radarr config.xml.
func APIKeyFromConfigXML(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("api key: %w", err)
	}
	var cfg struct {
		APIKey string `xml:"ApiKey"`
	}
	if err := xml.Unmarshal(b, &cfg); err != nil {
		return "", fmt.Errorf("api key: %s: %w", path, err)
	}
	key := strings.TrimSpace(cfg.APIKey)
	if key == "" {
		return "", fmt.Errorf("api key: %s has no <ApiKey>", path)
	}
	return key, nil
}

// StatusError is a response other than 200 and 404 (404 is domain.ErrNotFound).
type StatusError struct {
	Path   string // e.g. "series/102"
	Code   int
	Status string // "500 Internal Server Error"
}

func (e *StatusError) Error() string { return fmt.Sprintf("GET %s: %s", e.Path, e.Status) }

// Client reads one Sonarr or Radarr instance through its v3 API. It implements domain.ArrClient.
type Client struct {
	base string
	key  KeyFunc
	ua   string
	http *http.Client
}

// NewClient returns a client for the instance at baseURL (with its URL base, if any). timeout
// bounds each request; 0 means DefaultTimeout. version goes into the User-Agent.
func NewClient(baseURL string, key KeyFunc, version string, timeout time.Duration) *Client {
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	return &Client{
		base: strings.TrimRight(baseURL, "/") + "/api/v3/",
		key:  key,
		ua:   "heraldarr/" + version,
		http: &http.Client{
			Timeout: timeout,
			// Never follow: Go would forward X-Api-Key to the target, even on another host. The API
			// doesn't redirect, so a redirect is a proxy's login page: a StatusError says so.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}
}

var _ domain.ArrClient = (*Client)(nil)

// Series reads GET /api/v3/series/{id}.
func (c *Client) Series(ctx context.Context, id int) (*domain.SeriesDetail, error) {
	path := "series/" + strconv.Itoa(id)
	body, err := c.get(ctx, path)
	if err != nil {
		return nil, err
	}
	d, err := DecodeSeries(body)
	if err != nil {
		return nil, fmt.Errorf("GET %s: %w", path, err)
	}
	return d, nil
}

// Movie reads GET /api/v3/movie/{id} and its credits. A credits failure is logged and leaves
// Directors and Cast empty, unless ctx is done: then Movie returns ctx's error.
func (c *Client) Movie(ctx context.Context, id int) (*domain.MovieDetail, error) {
	path := "movie/" + strconv.Itoa(id)
	body, err := c.get(ctx, path)
	if err != nil {
		return nil, err
	}
	credits, err := c.get(ctx, "credit?movieId="+strconv.Itoa(id))
	if err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("GET %s: %w", path, ctx.Err())
		}
		slog.WarnContext(ctx, "arr: movie credits unavailable", "movie", id, "err", err)
		credits = nil
	}
	d, err := DecodeMovie(body, credits)
	if err != nil {
		return nil, fmt.Errorf("GET %s: %w", path, err)
	}
	return d, nil
}

// Queue reads GET /api/v3/queue/details?seriesId={id}: one record per grabbed episode.
func (c *Client) Queue(ctx context.Context, seriesID int) ([]domain.QueueItem, error) {
	path := "queue/details?seriesId=" + strconv.Itoa(seriesID) + "&includeEpisode=true"
	body, err := c.get(ctx, path)
	if err != nil {
		return nil, err
	}
	q, err := DecodeQueue(body)
	if err != nil {
		return nil, fmt.Errorf("GET %s: %w", path, err)
	}
	return q, nil
}

// get returns the body of a 200 response to GET /api/v3/{path}.
func (c *Client) get(ctx context.Context, path string) ([]byte, error) {
	key, err := c.key()
	if err != nil {
		return nil, fmt.Errorf("GET %s: %w", path, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+path, http.NoBody)
	if err != nil {
		return nil, fmt.Errorf("GET %s: %w", path, err)
	}
	req.Header.Set("X-Api-Key", key)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", c.ua)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err // *url.Error: method, URL (no key: that's a header) and cause
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		// Drain a little so the connection is reused: 404 (a deleted item) is routine.
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
		if resp.StatusCode == http.StatusNotFound {
			return nil, fmt.Errorf("GET %s: %w", path, domain.ErrNotFound)
		}
		return nil, &StatusError{Path: path, Code: resp.StatusCode, Status: resp.Status}
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	if err != nil {
		return nil, fmt.Errorf("GET %s: %w", path, err)
	}
	if len(body) > maxBody {
		return nil, fmt.Errorf("GET %s: response over %d bytes", path, maxBody)
	}
	return body, nil
}
