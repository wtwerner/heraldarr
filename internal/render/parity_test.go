package render_test

import (
	"context"
	"sort"
	"strconv"
	"testing"

	"github.com/wtwerner/heraldarr/internal/domain"
	"github.com/wtwerner/heraldarr/internal/render"
	"github.com/wtwerner/heraldarr/internal/source/arr"
	"github.com/wtwerner/heraldarr/internal/testkit"
)

// TestParity renders every scenario the way the reference implementation's send_batch does and
// compares each post's card and all three layouts with testdata/expected.
func TestParity(t *testing.T) {
	if !render.Implemented() {
		t.Skip("render not implemented yet (Wave 1)")
	}
	for _, name := range testkit.Names(t) {
		t.Run(name, func(t *testing.T) {
			sc, exp := testkit.Load(t, name)
			posts := renderScenario(t, sc)
			if len(posts) != len(exp.Posts) {
				t.Fatalf("got %d posts, want %d", len(posts), len(exp.Posts))
			}
			for i, p := range posts {
				want := exp.Posts[i]
				testkit.EqualJSON(t, "card", testkit.ReferenceCard(p), want.Card)
				ls := render.Layouts(p, sc.Now)
				if len(ls) != 3 {
					t.Fatalf("got %d layouts, want 3", len(ls))
				}
				testkit.EqualJSON(t, "v2", ls[0].Body, want.V2)
				testkit.EqualJSON(t, "embed", ls[1].Body, want.Embed)
				testkit.EqualJSON(t, "embed_inline", ls[2].Body, want.EmbedInline)
			}
		})
	}
}

// renderScenario builds the batches the reference would have pending (grouping decoded events by
// series / source, skipping upgrades and already-posted items) and renders them in key order.
func renderScenario(t *testing.T, sc *testkit.Scenario) []domain.Card {
	t.Helper()
	ctx := context.Background()
	posted := map[domain.ItemKey]bool{}
	for _, k := range sc.Posted {
		posted[k] = true
	}
	batches := map[string]*domain.Batch{}
	for _, ev := range sc.Events {
		src := testkit.Sources[ev.Source]
		imp, ok, err := arr.ParseWebhook(ev.Source, src.Kind, ev.Payload)
		if err != nil || !ok || imp.Upgrade {
			continue
		}
		if src.Kind == domain.KindTV {
			key := ev.Source + ":" + itoa(imp.Series.ID)
			b := batches[key]
			if b == nil {
				b = &domain.Batch{
					Key: key, Source: ev.Source, Kind: domain.KindTV, Series: imp.Series,
					Episodes: map[domain.ItemKey]domain.Episode{},
				}
				batches[key] = b
			}
			for _, e := range imp.Episodes {
				if k := domain.EpisodeKey(ev.Source, e.ID); !posted[k] {
					b.Episodes[k] = e
				}
			}
		} else {
			key := ev.Source + ":movies"
			b := batches[key]
			if b == nil {
				b = &domain.Batch{Key: key, Source: ev.Source, Kind: domain.KindMovie, Movies: map[domain.ItemKey]domain.Movie{}}
				batches[key] = b
			}
			if k := domain.MovieKey(ev.Source, imp.Movie.ID); !posted[k] {
				b.Movies[k] = *imp.Movie
			}
		}
	}
	keys := make([]string, 0, len(batches))
	for k := range batches {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	media, arrs, rt := testkit.Media{Sc: sc}, testkit.Arr{Sc: sc}, testkit.RottenTomatoes(sc)
	var cards []domain.Card
	for _, key := range keys {
		b := batches[key]
		if b.Len() == 0 {
			continue
		}
		common := render.Common{ServerName: media.Name(), Style: testkit.Sources[b.Source].Style, Now: sc.Now}
		if b.Kind == domain.KindTV {
			s := b.Series
			in := render.TVInput{Common: common, Batch: b, RottenTomatoes: rt(s.IMDbID, s.Title)}
			in.Detail, _ = arrs.Series(ctx, s.ID)
			in.Show, _ = media.Find(ctx, []string{"tvdb://" + itoa(s.TVDBID), imdbGUID(s.IMDbID)}, s.Path, domain.KindTV, false)
			if in.Show != nil {
				in.ShowURL = media.URL(in.Show.RatingKey)
				if seasons := seasonsIn(b); len(seasons) == 1 {
					if k, _ := media.SeasonKey(ctx, in.Show, seasons[0]); k != "" {
						in.SeasonURL = media.URL(k)
					}
				}
			}
			cards = append(cards, render.TV(in))
			continue
		}
		var movies []render.MovieInput
		for _, k := range b.Keys() {
			m := b.Movies[k]
			in := render.MovieInput{Key: k, Movie: m, RottenTomatoes: rt(m.IMDbID, m.Title)}
			in.Detail, _ = arrs.Movie(ctx, m.ID)
			if it, _ := media.Find(ctx, []string{"tmdb://" + itoa(m.TMDBID), imdbGUID(m.IMDbID)}, m.Path, domain.KindMovie, false); it != nil {
				in.URL = media.URL(it.RatingKey)
			}
			movies = append(movies, in)
		}
		if len(movies) >= 4 {
			cards = append(cards, render.Digest(movies, common))
			continue
		}
		// The reference posts single movies in arrival order; scenario events arrive in key order.
		for _, in := range movies {
			cards = append(cards, render.Movie(in, common))
		}
	}
	return cards
}

func seasonsIn(b *domain.Batch) []int {
	seen := map[int]bool{}
	var out []int
	for _, e := range b.Episodes {
		if !seen[e.Season] {
			seen[e.Season] = true
			out = append(out, e.Season)
		}
	}
	return out
}

func imdbGUID(id string) string {
	if id == "" {
		return ""
	}
	return "imdb://" + id
}

func itoa(i int) string { return strconv.Itoa(i) }
