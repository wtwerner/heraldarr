// Package store is the SQLite domain.Store: batches as JSON blobs keyed by batch key, and real
// tables for the posted ledger, the cache and the post history. It also imports the reference
// implementation's data folder (legacy.go) so a cutover keeps the reannounce window.
package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite" // registers the "sqlite" driver

	"github.com/wtwerner/heraldarr/internal/domain"
)

// migrations[i] takes the schema from version i to i+1. Append only; never edit a shipped one.
var migrations = []string{
	`CREATE TABLE batches (
		key  TEXT PRIMARY KEY,
		data BLOB NOT NULL -- domain.Batch as JSON
	);
	CREATE TABLE posted (
		key TEXT PRIMARY KEY,
		at  INTEGER NOT NULL -- unix ms
	);
	CREATE INDEX posted_at ON posted (at);
	CREATE TABLE cache (
		ns  TEXT NOT NULL,
		key TEXT NOT NULL,
		val BLOB NOT NULL,
		at  INTEGER NOT NULL,
		PRIMARY KEY (ns, key)
	) WITHOUT ROWID;
	CREATE TABLE history (
		id          INTEGER PRIMARY KEY,
		at          INTEGER NOT NULL,
		source      TEXT NOT NULL,
		destination TEXT NOT NULL,
		title       TEXT NOT NULL,
		headline    TEXT NOT NULL,
		items       INTEGER NOT NULL,
		following   INTEGER NOT NULL,
		message_id  TEXT NOT NULL
	);
	CREATE INDEX history_at ON history (at);`,
}

// busyTimeout is how long a statement waits for another connection's lock (e.g. a second process
// on the same file) before failing with SQLITE_BUSY.
const busyTimeout = 5 * time.Second

// Store is a domain.Store over one SQLite file. Writes go through a single connection, so they
// never contend with each other; reads use their own pool and, with WAL, never wait for a write.
type Store struct {
	path string
	db   *sql.DB // the single writer
	rdb  *sql.DB // readers
}

var _ domain.Store = (*Store)(nil)

// Open opens or creates the database at path and brings its schema up to date. The directory must
// exist.
func Open(path string) (*Store, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("store: open %s: %w", path, err)
	}
	w, err := sql.Open("sqlite", dsn(abs, url.Values{
		"_journal_mode": {"WAL"},
		"_synchronous":  {"NORMAL"}, // safe with WAL: a crash loses at most the last commits, never corrupts
		"_txlock":       {"immediate"},
	}))
	if err != nil {
		return nil, fmt.Errorf("store: open %s: %w", path, err)
	}
	w.SetMaxOpenConns(1)
	s := &Store{path: path, db: w}
	if err := s.migrate(context.Background()); err != nil {
		_ = w.Close()
		return nil, fmt.Errorf("store: open %s: %w", path, err)
	}
	r, err := sql.Open("sqlite", dsn(abs, url.Values{"_query_only": {"1"}}))
	if err != nil {
		_ = w.Close()
		return nil, fmt.Errorf("store: open %s: %w", path, err)
	}
	r.SetMaxOpenConns(4)
	s.rdb = r
	return s, nil
}

// dsn is a SQLite URI for path, so a path holding '?' or '#' stays a path.
func dsn(abs string, q url.Values) string {
	q.Set("_busy_timeout", fmt.Sprint(busyTimeout.Milliseconds()))
	return "file:" + (&url.URL{Path: filepath.ToSlash(abs)}).EscapedPath() + "?" + q.Encode()
}

func (s *Store) migrate(ctx context.Context) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_version (version INTEGER NOT NULL)`); err != nil {
		return err
	}
	var v int
	switch err := tx.QueryRowContext(ctx, `SELECT version FROM schema_version`).Scan(&v); {
	case errors.Is(err, sql.ErrNoRows):
		if _, err := tx.ExecContext(ctx, `INSERT INTO schema_version (version) VALUES (0)`); err != nil {
			return err
		}
	case err != nil:
		return err
	}
	if v > len(migrations) {
		return fmt.Errorf("schema version %d is newer than this build knows (%d)", v, len(migrations))
	}
	for i := v; i < len(migrations); i++ {
		if _, err := tx.ExecContext(ctx, migrations[i]); err != nil {
			return fmt.Errorf("migration %d: %w", i+1, err)
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE schema_version SET version = ?`, len(migrations)); err != nil {
		return err
	}
	return tx.Commit()
}

// Times are stored as unix milliseconds: enough for every timestamp here, and unlike nanoseconds
// the zero time.Time fits.
func ms(t time.Time) int64 { return t.UnixMilli() }

func fromMS(v int64) time.Time { return time.UnixMilli(v).UTC() }

func (s *Store) LoadBatches(ctx context.Context) (map[string]*domain.Batch, error) {
	rows, err := s.rdb.QueryContext(ctx, `SELECT key, data FROM batches`)
	if err != nil {
		return nil, fmt.Errorf("store: load batches: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := map[string]*domain.Batch{}
	for rows.Next() {
		var key string
		var data []byte
		if err := rows.Scan(&key, &data); err != nil {
			return nil, fmt.Errorf("store: load batches: %w", err)
		}
		b := &domain.Batch{}
		if err := json.Unmarshal(data, b); err != nil {
			return nil, fmt.Errorf("store: load batch %s: %w", key, err)
		}
		out[key] = b
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: load batches: %w", err)
	}
	return out, nil
}

func (s *Store) SaveBatch(ctx context.Context, b *domain.Batch) error {
	data, err := json.Marshal(b)
	if err != nil {
		return fmt.Errorf("store: save batch %s: %w", b.Key, err)
	}
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO batches (key, data) VALUES (?, ?) ON CONFLICT (key) DO UPDATE SET data = excluded.data`, b.Key, data)
	if err != nil {
		return fmt.Errorf("store: save batch %s: %w", b.Key, err)
	}
	return nil
}

func (s *Store) DeleteBatch(ctx context.Context, key string) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM batches WHERE key = ?`, key); err != nil {
		return fmt.Errorf("store: delete batch %s: %w", key, err)
	}
	return nil
}

// Key lists are passed as one JSON array and expanded with json_each, so any number of keys takes
// one statement and one bound parameter.
func keyList(keys []domain.ItemKey) string {
	b, _ := json.Marshal(keys) // []string never fails
	return string(b)
}

func (s *Store) PostedSince(ctx context.Context, keys []domain.ItemKey, since time.Time) (map[domain.ItemKey]bool, error) {
	out := map[domain.ItemKey]bool{}
	if len(keys) == 0 {
		return out, nil
	}
	rows, err := s.rdb.QueryContext(ctx,
		`SELECT key FROM posted WHERE key IN (SELECT value FROM json_each(?)) AND at >= ?`, keyList(keys), ms(since))
	if err != nil {
		return nil, fmt.Errorf("store: posted since: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			return nil, fmt.Errorf("store: posted since: %w", err)
		}
		out[domain.ItemKey(k)] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: posted since: %w", err)
	}
	return out, nil
}

func (s *Store) MarkPosted(ctx context.Context, keys []domain.ItemKey, at time.Time) error {
	if len(keys) == 0 {
		return nil
	}
	// "WHERE true" lets SQLite parse ON CONFLICT after a SELECT.
	_, err := s.db.ExecContext(ctx, `INSERT INTO posted (key, at) SELECT value, ? FROM json_each(?) WHERE true
		ON CONFLICT (key) DO UPDATE SET at = excluded.at`, ms(at), keyList(keys))
	if err != nil {
		return fmt.Errorf("store: mark posted: %w", err)
	}
	return nil
}

func (s *Store) PrunePosted(ctx context.Context, before time.Time) (int, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM posted WHERE at < ?`, ms(before))
	if err != nil {
		return 0, fmt.Errorf("store: prune posted: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("store: prune posted: %w", err)
	}
	return int(n), nil
}

const insertHistory = `INSERT INTO history (at, source, destination, title, headline, items, following, message_id)
	VALUES (?, ?, ?, ?, ?, ?, ?, ?)`

func historyArgs(e domain.HistoryEntry) []any {
	return []any{ms(e.At), e.Source, e.Destination, e.Title, e.Headline, e.Items, e.Following, e.MessageID}
}

func (s *Store) AppendHistory(ctx context.Context, e domain.HistoryEntry) error {
	if _, err := s.db.ExecContext(ctx, insertHistory, historyArgs(e)...); err != nil {
		return fmt.Errorf("store: append history: %w", err)
	}
	return nil
}

func (s *Store) CacheGet(ctx context.Context, ns, key string) (val []byte, at time.Time, ok bool, err error) {
	var t int64
	err = s.rdb.QueryRowContext(ctx, `SELECT val, at FROM cache WHERE ns = ? AND key = ?`, ns, key).Scan(&val, &t)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return nil, time.Time{}, false, nil
	case err != nil:
		return nil, time.Time{}, false, fmt.Errorf("store: cache get %s/%s: %w", ns, key, err)
	}
	return val, fromMS(t), true, nil
}

func (s *Store) CachePut(ctx context.Context, ns, key string, val []byte, at time.Time) error {
	if val == nil {
		val = []byte{} // NOT NULL
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO cache (ns, key, val, at) VALUES (?, ?, ?, ?)
		ON CONFLICT (ns, key) DO UPDATE SET val = excluded.val, at = excluded.at`, ns, key, val, ms(at))
	if err != nil {
		return fmt.Errorf("store: cache put %s/%s: %w", ns, key, err)
	}
	return nil
}

// Close closes the database. Calling it again is a no-op.
func (s *Store) Close() error {
	var err error
	if s.rdb != nil {
		err = s.rdb.Close()
	}
	return errors.Join(err, s.db.Close())
}
