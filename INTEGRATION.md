# Integration Guide

Machine-readable JSON contract for programmatic consumers (Dependabot, CI tools, custom automation).

## Contract Version

**v1** — fields are additive-only; no breaking changes within a major version.

## Exit Codes

| Code | Meaning |
|------|---------|
| 0    | All checks passed (or upgrade preview shown) |
| 1    | Validation failures found (tampered, stale, unreachable) |

## Non-Interactive Flags

For CI and automation, always pass these flags:

```bash
gh actions-pin check --no-interactive --json <fields>
gh actions-pin upgrade --action <nwo>@<ref> --write --json updated
```

`--no-interactive` suppresses prompts. `CI=true` in the environment has the same effect.

---

## Check Command

### `--json valid,errors,warnings`

Basic validation result.

```json
{
  "valid": true,
  "errors": [],
  "warnings": []
}
```

**Error object:**

```json
{
  "type": "TAMPERED | STALE | UNREACHABLE | ERROR",
  "dependency": "actions/checkout",
  "details": "expected abc123 but live resolution is def456",
  "workflow_path": ".github/workflows/ci.yml",
  "transitive": false,
  "unreachable_detail": "SHA abc123 is not reachable from ref v4 ..."
}
```

**Warning object:**

```json
{
  "details": "pinned to a bare SHA without a tag ref ...",
  "workflow_path": ".github/workflows/ci.yml",
  "transitive": true
}
```

### `--json dependencies`

Full dependency inventory (Dependabot FileParser contract).

```json
{
  "dependencies": [
    {
      "nwo": "actions/checkout",
      "ref": "v4",
      "sha": "11bd71901bbe5b1630ceea73d27597364c9af683",
      "hash_algo": "sha1",
      "file": ".github/workflows/ci.yml",
      "direct": true
    },
    {
      "nwo": "actions/attest",
      "ref": "67422f5511b7ff725f4dbd6fb9bd2cd925c65a8d",
      "sha": "67422f5511b7ff725f4dbd6fb9bd2cd925c65a8d",
      "hash_algo": "sha1",
      "file": ".github/workflows/release.yml",
      "direct": false
    }
  ]
}
```

| Field | Description |
|-------|-------------|
| `nwo` | `owner/repo` or `owner/repo/path` for sub-actions |
| `ref` | The tag, branch, or SHA used in the `uses:` line |
| `sha` | Full commit hash the ref is pinned to |
| `hash_algo` | `sha1` or `sha256` |
| `file` | Workflow file path |
| `direct` | `true` if referenced in a `uses:` line; `false` for transitive deps from composite actions |

### `--json workflows`

Per-workflow breakdown (for processing one file at a time).

```json
{
  "workflows": [
    {
      "path": ".github/workflows/ci.yml",
      "valid": true,
      "dependencies": [ ... ],
      "errors": [],
      "warnings": []
    }
  ]
}
```

### `--json findings`

Doctor-style diagnostic findings (remediation input).

```json
{
  "findings": [
    {
      "action": "actions/checkout",
      "workflow": ".github/workflows/ci.yml",
      "kind": "not-pinned",
      "detail": "..."
    }
  ]
}
```

### Combining Fields

Fields can be combined with commas:

```bash
gh actions-pin check --json valid,dependencies,workflows
```

---

## Upgrade Command

### `--json updated`

Returns the list of changes made (or that would be made in dry-run).

```bash
# Dry-run (no --write): shows what would change
gh actions-pin upgrade --action actions/checkout@v5 --json updated

# Apply changes
gh actions-pin upgrade --action actions/checkout@v5 --write --json updated
```

```json
{
  "updated": [
    {
      "nwo": "actions/checkout",
      "old_ref": "v4",
      "new_ref": "v5",
      "old_sha": "11bd71901bbe5b1630ceea73d27597364c9af683",
      "new_sha": "abc1234def5678...",
      "files": [".github/workflows/ci.yml"]
    }
  ]
}
```

| Field | Description |
|-------|-------------|
| `nwo` | Action that was upgraded |
| `old_ref` | Previous ref (tag/branch) |
| `new_ref` | New ref after upgrade |
| `old_sha` | Previous pinned SHA (empty if newly pinned) |
| `new_sha` | New pinned SHA (empty in dry-run without resolution) |
| `files` | Workflow files affected |

---

## Dependabot Integration Pattern

The CLI maps to Dependabot's four-phase contract:

### 1. FileFetcher

Dependabot fetches workflow files from the repo. No CLI involvement.

### 2. FileParser

```bash
gh actions-pin check --no-interactive --json dependencies
```

Returns the full dependency graph. Dependabot creates one `Dependency` per entry, using `nwo` as the package name and `ref` as the version.

### 3. UpdateChecker

```bash
gh actions-pin upgrade --action actions/checkout --json updated
```

Dry-run (no `--write`). Returns what version is available and what would change.

### 4. FileUpdater

```bash
gh actions-pin upgrade --action actions/checkout@v5 --write --json updated
```

Applies the upgrade and returns the change list. Dependabot commits the modified workflow files.

### Environment Requirements

- `GH_TOKEN` or `GITHUB_TOKEN` must be set for API access
- `--hostname` flag for GHES instances
- The CLI must be installed in the Dependabot runner environment
