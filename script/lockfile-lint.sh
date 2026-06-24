#!/usr/bin/env bash
# Lockfile lint check with rich output for CI.
# Runs gh-actions-lock in read-only mode and produces annotations,
# grouped log output, a lockfile diff, and a job summary.
set -o pipefail

binary="${1:?usage: lockfile-lint.sh <gh-actions-lock-command>}"

# Run the check, capture JSON stdout separately from stderr
exit_code=0
json_output=$($binary --no-fix --no-interactive --json=valid,findings,workflows 2>/dev/null) || exit_code=$?

# If exit code 2, the tool itself failed — re-run to show stderr
if [ "$exit_code" -eq 2 ]; then
  echo "::error::gh-actions-lock failed:"
  $binary --no-fix --no-interactive 2>&1 || true
  exit 2
fi

valid=$(echo "$json_output" | jq -r '.valid // empty')
num_findings=$(echo "$json_output" | jq -r '.findings // [] | length')

# Log findings as annotations
if [ "$num_findings" != "0" ] && [ -n "$num_findings" ]; then
  echo ""
  echo "::group::Findings ($num_findings)"
  echo "$json_output" | jq -r '.findings[] | "::error file=\(.workflow)::[\(.category)] \(.dependency): \(.detail)"'
  echo "::endgroup::"
fi

# Log per-workflow status
echo ""
echo "::group::Workflow status"
echo "$json_output" | jq -r '.workflows[] | (if .valid then "pass" else "FAIL" end) + " " + .path + " (" + (.findings | length | tostring) + " finding(s))"'
echo "::endgroup::"

# Show what a fix would change
if [ "$exit_code" -ne 0 ]; then
  echo ""
  echo "::group::Lockfile diff (what gh actions-lock would change)"
  cp .github/workflows/actions.lock .github/workflows/actions.lock.bak 2>/dev/null || true
  $binary --no-interactive 2>/dev/null || true
  diff -u .github/workflows/actions.lock.bak .github/workflows/actions.lock || true
  git checkout -- . 2>/dev/null
  echo "::endgroup::"
fi

# Job summary
if [ -n "$GITHUB_STEP_SUMMARY" ]; then
  {
    echo "## Lockfile lint"
    echo ""
    if [ "$valid" = "true" ]; then
      echo "Lockfile is in sync."
    else
      echo "Lockfile is **out of sync**. Run \`gh actions-lock\` to fix."
      echo ""
      echo "### Findings"
      echo ""
      echo "| Workflow | Category | Dependency | Detail |"
      echo "|----------|----------|------------|--------|"
      echo "$json_output" | jq -r '.findings[] | "| `\(.workflow)` | \(.category) | `\(.dependency)` | \(.detail) |"'
    fi
  } >> "$GITHUB_STEP_SUMMARY"
fi

exit $exit_code
