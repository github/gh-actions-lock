# Spec: bare SHA is not a smell when metadata is sufficient

> Status: draft. Not implemented. No code in this PR.

## 1. Premise

The current model treats any `uses: owner/repo@<40-hex>` as a warning ("pinned to
a bare SHA without a symbolic ref — weakens supply-chain traceability"). That is
backwards. Bare SHA pinning is — and for years has been — the GitHub-recommended
baseline for third-party action dependency hardening. The shape of the ref is
not the problem.

The problem is **provenance metadata**. "We don't know whether this SHA is what
the user meant" is a metadata gap, not a syntactic one. A bare SHA with enough
corroborating metadata is fine. A tagged ref with no corroborating metadata is
worse than a bare SHA.

This spec reframes the check around metadata sufficiency and lays out which
existing warnings stay, which go, and which new categories appear.

## 2. The reframed model

Provenance metadata channels are not equal. The check needs to grade each one
explicitly rather than collapsing them into a binary "covered / not covered."

### Trust tiers

**Strong** (sufficient on their own):

- **Lockfile tag-ratification.** A lockfile entry with a symbolic ref (tag or
  branch) whose `Hex` matches the workflow SHA. We resolved that ref to that
  SHA at lock time. This is the gold standard.
- **Immutable / protected release tag.** A tag GitHub reports as immutable
  (or that the repo's branch-protection / tag-protection rules treat as
  release-grade) currently resolves to the workflow SHA.
- **Runtime reverification.** A future runtime reverify mode (the
  `runtime-reverify-mode` card) records that the SHA was observed and
  validated at job start. Out of scope for the local check.

**Medium** (positive signal, but not on their own enough to declare OK
when nothing stronger is present):

- **Inline tag hint resolved.** A `# pin@vX.Y.Z` (or canonical equivalent)
  trailing comment names a ref, and the resolver currently resolves that ref
  to the workflow SHA. Stronger than nothing — the user explicitly asserted
  the tag — but the tag itself may be mutable, so it isn't equivalent to a
  lockfile entry.
- **Ordinary mutable tag currently points at SHA.** `SuggestTagsForSHA` finds
  a stable-semver tag whose current target is the workflow SHA, but we have
  neither a lockfile ratification nor an immutability guarantee.
- **Ancestry confirmed against default branch HEAD.** `CheckAncestry` reports
  `AncestryConfirmed`. The SHA is on the upstream's mainline lineage. This
  alone is *not* equivalent to a release tag — bot branches, automation
  branches, and merge commits all live there too.

**Weak** (presence is some evidence but absence isn't a problem):

- **Reachable from any branch.** `CheckReachability` reports `Reachable`. Tells
  us the SHA exists in the upstream's history, but not where.
- **SHA-keyed lockfile entry.** A lockfile entry whose key is the SHA itself.
  Evidence that *some prior tooling run recorded this SHA*, but not that any
  symbolic ref ever pointed at it.

**Negative** (these are the actual smells):

- **Misleading SHA-shaped ref.** Already covered by `CategoryMisleadingSHA`.
  Must be checked first; if it fires, none of the rest of this tree applies.
- **Lockfile forgery.** Already covered by `CategoryLockfileForgery`. Triggers
  only when a *symbolic ref* is in play and the locked SHA isn't an ancestor
  of the live ref it claims to represent. Bare-SHA pins do not have a claimed
  symbolic ref, so this category does not generalise to "bare SHA isn't an
  ancestor of default-branch HEAD". That case is benign-or-impostor depending
  on reachability.
- **Impostor commit.** `CategoryImpostorCommit` already fires when a SHA is
  unreachable from any upstream branch. Must run for SHA-keyed lockfile
  entries too, not just tag-keyed ones (today it only fires for tagged refs).
  See §6e.
- **Hint mismatch.** New: the user asserted `# v4.1.1` but the resolver maps
  `v4.1.1` to a different SHA. High-confidence error — see §6a.

### Sufficiency rule

A bare-SHA pin is **sufficient (no warning)** if any **Strong** channel is
present and no **Negative** channel fires.

A bare-SHA pin is **partial (INFO)** if at least one **Medium** channel is
present, no **Strong** channel is present, and no **Negative** channel fires.
Surface the partial state but don't nag — the operator can promote it to
strong by adding a lockfile entry.

A bare-SHA pin is **weakly-recorded (INFO)** if only **Weak** channels are
present.

A bare-SHA pin is **bare (WARN: `BareSHANoProvenance`)** if no channel above
INFO is present. The warning names which channel would resolve it ("add a
lockfile entry with `gh actions-pin`" or "add a `# pin@vX.Y.Z` hint").

Negative channels short-circuit everything above and produce their own
finding (forgery / impostor / hint-mismatch / misleading-SHA).

Most of today's `SHAAsRef` warnings move to "Strong: lockfile tag-ratification"
and disappear. The remaining warnings name *which channel* is missing.

## 3. Inventory: where bare SHA is currently flagged

### 3a. The structural check (root of the issue)

`internal/doctor/check_structural.go:41-62` — `checkShaAsRef`:

```go
for _, ref := range pw.Refs {
    if !lockfile.IsFullSha(ref.Ref) {
        continue
    }
    f := newRefFinding(pw, ref, CategorySHAAsRef, SeverityWarning, ConfidenceHigh)
    f.Detail = "pinned to a bare SHA without a symbolic ref — weakens supply-chain traceability"
    f.Remediation = fmt.Sprintf("pin to a tag instead: https://github.com/%s/releases", nwoLower(ref.Owner, ref.Repo))
    lockedSha := ref.Ref
    if locked, ok := depIndex[lockfile.IndexKey(ref.Owner, ref.Repo, ref.Ref)]; ok {
        lockedSha = locked.Hex
    }
    f.Dependency = synthDep(ref, lockedSha)
    out = append(out, f)
}
```

This fires on **every** SHA-shaped `uses:` ref regardless of whether the
lockfile has a corroborating entry. It is the primary noise source.

### 3b. Terminal warning surface

`cmd/gh-actions-pin/format/terminal.go:195-232` aggregates `CategorySHAAsRef`
findings, splits direct (printed) from transitive (silently swallowed), and
prints "N actions are pinned to a bare SHA without a tag ref". The TODO at
`:196-200` is the immediate prompt for this spec.

The transitive suppression is a workaround for the noise problem — if we fix
the root check, transitive bare SHAs become much rarer (only fire when the
upstream composite has no metadata channel either) and we may not need the
suppression at all. See §7.

### 3c. Auto-fix path

`internal/doctor/remediate_strategies.go:362-678` — `handleSHAAsRef` and the
chain of `applySHAToTag` calls. Today this aggressively rewrites bare SHAs to
tagged refs. Under the reframed model:

- For internal (same-owner) repos: behaviour is fine. Internal callers
  generally want tag refs and the auto-pin to default branch is convenient.
- For external repos: rewriting `actions/checkout@<SHA>` to `actions/checkout@v4`
  is **moving away from the more secure pin shape**. It should not happen
  automatically. The "fix" should instead either (a) leave the SHA alone and
  add an inline tag hint comment, or (b) add a lockfile entry that ratifies the
  SHA. See §6.

### 3d. Test fixtures that lock in the current behaviour

- `cmd/gh-actions-pin/format/terminal_test.go:55-74` — "sha-as-ref direct
  warning surfaces" expects the warning text. Will need to be re-scoped to
  "fires only when no metadata channel covers the SHA".
- `cmd/gh-actions-pin/format/terminal_test.go:136-167` — locks in the
  transitive suppression. Likely deleted or repurposed once the upstream check
  is metadata-aware.
- `internal/doctor/check_test.go` — has `checkShaAsRef` cases that will need
  table-driven extension across the metadata matrix.
- `pkg/findings/findings_test.go`, `cmd/gh-actions-pin/format/sarif_test.go`,
  `internal/lockfile/diff_test.go`, `internal/resolver/resolver_test.go`,
  `internal/resolver/reachability_integration_test.go` — touch the category
  string. Most are probably fine but should be audited.

### 3e. Documentation surfaces

- `INTEGRATION.md` and `internal/doctor/doc_urls.go` — both reference the
  category. Need updated copy that frames the warning as "missing provenance
  metadata", not "wrong ref shape".

## 4. Inventory: existing metadata channels

| Channel | Where it lives | Notes |
|--------|----------------|-------|
| Lockfile entry (tag-keyed) | `internal/lockfile.File.Workflows`; lookup via `parseWorkflowDeps` in `internal/doctor/check_run.go:41-53` | Strongest channel. A tag-keyed entry whose `Hex` matches the workflow SHA is full ratification — we resolved that tag to that SHA at lock time. |
| Lockfile entry (SHA-keyed) | Same | Currently treated identically to no entry by `checkShaAsRef`. Under the reframed model this should count as **partial** ratification — we have a record but no human-readable ref. Best treated as INFO (or OK if any other channel corroborates). |
| Live ref resolution | `Resolver.ResolveRef`, used via `checkResolver` interface in `check_resolver.go:11-26` | Current `checkMisleadingSha` already uses this for the SHA-shaped-ref-that-is-actually-a-tag case. Same plumbing can answer "does any known tag of this repo resolve to this SHA?" if we extend it. |
| Tag-object peel | `Resolver.PeelTagObject` | Resolves annotated tag SHAs to underlying commits. Already used by `checkMisleadingSha`. |
| Ancestry | `Resolver.CheckAncestry` (`internal/resolver/ancestry.go`) | Returns `AncestryConfirmed` / `NotAncestor` / `Unknown`. Currently only used for ref-moved → forgery escalation, but the "is this SHA on the lineage?" question generalises. |
| Reachability | `Resolver.CheckReachability` | Returns `Reachable` / `Unreachable` / `Unknown`. Used by `checkImpostorCommit` and `liveRefImpostorFinding`. |
| Tag suggestions for SHA | `tagLister.SuggestTagsForSHA` (used in `handleSHAAsRef`) | Returns tags that currently point at a given SHA. Today only used by the auto-fix flow but is a strong corroborating signal — if a stable tag exists pointing at the SHA, the SHA is on a release. |
| Default-branch HEAD | `tagLister.GetRepoInfo` | Used by remediation. Could feed an "is the SHA an ancestor of default-branch HEAD?" check via `CheckAncestry`. |
| Inline tag hint comment | **Does not exist yet** | Standard convention is `uses: actions/checkout@<sha>  # v4.1.1`. The repo's lockfile YAML rewriter already preserves trailing comments (`internal/lockfile/lockfile_test.go:108`). A parser pass that surfaces these as structured `RefHint` metadata is straightforward. See §6. |
| Runtime reverify | **Does not exist yet** | Tracked under `runtime-reverify-mode` card. Out of scope here but counted as a future channel. |

## 5. Decision tree (replaces the current `IsFullSha → warn`)

For each `uses:` ref where `lockfile.IsFullSha(ref.Ref)` is true, evaluated in
this order. Negative channels short-circuit; positive channels accumulate to
the strongest tier observed.

```
# 0. Negative channels first — these are not "missing metadata", they are
#    real smells and trump everything else.

if checkMisleadingSha fires:
    # SHA-shaped ref that resolves to a different commit (and isn't an
    # annotated tag object). Already-handled by check_misleading.go.
    return ERROR(MisleadingSHA)

if inline_tag_hint_present(ref) and not hint_resolves_to_sha(ref):
    return ERROR(HintMismatch)
    # The user asserted v4.1.1 and we can prove the SHA isn't v4.1.1.

if reachability(ref.SHA, anywhere upstream) == Unreachable:
    return ERROR(ImpostorCommit)
    # Today this only fires for tag-keyed entries; §6e generalises it.

# 1. Strong channels — sufficient on their own.

if lockfile_tag_entry_matches_sha(ref):
    return OK("lockfile tag-ratified")

if immutable_tag_resolves_to_sha(ref):
    return OK("immutable release tag")

# 2. Medium channels — partial provenance.

partial_signals = []
if inline_tag_hint_resolves_to_sha(ref):
    partial_signals.append("inline hint resolved")
if any_stable_semver_tag_currently_resolves_to_sha(ref):
    partial_signals.append("tag currently points at SHA")
if ancestry_confirmed_against_default_branch_head(ref):
    partial_signals.append("on default-branch lineage")

if partial_signals:
    return INFO("partial provenance: " + ", ".join(partial_signals))

# 3. Weak channels — recorded but not corroborated.

weak_signals = []
if lockfile_sha_entry_exists(ref):
    weak_signals.append("lockfile sha-entry")
if reachability(ref.SHA, anywhere upstream) == Reachable:
    weak_signals.append("reachable from some branch")

if weak_signals:
    return INFO("recorded only: " + ", ".join(weak_signals))

# 4. Inconclusive — distinguish from absent.

if ancestry == Unknown or reachability == Unknown:
    return WARN(BareSHAUnverified, "could not verify provenance: API unavailable")

# 5. Catch-all — no positive signal, no inconclusive signal.

return WARN(BareSHANoProvenance,
            "no lockfile entry, no tag hint, no upstream corroboration")
```

Notes on the ordering:

- **Default-branch ancestry as a positive signal only.** `not_ancestor` does
  *not* mean forgery here — bare-SHA pins legitimately live on release
  branches, maintenance branches, bot branches, and PR merge commits, none
  of which are ancestors of `main`. `LockfileForgery` is reserved for the
  case where a *claimed symbolic ref* doesn't have the locked SHA in its
  lineage; bare-SHA pins make no such claim.
- **Reachability is the negative signal.** If the SHA is unreachable from
  any branch of the upstream repo, that's the impostor-commit shape and
  already a high-confidence error.
- **`AncestryUnknown` against default branch is weaker than against a
  claimed ref.** See §7 for the coordination story.
- **`AnyTagForSHA` is gated.** It's an extra API call per bare-SHA ref, so
  in practice the implementation should consult cache first and fall back
  to the network only when no Strong channel resolved. See §6b.

## 6. New machinery this spec requires

### 6a. Inline tag hint parser

Add a `RefHint` field on `lockfile.ActionRef` populated from trailing comments.
Parsing is deliberately strict to avoid false positives from arbitrary prose:

**Canonical forms (always recognised):**

- `# pin@<ref>` — preferred; matches the `pin@vX.Y.Z` mental model
- `# actions-pin: <ref>` — explicit tool attribution
- `# tag: <ref>` — generic

**Permissive form (recognised only when the trailing comment is the
*entire* comment, with no other tokens):**

- `# vX[.Y[.Z]][-prerelease]` — bare semver tag and nothing else after it.

A trailing comment like `# v4 broke on Windows` is **not** a hint — it has
prose after the version. `# copied from v1.2.3 docs` is **not** a hint — `v1.2.3`
is embedded mid-sentence. Only the canonical forms or a whole-comment-bare-semver
count.

**Allowed ref shapes inside a hint:**

- Stable: `v1`, `v1.2`, `v1.2.3`
- Prerelease: `v1.2.3-beta.1`, `v1.2.3-rc.2`
- Non-semver release tags (e.g. `release/v1`) — only via canonical forms,
  never via the permissive bare form.

**Hint validation.** When a hint is present, the resolver checks `ResolveRef(hint)`
matches the workflow SHA. Mismatch produces `HintMismatch` (ERROR — see §5).

**Why ERROR for mismatch:** The user *explicitly asserted* a tag. If we can
prove the assertion is false the developer either typo'd, the upstream tag
moved, or someone tampered with the workflow file. All three are worth
surfacing loudly.

The lockfile YAML rewriter already preserves trailing comments (see
`internal/lockfile/lockfile_test.go:108`), so the read-side extension does not
require touching the writer.

### 6b. Generalised tag lookup with strength annotation

Extend the resolver to expose `AnyTagForSHA(owner, repo, sha) []TagInfo` where
each `TagInfo` carries:

- `Name` (e.g. `v4.1.1`)
- `Immutable bool` — GitHub reports the tag as immutable, OR a tag-protection
  rule covers it
- `IsStableSemver bool`
- `Annotated bool`

The decision tree consumes:

- An immutable / protected tag pointing at the SHA → **Strong** ("immutable
  release tag").
- Any stable-semver tag pointing at the SHA → **Medium** ("tag currently
  points at SHA").
- Lightweight non-release tags do not contribute. They're too cheap for
  attackers to create and too easy for repo owners to retarget.

**API budget guardrail.** `AnyTagForSHA` is only called when no Strong channel
resolved (i.e. no lockfile tag-entry match). Cache hits stay cheap; cache
misses are gated and should be coalesced per `(owner, repo)` so a workflow
with N bare-SHA refs against the same repo costs one API call, not N.

### 6c. Auto-fix changes

`handleSHAAsRef` (`internal/doctor/remediate_strategies.go:362-678`) splits by
trust outcome rather than by ref shape. Concrete outcomes per case:

- **Strong already (lockfile tag-ratified, immutable tag):** no remediation
  needed. The check shouldn't fire and the remediation handler doesn't run.
- **Medium with inline hint that resolves:** offer to *promote* by adding a
  tag-keyed lockfile entry (running the existing `gh actions-pin` flow with
  the hint's ref as the source ref). Don't rewrite the workflow file.
- **Medium with currently-resolving tag, no hint:** offer to add a `# pin@<tag>`
  hint comment **and** a lockfile entry. Default action is "leave SHA, add
  hint + lockfile entry".
- **Weak (sha-entry only or reachable only):** the same lockfile-entry
  remediation, but the prompt explicitly says "this records the SHA but does
  not establish a verified release ref." Output should not present this as
  full provenance — the resulting state is still INFO.
- **Bare (no metadata at all):** prompt user to either run `gh actions-pin`
  to refresh from a tag, or accept the bare-SHA state explicitly. The prompt
  surfaces *which* channel would resolve the warning.
- **Same-owner repos (internal):** preserve today's behaviour. Same-owner
  bare-SHA refs auto-rewrite to a tag or default branch. Internal callers
  generally want tag refs and there is no fork-network risk inside an org
  boundary. This is called out as a deliberate exception in user-facing docs.

Critically, the SHA → tag rewrite for **external** repos is no longer the
default. It moves behind an explicit `--prefer-tags` flag for users who
genuinely want tag refs in their workflow files.

**Compat for users who relied on the rewrite.** The CLI gains a config option
(`prefer-tags: true` in repo config or env var equivalent) that flips the
default back on a per-repo basis. The CHANGELOG entry calls out the change
prominently. One release of telemetry before flipping the default counts how
often the rewrite would have happened.

### 6d. Telemetry hooks

The dual-run phase (§8) needs concrete fields, not vibes. Per finding, record:

- `decision_path` — which branch of the §5 tree fired (`lockfile_tag`,
  `immutable_tag`, `hint_resolved`, `tag_currently_points`, `default_branch_ancestry`,
  `lockfile_sha`, `reachable`, `unknown`, `unreachable`, `hint_mismatch`,
  `none`)
- `direct_or_transitive` — whether this is a direct workflow ref or a
  transitive composite-action ref
- `old_verdict` — what `checkShaAsRef` would have emitted under the current
  code path (`SHAAsRef:warn`, `none`)
- `new_verdict` — what the new tree emits (`OK`, `INFO`, `WARN(category)`,
  `ERROR(category)`)
- `would_remediate_old` — would the old auto-fix have rewritten SHA → tag
- `would_remediate_new` — would the new flow remediate (and to what)
- `resolver_failures` — count of `ResolveRef` / `CheckAncestry` /
  `CheckReachability` / `AnyTagForSHA` failures encountered for this finding
  (rate-limit attribution matters)
- `repo_hash` — anonymised stable hash of `owner/repo` (privacy-policy
  permitting), so we can spot whether disagreements concentrate in a few
  repos vs spread evenly

The success criterion for the dual-run phase is:

- Volume of `decision_path=none` ≤ 10% of today's `SHAAsRef:warn` volume
  (most warnings should have a corroborating channel).
- `decision_path=hint_mismatch`, `unreachable`, and `none` rates broadly
  match today's `MisleadingSHA`/`ImpostorCommit`/`SHAAsRef` rates
  respectively (we're not silently hiding real problems, only reframing
  the noisy ones).
- `would_remediate_old=true ∧ would_remediate_new=hint+lockfile` is the
  bulk of the rewrite-removal blast radius. If a non-trivial population
  has `would_remediate_new=none`, we may need to lengthen the dual-run
  phase or add a louder warning before flipping defaults.

### 6e. Generalise `checkImpostorCommit` to SHA-keyed entries

Today `checkImpostorCommit` (`internal/doctor/check_misleading.go:148-177`)
skips entries where `lockfile.IsFullSha(ref.Ref)` is true (its second early
return). That made sense when SHA-shaped refs were already covered by
`SHAAsRef`. Under this spec, the impostor check needs to fire for SHA-shaped
refs too, otherwise a SHA-keyed lockfile entry pointing at an unreachable
commit slips through with at most an INFO finding.

Concretely: drop the `IsFullSha` early return, and use `ref.Ref` as both the
SHA-under-test and the upstream branch reference (`CheckReachability` already
accepts `ref` as an arbitrary symbolic name; for bare-SHA refs we'd need to
plumb the default branch through, or call `CheckReachability` against
"any branch" semantics — implementer's choice).

## 7. Coordination with other open cards

### `ancestry-unknown-severity-escalation`

That card escalates `AncestryUnknown` from a soft warning toward a forgery-class
error after retries. The escalation logic was designed for the
**ref-moved → forgery** path, where we're comparing the locked SHA against a
*claimed symbolic ref* (the workflow says `actions/checkout@v4`, the lockfile
says SHA X, the live `v4` resolves to SHA Y, so we ask whether X is an
ancestor of Y). Unknown there genuinely is suspicious — the claim is concrete
and we can't verify it.

The bare-SHA decision tree introduces a *different* unknown question: "is this
SHA on the upstream's lineage at all?" That's a much weaker claim and an
unknown answer is much more often benign (rate-limit, cold cache, large repo).
Promoting it to forgery-class would generate false positives.

To keep both cards coherent, this spec proposes splitting the unknown taxonomy:

- **`AncestryUnknownForClaimedRef`** — current behaviour. The user-or-lockfile
  asserted a symbolic ref. Eligible for the
  `ancestry-unknown-severity-escalation` upgrade path.
- **`AncestryUnknownForBareSHA`** — diagnostic only. Counts as the
  `BareSHAUnverified` WARN in §5 but does not escalate to forgery. The
  remediation is "retry when the API is available", same as today's soft
  ancestry-unknown.

Recommendation: ancestry-unknown-severity-escalation explicitly only applies
to the claimed-ref variant. The bare-SHA variant uses its own category and
stays soft.

### `orphan-tag-warn-only-rollout`

The inverse problem: a tagged ref where the tag is moveable / the SHA cannot
be corroborated against a stable release. The two cards together cover the
full matrix:

|                | Tag-shaped ref            | SHA-shaped ref           |
|----------------|---------------------------|--------------------------|
| metadata OK    | OK                         | OK (this spec)           |
| metadata bad   | orphan-tag (warn)          | BareSHANoProvenance (warn) |

**Mutual-exclusion rules** to prevent double-warning the same dependency:

- If a workflow uses a SHA *and* the lockfile contains a tag-keyed entry whose
  `Hex` matches that SHA, the bare-SHA check sees Strong tag-ratification and
  emits no finding. The orphan-tag check should not separately complain about
  the lockfile entry being "orphan" — its `Hex` ratifies a real workflow ref.
- If both checks would fire on the same `(owner, repo)` for related but
  not-identical reasons (e.g. workflow has SHA, lockfile has a tag whose
  current resolution differs from the workflow SHA), the framing should be
  one finding — `RefMoved` already covers the "lockfile and live ref disagree"
  shape and should win.

Naming should converge so the user sees a single "missing provenance metadata"
framing in both shapes, with sub-classifiers for which channel is missing.

### `runtime-reverify-mode`

Adds a Strong channel on top of the trust tiers. The decision tree above is
structured so adding it is purely additive — a new clause near the top of the
Strong tier check (`if runtime_reverified_recently(ref): return OK`). No table
re-architecture needed.

## 8. Migration plan

1. **Phase 0 — parser groundwork (no behaviour change).** Add `RefHint`
   parsing per §6a, the `AnyTagForSHA` resolver method per §6b, and the
   `checkImpostorCommit` generalisation per §6e. Wire the telemetry fields
   from §6d but emit them only to internal logs / a dev-only flag — no
   user-visible warning text changes yet.
2. **Phase 1 — silent dual-run.** Run the new decision tree alongside the
   current `checkShaAsRef` on every diagnose call. The user still sees the
   current warnings; telemetry records the new verdict and the disagreement
   per finding. Bake for one or two `gh-actions-pin` releases. Success
   criteria are defined in §6d. If they're not hit, fix the gaps before
   advancing.
3. **Phase 2 — flip behaviour.** Replace `checkShaAsRef` with the new
   metadata-aware check. Rename `CategorySHAAsRef` to
   `BareSHANoProvenance` and have it fire only on the catch-all branch.
   Auto-fix defaults flip per §6c. CLI gains the `--prefer-tags` flag and
   the `prefer-tags` repo config option for users who want the old rewrite
   default. CHANGELOG entry calls out the change explicitly.
4. **Phase 3 — cleanup.** If telemetry shows the transitive-bare-SHA
   warning is now negligible, delete the suppression hack in
   `terminal.go:202-218`. Update `INTEGRATION.md` and SARIF docs to the
   metadata framing. Decide on SARIF rule alias retention (see §9).

User-visible diff at the end of phase 2:

- Most `gh actions-pin doctor` runs go from "N actions pinned to a bare SHA"
  to silent or to a few INFO lines.
- Workflows with bare SHAs **and no lockfile / hint / lineage** still warn,
  with a precise message (`BareSHANoProvenance` — names the missing channel)
  rather than a shape-of-ref scolding.
- New errors appear in cases that used to slip through silently: hint
  mismatch (user asserted a tag the SHA doesn't match), and impostor
  commits behind SHA-keyed lockfile entries.
- Auto-fix on external repos no longer silently rewrites SHA → tag. Users
  who want that opt in via `--prefer-tags` or `prefer-tags: true`.

## 9. Open questions

- **Default-branch ancestry semantics.** Today `CheckAncestry` is called
  against a specific `head` (the live SHA of a claimed ref). For bare-SHA
  diagnostics we'd call it against default-branch HEAD. Is that the right
  base, or should we also try release-branch HEADs (`release/*`,
  `maintenance/*`)? Recommend default branch only for v1; revisit if
  telemetry shows lots of `decision_path=none` cases that would have
  resolved against a release branch.
- **Cost of `AnyTagForSHA`.** §6b gates this behind cache and per-`(owner,
  repo)` coalescing. If the gating isn't sufficient in practice (large
  monorepo workflows hitting many distinct upstreams), is there a feature
  flag to disable the check entirely? Recommend yes:
  `--skip-tag-corroboration` for users who hit rate limits.
- **Tag immutability detection.** GitHub exposes immutability for releases
  and `tag_protection` rules separately. Does the resolver have access to
  both, or do we treat only one as Strong evidence? Confirm with whoever
  owns `internal/resolver/peel.go` before implementing §6b.
- **Hint canonicalisation.** When `gh actions-pin` writes a hint, should
  it always write `# pin@vX.Y.Z` (consistent), or preserve a user's
  existing canonical form if there is one? Recommend: always normalise to
  `# pin@vX.Y.Z` on write, accept the broader canonical set on read.
- **Same-owner exception in §6c.** Today same-owner bare-SHA refs
  auto-rewrite to tag/default-branch. Spec preserves that. Is it worth
  unifying internal and external policy under the new model? Recommend
  not for v1 — the internal-vs-external split has worked well and breaking
  it adds risk for no obvious gain.
- **SARIF rule ID retention.** Renaming `SHAAsRef` → `BareSHANoProvenance`
  changes rule IDs in CodeQL-style consumers. Keep `SHAAsRef` as an alias
  for one release? Recommend yes — drop the alias in the release after
  Phase 3.
- **Hint mismatch severity.** Spec calls it ERROR. Some operators may
  prefer WARN for a soft rollout. Open to feedback from the
  `orphan-tag-warn-only-rollout` retrospective — if that team learned
  hard categories slow adoption, we should follow suit.
- **Transitive deps post-rollout.** Phase 3 plans to delete the
  transitive-suppression hack. Telemetry has to actually show negligible
  transitive bare-SHA warnings before we pull the trigger; otherwise
  users see a flood of new noise from composite-action authors who
  haven't adopted lockfile pinning. Concrete kill threshold:
  fewer than 1% of dual-run diagnoses produce transitive-bare-SHA findings
  with `decision_path=none`.

## References

- PR review thread that triggered this spec (parent session
  `nodeselector/fix-sha-pin-destruction`)
- `cmd/gh-actions-pin/format/terminal.go:196-232`
- `internal/doctor/check_structural.go:41-62`
- `internal/doctor/check_misleading.go`
- `internal/doctor/check_resolver.go`
- `internal/doctor/check_run.go`
- `internal/doctor/remediate_strategies.go:362-678`
- `internal/resolver/ancestry.go`
- `internal/resolver/peel.go`
- `internal/lockfile/lockfile_test.go:108`
- Sister cards: `ancestry-unknown-severity-escalation`,
  `orphan-tag-warn-only-rollout`, `runtime-reverify-mode`
