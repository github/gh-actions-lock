# Integration Guide

Machine-readable JSON contract for programmatic consumers (Dependabot, CI tools, custom automation).

## Contract Version

**v1** — fields are additive-only; no breaking changes within a major version.

Every JSON payload carries top-level `cli_version` and `lockfile_version`
fields so consumers can pin behavior to a known schema without parsing the
on-disk lockfile YAML.

> **`--json` flag form.** `--json` is an *optional-value* flag (it has a
> default value when used bare). The **only** working way to bind a field
> selector is `--json=<csv>`. The space form `--json <fields>` silently
> parses the field selector as a workflow path. All examples below use the
> `=` form; do not change them.

## Exit Codes

| Code | Meaning |
|------|---------|
| 0    | `valid:true`. No blocking findings; nothing to do. |
| 1    | **Overloaded.** Either (a) `valid:false` — blocking findings present (tampered, stale, unreachable, not-pinned, …), stdout JSON is complete and well-formed — **or** (b) actual tool failure (bad flag, IO error, network failure, etc.), stdout may be empty or partial. |

> **Footgun.** Exit 1 does **not** mean "the tool failed." It is the normal
> exit code for a successful run that found things to fix. Shell-style "raise
> on any non-zero exit" wrappers will misreport every unpinned-but-otherwise-fine
> repo as broken.

**Recommended consumer pattern:** capture stdout regardless of exit code,
then drive control flow off the JSON payload's `valid` and `findings[]`
fields. Use the *shape* of stdout to distinguish blocking findings
(well-formed JSON, `valid:false`) from a tool failure (empty or non-JSON
stdout):

```bash
output=$(gh actions-pin check --no-interactive --json=valid,findings,workflows,dependencies 2>/dev/null)
rc=$?

if [ -z "$output" ] || ! printf '%s' "$output" | jq -e . >/dev/null 2>&1; then
  # Empty / non-JSON stdout → tool failure (regardless of $rc).
  echo "gh actions-pin failed (exit $rc) with no parseable output" >&2
  exit 2
fi

# stdout is well-formed JSON: branch on .valid, not on $rc.
case "$(printf '%s' "$output" | jq -r '.valid')" in
  true)  : ;;                                          # clean, nothing to do
  false) printf '%s' "$output" | jq '.findings' ;;     # act on findings
esac
```

The same pattern in pseudocode for non-shell consumers:

```
result, exit_code := run("gh actions-pin", "--no-interactive", "--json=valid,findings")
parsed, ok        := try_parse_json(result.stdout)
if !ok                  → tool failure (treat as engine error; exit_code is informational)
else if parsed.valid    → success, nothing to do
else                    → act on parsed.findings (this is a normal result, not a failure)
```

## Non-Interactive Flags

For CI and automation, always pass these flags:

```bash
gh actions-pin check --no-interactive --json=<fields>
gh actions-pin upgrade --action <nwo>@<ref> --write --json=updated
```

`--no-interactive` suppresses prompts. `CI=true` in the environment has the same effect.

---

## Check Command

Supported field selectors: `valid`, `findings`, `workflows`, `dependencies`.
Any combination may be passed as a comma-separated list; the default (when
`--json` is used with no value) is `valid,findings,workflows`.

### `--json=valid`

Basic pass/fail result.

```json
{
  "cli_version": "v0.0.4",
  "lockfile_version": "v0.0.1",
  "valid": true
}
```

### `--json=findings`

Doctor-style diagnostic findings — the primary error/warning surface.

```json
{
  "cli_version": "v0.0.4",
  "lockfile_version": "v0.0.1",
  "findings": [
    {
      "workflow":    ".github/workflows/ci.yml",
      "category":    "impostor-commit",
      "severity":    "error",
      "confidence":  "high",
      "dependency":  "actions/checkout@v4",
      "required_by": "actions/setup-go@v6",
      "detail":      "locked SHA abc123 is not reachable from any branch",
      "remediation": "re-pin to a SHA reachable from main",
      "doc_url":     "https://docs.github.com/..."
    }
  ]
}
```

| Field         | Description                                                                         |
|---------------|-------------------------------------------------------------------------------------|
| `workflow`    | Workflow file the finding applies to (may be empty for repo-level findings).        |
| `category`    | Kebab-case category string. See [Categories](#finding-categories) below.            |
| `severity`    | `ok`, `warning`, or `error`.                                                        |
| `confidence`  | `low`, `medium`, or `high`. Omitted when not set.                                   |
| `dependency`  | Dep key (`nwo@ref`) or `nwo@ref` form when the finding ties to a specific dep.       |
| `required_by` | Parent dep key for transitive findings. Omitted for direct deps.                    |
| `detail`      | Human-readable detail.                                                              |
| `remediation` | Suggested fix, when one exists.                                                     |
| `doc_url`     | Docs URL for the category, when one exists.                                         |

#### Finding categories

| Category           | Severity | Meaning                                                                                                |
|--------------------|----------|--------------------------------------------------------------------------------------------------------|
| `not-pinned`       | error    | Workflow has `uses:` refs but no lockfile entry.                                                       |
| `sha-as-ref`       | warning  | Bare SHA pin with no human-readable ref alongside.                                                     |
| `stale`            | warning  | Lockfile entry exists for a `uses:` no longer in the workflow.                                         |
| `ref-changed`      | error    | The workflow's `uses:` ref was edited (e.g. `v4` → `v5`) while the lock still records the prior pin.    |
| `impostor-commit`  | error    | Locked SHA is not reachable from any branch in the upstream repo (fork-network shape).                  |
| `lockfile-forgery` | error    | Pinned SHA is not an ancestor of the upstream ref it claims to represent.                              |
| `misleading-sha`   | error    | `uses:` ref is SHA-shaped but resolves to a different commit.                                          |
| `ref-moved`        | error    | Upstream tag now resolves to a different SHA than the lock.                                            |
| `valid`            | ok       | The dependency is pinned and verified.                                                                 |
| `run-only`         | ok       | The workflow has no action refs (only `run:` steps).                                                   |

### `--json=dependencies`

Full dependency inventory (Dependabot FileParser contract). The top-level
`dependencies` array is **deduplicated across workflows** — for per-workflow
file association, use `--json=workflows` (each workflow object carries its
own `dependencies` list).

```json
{
  "cli_version": "v0.0.4",
  "lockfile_version": "v0.0.1",
  "dependencies": [
    {
      "nwo": "actions/checkout",
      "ref": "v4",
      "sha": "11bd71901bbe5b1630ceea73d27597364c9af683",
      "hash_algo": "sha1",
      "direct": true
    },
    {
      "nwo": "actions/cache",
      "ref": "v4",
      "sha": "5a3ec84eff668545956fd18022155c47e93e2684",
      "hash_algo": "sha1",
      "direct": false,
      "required_by": ["actions/setup-go@v6"]
    }
  ]
}
```

| Field         | Description                                                                                                 |
|---------------|-------------------------------------------------------------------------------------------------------------|
| `nwo`         | `owner/repo` or `owner/repo/path` for sub-actions                                                            |
| `ref`         | The tag, branch, or SHA used in the `uses:` line                                                             |
| `sha`         | Full commit hash the ref is pinned to                                                                        |
| `hash_algo`   | Hash algorithm; currently always `sha1`. Field exists so a future SHA-256 transition is non-breaking.        |
| `direct`      | `true` if referenced in a `uses:` line; `false` for transitive deps reached via composite actions             |
| `required_by` | Parent dep keys (`nwo@ref`) that pull this dep in. Omitted for `direct: true`. **Not** a file path.           |

### `--json=workflows`

Per-workflow breakdown (use this when you need per-file dependency
association — the top-level `dependencies` array is deduplicated and does
not carry file paths).

```json
{
  "cli_version": "v0.0.4",
  "lockfile_version": "v0.0.1",
  "workflows": [
    {
      "path": ".github/workflows/ci.yml",
      "valid": true,
      "findings": [ /* Finding objects, same shape as --json=findings */ ],
      "dependencies": [ /* Dependency objects, same shape as --json=dependencies */ ]
    }
  ]
}
```

### Combining Fields

Fields can be combined with commas:

```bash
gh actions-pin check --json=valid,dependencies,workflows
```

---

## Upgrade Command

### `--json=updated`

Returns the list of changes made (or that would be made in dry-run).

```bash
# Dry-run (no --write): shows what would change
gh actions-pin upgrade --action actions/checkout@v5 --json=updated

# Apply changes
gh actions-pin upgrade --action actions/checkout@v5 --write --json=updated
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
gh actions-pin check --no-interactive --json=dependencies
```

Returns the full dependency graph. Dependabot creates one `Dependency` per entry, using `nwo` as the package name and `ref` as the version.

### 3. UpdateChecker

```bash
gh actions-pin upgrade --action actions/checkout --json=updated
```

Dry-run (no `--write`). Returns what version is available and what would change.

### 4. FileUpdater

```bash
gh actions-pin upgrade --action actions/checkout@v5 --write --json=updated
```

Applies the upgrade and returns the change list. Dependabot commits the modified workflow files.

### Environment Requirements

- `GH_TOKEN` or `GITHUB_TOKEN` must be set to a non-empty string for API
  access. The token's *value* is not validated by the CLI; in
  hosted-Dependabot's tokenless model, set `GH_TOKEN=x-access-token` and
  route egress through a proxy via standard `HTTPS_PROXY` / `SSL_CERT_FILE`
  env vars. The proxy injects the real job-scoped `Authorization` header.
- `--hostname` flag for GHES instances.
- The CLI is shipped as a standalone per-OS binary attached to every
  release at
  `https://github.com/github/gh-actions-pin/releases/download/<tag>/<os>-<arch>`.
  It has no `gh` runtime dependency; exec it directly.

For the full Dependabot ↔ CLI contract audit (gaps G1–G9 and the
detached-lockfile pivot status) see
[`docs/dependabot-cli-contract.md`](docs/dependabot-cli-contract.md).
