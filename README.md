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

**3. Edit `config/config.yaml`.** Set each source's `url` to where heraldarr can reach it.

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

**5. Connect Sonarr and Radarr.** *Coming soon:* `heraldarr setup` will create these connections
for you from `config.yaml` ([#28](https://github.com/wtwerner/heraldarr/issues/28)). Until then,
in each Sonarr/Radarr: Settings → Connect → + → **Webhook**:

- Notification triggers: **On Import** only
- URL: `http://<heraldarr host>:8790/hook/<source name>`, e.g. `/hook/sonarr`
- Method: POST; Username `heraldarr`, Password the contents of `config/secrets/webhook_password`

**6. Press Test.** A green tick means the URL and password work. heraldarr accepts the Test event
and posts nothing for it. A 401 means the username or password is wrong, a 404 the source name in
the URL.

**7. The first card.** The next import is posted once its series has been quiet for 5 minutes
(30 for back catalog; 5 for movies). With Plex, heraldarr first makes sure Plex has the item, so
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
| `heraldarr preview radarr 12,34 -to private` | render items already on disk as new and post them to a non-public destination (without `-to`: print the JSON) |
| `heraldarr preview sonarr 7 S02` | same for a series, a season or `S02E05,S02E06` |
| `heraldarr flush` | post everything pending now (asks the running server) |
| `heraldarr health` | exit 0 if the running server is healthy (the image's health check) |
| `heraldarr import-legacy DIR` | import state from the Python predecessor's `data/` folder (`posted.json`, `rt_cache.json`, `history.jsonl`); stop the server first |
| `heraldarr version` | print the version |

Endpoints: `POST /hook/{source}`, `GET /health` (no auth), `GET /pending`, `POST /flush`.

Set `HERALDARR_DEBUG=1` to log every webhook, including ignored ones such as Test.

## FAQ

**Why not Tautulli's grouped notifications?** Tautulli groups what Plex adds within one fixed
delay, so a season that downloads over an hour still arrives as several posts. heraldarr waits
until the series goes quiet, with a short window for episodes people follow as they air and a long
one for back catalog, and caps the wait at 4 hours. It reads the *arr events rather than Plex, so it
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
