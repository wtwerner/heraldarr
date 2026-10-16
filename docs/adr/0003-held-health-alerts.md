# 3. Health alerts held until they last, checked against the app

Date: 2026-10-16 · Status: accepted

## Context

Sonarr, Radarr and Prowlarr send a notification when a health check starts failing and another
when it passes. Their only filter is "include warnings" on or off. Most indexer warnings are
blips: in one month of a real Prowlarr's history, two thirds of indexer outages cleared within
15 minutes and 85% within an hour, each one a pair of messages. Prowlarr and every *arr using the
indexer report the same outage. Switching warnings off hides the outages that matter too.

The apps key an issue by health check (`type`, e.g. `IndexerStatusCheck`): Health once when the
check starts failing, Health Restored once when it passes (with the last message), nothing in
between, even if the message or level changes. 15 minutes after an app starts it re-sends every
open issue. Webhook deliveries aren't retried.

## Decisions

- **Hold, then post.** An issue posts only once it has failed for `alerts.hold` (30 min; errors:
  `alerts.hold_errors`, 0). Restored within the hold → never posted.
- **Flapping is one issue.** A cleared issue is kept for one more hold; failing again in that time
  reopens it with its first failure time, so a check that fails on and off for longer than a hold
  still posts.
- **The app's API is the tiebreaker.** Before a held issue posts, and every `alerts.recheck`
  (15 min) while a posted one is open, heraldarr reads `GET /api/v3/health` (Prowlarr `/api/v1`).
  A check the app no longer reports is cleared (unposted) or resolved (posted). An unreadable API
  never blocks a post. Without this, a Health Restored lost while heraldarr restarted would leave
  the issue open for good, and the next real failure of that check would be taken for a repeat.
- **Resolved is an edit by default.** The posted card becomes "Resolved · lasted 2 h 14 min",
  which Discord doesn't notify; `alerts.restored: post` posts a resolved card instead.
- **Per source, no cross-app dedupe.** The same outage seen by Prowlarr and an *arr is two issues.
  Choosing which apps send warnings (in `alerts.sources`, or in each app's connection) is simpler
  and more predictable than matching messages across apps.
- **One destination, never public.** Alerts name hosts, indexers and errors.
- **Prowlarr is a source kind** with no route: it only sends alerts. Application Update events post
  at the next flush.
- **Seams:** `domain.AlertEvent`, `domain.Alert`, `domain.HealthCheck`, `domain.HealthReader`, and
  `Store.LoadAlerts/SaveAlert/DeleteAlert` (table `alerts`, JSON per alert like batches).

## Consequences

- A warning that matters shows up `hold` late. Errors don't wait by default.
- An escalation from warning to error under the same check sends no event, so it keeps the
  warning's hold unless the app reports it again.
- heraldarr calls each alert source's health API at most once per held alert and once per
  `recheck` per posted alert.
