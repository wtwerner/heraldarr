#!/bin/bash
# Stop hook: an agent can't finish while `make check` is red. On a second stop attempt in a row
# (stop_hook_active) it lets the agent stop, so a check it can't fix never loops forever.
input=$(cat)
if grep -q '"stop_hook_active": *true' <<<"$input"; then
  exit 0
fi
cd "${CLAUDE_PROJECT_DIR:-.}" || exit 0
# Nothing changed on this branch and nothing uncommitted: nothing to check.
if git diff --quiet HEAD 2>/dev/null && [ -z "$(git ls-files --others --exclude-standard)" ] \
   && [ "$(git rev-parse HEAD)" = "$(git merge-base HEAD origin/main 2>/dev/null || git rev-parse HEAD)" ]; then
  exit 0
fi
if ! out=$(mise exec -- make check 2>&1); then
  echo "make check failed; fix it before stopping:" >&2
  tail -40 <<<"$out" >&2
  exit 2
fi
