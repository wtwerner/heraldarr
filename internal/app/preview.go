package app

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"sync"

	"github.com/wtwerner/heraldarr/internal/domain"
)

// Preview renders imports of one source as if they had just arrived, without touching the
// batcher or the posted ledger. With dest nil it returns each card's layouts; otherwise it posts
// them to dest, which must not be public.
func (a *App) Preview(ctx context.Context, source string, imps []domain.Import, dest *domain.Destination,
) ([][]domain.Layout, error) {
	src, ok := a.Sources[source]
	if !ok {
		return nil, fmt.Errorf("unknown source %q", source)
	}
	if dest != nil && dest.Public {
		return nil, fmt.Errorf("destination %q is public; previews go to a private one", dest.Name)
	}
	now := a.Clock.Now()
	b := &domain.Batch{
		Key: source + ":preview", Source: source, Kind: src.Kind, First: now, Last: now,
		MediaChecks: a.WaitChecks,
	}
	for _, imp := range imps {
		switch {
		case imp.Series != nil:
			if b.Series == nil {
				b.Series, b.Episodes = imp.Series, map[domain.ItemKey]domain.Episode{}
			}
			for _, e := range imp.Episodes {
				b.Episodes[domain.EpisodeKey(source, e.ID)] = e
			}
		case imp.Movie != nil:
			if b.Movies == nil {
				b.Movies = map[domain.ItemKey]domain.Movie{}
			}
			b.Movies[domain.MovieKey(source, imp.Movie.ID)] = *imp.Movie
		}
	}
	if b.Len() == 0 {
		return nil, errors.New("nothing to preview")
	}
	col := &collector{}
	p := *a
	if dest == nil {
		p.Notifier = col
		dest = &domain.Destination{Name: "preview"}
	}
	if _, _, _, err := p.deliver(ctx, b, true, dest, false); err != nil {
		return nil, err
	}
	return col.layouts, nil
}

// Edit is never called: a preview batch has no run.
func (c *collector) Edit(context.Context, domain.Destination, string, domain.Layout) error {
	return errors.New("preview: nothing to edit")
}

type collector struct {
	mu      sync.Mutex
	layouts [][]domain.Layout
}

func (c *collector) Post(_ context.Context, _ domain.Destination, l []domain.Layout) (domain.PostResult, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.layouts = append(c.layouts, l)
	return domain.PostResult{Layout: l[0].Name, MessageID: strconv.Itoa(len(c.layouts))}, nil
}
