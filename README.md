# heraldarr

Announce new movies and episodes to the Discord channel your friends read, without the spam.
Self-hosted, no cloud service, no bot to invite.

> **Status: pre-release.** The first image is published at launch. Until then, build it yourself
> with `make docker` and use `heraldarr:dev` in place of `ghcr.io/wtwerner/heraldarr`.

heraldarr receives Sonarr's and Radarr's *On Import* webhooks, waits until a series has stopped
arriving, and posts **one card per series** ("New season", "3 new episodes") or a **digest** when
several movies land together. Cards show ratings, cast and runtime, and have buttons that open the
item in Plex for every friend the library is shared with. Upgrades are never announced, and nothing
is announced twice within 30 days.

## Screenshots

<!-- Card images go here: a single-series card, a "3 new episodes" card, a movie digest. -->

*Coming soon: a series card, an episodes card and a movie digest.*

## How it compares

| | Sonarr/Radarr Discord | Tautulli (grouped) | Notifiarr | heraldarr |
|---|---|---|---|---|
| One post per series across separate downloads | no (per release) | yes (one delay) | no | yes (5 min for new episodes, 30 min for back catalog) |
| Digest card for many movies | no | no | no | yes |
| Hides upgrades and repeats | upgrades only | partly | per trigger | yes, across instances |
| Deep links that work for shared friends | no | no | no | yes |
| Runs without a cloud service or bot | yes | yes | no | yes |

## Quickstart

About five minutes, with Docker and Compose. You need a Discord channel webhook for each channel
you post to (channel settings → Integrations → Webhooks → New Webhook → Copy Webhook URL), and the
API key of each Sonarr/Radarr (Settings → General → Security).

**1. Get the files.**

```bash
mkdir -p heraldarr/config/secrets && cd heraldarr
base=https://raw.githubusercontent.com/wtwerner/heraldarr/main
curl -fsSLo docker-compose.yml "$base/docker-compose.example.yml"
curl -fsSLo config/config.yaml "$base/config.example.yaml"
```

In `docker-compose.yml`, set `user:` to your own IDs (`echo "$(id -u):$(id -g)"`), so heraldarr can
read the secrets you write next and create `config/data/`.

**2. Write the secrets, one per file.** The names match the `{file: …}` entries in
`config.yaml`.

```bash
openssl rand -hex 16 > config/secrets/webhook_password   # Sonarr/Radarr use this to reach heraldarr
echo 'SONARR-API-KEY'  > config/secrets/sonarr_api_key
echo 'RADARR-API-KEY'  > config/secrets/radarr_api_key
echo 'https://discord.com/api/webhooks/…' > config/secrets/discord_tv
echo 'https://discord.com/api/webhooks/…' > config/secrets/discord_movies
echo 'PLEX-TOKEN'      > config/secrets/plex_token      # with Plex only, see below
chmod 600 config/secrets/*
```

The Plex token is your server's `X-Plex-Token`: Plex's support article "Finding an authentication
token" shows where to copy it.

Instead of a token or API key file, heraldarr can read them from the apps' own config files:
`media_server.preferences_xml` from Plex's `Preferences.xml`, and a source's `config_xml` from that
*arr's `config.xml`. Those files live outside `config/`, so mount each one into the container,
read-only, under `volumes:` in `docker-compose.yml`:

```yaml
      - "/path/to/plex/Library/Application Support/Plex Media Server/Preferences.xml:/plex/Preferences.xml:ro"
      - /path/to/sonarr/config.xml:/sonarr/config.xml:ro
```

With `docker run`, the same is `-v /path/to/sonarr/config.xml:/sonarr/config.xml:ro`. Then, in
`config.yaml`, replace `token:` with `preferences_xml: /plex/Preferences.xml`, or a source's
`api_key:` with `config_xml: /sonarr/config.xml` (each takes one or the other, not both).

**3. Edit `config/config.yaml`.** Set each source's `url` to where heraldarr can reach it, and
`server.public_url` to where Sonarr and Radarr can reach heraldarr: `http://heraldarr:8790` when
they share a Docker network with it, otherwise `http://<heraldarr host>:8790`.

- With Plex: set `media_server.url`, and `path_map` so that a file path as Sonarr/Radarr see it
  becomes the path Plex sees (`/data/media/` → `/media/` in the example). The "Open in Plex"
  button needs both.
- No Plex: delete the `media_server` block. Cards post without the "Open in Plex" button.

Then check it:

```bash
docker compose run --rm heraldarr validate
# /config/config.yaml: ok (2 sources, 2 destinations, 2 routes)
```

**4. Start it.**

```bash
docker compose up -d
docker compose logs heraldarr     # "heraldarr listening" with your sources
```

**5. Connect Sonarr and Radarr.** `setup` adds a **Webhook** connection named `heraldarr` to each
of them (or updates the one already there): `<public_url>/hook/<source name>`, POST, the username
and password from `server.auth`, and only **On Import** checked.

```bash
docker compose exec heraldarr /heraldarr setup -dry-run   # show what would change
docker compose exec heraldarr /heraldarr setup
# sonarr: created "heraldarr" (On Import → http://heraldarr:8790/hook/sonarr), test event accepted
```

**6. Check the Test event.** Each *arr sends heraldarr a Test event before it saves, so
`test event accepted` means the URL and password work (heraldarr posts nothing for it). If an *arr
reports `Unable to send test message`, it can't reach `public_url`, or the password differs from
the one the running server loaded. A 401 means the username or password is wrong, a 404 the
source name in the URL. Run `setup` again after changing `config.yaml`: it updates the connections
in place.

To set a connection up by hand instead: Settings → Connect → + → **Webhook**, **On Import** only,
URL `<public_url>/hook/<source name>`, method POST, Username `heraldarr`, Password the contents of
`config/secrets/webhook_password`, then press Test.

**7. The first card.** The next import is posted once its series has been quiet for 5 minutes
(5 for movies too). Back catalog (episodes that aired more than two weeks ago) is grouped into one
run per show: its card posts when the first episodes land, and the rest of the run is added to
that card by editing it, which doesn't notify anyone. See `backlog` in the example config for
other modes, such as one card once everything queued has arrived, or one run per season. With Plex, heraldarr first makes sure Plex has the item, so
the button works: if it doesn't, heraldarr asks Plex to scan that folder and checks again up to 4
times, 3 minutes apart. Then it does one final fresh lookup and posts, without the button for
anything Plex still hasn't found. To post what is waiting now, skipping that wait:

```bash
docker compose exec heraldarr /heraldarr flush
```

To see a card without waiting for a download, add a destination named `private` with
`public: false` (a test channel's webhook) and preview something already in your library, by its
Sonarr/Radarr ID (the `id` field of `/api/v3/series` or `/api/v3/movie`):

```bash
docker compose exec heraldarr /heraldarr preview radarr 12 -to private
```

## Reference

Secrets can be inline, read from a file (`{file: …}`, recommended) or read from the environment
(`{env: …}`). Every command takes `-config PATH` (default `$HERALDARR_CONFIG` or
`/config/config.yaml`).

| Command | |
|---|---|
| `heraldarr serve` | run the webhook receiver and poster (the image's default) |
| `heraldarr validate` | check the configuration |
| `heraldarr setup [-name NAME] [-dry-run]` | create or update the Webhook connection in every Sonarr/Radarr |
| `heraldarr preview radarr 12,34 -to private` | render items already on disk as new and post them to a non-public destination (without `-to`: print the JSON) |
| `heraldarr preview sonarr 7 S02` | same for a series, a season or `S02E05,S02E06` |
| `heraldarr flush` | post everything pending now (asks the running server) |
| `heraldarr health` | exit 0 if the running server is healthy (the image's health check) |
| `heraldarr import-legacy DIR` | import state from the Python predecessor's `data/` folder (`posted.json`, `rt_cache.json`, `history.jsonl`); stop the server first |
| `heraldarr version` | print the version |

Endpoints: `POST /hook/{source}`, `GET /health` (no auth), `GET /pending`, `POST /flush`, `GET /metrics`.

## Monitoring

`GET /metrics` serves Prometheus metrics, behind the same `server.auth` as `/pending` (set
`basic_auth` in the scrape config):

| Metric | |
|---|---|
| `heraldarr_imports_total{source,result}` | webhooks received; `result` is `queued`, `upgrade`, `already_announced`, `ignored` (Test, Grab…), `invalid` or `error` |
| `heraldarr_posts_total{source,destination,layout}` | cards posted, by the layout Discord accepted (`v2`, then the embed fallbacks) |
| `heraldarr_delivery_failures_total{source}` | deliveries that failed; they are retried `retry_max` times |
| `heraldarr_pending_batches` | batches waiting to be posted |
| `heraldarr_media_server_waits_total` | deliveries postponed until the media server has the items |
| `heraldarr_last_post_timestamp_seconds` | the last post since start (0 until the first) |

A useful alert: `increase(heraldarr_delivery_failures_total[1h]) > 0` (a Discord webhook was
deleted, or Discord is down).

To be told when heraldarr itself dies, set `server.heartbeat_url` to a dead man's switch such as a
[healthchecks.io](https://healthchecks.io) check. heraldarr GETs it at most once a minute (in
practice every 60 to 90 seconds) after each flush that ran without a database error; the monitor
alerts when the pings stop. Give the check a period or grace of a few minutes. The URL can be a
secret (`{file: …}`). Failed pings are only logged at debug level.

Logs are text on stdout. `HERALDARR_LOG_FORMAT=json` writes one JSON object per line for log
collectors; `HERALDARR_DEBUG=1` adds debug lines.

Set `HERALDARR_DEBUG=1` to log every webhook, including ignored ones such as Test.

## FAQ

**Why not Tautulli's grouped notifications?** Tautulli groups what Plex adds within one fixed
delay, so a season that downloads over an hour still arrives as several posts. heraldarr waits
until the series goes quiet for episodes people follow as they air. Back catalog is different:
requesting old seasons makes them download one episode at a time over hours or days, so no quiet
window can hold it together. heraldarr asks Sonarr what is still queued and keeps the whole run on one
card, edited as episodes land. It reads the *arr events rather than Plex, so it
knows an upgrade from a new file, and it remembers what it posted for 30 days, so a delete and
re-import isn't announced twice. Its buttons open the item for friends the library is shared with.

**Does it need a Discord bot?** No. It posts through ordinary channel webhooks; link buttons work
on webhooks, so there is nothing to invite or keep online.

**Jellyfin?** Planned. Today the only media server is Plex, and it is optional: without one, cards
post without the "Open in" button.

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md). Security issues: [SECURITY.md](SECURITY.md).

## License

MIT
