package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/wtwerner/heraldarr/internal/domain"
	"github.com/wtwerner/heraldarr/internal/testkit"
)

func openTemp(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "heraldarr.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestStoreContract(t *testing.T) {
	testkit.StoreContract(t,
		func(t *testing.T) domain.Store { return openTemp(t) },
		func(t *testing.T, s domain.Store) domain.Store {
			path := s.(*Store).path
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			r, err := Open(path)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = r.Close() })
			return r
		})
}

func TestOpenSetsUpDatabase(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()

	var version int
	if err := s.db.QueryRowContext(ctx, `SELECT version FROM schema_version`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != len(migrations) {
		t.Errorf("schema_version = %d, want %d", version, len(migrations))
	}
	var mode string
	if err := s.db.QueryRowContext(ctx, `PRAGMA journal_mode`).Scan(&mode); err != nil || mode != "wal" {
		t.Errorf("journal_mode = %q, %v; want wal", mode, err)
	}
	var busy int
	if err := s.db.QueryRowContext(ctx, `PRAGMA busy_timeout`).Scan(&busy); err != nil || busy <= 0 {
		t.Errorf("busy_timeout = %d, %v", busy, err)
	}
	if n := s.db.Stats().MaxOpenConnections; n != 1 {
		t.Errorf("writer pool allows %d connections, want 1", n)
	}
}

func TestOpenIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "heraldarr.db")
	for range 3 {
		s, err := Open(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestOpenRefusesNewerSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "heraldarr.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(context.Background(), `UPDATE schema_version SET version = ?`, len(migrations)+1); err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
	if s, err := Open(path); err == nil {
		_ = s.Close()
		t.Fatal("opened a database written by a newer version")
	}
}

func TestOpenFailsOnBadPath(t *testing.T) {
	if s, err := Open(filepath.Join(t.TempDir(), "missing", "dir", "heraldarr.db")); err == nil {
		_ = s.Close()
		t.Fatal("want an error for a directory that doesn't exist")
	}
}

func TestConcurrentUse(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	t0 := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	var wg sync.WaitGroup
	errs := make(chan error, 64)
	for i := range 16 {
		wg.Go(func() {
			key := domain.ItemKey(fmt.Sprintf("sonarr:ep:%d", i))
			b := &domain.Batch{Key: fmt.Sprintf("sonarr:%d", i), Source: "sonarr", Kind: domain.KindTV, First: t0, Last: t0}
			for _, err := range []error{
				s.SaveBatch(ctx, b),
				s.MarkPosted(ctx, []domain.ItemKey{key}, t0),
				s.CachePut(ctx, "rt", string(key), []byte("null"), t0),
				s.AppendHistory(ctx, domain.HistoryEntry{At: t0, Source: "sonarr", Title: "X", Items: 1}),
			} {
				if err != nil {
					errs <- err
				}
			}
			if _, err := s.LoadBatches(ctx); err != nil {
				errs <- err
			}
			if got, err := s.PostedSince(ctx, []domain.ItemKey{key}, t0); err != nil || !got[key] {
				errs <- fmt.Errorf("PostedSince(%s) = %v: %w", key, got, err)
			}
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	if got, _ := s.LoadBatches(ctx); len(got) != 16 {
		t.Errorf("got %d batches, want 16", len(got))
	}
	if n := historyCount(t, s); n != 16 {
		t.Errorf("got %d history rows, want 16", n)
	}
}

func TestPostedSinceManyKeys(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	t0 := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	keys := make([]domain.ItemKey, 50000) // more than SQLite's bound-parameter limit
	for i := range keys {
		keys[i] = domain.ItemKey(fmt.Sprintf("radarr:movie:%d", i))
	}
	if err := s.MarkPosted(ctx, keys, t0); err != nil {
		t.Fatal(err)
	}
	got, err := s.PostedSince(ctx, keys, t0)
	if err != nil || len(got) != len(keys) {
		t.Fatalf("PostedSince: %d keys, %v", len(got), err)
	}
	if got, err := s.PostedSince(ctx, nil, t0); err != nil || len(got) != 0 {
		t.Errorf("PostedSince(nil) = %v, %v", got, err)
	}
}

func TestClosedStoreErrors(t *testing.T) {
	s := openTemp(t)
	_ = s.Close()
	if err := s.SaveBatch(context.Background(), &domain.Batch{Key: "k"}); err == nil {
		t.Error("SaveBatch on a closed store succeeded")
	}
}

func TestImportLegacy(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	got, err := s.ImportLegacy(ctx, "testdata/legacy")
	if err != nil {
		t.Fatal(err)
	}
	if want := (Imported{Batches: 2, Posted: 3, Cache: 2, History: 2}); got != want {
		t.Errorf("ImportLegacy = %+v, want %+v", got, want)
	}

	t.Run("posted", func(t *testing.T) {
		at := time.UnixMilli(1781524800250)
		keys := []domain.ItemKey{"sonarr:ep:501", "sonarr:ep:502", "radarr:movie:77", "sonarr:ep:9"}
		p, err := s.PostedSince(ctx, keys, at)
		if err != nil || len(p) != 2 || !p["sonarr:ep:501"] || !p["sonarr:ep:502"] {
			t.Errorf("PostedSince(at) = %v, %v", p, err)
		}
		if p, _ := s.PostedSince(ctx, keys, at.Add(time.Millisecond)); len(p) != 0 {
			t.Errorf("fractional seconds lost: %v", p)
		}
		if p, _ := s.PostedSince(ctx, keys, time.Unix(1780000000, 0)); len(p) != 3 {
			t.Errorf("PostedSince(older) = %v", p)
		}
	})

	t.Run("rt cache", func(t *testing.T) {
		v, at, ok, err := s.CacheGet(ctx, "rt", "tt0000001")
		if !ok || err != nil || string(v) != `"m/example_movie"` || !at.Equal(time.UnixMilli(1781000000500)) {
			t.Errorf("hit = %s %v %v %v", v, at, ok, err)
		}
		v, at, ok, err = s.CacheGet(ctx, "rt", "tt0000002")
		if !ok || err != nil || string(v) != "null" || !at.Equal(time.Unix(1781100000, 0)) {
			t.Errorf("miss = %s %v %v %v", v, at, ok, err)
		}
	})

	t.Run("batches", func(t *testing.T) {
		b, err := s.LoadBatches(ctx)
		if err != nil || len(b) != 2 {
			t.Fatalf("LoadBatches = %d, %v", len(b), err)
		}
		tv := b["sonarr:101"]
		want := &domain.Series{
			ID: 101, Title: "Example Show", Year: 2024, Path: "/data/media/TV/Example Show", TVDBID: 9001,
			IMDbID: "tt0000003", Genres: []string{"Drama"}, Poster: "https://example.org/poster.jpg",
		}
		if tv.Key != "sonarr:101" || tv.Source != "sonarr" || tv.Kind != domain.KindTV || fmt.Sprint(tv.Series) != fmt.Sprint(want) {
			t.Errorf("tv batch = %+v, series %+v", tv, tv.Series)
		}
		if !tv.First.Equal(time.UnixMilli(1782000000500)) || !tv.Last.Equal(time.Unix(1782000060, 0)) ||
			!tv.Following || tv.MediaChecks != 1 || tv.Tries != 0 || !tv.NotBefore.Equal(time.Unix(1782000240, 0)) {
			t.Errorf("tv batch state = %+v", tv)
		}
		ep := tv.Episodes["sonarr:ep:503"]
		if want := (domain.Episode{ID: 503, Season: 2, Number: 3, Title: "The Third", Aired: time.Date(2026, 9, 27, 1, 0, 0, 0, time.UTC), Quality: "WEBDL-1080p"}); ep != want {
			t.Errorf("episode = %+v, want %+v", ep, want)
		}
		if ep := tv.Episodes["sonarr:ep:504"]; ep != (domain.Episode{ID: 504, Season: 2, Number: 4}) {
			t.Errorf("episode without details = %+v", ep)
		}

		mv := b["radarr:movies"]
		if mv.Kind != domain.KindMovie || mv.Series != nil || mv.Tries != 2 || !mv.NotBefore.IsZero() || mv.Following {
			t.Errorf("movie batch = %+v", mv)
		}
		m := mv.Movies["radarr:movie:78"]
		if m.ID != 78 || m.TMDBID != 4242 || m.Size != 1<<40 || m.ReleaseGroup != "EXAMPLE" || m.HDR != "DV HDR10" ||
			m.AudioCodec != "TrueHD Atmos" || m.AudioChannels != 7.1 || m.IMDbID != "" || m.Quality != "Bluray-2160p" {
			t.Errorf("movie = %+v", m)
		}
	})

	t.Run("history", func(t *testing.T) {
		rows, err := s.db.QueryContext(ctx, `SELECT at, source, title, headline, items, following FROM history ORDER BY id`)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = rows.Close() }()
		var got []string
		for rows.Next() {
			var at int64
			var src, title, headline string
			var items int
			var following bool
			if err := rows.Scan(&at, &src, &title, &headline, &items, &following); err != nil {
				t.Fatal(err)
			}
			got = append(got, fmt.Sprintf("%s|%s|%s|%s|%d|%v", time.UnixMilli(at).UTC().Format(time.RFC3339), src, title, headline, items, following))
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		// The reference wrote local wall-clock time without a zone.
		first := time.Date(2026, 9, 27, 20, 15, 0, 0, time.Local).UTC().Format(time.RFC3339)
		second := time.Date(2026, 9, 28, 9, 0, 5, 0, time.Local).UTC().Format(time.RFC3339)
		want := []string{
			first + "|sonarr|Example Show|2 new episodes|2|true",
			second + "|radarr|Example Movie (2025)|New movie|1|false",
		}
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Errorf("history =\n%v\nwant\n%v", got, want)
		}
	})
}

func TestImportLegacyKeepsNewerState(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	later := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC) // after every fixture timestamp
	if err := s.MarkPosted(ctx, []domain.ItemKey{"sonarr:ep:501"}, later); err != nil {
		t.Fatal(err)
	}
	if err := s.CachePut(ctx, "rt", "tt0000002", []byte(`"m/found_later"`), later); err != nil {
		t.Fatal(err)
	}
	live := &domain.Batch{Key: "sonarr:101", Source: "sonarr", Kind: domain.KindTV, First: later, Last: later}
	if err := s.SaveBatch(ctx, live); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ImportLegacy(ctx, "testdata/legacy"); err != nil {
		t.Fatal(err)
	}
	if p, _ := s.PostedSince(ctx, []domain.ItemKey{"sonarr:ep:501"}, later); !p["sonarr:ep:501"] {
		t.Error("import moved a posted timestamp back")
	}
	if v, _, _, _ := s.CacheGet(ctx, "rt", "tt0000002"); string(v) != `"m/found_later"` {
		t.Errorf("import replaced a newer cache entry: %s", v)
	}
	if b, _ := s.LoadBatches(ctx); !b["sonarr:101"].First.Equal(later) || len(b["sonarr:101"].Episodes) != 0 {
		t.Errorf("import replaced a live batch: %+v", b["sonarr:101"])
	}
}

func TestImportLegacyMissingFiles(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "posted.json"), []byte(`{"sonarr:ep:1": 1781524800}`), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := s.ImportLegacy(ctx, dir)
	if err != nil || got != (Imported{Posted: 1}) {
		t.Errorf("ImportLegacy = %+v, %v", got, err)
	}
	if _, err := s.ImportLegacy(ctx, filepath.Join(dir, "nope")); err == nil {
		t.Error("want an error for a directory that doesn't exist")
	}
	if _, err := s.ImportLegacy(ctx, t.TempDir()); err == nil {
		t.Error("want an error for a directory with no reference data")
	}
}

func TestImportLegacyIsAtomic(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "posted.json"), []byte(`{"sonarr:ep:1": 1781524800}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "rt_cache.json"), []byte(`{not json`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ImportLegacy(ctx, dir); err == nil {
		t.Fatal("want an error for a malformed file")
	}
	if p, _ := s.PostedSince(ctx, []domain.ItemKey{"sonarr:ep:1"}, time.Time{}); len(p) != 0 {
		t.Error("a failed import left partial data")
	}
}

func historyCount(t *testing.T, s *Store) int {
	t.Helper()
	var n int
	if err := s.db.QueryRowContext(context.Background(), `SELECT count(*) FROM history`).Scan(&n); err != nil && !errors.Is(err, sql.ErrNoRows) {
		t.Fatal(err)
	}
	return n
}
