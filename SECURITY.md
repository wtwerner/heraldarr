# Security

## Reporting a vulnerability

Report it privately through GitHub:
[Security → Report a vulnerability](https://github.com/wtwerner/heraldarr/security/advisories/new).
Please don't open a public issue or discussion. Include what an attacker needs (network access,
a secret, a crafted webhook) and what they get. The fix and the advisory are published together,
crediting you unless you'd rather not be.

Only the latest release is supported.

## Your secrets

heraldarr holds several secrets. Treat each like a password:

- **Discord webhook URLs**: anyone with one can post to your channel as that webhook.
- **Sonarr/Radarr API keys** and the **Plex token**: full access to those apps.
- **`server.auth` password**: lets anyone who can reach heraldarr queue announcements.

Keep them in files (`{file: /config/secrets/…}`), not inline in `config.yaml` or in the
environment. Before you paste a config, a log or a screenshot into an issue, remove every one of
them. If one leaks, rotate it: delete and recreate the Discord webhook, regenerate the API key in
the app's Settings → General, and sign the Plex token's device out (Plex → Settings → Authorized
Devices).

heraldarr's webhook listener speaks plain HTTP with basic auth. Keep it on your local network next
to Sonarr and Radarr, and don't expose port 8790 to the internet.
