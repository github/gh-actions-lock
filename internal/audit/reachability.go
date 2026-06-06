package audit

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/github/gh-actions-pin/internal/cachekey"
	"github.com/github/gh-actions-pin/internal/lockfile"
	"github.com/github/gh-actions-pin/internal/resolve"
)

// CheckReachability verifies that the pinned SHA is reachable from at least
// one branch of owner/repo, using the documented REST APIs (list-branches +
// compare for ancestry). This catches fork-network injection where a SHA
// exists in GitHub's shared object store but is not part of the canonical
// repository's history.
//
// See: https://docs.zizmor.sh/audits/#impostor-commit
func (a *Auditor) CheckReachability(ctx context.Context, owner, repo, sha, ref string) resolve.ReachabilityResult {
	result := resolve.ReachabilityResult{
		Owner: owner,
		Repo:  repo,
		Ref:   ref,
		SHA:   sha,
	}

	if status, detail, ok := a.r.GetReachCache(owner, repo, sha, ref); ok {
		result.Status = status
		result.Detail = detail
		return result
	}

	// Allow tests to inject a fake implementation.
	if fn := a.r.CheckReachFn(); fn != nil {
		result.Status, result.Detail = fn(ctx, owner, repo, sha, ref)
		a.r.PutReachCache(owner, repo, sha, ref, result.Status, result.Detail)
		return result
	}

	defaultBranch := a.r.GetDefaultBranch(ctx, owner, repo)

	// Phase 1: validate the likely/canonical branch set first.
	likely := a.likelyBranches(ctx, owner, repo, sha, ref, defaultBranch)
	foundBranch, likelyChecked := a.reachabilityScan(ctx, owner, repo, sha, ref, likely, defaultBranch)

	// Phase 2: full breadth scan only on a phase-1 miss.
	var branches []resolve.BranchHead
	var protectedAnyChecked, allAnyChecked bool
	if foundBranch == "" {
		result.FullScanUsed = true
		a.r.Progress("%s/%s: not on a canonical branch — scanning all branches", owner, repo)
		var err error
		branches, err = a.r.ListBranches(ctx, owner, repo)
		if err != nil {
			result.Status = resolve.ReachabilityUnknown
			result.Detail = fmt.Sprintf("could not list branches for %s/%s: %s", owner, repo, err)
			a.r.PutReachCache(owner, repo, sha, ref, result.Status, result.Detail)
			return result
		}
		protectedBranches := make([]resolve.BranchHead, 0, len(branches))
		for _, b := range branches {
			if b.Protected {
				protectedBranches = append(protectedBranches, b)
			}
		}
		foundBranch, protectedAnyChecked = a.reachabilityScan(ctx, owner, repo, sha, ref, protectedBranches, defaultBranch)
		if foundBranch == "" {
			foundBranch, allAnyChecked = a.reachabilityScan(ctx, owner, repo, sha, ref, branches, defaultBranch)
		}
	}

	anyChecked := likelyChecked || protectedAnyChecked || allAnyChecked
	hadBranches := len(likely) > 0 || len(branches) > 0

	if foundBranch != "" {
		result.Status = resolve.Reachable
		if lockfile.IsFullSha(ref) {
			result.Detail = fmt.Sprintf("pinned to a bare SHA; commit is on branch %s but origin cannot be verified at job runtime — prefer pinning to a tag", foundBranch)
		} else {
			result.Detail = fmt.Sprintf("commit is on branch %s", foundBranch)
		}
	} else if !anyChecked && hadBranches {
		result.Status = resolve.ReachabilityUnknown
		result.Detail = fmt.Sprintf("could not verify commit reachability for %s/%s — every Compare lookup failed (rate limit or transient error); try again later", owner, repo)
	} else {
		result.Status = resolve.Unreachable
		if lockfile.IsFullSha(ref) {
			result.Detail = "pinned to a bare SHA; commit is NOT on any branch — possible fork-network commit"
		} else {
			result.Detail = fmt.Sprintf("commit %s not found on any branch of %s/%s — possible fork-network injection",
				shortSha(sha), owner, repo)
		}
	}

	a.r.PutReachCache(owner, repo, sha, ref, result.Status, result.Detail)
	return result
}

// reachabilityScan walks candidates (in OrderedBranches tier order),
// returning the first branch whose HEAD is sha or whose lineage contains
// sha as an ancestor via the Compare API.
func (a *Auditor) reachabilityScan(ctx context.Context, owner, repo, sha, ref string, candidates []resolve.BranchHead, defaultBranch string) (matched string, anyChecked bool) {
	if len(candidates) == 0 {
		return "", false
	}
	// Fast path: exact HEAD match.
	for _, b := range candidates {
		if eqFoldSHA(b.SHA, sha) {
			return b.Name, true
		}
	}
	// Slow path: ancestry via Compare API in tier order.
	hintBranch := a.r.BranchHint(owner, repo, sha)
	ordered := resolve.OrderedBranches(candidates, hintBranch, ref, defaultBranch)
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
		go func(i int, b resolve.BranchHead) {
			defer wg.Done()
			defer func() { <-sem }()
			a.r.Progress("scanning %s/%s branch %s", owner, repo, b.Name)
			ok, err := a.r.BranchContainsCommit(ctx, owner, repo, sha, b.SHA)
			if err != nil {
				return
			}
			results[i] = result{contains: ok, checked: true}
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
func (a *Auditor) CheckReachabilityAll(ctx context.Context, deps []lockfile.Dependency) []resolve.ReachabilityResult {
	seenReach := make(map[cachekey.Reach]bool)

	unique := make([]lockfile.Dependency, 0, len(deps))
	for _, dep := range deps {
		owner, repo := dep.OwnerRepo()
		if owner == "" {
			continue
		}
		key := cachekey.ForReach(owner, repo, dep.SHA, dep.Ref)
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

	a.r.FireVerifyProgress(0, total)
	if a.r.WorkerProgressFn != nil {
		for slot := 0; slot < reachabilityConcurrency; slot++ {
			a.r.WorkerProgressFn(slot, "")
		}
	}

	// Pre-warm the per-repo caches.
	type repoEntry struct {
		owner, repo string
	}
	uniqueRepos := make([]repoEntry, 0)
	seenRepo := make(map[cachekey.Repo]bool)
	for _, dep := range unique {
		owner, repo := dep.OwnerRepo()
		k := cachekey.ForRepo(owner, repo)
		if !seenRepo[k] {
			seenRepo[k] = true
			uniqueRepos = append(uniqueRepos, repoEntry{owner: owner, repo: repo})
		}
	}
	if a.r.CheckReachFn() == nil && len(uniqueRepos) > 0 {
		warmupLimit := reachabilityConcurrency
		if warmupLimit > len(uniqueRepos) {
			warmupLimit = len(uniqueRepos)
		}
		warmupSlots := make(chan int, warmupLimit)
		for i := 0; i < warmupLimit; i++ {
			warmupSlots <- i
		}
		var warmupWG sync.WaitGroup
		for _, re := range uniqueRepos {
			warmupWG.Add(1)
			slot := <-warmupSlots
			go func(re repoEntry, slot int) {
				defer warmupWG.Done()
				defer func() { warmupSlots <- slot }()
				rk := re.owner + "/" + re.repo
				if a.r.WorkerProgressFn != nil {
					a.r.WorkerProgressFn(slot, "→ loading "+rk)
				}
				_, _ = a.r.ListBranches(ctx, re.owner, re.repo)
				_ = a.r.GetDefaultBranch(ctx, re.owner, re.repo)
				if a.r.WorkerProgressFn != nil {
					a.r.WorkerProgressFn(slot, "")
				}
			}(re, slot)
		}
		warmupWG.Wait()
	}

	// Fan out the per-dependency checks.
	results := make([]resolve.ReachabilityResult, total)
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
			if a.r.WorkerProgressFn != nil {
				a.r.WorkerProgressFn(slot, "→ "+dep.NWO+"@"+dep.Ref)
			}
			result := a.CheckReachability(ctx, owner, repo, dep.SHA, dep.Ref)
			result.DepKey = dep.Key()
			results[i] = result
			done := atomic.AddInt32(&completed, 1)
			if a.r.WorkerProgressFn != nil {
				a.r.WorkerProgressFn(slot, "✓ "+dep.NWO)
			}
			a.r.FireVerifyProgress(int(done), total)
		}(i, dep, slot)
	}
	wg.Wait()

	return results
}

// likelyBranches assembles the high-trust candidate set validated before any
// full branch scan.
func (a *Auditor) likelyBranches(ctx context.Context, owner, repo, sha, ref, defaultBranch string) []resolve.BranchHead {
	seen := make(map[string]bool)
	var out []resolve.BranchHead
	addNamed := func(name string) {
		if name == "" || seen[name] {
			return
		}
		if bh, ok := a.r.GetBranchHead(ctx, owner, repo, name); ok {
			seen[name] = true
			out = append(out, bh)
		}
	}
	if ref != "" && !lockfile.IsFullSha(ref) {
		addNamed(ref)
	}
	addNamed(a.r.BranchHint(owner, repo, sha))
	addNamed(defaultBranch)
	for _, bh := range a.r.ListProtectedBranches(ctx, owner, repo) {
		if bh.Name == "" || seen[bh.Name] {
			continue
		}
		seen[bh.Name] = true
		out = append(out, bh)
	}
	for _, bh := range a.r.ListReleaseBranches(ctx, owner, repo) {
		if bh.Name == "" || seen[bh.Name] {
			continue
		}
		seen[bh.Name] = true
		out = append(out, bh)
	}
	return out
}

func shortSha(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	return s
}

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
