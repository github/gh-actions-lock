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
  awk '
    /^# Automatically generated/ { in_deps=1 }
    /uses:/ || (in_deps && (/^# / || /^dependencies:/ || /^  # / || /^  - / || /^$/)) {
      print
    }
  ' "$1"
}
show_lockfile() {
  awk '
    /^# Automatically generated/ { in_deps=1 }
    in_deps && (/^# / || /^dependencies:/ || /^  # / || /^  - / || /^$/) {
      print
    }
  ' "$1"
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
  comment "Unpinned workflow — 4 actions including a nested composite with transitive deps"
  run grep uses: demo/workflows-check/ci.yml
  comment "Pin all actions (non-interactive auto-fix)"
  run gh actions-pin check --no-interactive demo/workflows-check/ci.yml
  comment "Lockfile groups transitive deps by parent composite action"
  run show_lockfile demo/workflows-check/ci.yml
  comment "Subsequent check passes"
  run gh actions-pin check demo/workflows-check/ci.yml
}

scenario_ci_failure() {
  banner "CI failure on ambiguous actions"
  bash "$RESET"
  comment "One action has a tag (fixable), one is a bare SHA (needs human)"
  run grep uses: demo/workflows-interactive/sha-as-ref.yml
  comment "CI mode: auto-fix what you can, fail on what you cannot"
  gh actions-pin check --no-interactive demo/workflows-interactive/sha-as-ref.yml || true
  echo -e "\n${YELLOW}Exit code: $?${RESET_COLOR}"
  echo -e "${DIM}# CI fails — developer runs 'gh actions-pin check' locally for interactive resolution${RESET_COLOR}\n"
}

scenario_sha_as_ref() {
  banner "Edge case: bare SHA without tag ref (interactive)"
  bash "$RESET"
  comment "Action uses a bare SHA instead of a tag"
  run grep uses: demo/workflows-interactive/sha-as-ref.yml
  comment "The tool detects this and offers tag suggestions"
  echo -e "${GREEN}\$ gh actions-pin check demo/workflows-interactive/sha-as-ref.yml${RESET_COLOR}"
  gh actions-pin check demo/workflows-interactive/sha-as-ref.yml
}

scenario_unreleased_commit() {
  banner "Edge case: unreleased commit (interactive)"
  bash "$RESET"
  comment "Action pinned to a commit with no tagged release"
  run grep uses: demo/workflows-interactive/unreleased-commit.yml
  comment "The tool warns about unreleased code and offers releases"
  echo -e "${GREEN}\$ gh actions-pin check demo/workflows-interactive/unreleased-commit.yml${RESET_COLOR}"
  gh actions-pin check demo/workflows-interactive/unreleased-commit.yml
}

scenario_upgrade_latest() {
  banner "Upgrade to latest"
  bash "$RESET"
  comment "Currently pinned to older versions"
  run show_workflow_summary demo/workflows-upgrade/ci.yml
  comment "Upgrade actions/checkout to latest"
  run gh actions-pin upgrade --action actions/checkout demo/workflows-upgrade/ci.yml
  comment "Updated refs and lockfile"
  run show_workflow_summary demo/workflows-upgrade/ci.yml
}

scenario_upgrade_version() {
  banner "Upgrade to a specific version"
  bash "$RESET"
  comment "Currently pinned to actions/checkout@v4.2.0"
  run grep checkout demo/workflows-upgrade/ci.yml
  comment "Upgrade to a specific version (v5), not latest"
  run gh actions-pin upgrade --action actions/checkout --version v5 demo/workflows-upgrade/ci.yml
  comment "Verify the pinned version"
  run grep checkout demo/workflows-upgrade/ci.yml
}

scenario_edit_repin() {
  banner "Edit and re-pin (Dependabot workflow)"
  bash "$RESET"
  comment "Simulate a Dependabot PR: bump checkout v4.2.0 -> v5"
  run grep checkout demo/workflows-upgrade/ci.yml
  comment "Edit the ref (like Dependabot would)"
  run sed -i '' 's/checkout@v4.2.0/checkout@v5/g' demo/workflows-upgrade/ci.yml
  comment "Remove stale lockfile so check can re-resolve"
  sed -i '' '/# Automatically generated/,$d' demo/workflows-upgrade/ci.yml
  echo -e "${GREEN}\$ sed -i '' '/# Automatically generated/,\\\$d' demo/workflows-upgrade/ci.yml${RESET_COLOR}\n"
  comment "Re-pin with check"
  run gh actions-pin check --no-interactive demo/workflows-upgrade/ci.yml
  comment "New lockfile written"
  run show_lockfile demo/workflows-upgrade/ci.yml
}

scenario_ref_moved() {
  banner "Ref moved — routine update"
  bash "$RESET"
  comment "Workflow pinned before tag moved forward (normal release)"
  run show_workflow_summary demo/workflows-pwned/5-pinned-before-update.yml
  comment "Check detects the tag now points to a newer commit"
  run gh actions-pin check demo/workflows-pwned/5-pinned-before-update.yml
}

scenario_imposter_commit() {
  banner "Imposter commit — fork injection"
  bash "$RESET"
  enable_reachability
  trap disable_reachability EXIT
  comment "Workflow pinned BEFORE the tag was hijacked"
  run show_workflow_summary demo/workflows-pwned/1-pinned-before-hijack.yml
  comment "Check detects the tag moved to a fork-network commit"
  run gh actions-pin check demo/workflows-pwned/1-pinned-before-hijack.yml
  disable_reachability
  trap - EXIT
}

scenario_lockfile_forgery() {
  banner "Lockfile forgery — injected SHA not in ref lineage"
  bash "$RESET"
  comment "Lockfile was tampered with — pinned SHA replaced by a fork commit"
  run show_workflow_summary demo/workflows-pwned/6-lockfile-forgery.yml
  comment "Check detects the pinned SHA is not an ancestor of the live ref"
  run gh actions-pin check demo/workflows-pwned/6-lockfile-forgery.yml
}

scenario_json_output() {
  banner "JSON output for CI integration"
  bash "$RESET"
  comment "Machine-readable output for CI pipelines"
  echo -e "${GREEN}\$ gh actions-pin check --json demo/workflows-pwned/*.yml 2>/dev/null | jq .${RESET_COLOR}"
  gh actions-pin check --json demo/workflows-pwned/*.yml 2>/dev/null | python3 -m json.tool
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
