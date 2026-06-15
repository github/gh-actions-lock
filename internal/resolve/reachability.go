package resolve

import (
	"context"
	"fmt"
	"sync"

	parserlock "github.com/github/actions-lockfile/go/pkg/lockfile"
	"github.com/github/gh-actions-lock/internal/dep"
	"github.com/github/gh-actions-lock/internal/ghapi"
	"github.com/github/gh-actions-lock/internal/pinpool"
)

// CheckReachability verifies that the pinned SHA is reachable from at least
// one branch of owner/repo, using the documented REST APIs (list-branches +
// compare for ancestry). This catches fork-network injection where a SHA
// exists in GitHub's shared object store but is not part of the canonical
// repository's history.
//
// See: https://docs.zizmor.sh/audits/#impostor-commit
func (r *Resolver) CheckReachability(ctx context.Context, owner, repo, sha, ref string) ReachabilityResult {
	// Fast path: cache hit.
	if status, detail, ok := r.getReachCache(owner, repo, sha, ref); ok {
		return ReachabilityResult{
			Owner: owner, Repo: repo, Ref: ref, SHA: sha,
			Status: status, Detail: detail,
		}
	}

	// Coalesce concurrent checks for the same dep across parallel workflows.
	sfKey := owner + "/" + repo + "@" + ref + ":" + sha
	v, _, _ := r.reachSF.Do(sfKey, func() (any, error) {
		return r.checkReachabilityOnce(ctx, owner, repo, sha, ref), nil
	})
	return v.(ReachabilityResult)
}

// checkReachabilityOnce is the actual reachability check, called at most once
// per unique (owner, repo, sha, ref) via singleflight coalescing.
func (r *Resolver) checkReachabilityOnce(ctx context.Context, owner, repo, sha, ref string) ReachabilityResult {
	result := ReachabilityResult{
		Owner: owner,
		Repo:  repo,
		Ref:   ref,
		SHA:   sha,
	}

	// Double-check cache (another goroutine may have populated it).
	if status, detail, ok := r.getReachCache(owner, repo, sha, ref); ok {
		result.Status = status
		result.Detail = detail
		return result
	}

	// Allow tests to inject a fake implementation.
	if fn := r.checkReachFn; fn != nil {
		result.Status, result.Detail = fn(ctx, owner, repo, sha, ref)
		r.putReachCache(owner, repo, sha, ref, result.Status, result.Detail)
		return result
	}

	defaultBranch := r.GetDefaultBranch(ctx, owner, repo)

	// Phase 0: fast GraphQL check against default branch + ref branch.
	// This avoids the expensive ListProtectedBranches pagination when the
	// SHA is a simple ancestor of the default branch (the common case).
	var foundBranch string
	var anyChecked bool
	quickCandidates := r.quickBranches(ctx, owner, repo, sha, ref, defaultBranch)
	if len(quickCandidates) > 0 {
		foundBranch, anyChecked = r.reachabilityScan(ctx, owner, repo, sha, ref, quickCandidates, defaultBranch)
	}

	// Phase 1: likely/canonical branch set (includes protected + release branches).
	var likelyChecked bool
	if foundBranch == "" {
		likely := r.likelyBranches(ctx, owner, repo, sha, ref, defaultBranch)
		// Dedup: skip branches already checked in phase 0.
		quickSeen := make(map[string]bool, len(quickCandidates))
		for _, b := range quickCandidates {
			quickSeen[b.Name] = true
		}
		var remaining []ghapi.BranchHead
		for _, b := range likely {
			if !quickSeen[b.Name] {
				remaining = append(remaining, b)
			}
		}
		if len(remaining) > 0 {
			foundBranch, likelyChecked = r.reachabilityScan(ctx, owner, repo, sha, ref, remaining, defaultBranch)
		}
		anyChecked = anyChecked || likelyChecked
	}

	// Phase 2: full breadth scan only on a phase-1 miss.
	var branches []ghapi.BranchHead
	var protectedAnyChecked, allAnyChecked bool
	if foundBranch == "" {
		result.FullScanUsed = true
		var err error
		branches, err = r.ListBranches(ctx, owner, repo)
		if err != nil {
			result.Status = ReachabilityUnknown
			result.Detail = fmt.Sprintf("could not list branches for %s/%s: %s", owner, repo, err)
			r.putReachCache(owner, repo, sha, ref, result.Status, result.Detail)
			return result
		}
		protectedBranches := make([]ghapi.BranchHead, 0, len(branches))
		for _, b := range branches {
			if b.Protected {
				protectedBranches = append(protectedBranches, b)
			}
		}
		foundBranch, protectedAnyChecked = r.reachabilityScan(ctx, owner, repo, sha, ref, protectedBranches, defaultBranch)
		if foundBranch == "" {
			foundBranch, allAnyChecked = r.reachabilityScan(ctx, owner, repo, sha, ref, branches, defaultBranch)
		}
		anyChecked = anyChecked || protectedAnyChecked || allAnyChecked
	}

	hadBranches := len(quickCandidates) > 0 || len(branches) > 0

	if foundBranch != "" {
		result.Status = Reachable
		// Stash the discovered branch so DiscoverContaining can reuse it
		// via branchHintBySHA, avoiding a redundant full-branch scan.
		r.branchHintBySHA.Put(ghapi.ForNWOSha(owner, repo, sha), foundBranch)
		if parserlock.IsFullSha(ref) {
			result.Detail = fmt.Sprintf("pinned to a bare SHA; commit is on branch %s but origin cannot be verified at job runtime — prefer pinning to a tag", foundBranch)
		} else {
			result.Detail = fmt.Sprintf("commit is on branch %s", foundBranch)
		}
	} else if !anyChecked && hadBranches {
		result.Status = ReachabilityUnknown
		result.Detail = fmt.Sprintf("could not verify commit reachability for %s/%s — every Compare lookup failed (rate limit or transient error); try again later", owner, repo)
	} else {
		result.Status = Unreachable
		if parserlock.IsFullSha(ref) {
			result.Detail = "pinned to a bare SHA; commit is NOT on any branch — possible fork-network commit"
		} else {
			result.Detail = fmt.Sprintf("commit %s not found on any branch of %s/%s — possible fork-network injection",
				parserlock.ShortSHA(sha), owner, repo)
		}
	}

	r.putReachCache(owner, repo, sha, ref, result.Status, result.Detail)
	return result
}

// reachabilityScan walks candidates (in OrderedBranches tier order),
// returning the first branch whose HEAD is sha or whose lineage contains
// sha as an ancestor via the Compare API.
func (r *Resolver) reachabilityScan(ctx context.Context, owner, repo, sha, ref string, candidates []ghapi.BranchHead, defaultBranch string) (matched string, anyChecked bool) {
	if len(candidates) == 0 {
		return "", false
	}
	// Fast path: exact HEAD match.
	for _, b := range candidates {
		if eqFoldSHA(b.SHA, sha) {
			return b.Name, true
		}
	}
	// Slow path: ancestry via batched GraphQL Ref.compare.
	// One query checks all branches at once instead of N serial REST calls.
	hintBranch := r.branchHint(owner, repo, sha)
	ordered := ghapi.OrderedBranches(candidates, hintBranch, ref, defaultBranch)
	if len(ordered) == 0 {
		return "", false
	}

	matched, checked, err := r.gh.BatchBranchContains(ctx, owner, repo, sha, ordered)
	if matched != "" {
		return matched, true
	}
	if err != nil {
		// Partial or total GraphQL failure with no positive match: re-check
		// via serial REST Compare. Otherwise a batch that errored after
		// checking only some branches (anyChecked=true, matched="") would be
		// reported as "checked everything, found nothing" — a false
		// Unreachable.
		return r.reachabilityScanREST(ctx, owner, repo, sha, ordered)
	}
	return "", checked
}

// reachabilityScanREST is the legacy per-branch REST Compare fallback,
// used when GraphQL batch fails entirely.
func (r *Resolver) reachabilityScanREST(ctx context.Context, owner, repo, sha string, ordered []ghapi.BranchHead) (matched string, anyChecked bool) {
	limit := reachabilityConcurrency
	if len(ordered) < limit {
		limit = len(ordered)
	}

	scanCtx, scanCancel := context.WithCancel(ctx)
	defer scanCancel()

	type result struct {
		contains bool
		checked  bool
	}
	results := make([]result, len(ordered))
	sem := make(chan struct{}, limit)
	var wg sync.WaitGroup
	for i, b := range ordered {
		wg.Add(1)
		select {
		case sem <- struct{}{}:
		case <-scanCtx.Done():
			wg.Done()
			continue
		}
		go func(i int, b ghapi.BranchHead) {
			defer wg.Done()
			defer func() { <-sem }()
			ok, err := r.gh.CompareCommits(scanCtx, owner, repo, sha, b.SHA)
			if err != nil {
				return
			}
			results[i] = result{contains: ok, checked: true}
			if ok {
				scanCancel()
			}
		}(i, b)
	}
	wg.Wait()
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

// CheckReachabilityAll runs reachability checks on a batch of dependencies,
// deduplicating by owner/repo/sha/ref.
func (r *Resolver) CheckReachabilityAll(ctx context.Context, deps []dep.Dependency) []ReachabilityResult {
	seenReach := make(map[ghapi.Reach]bool)

	unique := make([]dep.Dependency, 0, len(deps))
	for _, dep := range deps {
		owner, repo := dep.OwnerRepo()
		if owner == "" {
			continue
		}
		key := ghapi.ForReach(owner, repo, dep.SHA, dep.Ref)
		if seenReach[key] {
			continue
		}
		seenReach[key] = true
		unique = append(unique, dep)
	}

	total := len(unique)
	if total == 0 {
		return nil
	}

	return r.checkReachabilityAllPooled(ctx, unique)
}

// repoEntry is a small pair used by the warmup loop.
type repoEntry struct {
	owner, repo string
}

// checkReachabilityAllPooled uses the shared pool for warmup + main fan-out.
func (r *Resolver) checkReachabilityAllPooled(ctx context.Context, unique []dep.Dependency) []ReachabilityResult {
	total := len(unique)

	// Warmup: pre-populate per-repo caches in parallel.
	// Skip warmup if all deps are already cached (avoids spinner noise).
	if r.checkReachFn == nil {
		var repos []repoEntry
		seenRepo := make(map[ghapi.Repo]bool)
		for _, dep := range unique {
			owner, repo := dep.OwnerRepo()
			if _, _, ok := r.getReachCache(owner, repo, dep.SHA, dep.Ref); ok {
				continue // already cached, no warmup needed
			}
			k := ghapi.ForRepo(owner, repo)
			if !seenRepo[k] {
				seenRepo[k] = true
				repos = append(repos, repoEntry{owner: owner, repo: repo})
			}
		}
		if len(repos) > 0 {
			_ = pinpool.RunTyped(r.Pool, ctx, "",
				repos,
				func(re repoEntry) string { return "fetching metadata " + re.owner + "/" + re.repo },
				func(ctx context.Context, _ int, re repoEntry) error {
					// Only pre-warm default branch (cheap, single REST call).
					// Branch listing is deferred to Phase 2 on reachability
					// miss — GraphQL batch compare eliminates the need for
					// eagerly listing all branches.
					_ = r.GetDefaultBranch(ctx, re.owner, re.repo)
					return nil
				},
			)
		}
	}

	// Separate deps into three groups:
	//  1. cached   — result already available, no work needed
	//  2. pooled   — first to claim this dep, submit to the pool
	//  3. waiters  — another goroutine already claimed it, wait via
	//                singleflight without occupying a pool slot
	results := make([]ReachabilityResult, total)

	type indexedDep struct {
		idx int
		dep dep.Dependency
	}
	var pooled, waiters []indexedDep
	for i, dep := range unique {
		owner, repo := dep.OwnerRepo()
		if status, detail, ok := r.getReachCache(owner, repo, dep.SHA, dep.Ref); ok {
			results[i] = ReachabilityResult{
				Owner: owner, Repo: repo, Ref: dep.Ref, SHA: dep.SHA,
				Status: status, Detail: detail, DepKey: dep.Key(),
			}
			continue
		}
		sfKey := owner + "/" + repo + "@" + dep.Ref + ":" + dep.SHA
		id := indexedDep{idx: i, dep: dep}
		if r.claimReachability(sfKey) {
			pooled = append(pooled, id)
		} else {
			waiters = append(waiters, id)
		}
	}

	// Submit first-claimers to the pool (visible in spinner).
	var poolErr error
	if len(pooled) > 0 {
		poolErr = pinpool.RunTyped(r.Pool, ctx, "",
			pooled,
			func(id indexedDep) string { return "verifying " + id.dep.NWO + "@" + id.dep.Ref },
			func(ctx context.Context, _ int, id indexedDep) error {
				owner, repo := id.dep.OwnerRepo()
				result := r.CheckReachability(ctx, owner, repo, id.dep.SHA, id.dep.Ref)
				result.DepKey = id.dep.Key()
				results[id.idx] = result
				return nil
			},
		)
	}

	// Waiters: another goroutine is checking these in the pool. Wait via
	// singleflight (coalesces to the in-flight call) without occupying a
	// pool worker slot or spinner line.
	for _, id := range waiters {
		owner, repo := id.dep.OwnerRepo()
		result := r.CheckReachability(ctx, owner, repo, id.dep.SHA, id.dep.Ref)
		result.DepKey = id.dep.Key()
		results[id.idx] = result
	}

	// Fail closed: if the pool cancelled or errored before a job ran, its
	// result stays zero-value (empty Status). plan.go only acts on
	// Unreachable/ReachabilityUnknown, so an empty status would let a dep be
	// pinned unverified. Backfill any unset result as ReachabilityUnknown.
	for i := range results {
		if results[i].Status == "" {
			owner, repo := unique[i].OwnerRepo()
			detail := "reachability check did not complete"
			if poolErr != nil {
				detail = fmt.Sprintf("reachability check did not complete: %v", poolErr)
			}
			results[i] = ReachabilityResult{
				Owner: owner, Repo: repo, Ref: unique[i].Ref, SHA: unique[i].SHA,
				Status: ReachabilityUnknown, Detail: detail, DepKey: unique[i].Key(),
			}
		}
	}

	return results
}

// quickBranches returns the 1-2 cheapest candidates: the default branch and
// (for non-SHA refs) the ref-matching branch. No REST pagination — just
// single-branch HEAD lookups. This lets the GraphQL batch short-circuit
// before the expensive ListProtectedBranches call in likelyBranches.
func (r *Resolver) quickBranches(ctx context.Context, owner, repo, sha, ref, defaultBranch string) []ghapi.BranchHead {
	seen := make(map[string]bool)
	var out []ghapi.BranchHead
	addNamed := func(name string) {
		if name == "" || seen[name] {
			return
		}
		if bh, ok := r.GetBranchHead(ctx, owner, repo, name); ok {
			seen[name] = true
			out = append(out, bh)
		}
	}
	addNamed(defaultBranch)
	if ref != "" && !parserlock.IsFullSha(ref) {
		addNamed(ref)
	}
	addNamed(r.branchHint(owner, repo, sha))
	return out
}

// likelyBranches assembles the high-trust candidate set validated before any
// full branch scan.
func (r *Resolver) likelyBranches(ctx context.Context, owner, repo, sha, ref, defaultBranch string) []ghapi.BranchHead {
	seen := make(map[string]bool)
	var out []ghapi.BranchHead
	addNamed := func(name string) {
		if name == "" || seen[name] {
			return
		}
		if bh, ok := r.GetBranchHead(ctx, owner, repo, name); ok {
			seen[name] = true
			out = append(out, bh)
		}
	}
	if ref != "" && !parserlock.IsFullSha(ref) {
		addNamed(ref)
	}
	addNamed(r.branchHint(owner, repo, sha))
	addNamed(defaultBranch)
	for _, bh := range r.ListProtectedBranches(ctx, owner, repo) {
		if bh.Name == "" || seen[bh.Name] {
			continue
		}
		seen[bh.Name] = true
		out = append(out, bh)
	}
	for _, bh := range r.ListReleaseBranches(ctx, owner, repo) {
		if bh.Name == "" || seen[bh.Name] {
			continue
		}
		seen[bh.Name] = true
		out = append(out, bh)
	}
	return out
}

// eqFoldSHA compares two hex SHAs case-insensitively. Length must match.
func eqFoldSHA(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		ca, cb := a[i], b[i]
		if ca >= 'A' && ca <= 'Z' {
			ca += 'a' - 'A'
		}
		if cb >= 'A' && cb <= 'Z' {
			cb += 'a' - 'A'
		}
		if ca != cb {
			return false
		}
	}
	return true
}
