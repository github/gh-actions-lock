// Package resolver resolves action refs to commit SHAs and recursively
// discovers transitive dependencies from composite actions.
package resolver

import (
	"fmt"
	"sync"
	"time"

	"github.com/cli/go-gh/v2/pkg/api"
	"github.com/github/gh-actions-pin/internal/cachekey"
	"github.com/github/gh-actions-pin/internal/lockfile"
)

// ReachabilityStatus represents the result of a commit reachability check.
type ReachabilityStatus string

const (
	// Reachable means the SHA is confirmed on the ref's lineage.
	Reachable ReachabilityStatus = "reachable"
	// Unreachable means the SHA is confirmed NOT on the ref's lineage
	// (e.g. it exists only in a fork network).
	Unreachable ReachabilityStatus = "unreachable"
	// ReachabilityUnknown means the check could not be completed
	// (timeout, rate limit, API error).
	ReachabilityUnknown ReachabilityStatus = "unknown"
)

// DefaultMaxRecursionDepth matches the runner's composite action recursion limit.
const DefaultMaxRecursionDepth = 10

// MaxBatchSize is the maximum number of action refs per GraphQL query.
const MaxBatchSize = 20

// reachabilityConcurrency bounds how many per-dependency reachability checks
// run in parallel in CheckReachabilityAll. Each check makes several REST calls
// (list-branches + per-branch Compare), so a small fan-out meaningfully cuts
// wall-clock time on workflows with many distinct actions while staying well
// under GitHub's secondary (abuse) rate limits. The retry transport still
// absorbs any 429/5xx with Retry-After-aware backoff.
const reachabilityConcurrency = 8

type resolvedEntry struct {
	dep       lockfile.Dependency
	actionYML string
}

// ReachabilityResult holds the outcome of a single reachability check.
type ReachabilityResult struct {
	Owner  string
	Repo   string
	Ref    string
	SHA    string
	DepKey string // full dependency key (e.g. "actions/cache/save@v4")
	Status ReachabilityStatus
	Detail string // human-readable detail (e.g. compare status or error)
	// FullScanUsed is true when the commit was not found in the canonical
	// "likely" branch set (default, protected, release/v*, literal ref,
	// lockfile hint) and the check had to fall back to scanning every branch
	// in the repo. Even when the commit is ultimately Reachable, a full-scan
	// fallback means it is not on a canonical branch — a notable signal worth
	// surfacing to the user.
	FullScanUsed bool
}

// Resolver resolves action refs to commit SHAs.
type Resolver struct {
	client            *api.GraphQLClient
	restClient        *api.RESTClient
	hostname          string
	MaxRecursionDepth int
	// Each cache below is a self-contained syncMap: one mutex paired with
	// one map. Concurrent access happens during the parallel resolve and
	// reachability fan-outs (CheckReachabilityAll, resolveWithActionYMLParallel),
	// but the locks are only held around in-memory map ops, never across
	// HTTP I/O — they don't serialize the network calls the fan-out is
	// meant to overlap. There is no cross-cache atomicity requirement: every
	// callsite touches exactly one cache, so no shared mutex is needed.
	cache              syncMap[cachekey.ActionRef, resolvedEntry]
	latestRefCache     syncMap[cachekey.Repo, string]
	reachCache         syncMap[cachekey.Reach, reachCacheEntry]
	branchListCache    syncMap[cachekey.Repo, []branchHead]
	tagListCache       syncMap[cachekey.Repo, []tagEntry]
	repoIDsCache       syncMap[cachekey.Repo, [2]int64]
	defaultBranchCache syncMap[cachekey.Repo, string] // "" caches a failed lookup
	// compareCache memoizes branchContainsCommit verdicts. Compare API
	// responses are deterministic for an immutable (commit, branch-head)
	// pair, so within a single CLI invocation we never repeat the call.
	compareCache syncMap[cachekey.Compare, bool]
	// branchHintBySHA records the branch we believe contains a given
	// commit. Populated by SeedBranchHints from the existing lockfile so
	// reruns can short-circuit the full branch scan when the recorded
	// branch still contains the commit.
	branchHintBySHA syncMap[cachekey.NWOSha, string]
	// namedBranchCache memoizes single-branch HEAD lookups (getBranchHead).
	// A zero-value branchHead (empty Name) records a known-missing branch
	// so 404s are not re-fetched. These lookups use the git/ref endpoint
	// and so bypass the paginated branch-listing page cap.
	namedBranchCache syncMap[cachekey.NWOName, branchHead]
	// protectedBranchCache memoizes the protected-branch list per repo
	// (GET /branches?protected=true) — part of the canonical "likely" set
	// validated before any full branch scan.
	protectedBranchCache syncMap[cachekey.Repo, []branchHead]
	// releaseBranchCache memoizes release/v* branches per repo (git
	// matching-refs for heads/v and heads/release), the canonical
	// publication branches for actions.
	releaseBranchCache syncMap[cachekey.Repo, []branchHead]
	// tagObjectCache memoizes PeelTagObject results. An annotated /
	// immutable-release tag is stored in Git as a tag *object* whose own
	// SHA differs from the commit it points at; this cache records whether
	// a given SHA is such a tag object and, if so, the commit it peels to.
	tagObjectCache syncMap[cachekey.NWOSha, tagPeel]

	// checkReachFn overrides the default branch-discovery check (for tests).
	checkReachFn func(owner, repo, sha, ref string) (ReachabilityStatus, string)
	// nowFn and sleepFn back CheckAncestry's rate-limit retry loop so
	// tests can drive deterministic X-RateLimit-Reset waits without
	// actually sleeping. Both default to the stdlib equivalents in
	// NewWithOptions.
	nowFn   func() time.Time
	sleepFn func(time.Duration)

	// progressMu serializes ProgressFn invocations so the single-writer spinner
	// UI never sees concurrent updates from parallel reachability workers.
	progressMu sync.Mutex

	// ProgressFn, if set, is invoked with a short human-readable status
	// string when a slow operation begins (per-dependency discovery,
	// per-branch Compare scan). Used by the CLI to update an active
	// spinner so users get feedback during long scans. Safe to leave nil.
	// Suppressed when WorkerProgressFn is set (workers own the display then).
	ProgressFn func(detail string)

	// OnResolveProgress, if set, is invoked with rolling done/total counts
	// during ResolveAllRecursive. Total grows across BFS depth as transitive
	// refs are discovered, so the caller can render a single non-jumping bar
	// covering all resolution work (direct + transitive). Safe to leave nil.
	// Calls are serialized so the spinner UI sees one update at a time.
	OnResolveProgress func(done, total int)

	// OnVerifyProgress, if set, is invoked with rolling done/total counts
	// during CheckReachabilityAll. Distinct from OnResolveProgress so the
	// caller can label this phase clearly (e.g. "Verifying reachability").
	OnVerifyProgress func(done, total int)

	// WorkerProgressFn, if set, is invoked by parallel resolver workers with
	// their slot index and a status string. An empty status means the slot is
	// now idle. When set, ProgressFn calls are suppressed during fan-out so
	// per-worker rows + counter callbacks own the display.
	WorkerProgressFn func(slot int, status string)
}

// progress fires ProgressFn with a formatted message when set. Safe to call
// from multiple goroutines: invocations are serialized so the single-writer
// spinner UI never sees concurrent updates.
func (r *Resolver) progress(format string, args ...any) {
	if r.ProgressFn == nil {
		return
	}
	r.progressMu.Lock()
	defer r.progressMu.Unlock()
	r.ProgressFn(fmt.Sprintf(format, args...))
}

// fireResolveProgress fires OnResolveProgress with the given counts. Safe to
// call from multiple goroutines: invocations are serialized so the spinner UI
// never sees concurrent updates.
func (r *Resolver) fireResolveProgress(done, total int) {
	if r.OnResolveProgress == nil {
		return
	}
	r.progressMu.Lock()
	defer r.progressMu.Unlock()
	r.OnResolveProgress(done, total)
}

// fireVerifyProgress fires OnVerifyProgress with the given counts.
func (r *Resolver) fireVerifyProgress(done, total int) {
	if r.OnVerifyProgress == nil {
		return
	}
	r.progressMu.Lock()
	defer r.progressMu.Unlock()
	r.OnVerifyProgress(done, total)
}

// SeedBranchHints records a branch-of-record for each (owner, repo, sha) in
// deps so subsequent containing-branch scans try that branch first via the
// Compare API. Hints come from a previously-written lockfile and are purely
// advisory: a missed hint just falls through to a full branch scan. Empty
// Branch values are ignored. Safe to call multiple times; later calls
// overwrite earlier hints for the same key.
func (r *Resolver) SeedBranchHints(deps []lockfile.Dependency) {
	for _, d := range deps {
		if d.Branch == "" || d.SHA == "" {
			continue
		}
		owner, repo := d.OwnerRepo()
		if owner == "" || repo == "" {
			continue
		}
		r.branchHintBySHA.put(cachekey.ForNWOSha(owner, repo, d.SHA), d.Branch)
	}
}

// ParentMap is a child dep key → parent dep keys mapping returned alongside
// resolved dependencies by ResolveAllRecursive. It is value-typed so callers
// can hold their own copy across concurrent calls without racing on resolver
// state.
type ParentMap map[string][]string

// RekeyParentMap returns a new ParentMap with both child keys and parent
// values rewritten according to `rewrites` (e.g. tag narrowing v4 → v4.3.1,
// or NormalizeContaining replacing a SHA with a discovered tag). The input
// is not mutated.
func RekeyParentMap(pm ParentMap, rewrites map[string]string) ParentMap {
	if len(pm) == 0 {
		return ParentMap{}
	}
	if len(rewrites) == 0 {
		out := make(ParentMap, len(pm))
		for k, v := range pm {
			out[k] = append([]string(nil), v...)
		}
		return out
	}
	updated := make(ParentMap, len(pm))
	for childKey, parents := range pm {
		newChild := childKey
		if rk, ok := rewrites[childKey]; ok {
			newChild = rk
		}
		newParents := make([]string, len(parents))
		for i, p := range parents {
			if rk, ok := rewrites[p]; ok {
				newParents[i] = rk
			} else {
				newParents[i] = p
			}
		}
		updated[newChild] = newParents
	}
	return updated
}

// Hostname returns the GitHub host the resolver is targeting.
func (r *Resolver) Hostname() string {
	return r.hostname
}

// SetCheckReachabilityFunc overrides the default REST-based reachability check.
// Intended for tests.
func (r *Resolver) SetCheckReachabilityFunc(fn func(owner, repo, sha, ref string) (ReachabilityStatus, string)) {
	r.checkReachFn = fn
}

// HasReachabilityFunc reports whether a test reachability function has been injected.
func (r *Resolver) HasReachabilityFunc() bool {
	return r.checkReachFn != nil
}

func shortSha(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	return s
}
