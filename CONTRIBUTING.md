# Contributing

Thanks for helping. Bugs and ideas go in [issues](https://github.com/wtwerner/heraldarr/issues/new/choose);
security problems go through [SECURITY.md](SECURITY.md), not issues.

## Build and check

You need Go (the version in `go.mod`; `mise install` picks it up from `mise.toml`) and
[golangci-lint](https://golangci-lint.run/) v2. Nothing else: SQLite is pure Go, no cgo.

```bash
make check     # lint + go test -race ./... — the gate for every PR, same as CI
make build     # bin/heraldarr
make docker    # heraldarr:dev image
make fmt       # format
```

A PR is ready when `make check` is green.

## Parity and the oracle

heraldarr replaced an earlier service, and its first job is to behave exactly like it. The
**oracle** is `testdata/expected/`: what that reference implementation did with each fictional
scenario in `testdata/scenarios/`: which imports it queued, when batches became due, and the exact
Discord JSON it posted. Parity tests feed the same scenarios through the Go packages and compare
the output as semantically equal JSON.

Match the oracle even where it looks wrong, and open an issue labelled `divergence` describing the
better behavior; parity first, improvements after, each as its own change. Don't edit
`testdata/expected/` or `testdata/scenarios/` by hand: they are generated (`make harness`) with the
reference implementation, which only the maintainer has. If you need a new scenario, ask in the PR.

## How changes are shaped

- **One package per PR.** Each package under `internal/` changes on its own. Packages meet only
  through the types and interfaces in `internal/domain`; if that seam has to change, keep the change
  minimal, say so at the top of the PR, and expect it to be reviewed and merged first.
- **Tests that can fail.** A test for a behavior must go red when that behavior breaks: break the
  code, watch the test fail, restore it.
- **Fictional test data.** Invented titles, `example.org` URLs, placeholder IDs. Never a real
  webhook URL, token or API key.
- **Stdlib first.** A new module dependency needs a line in the PR on why the standard library
  isn't enough.
- **Public destinations stay friendly.** Cards on public destinations show titles, scores and
  links, never file paths, sizes or release groups.
- Words like *batch*, *following* and *quiet window* have precise meanings: see
  [CONTEXT.md](CONTEXT.md). Past decisions and their reasons are in [docs/adr/](docs/adr/).

The PR template asks for the issue it closes, what changed, and any difference from the oracle.

## AI-assisted contributions

Welcome, and much of this repository was written that way. The bar is the same as for any PR: you
understand every line you submit, you ran `make check`, and the tests prove the behavior you claim.
You are the author and answer the review.
