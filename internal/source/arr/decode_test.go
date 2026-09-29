package arr_test

import (
	"testing"

	"github.com/wtwerner/heraldarr/internal/domain"
	"github.com/wtwerner/heraldarr/internal/source/arr"
	"github.com/wtwerner/heraldarr/internal/testkit"
)

// Every scenario event decodes, and the reference's upgrade rule holds: only non-upgrade events
// can end up pending.
func TestParseWebhookScenarios(t *testing.T) {
	for _, name := range testkit.Names(t) {
		sc, _ := testkit.Load(t, name)
		for i, ev := range sc.Events {
			src := testkit.Sources[ev.Source]
			imp, ok, err := arr.ParseWebhook(ev.Source, src.Kind, ev.Payload)
			if err != nil || !ok {
				t.Fatalf("%s event %d: ok=%v err=%v", name, i, ok, err)
			}
			if (src.Kind == domain.KindTV) != (imp.Series != nil) || (src.Kind == domain.KindMovie) != (imp.Movie != nil) {
				t.Errorf("%s event %d: wrong shape %+v", name, i, imp)
			}
		}
	}
}

func TestParseWebhookFields(t *testing.T) {
	sc, _ := testkit.Load(t, "movie_4k_private")
	imp, _, err := arr.ParseWebhook("radarr4k", domain.KindMovie, sc.Events[0].Payload)
	if err != nil {
		t.Fatal(err)
	}
	m := imp.Movie
	if m.HDR != "DV HDR10Plus" || m.AudioCodec != "TrueHD Atmos" || m.AudioChannels != 7.1 || m.Size != 58_300_000_000 ||
		m.Quality != "Remux-2160p" || m.Path != "/data/media/Movies 4K/The Long Meridian (2025)" ||
		m.Poster != "https://images.example.org/poster/the-long-meridian.jpg" {
		t.Errorf("unexpected movie %+v", m)
	}

	sc, _ = testkit.Load(t, "tv_upgrade_ignored")
	for i, ev := range sc.Events {
		imp, _, _ := arr.ParseWebhook("sonarr", domain.KindTV, ev.Payload)
		if !imp.Upgrade {
			t.Errorf("event %d: want Upgrade (isUpgrade or deletedFiles)", i)
		}
	}
}

func TestParseWebhookIgnoresOtherEvents(t *testing.T) {
	_, ok, err := arr.ParseWebhook("sonarr", domain.KindTV, []byte(`{"eventType":"Test","series":{"id":1}}`))
	if ok || err != nil {
		t.Errorf("Test event: ok=%v err=%v", ok, err)
	}
	if _, _, err := arr.ParseWebhook("sonarr", domain.KindTV, []byte(`{`)); err == nil {
		t.Error("bad json: want error")
	}
}

func TestDecodeDetails(t *testing.T) {
	sc, _ := testkit.Load(t, "movie_single")
	d, err := arr.DecodeMovie(sc.Arr["movie/201"], sc.Arr["credit?movieId=201"])
	if err != nil {
		t.Fatal(err)
	}
	if d.IMDbRating != 7.4 || d.RTRating != 91 || d.RuntimeMin != 124 || d.Collection != "The Meridian Collection" ||
		len(d.Directors) != 1 || len(d.Cast) != 4 || d.Cast[0] != "Lena Marsh" {
		t.Errorf("unexpected detail %+v", d)
	}

	sc, _ = testkit.Load(t, "tv_weekly_episode")
	s, err := arr.DecodeSeries(sc.Arr["series/102"])
	if err != nil {
		t.Fatal(err)
	}
	if s.EpisodeFileCount == nil || *s.EpisodeFileCount != 25 || s.Seasons[3].TotalEpisodeCount != 10 || s.NextAiring.IsZero() {
		t.Errorf("unexpected series %+v", s)
	}
}
