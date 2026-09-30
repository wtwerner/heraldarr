# 2. Back-catalog runs: one card per show, edited as episodes land

Date: 2026-09-30 · Status: accepted

## Context

The reference implementation batches back catalog with a 30-minute quiet window and decides
"following" partly by file counts: a batch is following when every season in it already had files.
In use, requesting old seasons makes Sonarr grab each episode separately, and they import one at a
time, often 20 to 60 minutes apart, for hours or days. Two things went wrong:

- Once the first card of a season went out, that season "already had files", so every later
  episode counted as following and posted alone after 5 minutes (issue #16 was a narrow case of this).
- Even classified correctly, the gaps are longer than any quiet window worth waiting, so a season
  still became many cards.

No quiet window can tell that more episodes are coming. The *arr can: its download queue lists every
grabbed episode until it imports.

## Decisions

- **Following by air date alone.** Each episode is recent (aired within `following_window`, or due
  within `following_early`, see #15) or back catalog. Recent episodes batch per series with
  `quiet_episodes`, as before, so a show airing now is unaffected by a backfill of its old seasons.
- **Runs.** Back-catalog episodes go into their own batch per series (or per season), which keeps a
  record of what it announced and the Discord message it posted. Later episodes are folded into that
  card rather than posted: the card is re-rendered for the whole run and the message edited
  (edits don't notify), or, with editing off or impossible, they're just marked announced.
- **Per show by default, per season configurable.** A backfill of several seasons is one card, not
  one per season.
- **The queue decides "more coming".** `GET /api/v3/queue/details?seriesId=` feeds the card's
  "N more on the way" line and, in `complete` mode, holds the first card until the run's queue is
  empty (capped by `backlog.max_hold`). Only the run's own back catalog counts: not new episodes
  of the same show, not other seasons when runs are per season, and not downloads that won't import
  on their own (failed, ignored, import blocked). An unreadable queue never holds a card.
- **Parity stays testable.** `backlog.mode: quiet` is the reference behavior and the batcher's zero
  value, so the oracle tests run unchanged. The default for new configs is `lead`.
- **Seams:** `ArrClient.Queue`, `Notifier.Edit`, `Batch.Backlog/Season/Run`, and `domain.ErrRefused`.
  A run lives in the batch's JSON, so the Store interface and schema don't change.

## Consequences

- A run ends `backlog.idle` (24 h) after its last import. A straggler after that, or another season
  requested later, gets a new card. Within the window a new request is folded into the old card
  silently.
- A deleted card, or one Discord won't take as an edit, stops the edits for that run. Its remaining
  episodes are announced silently rather than posted one by one.
- The post history records the first card only; edits are logged, not written to history.
