package app

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"strconv"

	"github.com/wtwerner/heraldarr/internal/batcher"
	"github.com/wtwerner/heraldarr/internal/domain"
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
// writes the post history. For a back-catalog batch, msg is what its run should edit from now on
// (nil: unchanged): the card it just posted, or zero when the run's card can't be edited.
func (a *App) deliver(ctx context.Context, b *domain.Batch, force bool, dest *domain.Destination, record bool,
) (sent []domain.ItemKey, outcome batcher.Outcome, msg *domain.Message, err error) {
	src, ok := a.Sources[b.Source]
	if !ok {
		// Configuration changed since the batch was queued: a failed try, so it is retried (the
		// source may come back) and eventually dropped without being recorded as posted.
		return nil, batcher.Failed, nil, fmt.Errorf("source %q is no longer configured", b.Source)
	}
	if dest == nil {
		dest = &src.Destination
	}
	downloading := 0
	if b.Backlog {
		n, err := a.queued(ctx, b, src)
		switch {
		case err != nil:
			a.Log.Warn("download queue unavailable", "batch", batchName(b), "err", err)
		case n > 0 && !force && a.batcher.AwaitsQueue(b):
			a.Log.Info("run still downloading", "batch", batchName(b), "queued", n)
			return nil, batcher.Queued, nil, nil
		}
		downloading = n
		b = withRun(b) // the card shows the whole run
	}
	found, wait := a.lookup(ctx, b, force)
	if wait {
		return nil, batcher.Waiting, nil, nil
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
		c, err := a.renderTV(ctx, b, src, found[showKey], common, downloading)
		if err != nil {
			return nil, batcher.Failed, nil, err
		}
		if b.Run != nil {
			return a.update(ctx, b, c, dest, record)
		}
		cards = append(cards, card{b.Keys(), c})
	} else {
		digest := b.Len() >= a.DigestFrom
		var movies []render.MovieInput
		for _, k := range b.Keys() {
			m := b.Movies[k]
			in := render.MovieInput{Key: k, Movie: m, URL: a.url(found[string(k)]), Scores: a.scores(ctx, found[string(k)])}
			if !digest { // digest cards have no Rotten Tomatoes button
				in.RottenTomatoes = a.RT.RottenTomatoes(ctx, m.IMDbID, m.Title)
			}
			if d, err := src.Arr.Movie(ctx, m.ID); err == nil {
				in.Detail = d
			} // deleted or unreachable: the webhook data is enough
			movies = append(movies, in)
		}
		if digest {
			cards = append(cards, card{b.Keys(), render.Digest(movies, common)})
		} else {
			for _, in := range movies {
				cards = append(cards, card{[]domain.ItemKey{in.Key}, render.Movie(in, common)})
			}
		}
	}

	for _, c := range cards {
		res, err := a.Notifier.Post(ctx, *dest, render.Layouts(c.card, a.Clock.Now()))
		if err != nil {
			// As in the reference, a card Discord refuses in every layout is a failed try too
			// (a deleted or rotated webhook refuses with 401/404): retried, then dropped.
			return sent, batcher.Failed, nil, err
		}
		sent = append(sent, c.keys...)
		if b.Backlog {
			msg = &domain.Message{Destination: dest.Name, ID: res.MessageID, Layout: res.Layout}
		}
		a.Log.Info("posted", "source", b.Source, "title", c.card.Title, "headline", c.card.Headline,
			"layout", res.Layout, "items", len(c.keys))
		if !record {
			continue
		}
		a.metrics.posted(b.Source, dest.Name, res.Layout, a.Clock.Now())
		// Recorded per card, so a crash mid-batch doesn't post the earlier cards again.
		if err := a.Store.MarkPosted(context.WithoutCancel(ctx), c.keys, a.Clock.Now()); err != nil {
			a.Log.Warn("recording posted items", "err", err)
		}
		if err := a.Store.AppendHistory(context.WithoutCancel(ctx), domain.HistoryEntry{
			At: a.Clock.Now(), Source: b.Source,
			Destination: dest.Name, Title: c.card.Title, Headline: c.card.Headline,
			Items: len(c.keys), Following: b.Following, MessageID: res.MessageID,
		}); err != nil {
			a.Log.Warn("writing history", "err", err)
		}
	}
	return sent, batcher.Posted, msg, nil
}

// update folds a run's new episodes (b holds the whole run, see withRun) into the card its first
// episodes posted: an edit, silent in Discord. Without an editable card they are folded in
// without one. Either way they count as announced.
func (a *App) update(ctx context.Context, b *domain.Batch, c domain.Card, dest *domain.Destination, record bool,
) ([]domain.ItemKey, batcher.Outcome, *domain.Message, error) {
	keys := newKeys(b)
	m := b.Run.Message
	var msg *domain.Message
	switch {
	case !a.Timing.Backlog.Edit:
		a.Log.Info("folded into the run", "batch", batchName(b), "items", len(keys))
	case m.ID == "" || m.Destination != dest.Name:
		a.Log.Info("folded into the run; its card can't be edited", "batch", batchName(b), "items", len(keys))
	default:
		var layout *domain.Layout
		for _, l := range render.Layouts(c, a.Clock.Now()) {
			if l.Name == m.Layout {
				layout = &l
			}
		}
		var err error
		if layout == nil {
			err = fmt.Errorf("layout %q: %w", m.Layout, domain.ErrNotFound)
		} else {
			err = a.Notifier.Edit(ctx, *dest, m.ID, *layout)
		}
		switch {
		case errors.Is(err, domain.ErrNotFound) || errors.Is(err, domain.ErrRefused):
			// Deleted, or Discord won't take this card as an edit: stop trying, keep quiet.
			a.Log.Warn("can't edit the run's card; folding in without it", "batch", batchName(b), "err", err)
			msg = &domain.Message{}
		case err != nil:
			return nil, batcher.Failed, nil, err
		default:
			a.Log.Info("edited", "source", b.Source, "title", c.Title, "headline", c.Headline,
				"items", len(keys), "run", len(b.Episodes))
		}
	}
	if record {
		if err := a.Store.MarkPosted(context.WithoutCancel(ctx), keys, a.Clock.Now()); err != nil {
			a.Log.Warn("recording posted items", "err", err)
		}
	}
	return keys, batcher.Posted, msg, nil
}

// withRun returns b with its run's announced episodes added, so the card covers the whole run.
func withRun(b *domain.Batch) *domain.Batch {
	if b.Run == nil || len(b.Run.Episodes) == 0 {
		return b
	}
	v := *b
	v.Episodes = maps.Clone(b.Run.Episodes)
	maps.Copy(v.Episodes, b.Episodes)
	return &v
}

// newKeys are the keys of a run batch that aren't announced yet.
func newKeys(b *domain.Batch) []domain.ItemKey {
	var out []domain.ItemKey
	for _, k := range b.Keys() {
		if _, done := b.Run.Episodes[k]; !done {
			out = append(out, k)
		}
	}
	return out
}

// queued counts the episodes the *arr still has queued for b's run: back catalog (new episodes
// go to their own card), of its season when runs are per season, and not already in the batch.
func (a *App) queued(ctx context.Context, b *domain.Batch, src Source) (int, error) {
	q, err := src.Arr.Queue(ctx, b.Series.ID)
	if err != nil {
		return 0, err
	}
	have := map[domain.ItemKey]bool{}
	for k := range b.Episodes {
		have[k] = true
	}
	if b.Run != nil {
		for k := range b.Run.Episodes {
			have[k] = true
		}
	}
	n := 0
	for _, it := range q {
		inRun := b.Season == nil || *b.Season == it.Season
		if inRun && !have[domain.EpisodeKey(b.Source, it.EpisodeID)] && !a.batcher.Recent(domain.Episode{Aired: it.Aired}) {
			n++
		}
	}
	return n, nil
}

func (a *App) renderTV(ctx context.Context, b *domain.Batch, src Source, show *domain.MediaItem,
	common render.Common, downloading int,
) (domain.Card, error) {
	s := b.Series
	in := render.TVInput{
		Common: common, Batch: b, Show: show, RottenTomatoes: a.RT.RottenTomatoes(ctx, s.IMDbID, s.Title),
		Downloading: downloading,
	}
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
		in.Scores = a.scores(ctx, show)
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
					// As in the reference: the media server is in trouble, so post without links now.
					a.Log.Warn("media server scan request failed; posting without links", "path", want[k].path, "err", err)
					return nil, false
				}
			}
		}
		a.Log.Info("not in the media server yet", "batch", batchName(b), "missing", len(missing),
			"check", b.MediaChecks+1, "of", a.WaitChecks)
		return nil, true
	}
	for _, k := range missing {
		it, err := a.Media.Find(ctx, want[k].guids, want[k].path, b.Kind, true)
		if err != nil {
			// As in the reference, any media server error means no links on this card at all.
			a.Log.Warn("media server lookup failed; posting without links", "batch", batchName(b), "err", err)
			return nil, false
		}
		if it != nil {
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

// scores asks the media server for an item's review scores. Without them the card shows what the
// *arr knows.
func (a *App) scores(ctx context.Context, it *domain.MediaItem) domain.Scores {
	if it == nil || a.Media == nil {
		return domain.Scores{}
	}
	s, err := a.Media.Scores(ctx, it.RatingKey)
	if err != nil {
		a.Log.Warn("media server scores unavailable", "item", it.RatingKey, "err", err)
	}
	return s
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
