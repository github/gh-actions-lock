#!/usr/bin/env bash
# Try any demo scenario from the design proposal yourself.
# Usage: demo/try-it.sh [scenario]
#
# Resets fixtures to a clean state before each run so scenarios are repeatable.
set -euo pipefail
cd "$(git rev-parse --show-toplevel)"

RESET="demo/vhs/reset-fixtures.sh"

# Colors
BOLD='\033[1m'
DIM='\033[2m'
CYAN='\033[36m'
GREEN='\033[32m'
YELLOW='\033[33m'
RESET_COLOR='\033[0m'

banner() { echo -e "\n${BOLD}${CYAN}── $1 ──${RESET_COLOR}\n"; }
comment() { echo -e "${DIM}# $1${RESET_COLOR}"; }
run() { echo -e "${GREEN}\$ $*${RESET_COLOR}"; "$@"; echo; }

# Enable the reachability check (branch_commits) for scenarios that need it.
# This undocumented endpoint 429s aggressively so it's off by default.
# Uses GH_ACTIONS_PIN_CONFIG to avoid mutating real user config.
enable_reachability() {
  _DEMO_CONFIG=$(mktemp /tmp/gh-actions-pin-demo-XXXXXX.yml)
  echo "reachability_check: true" > "$_DEMO_CONFIG"
  export GH_ACTIONS_PIN_CONFIG="$_DEMO_CONFIG"
}
disable_reachability() {
  unset GH_ACTIONS_PIN_CONFIG
  rm -f "${_DEMO_CONFIG:-}"
}
show_workflow_summary() {
  awk '/uses:/ { print }' "$1"
  local lock
  lock="$(dirname "$1")/actions.lock"
  if [[ -f "$lock" ]]; then
    echo
    echo "# $lock"
    cat "$lock"
  fi
}
show_lockfile() {
  local lock
  lock="$(dirname "$1")/actions.lock"
  if [[ -f "$lock" ]]; then
    echo "# $lock"
    cat "$lock"
  else
    echo "(no lockfile)"
  fi
}

# stage_workflow copies a demo fixture into a per-scenario scratch dir laid out
# like a real repo: `<scratch>/.github/workflows/<name>.yml`. If the fixture
# embeds a legacy inline `dependencies:` block, the helper splits it out into
# `<scratch>/.github/workflows/actions.lock` (v0.0.1 schema, stub owner/repo IDs).
# Emits the staged workflow path on stdout; the caller should `cd` to the
# scratch dir before invoking the CLI.
stage_workflow() {
  local fixture="$1"
  local scratch
  scratch="$(mktemp -d /tmp/gh-actions-pin-demo-XXXXXX)"
  mkdir -p "$scratch/.github/workflows"
  local name
  name="$(basename "$fixture")"
  local staged="$scratch/.github/workflows/$name"
  python3 - "$fixture" "$staged" "$scratch/.github/workflows/actions.lock" <<'PY'
import re, sys, pathlib
src, dst, lockpath = (pathlib.Path(p) for p in sys.argv[1:])
body = src.read_text()
m = re.search(r"\n(?:# Automatically generated[^\n]*\n)?dependencies:\n((?:[ \t]+#[^\n]*\n|[ \t]*-[^\n]*\n)+)", body)
pins = []
if m:
    for line in m.group(1).splitlines():
        s = line.strip()
        if s.startswith("- "):
            pin = s[2:].strip()
            pins.append(pin)
    body = body[:m.start()].rstrip() + "\n"
dst.write_text(body)
if pins:
    out = ["version: v0.0.1", "actions:"]
    for p in pins:
        # Split "owner/repo[/path]@ref:algo-hex" to surface ref + sha lines.
        at = p.index("@")
        colon = p.rindex(":")
        ref = p[at+1:colon]
        sha = p[colon+1:]
        out.append(f"  {p}:")
        out.append(f"    ref: {ref}")
        out.append(f"    sha: {sha}")
        out.append("    owner_id: 1")
        out.append("    repo_id: 1")
    out.append("workflows:")
    out.append(f"  .github/workflows/{dst.name}:")
    out.append("    dependencies:")
    for p in pins:
        out.append(f"      - {p}")
    lockpath.write_text("\n".join(out) + "\n")
PY
  echo "$scratch"
}

scenarios=(
  "check-autofix"
  "ci-failure"
  "sha-as-ref"
  "unreleased-commit"
  "upgrade-latest"
  "upgrade-version"
  "edit-repin"
  "ref-moved"
  "imposter-commit"
  "lockfile-forgery"
  "json-output"
)

usage() {
  echo "Usage: demo/try-it.sh <scenario>"
  echo ""
  echo "Available scenarios:"
  echo ""
  echo "  Onboarding"
  echo "    check-autofix      Auto-fix unpinned actions (non-interactive)"
  echo "    ci-failure         CI mode fails on ambiguous actions"
  echo "    sha-as-ref         Bare SHA triggers interactive tag picker"
  echo "    unreleased-commit  Unreleased commit warning + tag picker"
  echo ""
  echo "  Upgrading"
  echo "    upgrade-latest     Upgrade action to latest stable tag"
  echo "    upgrade-version    Upgrade to a specific version (--version)"
  echo "    edit-repin         Edit ref + re-pin (Dependabot workflow)"
  echo ""
  echo "  Change detection"
  echo "    ref-moved          Tag moved forward (routine release)"
  echo "    imposter-commit    Tag hijacked to fork-network commit (fork injection)"
  echo "    lockfile-forgery   Lockfile entry replaced with fork commit SHA"
  echo "    json-output        JSON output for CI integration"
  echo ""
  echo "  all                  Run all scenarios sequentially"
  exit 1
}

scenario_check_autofix() {
  banner "Auto-fix (non-interactive)"
  bash "$RESET"
  local scratch wf
  scratch="$(stage_workflow demo/workflows-check/ci.yml)"
  wf=".github/workflows/ci.yml"
  comment "Unpinned workflow — 4 actions including a nested composite with transitive deps"
  run grep uses: "$scratch/$wf"
  comment "Pin all actions (non-interactive auto-fix)"
  ( cd "$scratch" && run gh actions-pin check --no-interactive "$wf" )
  comment "Lockfile groups transitive deps by parent composite action"
  run show_lockfile "$scratch/$wf"
  comment "Subsequent check passes"
  ( cd "$scratch" && run gh actions-pin check "$wf" )
}

scenario_ci_failure() {
  banner "CI failure on ambiguous actions"
  bash "$RESET"
  local scratch wf
  scratch="$(stage_workflow demo/workflows-interactive/sha-as-ref.yml)"
  wf=".github/workflows/sha-as-ref.yml"
  comment "One action has a tag (fixable), one is a bare SHA (needs human)"
  run grep uses: "$scratch/$wf"
  comment "CI mode: auto-fix what you can, fail on what you cannot"
  ( cd "$scratch" && gh actions-pin check --no-interactive "$wf" ) || true
  echo -e "\n${YELLOW}Exit code: $?${RESET_COLOR}"
  echo -e "${DIM}# CI fails — developer runs 'gh actions-pin check' locally for interactive resolution${RESET_COLOR}\n"
}

scenario_sha_as_ref() {
  banner "Edge case: bare SHA without tag ref (interactive)"
  bash "$RESET"
  local scratch wf
  scratch="$(stage_workflow demo/workflows-interactive/sha-as-ref.yml)"
  wf=".github/workflows/sha-as-ref.yml"
  comment "Action uses a bare SHA instead of a tag"
  run grep uses: "$scratch/$wf"
  comment "The tool detects this and offers tag suggestions"
  echo -e "${GREEN}\$ gh actions-pin check $wf${RESET_COLOR}"
  ( cd "$scratch" && gh actions-pin check "$wf" )
}

scenario_unreleased_commit() {
  banner "Edge case: unreleased commit (interactive)"
  bash "$RESET"
  local scratch wf
  scratch="$(stage_workflow demo/workflows-interactive/unreleased-commit.yml)"
  wf=".github/workflows/unreleased-commit.yml"
  comment "Action pinned to a commit with no tagged release"
  run grep uses: "$scratch/$wf"
  comment "The tool warns about unreleased code and offers releases"
  echo -e "${GREEN}\$ gh actions-pin check $wf${RESET_COLOR}"
  ( cd "$scratch" && gh actions-pin check "$wf" )
}

scenario_upgrade_latest() {
  banner "Upgrade to latest"
  bash "$RESET"
  local scratch wf
  scratch="$(stage_workflow demo/workflows-upgrade/ci.yml)"
  wf=".github/workflows/ci.yml"
  comment "Currently pinned to older versions"
  run show_workflow_summary "$scratch/$wf"
  comment "Upgrade actions/checkout to latest"
  ( cd "$scratch" && run gh actions-pin upgrade --action actions/checkout "$wf" )
  comment "Updated refs and lockfile"
  run show_workflow_summary "$scratch/$wf"
}

scenario_upgrade_version() {
  banner "Upgrade to a specific version"
  bash "$RESET"
  local scratch wf
  scratch="$(stage_workflow demo/workflows-upgrade/ci.yml)"
  wf=".github/workflows/ci.yml"
  comment "Currently pinned to actions/checkout@v4.2.0"
  run grep checkout "$scratch/$wf"
  comment "Upgrade to a specific version (v5), not latest"
  ( cd "$scratch" && run gh actions-pin upgrade --action actions/checkout --version v5 "$wf" )
  comment "Verify the pinned version"
  run grep checkout "$scratch/$wf"
}

scenario_edit_repin() {
  banner "Edit and re-pin (Dependabot workflow)"
  bash "$RESET"
  local scratch wf
  scratch="$(stage_workflow demo/workflows-upgrade/ci.yml)"
  wf=".github/workflows/ci.yml"
  comment "Simulate a Dependabot PR: bump checkout v4.2.0 -> v5"
  run grep checkout "$scratch/$wf"
  comment "Edit the ref (like Dependabot would)"
  run sed -i '' 's/checkout@v4.2.0/checkout@v5/g' "$scratch/$wf"
  comment "Remove stale lockfile so check can re-resolve"
  rm -f "$scratch/.github/workflows/actions.lock"
  echo -e "${GREEN}\$ rm .github/workflows/actions.lock${RESET_COLOR}\n"
  comment "Re-pin with check"
  ( cd "$scratch" && run gh actions-pin check --no-interactive "$wf" )
  comment "New lockfile written"
  run show_lockfile "$scratch/$wf"
}

scenario_ref_moved() {
  banner "Ref moved — routine update"
  bash "$RESET"
  local scratch wf
  scratch="$(stage_workflow demo/workflows-pwned/5-pinned-before-update.yml)"
  wf=".github/workflows/5-pinned-before-update.yml"
  comment "Workflow pinned before tag moved forward (normal release)"
  run show_workflow_summary "$scratch/$wf"
  comment "Check detects the tag now points to a newer commit"
  ( cd "$scratch" && run gh actions-pin check "$wf" )
}

scenario_imposter_commit() {
  banner "Imposter commit — fork injection"
  bash "$RESET"
  enable_reachability
  trap disable_reachability EXIT
  local scratch wf
  scratch="$(stage_workflow demo/workflows-pwned/1-pinned-before-hijack.yml)"
  wf=".github/workflows/1-pinned-before-hijack.yml"
  comment "Workflow pinned BEFORE the tag was hijacked"
  run show_workflow_summary "$scratch/$wf"
  comment "Check detects the tag moved to a fork-network commit"
  ( cd "$scratch" && run gh actions-pin check "$wf" )
  disable_reachability
  trap - EXIT
}

scenario_lockfile_forgery() {
  banner "Lockfile forgery — injected SHA not in ref lineage"
  bash "$RESET"
  local scratch wf
  scratch="$(stage_workflow demo/workflows-pwned/6-lockfile-forgery.yml)"
  wf=".github/workflows/6-lockfile-forgery.yml"
  comment "Lockfile was tampered with — pinned SHA replaced by a fork commit"
  run show_workflow_summary "$scratch/$wf"
  comment "Check detects the pinned SHA is not an ancestor of the live ref"
  ( cd "$scratch" && run gh actions-pin check "$wf" )
}

scenario_json_output() {
  banner "JSON output for CI integration"
  bash "$RESET"
  local scratch
  scratch="$(mktemp -d /tmp/gh-actions-pin-demo-XXXXXX)"
  mkdir -p "$scratch/.github/workflows"
  for f in demo/workflows-pwned/*.yml; do
    stage_workflow "$f" >/dev/null
  done
  # Re-stage them all into a single scratch dir so one lockfile covers them.
  scratch="$(mktemp -d /tmp/gh-actions-pin-demo-XXXXXX)"
  mkdir -p "$scratch/.github/workflows"
  local lock="$scratch/.github/workflows/actions.lock"
  {
    echo "version: v1"
    echo "actions:"
  } > "$lock"
  local -a wf_args=()
  for f in demo/workflows-pwned/*.yml; do
    local one_scratch
    one_scratch="$(stage_workflow "$f")"
    cp "$one_scratch/.github/workflows/$(basename "$f")" "$scratch/.github/workflows/"
    wf_args+=(".github/workflows/$(basename "$f")")
    if [[ -f "$one_scratch/.github/workflows/actions.lock" ]]; then
      # Concatenate; duplicates resolved by last-write-wins on simple union
      grep -E '^  [^ ]' "$one_scratch/.github/workflows/actions.lock" 2>/dev/null \
        | grep -v '^  - ' \
        >> "$lock" || true
    fi
  done
  # Build a clean union lockfile.
  python3 - "$scratch/.github/workflows" demo/workflows-pwned/*.yml <<'PY'
import os, sys, re, pathlib
outdir = pathlib.Path(sys.argv[1])
fixtures = [pathlib.Path(p) for p in sys.argv[2:]]
actions = {}
wf_map = {}
for fix in fixtures:
    body = fix.read_text()
    m = re.search(r"\n(?:# Automatically generated[^\n]*\n)?dependencies:\n((?:[ \t]+#[^\n]*\n|[ \t]*-[^\n]*\n)+)", body)
    pins = []
    if m:
        for line in m.group(1).splitlines():
            s = line.strip()
            if s.startswith("- "):
                pin = s[2:].strip()
                pins.append(pin)
                actions[pin] = (1, 1)
    wf_map[fix.name] = pins
lines = ["version: v1", "actions:"]
for pin in sorted(actions):
    o, r = actions[pin]
    lines += [f"  {pin}:", f"    owner_id: {o}", f"    repo_id: {r}"]
lines.append("workflows:")
for name in sorted(wf_map):
    lines.append(f"  .github/workflows/{name}:")
    lines.append("    dependencies:")
    for pin in wf_map[name]:
        lines.append(f"      - {pin}")
(outdir / "actions.lock").write_text("\n".join(lines) + "\n")
PY
  comment "Machine-readable output for CI pipelines"
  echo -e "${GREEN}\$ gh actions-pin check --json .github/workflows/*.yml 2>/dev/null | jq .${RESET_COLOR}"
  ( cd "$scratch" && gh actions-pin check --json "${wf_args[@]}" 2>/dev/null | python3 -m json.tool )
  echo
}

run_all() {
  scenario_check_autofix
  scenario_ci_failure
  # Skip interactive scenarios in batch mode
  echo -e "${YELLOW}Skipping sha-as-ref and unreleased-commit (interactive — run individually)${RESET_COLOR}\n"
  scenario_upgrade_latest
  scenario_upgrade_version
  scenario_edit_repin
  scenario_ref_moved
  scenario_imposter_commit
  scenario_lockfile_forgery
  scenario_json_output
  banner "All non-interactive scenarios complete"
}

# --- Main ---
[[ $# -eq 0 ]] && usage

case "${1}" in
  check-autofix)      scenario_check_autofix ;;
  ci-failure)         scenario_ci_failure ;;
  sha-as-ref)         scenario_sha_as_ref ;;
  unreleased-commit)  scenario_unreleased_commit ;;
  upgrade-latest)     scenario_upgrade_latest ;;
  upgrade-version)    scenario_upgrade_version ;;
  edit-repin)         scenario_edit_repin ;;
  ref-moved)          scenario_ref_moved ;;
  imposter-commit|imposter) scenario_imposter_commit ;;
  lockfile-forgery|forgery) scenario_lockfile_forgery ;;
  tamper-detection|tamper) scenario_ref_moved; scenario_imposter_commit; scenario_lockfile_forgery ;;
  json-output|json)   scenario_json_output ;;
  all)                run_all ;;
  *)                  echo "Unknown scenario: $1"; echo; usage ;;
esac
