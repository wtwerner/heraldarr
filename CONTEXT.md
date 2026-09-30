# Glossary

**Source**: one Sonarr or Radarr instance, named in config (`sonarr`, `radarr4k`). Its webhook
posts to `/hook/<name>`. Kind `tv` (Sonarr) or `movie` (Radarr).

**Import**: one "On Import" webhook event (`eventType: Download`): one episode file (with its
episodes) or one movie file. An **upgrade** (`isUpgrade`, or `deletedFiles` present) is never
announced.

**Item**: the unit that is announced once: an episode or a movie, keyed `<source>:ep:<id>` /
`<source>:movie:<id>` (`domain.ItemKey`).

**Batch**: items waiting together. TV: one batch per series per source (`sonarr:101`). Movies: one
batch per source (`radarr:movies`). A batch becomes one card (TV), one card per movie, or one
digest.

**Quiet window**: how long a batch must receive nothing new before it is **due**.
- `quiet_episodes` (5 min) when the batch is **following**.
- `quiet_backlog` (30 min) otherwise: back-catalog seasons trickle in over hours (`backlog.mode:
  quiet` only; otherwise back catalog goes into runs).
- `quiet_movies` (5 min) for movie batches.
- **Max hold** (4 h): due this long after the batch's first item, however busy.

**Following**: the batch is new episodes people watch as they air. True when every episode aired
within `following_window` (14 days), or every season in the batch already had files before it.
With back-catalog runs on (`backlog.mode` lead or complete), each episode is decided by its air date
alone: aired within `following_window`, or due within `following_early` (1 day; streaming releases
often land before the air date the *arr has).

**Back catalog**: episodes that aired before `following_window`. Requesting old seasons makes them
arrive one download at a time over hours or days.

**Run**: the back-catalog episodes of one series (or one season, `backlog.scope: season`) arriving
together, in a batch of their own (`sonarr:101:backlog`, `sonarr:101:s3`). Its first card is posted
once; later episodes are **folded into** it: the card is **edited** (`backlog.edit`, silent in
Discord) or, when it can't be, they are marked announced without a post. A run with nothing
pending ends after `backlog.idle` (24 h) without an import.
- **Lead** (`backlog.mode: lead`, default): the first card goes out once the first episodes have
  been quiet for `backlog.settle` (5 min; a season pack lands in a minute or two).
- **Complete**: the first card waits until the *arr's download **queue** has nothing more for the
  run, up to `backlog.max_hold` (24 h).
- **Quiet**: no runs; back catalog uses `quiet_backlog` as in the reference implementation.

**Digest**: one card for `digest_from` (4) or more movies arriving together: a line per movie and an
image grid.

**Reannounce window**: an item posted within `reannounce_after` (30 days) is never queued again
(delete + re-import doesn't post twice).

**Wait for the library**: before posting, the media server is asked for each item so the card can
deep-link it. Missing → a partial scan is requested and the batch waits (`wait_interval` ×
`wait_checks`), then posts without the link.

**Card**: the renderer's output (`domain.Card`): headline ("New series", "New season",
"3 new episodes", "New movie", "New in 4K"), title, lines, facts, poster, gallery, footer, buttons.
**Layouts** are its Discord encodings, tried in order: `v2` (Components V2), `embed`,
`embed, inline links`.

**Destination**: a Discord webhook plus username/avatar. **Public** destinations are read by
friends; they never show technical details.

**Route**: which sources post to which destination, with a **style** (label such as "4K", accent
color, `tech_details`).

**Oracle**: `testdata/expected/`, the reference implementation's output for each **scenario** in
`testdata/scenarios/`. **Parity** means matching it exactly.
