---
name: cli-design-proposal
description: >
  Prepares and iterates on a gh CLI design proposal (RFC-style) for submission
  to cli/cli. Handles gist-based iteration, scenario-driven formatting, and
  alignment with cli/cli's UX review process.
---

# CLI Design Proposal Skill

## When to Use

- User asks to "write a design proposal", "prepare for CLI review", "update the proposal", "push to gist"
- User is preparing a `gh` extension for UX review by the cli/cli team
- User wants to iterate on RFC-style scenario documentation

## Context: cli/cli's Process

From [cli/cli docs/working-with-us.md](https://github.com/cli/cli/blob/trunk/docs/working-with-us.md):

1. **Step 0**: Build as a `gh` extension first (we're here — `gh-actions-pin`)
2. **Step 1**: UX review — open an issue in cli/cli or github/cli describing command usage with mock-ups and `--help` output
3. **Step 2**: Public preview quality
4. **Step 3**: Merge decision (complexity, cadence, audience)

The design proposal is **Step 1** — it's a UX review, not a security review or architecture document.

## Proposal Format: RFC-Style (Scenario-Driven)

Follow the pattern from [cli/cli#13120](https://github.com/cli/cli/issues/13120) (Issues 2.0 RFC):

### Structure

```
# Title — one-line description

Covering: [comma-separated list of what this covers]

### Link to issue
[issue URL]

Related:
- [contextual links — roadmap, community discussions, prior art]

## Problem
[numbered list of DX pain points — lead with developer experience, not security]

## Proposed design
[brief description + command table + help text in <details> blocks]

### How the lockfile works
[format explanation with example YAML]

## Try it yourself
[how to reproduce every scenario locally — try-it.sh, demo fixtures]

## Table of Contents
### `command-name`
| Category | Scenario |
|---|---|
| Category | [Scenario name](#anchor) |

### `try-it.sh` scenarios
| Scenario | Section | Interactive? |
|---|---|---|

## `command-name`
[Per-scenario sections below]

### Scenario name

[Brief description of what this demonstrates]

*Input:*
\`\`\`bash
$ command here
\`\`\`

*Output:*
<img src="..." alt="..." width="800">

<details>
<summary>Full terminal session</summary>
[complete terminal output]
</details>

*Expected effects:*
- What happens as a result
- Exit codes, file changes, user-visible state changes
- `try-it.sh scenario-name` reproduces this scenario
```

### Key Principles

1. **Scenario-driven, not feature-driven** — each scenario is Input → Output → Expected effects
2. **Reproducible** — every scenario links to a `try-it.sh` scenario and checked-in demo fixtures
3. **UX focus** — lead with what the developer sees and experiences, not implementation details
4. **GIF + text** — animated GIF for quick visual, expandable `<details>` block with full terminal text for copy-paste
5. **Self-contained** — the proposal + demo dir + try-it.sh should let a reviewer validate everything without building from source
6. **Help text included** — full `--help` output in collapsible blocks so reviewers see exact CLI surface

### What to Include

- Command table with all commands
- Full `--help` output for each command (in `<details>`)
- Lockfile format explanation with real YAML example
- `try-it.sh` scenario table linking scenarios to proposal sections
- Each scenario: Input → GIF → Full terminal text → Expected effects
- Brief contextual links (roadmap blog, community discussion, ADR)

### What to Exclude

- Security analysis (defer to ADR, link once)
- Implementation notes (it's a UX review)
- Phased rollout plans (unless directly relevant to UX)
- Comparison tables with other tools (let the UX speak for itself)
- Open questions (resolve before submitting)

## Gist-Based Iteration Workflow

The proposal lives in a GitHub Gist for rapid iteration with the user:

### Setup (first time)

```bash
# Create the gist
gh gist create --public -f proposal.md /tmp/new-proposal.md
# Returns: https://gist.github.com/<user>/<id>
```

### Iteration loop

1. **Edit locally**: All changes go to `/tmp/new-proposal.md` (working copy)
2. **Upload to gist**: After each change batch:
   ```bash
   gh api "gists/<GIST_ID>" --method PATCH \
     --field "files[proposal.md][content]=@/tmp/new-proposal.md" \
     --jq '.html_url'
   ```
3. **User reviews on GitHub**: Gist renders markdown with GIFs, anchors, details blocks
4. **User gives feedback**: Targeted corrections, section adds/drops, reframing
5. **Repeat** — changes are surgical (python string replacements or regex), not full rewrites

### Tips

- Use `python3` for complex multi-site edits — safer than sed for large markdown
- Always verify structure after edits: `grep -n "^## \|^### " /tmp/new-proposal.md`
- The gist URL is stable — share it once, iterate in place
- GIF URLs from VHS (vhs.charm.sh) are immutable — no need to re-record unless CLI output changes

## GIF Recording (VHS)

Terminal recordings use [VHS](https://github.com/charmbracelet/vhs) with `.tape` files in the repo:

```bash
# Record a tape
vhs demo/tapes/check-autofix.tape

# VHS uploads to vhs.charm.sh and returns a URL
# Use that URL in the proposal: <img src="https://vhs.charm.sh/vhs-XXXX.gif" ...>
```

When CLI output changes (e.g., category renames), tapes need re-recording.

## Demo Fixtures and try-it.sh

The `demo/` directory contains:
- `demo/workflows-check/` — clean unpinned workflows for onboarding scenarios
- `demo/workflows-upgrade/` — pre-pinned workflows for upgrade scenarios
- `demo/workflows-interactive/` — edge cases requiring interactive prompts
- `demo/workflows-pwned/` — tampered/moved ref scenarios for change detection
- `demo/try-it.sh` — runs any scenario against fixtures, resets state between runs

Each proposal scenario should map to a `try-it.sh` scenario so reviewers can validate.

## Current Proposal State

- **Gist**: https://gist.github.com/nodeselector/4ca66e60f1a1760f41bbab3fb84d3d7c
- **Issue**: https://github.com/cli/cli/issues/10553
- **Working copy**: `/tmp/new-proposal.md`
- **Format**: RFC-style, scenario-driven (modeled after cli/cli#13120)

## Finding Category Reference

These are the user-facing finding categories shown in CLI output and JSON:

| Category | Meaning |
|---|---|
| `REF_MOVED` | Locked SHA no longer matches upstream (expected for mutable tags like v4) |
| `MISSING` | Action in workflow has no lock entry |
| `STALE` | Lock entry references an action no longer in the workflow |
| `REF_CHANGED` | Workflow ref was edited; lock needs updating |
| `MISLEADING_SHA` | Ref looks like a SHA but resolves to a different commit |
| `IMPOSTOR_COMMIT` | Locked SHA is not in the ref's history |
