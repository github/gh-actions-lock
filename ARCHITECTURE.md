# Architecture Walkthrough

> A guided tour of `gh-actions-pin` internals. Follow the links — every section points at the actual code.

```
7,650 lines of Go across 18 source files + 11 test files
```

```
cmd/gh-actions-pin/          ← CLI entry point, command routing, presentation
  main.go                       7 LOC   — os.Exit(Execute())
  root.go                     149 LOC   — pinFactory, cobra command tree
  check.go                    564 LOC   — check command, JSON output, result presentation
  upgrade.go                  416 LOC   — upgrade command, diff display

internal/lockfile/           ← YAML parsing, dependency model, serialization
  lockfile.go                 633 LOC   — ActionRef, Dependency, File, YAML read/write
  diff.go                     137 LOC   — dependency diffing
  version.go                   65 LOC   — semver parsing

internal/resolver/           ← GitHub API interaction, dependency resolution
  resolver.go                 614 LOC   — GraphQL batched resolution, reachability checks
  retry.go                     56 LOC   — exponential backoff

internal/doctor/             ← Diagnostic analysis + interactive remediation
  finding.go                  191 LOC   — Finding, Report, Category types
  diagnose.go                 361 LOC   — workflow analysis, finding generation
  remediate.go                828 LOC   — interactive fix engine
  apply.go                    209 LOC   — write fixes to disk
  prompt.go                   193 LOC   — Prompter interface + implementations
  tagging.go                  341 LOC   — tag suggestion logic
  tags.go                     335 LOC   — tag fetching and filtering
  version.go                   87 LOC   — mutable version helpers

internal/ui/                 ← Terminal output formatting
  ui.go                       252 LOC   — colors, progress, hyperlinks

internal/httpmock/           ← Test utilities
  httpmock.go                 188 LOC   — HTTP response stubbing
```

---

## 1. Package Dependency Graph

```
┌─────────────────────────────────┐
│     cmd/gh-actions-pin          │  CLI layer: cobra commands, I/O, JSON
│  main → root → check, upgrade   │
└──────────┬──────────────────────┘
           │ uses
           ▼
┌──────────────────┐     ┌──────────────────┐
│  internal/doctor │────▶│ internal/lockfile │  YAML parsing, dependency model
│  diagnose        │     │ ActionRef         │
│  remediate       │     │ Dependency        │
│  apply           │     │ File              │
│  prompt          │     │ WriteDependencies │
│  tagging/tags    │     └────────┬─────────┘
└──────┬───────────┘              │
       │ uses                     │ uses
       ▼                          ▼
┌──────────────────┐     ┌──────────────────┐
│ internal/resolver│     │   internal/ui     │
│ ResolveAll       │     │   colors, links   │
│ CheckReachability│     │   progress        │
│ LatestRef        │     └──────────────────┘
└──────────────────┘
       │ uses
       ▼
   go-gh/v2/pkg/api (GraphQL + REST clients)
```

**Key rule**: `lockfile` is the lowest layer (pure data + YAML). `resolver` talks to the API. `doctor` orchestrates diagnosis and remediation. `cmd` handles CLI concerns (cobra, JSON output, presentation). The `ui` package is used everywhere for formatted output.

---

## 2. Data Model

### [`lockfile.ActionRef`](https://github.com/github/gh-actions-pin/blob/ns/primer-output-formatting/internal/lockfile/lockfile.go#L44-L52)

The parsed form of a `uses:` line in a workflow:

```go
type ActionRef struct {
    Owner    string // "actions"
    Repo     string // "checkout"
    Path     string // "nested-composite" (optional subpath)
    Ref      string // "v4" or "abc123..."
    Location int    // line number in YAML
}
```

Parsed by [`ParseActionRef`](https://github.com/github/gh-actions-pin/blob/ns/primer-output-formatting/internal/lockfile/lockfile.go#L213) which filters out expressions (`${...}`), local paths (`./`), Docker refs (`docker://`), and reusable workflows.

### [`lockfile.Dependency`](https://github.com/github/gh-actions-pin/blob/ns/primer-output-formatting/internal/lockfile/lockfile.go#L69-L76)

The resolved + serialized form that goes into the `dependencies:` block:

```go
type Dependency struct {
    NWO      string // "actions/checkout" (with optional /path)
    Ref      string // "v4.3.1" (narrowed)
    SHA      string // "34e114876b0b..."
    HashAlgo string // "sha1"
    Direct   bool   // true = in workflow, false = transitive
}
```

Serialized as: `github.com/actions/checkout@v4.3.1:sha1-34e114876b0b...`

Parsed by [`ParseDependencyString`](https://github.com/github/gh-actions-pin/blob/ns/primer-output-formatting/internal/lockfile/lockfile.go#L104) with validation for control character injection, hash algorithm, and SHA length.

### [`doctor.Finding`](https://github.com/github/gh-actions-pin/blob/ns/primer-output-formatting/internal/doctor/finding.go#L40-L58)

A diagnostic issue found during analysis:

```go
type Finding struct {
    Category    Category    // REF_MOVED, MISSING, STALE, etc.
    Severity    Severity    // Error or Warning
    Dependency  *lockfile.Dependency
    Detail      string
    Remediation string
    Workflow    string
    ActionRef   *lockfile.ActionRef
    NewSHA      string      // for REF_MOVED: what the ref resolves to now
}
```

Categories ([`finding.go:6-27`](https://github.com/github/gh-actions-pin/blob/ns/primer-output-formatting/internal/doctor/finding.go#L6-L27)):

| Category | Meaning |
|---|---|
| `CategoryNotPinned` | Action in workflow has no lock entry |
| `CategoryStale` | Lock entry for action no longer in workflow |
| `CategoryRefChanged` | Workflow ref edited; lock needs updating |
| `CategoryRefMoved` | Upstream tag now resolves to different SHA |
| `CategoryMisleadingSHA` | Ref looks like SHA but resolves differently |
| `CategoryImposterCommit` | Locked SHA not in ref's git history |

---

## 3. The `pinFactory` — Dependency Injection

[`root.go:22-35`](https://github.com/github/gh-actions-pin/blob/ns/primer-output-formatting/cmd/gh-actions-pin/root.go#L22-L35)

```go
type pinFactory struct {
    Prompter     doctor.Prompter
    ResolverFunc func(hostname string) (*resolver.Resolver, error)
    Out          *ui.UI
    Stdout       io.Writer
    Stderr       io.Writer
    Hostname     string
}
```

This is the seam for testing. Commands receive `*pinFactory` and use it for all I/O and API access. Tests swap in:
- `TestPrompter` with pre-programmed confirm/select responses
- `httpmock.Registry` transport via `NewWithTransport`
- Piped `os.Stdout`/`os.Stderr` for output capture

> **Note for cli/cli merge**: Comment at [root.go:21](https://github.com/github/gh-actions-pin/blob/ns/primer-output-formatting/cmd/gh-actions-pin/root.go#L21) says "swap this for cmdutil.Factory" — the interface is designed to be compatible.

---

## 4. Control Flow: `check` Command

The main entry point. Here's the full path from CLI invocation to disk write:

### 4.1 Command Setup

[`check.go:52-114`](https://github.com/github/gh-actions-pin/blob/ns/primer-output-formatting/cmd/gh-actions-pin/check.go#L52-L114) — Cobra command with flags `--json`, `--no-interactive`, `--hostname`.

The bare `gh actions-pin` (no subcommand) calls `runCheck` directly via [`root.go:92-96`](https://github.com/github/gh-actions-pin/blob/ns/primer-output-formatting/cmd/gh-actions-pin/root.go#L92-L96).

### 4.2 `runCheck` — The Orchestrator

[`check.go:115-227`](https://github.com/github/gh-actions-pin/blob/ns/primer-output-formatting/cmd/gh-actions-pin/check.go#L115-L227)

```
runCheck(f, opts)
  │
  ├─ 1. Discover workflow paths (args or .github/workflows/*.yml)
  │
  ├─ 2. Create Resolver (API client with hostname)
  │
  ├─ 3. Diagnose all workflows ──▶ doctor.Diagnose(paths, resolver)
  │     Returns: *Report (findings per workflow)
  │
  ├─ 4. Compute validity (any non-warning findings?)
  │
  ├─ 5. Present results ──▶ presentCheckResults(out, report, valid)
  │     Human-readable summary to stderr
  │
  ├─ 6. Write JSON (if --json) ──▶ writeCheckJSON(stdout, report, valid)
  │
  ├─ 7. If invalid + interactive:
  │     └─ Remediate ──▶ Remediator.Remediate(report)
  │        Interactive fix loop (prompts, applies fixes)
  │
  └─ 8. Return error if still invalid (exit 1)
```

### 4.3 Diagnosis

[`diagnose.go:14-22`](https://github.com/github/gh-actions-pin/blob/ns/primer-output-formatting/internal/doctor/diagnose.go#L14-L22) — Iterates workflow paths, calls `diagnoseOneWorkflow` for each.

[`diagnoseOneWorkflow`](https://github.com/github/gh-actions-pin/blob/ns/primer-output-formatting/internal/doctor/diagnose.go#L23) (361 lines) does:

```
diagnoseOneWorkflow(path, resolver)
  │
  ├─ Parse YAML ──▶ lockfile.Load(path)
  ├─ Extract action refs ──▶ file.ExtractActionRefs()
  ├─ Read existing dependencies ──▶ file.ReadDependencies()
  │
  ├─ If no deps exist → all refs are NotPinned findings
  │
  ├─ If deps exist:
  │   ├─ Re-resolve all refs ──▶ resolver.ResolveAllRecursive()
  │   ├─ Compare resolved vs locked:
  │   │   ├─ SHA mismatch? → REF_MOVED finding
  │   │   ├─ Ref edited? → REF_CHANGED finding
  │   │   ├─ Entry missing? → NotPinned finding
  │   │   ├─ Extra entry? → STALE finding
  │   │   └─ SHA-as-ref? → MISLEADING_SHA finding
  │   │
  │   └─ Check reachability ──▶ resolver.CheckReachabilityAll()
  │       └─ Unreachable? → IMPOSTER_COMMIT finding
  │
  └─ Return WorkflowReport{Path, Findings, File, ActionRefs, ...}
```

### 4.4 Resolution — The GraphQL Engine

[`resolver.go:329-390`](https://github.com/github/gh-actions-pin/blob/ns/primer-output-formatting/internal/resolver/resolver.go#L329-L390) — `ResolveAllRecursive`:

```
ResolveAllRecursive(refs)
  │
  ├─ Resolve batch via GraphQL ──▶ resolveWithActionYML(refs)
  │   Batches up to 20 refs per GraphQL query
  │   Each query fetches: commit SHA + action.yml content
  │
  ├─ For each resolved dep:
  │   ├─ Parse action.yml ──▶ lockfile.ParseActionMeta()
  │   ├─ If composite action with nested uses:
  │   │   └─ Extract nested refs, recurse (max depth 10)
  │   └─ Mark as direct=true or direct=false (transitive)
  │
  ├─ Narrow mutable tags: v4 → v4.3.1 ──▶ LatestRef()
  │
  └─ Return deduplicated []Dependency
```

The GraphQL query is built by [`buildResolveWithFileQuery`](https://github.com/github/gh-actions-pin/blob/ns/primer-output-formatting/internal/resolver/resolver.go#L485) which constructs aliased queries:

```graphql
query {
  repo0: repository(owner: "actions", name: "checkout") {
    ref0: object(expression: "v4") { ... on Commit { oid } }
    actionYml: object(expression: "v4:action.yml") { ... on Blob { text } }
  }
}
```

### 4.5 Reachability — Fork-Network Attack Detection

[`resolver.go:151-207`](https://github.com/github/gh-actions-pin/blob/ns/primer-output-formatting/internal/resolver/resolver.go#L151-L207) — `CheckReachability`:

Uses the [Compare API](https://docs.github.com/en/rest/commits/commits#compare-two-commits) to verify ancestry:

```
CheckReachability(owner, repo, sha, ref)
  │
  ├─ GET /repos/{owner}/{repo}/compare/{sha}...{ref}
  │
  ├─ If merge_base == sha → Reachable (sha is ancestor of ref)
  ├─ If merge_base != sha → Unreachable (fork-network commit!)
  └─ If 404 → NotFound
```

The key insight: if `merge_base(pinnedSHA, currentRef) != pinnedSHA`, then the pinned commit isn't in the ref's ancestry — it may be from a fork network.

### 4.6 Remediation — The Interactive Fix Engine

[`remediate.go:87-110`](https://github.com/github/gh-actions-pin/blob/ns/primer-output-formatting/internal/doctor/remediate.go#L87-L110) — `Remediate(report)`:

```
Remediate(report)
  │
  ├─ For each workflow with findings:
  │   └─ remediateWorkflow(wr)
  │       │
  │       ├─ NotPinned findings:
  │       │   └─ handleNotPinned() ──▶ resolve, narrow, write lockfile
  │       │
  │       ├─ SHA-as-ref findings:
  │       │   └─ handleSHAAsRef() ──▶ tag picker or auto-suggest
  │       │       ├─ Same owner? Auto-apply best tag
  │       │       ├─ Tags found? Suggest or picker
  │       │       └─ No tags? Unreleased commit warning
  │       │
  │       ├─ RefChanged findings:
  │       │   └─ handleRefChanged() ──▶ re-resolve with new ref
  │       │
  │       └─ Stale findings:
  │           └─ handleStale() ──▶ remove lockfile entry
  │
  └─ Session memory: choices{}, approvedRefs{}, remaining{}
     Avoids re-prompting for same action across workflows
```

The [`Prompter` interface](https://github.com/github/gh-actions-pin/blob/ns/primer-output-formatting/internal/doctor/prompt.go#L17-L27) abstracts all user interaction:

```go
type Prompter interface {
    IsInteractive() bool
    Confirm(message string, defaultVal bool) (bool, error)
    Select(message string, options []string) (int, error)
    MultiSelect(message string, options []string) ([]int, error)
}
```

Three implementations: `HuhPrompter` (real TUI via charmbracelet/huh), `TestPrompter` (pre-programmed), `NoopPrompter` (non-interactive mode).

### 4.7 Applying Fixes to Disk

[`apply.go:65-135`](https://github.com/github/gh-actions-pin/blob/ns/primer-output-formatting/internal/doctor/apply.go#L65-L135) — `applyPin`:

```
applyPin(wr)
  │
  ├─ Collect all ActionRefs from workflow
  ├─ Build ref replacements (v4 → v4.3.1 if narrowed)
  ├─ RewriteActionRefs() ──▶ line-based YAML rewrite of uses: lines
  ├─ ResolveAllRecursive() ──▶ get SHAs for all (including transitive)
  └─ WriteDependencies() ──▶ append dependencies: block to YAML
```

---

## 5. Control Flow: `upgrade` Command

[`upgrade.go:108-150`](https://github.com/github/gh-actions-pin/blob/ns/primer-output-formatting/cmd/gh-actions-pin/upgrade.go#L108-L150) — `runUpgrade`:

```
runUpgrade(f, opts)
  │
  ├─ Parse targets ──▶ parseUpgradeTargets(actions, from, version)
  │   Each target: {match: "actions/checkout", targetRef: "v5", fromRef: "v4"}
  │
  ├─ For each workflow file:
  │   └─ upgradeOneFile()
  │       ├─ Load + extract refs
  │       ├─ Match refs against targets
  │       ├─ Resolve target ref → latest patch tag ──▶ LatestRef()
  │       ├─ Build replacements map: old ref → new ref
  │       ├─ RewriteActionRefs() ──▶ update uses: lines
  │       ├─ Re-resolve all ──▶ ResolveAllRecursive()
  │       ├─ WriteDependencies() ──▶ new lockfile
  │       ├─ Show diff ──▶ showDiff() (old deps vs new deps)
  │       └─ Write file (if --write, default true)
  │
  └─ JSON output (if --json): list of changes with old/new refs
```

---

## 6. Tag Selection — The Narrowing Pipeline

When pinning `actions/checkout@v4`, the tool narrows to `v4.3.1`. This involves:

### [`resolver.LatestRef`](https://github.com/github/gh-actions-pin/blob/ns/primer-output-formatting/internal/resolver/resolver.go#L277-L321)

Fetches all tags via GraphQL, filters by prefix match, selects latest semver.

### [`doctor/tagging.go`](https://github.com/github/gh-actions-pin/blob/ns/primer-output-formatting/internal/doctor/tagging.go) — Tag Suggestions

When handling SHA-as-ref, suggests tags that point at the same commit:

```
TagSuggestions(owner, repo, sha)
  │
  ├─ Fetch tags via REST ──▶ GET /repos/{owner}/{repo}/tags
  ├─ Filter: tags whose commit.sha == pinned SHA
  ├─ Sort by semver (highest first)
  ├─ Apply cooldown filter (skip tags < 3 days old)
  └─ Return []TagSuggestion with badges (📌 current, recommended)
```

### [`doctor/tags.go`](https://github.com/github/gh-actions-pin/blob/ns/primer-output-formatting/internal/doctor/tags.go) — Tag Fetching

Pagination, filtering, semver sorting for the full tag picker.

---

## 7. UI Layer

[`ui/ui.go`](https://github.com/github/gh-actions-pin/blob/ns/primer-output-formatting/internal/ui/ui.go)

```go
type UI struct {
    w           io.Writer
    isTTY       bool
    spinner     *spinner.Spinner   // charmbracelet/bubbles
    spinnerDone chan struct{}
}
```

- **Colors**: `Success()`, `Error()`, `Warning()`, `Skip()` — prefixed with ✓/✗/!/- glyphs
- **Hyperlinks**: `Hyperlink(text, url)` — OSC 8 terminal hyperlinks when TTY, plain URLs otherwise
- **Progress**: `StartProgress(label)` / `StopProgress()` — spinner on TTY, static label on non-TTY
- **TTY detection**: [`IsTTY()`](https://github.com/github/gh-actions-pin/blob/ns/primer-output-formatting/internal/ui/ui.go#L211) via `isatty`

---

## 8. Testing Patterns

### HTTP Mocking

[`httpmock/httpmock.go`](https://github.com/github/gh-actions-pin/blob/ns/primer-output-formatting/internal/httpmock/httpmock.go) — Register stubs by method + path pattern, verify all stubs consumed.

### Command Tests

[`command_test.go`](https://github.com/github/gh-actions-pin/blob/ns/primer-output-formatting/cmd/gh-actions-pin/command_test.go) — Full end-to-end: create temp workflow files, register API stubs, run command, assert stdout/stderr/exit code.

Pattern: swap `os.Stdout`/`os.Stderr` with pipes, read output in goroutines. See the stored memory about this.

### Integration Tests

[`reachability_integration_test.go`](https://github.com/github/gh-actions-pin/blob/ns/primer-output-formatting/internal/resolver/reachability_integration_test.go) — Gated behind `-tags integration`, hits real GitHub API with `GH_TOKEN`. Tests against fixture repos with known commit/tag/fork relationships.

---

## 9. Code Smells 🔍

Issues noticed during this walkthrough, tracked for cleanup:

### Smell 1: Semver parsing duplicated

`lockfile/version.go` has `ParseSemver`, `Semver.IsFullSemver()`, `Semver.MatchesMajorMinor()`.
`doctor/version.go` wraps these with `IsMutableVersionTag()`, `IsNarrowedVersion()`.

**Not a real problem** — `doctor/version.go` is a thin wrapper using `lockfile.ParseSemver`. The layering is correct. Skip.

### Smell 2: `openBrowser()` in remediate.go

[`remediate.go:818`](https://github.com/github/gh-actions-pin/blob/ns/primer-output-formatting/internal/doctor/remediate.go#L818) — `exec.Command("open", url)` hardcoded in the remediation layer. Should be in UI or injected via factory for testability and cross-platform support.

### Smell 3: `diagnoseOneWorkflow` is 361 lines

[`diagnose.go:23`](https://github.com/github/gh-actions-pin/blob/ns/primer-output-formatting/internal/doctor/diagnose.go#L23) — Single function handling all comparison logic. The comparison loop (existing deps vs resolved deps) could be extracted into a `compareSnapshots()` function.

### Smell 4: Three tag picker flows

- `handleSHAWithSuggestions` — tags that point at the SHA
- `handleSHATagPicker` — full tag list picker
- tag logic in `handleRefChanged` — stale ref picker

These share structure (fetch tags → filter → present → confirm) but are separate implementations. Could consolidate with a strategy parameter.

### Smell 5: `Remediator` has extensive session state

[`remediate.go:23-51`](https://github.com/github/gh-actions-pin/blob/ns/primer-output-formatting/internal/doctor/remediate.go#L23-L51) — `choices`, `internalRefChoices`, `approvedRefs`, `remaining`, `skippedDeps` — five maps tracking session state. Works but complex to reason about. Would benefit from a dedicated `SessionState` type.

### Smell 6: Line-based YAML rewriting

[`lockfile.go:459`](https://github.com/github/gh-actions-pin/blob/ns/primer-output-formatting/internal/lockfile/lockfile.go#L459) — `RewriteActionRefs` does string replacement on YAML lines. This is brittle if formatting changes (extra spaces, comments on same line). Works because `uses:` lines have a predictable format, but worth noting.

### Smell 7: GraphQL query string concatenation

[`resolver.go:485`](https://github.com/github/gh-actions-pin/blob/ns/primer-output-formatting/internal/resolver/resolver.go#L485) — `buildResolveWithFileQuery` concatenates strings to build GraphQL. Works for the fixed query shape but can't parameterize. Not urgent — the query is simple.

---

## 10. Key Design Decisions

| Decision | Rationale |
|---|---|
| Inline lockfile (not separate file) | Can't drift from workflow; one file = one truth |
| Narrow mutable tags (v4 → v4.3.1) | Upgrades show meaningful version diffs |
| Compare API for reachability | REST, no git clone needed, O(1) per check |
| Batch GraphQL (20 refs/query) | Minimizes API calls for large workflows |
| Max recursion depth 10 | Matches GitHub runner limit for composite actions |
| Cooldown on new tags (3 days) | Prevents pinning to a tag before it's stable |
| `dependencies:` as YAML key | Valid YAML, runtime ignores unknown keys |
| SHA prefix (`sha1-`) | Future-proofs for SHA-256 OIDs |
