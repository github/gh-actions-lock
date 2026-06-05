// Package resolver resolves action refs to commit SHAs via the GitHub GraphQL
// API and recursively discovers transitive dependencies from composite actions.
package resolver

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/cli/go-gh/v2/pkg/api"
	"github.com/github/gh-actions-pin/internal/lockfile"
	parserlock "github.com/github/gh-actions-pin/pkg/lockfile"
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
	client             *api.GraphQLClient
	restClient         *api.RESTClient
	hostname           string
	MaxRecursionDepth  int
	cache              map[string]resolvedEntry
	latestRefCache     map[string]string
	reachCache         map[string]reachCacheEntry
	branchListCache    map[string][]branchHead // "owner/repo" → cached branch list
	tagListCache       map[string][]tagEntry   // "owner/repo" → cached tag list
	repoIDsCache       map[string][2]int64     // owner/repo → [ownerID, repoID]
	defaultBranchCache map[string]string       // "owner/repo" → cached default branch name ("" = lookup failed / unknown)
	// compareCache memoizes branchContainsCommit results, keyed by
	// "owner/repo|sha|branchHeadSHA". Compare API responses are deterministic
	// for an immutable (commit, branch-head) pair, so within a single CLI
	// invocation we never need to repeat the call.
	compareCache map[string]bool
	// branchHintBySHA records the branch we believe contains a given commit,
	// keyed by "owner/repo|sha" (sha lowercased). Populated by SeedBranchHints
	// from the existing lockfile so reruns can short-circuit the full branch
	// scan when the recorded branch still contains the commit.
	branchHintBySHA map[string]string
	// namedBranchCache memoizes single-branch HEAD lookups (getBranchHead),
	// keyed "owner/repo|name". A zero-value branchHead (empty Name) records a
	// known-missing branch so 404s are not re-fetched. These lookups use the
	// git/ref endpoint and so bypass the paginated branch-listing page cap.
	namedBranchCache map[string]branchHead
	// protectedBranchCache memoizes the protected-branch list per "owner/repo"
	// (GET /branches?protected=true) — part of the canonical "likely" set
	// validated before any full branch scan.
	protectedBranchCache map[string][]branchHead
	// releaseBranchCache memoizes release/v* branches per "owner/repo"
	// (git matching-refs for heads/v and heads/release), the canonical
	// publication branches for actions.
	releaseBranchCache map[string][]branchHead
	// checkReachFn overrides the default branch-discovery check (for tests).
	checkReachFn func(owner, repo, sha, ref string) (ReachabilityStatus, string)
	// tagObjectCache memoizes PeelTagObject results, keyed by
	// "owner/repo|sha" (sha lowercased). An annotated/immutable-release tag
	// is stored in Git as a tag *object* whose own SHA differs from the
	// commit it points at; this cache records whether a given SHA is such a
	// tag object and, if so, the commit it peels to.
	tagObjectCache map[string]tagPeel

	// cacheMu guards the cache maps that may be read and written concurrently
	// during the parallel reachability phase (CheckReachabilityAll): reachCache,
	// branchListCache, compareCache, defaultBranchCache and branchHintBySHA. The
	// lock is only ever held around in-memory map access, never across HTTP I/O,
	// so it does not serialize the network calls the fan-out is meant to overlap.
	cacheMu sync.Mutex
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
		r.cacheMu.Lock()
		r.branchHintBySHA[hintKey(owner, repo, d.SHA)] = d.Branch
		r.cacheMu.Unlock()
	}
}

func hintKey(owner, repo, sha string) string {
	return owner + "/" + repo + "|" + strings.ToLower(sha)
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

// New creates a resolver using the authenticated gh context.
func New(hostname string) (*Resolver, error) {
	return NewWithOptions(api.ClientOptions{Host: hostname})
}

// NewWithOptions creates a resolver using the provided client options.
func NewWithOptions(opts api.ClientOptions) (*Resolver, error) {
	hostname := opts.Host
	if hostname == "" {
		hostname = "github.com"
	}
	opts.Host = hostname

	// Wrap the transport with retry logic for transient 5xx/429 errors.
	if opts.Transport == nil {
		opts.Transport = newRetryTransport(http.DefaultTransport, 3)
	}

	client, err := api.NewGraphQLClient(opts)
	if err != nil {
		return nil, err
	}

	restClient, err := api.NewRESTClient(opts)
	if err != nil {
		return nil, err
	}

	return &Resolver{
		client:               client,
		restClient:           restClient,
		hostname:             hostname,
		MaxRecursionDepth:    DefaultMaxRecursionDepth,
		cache:                make(map[string]resolvedEntry),
		latestRefCache:       make(map[string]string),
		reachCache:           make(map[string]reachCacheEntry),
		branchListCache:      make(map[string][]branchHead),
		tagListCache:         make(map[string][]tagEntry),
		repoIDsCache:         make(map[string][2]int64),
		defaultBranchCache:   make(map[string]string),
		compareCache:         make(map[string]bool),
		branchHintBySHA:      make(map[string]string),
		namedBranchCache:     make(map[string]branchHead),
		protectedBranchCache: make(map[string][]branchHead),
		releaseBranchCache:   make(map[string][]branchHead),
		tagObjectCache:       make(map[string]tagPeel),
	}, nil
}

// tagPeel records the outcome of a PeelTagObject lookup so repeated checks
// for the same SHA avoid additional API calls.
type tagPeel struct {
	commit string // commit the tag object peels to (empty when isTag is false)
	isTag  bool   // true when the SHA is an annotated tag object
}

// PeelTagObject reports whether sha is an annotated tag object in owner/repo
// and, if so, the commit SHA it dereferences to. Annotated and immutable
// release tags are stored as tag *objects* whose own SHA differs from the
// commit they point at, so a `uses:` pin to that tag-object SHA is a
// legitimate immutable pin even though it does not equal the commit SHA the
// ref resolves to. Follows tag-of-tag chains up to a small depth cap.
// Returns ok=false for lightweight tags, plain commits, cycles, or any
// lookup failure (fail open).
func (r *Resolver) PeelTagObject(owner, repo, sha string) (commit string, ok bool) {
	return r.peelTagObject(owner, repo, sha, 0)
}

// IsKnownTagObject reports whether (owner, repo, sha) is already cached as
// an annotated tag object. Cache-only — never issues a network call. Use
// from display paths (e.g. URL builders) that must not block on I/O. A
// false return means either "definitely not a tag object" or "we have not
// peeled this SHA yet" — callers must treat the negative as ambiguous.
func (r *Resolver) IsKnownTagObject(owner, repo, sha string) bool {
	key := hintKey(owner, repo, sha)
	r.cacheMu.Lock()
	cached, hit := r.tagObjectCache[key]
	r.cacheMu.Unlock()
	return hit && cached.isTag
}

const maxTagPeelDepth = 5

func (r *Resolver) peelTagObject(owner, repo, sha string, depth int) (string, bool) {
	if depth >= maxTagPeelDepth {
		return "", false
	}
	key := hintKey(owner, repo, sha)
	r.cacheMu.Lock()
	cached, hit := r.tagObjectCache[key]
	r.cacheMu.Unlock()
	if hit {
		return cached.commit, cached.isTag
	}
	var resp struct {
		Object struct {
			Type string `json:"type"`
			SHA  string `json:"sha"`
		} `json:"object"`
	}
	path := fmt.Sprintf("repos/%s/%s/git/tags/%s",
		url.PathEscape(owner), url.PathEscape(repo), url.PathEscape(sha))
	if err := r.restClient.Get(path, &resp); err != nil {
		// 404 means "not a tag object"; any other error is transient. Either
		// way we fail open (treat as not-a-tag) and do not cache transient
		// failures so a later retry can succeed.
		return "", false
	}
	switch {
	case resp.Object.Type == "commit" && resp.Object.SHA != "":
		r.cacheMu.Lock()
		r.tagObjectCache[key] = tagPeel{commit: resp.Object.SHA, isTag: true}
		r.cacheMu.Unlock()
		return resp.Object.SHA, true
	case resp.Object.Type == "tag" && resp.Object.SHA != "":
		// Tag-of-tag chain — recurse to find the underlying commit.
		commit, ok := r.peelTagObject(owner, repo, resp.Object.SHA, depth+1)
		if ok {
			r.cacheMu.Lock()
			r.tagObjectCache[key] = tagPeel{commit: commit, isTag: true}
			r.cacheMu.Unlock()
		}
		return commit, ok
	default:
		r.cacheMu.Lock()
		r.tagObjectCache[key] = tagPeel{}
		r.cacheMu.Unlock()
		return "", false
	}
}

// RepoIDs returns the numeric owner ID and repo ID for a NWO, querying
// the GitHub REST API on cache miss. Results are cached for the lifetime of
// the resolver.
func (r *Resolver) RepoIDs(owner, repo string) (int64, int64, error) {
	key := owner + "/" + repo
	r.cacheMu.Lock()
	ids, ok := r.repoIDsCache[key]
	r.cacheMu.Unlock()
	if ok {
		return ids[0], ids[1], nil
	}
	var resp struct {
		ID    int64 `json:"id"`
		Owner struct {
			ID int64 `json:"id"`
		} `json:"owner"`
	}
	path := fmt.Sprintf("repos/%s/%s", url.PathEscape(owner), url.PathEscape(repo))
	if err := r.restClient.Get(path, &resp); err != nil {
		return 0, 0, fmt.Errorf("fetching %s: %w", path, err)
	}
	if resp.ID == 0 || resp.Owner.ID == 0 {
		return 0, 0, fmt.Errorf("%s returned zero IDs (owner=%d repo=%d)", path, resp.Owner.ID, resp.ID)
	}
	r.cacheMu.Lock()
	r.repoIDsCache[key] = [2]int64{resp.Owner.ID, resp.ID}
	r.cacheMu.Unlock()
	return resp.Owner.ID, resp.ID, nil
}

// NewWithTransport creates a resolver with a custom HTTP transport and a
// placeholder auth token. Intended for tests that stub HTTP responses.
func NewWithTransport(hostname string, transport http.RoundTripper) (*Resolver, error) {
	return NewWithOptions(api.ClientOptions{
		AuthToken:    "test-placeholder-token",
		Host:         hostname,
		Transport:    transport,
		LogIgnoreEnv: true,
	})
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

// CheckReachability verifies that the pinned SHA is reachable from at least
// one branch of owner/repo, using the documented REST APIs (list-branches +
// compare for ancestry). This catches fork-network injection where a SHA
// exists in GitHub's shared object store but is not part of the canonical
// repository's history.
//
// See: https://docs.zizmor.sh/audits/#impostor-commit
func (r *Resolver) CheckReachability(owner, repo, sha, ref string) ReachabilityResult {
	result := ReachabilityResult{
		Owner: owner,
		Repo:  repo,
		Ref:   ref,
		SHA:   sha,
	}

	cacheKey := owner + "/" + repo + "/" + sha + "/" + ref
	r.cacheMu.Lock()
	entry, ok := r.reachCache[cacheKey]
	r.cacheMu.Unlock()
	if ok {
		result.Status = entry.status
		result.Detail = entry.detail
		return result
	}

	// Allow tests to inject a fake implementation.
	if r.checkReachFn != nil {
		result.Status, result.Detail = r.checkReachFn(owner, repo, sha, ref)
		r.setReachCache(cacheKey, result.Status, result.Detail)
		return result
	}

	defaultBranch := r.getDefaultBranch(owner, repo)

	// Phase 1: validate the likely/canonical branch set first (literal ref,
	// recorded hint branch, default branch, protected branches, release/v*
	// branches). Each is fetched directly, so a relevant branch is checked
	// even in repos whose branch list exceeds the paginated cap (e.g. a
	// monorepo whose default branch sorts beyond the first few hundred).
	likely := r.likelyBranches(owner, repo, sha, ref, defaultBranch)
	foundBranch, likelyChecked := r.reachabilityScan(owner, repo, sha, ref, likely, defaultBranch)

	// Phase 2: full breadth scan only on a phase-1 miss. An inability to
	// complete the full scan (list-branches failure) yields Unknown rather
	// than a false impostor verdict, exactly as before.
	var branches []branchHead
	var protectedAnyChecked, allAnyChecked bool
	if foundBranch == "" {
		result.FullScanUsed = true
		r.progress("%s/%s: not on a canonical branch — scanning all branches", owner, repo)
		var err error
		branches, err = r.listBranches(owner, repo)
		if err != nil {
			result.Status = ReachabilityUnknown
			result.Detail = fmt.Sprintf("could not list branches for %s/%s: %s", owner, repo, err)
			r.setReachCache(cacheKey, result.Status, result.Detail)
			return result
		}
		protectedBranches := make([]branchHead, 0, len(branches))
		for _, b := range branches {
			if b.Protected {
				protectedBranches = append(protectedBranches, b)
			}
		}
		// Walk protected first, then fall back to all branches. Track whether
		// any Compare lookup succeeded so we can distinguish `Unreachable`
		// (definitively not on any branch) from `Unknown` (every Compare
		// errored, e.g. under rate-limit) — the latter must NOT be reported
		// as an impostor verdict.
		foundBranch, protectedAnyChecked = r.reachabilityScan(owner, repo, sha, ref, protectedBranches, defaultBranch)
		if foundBranch == "" {
			foundBranch, allAnyChecked = r.reachabilityScan(owner, repo, sha, ref, branches, defaultBranch)
		}
	}

	anyChecked := likelyChecked || protectedAnyChecked || allAnyChecked
	hadBranches := len(likely) > 0 || len(branches) > 0

	if foundBranch != "" {
		result.Status = Reachable
		if parserlock.IsFullSha(ref) {
			result.Detail = fmt.Sprintf("pinned to a bare SHA; commit is on branch %s but origin cannot be verified at job runtime — prefer pinning to a tag", foundBranch)
		} else {
			result.Detail = fmt.Sprintf("commit is on branch %s", foundBranch)
		}
	} else if !anyChecked && hadBranches {
		// Every Compare lookup errored — don't claim impostor.
		result.Status = ReachabilityUnknown
		result.Detail = fmt.Sprintf("could not verify commit reachability for %s/%s — every Compare lookup failed (rate limit or transient error); try again later", owner, repo)
	} else {
		result.Status = Unreachable
		if parserlock.IsFullSha(ref) {
			result.Detail = "pinned to a bare SHA; commit is NOT on any branch — possible fork-network commit"
		} else {
			result.Detail = fmt.Sprintf("commit %s not found on any branch of %s/%s — possible fork-network injection",
				shortSha(sha), owner, repo)
		}
	}

	r.setReachCache(cacheKey, result.Status, result.Detail)
	return result
}

// setReachCache stores a reachability verdict under cacheMu.
func (r *Resolver) setReachCache(key string, status ReachabilityStatus, detail string) {
	r.cacheMu.Lock()
	r.reachCache[key] = reachCacheEntry{status: status, detail: detail}
	r.cacheMu.Unlock()
}

// reachCacheEntry stores both the verdict and the human-readable detail so
// re-reads (e.g. across pre-warm + per-workflow phases) carry the original
// rationale instead of a generic "cached" placeholder.
type reachCacheEntry struct {
	status ReachabilityStatus
	detail string
}

// reachabilityScan walks candidates (in orderedBranches tier order),
// returning the first branch whose HEAD is sha or whose lineage contains
// sha as an ancestor via the Compare API. Returns the matched branch name
// and a boolean indicating whether at least one Compare lookup succeeded
// (regardless of result). When every Compare call errored (e.g. fully
// rate-limited / 5xx storm) we cannot conclude `Unreachable`; the caller
// should surface that as `ReachabilityUnknown` instead of an impostor
// verdict.
//
// The slow path (Compare API per branch) runs with bounded concurrency:
// a SHA that lives only on a non-canonical branch can require walking
// many heads, and a strictly sequential walk multiplies the wall-clock
// cost by branch count. We still honor tier preference for the *reported*
// branch — the earliest-tier match wins — but all candidates are dispatched
// concurrently so the longest single Compare bounds the wall time rather
// than their sum. The retry transport absorbs any 429/secondary-limit.
func (r *Resolver) reachabilityScan(owner, repo, sha, ref string, candidates []branchHead, defaultBranch string) (matched string, anyChecked bool) {
	if len(candidates) == 0 {
		return "", false
	}
	// Fast path: exact HEAD match.
	for _, b := range candidates {
		if strings.EqualFold(b.SHA, sha) {
			return b.Name, true
		}
	}
	// Slow path: ancestry via Compare API in tier order. The walk runs
	// concurrently — see function comment.
	r.cacheMu.Lock()
	hintBranch := r.branchHintBySHA[hintKey(owner, repo, sha)]
	r.cacheMu.Unlock()
	ordered := orderedBranches(candidates, hintBranch, ref, defaultBranch)
	if len(ordered) == 0 {
		return "", false
	}
	limit := reachabilityConcurrency
	if len(ordered) < limit {
		limit = len(ordered)
	}
	type result struct {
		contains bool
		checked  bool
	}
	results := make([]result, len(ordered))
	sem := make(chan struct{}, limit)
	var wg sync.WaitGroup
	for i, b := range ordered {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int, b branchHead) {
			defer wg.Done()
			defer func() { <-sem }()
			r.progress("scanning %s/%s branch %s", owner, repo, b.Name)
			ok, err := r.branchContainsCommit(owner, repo, sha, b.SHA)
			if err != nil {
				return
			}
			results[i] = result{contains: ok, checked: true}
		}(i, b)
	}
	wg.Wait()
	// Tier preference: report the earliest-tier branch that contains the
	// SHA. orderedBranches already encodes the tier order, so the first
	// `contains` wins.
	for i, res := range results {
		if res.checked {
			anyChecked = true
			if res.contains {
				return ordered[i].Name, true
			}
		}
	}
	return "", anyChecked
}

// branchHead holds a branch name, the SHA of its HEAD commit, and whether
// the branch has branch-protection rules enabled in the upstream repo.
type branchHead struct {
	Name      string
	SHA       string
	Protected bool
}

// tagEntry holds a tag name and the commit SHA it points at.
type tagEntry struct {
	Name string
	SHA  string
}

// listBranches returns all branches with their HEAD SHAs for a repo, using
// the documented REST API (GET /repos/{owner}/{repo}/branches). Results are
// cached per owner/repo. Paginates up to 3 pages (300 branches) to bound
// the number of API calls.
func (r *Resolver) listBranches(owner, repo string) ([]branchHead, error) {
	key := owner + "/" + repo
	r.cacheMu.Lock()
	cached, ok := r.branchListCache[key]
	r.cacheMu.Unlock()
	if ok {
		return cached, nil
	}
	var all []branchHead
	for page := 1; page <= 3; page++ {
		path := fmt.Sprintf("repos/%s/%s/branches?per_page=100&page=%d",
			url.PathEscape(owner), url.PathEscape(repo), page)
		var resp []struct {
			Name   string `json:"name"`
			Commit struct {
				SHA string `json:"sha"`
			} `json:"commit"`
			Protected bool `json:"protected"`
		}
		if err := r.restClient.Get(path, &resp); err != nil {
			return nil, fmt.Errorf("listing branches for %s/%s: %w", owner, repo, err)
		}
		for _, b := range resp {
			all = append(all, branchHead{Name: b.Name, SHA: b.Commit.SHA, Protected: b.Protected})
		}
		if len(resp) < 100 {
			break // last page
		}
	}
	r.cacheMu.Lock()
	r.branchListCache[key] = all
	r.cacheMu.Unlock()
	return all, nil
}

// listTagsForRepo returns all tags with their commit SHAs for a repo, using
// the documented REST API (GET /repos/{owner}/{repo}/tags). Results are
// cached per owner/repo.
func (r *Resolver) listTagsForRepo(owner, repo string) ([]tagEntry, error) {
	key := owner + "/" + repo
	r.cacheMu.Lock()
	cached, ok := r.tagListCache[key]
	r.cacheMu.Unlock()
	if ok {
		return cached, nil
	}
	path := fmt.Sprintf("repos/%s/%s/tags?per_page=100",
		url.PathEscape(owner), url.PathEscape(repo))
	var resp []struct {
		Name   string `json:"name"`
		Commit struct {
			SHA string `json:"sha"`
		} `json:"commit"`
	}
	if err := r.restClient.Get(path, &resp); err != nil {
		return nil, fmt.Errorf("listing tags for %s/%s: %w", owner, repo, err)
	}
	tags := make([]tagEntry, 0, len(resp))
	for _, t := range resp {
		tags = append(tags, tagEntry{Name: t.Name, SHA: t.Commit.SHA})
	}
	r.cacheMu.Lock()
	r.tagListCache[key] = tags
	r.cacheMu.Unlock()
	return tags, nil
}

// branchContainsCommit reports whether sha is on the lineage of branchHeadSHA
// using the documented Compare API. A 404 or 422 response (unrelated histories
// or missing commit) is treated as a non-error false return. Results are
// memoized in compareCache for the lifetime of the Resolver.
func (r *Resolver) branchContainsCommit(owner, repo, sha, branchHeadSHA string) (bool, error) {
	if strings.EqualFold(sha, branchHeadSHA) {
		return true, nil
	}
	key := owner + "/" + repo + "|" + strings.ToLower(sha) + "|" + strings.ToLower(branchHeadSHA)
	r.cacheMu.Lock()
	v, ok := r.compareCache[key]
	r.cacheMu.Unlock()
	if ok {
		return v, nil
	}
	path := fmt.Sprintf("repos/%s/%s/compare/%s...%s",
		url.PathEscape(owner), url.PathEscape(repo),
		url.PathEscape(sha), url.PathEscape(branchHeadSHA))
	var resp compareResponse
	if err := r.restClient.Get(path, &resp); err != nil {
		var httpErr *api.HTTPError
		if errors.As(err, &httpErr) &&
			(httpErr.StatusCode == http.StatusNotFound || httpErr.StatusCode == http.StatusUnprocessableEntity) {
			r.setCompareCache(key, false)
			return false, nil // unrelated histories or missing commit
		}
		return false, err
	}
	// sha is an ancestor of branchHeadSHA iff the merge base IS sha.
	contains := strings.EqualFold(resp.MergeBaseCommit.SHA, sha)
	r.setCompareCache(key, contains)
	return contains, nil
}

// setCompareCache stores a Compare verdict under cacheMu.
func (r *Resolver) setCompareCache(key string, contains bool) {
	r.cacheMu.Lock()
	r.compareCache[key] = contains
	r.cacheMu.Unlock()
}

// orderedBranches returns branches in tiered order so the most
// trust-bearing candidates are compared first when walking the slow path:
//
//  1. hintBranch (the branch previously recorded in the lockfile for this commit)
//  2. hintRef (the ref the user literally wrote in the workflow)
//  3. defaultBranch (e.g. main / master)
//  4. protected branches (branch-protection rules in upstream), lex within tier
//  5. unprotected branches, lex within tier
//
// Tiers 1–3 are skipped when empty or absent from branches.
func orderedBranches(branches []branchHead, hintBranch, hintRef, defaultBranch string) []branchHead {
	priority := make(map[string]bool)
	var result []branchHead
	for _, name := range []string{hintBranch, hintRef, defaultBranch} {
		if name == "" || priority[name] {
			continue
		}
		for _, b := range branches {
			if b.Name == name {
				result = append(result, b)
				priority[b.Name] = true
				break
			}
		}
	}
	protected := make([]branchHead, 0, len(branches))
	unprotected := make([]branchHead, 0, len(branches))
	for _, b := range branches {
		if priority[b.Name] {
			continue
		}
		if b.Protected {
			protected = append(protected, b)
		} else {
			unprotected = append(unprotected, b)
		}
	}
	sort.Slice(protected, func(i, j int) bool { return protected[i].Name < protected[j].Name })
	sort.Slice(unprotected, func(i, j int) bool { return unprotected[i].Name < unprotected[j].Name })
	result = append(result, protected...)
	result = append(result, unprotected...)
	return result
}

// getDefaultBranch returns the repo's default branch name (e.g. "main"),
// looked up via GET /repos/{owner}/{repo} and cached for the lifetime of
// the resolver. On lookup failure an empty string is cached so subsequent
// callers don't retry.
func (r *Resolver) getDefaultBranch(owner, repo string) string {
	key := owner + "/" + repo
	r.cacheMu.Lock()
	name, ok := r.defaultBranchCache[key]
	r.cacheMu.Unlock()
	if ok {
		return name
	}
	var resp struct {
		DefaultBranch string `json:"default_branch"`
	}
	path := fmt.Sprintf("repos/%s/%s", url.PathEscape(owner), url.PathEscape(repo))
	if err := r.restClient.Get(path, &resp); err != nil {
		r.setDefaultBranchCache(key, "")
		return ""
	}
	r.setDefaultBranchCache(key, resp.DefaultBranch)
	return resp.DefaultBranch
}

// setDefaultBranchCache stores a default-branch lookup under cacheMu.
func (r *Resolver) setDefaultBranchCache(key, name string) {
	r.cacheMu.Lock()
	r.defaultBranchCache[key] = name
	r.cacheMu.Unlock()
}

// escapeBranchPath percent-escapes each slash-delimited segment of a ref name
// while preserving the slashes themselves, so names like "releases/v4" form a
// valid git/ref path.
func escapeBranchPath(name string) string {
	parts := strings.Split(name, "/")
	for i, p := range parts {
		parts[i] = url.PathEscape(p)
	}
	return strings.Join(parts, "/")
}

// getBranchHead resolves a single branch's HEAD commit directly via
// GET /repos/{owner}/{repo}/git/ref/heads/{name}. Unlike listBranches this is
// not subject to the paginated 300-branch cap, so a known branch is validated
// even in repos with thousands of branches. Results (including 404s) are
// cached per (owner/repo, name). Returns ok=false on any error.
func (r *Resolver) getBranchHead(owner, repo, name string) (branchHead, bool) {
	if name == "" {
		return branchHead{}, false
	}
	key := owner + "/" + repo + "|" + name
	r.cacheMu.Lock()
	bh, ok := r.namedBranchCache[key]
	r.cacheMu.Unlock()
	if ok {
		return bh, bh.Name != ""
	}
	path := fmt.Sprintf("repos/%s/%s/git/ref/heads/%s",
		url.PathEscape(owner), url.PathEscape(repo), escapeBranchPath(name))
	var resp struct {
		Ref    string `json:"ref"`
		Object struct {
			SHA  string `json:"sha"`
			Type string `json:"type"`
		} `json:"object"`
	}
	if err := r.restClient.Get(path, &resp); err != nil {
		var httpErr *api.HTTPError
		if errors.As(err, &httpErr) && httpErr.StatusCode == http.StatusNotFound {
			r.setNamedBranch(key, branchHead{}) // negative cache
		}
		return branchHead{}, false
	}
	// A prefix (non-exact) match returns an array, decoding to an empty
	// object here; treat that as "not found" rather than guessing.
	if resp.Object.SHA == "" {
		return branchHead{}, false
	}
	bh = branchHead{Name: name, SHA: resp.Object.SHA}
	r.setNamedBranch(key, bh)
	return bh, true
}

// setNamedBranch stores a single-branch lookup under cacheMu.
func (r *Resolver) setNamedBranch(key string, bh branchHead) {
	r.cacheMu.Lock()
	r.namedBranchCache[key] = bh
	r.cacheMu.Unlock()
}

// listProtectedBranches returns the repo's protected branches via
// GET /repos/{owner}/{repo}/branches?protected=true. Best-effort: any error
// yields whatever was collected so far (possibly empty). Cached per owner/repo.
func (r *Resolver) listProtectedBranches(owner, repo string) []branchHead {
	key := owner + "/" + repo
	r.cacheMu.Lock()
	cached, ok := r.protectedBranchCache[key]
	r.cacheMu.Unlock()
	if ok {
		return cached
	}
	var all []branchHead
	for page := 1; page <= 3; page++ {
		path := fmt.Sprintf("repos/%s/%s/branches?protected=true&per_page=100&page=%d",
			url.PathEscape(owner), url.PathEscape(repo), page)
		var resp []struct {
			Name   string `json:"name"`
			Commit struct {
				SHA string `json:"sha"`
			} `json:"commit"`
		}
		if err := r.restClient.Get(path, &resp); err != nil {
			break // best-effort
		}
		for _, b := range resp {
			all = append(all, branchHead{Name: b.Name, SHA: b.Commit.SHA, Protected: true})
		}
		if len(resp) < 100 {
			break
		}
	}
	r.cacheMu.Lock()
	r.protectedBranchCache[key] = all
	r.cacheMu.Unlock()
	return all
}

// matchingHeadRefs returns branches whose names start with prefix via
// GET /repos/{owner}/{repo}/git/matching-refs/heads/{prefix}. Best-effort:
// any error yields nil.
func (r *Resolver) matchingHeadRefs(owner, repo, prefix string) []branchHead {
	path := fmt.Sprintf("repos/%s/%s/git/matching-refs/heads/%s",
		url.PathEscape(owner), url.PathEscape(repo), escapeBranchPath(prefix))
	var resp []struct {
		Ref    string `json:"ref"`
		Object struct {
			SHA  string `json:"sha"`
			Type string `json:"type"`
		} `json:"object"`
	}
	if err := r.restClient.Get(path, &resp); err != nil {
		return nil // best-effort
	}
	var out []branchHead
	for _, ref := range resp {
		name := strings.TrimPrefix(ref.Ref, "refs/heads/")
		if name == ref.Ref || name == "" || ref.Object.SHA == "" {
			continue
		}
		out = append(out, branchHead{Name: name, SHA: ref.Object.SHA})
	}
	return out
}

// listReleaseBranches returns release/v* branches (the canonical action
// publication branches) by matching heads/v and heads/release. Best-effort;
// cached per owner/repo.
func (r *Resolver) listReleaseBranches(owner, repo string) []branchHead {
	key := owner + "/" + repo
	r.cacheMu.Lock()
	cached, ok := r.releaseBranchCache[key]
	r.cacheMu.Unlock()
	if ok {
		return cached
	}
	seen := make(map[string]bool)
	var all []branchHead
	for _, prefix := range []string{"v", "release"} {
		for _, bh := range r.matchingHeadRefs(owner, repo, prefix) {
			if bh.Name == "" || seen[bh.Name] {
				continue
			}
			seen[bh.Name] = true
			all = append(all, bh)
		}
	}
	r.cacheMu.Lock()
	r.releaseBranchCache[key] = all
	r.cacheMu.Unlock()
	return all
}

// likelyBranches assembles the high-trust candidate set validated before any
// full branch scan: the literal ref (when it is a branch), the recorded
// lockfile hint branch, the default branch, protected branches and
// release/v* branches. Every member is fetched directly (not via the
// paginated listing), so a relevant branch is checked even when it would sort
// beyond the listing page cap. Deduplicated by name; order is most-trusted
// first.
func (r *Resolver) likelyBranches(owner, repo, sha, ref, defaultBranch string) []branchHead {
	seen := make(map[string]bool)
	var out []branchHead
	addNamed := func(name string) {
		if name == "" || seen[name] {
			return
		}
		if bh, ok := r.getBranchHead(owner, repo, name); ok {
			seen[name] = true
			out = append(out, bh)
		}
	}
	if ref != "" && !parserlock.IsFullSha(ref) {
		addNamed(ref)
	}
	r.cacheMu.Lock()
	hint := r.branchHintBySHA[hintKey(owner, repo, sha)]
	r.cacheMu.Unlock()
	addNamed(hint)
	addNamed(defaultBranch)
	for _, bh := range r.listProtectedBranches(owner, repo) {
		if bh.Name == "" || seen[bh.Name] {
			continue
		}
		seen[bh.Name] = true
		out = append(out, bh)
	}
	for _, bh := range r.listReleaseBranches(owner, repo) {
		if bh.Name == "" || seen[bh.Name] {
			continue
		}
		seen[bh.Name] = true
		out = append(out, bh)
	}
	return out
}

// DiscoverContaining returns (tag, branch) for sha in owner/repo, using
// the documented REST APIs: list-branches (with compare for ancestry) and
// list-tags. Results are cached for the lifetime of the resolver.
//
// Selection rules:
//   - If hintRef is one of the discovered tags it wins; otherwise the first
//     tag (lexicographic) is picked. tag may be empty when no tag points at sha.
//   - Protected branches are searched first (hintRef → default → lex).
//     If no protected branch contains sha, the search falls back to all
//     branches in the same tier order.
//   - branch is REQUIRED to be non-empty; an error is returned otherwise
//     (impostor / fork-network signal).
//
// hintRef may be empty (e.g. for bare-SHA pins). The repo's default branch
// is discovered automatically via GET /repos/{owner}/{repo} (cached).
func (r *Resolver) DiscoverContaining(owner, repo, sha, hintRef string) (tag, branch string, err error) {
	return r.DiscoverContainingDefault(owner, repo, sha, hintRef, r.getDefaultBranch(owner, repo))
}

// DiscoverContainingDefault is DiscoverContaining with an explicit hint at
// the repository's default branch (e.g. "main"). When the discovered branch
// set contains defaultBranch it is preferred over lexicographic ordering.
//
// Branch search is two-phase. Phase 1 validates the likely/canonical set
// directly (literal ref, recorded hint branch, default branch, protected
// branches, release/v* branches) so a relevant branch is never missed because
// it sorts beyond the paginated listing cap. Phase 2 — a full protected-first
// then all-branches scan — runs only when phase 1 finds nothing. An impostor
// error is returned only if both phases fail to place the commit.
func (r *Resolver) DiscoverContainingDefault(owner, repo, sha, hintRef, defaultBranch string) (tag, branch string, err error) {
	// Phase 1: canonical "likely" branches, fetched directly.
	likely := r.likelyBranches(owner, repo, sha, hintRef, defaultBranch)
	branch, err = r.findContainingBranch(owner, repo, sha, hintRef, defaultBranch, likely)
	if err != nil {
		return "", "", err
	}

	// Phase 2: full paginated scan, only on a phase-1 miss.
	if branch == "" {
		branches, lerr := r.listBranches(owner, repo)
		if lerr != nil {
			return "", "", lerr
		}
		protectedBranches := make([]branchHead, 0, len(branches))
		for _, b := range branches {
			if b.Protected {
				protectedBranches = append(protectedBranches, b)
			}
		}
		branch, err = r.findContainingBranch(owner, repo, sha, hintRef, defaultBranch, protectedBranches)
		if err != nil {
			return "", "", err
		}
		if branch == "" {
			branch, err = r.findContainingBranch(owner, repo, sha, hintRef, defaultBranch, branches)
			if err != nil {
				return "", "", err
			}
		}
	}

	if branch == "" {
		return "", "", &ImpostorError{NWO: owner + "/" + repo, Ref: hintRef, SHA: sha}
	}

	// Discover tags pointing at sha.
	allTags, err := r.listTagsForRepo(owner, repo)
	if err != nil {
		return "", "", err
	}
	var tagNames []string
	for _, t := range allTags {
		if strings.EqualFold(t.SHA, sha) {
			tagNames = append(tagNames, t.Name)
		}
	}
	tag = pickPreferredTag(tagNames, hintRef)

	return tag, branch, nil
}

// findContainingBranch returns the name of the first branch (from the given
// candidate set, in orderedBranches tier order) whose HEAD is sha or which
// contains sha as an ancestor via the Compare API. Returns "" if none
// match. Returns a non-nil error only for unexpected transport failures
// from the Compare API (404/422 are treated as "not contained").
func (r *Resolver) findContainingBranch(owner, repo, sha, hintRef, defaultBranch string, candidates []branchHead) (string, error) {
	if len(candidates) == 0 {
		return "", nil
	}
	// Fast path: exact HEAD match.
	var exact []string
	for _, b := range candidates {
		if strings.EqualFold(b.SHA, sha) {
			exact = append(exact, b.Name)
		}
	}
	if len(exact) > 0 {
		return pickPreferred(exact, hintRef, defaultBranch), nil
	}
	// Slow path: ancestry via Compare API in tier order.
	r.cacheMu.Lock()
	hintBranch := r.branchHintBySHA[hintKey(owner, repo, sha)]
	r.cacheMu.Unlock()
	for _, b := range orderedBranches(candidates, hintBranch, hintRef, defaultBranch) {
		r.progress("scanning %s/%s branch %s", owner, repo, b.Name)
		ok, err := r.branchContainsCommit(owner, repo, sha, b.SHA)
		if err != nil {
			return "", fmt.Errorf("comparing %s with %s/%s branch %s: %w",
				shortSha(sha), owner, repo, b.Name, err)
		}
		if ok {
			return b.Name, nil
		}
	}
	return "", nil
}

// hintMatch returns hintRef if it is non-empty and present in candidates,
// else "". Shared by the branch and tag pickers to honor author intent first.
func hintMatch(candidates []string, hintRef string) string {
	if hintRef == "" {
		return ""
	}
	for _, c := range candidates {
		if c == hintRef {
			return c
		}
	}
	return ""
}

// pickPreferred returns hintRef if it appears in candidates, else
// defaultPick if non-empty and present, else the lexicographically-first
// candidate, else "".
func pickPreferred(candidates []string, hintRef, defaultPick string) string {
	if len(candidates) == 0 {
		return ""
	}
	if hit := hintMatch(candidates, hintRef); hit != "" {
		return hit
	}
	if defaultPick != "" {
		for _, c := range candidates {
			if c == defaultPick {
				return c
			}
		}
	}
	best := candidates[0]
	for _, c := range candidates[1:] {
		if c < best {
			best = c
		}
	}
	return best
}

// pickPreferredTag selects the canonical tag from the set of tags pointing at
// a SHA. Selection order:
//
//  1. hintRef — if the workflow's original ref is itself one of the tags, it
//     wins (preserve author intent).
//  2. Highest semantic version (e.g. v1.4.4 beats v1). Both v-prefixed
//     (v1.4.4) and bare (1.4.4) forms qualify; a v-prefixed tag wins ties
//     against the same bare version.
//  3. Lexicographically-first of whatever remains.
//
// Preferring a clean version tag means oddly-named monorepo sub-action tags
// (which can contain '@') are only chosen when no version tag points at the
// same commit.
func pickPreferredTag(candidates []string, hintRef string) string {
	if hit := hintMatch(candidates, hintRef); hit != "" {
		return hit
	}
	var best string
	var bestVer lockfile.Version
	haveSemver := false
	for _, c := range candidates {
		sv, ok := lockfile.ParseVersion(c)
		if !ok {
			continue
		}
		if !haveSemver || sv.Greater(bestVer) {
			best, bestVer, haveSemver = c, sv, true
		}
	}
	if haveSemver {
		return best
	}
	return pickPreferred(candidates, hintRef, "")
}

func shortSha(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	return s
}

// NormalizeContaining runs DiscoverContaining for every entry in deps,
// populates dep.Tag and dep.Branch, and computes the canonical @ref.
// When the canonical ref differs from dep.Ref the change is recorded in
// the returned rewrites map (keyed by "owner/repo[/path]@old-ref" →
// "owner/repo[/path]@new-ref") and dep.Ref is updated in place. Callers
// should pass the resulting map to lockfile.WorkflowFile.RewriteActionRefs to
// mutate the workflow uses: lines.
//
// Ref selection: if the original ref was itself a discovered branch (user
// wrote e.g. @main), that branch is preserved as the key ref. Otherwise
// the discovered tag wins (if any), falling back to the containing branch.
//
// Returns an error from the first dep whose discovery fails (e.g. commit
// not on any branch — fail closed). On error rewrites is nil and earlier
// in-place mutations may have already happened; the caller should treat
// the deps slice as tainted.
func (r *Resolver) NormalizeContaining(deps []lockfile.Dependency) (map[string]string, error) {
	rewrites := map[string]string{}
	for i := range deps {
		d := &deps[i]
		owner, repo := d.OwnerRepo()
		if owner == "" || repo == "" {
			continue
		}
		r.progress("resolving %s@%s", d.NWO, d.Ref)
		tag, branch, err := r.DiscoverContaining(owner, repo, d.SHA, d.Ref)
		if err != nil {
			var imp *ImpostorError
			if errors.As(err, &imp) {
				imp.NWO = d.NWO
				imp.Ref = d.Ref
				return nil, imp
			}
			return nil, fmt.Errorf("%s@%s: %w", d.NWO, d.Ref, err)
		}
		d.Tag = tag
		d.Branch = branch
		// Prefer the user's explicit branch ref when the workflow file
		// already names a branch (e.g. @main stays @main even when tags
		// also exist). Otherwise prefer the discovered tag, falling back
		// to the containing branch.
		newRef := tag
		if branch == d.Ref {
			// hintRef matched a branch — user wrote a branch ref.
			newRef = branch
		}
		if newRef == "" {
			newRef = branch
		}
		if newRef == "" || newRef == d.Ref {
			continue
		}
		rewrites[d.NWO+"@"+d.Ref] = d.NWO + "@" + newRef
		d.Ref = newRef
	}
	return rewrites, nil
}

// ImpostorError indicates a commit that is not reachable from any branch — a
// fork-network / impostor signal. It carries the offending action so callers
// can report which workflow is affected without abandoning the whole run.
type ImpostorError struct {
	NWO string // owner/repo
	Ref string // ref as written in the workflow
	SHA string // resolved commit SHA
}

func (e *ImpostorError) Error() string {
	return fmt.Sprintf("%s@%s is not on any branch — fork-network / impostor signal; refusing to pin", e.NWO, shortSha(e.SHA))
}

// AncestryStatus represents whether a pinned SHA is a legitimate ancestor of the live SHA.
type AncestryStatus int

const (
	// AncestryConfirmed means the pinned SHA is an ancestor of the live SHA.
	AncestryConfirmed AncestryStatus = iota
	// AncestryNotAncestor means the pinned SHA is NOT an ancestor — possible forgery.
	AncestryNotAncestor
	// AncestryUnknown means the check could not be completed (rate limit, API error).
	AncestryUnknown
)

// compareResponse is the subset of the GitHub Compare API response we need.
type compareResponse struct {
	Status          string `json:"status"`
	MergeBaseCommit struct {
		SHA string `json:"sha"`
	} `json:"merge_base_commit"`
}

// CheckAncestry uses the Compare API to test whether pinnedSHA is an ancestor
// of liveSHA. This detects lockfile forgery: if someone injects a SHA that was
// never in the ref's lineage, merge_base(pinned, live) ≠ pinned.
func (r *Resolver) CheckAncestry(owner, repo, pinnedSHA, liveSHA string) (AncestryStatus, string) {
	path := fmt.Sprintf("repos/%s/%s/compare/%s...%s",
		url.PathEscape(owner), url.PathEscape(repo),
		url.PathEscape(pinnedSHA), url.PathEscape(liveSHA))

	var resp compareResponse
	err := r.restClient.Get(path, &resp)
	if err != nil {
		var httpErr *api.HTTPError
		if errors.As(err, &httpErr) {
			switch httpErr.StatusCode {
			case http.StatusNotFound:
				return AncestryNotAncestor, "commit not found in repository"
			case http.StatusConflict: // 409 — no common ancestor
				return AncestryNotAncestor, "no common ancestor between pinned and live SHA"
			case http.StatusForbidden, http.StatusTooManyRequests:
				detail := fmt.Sprintf("rate limited (HTTP %d)", httpErr.StatusCode)
				if reset := httpErr.Headers.Get("X-RateLimit-Reset"); reset != "" {
					detail += "; resets at " + reset
				}
				return AncestryUnknown, detail
			default:
				return AncestryUnknown, fmt.Sprintf("API error (HTTP %d): %s", httpErr.StatusCode, httpErr.Message)
			}
		}
		return AncestryUnknown, err.Error()
	}

	if strings.EqualFold(resp.MergeBaseCommit.SHA, pinnedSHA) {
		return AncestryConfirmed, fmt.Sprintf("pinned SHA is ancestor of live SHA (compare: %s)", resp.Status)
	}
	return AncestryNotAncestor, fmt.Sprintf("merge base is %s, not the pinned SHA — possible lockfile forgery or upstream history rewrite", resp.MergeBaseCommit.SHA[:12])
}

// CheckReachabilityAll runs reachability checks on a batch of dependencies,
// deduplicating by owner/repo/sha/ref.
func (r *Resolver) CheckReachabilityAll(deps []lockfile.Dependency) []ReachabilityResult {
	var results []ReachabilityResult
	seen := make(map[string]bool)

	// Pre-filter to the unique deps we'll actually check so progress can be
	// reported as [i/N] against a stable total.
	unique := make([]lockfile.Dependency, 0, len(deps))
	for _, dep := range deps {
		owner, _ := dep.OwnerRepo()
		if owner == "" {
			continue
		}
		key := dep.NWO + "/" + dep.SHA + "/" + dep.Ref
		if seen[key] {
			continue
		}
		seen[key] = true
		unique = append(unique, dep)
	}

	total := len(unique)
	if total == 0 {
		return nil
	}

	// Fire the verify-phase counter immediately so the top label transitions
	// out of "Resolving" the moment we leave that phase — even before warmup
	// completes. Without this the user sees "[49/49] Resolving" frozen for
	// the duration of the (sometimes slow) per-repo metadata warmup.
	r.fireVerifyProgress(0, total)
	if r.WorkerProgressFn != nil {
		// Stale "✓ NWO" rows from the resolve phase carry into verify and
		// confuse the picture; clear them so warmup/verify own a fresh slate.
		for slot := 0; slot < reachabilityConcurrency; slot++ {
			r.WorkerProgressFn(slot, "")
		}
	}

	// Pre-warm the per-repo caches (branch list + default branch) for each
	// distinct repo before fanning out the per-dep Compare calls. Warming once
	// per repo avoids the thundering-herd of identical list-branches calls
	// when N parallel workers race on the same repo. Parallelized across the
	// same slot pool used by the verify fan-out so the user sees movement.
	uniqueRepos := make([]string, 0)
	seenRepo := make(map[string]bool)
	for _, dep := range unique {
		owner, repo := dep.OwnerRepo()
		rk := owner + "/" + repo
		if !seenRepo[rk] {
			seenRepo[rk] = true
			uniqueRepos = append(uniqueRepos, rk)
		}
	}
	if r.checkReachFn == nil && len(uniqueRepos) > 0 {
		warmupLimit := reachabilityConcurrency
		if warmupLimit > len(uniqueRepos) {
			warmupLimit = len(uniqueRepos)
		}
		warmupSlots := make(chan int, warmupLimit)
		for i := 0; i < warmupLimit; i++ {
			warmupSlots <- i
		}
		var warmupWG sync.WaitGroup
		for _, rk := range uniqueRepos {
			warmupWG.Add(1)
			slot := <-warmupSlots
			go func(rk string, slot int) {
				defer warmupWG.Done()
				defer func() { warmupSlots <- slot }()
				parts := strings.SplitN(rk, "/", 2)
				if len(parts) != 2 {
					return
				}
				owner, repo := parts[0], parts[1]
				if r.WorkerProgressFn != nil {
					r.WorkerProgressFn(slot, "→ loading "+rk)
				}
				_, _ = r.listBranches(owner, repo)
				_ = r.getDefaultBranch(owner, repo)
				if r.WorkerProgressFn != nil {
					r.WorkerProgressFn(slot, "")
				}
			}(rk, slot)
		}
		warmupWG.Wait()
	}

	// Fan out the per-dependency checks with a bounded worker pool. Each dep is
	// independent; the cache maps they touch are guarded by cacheMu and progress
	// reporting is serialized, so results[i] is the only unsynchronized write
	// and each goroutine owns a distinct index. Each goroutine also owns a
	// stable slot index (0..limit-1) for the per-worker UI display.
	results = make([]ReachabilityResult, total)
	limit := reachabilityConcurrency
	if limit > total {
		limit = total
	}
	slots := make(chan int, limit)
	for i := 0; i < limit; i++ {
		slots <- i
	}
	var (
		wg        sync.WaitGroup
		completed int32
	)
	for i, dep := range unique {
		wg.Add(1)
		slot := <-slots
		go func(i int, dep lockfile.Dependency, slot int) {
			defer wg.Done()
			defer func() { slots <- slot }()
			owner, repo := dep.OwnerRepo()
			if r.WorkerProgressFn != nil {
				r.WorkerProgressFn(slot, "→ "+dep.NWO+"@"+dep.Ref)
			}
			result := r.CheckReachability(owner, repo, dep.SHA, dep.Ref)
			result.DepKey = dep.Key()
			results[i] = result
			done := atomic.AddInt32(&completed, 1)
			if r.WorkerProgressFn != nil {
				// Mark completed rather than clearing so the row stays visible
				// until the slot is reused or the spinner stops.
				r.WorkerProgressFn(slot, "✓ "+dep.NWO)
			}
			r.fireVerifyProgress(int(done), total)
		}(i, dep, slot)
	}
	wg.Wait()

	return results
}

// LatestRef returns the highest stable tag for an action repository.
func (r *Resolver) LatestRef(owner, repo string) (string, error) {
	key := owner + "/" + repo
	r.cacheMu.Lock()
	ref, ok := r.latestRefCache[key]
	r.cacheMu.Unlock()
	if ok {
		return ref, nil
	}

	query := `query($owner: String!, $name: String!) {
  repository(owner: $owner, name: $name) {
    refs(refPrefix: "refs/tags/", first: 100) {
      nodes {
        name
      }
    }
  }
}`

	var data struct {
		Repository *struct {
			Refs struct {
				Nodes []struct {
					Name string `json:"name"`
				} `json:"nodes"`
			} `json:"refs"`
		} `json:"repository"`
	}
	if err := r.client.Do(query, map[string]any{"owner": owner, "name": repo}, &data); err != nil {
		return "", err
	}
	if data.Repository == nil {
		return "", fmt.Errorf("%s: repository not found or not accessible", key)
	}

	tags := make([]string, 0, len(data.Repository.Refs.Nodes))
	for _, node := range data.Repository.Refs.Nodes {
		tags = append(tags, node.Name)
	}

	best := selectLatestTag(tags)
	if best == "" {
		return "", fmt.Errorf("%s: no tags available to upgrade", key)
	}

	r.cacheMu.Lock()
	r.latestRefCache[key] = best
	r.cacheMu.Unlock()
	return best, nil
}

func cacheKey(ref parserlock.ActionRef) string {
	return ref.FullName() + "@" + ref.Ref
}

// ResolveAllRecursive resolves action refs and recursively discovers transitive
// dependencies from composite actions by reading their action.yml via GraphQL.
// The returned ParentMap (child dep key → parent dep keys) is owned by the
// caller and safe to mutate or hold across concurrent resolver calls.
func (r *Resolver) ResolveAllRecursive(refs []parserlock.ActionRef) ([]lockfile.Dependency, ParentMap, error) {
	seen := make(map[string]bool)
	var allDeps []lockfile.Dependency
	parentMap := make(ParentMap)

	pending := refs
	depth := 0

	// Rolling counters spanning every BFS depth. Total grows as transitive
	// refs are discovered; done grows as workers complete. The caller renders
	// a single non-jumping bar over the union of work — "transitive" is not a
	// distinct top-level phase, just deeper edges in the same graph.
	var resolveDone atomic.Int64
	var resolveTotal atomic.Int64
	// Fire an initial 0/N callback so the UI shows the bar immediately at the
	// known size of the first wave (refs the caller passed in).
	if r.OnResolveProgress != nil {
		// Compute first-wave uncached size up-front for an accurate initial total.
		firstWave := 0
		r.cacheMu.Lock()
		for _, ref := range refs {
			if _, ok := r.cache[cacheKey(ref)]; !ok {
				firstWave++
			}
		}
		r.cacheMu.Unlock()
		resolveTotal.Store(int64(firstWave))
		r.fireResolveProgress(0, firstWave)
	}

	for len(pending) > 0 {
		if depth >= r.MaxRecursionDepth {
			return allDeps, parentMap, fmt.Errorf("composite action recursion exceeded max depth %d", r.MaxRecursionDepth)
		}

		var toResolve []parserlock.ActionRef
		for _, ref := range pending {
			key := ref.FullName() + "@" + ref.Ref
			if !seen[key] {
				seen[key] = true
				toResolve = append(toResolve, ref)
			}
		}

		if len(toResolve) == 0 {
			break
		}

		var deps []lockfile.Dependency
		var actionYMLs []string
		var err error
		if r.WorkerProgressFn != nil {
			deps, actionYMLs, err = r.resolveWithActionYMLParallel(toResolve, depth, &resolveDone, &resolveTotal)
		} else {
			deps, actionYMLs, err = r.resolveWithActionYML(toResolve)
		}
		// Keep partial results: per-ref failures are surfaced via err, but
		// successful resolutions in `deps` should not be discarded — downstream
		// renderers degrade gracefully per-ref instead of marking everything
		// unresolved.
		allDeps = append(allDeps, deps...)
		if err != nil {
			return dedup(allDeps), parentMap, err
		}

		var nextPending []parserlock.ActionRef
		for i := range deps {
			yml := actionYMLs[i]
			if yml == "" {
				continue
			}

			meta, parseErr := parserlock.ParseActionMeta(yml)
			if parseErr != nil || meta.Execution != parserlock.ExecComposite {
				continue
			}

			// parentMap is keyed at runner-download granularity (NWO@Ref =
			// Dependency.Key()). BFS traversal above is path-aware via
			// cacheKey/seen{} so we visit every sub-action's action.yml,
			// but the recorded edges flatten subpaths back into one node
			// per tarball — same model the runner uses.
			parentKey := deps[i].Key()
			for _, use := range meta.NestedUses {
				actionRef := parserlock.ParseActionRef(use)
				if actionRef == nil {
					continue
				}
				childKey := actionRef.NWO() + "@" + actionRef.Ref
				// Same-tarball edge: a composite whose `uses:` names another
				// subpath in its own repo+ref. At runner-download granularity
				// this is not a new transitive dependency (same tarball, same
				// SHA), so we must not record a parentMap edge — once subpaths
				// collapse to NWO@Ref it would become a self-edge. But we must
				// still descend into the sibling sub-action's action.yml: it
				// can pull in cross-repo transitive deps the parent never
				// references directly (e.g. nested-composite → simple-composite
				// → other-repo). The path-aware seen{} set keys on FullName@Ref
				// and so prevents re-resolving an exact self-reference, making
				// this enqueue loop-safe.
				if childKey == parentKey {
					nextPending = append(nextPending, *actionRef)
					continue
				}
				// Track all parents, deduplicating.
				parents := parentMap[childKey]
				found := false
				for _, p := range parents {
					if p == parentKey {
						found = true
						break
					}
				}
				if !found {
					parentMap[childKey] = append(parents, parentKey)
				}
				nextPending = append(nextPending, *actionRef)
			}
		}

		pending = nextPending
		depth++
	}

	return dedup(allDeps), parentMap, nil
}

func dedup(deps []lockfile.Dependency) []lockfile.Dependency {
	seen := make(map[string]bool)
	var out []lockfile.Dependency
	for _, dep := range deps {
		if !seen[dep.Key()] {
			seen[dep.Key()] = true
			out = append(out, dep)
		}
	}
	return out
}

// resolveWithActionYMLParallel resolves refs one-per-worker so the UI can show
// a stable [done/total] counter and per-worker "→ NWO@Ref" / "✓ NWO" rows.
// Cached refs short-circuit without a worker. Used when WorkerProgressFn is set.
//
// resolveDone and resolveTotal are rolling counters owned by ResolveAllRecursive
// that span every BFS depth, so a single non-jumping progress bar can cover
// direct + transitive resolution as one phase.
func (r *Resolver) resolveWithActionYMLParallel(refs []parserlock.ActionRef, depth int, resolveDone, resolveTotal *atomic.Int64) ([]lockfile.Dependency, []string, error) {
	type resolveResult struct {
		dep lockfile.Dependency
		yml string
		ok  bool
	}
	results := make([]resolveResult, len(refs))

	var uncachedIdx []int
	r.cacheMu.Lock()
	for i, ref := range refs {
		if entry, ok := r.cache[cacheKey(ref)]; ok {
			results[i] = resolveResult{dep: entry.dep, yml: entry.actionYML, ok: true}
		} else {
			uncachedIdx = append(uncachedIdx, i)
		}
	}
	r.cacheMu.Unlock()

	flatten := func() ([]lockfile.Dependency, []string) {
		var deps []lockfile.Dependency
		var ymls []string
		for _, res := range results {
			if !res.ok {
				continue
			}
			deps = append(deps, res.dep)
			ymls = append(ymls, res.yml)
		}
		return deps, ymls
	}

	total := len(uncachedIdx)
	if total == 0 {
		deps, ymls := flatten()
		return deps, ymls, nil
	}

	// Grow the rolling resolve total by the new uncached refs at this depth.
	// First-wave size was preseeded by ResolveAllRecursive, so only count
	// deeper depths here to avoid double-counting.
	if depth > 0 {
		newTotal := resolveTotal.Add(int64(total))
		r.fireResolveProgress(int(resolveDone.Load()), int(newTotal))
	}

	limit := reachabilityConcurrency
	if limit > total {
		limit = total
	}
	slots := make(chan int, limit)
	for i := 0; i < limit; i++ {
		slots <- i
	}

	var (
		wg       sync.WaitGroup
		firstErr error
		errMu    sync.Mutex
	)

	for _, idx := range uncachedIdx {
		wg.Add(1)
		slot := <-slots
		go func(i int, ref parserlock.ActionRef, slot int) {
			defer wg.Done()
			defer func() { slots <- slot }()

			r.WorkerProgressFn(slot, "→ "+ref.NWO()+"@"+ref.Ref)

			deps, ymls, keys, err := r.resolveWithActionYMLBatch([]parserlock.ActionRef{ref})
			if err != nil {
				errMu.Lock()
				if firstErr == nil {
					firstErr = err
				}
				errMu.Unlock()
			}
			if len(deps) > 0 {
				r.cacheMu.Lock()
				for j, dep := range deps {
					r.cache[keys[j]] = resolvedEntry{dep: dep, actionYML: ymls[j]}
				}
				r.cacheMu.Unlock()
				results[i] = resolveResult{dep: deps[0], yml: ymls[0], ok: true}
			}

			done := resolveDone.Add(1)
			r.WorkerProgressFn(slot, "✓ "+ref.NWO())
			r.fireResolveProgress(int(done), int(resolveTotal.Load()))
		}(idx, refs[idx], slot)
	}
	wg.Wait()

	deps, ymls := flatten()
	return deps, ymls, firstErr
}

func (r *Resolver) resolveWithActionYML(refs []parserlock.ActionRef) ([]lockfile.Dependency, []string, error) {
	var allDeps []lockfile.Dependency
	var allYMLs []string
	var uncached []parserlock.ActionRef

	cachedIdx := make(map[int]bool)
	r.cacheMu.Lock()
	for i, ref := range refs {
		if _, ok := r.cache[cacheKey(ref)]; ok {
			cachedIdx[i] = true
		} else {
			uncached = append(uncached, ref)
		}
	}
	r.cacheMu.Unlock()

	var freshDeps []lockfile.Dependency
	var freshYMLs []string
	var freshKeys []string
	var batchErr error
	for i := 0; i < len(uncached); i += MaxBatchSize {
		end := i + MaxBatchSize
		if end > len(uncached) {
			end = len(uncached)
		}
		deps, ymls, keys, err := r.resolveWithActionYMLBatch(uncached[i:end])
		// Keep partial batch results: per-ref failures shouldn't discard
		// successful resolutions from the same batch.
		freshDeps = append(freshDeps, deps...)
		freshYMLs = append(freshYMLs, ymls...)
		freshKeys = append(freshKeys, keys...)
		if err != nil {
			batchErr = err
			break
		}
	}

	// Store fresh resolutions in the cache keyed by cacheKey (FullName@Ref).
	// This preserves per-sub-action entries (e.g. actions/cache/save vs
	// actions/cache/restore) since their action.yml paths differ.
	r.cacheMu.Lock()
	for i, dep := range freshDeps {
		r.cache[freshKeys[i]] = resolvedEntry{dep: dep, actionYML: freshYMLs[i]}
	}
	r.cacheMu.Unlock()

	// Build allDeps from cached refs + freshly-resolved ones. Refs that failed
	// to resolve are simply absent — callers see them missing rather than
	// receiving an empty slice for the whole workflow.
	resolvedFresh := make(map[string]int, len(freshDeps))
	for i := range freshDeps {
		resolvedFresh[freshKeys[i]] = i
	}
	for i, ref := range refs {
		key := cacheKey(ref)
		if cachedIdx[i] {
			r.cacheMu.Lock()
			entry := r.cache[key]
			r.cacheMu.Unlock()
			allDeps = append(allDeps, entry.dep)
			allYMLs = append(allYMLs, entry.actionYML)
			continue
		}
		if fi, ok := resolvedFresh[key]; ok {
			allDeps = append(allDeps, freshDeps[fi])
			allYMLs = append(allYMLs, freshYMLs[fi])
		}
	}

	return allDeps, allYMLs, batchErr
}

type repoResponse struct {
	NameWithOwner string `json:"nameWithOwner"`
	Object        *struct {
		OID  string `json:"oid"`
		File *struct {
			Object *struct {
				Text string `json:"text"`
			} `json:"object"`
		} `json:"file"`
		FileYAML *struct {
			Object *struct {
				Text string `json:"text"`
			} `json:"object"`
		} `json:"fileYaml"`
	} `json:"object"`
}

func (r *Resolver) resolveWithActionYMLBatch(refs []parserlock.ActionRef) ([]lockfile.Dependency, []string, []string, error) {
	query, vars, aliasMap := buildResolveWithFileQuery(refs)

	var data map[string]json.RawMessage
	err := r.client.Do(query, vars, &data)
	var gqlErr *api.GraphQLError
	if err != nil {
		if !errors.As(err, &gqlErr) {
			return nil, nil, nil, err
		}
	}

	return parseResolveWithFileResponse(data, refs, aliasMap, gqlErr, r.hostname)
}

// buildResolveWithFileQuery emits a GraphQL query that resolves each ref's
// commit OID and action.{yml,yaml} blob in a single round-trip. All
// untrusted inputs (owner, repo, ref, path) are passed via GraphQL
// variables rather than interpolated with %q so that a YAML-supplied
// value like `"\n  malicious: query { viewer { login } }"` cannot escape
// the string literal and inject sibling fields.
func buildResolveWithFileQuery(refs []parserlock.ActionRef) (string, map[string]any, map[string]int) {
	aliasMap := make(map[string]int, len(refs))
	vars := make(map[string]any, len(refs)*5)

	var decl strings.Builder
	var body strings.Builder
	decl.WriteString("query(")
	body.WriteString(") {")

	for i, ref := range refs {
		alias := fmt.Sprintf("a%d", i)
		aliasMap[alias] = i

		ownerVar := fmt.Sprintf("owner%d", i)
		nameVar := fmt.Sprintf("name%d", i)
		exprVar := fmt.Sprintf("expr%d", i)
		ymlVar := fmt.Sprintf("yml%d", i)
		yamlVar := fmt.Sprintf("yaml%d", i)

		ymlPath := "action.yml"
		yamlPath := "action.yaml"
		if ref.Path != "" {
			ymlPath = ref.Path + "/action.yml"
			yamlPath = ref.Path + "/action.yaml"
		}

		vars[ownerVar] = ref.Owner
		vars[nameVar] = ref.Repo
		// Peel through annotated tags with `^{commit}`. Without it, an
		// annotated tag's `object(expression:)` returns a Tag object, not a
		// Commit — the `... on Commit` fragment doesn't match and `oid` comes
		// back empty. The peel is a no-op for branches, SHAs, and lightweight
		// tags, so we can apply it unconditionally.
		vars[exprVar] = ref.Ref + "^{commit}"
		vars[ymlVar] = ymlPath
		vars[yamlVar] = yamlPath

		if i > 0 {
			decl.WriteString(", ")
		}
		fmt.Fprintf(&decl, "$%s: String!, $%s: String!, $%s: String!, $%s: String!, $%s: String!",
			ownerVar, nameVar, exprVar, ymlVar, yamlVar)

		fmt.Fprintf(&body, " %s: repository(owner: $%s, name: $%s) {", alias, ownerVar, nameVar)
		body.WriteString(" nameWithOwner")
		fmt.Fprintf(&body, " object(expression: $%s) {", exprVar)
		body.WriteString(" ... on Commit { oid")
		fmt.Fprintf(&body, " file: file(path: $%s) { object { ... on Blob { text } } }", ymlVar)
		fmt.Fprintf(&body, " fileYaml: file(path: $%s) { object { ... on Blob { text } } }", yamlVar)
		body.WriteString(" }")
		body.WriteString(" }")
		body.WriteString(" }")
	}

	body.WriteString(" }")
	return decl.String() + body.String(), vars, aliasMap
}

func parseResolveWithFileResponse(data map[string]json.RawMessage, refs []parserlock.ActionRef, aliasMap map[string]int, gqlErr *api.GraphQLError, hostname string) ([]lockfile.Dependency, []string, []string, error) {
	var deps []lockfile.Dependency
	var ymls []string
	var keys []string
	var errs []string

	samlOwners := samlBlockedOwners(gqlErr, refs, aliasMap)

	for alias, idx := range aliasMap {
		ref := refs[idx]
		raw, ok := data[alias]
		if !ok {
			if samlOwners[ref.Owner] {
				errs = append(errs, fmt.Sprintf("%s@%s: %s", ref.NWO(), ref.Ref, ssoRequiredMessage(hostname, ref.Owner)))
				continue
			}
			errs = append(errs, fmt.Sprintf("%s@%s: not found in response", ref.NWO(), ref.Ref))
			continue
		}
		if string(raw) == "null" {
			if samlOwners[ref.Owner] {
				errs = append(errs, fmt.Sprintf("%s@%s: %s", ref.NWO(), ref.Ref, ssoRequiredMessage(hostname, ref.Owner)))
				continue
			}
			errs = append(errs, fmt.Sprintf("%s@%s: repository not found or not accessible", ref.NWO(), ref.Ref))
			continue
		}

		var repo repoResponse
		if err := json.Unmarshal(raw, &repo); err != nil {
			errs = append(errs, fmt.Sprintf("%s@%s: failed to parse: %v", ref.NWO(), ref.Ref, err))
			continue
		}

		if repo.Object == nil || repo.Object.OID == "" {
			errs = append(errs, fmt.Sprintf("%s@%s: ref %q does not exist", ref.NWO(), ref.Ref, ref.Ref))
			continue
		}

		dep := lockfile.Dependency{
			NWO:  ref.NWO(),
			Path: ref.Path,
			Ref:  ref.Ref,
			SHA:  repo.Object.OID,
		}
		deps = append(deps, dep)
		keys = append(keys, cacheKey(ref))

		var yml string
		if repo.Object.File != nil && repo.Object.File.Object != nil {
			yml = repo.Object.File.Object.Text
		} else if repo.Object.FileYAML != nil && repo.Object.FileYAML.Object != nil {
			yml = repo.Object.FileYAML.Object.Text
		}
		ymls = append(ymls, yml)
	}

	if len(errs) > 0 {
		return deps, ymls, keys, fmt.Errorf("resolution errors:\n  %s", strings.Join(errs, "\n  "))
	}

	return deps, ymls, keys, nil
}

// samlBlockedOwners returns the set of repository owners whose resolution
// failed an organization SAML SSO enforcement check. GitHub's GraphQL API
// reports these as per-alias FORBIDDEN errors carrying
// extensions.saml_failure == true alongside a null data entry for that
// alias; without this mapping the null entry is indistinguishable from a
// genuinely missing repository. The GraphQL alias in each error's Path is
// translated back to its owner via aliasMap + refs.
func samlBlockedOwners(gqlErr *api.GraphQLError, refs []parserlock.ActionRef, aliasMap map[string]int) map[string]bool {
	if gqlErr == nil {
		return nil
	}
	owners := make(map[string]bool)
	for _, e := range gqlErr.Errors {
		if blocked, _ := e.Extensions["saml_failure"].(bool); !blocked {
			continue
		}
		if len(e.Path) == 0 {
			continue
		}
		alias, ok := e.Path[0].(string)
		if !ok {
			continue
		}
		idx, ok := aliasMap[alias]
		if !ok || idx < 0 || idx >= len(refs) {
			continue
		}
		owners[refs[idx].Owner] = true
	}
	return owners
}

// ssoRequiredMessage builds an actionable error directing the user to
// authorize their token for an SSO-protected organization, rather than
// collapsing the failure into a generic "not found" message.
func ssoRequiredMessage(hostname, owner string) string {
	host := hostname
	if host == "" {
		host = "github.com"
	}
	return fmt.Sprintf("SSO authorization required: your token is not authorized for the %q organization (SAML enforcement). Authorize it at https://%s/orgs/%s/sso and retry", owner, host, owner)
}

func selectLatestTag(tags []string) string {
	seen := make(map[string]struct{}, len(tags))
	bestMajor := -1
	bestMajorTag := ""
	bestVersion := [3]int{-1, -1, -1}
	bestVersionTag := ""
	bestFallback := ""

	for _, tag := range tags {
		tag = strings.TrimSpace(tag)
		if tag == "" {
			continue
		}
		if _, ok := seen[tag]; ok {
			continue
		}
		seen[tag] = struct{}{}
		if tag > bestFallback {
			bestFallback = tag
		}

		sv, ok := lockfile.ParseVersion(tag)
		if !ok || !sv.IsStable() {
			continue
		}

		if sv.Raw == sv.MajorTag() && sv.Major > bestMajor {
			bestMajor = sv.Major
			bestMajorTag = tag
		}

		version := [3]int{sv.Major, sv.Minor, sv.Patch}
		if version[0] > bestVersion[0] ||
			(version[0] == bestVersion[0] && version[1] > bestVersion[1]) ||
			(version[0] == bestVersion[0] && version[1] == bestVersion[1] && version[2] > bestVersion[2]) {
			bestVersion = version
			bestVersionTag = tag
		}
	}

	if bestMajorTag != "" {
		return bestMajorTag
	}
	if bestVersionTag != "" {
		return bestVersionTag
	}
	return bestFallback
}
