# 1. Go, SQLite, Components V2 over plain webhooks, Docker image only

Date: 2026-09-28 · Status: accepted

## Context

heraldarr replaces a single-file Python service (stdlib only, JSON files for state) that has run
on the maintainer's server since 2026-09-28. It is being rebuilt as a public project, partly to
practise running several coding agents in parallel on one codebase.

## Decisions

- **Go.** One static binary, a tiny distroless image, the common choice for *arr-adjacent tools
  (Unpackerr, the Notifiarr client). Strong typing makes the package seams explicit for parallel work.
- **SQLite via `modernc.org/sqlite`** (pure Go, no cgo) for batches, the posted ledger, caches and
  history. JSON files re-read under a global lock don't survive concurrent writes or grow well.
- **Discord Components V2 over plain webhooks**, with classic-embed fallbacks. No bot to host or
  invite: link buttons work on webhooks (`?with_components=true`, flag `1<<15`). Bot-only features
  (interactive buttons, DMs) are out of scope until there is demand.
- **Parity before features.** The Python implementation is the oracle: `testdata/expected` is its
  output on fictional scenarios. The Go port matches it exactly; improvements come after, each as
  its own decision.
- **Docker image is the product.** Multi-arch (amd64/arm64) to GHCR from a tag via buildx. No
  goreleaser or binary releases until someone asks.
- **Secrets from files** (`{file: …}`), with `{env: …}` and inline as options: files keep them out
  of `docker inspect`, env vars suit Unraid templates.

## Consequences

- Contributors need Go 1.27 (pinned in `mise.toml`) and nothing else; regenerating the oracle needs
  the reference implementation, which only the maintainer has.
- The distroless image runs as `nonroot` (65532). Unraid users map `/config` and may need
  `--user 99:100`; the CA template (roadmap) sets it.
