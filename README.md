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

## Configuration

See [config.example.yaml](config.example.yaml). Secrets can be read from files, which is recommended.

## License

MIT
