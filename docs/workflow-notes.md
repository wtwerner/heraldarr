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

## Wave 1 (2026-09-29, 7 agents in parallel)
- Worked: the Wave 0 parity tests made agents nearly self-supervising, and all 7 packages merged
  with no oracle or test-kit tampering. Agents filed `divergence` issues (#9, #11–#17) instead of
  quietly "improving" behavior. `/ship` took review and merging off my plate after the first two.
- Didn't: before `/ship`, reviews ran in separate sessions and left no trail on the PRs. Agents
  started before a file existed can't see it (the `reviewer` type, the skill). One push to `main`
  triggered no CI run.
- Rule: reviews are posted to the PR, every round (`/ship` step 2).

## Wave 2 (integration, one session)
- Worked: the full-pipeline parity test (17 scenarios over HTTP, real store) proved the wiring in
  one go. The smoke test of the real binary caught a CLI flag bug that no unit test would have.
- Didn't: three review rounds each found real gaps in *my* tests: a shutdown race; a test that
  never reached its code; stated behaviors that survived mutation. The fix each time was to make
  the test fail on the old code first.
- Rule: a test for a stated behavior must be shown to fail when that behavior is broken (mutate,
  watch it go red, restore). Reviewers run mutation checks.
