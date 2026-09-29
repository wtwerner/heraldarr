package app

import (
	"context"
	"errors"
	"strconv"

	"github.com/wtwerner/heraldarr/internal/batcher"
	"github.com/wtwerner/heraldarr/internal/domain"
	"github.com/wtwerner/heraldarr/internal/notify/discord"
	"github.com/wtwerner/heraldarr/internal/render"
)

// showKey is the lookup key of a TV batch's series (movie batches use their item keys).
const showKey = "show"

type wanted struct {
	guids []string
	path  string
}

// deliver renders and posts one batch to dest (nil: the source's destination). It returns the
// item keys that went out and the outcome for the batcher; err explains a Failed outcome. record
// writes the post history.
func (a *App) deliver(ctx context.Context, b *domain.Batch, force bool, dest *domain.Destination, record bool,
) ([]domain.ItemKey, batcher.Outcome, error) {
	src, ok := a.Sources[b.Source]
	if !ok {
		// Configuration changed since the batch was queued: nowhere to post it.
		a.Log.Warn("dropping batch of a source that is no longer configured", "batch", b.Key)
		return b.Keys(), batcher.Posted, nil
	}
	if dest == nil {
		dest = &src.Destination
	}
	found, wait := a.lookup(ctx, b, force)
	if wait {
		return nil, batcher.Waiting, nil
	}
	common := render.Common{Style: src.Style, Now: a.Clock.Now()}
	if a.Media != nil {
		common.ServerName = a.Media.Name()
	}

	type card struct {
		keys []domain.ItemKey
		card domain.Card
	}
	var cards []card
	if b.Kind == domain.KindTV {
		c, err := a.renderTV(ctx, b, src, found[showKey], common)
		if err != nil {
			return nil, batcher.Failed, err
		}
		cards = append(cards, card{b.Keys(), c})
	} else {
		var movies []render.MovieInput
		for _, k := range b.Keys() {
			m := b.Movies[k]
			in := render.MovieInput{
				Key: k, Movie: m, URL: a.url(found[string(k)]),
				RottenTomatoes: a.RT.RottenTomatoes(ctx, m.IMDbID, m.Title),
			}
			if d, err := src.Arr.Movie(ctx, m.ID); err == nil {
				in.Detail = d
			} // deleted or unreachable: the webhook data is enough
			movies = append(movies, in)
		}
		if len(movies) >= a.DigestFrom {
			cards = append(cards, card{b.Keys(), render.Digest(movies, common)})
		} else {
			for _, in := range movies {
				cards = append(cards, card{[]domain.ItemKey{in.Key}, render.Movie(in, common)})
			}
		}
	}

	var sent []domain.ItemKey
	for _, c := range cards {
		res, err := a.Notifier.Post(ctx, *dest, render.Layouts(c.card, a.Clock.Now()))
		var refused *discord.RefusedError
		switch {
		case errors.As(err, &refused):
			// Discord rejects this card in every layout; resending won't change that.
			a.Log.Error("discord refused the card in every layout; dropping it", "batch", batchName(b),
				"title", c.card.Title, "err", err)
			sent = append(sent, c.keys...)
			continue
		case err != nil:
			return sent, batcher.Failed, err
		}
		sent = append(sent, c.keys...)
		a.Log.Info("posted", "source", b.Source, "title", c.card.Title, "headline", c.card.Headline,
			"layout", res.Layout, "items", len(c.keys))
		if !record {
			continue
		}
		if err := a.Store.AppendHistory(ctx, domain.HistoryEntry{
			At: a.Clock.Now(), Source: b.Source,
			Destination: dest.Name, Title: c.card.Title, Headline: c.card.Headline,
			Items: len(c.keys), Following: b.Following, MessageID: res.MessageID,
		}); err != nil {
			a.Log.Warn("writing history", "err", err)
		}
	}
	return sent, batcher.Posted, nil
}

func (a *App) renderTV(ctx context.Context, b *domain.Batch, src Source, show *domain.MediaItem,
	common render.Common,
) (domain.Card, error) {
	s := b.Series
	in := render.TVInput{Common: common, Batch: b, Show: show, RottenTomatoes: a.RT.RottenTomatoes(ctx, s.IMDbID, s.Title)}
	detail, err := src.Arr.Series(ctx, s.ID)
	switch {
	case errors.Is(err, domain.ErrNotFound):
		// Deleted since the import: post from the webhook data alone.
	case err != nil:
		return domain.Card{}, err
	default:
		in.Detail = detail
	}
	if show != nil {
		in.ShowURL = a.url(show)
		if seasons := seasonsIn(b); len(seasons) == 1 {
			if k, err := a.Media.SeasonKey(ctx, show, seasons[0]); err == nil && k != "" {
				in.SeasonURL = a.Media.URL(k)
			}
		}
	}
	return render.TV(in), nil
}

// lookup finds the batch's items in the media server. wait is true when some aren't there yet and
// the batch should be checked again later (a partial scan is requested on the first check).
// Media server errors never block a post: the card goes out without deep links.
func (a *App) lookup(ctx context.Context, b *domain.Batch, force bool) (map[string]*domain.MediaItem, bool) {
	if a.Media == nil {
		return nil, false
	}
	want := map[string]wanted{}
	if b.Kind == domain.KindTV {
		want[showKey] = wanted{[]string{"tvdb://" + strconv.Itoa(b.Series.TVDBID), imdbGUID(b.Series.IMDbID)}, b.Series.Path}
	} else {
		for k, m := range b.Movies {
			want[string(k)] = wanted{[]string{"tmdb://" + strconv.Itoa(m.TMDBID), imdbGUID(m.IMDbID)}, m.Path}
		}
	}
	found := map[string]*domain.MediaItem{}
	var missing []string
	for k, w := range want {
		it, err := a.Media.Find(ctx, w.guids, w.path, b.Kind, false)
		if err != nil {
			a.Log.Warn("media server lookup failed; posting without links", "batch", batchName(b), "err", err)
			return nil, false
		}
		if it == nil {
			missing = append(missing, k)
			continue
		}
		found[k] = it
	}
	if len(missing) > 0 && !force && b.MediaChecks < a.WaitChecks {
		if b.MediaChecks == 0 {
			for _, k := range missing {
				if err := a.Media.Scan(ctx, want[k].path); err != nil {
					a.Log.Warn("media server scan request failed", "path", want[k].path, "err", err)
				}
			}
		}
		a.Log.Info("not in the media server yet", "batch", batchName(b), "missing", len(missing),
			"check", b.MediaChecks+1, "of", a.WaitChecks)
		return nil, true
	}
	for _, k := range missing {
		if it, err := a.Media.Find(ctx, want[k].guids, want[k].path, b.Kind, true); err == nil && it != nil {
			found[k] = it
		}
	}
	return found, false
}

func (a *App) url(it *domain.MediaItem) string {
	if it == nil || a.Media == nil {
		return ""
	}
	return a.Media.URL(it.RatingKey)
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
