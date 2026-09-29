# heraldarr

Announce new movies and episodes to the Discord channel your friends read, without the spam.
Self-hosted, no cloud service, no bot to invite.

> **Status: pre-release.** The Go port is in progress and there is no image yet.

heraldarr receives Sonarr's and Radarr's *On Import* webhooks, waits until a series has stopped
arriving, and posts **one card per series** ("New season", "3 new episodes") or a **digest** when
several movies land together. Cards show ratings, cast and runtime, and have buttons that open the
item in Plex for every friend the library is shared with. Upgrades are never announced, and nothing
is announced twice within 30 days.

## How it compares

| | Sonarr/Radarr Discord | Tautulli (grouped) | Notifiarr | heraldarr |
|---|---|---|---|---|
| One post per series across separate downloads | no (per release) | yes (one delay) | no | yes (5 min for new episodes, 30 min for back catalog) |
| Digest card for many movies | no | no | no | yes |
| Hides upgrades and repeats | upgrades only | partly | per trigger | yes, across instances |
| Deep links that work for shared friends | no | no | no | yes |
| Runs without a cloud service or bot | yes | yes | no | yes |

## Running it

```bash
docker run -d --name heraldarr -p 8790:8790 -v /path/to/config:/config ghcr.io/wtwerner/heraldarr
```

Copy [config.example.yaml](config.example.yaml) to `/config/config.yaml`. Secrets can be inline, read from a
file (`{file: …}`, recommended) or read from the environment (`{env: …}`). Then in each Sonarr/Radarr, add a
**Webhook** connection with only **On Import** checked, pointing at `http://<host>:8790/hook/<source name>` with
the username and password from `server.auth`. Its **Test** button should succeed.

| Command | |
|---|---|
| `heraldarr validate` | check the configuration |
| `heraldarr preview radarr 12,34 -to private` | render items already on disk as new and post them to a non-public destination (without `-to`: print the JSON) |
| `heraldarr preview sonarr 7 S02` | same for a series, a season or `S02E05,S02E06` |
| `heraldarr flush` | post everything pending now |
| `heraldarr import-legacy DIR` | import an older `arr-discord` data folder |

Endpoints: `POST /hook/{source}`, `GET /health` (no auth), `GET /pending`, `POST /flush`.

## License

MIT
