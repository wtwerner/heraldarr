---
name: ship
description: Take a finished issue from green tests to merged. Use when your issue's work is done and make check passes.
---

# Ship

The issue's work is done and `make check` is green. This takes it to merged on `main`, or to a
clear hand-off to the maintainer. Run the steps in order.

## 1. Pull request

1. `git fetch origin && git rebase origin/main`; re-run `make check`. Done when it is green on
   top of the current `main`.
2. `git push --force-with-lease -u origin HEAD`.
3. Open the PR if there is none: `gh pr create --title "<package>: <summary>" --body …` with
   `Closes #<issue>`, what changed, and every difference from the oracle. Done when
   `gh pr view --json number` returns the number.

## 2. Review loop

1. Spawn the `reviewer` subagent with: `Review PR #<n>.` Nothing else: it gathers its own context.
2. Post its full output as a PR comment (`gh pr comment <n> --body-file …`), headed
   `Review round <k>`. The comments are the audit trail.
3. Verdict `APPROVE` → go to step 3.
   Verdict `CHANGES` → fix every blocker and major (minors are your call), `make check`, commit,
   push, and reply in the PR with one line per finding: fixed (commit) or declined (why). Then
   run the reviewer again, with a fresh agent each round.
4. Stop after round 3 without `APPROVE`: this is a **hand-off**.

## 3. Gate

The maintainer merges it (a **hand-off**) when the PR touches a path in `.github/CODEOWNERS`
(the seams, the oracle, CI, agent config, ADRs, the Dockerfile). GitHub enforces this: those PRs
need the code owner's review, which only the maintainer can give. Check with
`gh pr diff <n> --name-only`.
Everything else continues.

## 4. CI and merge

1. `gh pr checks <n> --watch`. Red → read the log (`gh run view --log-failed`), fix, push, and
   go back to step 2 (a fix is a change the reviewer hasn't seen).
2. `gh pr merge <n> --squash --delete-branch`. The `main` ruleset requires the branch to be up to
   date with `main` and CI green on it: when `main` moved, rebase (step 1.1), push, and repeat
   step 4. Done when `gh pr view <n> --json state` says `MERGED`. Merging never uses `--admin`
   (a hook blocks it); a PR the ruleset refuses for code-owner review is a **hand-off**.
3. Comment on the issue: merged in #<n>, plus anything a later agent should know.

## Hand-off

Add the `needs-human` label, comment on the PR with why (gate path, unresolved findings, or a
failure you can't fix), and finish with that reason as your final message.
