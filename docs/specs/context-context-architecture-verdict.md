# Verdict: `context.Context` architecture in 2026 Go

**Status:** decision memo, not a spec. No code changes accompany this doc.
**Question:** in 2026, is shipping a Go CLI with zero `context.Context`
propagation embarrassing, or pragmatic? Defer the wiring or do it now while
the surface is small?

**Answer:** do it now. The retrofit is mechanical, the upstream is ready,
the convention is established, and every week we wait the bill gets bigger
in proportion to lines of code, not features.

---

## 1. Audit: what cancellation actually does today

### 1a. Entry point

`cmd/gh-actions-pin/main.go`:

```go
func main() { os.Exit(Execute()) }
```

`Execute()` calls `NewRootCmd(f).Execute()` — i.e. cobra's plain `Execute`,
not `ExecuteContext` or `ExecuteContextC`. There is no `signal.NotifyContext`,
no `os/signal.Notify`, no goroutine watching for `SIGINT`/`SIGTERM`. When the
user hits `Ctrl-C` during a long resolver scan, the Go runtime's default signal
handler kills the process. The spinner does not get cleared, the narration log
is not flushed cleanly, in-flight HTTP requests are abandoned by the OS rather
than cancelled by us, and partial state (caches, half-written diagnostics) is
discarded.

### 1b. Repo-wide ctx footprint

Two files mention `context` at all (`grep -l 'context\.'`), and both hits are
the English word in a doc comment:

- `internal/resolver/httpclient.go:11` — "// New creates a resolver using the
  authenticated gh context."
- `internal/doctor/finding.go:51` — "// InventoryEntry describes a single
  dependency with context."

Zero imports of the `context` package. Zero `context.Context` parameters.
Zero `cmd.Context()` calls in `RunE` bodies. The codebase is, definitionally,
ctx-free.

### 1c. HTTP layer: the actual call sites

`internal/resolver/httpclient.go` constructs `cli/go-gh/v2/pkg/api`'s
`GraphQLClient` and `RESTClient` and stores both on the `Resolver`. Every
HTTP call goes through one of:

```
r.client.Do(query, vars, &resp)              // GraphQL, no ctx
r.client.Query(name, q, vars)                // GraphQL, no ctx
r.restClient.Get(path, &resp)                // REST, no ctx
r.restClient.Do(method, path, body, &resp)   // REST, no ctx
```

`grep` shows ~18 such call sites in `internal/resolver/`, plus a handful in
`internal/doctor/tags.go`.

The non-ctx variants on the `cli/go-gh/v2` clients are implemented as
`DoWithContext(context.Background(), ...)`. That's not speculation — the
upstream source confirms it. Every HTTP call this CLI makes today is
implicitly bound to `context.Background()`. **There is no path for a caller
to cancel an in-flight request short of killing the process.**

### 1d. `internal/pinpool/pool.go`

`pinpool.Run[T]` is a generic worker pool. It takes a job slice, fans out
goroutines, joins on a `sync.WaitGroup`, and returns the first non-nil error
after every worker has finished. There is no ctx, no cancel channel, no
"early-exit" hook. If one worker fails, the other workers continue draining
the channel until empty. If the user hits `Ctrl-C`, all N workers are killed
abruptly along with the rest of the process.

For a pool that sits in front of network I/O, this is a meaningful gap.
A failed pin should short-circuit pending pins; today it can't.

### 1e. Cobra wiring

`NewRootCmd` registers `RunE` handlers that close over a `*pinFactory`.
None of them call `cmd.Context()`. They could, harmlessly, today — `cobra`
populates `cmd.Context()` from `Background()` if you don't call
`ExecuteContext`. But because no downstream method takes a ctx, the wire
has no terminus.

### 1f. Summary

The codebase is consistent with itself: every layer assumes synchronous,
uncancellable, deadline-free network I/O backed by an implicit
`Background()`. There is nothing to "fix incrementally" — there is no
existing ctx plumbing to extend. It's a green-field decision.

---

## 2. Idiom check: what mainstream Go does in 2026

### 2a. Google Go Style Guide

Unambiguous on the rules: ctx is the first parameter, conventionally named
`ctx`, on any function that does I/O, cancellation, deadlines, or tracing.
Don't store ctx in a struct. Don't replace a received ctx with `Background()`
or `TODO()`. Propagate, don't terminate.

### 2b. `golangci-lint contextcheck`

A widely-deployed linter that flags exactly this codebase's pattern: a
function that calls into something ctx-aware (HTTP client, DB driver, etc.)
without itself accepting a ctx — typically by silently leaning on
`context.Background()` somewhere downstream. If we shipped this repo to a
shop with `contextcheck` enabled (i.e. most non-trivial Go shops), every
resolver method would light up.

### 2c. `cli/cli` precedent

This is the most relevant comparable, because we ship as a `gh` extension and
explicitly model our factory on `cli/cli`'s `cmdutil.Factory`.

What `cli/cli` does (verified in `cli/cli@trunk` `internal/ghcmd/cmd.go`):

- `Main()` constructs a `ctx := context.Background()` and threads it through
  `rootCmd.ExecuteContextC(ctx)`.
- `RunE` handlers call `cmd.Context()` and pass it down: e.g.
  `app.List(cmd.Context(), opts, exporter)` (see
  `pkg/cmd/codespace/list.go`, `pkg/cmd/repo/setdefault/setdefault.go`, etc.).
  16+ matches across the tree for the `RunE { return app.X(cmd.Context(), ...) }`
  pattern.
- The codespace subcommands in particular thread ctx everywhere because they
  spend minutes on a single command and need real cancellation semantics.

What `cli/cli` notably does **not** do:

- Use `signal.NotifyContext` to bind `SIGINT` to ctx cancellation. The root
  ctx is plain `context.Background()`. Cobra's `IsUserCancellation` check
  in `Main()` handles user cancel via `terminal.InterruptErr` (a survey
  prompt error), not via ctx cancel.

So the cli/cli convention is: **"thread the wires; let the runtime kill the
process on signal."** They have the shape of ctx propagation even though the
SIGINT-to-ctx connection is unsoldered. We have neither.

### 2d. `cli/go-gh` upstream readiness

`cli/go-gh/v2 v2.13.0` (what we depend on) ships:

- `(*RESTClient).DoWithContext`, `RequestWithContext`
- `(*GraphQLClient).DoWithContext`, `QueryWithContext`, `MutateWithContext`

All four are first-class. Adopting them is a search-and-replace at the call
site plus a ctx parameter on the enclosing method. We are **not** blocked on
upstream.

### 2e. Other 2026 reference points

- `kubectl`, `terraform`, `helm`, `crossplane`, every HashiCorp CLI: ctx
  threaded from cobra into every command. Standard.
- The Go 1.22+ vet checks (`lostcancel`) plus `contextcheck` are now table
  stakes in CI for serious Go projects.

---

## 3. Cost of retrofit

Counted against the current tree:

| Package           | Surface to touch                                    | Estimate |
|-------------------|-----------------------------------------------------|----------|
| `cmd/gh-actions-pin` | 3 cobra `RunE` (root, check, upgrade), `Execute()`, `runCheck`, `runUpgrade`, `runProvenance` | ~8 funcs |
| `internal/resolver`  | 15 exported `Resolver` methods + 22 unexported helpers + 18 HTTP call sites + 1 transport (retry) | ~37 funcs |
| `internal/doctor`    | 46 exported funcs (not all touch I/O — Diagnose, Apply, tags I/O) | ~15–20 I/O-touching funcs |
| `internal/pinpool`   | 1 generic `Run` signature; ctx threads into the per-job func | 1 sig change, big blast radius in callers |
| `internal/lockfile`, `internal/ui`, `internal/cachekey`, `internal/runlog` | pure local logic / formatting | 0 (skip) |
| **Tests**            | `httpmock`, table-driven resolver tests, doctor tests | ~30+ test files updated mechanically |

Rough total: **~80 production functions, ~30 test files**, almost entirely
mechanical: add `ctx context.Context` as the first parameter, pass it down,
call `*WithContext` at HTTP boundaries. The non-mechanical bits:

- `pinpool.Run` signature: the `run func(slot int, j T) error` callback
  becomes `run func(ctx context.Context, slot int, j T) error`. Every
  caller's closure must accept the new arg. This is the largest blast radius.
- Caches keyed in maps with internal locks: ctx threads through `setX/getX`
  but doesn't change cache semantics. (No "cancel cache lookup" — caches stay
  ctx-free; only the populating I/O takes ctx.)
- The retry transport: `roundTrip` already takes `*http.Request`, which
  carries ctx. No change needed once the request is built with ctx.

A rough back-of-envelope: one focused pass, maybe a day to write, half a day
to clean up tests. Bigger if we add `signal.NotifyContext` and per-request
deadlines (recommended; see §4).

---

## 4. Recommendation

**Do it now.**

Three reasons, in priority order:

1. **The cost is monotonically increasing.** Today the surface is ~80
   functions. The doctor package is the fastest-growing one
   (`remediate_strategies.go` is already 736 lines). Each new method shipped
   without ctx adds another retrofit point. Doing this in a year is 2–3×
   the work and touches code that is by then load-bearing.

2. **`Ctrl-C` is currently a UX bug, not just a style issue.** A
   long-running `gh actions-pin` scan can spend tens of seconds in HTTP. The
   user has no way to interrupt cleanly. The narration log is left in a
   half-written state. CI runs that hit a stuck network can't even use a
   wrapper timeout to get a clean exit. This is a real defect that the ctx
   retrofit fixes as a side effect.

3. **The 2026 idiom check fails.** Any reviewer or contributor stepping into
   this repo from a typical Go shop will reach for `cmd.Context()`, find it
   un-threaded, and either (a) write code that drops the ctx (silent
   regression), or (b) raise a PR rewriting half the package. We pay the
   review cost either way; better to pay it once on our schedule.

Embarrassment is the wrong frame, but if we have to pick: **mildly
embarrassing today, blatantly embarrassing in twelve months.** This is a
"`time.Now()` is the cheapest moment" situation.

### 4a. Sketch migration plan (do-now path)

Order matters. Each step compiles and ships independently.

1. **Wire the root ctx.**
   - In `Execute()`, build `ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM); defer cancel()`.
   - Switch to `NewRootCmd(f).ExecuteContext(ctx)`.
   - At this point nothing downstream uses ctx, but the wire reaches `cmd.Context()` in every `RunE`.

2. **Thread ctx through the cobra `RunE` boundary.**
   - Update `runCheck`, `runUpgrade`, `runProvenance` (and any other top-level
     command funcs) to take a `ctx context.Context` first arg and accept it
     from `cmd.Context()`.

3. **Push ctx into `*resolver.Resolver`.** Every HTTP-touching method gets
   `ctx context.Context` as its first param. Call sites switch to the
   `*WithContext` variants on the go-gh clients. Caches stay unchanged. Do
   the exported methods first (one PR per logical group: discovery, peel,
   ancestry, reachability), then sweep the unexported helpers.

4. **Ctx-aware `pinpool.Run`.** Change the run-callback signature to take
   ctx. Inside `Run`, watch `ctx.Done()` to short-circuit the dispatch loop
   on cancel and return `ctx.Err()` (alongside the first job error). All
   resolver call sites that pass closures already have a ctx in scope from
   step 3.

5. **Doctor package sweep.** `Diagnose`, `Apply`, `Remediate*`, `tags.go`'s
   I/O methods. Mostly mechanical at this point.

6. **Per-request deadlines (optional polish).** Once ctx is wired, add a
   default request timeout (e.g. `context.WithTimeout(ctx, 30*time.Second)`)
   inside the HTTP transport or at the resolver's outermost methods. This
   addresses the "stuck on a slow network" scenario without forcing the user
   to hit `Ctrl-C`.

7. **CI gate.** Add `contextcheck` to whatever linter config we use (or to
   golangci-lint if/when adopted). Catches future regressions for free.

### 4b. Defer-and-eat-the-shame path (rejected, but documented)

If we did defer:

- **Don't half-do it.** A codebase with ctx in some methods and not others
  is worse than uniformly ctx-free, because reviewers can't tell whether the
  absence is principled or accidental.
- **Set a tripwire.** "We will retrofit before X" — e.g. before we vendor
  into another project, before public preview, before adding the next
  network-touching feature. Without a tripwire, "later" becomes "never."
- **Document the gap.** A `KNOWN-LIMITATIONS.md` entry and a banner in
  `AGENTS.md`/`CONTRIBUTING` so contributors don't write ctx-aware methods
  on top of a ctx-free base and create the worst of both worlds.

This path is rejected. The cost-now is finite and the cost-later is
unbounded; this is the textbook case for paying the bill on the way in.

---

## 5. Open questions

1. **Should we bind `SIGINT` to ctx cancel (we go further than `cli/cli`),
   or match `cli/cli`'s "ctx wires only" convention?** Recommendation: bind
   it. We are smaller, we are I/O-bound (cli/cli is mostly request/response
   with short tail), and we have no `survey` interrupt path to lean on.
2. **Default request deadline?** `cli/go-gh` has no default. 30s feels
   right for a single API call; 5m for a whole `gh actions-pin` invocation.
   Both are configurable by env var (`GH_ACTIONS_PIN_TIMEOUT`?).
3. **Does the JSON output contract care about cancellation?** Today
   `errSilent` is the only "blocking findings" sentinel. If we add
   `ctx.Err()` returning a real error, does it map to exit 2 (tool failure)
   or a new exit code (e.g. 130 for SIGINT)? Probably 130, conventionally.
4. **Tests.** `httpmock` doesn't currently care about ctx. Do we add
   ctx-cancel-aware fakes, or treat ctx as opaque in tests for now? Defer:
   ctx-opaque is fine as long as production calls thread it correctly.
5. **Cache invalidation on cancel.** When a request is cancelled mid-flight,
   should partial cache writes be rolled back? Probably not — `setX` is
   called only on success, and the resolver caches are write-once per key.
   But worth a re-read of `cache.go` during the resolver sweep.

---

## Sources

- `cli/cli` `internal/ghcmd/cmd.go` — `ExecuteContextC` + `Background()` root.
- `cli/cli` `pkg/cmd/codespace/*.go` — pervasive `cmd.Context()` threading.
- `cli/go-gh/v2@v2.13.0` `pkg/api/{rest_client,graphql_client}.go` —
  `DoWithContext`, `QueryWithContext`, `MutateWithContext`,
  `RequestWithContext`.
- Google Go Style Guide, "Contexts" decision page.
- `golangci-lint` `contextcheck` docs.
- `cmd/gh-actions-pin/main.go`, `cmd/gh-actions-pin/root.go`,
  `internal/resolver/httpclient.go`, `internal/pinpool/pool.go` —
  current state of this repo at branch `nodeselector/verdict-ctx-architecture`.
