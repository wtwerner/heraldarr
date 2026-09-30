#!/bin/bash
# PreToolUse hook (Bash): commands an agent must never run in this repo. The GitHub ruleset is the
# real protection; this stops a session from using the maintainer's credentials to get around it.
# Exit 2 blocks the command and shows the reason to the agent.
cmd=$(python3 -c 'import json,sys; print(json.load(sys.stdin).get("tool_input",{}).get("command",""))' 2>/dev/null)
block() { echo "blocked by .claude/hooks/guard-bash.sh: $1. Hand off to the maintainer instead (see /ship)." >&2; exit 2; }

if grep -Eq 'gh[[:space:]]+pr[[:space:]]+merge' <<<"$cmd" && grep -Eq '(^|[[:space:]])--admin([[:space:]]|=|$)' <<<"$cmd"; then
  block "merging with --admin bypasses the ruleset"
fi
if grep -Eq 'git[[:space:]]+push' <<<"$cmd" && grep -Eq '([[:space:]]|:)(refs/heads/)?(main|master)([[:space:]]|$)' <<<"$cmd"; then
  block "pushing to main; every change goes through a PR"
fi
if grep -Eq 'git[[:space:]]+push' <<<"$cmd" && grep -Eq '[[:space:]]HEAD([[:space:]]|$)' <<<"$cmd" \
   && [ "$(git -C "${CLAUDE_PROJECT_DIR:-.}" rev-parse --abbrev-ref HEAD 2>/dev/null)" = main ]; then
  block "pushing HEAD while main is checked out"
fi
if grep -Eq 'gh[[:space:]]+(repo[[:space:]]+(edit|delete|rename|archive)|ruleset[[:space:]]+(create|edit|delete))' <<<"$cmd"; then
  block "changing repository settings or rulesets"
fi
if grep -Eq 'gh[[:space:]]+api' <<<"$cmd" && grep -Eq '(-X|--method)[[:space:]=]*(PUT|PATCH|POST|DELETE)' <<<"$cmd" \
   && grep -Eq 'rulesets|/protection|/actions/permissions|security_and_analysis|/collaborators|/keys|vulnerability|repos/[^/[:space:]]+/[^/[:space:]]+([[:space:]]|$)' <<<"$cmd"; then
  block "changing repository settings, protections or access"
fi
exit 0
