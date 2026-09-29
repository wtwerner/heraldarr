# Workflow notes

Three lines per wave: what worked, what didn't, the rule it added to CLAUDE.md.

## Wave 0 (2026-09-28, one session)
- Worked: generating the oracle from the live Python by stubbing its I/O. The oracle caught a
  wrong assumption in the batcher timing tests before any agent saw them.
- Didn't: the plan split decoding (arr JSON → domain) into the arr-client issue, but the
  renderer's tests need it. Moved the pure decoders into Wave 0 so Wave 1 packages are independent.
- Gap: branch protection needs GitHub Pro on a private repo, so until the repo is public, "merge
  only on green CI" is the reviewer's job (`gh pr checks <n>` before merging).
- Rule: every Wave 1 package gets its acceptance tests written in Wave 0, gated by `implemented`.
