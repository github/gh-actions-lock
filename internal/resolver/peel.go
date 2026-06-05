package resolver

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/cli/go-gh/v2/pkg/api"
	"github.com/github/gh-actions-pin/internal/cachekey"
	"github.com/github/gh-actions-pin/internal/lockfile"
)

// tagObjectPeelQuery resolves both the type of the object at $oid and the
// commit it peels to in a single round trip. `object(expression:)` with the
// `^{commit}` suffix follows tag-of-tag chains server-side, so depth costs
// us nothing. The two aliases are evaluated independently — `head` tells us
// whether the input SHA is itself an annotated tag object (vs a commit,
// blob, tree, or unknown OID), and `peeled.oid` is the underlying commit.
const tagObjectPeelQuery = `query($owner: String!, $name: String!, $oid: GitObjectID!, $expr: String!) {
  repository(owner: $owner, name: $name) {
    head: object(oid: $oid) { __typename }
    peeled: object(expression: $expr) { ... on Commit { oid } }
  }
}`

// PeelTagObject reports whether sha is an annotated tag object in owner/repo
// and, if so, the commit SHA it dereferences to. Annotated and immutable
// release tags are stored as tag *objects* whose own SHA differs from the
// commit they point at, so a `uses:` pin to that tag-object SHA is a
// legitimate immutable pin even though it does not equal the commit SHA the
// ref resolves to. Tag-of-tag chains are peeled server-side via the
// `^{commit}` revision suffix, so chain depth costs us nothing. Returns
// ok=false for lightweight tags, plain commits, unknown SHAs, or any
// lookup failure (fail open).
func (r *Resolver) PeelTagObject(owner, repo, sha string) (commit string, ok bool) {
	return r.peelTagObject(owner, repo, sha)
}

// IsKnownTagObject reports whether (owner, repo, sha) is already cached as
// an annotated tag object. Cache-only — never issues a network call. Use
// from display paths (e.g. URL builders) that must not block on I/O. A
// false return means either "definitely not a tag object" or "we have not
// peeled this SHA yet" — callers must treat the negative as ambiguous.
func (r *Resolver) IsKnownTagObject(owner, repo, sha string) bool {
	key := cachekey.ForNWOSha(owner, repo, sha)
	cached, hit := r.tagObjectCache.get(key)
	return hit && cached.isTag
}

func (r *Resolver) peelTagObject(owner, repo, sha string) (string, bool) {
	key := cachekey.ForNWOSha(owner, repo, sha)
	if cached, hit := r.tagObjectCache.get(key); hit {
		return cached.commit, cached.isTag
	}
	var resp struct {
		Repository *struct {
			Head *struct {
				Typename string `json:"__typename"`
			} `json:"head"`
			Peeled *struct {
				OID string `json:"oid"`
			} `json:"peeled"`
		} `json:"repository"`
	}
	vars := map[string]any{
		"owner": owner,
		"name":  repo,
		"oid":   sha,
		"expr":  sha + "^{commit}",
	}
	if err := r.client.Do(tagObjectPeelQuery, vars, &resp); err != nil {
		// Transient transport or partial GraphQL error — fail open and do
		// not cache, matching the prior REST behavior so a later retry
		// can still succeed.
		return "", false
	}
	if resp.Repository == nil || resp.Repository.Head == nil {
		// OID not present in the repo (or repo not accessible). Treat as
		// "not a tag object" but do not cache — the SHA may show up after
		// a fetch / permission grant.
		return "", false
	}
	if resp.Repository.Head.Typename != "Tag" {
		// Definitively not an annotated tag object — cache the negative so
		// the common "pin is a plain commit SHA" case never re-queries.
		r.tagObjectCache.put(key, tagPeel{})
		return "", false
	}
	if resp.Repository.Peeled == nil || resp.Repository.Peeled.OID == "" {
		// Annotated tag whose chain bottoms out at a non-commit (tree/blob)
		// or for whom GraphQL refused to peel. Don't cache — the result is
		// ambiguous rather than authoritatively negative.
		return "", false
	}
	r.tagObjectCache.put(key, tagPeel{commit: resp.Repository.Peeled.OID, isTag: true})
	return resp.Repository.Peeled.OID, true
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

	cacheKey := cachekey.ForReach(owner, repo, sha, ref)
	if entry, ok := r.reachCache.get(cacheKey); ok {
		result.Status = entry.status
		result.Detail = entry.detail
		return result
	}

	// Allow tests to inject a fake implementation.
	if r.checkReachFn != nil {
		result.Status, result.Detail = r.checkReachFn(owner, repo, sha, ref)
		r.reachCache.put(cacheKey, reachCacheEntry{status: result.Status, detail: result.Detail})
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
			r.reachCache.put(cacheKey, reachCacheEntry{status: result.Status, detail: result.Detail})
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
		if lockfile.IsFullSha(ref) {
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
		if lockfile.IsFullSha(ref) {
			result.Detail = "pinned to a bare SHA; commit is NOT on any branch — possible fork-network commit"
		} else {
			result.Detail = fmt.Sprintf("commit %s not found on any branch of %s/%s — possible fork-network injection",
				shortSha(sha), owner, repo)
		}
	}

	r.reachCache.put(cacheKey, reachCacheEntry{status: result.Status, detail: result.Detail})
	return result
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
	hintBranch, _ := r.branchHintBySHA.get(cachekey.ForNWOSha(owner, repo, sha))
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

// CheckReachabilityAll runs reachability checks on a batch of dependencies,
// deduplicating by owner/repo/sha/ref.
func (r *Resolver) CheckReachabilityAll(deps []lockfile.Dependency) []ReachabilityResult {
	// Reachability dedup keys: lowercased SHAs are fine because the API
	// returns canonical lowercase, and ref preserves case so the same SHA at
	// "v6" vs "V6" stays distinct.
	seenReach := make(map[cachekey.Reach]bool)

	// Pre-filter to the unique deps we'll actually check so progress can be
	// reported as [i/N] against a stable total.
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
	// uniqueRepos tracks distinct owner/repo pairs we still need to warm. We
	// keep the original strings alongside the typed key so the warmup loop
	// can issue API calls without re-parsing.
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
		for _, re := range uniqueRepos {
			warmupWG.Add(1)
			slot := <-warmupSlots
			go func(re repoEntry, slot int) {
				defer warmupWG.Done()
				defer func() { warmupSlots <- slot }()
				rk := re.owner + "/" + re.repo
				if r.WorkerProgressFn != nil {
					r.WorkerProgressFn(slot, "→ loading "+rk)
				}
				_, _ = r.listBranches(re.owner, re.repo)
				_ = r.getDefaultBranch(re.owner, re.repo)
				if r.WorkerProgressFn != nil {
					r.WorkerProgressFn(slot, "")
				}
			}(re, slot)
		}
		warmupWG.Wait()
	}

	// Fan out the per-dependency checks with a bounded worker pool. Each dep is
	// independent; per-cache locks serialize map access and progress reporting
	// is serialized via progressMu, so results[i] is the only unsynchronized
	// write and each goroutine owns a distinct index. Each goroutine also owns
	// a stable slot index (0..limit-1) for the per-worker UI display.
	results := make([]ReachabilityResult, total)
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

// listBranches returns all branches with their HEAD SHAs for a repo, using
// the documented REST API (GET /repos/{owner}/{repo}/branches). Results are
// cached per owner/repo. Paginates up to 3 pages (300 branches) to bound
// the number of API calls.
func (r *Resolver) listBranches(owner, repo string) ([]branchHead, error) {
	key := cachekey.ForRepo(owner, repo)
	if cached, ok := r.branchListCache.get(key); ok {
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
	r.branchListCache.put(key, all)
	return all, nil
}

// listTagsForRepo returns all tags with their commit SHAs for a repo, using
// the documented REST API (GET /repos/{owner}/{repo}/tags). Results are
// cached per owner/repo.
func (r *Resolver) listTagsForRepo(owner, repo string) ([]tagEntry, error) {
	key := cachekey.ForRepo(owner, repo)
	if cached, ok := r.tagListCache.get(key); ok {
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
	r.tagListCache.put(key, tags)
	return tags, nil
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
	key := cachekey.ForRepo(owner, repo)
	if name, ok := r.defaultBranchCache.get(key); ok {
		return name
	}
	var resp struct {
		DefaultBranch string `json:"default_branch"`
	}
	path := fmt.Sprintf("repos/%s/%s", url.PathEscape(owner), url.PathEscape(repo))
	if err := r.restClient.Get(path, &resp); err != nil {
		r.defaultBranchCache.put(key, "")
		return ""
	}
	r.defaultBranchCache.put(key, resp.DefaultBranch)
	return resp.DefaultBranch
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
	key := cachekey.ForNWOName(owner, repo, name)
	if bh, ok := r.namedBranchCache.get(key); ok {
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
			r.namedBranchCache.put(key, branchHead{}) // negative cache
		}
		return branchHead{}, false
	}
	// A prefix (non-exact) match returns an array, decoding to an empty
	// object here; treat that as "not found" rather than guessing.
	if resp.Object.SHA == "" {
		return branchHead{}, false
	}
	bh := branchHead{Name: name, SHA: resp.Object.SHA}
	r.namedBranchCache.put(key, bh)
	return bh, true
}

// listProtectedBranches returns the repo's protected branches via
// GET /repos/{owner}/{repo}/branches?protected=true. Best-effort: any error
// yields whatever was collected so far (possibly empty). Cached per owner/repo.
func (r *Resolver) listProtectedBranches(owner, repo string) []branchHead {
	key := cachekey.ForRepo(owner, repo)
	if cached, ok := r.protectedBranchCache.get(key); ok {
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
	r.protectedBranchCache.put(key, all)
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
	key := cachekey.ForRepo(owner, repo)
	if cached, ok := r.releaseBranchCache.get(key); ok {
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
	r.releaseBranchCache.put(key, all)
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
	if ref != "" && !lockfile.IsFullSha(ref) {
		addNamed(ref)
	}
	hint, _ := r.branchHintBySHA.get(cachekey.ForNWOSha(owner, repo, sha))
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
	hintBranch, _ := r.branchHintBySHA.get(cachekey.ForNWOSha(owner, repo, sha))
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
