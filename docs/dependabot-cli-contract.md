# Dependabot ↔ gh-actions-pin CLI contract

Audit of the nine gaps (G1–G9) the Dependabot `github_actions` updater raised
against the `gh-actions-pin` CLI. Each section states the **verified current
behavior** of the binary at this release, what we **commit to**, and any
**action required** on the gh-actions-pin side.

This is the gh-actions-pin-side answer to
`github_actions/docs/lockfile-cli-gaps.md` in dependabot-core.

## TL;DR

| Gap | One-line summary                                                           | Status | Blocks Dependabot CLI engine? |
|-----|----------------------------------------------------------------------------|--------|-------------------------------|
| G1  | `--json` is optional-value; only `--json=<fields>` works                   | 🔧 fixed in this release (docs) | no |
| G2  | Actual fields are `valid,findings,workflows,dependencies` (no errors/warnings) | 🔧 fixed in this release (docs) | no |
| G3  | Dep object is `{nwo,ref,sha,hash_algo,direct,required_by}` — no `file`     | 🔧 fixed in this release (docs) | no |
| G4  | Dummy `GH_TOKEN` + proxy mode works; no Authorization injected by CLI      | ✅ honored | no |
| G5  | Re-lock writes `updated` records; UNREACHABLE surfaces as `impostor-commit`/`lockfile-forgery` | ⚠️ partial — see G9 onboarding gate | no |
| G6  | Per-OS standalone binaries ship per release; no `gh` runtime dep            | ✅ honored | no |
| G7  | JSON carries `cli_version`+`lockfile_version`; v1 additive promise restated | ✅ honored | no |
| G8  | Writer is deterministic (sorted keys, no timestamps); double-write test added | 🔧 fixed in this release | no |
| G9  | `upgrade --write` currently CREATES workflow entries when missing — gap     | ❌ won't do this release; follow-up filed | **YES** until follow-up lands |

**Blockers for flipping the Dependabot default engine to `cli`:** G9 only. G4
and G8 (the two original blockers in the dependabot-core spec) are confirmed
honored; G9 is the new one this audit surfaces.

---

## G1 — `--json` flag form (`--json=dependencies`, not `--json dependencies`)

**Status:** 🔧 fixed in this release (docs).

**Today's behavior:** The `--json` flag on both `check` and `upgrade` uses
cobra's `NoOptDefVal` pattern (see `cmd/gh-actions-pin/check.go:90`,
`cmd/gh-actions-pin/upgrade.go:111`, `cmd/gh-actions-pin/root.go:106`). That
makes the flag *optional-value*: the only way to bind a value is `--json=<csv>`.

Verified by running the binary against this repo:

```
$ gh-actions-pin check --no-interactive --json dependencies
Scanning 1 workflow                       ← "dependencies" parsed as a path
{"cli_version":"…","findings":[…],…}      ← default fields, not what was asked

$ gh-actions-pin check --no-interactive --json=dependencies
Scanning 3 workflows
{"cli_version":"…","dependencies":[…],…}  ← correct
```

The space form is silently wrong (treats the field name as a workflow path
arg) — it never errors.

**Commitment:** The `--json=<csv>` form is the **only** supported form. We
will keep `NoOptDefVal` because `--json` with no value (defaults to
`valid,findings,workflows`) is a useful interactive convenience.

**Action required on this side:** Done in this PR.
- `INTEGRATION.md` examples switched from `--json <fields>` to `--json=<fields>`.
- `cmd/gh-actions-pin/root.go` example fixed.

---

## G2 — Actual JSON top-level fields

**Status:** 🔧 fixed in this release (docs).

**Today's behavior:** Field selector accepts `valid,findings,workflows,dependencies`
and always emits `cli_version` + `lockfile_version` as top-level identifiers
(see `cmd/gh-actions-pin/format/json.go:184–207`). There is **no**
`errors` or `warnings` top-level field — the old INTEGRATION.md examples
were stale.

Default fields when `--json` is used with no value: `valid,findings,workflows`
(documented; `dependencies` is opt-in to avoid duplicating
`workflows[].dependencies`).

Shape of each field, verified against the binary:

```jsonc
{
  "cli_version": "v0.0.4",
  "lockfile_version": "v0.0.1",
  "valid": true,
  "findings": [Finding…],
  "workflows": [Workflow…],
  "dependencies": [Dependency…]      // only when explicitly selected
}
```

**Finding** (`format.Finding` in `cmd/gh-actions-pin/format/json.go:18`):

```jsonc
{
  "workflow":    ".github/workflows/ci.yml",
  "category":    "impostor-commit",           // kebab-case Category string
  "severity":    "error",                     // ok | warning | error
  "confidence":  "high",                      // low | medium | high
  "dependency":  "actions/checkout@v4",       // dep key or ref, when applicable
  "required_by": "actions/setup-go@v6",       // parent NWO for transitive deps
  "detail":      "locked sha is not reachable…",
  "remediation": "…",                         // optional, when set
  "doc_url":     "https://docs.github.com/…"
}
```

**Workflow** (`format.Workflow`):

```jsonc
{
  "path":         ".github/workflows/ci.yml",
  "valid":        true,
  "findings":     [Finding…],
  "dependencies": [Dependency…]
}
```

**Commitment:** These field names and shapes are v1 of the CLI JSON contract.
Additive-only changes within the v0.x line; breaking changes bump the
top-level `cli_version` major. The `lockfile_version` field is a separate
versioning axis tied to the on-disk lockfile schema (currently `v0.0.1`).

**Action required on this side:** Done in this PR.
- `INTEGRATION.md`: replaced stale `--json valid,errors,warnings` block with
  the actual shape; replaced stale Error/Warning object docs with the actual
  Finding/Workflow shape.
- `cmd/gh-actions-pin/root.go:105`: flag help string corrected to list the
  real fields.

---

## G3 — `dependencies` object shape (the FileParser contract)

**Status:** 🔧 fixed in this release (docs + recorded fixture).

**Today's behavior:** Each entry in the top-level `dependencies` array (and
in `workflows[].dependencies`) is:

```jsonc
{
  "nwo":         "actions/setup-go",
  "ref":         "v6",
  "sha":         "d35c59abb061a4a6fb18e82ac0862c26744d6ab5",
  "hash_algo":   "sha1",
  "direct":      true,
  "required_by": ["actions/setup-go@v6"]    // omitted on direct deps
}
```

Verified against this repo (`gh-actions-pin check --no-interactive --json=valid,findings,workflows,dependencies`)
and against the existing test
`TestCheckCommand_JSONDependenciesInfersRequiredByWithoutComments` in
`cmd/gh-actions-pin/command_test.go:791`, which asserts the
direct=false + required_by=[parent-key] shape for a transitive dep
(`actions/cache` reached via `actions/setup-go`'s composite manifest).

**Differences from the spec in INTEGRATION.md prior to this PR:**

1. There is no `file` field on a dependency. The top-level `dependencies`
   array **deduplicates** across workflows. Per-workflow file association
   lives in `workflows[].dependencies`, where each entry carries the same
   dep object and the enclosing workflow's `path`.
2. `required_by` is a `[]string` of parent **dep keys** (e.g.
   `"actions/setup-go@v6"`), not a workflow file path. It is omitted for
   `direct: true` deps and populated for transitive ones.
3. `hash_algo` is currently always `"sha1"`. The field exists to make a
   future SHA-256 transition non-breaking; consumers should not key behavior
   off it being absent.

**Commitment:** This shape is stable across the v0.x line. Dependabot should
key `Dependency.transitive?` off `direct == false`, and use
`workflows[].dependencies` (not the deduplicated top-level array) when it
needs per-file association.

**Action required on this side:** Done in this PR.
- `INTEGRATION.md` dep-shape block rewritten to match the binary.
- Existing test
  `TestCheckCommand_JSONDependenciesInfersRequiredByWithoutComments`
  already serves as the recorded fixture for the transitive case. Reference
  added in the docs so Dependabot can lift the JSON shape directly from a
  green test.

---

## G4 — Auth: dummy `GH_TOKEN` + egress proxy

**Status:** ✅ honored.

**Today's behavior:**

- `internal/resolver/resolver.go` uses `go-gh/v2/pkg/api` (GraphQL + REST
  clients). go-gh requires a non-empty token but does not validate that the
  token is real or live; any string works.
- A grep of `internal/` confirms the CLI **never sets `Authorization` headers
  itself** — every API call goes through go-gh, which in turn relies on the
  HTTP client's default transport. The standard `HTTPS_PROXY`,
  `HTTP_PROXY`, and `SSL_CERT_FILE` environment variables are honored by
  Go's net/http transport with no extra configuration on our side.
- Probed locally with `GH_TOKEN=x-access-token gh-actions-pin check
  --no-interactive --json=valid`: clean run, no token validation, no
  Authorization-header collisions.

**Commitment:** The CLI is compatible with the hosted-Dependabot tokenless
model:

1. Caller sets `GH_TOKEN` to **any** non-empty string (e.g.
   `x-access-token`).
2. Caller routes egress through a proxy via standard `HTTPS_PROXY` /
   `SSL_CERT_FILE`. The proxy injects the real job-scoped token.
3. The CLI does not add its own `Authorization` header; the proxy's
   injection is authoritative.
4. The CLI does not require `gh auth login` state on disk.

We will not knowingly add CLI-owned `Authorization` header injection in any
future release. If a feature ever needs to, it must be gated behind an opt-in
flag and documented as incompatible with the Dependabot proxy mode.

**Action required on this side:** None for ship. Follow-up card to add
`docs/auth-tokenless-mode.md` and a smoke test.

---

## G5 — Re-lock contract + failure semantics + category vocab

**Status:** ⚠️ partial. The write path and JSON shape are honored; the
onboarding gate (G9) is not, and the category vocab differs from the spec.

**Today's behavior:**

1. **Re-lock**: `upgrade --action <nwo>@<ref> --write --json=updated`
   regenerates the lockfile and rewrites the affected workflow YAML. The
   JSON shape (`cmd/gh-actions-pin/upgrade.go:42–53`) is exactly what the
   spec asked for:
   ```jsonc
   {
     "updated": [
       {"nwo":"…","old_ref":"…","new_ref":"…",
        "old_sha":"…","new_sha":"…","files":["…"]}
     ]
   }
   ```
2. **Onboarding gate** (the G9 case): currently the CLI **does** create
   workflow entries when the targeted workflow isn't in the lock. See G9
   below for the violation and the proposed fix.
3. **Failure semantics**: when a resolution fails for one workflow, the run
   exits non-zero (`errSilent`), but `store.Save()` is still called at the
   end of the run for any workflows that succeeded. That means the on-disk
   lockfile **can** be partially mutated when there are mixed
   success/failure results across multiple workflows. Single-workflow
   invocations (the common Dependabot per-dep path) are safe: a failure
   aborts before `store.Set` is reached, so the in-memory store has no
   changes to persist.

**Category vocab — corrections to the inbound spec:**

The spec listed blocking categories as `UNREACHABLE, IMPOSTOR_COMMIT,
MISLEADING_SHA, REF_MOVED, REF_CHANGED` (upper-snake). The actual category
strings emitted by the binary (`internal/doctor/finding.go:11–35`) are
**kebab-case** and there is **no `unreachable` category**:

| Spec name (legacy)   | Actual `category` string | When it fires                                                  |
|----------------------|--------------------------|----------------------------------------------------------------|
| `UNREACHABLE`        | **`impostor-commit`**    | Locked SHA is not reachable from any branch in the upstream repo (fork-network shape). Most direct equivalent to the spec's UNREACHABLE concept. |
| `UNREACHABLE` (transitive variant) | **`lockfile-forgery`** | Pinned SHA is not an ancestor of the upstream ref it claims to represent. |
| `IMPOSTOR_COMMIT`    | `impostor-commit`        | (same as above; the spec already aliased these)                |
| `MISLEADING_SHA`     | `misleading-sha`         | `uses:` ref is SHA-shaped but resolves to a different commit. |
| `REF_MOVED`          | `ref-moved`              | Upstream tag now resolves to a different SHA than the lock.    |
| `REF_CHANGED`        | `ref-changed`            | The workflow's `uses:` ref was hand-edited (e.g. v4 → v5) while the lock still records the prior pin. |
| n/a                  | `not-pinned`             | Workflow has `uses:` refs but no lock entry.                   |
| n/a                  | `sha-as-ref`             | Bare SHA pin with no human-readable ref alongside.             |
| n/a                  | `stale`                  | Lock entry exists for a `uses:` that's no longer in the workflow. |

For the Dependabot `FindingMapper`, the canonical blocking set is:

```
impostor-commit, lockfile-forgery, misleading-sha, ref-moved, ref-changed
```

All five are `severity: "error"`. Map `UnresolvableDependency` off
`impostor-commit` (the primary "unreachable" semantic) and treat
`lockfile-forgery` as the same class of blocker.

**Commitment:**

- The `updated` JSON shape is stable v1.
- The category vocab above is stable v1; new categories are additive.
- Failure semantics for single-workflow `upgrade` invocations: a resolver
  error aborts before any write. **This is the path Dependabot uses today**
  (one `--action` per call, working tree contains only the relevant
  workflow files), so the practical contract holds.
- We **will** add the multi-workflow partial-write guard in a follow-up
  (see G9); it is not required to ship the per-action Dependabot path.

**Action required on this side:**
- This doc covers the vocab correction.
- Follow-up card: gate `store.Save()` on `!hadError` (or on a per-workflow
  basis) so multi-workflow runs are atomic. Filed as part of the G9 work.

---

## G6 — Distribution: standalone binary, not `gh extension install`

**Status:** ✅ honored.

**Today's behavior:** Every tagged release ships standalone binaries for
darwin/linux/windows/freebsd × amd64/arm64/386/arm, built by
`cli/gh-extension-precompile`. Confirmed against
`https://github.com/github/gh-actions-pin/releases/tag/v0.0.4` — fourteen
per-OS assets are attached, each ~9 MB and addressable as
`https://github.com/github/gh-actions-pin/releases/download/v0.0.4/linux-amd64`
etc.

The binary itself has **no `gh` runtime dependency**. `cmd/gh-actions-pin/main.go`
is a normal Go `main` package; `go-gh` is used only as a library for its
GraphQL/REST client. The binary can be `exec`'d directly without `gh` on
PATH.

**Commitment:**

- Standalone, statically-built (CGO-disabled by default in
  `gh-extension-precompile`) per-OS binaries continue to ship per tagged
  release.
- We will add a runtime `lockfile_version`-vs-binary-version compatibility
  check (already enforced in `pkg/lockfile/lockfile.go:262` for the lockfile
  schema; need to formalize the reverse direction — a v0.0.x binary
  refusing a future-major lock).

**Action required on this side:** None for ship; follow-up card to
formalize symmetric version check.

---

## G7 — `pkg/lockfile` Go API vs CLI JSON surface

**Status:** ✅ honored.

**Today's behavior:**

- `pkg/lockfile` exports a Go-only parser pinned at
  `Version = "v0.0.1"` (`pkg/lockfile/lockfile.go:77`). Parse rejects
  unknown versions with a structured `ParseError`. Fine for Go consumers,
  unusable from Ruby.
- The CLI JSON output (`cmd/gh-actions-pin/format/json.go`) always carries
  `cli_version` and `lockfile_version` top-level fields. These let Ruby (or
  any non-Go) consumer pin behavior to a known schema without parsing the
  YAML lockfile.
- `internal/runlog/provenance-v1.json` is a separate JSON schema for run
  provenance, also versioned (`v1`).

**Commitment:** The CLI's JSON contract — field names, shapes, version
fields — is treated as a **first-class API surface** equal to the Go
package. v1 additive-only: no field renames, no shape changes, no removed
fields without a `cli_version` major bump.

We will add an integration test that snapshots the JSON output for a fixed
input and fails on any non-additive change. Tracked as a follow-up.

**Action required on this side:** None for ship; follow-up card for the
golden-JSON regression test.

---

## G8 — `upgrade --write` is deterministic and working-tree-only

**Status:** 🔧 fixed in this release (test coverage).

**Today's behavior:**

1. **Determinism.** `internal/lockfile/store.go` already does the right
   things:
   - `marshalDeterministic` (line 478) emits every map with
     `sort.Strings(keys)` first.
   - Schema field names are unquoted; all user-supplied values are
     single-quoted to dodge YAML scalar-resolution quirks.
   - No timestamps are embedded in the file.
   - Save is atomic: write to `actions.lock.tmp`, then `os.Rename`.
2. **Working-tree-only.** `OpenStore` reads exactly one file:
   `filepath.Join(repoRoot, Path)` where `Path` is
   `.github/workflows/actions.lock`. No git operations, no `origin/`
   lookups, no network fetch of the lockfile.
3. The resolver does of course hit the GitHub API to discover the new SHA,
   but the **lockfile content** is sourced exclusively from the working
   tree and the resolver result for the in-flight `--action` target. The
   grouped-update flow Dependabot relies on (feed each dep the lock the
   previous dep just rewrote) is supported because each `upgrade` call
   reads fresh state from disk every time.

**Commitment:** Byte-deterministic `upgrade --write`. Two runs that resolve
to the same pins produce byte-identical `actions.lock`.

**Action required on this side:** Done in this PR.
- New test `TestStore_SaveIsByteDeterministic` in
  `internal/lockfile/store_test.go` constructs a non-trivial store with
  multiple workflows + transitive deps, saves it twice, and asserts the
  bytes are identical. Acts as a golden-file regression guard against any
  future writer change that re-introduces map-iteration nondeterminism.

---

## G9 — Lifecycle: create / delete

**Status:** ❌ won't do this release; follow-up card filed.

**Today's behavior — the gap:** `upgrade --write` calls
`store.Set(wfKey, deps, parentMap, directKeys)` (upgrade.go:443)
unconditionally on any workflow that matched the `--action` filter. There
is no check for "is this workflow already in `lockfile.workflows{}`?". So
if Dependabot runs `upgrade --action actions/checkout@v5 --write` against a
working tree that contains a workflow file using `actions/checkout` but has
**no lockfile entry for that workflow yet**, the CLI will:

1. Resolve the new pin.
2. Rewrite the workflow YAML.
3. **Create** a new entry in `lockfile.workflows{}` for that workflow.
4. Persist via `store.Save()`.

That violates G9 ("the CLI must never create a lock or add new workflows
during a Dependabot run"). It also conflicts with Dependabot's per-workflow
partition: Dependabot relocks only the onboarded subset and expects the
non-onboarded subset to stay on the legacy regex path.

**Commitment:** We will add a `--no-onboard` flag (or equivalent
strict-mode default) to `upgrade` that:

- Refuses to add a new entry to `lockfile.workflows{}`. If the targeted
  workflow has no existing entry, the file is skipped with a structured
  finding (`category: onboarding-required`) and the run continues for other
  workflows.
- Refuses to create a brand-new lockfile when none exists. The run exits
  with a structured finding instead.
- Multi-workflow partial-write fix (the G5 follow-up) lands as part of the
  same PR — `store.Save()` only runs when all in-flight workflows resolved
  cleanly.

This is a non-trivial behavior change and a flag-surface choice; it
deserves its own PR review pass rather than smuggling into this contract
audit.

**Action required on this side:** Follow-up card to file:
> Add `--no-onboard` strict mode to `upgrade`, gate `store.Set` on
> existing-workflow membership, and guard `store.Save()` against partial
> writes on multi-workflow failure. Required for Dependabot CLI engine
> default flip.

Until that lands, the Dependabot side can keep its current behavior:
partition workflows on its side using its own Ruby `Lockfile::Reader`,
invoke the CLI only against the onboarded subset, and trust the CLI not to
need to know about non-onboarded files because they were excluded from the
working tree it ships into the temp repo.

---

## Detached-lockfile pivot status (the open question)

**Today's status:** `github/launch#10378` ("lockfiles: pivot to detached
lockfiles", linked to `github/actions-workflow-parser#609`) is **open**
and targets all environments (dotcom, proxima, GHES). It is **not yet
merged** as of this audit.

**Effect on this release of gh-actions-pin:** None. The per-repo
`.github/workflows/actions.lock` file model is unchanged in v0.0.4 / the
release this card is part of. `pkg/lockfile.Path` still resolves to
`.github/workflows/actions.lock`, and `OpenStore` reads that exact path.

**Effect on Dependabot's `FileFetcher` / `FileUpdater` assumptions:**

- **If 10378 lands before Dependabot's CLI engine flips to default:** the
  fetch source for the lock changes, the per-workflow onboarding map
  changes, and the `operation: update` vs. `create` semantics likely
  change. Dependabot would need a path-normalization step and a new
  fetcher target.
- **If 10378 lands after:** Dependabot can ship the CLI engine against the
  current per-repo-file model and migrate later. The CLI JSON contract is
  unaffected either way (the JSON surface doesn't expose the on-disk path).

**Commitment:** When 10378 begins rollout, this doc and INTEGRATION.md get
a new section describing the detached path, a new `lockfile_path` field is
added to the JSON output, and a migration note is filed with Dependabot.

The stub-first boundary on the Dependabot side (per the inbound spec) keeps
Ruby unblocked under either outcome. There is no action required on
Dependabot's part to prepare for the pivot beyond what they're already
doing.

---

## Follow-up cards to file

1. **G9 onboarding gate.** Add `--no-onboard` to `upgrade`; gate
   `store.Set` on existing-workflow membership; guard multi-workflow
   `store.Save()` against partial writes. **Required before Dependabot can
   flip the CLI engine to default.**
2. **G4 dummy-token mode doc.** Write `docs/auth-tokenless-mode.md` and
   add a smoke test that runs `check` with `GH_TOKEN=x-access-token` +
   `HTTPS_PROXY` pointing at a mock proxy that injects an Authorization
   header. Verify no CLI-owned `Authorization` header collides.
3. **G6 symmetric version check.** Have the binary refuse a lockfile with a
   future-major schema version (we already refuse unknown versions; the
   reverse — old binary, new lock — needs a friendlier error message
   pointing at the upgrade path).
4. **G7 golden-JSON test.** Snapshot the full `check --json=…` output for a
   fixed fixture repo and fail any non-additive change.
5. **Detached-lockfile follow-up.** Land after `launch#10378` begins
   rollout: add `lockfile_path` to the JSON, doc the new path semantics,
   notify Dependabot.
