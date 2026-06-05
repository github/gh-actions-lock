# Spec: AncestryUnknown severity escalation via reachability cross-reference

Status: **draft / discussion**
Owner: nodeselector
Branch: `nodeselector/docs-ancestry-unknown-spec` (off `nodeselector/fix-sha-pin-destruction`)
Scope: design only — no implementation in this branch.
Coordination: this spec covers **only the claimed-ref subcase** of
`AncestryUnknown`. The bare-SHA variant is out of scope and handled by
[`bare-sha-not-a-smell.md`](./bare-sha-not-a-smell.md). See §Coordination.

## Problem

`internal/resolver/ancestry.go::CheckAncestry` returns one of `AncestryConfirmed`,
`AncestryNotAncestor`, or `AncestryUnknown`. The `Unknown` arm is reached when the
GitHub Compare API can't deliver a verdict even after the bounded retry budget — in
practice, sustained rate-limit pressure or transient 5xx storms.

`checkRefMovedAndForgery` maps that outcome to `CategoryAncestryUnknown` /
`SeverityWarning` / `ConfidenceMedium`. The function only runs for **non-full-SHA**
workflow refs (`if lockfile.IsFullSha(ref.Ref) { continue }` at the top of the
loop) that have a matching lockfile pin — i.e. the workflow asserts a symbolic
ref (`actions/checkout@v4`), the lockfile pins SHA X, the live `v4` resolves to
SHA Y. The unknown question is: "is X an ancestor of Y?" The user's claim is
falsifiable, which is exactly why warning-on-Unknown is unsatisfying — a real
lockfile forgery whose Compare check happens to be rate-limited gets demoted
from "investigate immediately" to a soft warning indistinguishable from a benign
release tag move. An attacker who can time tampering against API pressure gets
that demotion for free.

The bare-SHA variant — workflow uses a SHA with no symbolic claim, ancestry
miss is more often benign (feature branch, fork commit, abandoned branch) — has
no falsifiable assertion and is outside this spec.

We do not want to escalate severity on bare API failure — that flips the false-
positive risk in the opposite direction. We *do* want to use whatever orthogonal
evidence is cheap to collect to either:

1. confidently keep the finding at warning (suppress further worry), or
2. surface a stronger finding *via a different category* (the impostor-commit
   detector already does this and is unaffected by Compare rate-limits on the
   `pinned...live` axis).

The goal of this memo is to fix the decision tree, identify the cross-reference
data we already have, and define the test matrix needed to land the change
safely.

## Cross-reference signal

The relevant orthogonal signal is **reachability of the lockfile-pinned SHA**:
"is `pin.Hex` on any branch of `owner/repo`?" — exactly what
`Resolver.CheckReachability(owner, repo, pin.Hex, ref)` answers via the
`branch_commits`-style scan in `internal/resolver/peel.go`.

Why this signal:

- Reachability is **decoupled from the Compare-API axis** that produced the
  `AncestryUnknown`. Compare can be rate-limited while branch listing + per-
  branch contains-check still succeed (and vice versa). The two checks fail
  independently in practice.
- The result for `pin.Hex` is **already computed** before `runChecks` starts.
  `diagnose.go` runs `CheckReachabilityAll` over `pw.ExistingDeps` and stores
  results in a `prewarmedResolver` keyed by `cachekey.ForReach(owner, repo,
  sha, ref)`. The implementation cost of the `CheckReachability` lookup we
  add to the `AncestryUnknown` arm is therefore one map read against
  pre-existing data — no new API call.
- `CheckReachability` itself fails *open* into `ReachabilityUnknown` when its
  own scan can't complete. The `prewarmedResolver` collapses *absent* keys
  into the same `ReachabilityUnknown` status (see
  `internal/doctor/check_resolver.go:86-91`). That's a real interface
  limitation: at the call site, "we tried and got Unknown" and "we never
  asked" are indistinguishable. The decision tree below treats them as one
  cell.

## Decision tree

For a finding currently at `CategoryAncestryUnknownForClaimedRef /
SeverityWarning` (renamed per the [bare-sha-not-a-smell coordination](#coordination)),
given the reachability state of the **lockfile-pinned SHA** (`pin.Hex`)
against the upstream ref:

| Reachability(`pin.Hex`, ref)               | Outcome                                                                                       | Severity / Confidence | Rationale                                                                                                                |
| ------------------------------------------ | --------------------------------------------------------------------------------------------- | --------------------- | ------------------------------------------------------------------------------------------------------------------------ |
| `Reachable`                                | Keep `CategoryAncestryUnknownForClaimedRef`; enrich detail.                                   | warning / medium      | Locked SHA is in legitimate lineage. The observed SHA being newer is most plausibly a benign tag move. Forgery unlikely. |
| `Unreachable`                              | **Suppress** `CategoryAncestryUnknownForClaimedRef`; let `CategoryImpostorCommit` win.        | (n/a — suppressed)    | The pinned SHA is not on any branch — `checkImpostorCommit` already emits an impostor finding at error severity.         |
| `ReachabilityUnknown` (incl. "not asked")  | Keep `CategoryAncestryUnknownForClaimedRef`; enrich detail to note compound uncertainty.      | warning / low         | Both Compare and branch_commits failed (or branch_commits was never run for this dep). Confidence drops; severity unchanged because we still can't claim forgery. The current `checkResolver` interface conflates these two states (see prewarmedResolver behavior); see Open Questions. |

Two key non-decisions:

- **No severity escalation in any branch.** "Escalation" in the title is
  legacy framing. The actionable change is **mutual exclusion** with the
  impostor finding (so we don't double-classify) and **enrichment** of the
  detail/remediation strings using the new evidence. If we ever want to
  escalate to error, the trigger should be `AncestryNotAncestor` from
  Compare, not a heuristic stack on top of `Unknown`.
- **No new cross-finding category for *this* card.** This spec doesn't
  invent a new finding. It does adopt the
  `CategoryAncestryUnknownForClaimedRef` / `CategoryAncestryUnknownForBareSHA`
  rename proposed by [`bare-sha-not-a-smell.md`](./bare-sha-not-a-smell.md)
  §7. After that rename the partition the doctor uses is
  (`CategoryAncestryUnknownForClaimedRef`,
  `CategoryAncestryUnknownForBareSHA`, `CategoryImpostorCommit`,
  `CategoryLockfileForgery`, `CategoryRefMoved`). This card only touches
  the first.

## Implementation sketch (NOT IN THIS BRANCH)

The change is local to `checkRefMovedAndForgery` plus minor wiring:

1. In the `case resolver.AncestryUnknown:` arm of `checkRefMovedAndForgery`:

   - call `r.CheckReachability(ref.Owner, ref.Repo, pin.Hex, ref.Ref)`
   - branch on the result per the table above
   - on `Unreachable`, **drop** the `AncestryUnknown` finding and let
     `checkImpostorCommit` produce `CategoryImpostorCommit` for the same
     `(owner, repo, ref)` — note that today `checkImpostorCommit` is gated
     by `forgeryKeys`; we do **not** want to add `ancestryUnknownKeys` to
     that gate, because the whole point is to let the impostor finding
     fire.
   - on `Reachable` / `ReachabilityUnknown`, append a one-line summary of
     the reachability state to `Finding.Detail` so operators can tell which
     bucket they're in without reading the source.

2. The existing `liveRefImpostorFinding(...)` call inside the
   `AncestryUnknown` arm — which checks reachability of the **observed**
   SHA — stays as-is. It is targeting a different attack shape (live ref
   moved into a fork-network commit) and is independent of pin.Hex
   reachability.

3. Caching: no new cache, no new API call. `diagnose.go` already runs
   `CheckReachabilityAll` over `pw.ExistingDeps` before `runChecks` starts
   and stuffs the results into the `prewarmedResolver` map. The lookup we
   add to the `AncestryUnknown` arm is a map read against that prewarmed
   data — same map that `checkImpostorCommit` reads three checks later. If
   `pw.SkipReachWhenUnchanged` short-circuits the prewarm for this dep, the
   call lands in the absent-key path of `prewarmedResolver.CheckReachability`
   and returns `ReachabilityUnknown`; the decision tree handles that cell.

4. **Cost ceiling.** Because the lookup is against prewarmed data, the
   `AncestryUnknown` arm makes zero new API calls. The only cost change is
   the additional map read plus the suppression-vs-emit branch. If a future
   refactor removes the prewarm and falls back to live `CheckReachability`,
   that call inherits its own list-branches + Compare budget; it does not
   extend the `CheckAncestry` retry budget. Worst case: one warning, no
   retry storm.

### Rejected alternative: plumb reachability state through `checkResolver`

We could thread an explicit `ReachabilityFor(owner, repo, sha, ref)
(status, fetched bool)` view through `checkResolver` so
`checkRefMovedAndForgery` reads pre-computed state explicitly *and*
distinguishes "asked and got Unknown" from "never asked." Rejected for
now: the prewarm already handles overlap and the two `Unknown` cells
collapse to the same outcome (warning / low / enriched). Revisit if we
ever want different behavior for "never asked" vs "asked and got
Unknown" — that's a real interface gap, just not one we need to widen
the surface for today. See open question 2.

## Test matrix

Rate-limited Compare (`AncestryUnknown`) × reachability of pin.Hex × expected
findings emitted for the (workflow ref, lockfile pin) tuple:

| Compare result      | Reachability(pin.Hex, ref)                     | Expected findings                                                                  |
| ------------------- | ---------------------------------------------- | ---------------------------------------------------------------------------------- |
| `AncestryUnknown`   | `Reachable`                                    | `CategoryAncestryUnknownForClaimedRef` (warning, enriched).                        |
| `AncestryUnknown`   | `Unreachable`                                  | `CategoryImpostorCommit` only (error). `AncestryUnknownForClaimedRef` suppressed.  |
| `AncestryUnknown`   | `ReachabilityUnknown` (incl. absent in prewarm)| `CategoryAncestryUnknownForClaimedRef` (warning, low confidence, enriched).        |

Plus invariants we must preserve from existing tests:

- `AncestryNotAncestor` continues to emit `CategoryLockfileForgery` (error)
  regardless of reachability state.
- `AncestryConfirmed` continues to emit `CategoryRefMoved` (warning) plus
  whatever `liveRefImpostorFinding` produces for the *observed* SHA.
- The `forgeryKeys` mutex with `checkImpostorCommit` continues to suppress
  impostor findings when forgery fires. The new "AncestryUnknown +
  Unreachable" path is the inverse — we want impostor to fire and
  ancestry-unknown to back off.
- `liveRefImpostorFinding` for the **observed** SHA continues to fire on
  the `AncestryUnknown` path when the observed SHA is itself unreachable.
  That's a separate axis from the pin.Hex check we're adding.

Test surface: `internal/doctor/check_test.go` (table-driven cases for the
four rows above), and `internal/doctor/check_resolver_test.go` if the
fake resolver gains anything (it shouldn't, per the rejected-alternative
section).

## Open questions

1. **SARIF / terminal output**: the `CategoryAncestryUnknownForClaimedRef`
   finding's detail string is rendered downstream in
   `cmd/gh-actions-pin/format/`. Confirm the enriched detail (with
   reachability summary) renders cleanly in both terminal and SARIF
   without exceeding any known downstream size limits. The rename from
   `CategoryAncestryUnknown` also needs the SARIF rule-id table updated
   in lock-step (see `sarif.go:221-227`); coordinate with whichever card
   lands the rename first.

2. **Distinguishing prewarmed-Unknown from absent-from-prewarm.** Today
   the `prewarmedResolver.CheckReachability` shape returns
   `ReachabilityUnknown` for both "we ran branch_commits and it was
   inconclusive" and "we never enqueued this dep for prewarm" (e.g.
   when `pw.SkipReachWhenUnchanged` short-circuits). Operationally
   they're treated the same in this spec. If we later want different
   behavior — e.g. trigger a live reachability call on absent-from-
   prewarm but not on prewarmed-Unknown — the `checkResolver` surface
   would need to expose the distinction (e.g. `(status, fetched bool)`).
   Out of scope for this memo.

3. **Observed-SHA reachability as a third axis.** The `AncestryUnknown`
   arm currently calls `liveRefImpostorFinding` for the **observed**
   SHA, which folds Reachable / ReachabilityUnknown / Unreachable down
   to a boolean "emit impostor or not." That means the partition we
   build on top of pin.Hex reachability silently treats observed-SHA
   `Reachable` and observed-SHA `ReachabilityUnknown` as equivalent.
   The second case has more uncertainty than the first and could
   warrant a confidence demotion or detail enrichment. Worth
   considering in implementation, but does not change the outcome of
   any cell in the matrix above.

4. **Confidence demotion**: do we want to drop confidence to `Low` in the
   `ReachabilityUnknown` row, or leave at `Medium` to avoid a UI churn
   for what is still the same severity? Leaning Low for honesty.

5. **Telemetry** (if any): should we emit a counter for the matrix cells
   above so we can see in production how often each cell hits? Out of
   scope for this memo, worth a follow-up issue.

6. **`AncestryNotAncestor` + `Unreachable` of pin.Hex**: today this
   collapses to `CategoryLockfileForgery` and forgery suppresses impostor.
   That's still right (forgery is the stronger claim), but worth
   double-checking the test that exercises both signals firing
   simultaneously.

## Non-goals

- Severity escalation of `AncestryUnknown` to `SeverityError` under any
  reachability state. If we ever go there, the trigger should come from
  Compare, not heuristics layered on top of `Unknown`.
- A new finding category. The existing four cover the partition.
- Changes to retry budgets, backoff, or rate-limit detection in
  `ancestry.go` or `peel.go`.

## Coordination

### `bare-sha-not-a-smell.md` (sibling spec, integrated as `763117d`)

That spec proposes splitting today's single `CategoryAncestryUnknown` into
two categories before any escalation logic lands:

- `CategoryAncestryUnknownForClaimedRef` — workflow asserts a symbolic ref
  (tag/branch) and the resolver couldn't verify the SHA↔ref relationship.
  Falsifiable claim. **This spec's domain.**
- `CategoryAncestryUnknownForBareSHA` — no symbolic ref, just a SHA. No
  falsifiable claim. Stays soft; no reachability escalation. **Their
  spec's domain.**

**Agreement.** The split is the right shape and this card narrows
accordingly. The narrowing is a no-op at the code level — the function
this spec modifies (`checkRefMovedAndForgery`) already skips bare-SHA
workflow refs at line 57 (`if lockfile.IsFullSha(ref.Ref) { continue }`),
so the decision tree above was always going to operate only on the
claimed-ref subset. The categorization rename just makes that boundary
visible in the finding taxonomy and in any future SARIF / docs surface.

**Implementation ordering.** Whichever card lands first owns the rename
of `CategoryAncestryUnknown` → `CategoryAncestryUnknownForClaimedRef`
(plus a new `CategoryAncestryUnknownForBareSHA` constant in
`internal/doctor/finding_aliases.go` and the SARIF rule entry in
`cmd/gh-actions-pin/format/sarif.go`). If this card lands first, the new
bare-SHA category is introduced as a defined-but-unused constant pending
the bare-sha spec's check implementation. Either order is fine; neither
spec blocks the other.

**No disagreements to document.** The sibling spec's framing — escalation
only makes security sense against falsifiable assertions — is correct and
matches the original intent of this card. Title-level "escalation"
language in this memo is now doubly inaccurate: not only do we not
escalate severity (per the original rubber-duck pass), we also don't
even cover the bare-SHA case. Title kept for traceability with the
parent tracking issue.

## References

- `internal/resolver/ancestry.go::CheckAncestry`
- `internal/resolver/peel.go::CheckReachability`
- `internal/doctor/check_misleading.go::checkRefMovedAndForgery`,
  `liveRefImpostorFinding`, `checkImpostorCommit`
- `internal/doctor/finding_aliases.go` — category constants
- `cmd/gh-actions-pin/format/sarif.go:221-227` — SARIF rule entry that
  needs to move in lock-step with any category rename
- [`docs/specs/bare-sha-not-a-smell.md`](./bare-sha-not-a-smell.md) §7
  — sibling spec; source of the claimed-ref/bare-SHA category split
