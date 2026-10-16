package testkit

import (
	"context"
	"testing"
	"time"

	"github.com/wtwerner/heraldarr/internal/domain"
)

// StoreContract is the behavior every domain.Store must have. open returns a fresh, empty store;
// reopen (optional) returns a store over the same data, to check persistence.
func StoreContract(t *testing.T, open func(t *testing.T) domain.Store, reopen func(t *testing.T, s domain.Store) domain.Store) {
	ctx := context.Background()
	t0 := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)

	t.Run("batches round-trip", func(t *testing.T) {
		s := open(t)
		b := &domain.Batch{
			Key: "sonarr:1", Source: "sonarr", Kind: domain.KindTV,
			Series:   &domain.Series{ID: 1, Title: "Example", TVDBID: 9, Genres: []string{"Drama"}},
			Episodes: map[domain.ItemKey]domain.Episode{"sonarr:ep:5": {ID: 5, Season: 1, Number: 2, Aired: t0, Quality: "WEBDL-1080p"}},
			First:    t0, Last: t0.Add(time.Minute), Following: true, MediaChecks: 2, Tries: 1, NotBefore: t0.Add(time.Hour),
		}
		m := &domain.Batch{
			Key: "radarr:movies", Source: "radarr", Kind: domain.KindMovie,
			Movies: map[domain.ItemKey]domain.Movie{"radarr:movie:7": {ID: 7, Title: "M", Size: 1 << 40, AudioChannels: 5.1}},
			First:  t0, Last: t0,
		}
		season := 3
		r := &domain.Batch{ // a back-catalog run with nothing pending
			Key: "sonarr:1:s3", Source: "sonarr", Kind: domain.KindTV, Backlog: true, Season: &season,
			Series: &domain.Series{ID: 1, Title: "Example"}, First: t0, Last: t0,
			Run: &domain.Run{
				Episodes: map[domain.ItemKey]domain.Episode{"sonarr:ep:8": {ID: 8, Season: 3, Number: 1}},
				Message:  domain.Message{Destination: "tv", ID: "123", Layout: "v2"},
			},
		}
		for _, x := range []*domain.Batch{b, m, r} {
			if err := s.SaveBatch(ctx, x); err != nil {
				t.Fatal(err)
			}
		}
		if reopen != nil {
			s = reopen(t, s)
		}
		got, err := s.LoadBatches(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 3 {
			t.Fatalf("got %d batches", len(got))
		}
		if g := got["sonarr:1:s3"]; g == nil || !g.Backlog || g.Season == nil || *g.Season != 3 || g.Run == nil ||
			g.Run.Episodes["sonarr:ep:8"].Number != 1 || g.Run.Message.ID != "123" || g.Run.Message.Layout != "v2" {
			t.Errorf("run did not round-trip: %+v", g)
		}
		g := got["sonarr:1"]
		if g == nil || g.Series.Title != "Example" || !g.Episodes["sonarr:ep:5"].Aired.Equal(t0) || !g.Following ||
			g.MediaChecks != 2 || g.Tries != 1 || !g.NotBefore.Equal(t0.Add(time.Hour)) || !g.Last.Equal(t0.Add(time.Minute)) {
			t.Errorf("tv batch did not round-trip: %+v", g)
		}
		if mv := got["radarr:movies"].Movies["radarr:movie:7"]; mv.Size != 1<<40 || mv.AudioChannels != 5.1 {
			t.Errorf("movie did not round-trip: %+v", mv)
		}
		// Mutating a loaded batch must not change the stored one.
		g.Episodes["sonarr:ep:6"] = domain.Episode{ID: 6}
		again, _ := s.LoadBatches(ctx)
		if len(again["sonarr:1"].Episodes) != 1 {
			t.Error("LoadBatches returned shared state")
		}
		if err := s.DeleteBatch(ctx, "sonarr:1"); err != nil {
			t.Fatal(err)
		}
		if after, _ := s.LoadBatches(ctx); len(after) != 2 {
			t.Errorf("after delete: %d batches", len(after))
		}
		if err := s.DeleteBatch(ctx, "missing"); err != nil {
			t.Errorf("deleting a missing batch: %v", err)
		}
	})

	t.Run("posted ledger", func(t *testing.T) {
		s := open(t)
		if err := s.MarkPosted(ctx, []domain.ItemKey{"a", "b"}, t0); err != nil {
			t.Fatal(err)
		}
		if err := s.MarkPosted(ctx, []domain.ItemKey{"c"}, t0.Add(48*time.Hour)); err != nil {
			t.Fatal(err)
		}
		if reopen != nil {
			s = reopen(t, s)
		}
		got, _ := s.PostedSince(ctx, []domain.ItemKey{"a", "b", "c", "d"}, t0.Add(time.Hour))
		if len(got) != 1 || !got["c"] {
			t.Errorf("PostedSince = %v, want only c", got)
		}
		got, _ = s.PostedSince(ctx, []domain.ItemKey{"a"}, t0)
		if !got["a"] {
			t.Error("PostedSince must include items posted exactly at since")
		}
		n, err := s.PrunePosted(ctx, t0.Add(time.Hour))
		if err != nil || n != 2 {
			t.Errorf("PrunePosted = %d, %v; want 2", n, err)
		}
		if got, _ := s.PostedSince(ctx, []domain.ItemKey{"a"}, time.Time{}); len(got) != 0 {
			t.Error("pruned key still posted")
		}
		// Re-marking moves the timestamp forward.
		_ = s.MarkPosted(ctx, []domain.ItemKey{"c"}, t0.Add(72*time.Hour))
		if got, _ := s.PostedSince(ctx, []domain.ItemKey{"c"}, t0.Add(60*time.Hour)); !got["c"] {
			t.Error("MarkPosted did not update the timestamp")
		}
	})

	t.Run("cache", func(t *testing.T) {
		s := open(t)
		if _, _, ok, err := s.CacheGet(ctx, "rt", "tt1"); ok || err != nil {
			t.Errorf("empty cache: ok=%v err=%v", ok, err)
		}
		_ = s.CachePut(ctx, "rt", "tt1", []byte(`"m/x"`), t0)
		_ = s.CachePut(ctx, "other", "tt1", []byte("y"), t0)
		if reopen != nil {
			s = reopen(t, s)
		}
		v, at, ok, err := s.CacheGet(ctx, "rt", "tt1")
		if !ok || err != nil || string(v) != `"m/x"` || !at.Equal(t0) {
			t.Errorf("CacheGet = %q %v %v %v", v, at, ok, err)
		}
	})

	t.Run("alerts round-trip", func(t *testing.T) {
		s := open(t)
		held := &domain.Alert{
			Key: "prowlarr:health:IndexerStatusCheck", Source: "prowlarr", Kind: domain.AlertHealth,
			App: "Prowlarr", Check: "IndexerStatusCheck", Level: domain.LevelWarning,
			Message: "Indexers unavailable due to failures: Example", WikiURL: "https://wiki.example.org/x",
			First: t0, Tries: 2, NotBefore: t0.Add(2 * time.Minute),
		}
		posted := &domain.Alert{
			Key: "sonarr:health:DownloadClientCheck", Source: "sonarr", Kind: domain.AlertHealth,
			Check: "DownloadClientCheck", Level: domain.LevelError, Message: "Unable to communicate",
			First: t0, Resolved: t0.Add(time.Hour), PostedAt: t0.Add(time.Minute), Checked: t0.Add(30 * time.Minute),
			Posted: &domain.Message{Destination: "server", ID: "42", Layout: "v2"},
		}
		for _, a := range []*domain.Alert{held, posted} {
			if err := s.SaveAlert(ctx, a); err != nil {
				t.Fatal(err)
			}
		}
		held.Message = "changed after saving"
		if reopen != nil {
			s = reopen(t, s)
		}
		got, err := s.LoadAlerts(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 2 {
			t.Fatalf("got %d alerts", len(got))
		}
		if g := got[held.Key]; g == nil || g.Message != "Indexers unavailable due to failures: Example" || g.Level != domain.LevelWarning ||
			g.App != "Prowlarr" || g.WikiURL == "" || !g.First.Equal(t0) || g.Tries != 2 || !g.NotBefore.Equal(t0.Add(2*time.Minute)) ||
			g.Posted != nil || !g.Resolved.IsZero() {
			t.Errorf("held alert did not round-trip: %+v", g)
		}
		if g := got[posted.Key]; g == nil || g.Posted == nil || g.Posted.ID != "42" || !g.Resolved.Equal(t0.Add(time.Hour)) ||
			!g.PostedAt.Equal(t0.Add(time.Minute)) || !g.Checked.Equal(t0.Add(30*time.Minute)) {
			t.Errorf("posted alert did not round-trip: %+v", g)
		}
		got[posted.Key].Posted.ID = "mutated"
		if again, _ := s.LoadAlerts(ctx); again[posted.Key].Posted.ID != "42" {
			t.Error("LoadAlerts returned shared state")
		}
		posted.Level = domain.LevelWarning
		if err := s.SaveAlert(ctx, posted); err != nil {
			t.Fatal(err)
		}
		if again, _ := s.LoadAlerts(ctx); again[posted.Key].Level != domain.LevelWarning {
			t.Error("SaveAlert did not replace the alert")
		}
		if err := s.DeleteAlert(ctx, held.Key); err != nil {
			t.Fatal(err)
		}
		if after, _ := s.LoadAlerts(ctx); len(after) != 1 || after[posted.Key] == nil {
			t.Errorf("after delete: %v", after)
		}
		if err := s.DeleteAlert(ctx, "missing"); err != nil {
			t.Errorf("deleting a missing alert: %v", err)
		}
	})

	t.Run("history", func(t *testing.T) {
		s := open(t)
		if err := s.AppendHistory(ctx, domain.HistoryEntry{At: t0, Source: "sonarr", Title: "X", Items: 3}); err != nil {
			t.Fatal(err)
		}
	})
}
