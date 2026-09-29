package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/wtwerner/heraldarr/internal/domain"
)

// Imported counts the rows ImportLegacy wrote.
type Imported struct {
	Batches, Posted, Cache, History int
}

// ImportLegacy loads the reference implementation's data folder into the store, so a cutover
// keeps the reannounce ledger, the Rotten Tomatoes cache, waiting batches and the post history:
//
//   - posted.json: {"<item key>": unix seconds}
//   - rt_cache.json: {"<imdb id>": {"id": "m/x" or null, "at": unix seconds}}, stored in the "rt"
//     namespace with the RT path as a JSON value ("m/x" or null for a lookup that found nothing)
//   - pending.json: batches, as the reference kept them
//   - history.jsonl: one post per line
//
// Missing files are skipped; a folder with none of them is an error. The import is one
// transaction, and it never overwrites newer state: a batch already in the store is kept, and
// posted and cache entries keep the later timestamp. History is appended, so import once.
func (s *Store) ImportLegacy(ctx context.Context, dir string) (Imported, error) {
	var n Imported
	if fi, err := os.Stat(dir); err != nil {
		return n, fmt.Errorf("store: import %s: %w", dir, err)
	} else if !fi.IsDir() {
		return n, fmt.Errorf("store: import %s: not a directory", dir)
	}
	var (
		posted  map[string]float64
		rt      map[string]legacyRT
		pending map[string]legacyBatch
		history []legacyPost
	)
	found := 0
	for name, into := range map[string]func([]byte) error{
		"posted.json":   func(b []byte) error { return json.Unmarshal(b, &posted) },
		"rt_cache.json": func(b []byte) error { return json.Unmarshal(b, &rt) },
		"pending.json":  func(b []byte) error { return json.Unmarshal(b, &pending) },
		"history.jsonl": func(b []byte) (err error) { history, err = parseHistory(b); return err },
	} {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err == nil {
			err = into(data)
		}
		if err != nil {
			return n, fmt.Errorf("store: import %s: %w", filepath.Join(dir, name), err)
		}
		found++
	}
	if found == 0 {
		return n, fmt.Errorf("store: import %s: no reference data (posted.json, rt_cache.json, pending.json, history.jsonl)", dir)
	}
	batches := make([]*domain.Batch, 0, len(pending))
	for key, lb := range pending {
		b, err := lb.batch(key)
		if err != nil {
			return n, fmt.Errorf("store: import %s: pending.json: %w", dir, err)
		}
		batches = append(batches, b)
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return n, fmt.Errorf("store: import %s: %w", dir, err)
	}
	defer func() { _ = tx.Rollback() }()
	exec := func(count *int, query string, args ...any) error {
		res, err := tx.ExecContext(ctx, query, args...)
		if err != nil {
			return err
		}
		rows, err := res.RowsAffected()
		*count += int(rows)
		return err
	}
	for _, b := range batches {
		data, err := json.Marshal(b)
		if err == nil {
			err = exec(&n.Batches, `INSERT INTO batches (key, data) VALUES (?, ?) ON CONFLICT (key) DO NOTHING`, b.Key, data)
		}
		if err != nil {
			return Imported{}, fmt.Errorf("store: import %s: batch %s: %w", dir, b.Key, err)
		}
	}
	for key, at := range posted {
		if err := exec(&n.Posted, `INSERT INTO posted (key, at) VALUES (?, ?)
			ON CONFLICT (key) DO UPDATE SET at = excluded.at WHERE excluded.at > posted.at`, key, ms(fromUnix(at))); err != nil {
			return Imported{}, fmt.Errorf("store: import %s: posted %s: %w", dir, key, err)
		}
	}
	for imdb, hit := range rt {
		val, _ := json.Marshal(hit.ID) // *string never fails
		if err := exec(&n.Cache, `INSERT INTO cache (ns, key, val, at) VALUES ('rt', ?, ?, ?)
			ON CONFLICT (ns, key) DO UPDATE SET val = excluded.val, at = excluded.at WHERE excluded.at > cache.at`,
			imdb, val, ms(fromUnix(hit.At))); err != nil {
			return Imported{}, fmt.Errorf("store: import %s: rt cache %s: %w", dir, imdb, err)
		}
	}
	for _, p := range history {
		if err := exec(&n.History, insertHistory, historyArgs(p.entry())...); err != nil {
			return Imported{}, fmt.Errorf("store: import %s: history: %w", dir, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return Imported{}, fmt.Errorf("store: import %s: %w", dir, err)
	}
	return n, nil
}

type legacyRT struct {
	ID *string `json:"id"`
	At float64 `json:"at"`
}

// legacyBatch is a pending.json batch. Series and movie items already use domain's JSON names.
type legacyBatch struct {
	Source     string                     `json:"source"`
	Subject    *domain.Series             `json:"subject"` // nil: a movie batch
	Items      map[string]json.RawMessage `json:"items"`
	First      float64                    `json:"first"`
	Last       float64                    `json:"last"`
	Fast       bool                       `json:"fast"`
	PlexChecks int                        `json:"plex_checks"`
	Tries      int                        `json:"tries"`
	NotBefore  float64                    `json:"not_before"` // 0: none
}

type legacyEpisode struct {
	Season  int    `json:"season"`
	Number  int    `json:"number"`
	Title   string `json:"title"`
	Aired   string `json:"aired"`
	Quality string `json:"quality"`
}

func (lb legacyBatch) batch(key string) (*domain.Batch, error) {
	b := &domain.Batch{
		Key: key, Source: lb.Source, Series: lb.Subject,
		First: fromUnix(lb.First), Last: fromUnix(lb.Last), Following: lb.Fast,
		MediaChecks: lb.PlexChecks, Tries: lb.Tries,
	}
	if lb.NotBefore != 0 {
		b.NotBefore = fromUnix(lb.NotBefore)
	}
	if lb.Subject == nil {
		b.Kind = domain.KindMovie
		b.Movies = make(map[domain.ItemKey]domain.Movie, len(lb.Items))
		for k, raw := range lb.Items {
			var m domain.Movie
			if err := json.Unmarshal(raw, &m); err != nil {
				return nil, fmt.Errorf("%s: %s: %w", key, k, err)
			}
			b.Movies[domain.ItemKey(k)] = m
		}
		return b, nil
	}
	b.Kind = domain.KindTV
	b.Episodes = make(map[domain.ItemKey]domain.Episode, len(lb.Items))
	for k, raw := range lb.Items {
		var e legacyEpisode
		if err := json.Unmarshal(raw, &e); err != nil {
			return nil, fmt.Errorf("%s: %s: %w", key, k, err)
		}
		// The episode ID is only in the key: "<source>:ep:<id>".
		id, err := strconv.Atoi(k[strings.LastIndexByte(k, ':')+1:])
		if err != nil {
			return nil, fmt.Errorf("%s: episode key %q: %w", key, k, err)
		}
		b.Episodes[domain.ItemKey(k)] = domain.Episode{
			ID: id, Season: e.Season, Number: e.Number, Title: e.Title, Aired: parseISO(e.Aired), Quality: e.Quality,
		}
	}
	return b, nil
}

// legacyPost is a history.jsonl line.
type legacyPost struct {
	At       string `json:"at"` // local time without a zone
	Source   string `json:"source"`
	Title    string `json:"title"`
	Headline string `json:"headline"`
	Items    int    `json:"items"`
	Fast     bool   `json:"fast"` // null for movies
}

func (p legacyPost) entry() domain.HistoryEntry {
	return domain.HistoryEntry{
		At: parseISO(p.At), Source: p.Source, Title: p.Title, Headline: p.Headline, Items: p.Items, Following: p.Fast,
	}
}

func parseHistory(data []byte) ([]legacyPost, error) {
	var out []legacyPost
	for i, line := range bytes.Split(data, []byte("\n")) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var p legacyPost
		if err := json.Unmarshal(line, &p); err != nil {
			return nil, fmt.Errorf("line %d: %w", i+1, err)
		}
		out = append(out, p)
	}
	return out, nil
}

// fromUnix converts the reference's float unix seconds, keeping milliseconds.
func fromUnix(sec float64) time.Time {
	return time.UnixMilli(int64(math.Round(sec * 1000))).UTC()
}

// parseISO reads an ISO timestamp the way the reference did: without a zone it is local time.
// Unparseable is zero (unknown).
func parseISO(s string) time.Time {
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.UTC()
	}
	if t, err := time.ParseInLocation("2006-01-02T15:04:05", s, time.Local); err == nil {
		return t.UTC()
	}
	return time.Time{}
}
