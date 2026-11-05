package resolve

import (
	"context"
	"fmt"
	"slices"
	"strings"

	parserlock "github.com/github/actions-lockfile/go/pkg/lockfile"
	"github.com/github/gh-actions-lock/internal/dep"
	"github.com/github/gh-actions-lock/internal/ghapi"
)

// DiscoverContaining returns (tag, branch) for sha in owner/repo. Results
// are cached for the lifetime of the resolver.
//
// Tags are checked first: when a recordable tag points at sha, it is
// returned with an empty branch and no branch search runs. The tag is what
// gets recorded, and the branch search can cost one compare call per branch
// (see #267). If hintRef is one of the tags it wins; otherwise the first tag
// (lexicographic) is picked.
//
// With no tag, a containing branch is searched for (hintRef → default →
// protected → all). Both are empty when neither exists.
//
// hintRef may be empty (e.g. for bare-SHA pins). The repo's default branch
// is discovered automatically via GET /repos/{owner}/{repo} (cached).
func (r *Resolver) DiscoverContaining(ctx context.Context, owner, repo, sha, hintRef string) (tag, branch string, err error) {
	return r.DiscoverContainingDefault(ctx, owner, repo, sha, hintRef, r.GetDefaultBranch(ctx, owner, repo))
}

// DiscoverContainingDefault is DiscoverContaining with an explicit hint at
// the repository's default branch (e.g. "main"). When the discovered branch
// set contains defaultBranch it is preferred over lexicographic ordering.
//
// The branch search, run only when no tag points at sha, is two-phase.
// Phase 1 validates the likely/canonical set directly (literal ref, recorded
// hint branch, default branch, protected branches, release/v* branches) so a
// relevant branch is never missed because it sorts beyond the paginated
// listing cap. Phase 2 — a full protected-first then all-branches scan — runs
// only when phase 1 finds nothing.
func (r *Resolver) DiscoverContainingDefault(ctx context.Context, owner, repo, sha, hintRef, defaultBranch string) (tag, branch string, err error) {
	c := refCandidates{r: r, owner: owner, repo: repo}
	tags, err := c.tagsAt(ctx, sha)
	if err != nil {
		return "", "", err
	}
	if len(tags) > 0 {
		return pickPreferredTag(tags, hintRef), "", nil
	}
	branch, err = c.containingBranch(ctx, sha, hintRef, defaultBranch)
	if err != nil {
		return "", "", err
	}
	return "", branch, nil
}

// refCandidates answers "which tag or branch could the lockfile record for
// this commit": a name the Actions backend accepts that, looked up the way
// the backend looks it up (tag first, then branch), reaches the commit.
// Every reverse-lookup candidate goes through it.
type refCandidates struct {
	r           *Resolver
	owner, repo string
}

// tagsAt returns the recordable tags pointing at sha. The listing's first
// page is cached and shared; when it has none of them, every later page is
// read before falling back to the slower branch search.
func (c refCandidates) tagsAt(ctx context.Context, sha string) ([]string, error) {
	first, err := c.r.ListTagsForRepo(ctx, c.owner, c.repo)
	if err != nil {
		return nil, err
	}
	recordable := func(names []string) []string {
		return slices.DeleteFunc(names, func(n string) bool { return !ghapi.RecordableRefName(n) })
	}
	var names []string
	for _, t := range first {
		if strings.EqualFold(t.SHA, sha) {
			names = append(names, t.Name)
		}
	}
	// Filter before deciding to read more: an unrecordable tag on the first
	// page must not stop the search for a recordable one.
	names = recordable(names)
	if len(names) == 0 && len(first) >= 100 {
		all, err := c.r.gh.TagsAtCommit(ctx, c.owner, c.repo, sha)
		if err != nil {
			return nil, err
		}
		names = recordable(all)
	}
	return names, nil
}

// RefNamesCommit reports whether `uses: owner/repo@name` would run exactly
// sha: name is recordable and, looked up the way the Actions backend looks
// it up (tag first, then branch), points at sha now. Repairing a full-SHA
// `uses:` to a recorded ref goes through it, whether that ref was recorded
// as a tag or a branch.
func (r *Resolver) RefNamesCommit(ctx context.Context, owner, repo, name, sha string) (bool, error) {
	return refCandidates{r: r, owner: owner, repo: repo}.namesCommit(ctx, name, sha)
}

func (c refCandidates) namesCommit(ctx context.Context, name, sha string) (bool, error) {
	if !ghapi.RecordableRefName(name) {
		return false, nil
	}
	t, err := c.r.gh.LookupRefName(ctx, c.owner, c.repo, name)
	return err == nil && t.Kind != ghapi.NoRef && strings.EqualFold(t.Commit, sha), err
}

// containingBranch returns the first recordable branch containing sha in
// the phased search order. A branch whose name is also a tag resolves to
// the tag, so it is checked by exact tag name and the search continues
// without it.
func (c refCandidates) containingBranch(ctx context.Context, sha, hintRef, defaultBranch string) (string, error) {
	shadowed := map[string]bool{}
	for {
		branch, err := c.r.searchContainingBranch(ctx, c.owner, c.repo, sha, hintRef, defaultBranch, shadowed)
		if err != nil || branch == "" {
			return branch, err
		}
		isTag, err := c.r.gh.TagExists(ctx, c.owner, c.repo, branch)
		if err != nil {
			return "", err
		}
		if !isTag {
			return branch, nil
		}
		shadowed[branch] = true
	}
}

// searchContainingBranch runs the phased branch search, skipping branches
// named in skip and names the Actions backend doesn't accept.
func (r *Resolver) searchContainingBranch(ctx context.Context, owner, repo, sha, hintRef, defaultBranch string, skip map[string]bool) (string, error) {
	without := func(cands []ghapi.BranchHead) []ghapi.BranchHead {
		out := cands[:0:0]
		for _, b := range cands {
			if !skip[b.Name] && ghapi.RecordableRefName(b.Name) {
				out = append(out, b)
			}
		}
		return out
	}

	// Phase 0: check named branches directly (ref, hint, default) — one
	// REST call each, no pagination.
	named := without(r.namedBranches(ctx, owner, repo, sha, hintRef, defaultBranch))
	branch, err := r.findContainingBranch(ctx, owner, repo, sha, hintRef, defaultBranch, named)
	if err != nil || branch != "" {
		return branch, err
	}

	// Phase 1: canonical "likely" branches (protected + release), fetched
	// only when the cheap named-branch check didn't find a match.
	likely := r.LikelyBranches(ctx, owner, repo, sha, hintRef, defaultBranch)
	namedSeen := make(map[string]bool, len(named))
	for _, b := range named {
		namedSeen[b.Name] = true
	}
	var remaining []ghapi.BranchHead
	for _, b := range likely {
		if !namedSeen[b.Name] {
			remaining = append(remaining, b)
		}
	}
	branch, err = r.findContainingBranch(ctx, owner, repo, sha, hintRef, defaultBranch, without(remaining))
	if err != nil || branch != "" {
		return branch, err
	}

	// Phase 2: full paginated scan, only on a phase-1 miss.
	branches, err := r.ListBranches(ctx, owner, repo)
	if err != nil {
		return "", err
	}
	branches = without(branches)
	protectedBranches := make([]ghapi.BranchHead, 0, len(branches))
	for _, b := range branches {
		if b.Protected {
			protectedBranches = append(protectedBranches, b)
		}
	}
	branch, err = r.findContainingBranch(ctx, owner, repo, sha, hintRef, defaultBranch, protectedBranches)
	if err != nil || branch != "" {
		return branch, err
	}
	return r.findContainingBranch(ctx, owner, repo, sha, hintRef, defaultBranch, branches)
}

// findContainingBranch returns the name of the first branch (from the given
// candidate set, in OrderedBranches tier order) whose HEAD is sha or which
// contains sha as an ancestor via the Compare API. Returns "" if none
// match. Returns a non-nil error only for unexpected transport failures
// from the Compare API (404/422 are treated as "not contained").
func (r *Resolver) findContainingBranch(ctx context.Context, owner, repo, sha, hintRef, defaultBranch string, candidates []ghapi.BranchHead) (string, error) {
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
	hintBranch, _ := r.branchHintBySHA.Get(ghapi.ForNWOSha(owner, repo, sha))
	for _, b := range ghapi.OrderedBranches(candidates, hintBranch, hintRef, defaultBranch) {
		ok, err := r.gh.CompareCommits(ctx, owner, repo, sha, b.SHA)
		if err != nil {
			return "", fmt.Errorf("comparing %s with %s/%s branch %s: %w",
				parserlock.ShortSHA(sha), owner, repo, b.Name, err)
		}
		if ok {
			return b.Name, nil
		}
	}
	return "", nil
}

// ListReleaseBranches returns release/v* branches (the canonical action
// publication branches) by matching heads/v and heads/release. Best-effort;
// cached per owner/repo.
func (r *Resolver) ListReleaseBranches(ctx context.Context, owner, repo string) []ghapi.BranchHead {
	key := ghapi.ForRepo(owner, repo)
	if cached, ok := r.releaseBranchCache.Get(key); ok {
		return cached
	}
	sfKey := owner + "/" + repo
	v, _, _ := r.releaseBranchSF.Do(sfKey, func() (any, error) {
		if cached, ok := r.releaseBranchCache.Get(key); ok {
			return cached, nil
		}
		seen := make(map[string]bool)
		var all []ghapi.BranchHead
		for _, prefix := range []string{"v", "release"} {
			for _, bh := range r.MatchingHeadRefs(ctx, owner, repo, prefix) {
				if bh.Name == "" || seen[bh.Name] {
					continue
				}
				seen[bh.Name] = true
				all = append(all, bh)
			}
		}
		r.releaseBranchCache.Put(key, all)
		return all, nil
	})
	if v == nil {
		return nil
	}
	return v.([]ghapi.BranchHead)
}

// namedBranches returns the cheapest candidates: the literal ref branch,
// the recorded hint branch, and the default branch. Each costs a single
// REST call, no pagination.
func (r *Resolver) namedBranches(ctx context.Context, owner, repo, sha, ref, defaultBranch string) []ghapi.BranchHead {
	seen := make(map[string]bool)
	var out []ghapi.BranchHead
	add := func(name string) {
		if name == "" || seen[name] {
			return
		}
		if bh, ok := r.GetBranchHead(ctx, owner, repo, name); ok {
			seen[name] = true
			out = append(out, bh)
		}
	}
	if ref != "" && !parserlock.IsFullSha(ref) {
		add(ref)
	}
	hint, _ := r.branchHintBySHA.Get(ghapi.ForNWOSha(owner, repo, sha))
	add(hint)
	add(defaultBranch)
	return out
}

// LikelyBranches assembles the high-trust candidate set validated before any
// full branch scan: the literal ref (when it is a branch), the recorded
// lockfile hint branch, the default branch, protected branches and
// release/v* branches. Deduplicated by name; order is most-trusted first.
func (r *Resolver) LikelyBranches(ctx context.Context, owner, repo, sha, ref, defaultBranch string) []ghapi.BranchHead {
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
	hint, _ := r.branchHintBySHA.Get(ghapi.ForNWOSha(owner, repo, sha))
	addNamed(hint)
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

// LookupIssue describes a single dep that ReverseLookup could not resolve.
type LookupIssue struct {
	Index   int    // position in the deps slice
	NWO     string // owner/repo
	Ref     string // original ref
	SHA     string // commit hash
	Message string // human-readable reason
}

// ReverseLookup leaves symbolic refs unchanged and reverse-lookups bare SHA
// refs via DiscoverContaining. For bare SHAs it populates dep.Tag and dep.Branch
// and computes the canonical @ref. Exact tags take precedence over branch
// heads. A containing branch remains metadata when its head differs from SHA,
// so historical pins on the same branch keep distinct refs.
// When the canonical ref differs from dep.Ref the change is recorded in
// the returned rewrites map and dep.Ref is updated in place.
//
// Bare SHA deps with no containing ref are skipped and reported in the
// returned issues slice.
// Only transient/API errors are returned as err.
func (r *Resolver) ReverseLookup(ctx context.Context, deps []dep.Dependency) (map[string]string, []LookupIssue, error) {
	rewrites := map[string]string{}
	var issues []LookupIssue
	for i := range deps {
		d := &deps[i]
		owner, repo := d.OwnerRepo()
		if owner == "" || repo == "" {
			continue
		}
		if !LooksLikeSHA(d.Ref) {
			continue
		}
		tag, branch, err := r.DiscoverContaining(ctx, owner, repo, d.SHA, d.Ref)
		if err != nil {
			return nil, nil, fmt.Errorf("%s@%s: %w", d.NWO, d.Ref, err)
		}
		if tag == "" && branch == "" {
			var msg string
			if LooksLikeSHA(d.Ref) {
				msg = "this commit has no exact tag and is not on any branch. Update the workflow to use a current upstream tag or branch, then rerun"
			} else {
				msg = fmt.Sprintf("commit %s is not reachable from any ref (tag or branch) — orphaned commit",
					parserlock.ShortSHA(d.SHA))
			}
			issues = append(issues, LookupIssue{
				Index:   i,
				NWO:     d.NWO,
				Ref:     d.Ref,
				SHA:     d.SHA,
				Message: msg,
			})
			continue
		}
		d.Tag = tag
		d.Branch = branch
		newRef := tag
		if branch == d.Ref {
			newRef = branch
		}
		if newRef == "" {
			if head, ok := r.GetBranchHead(ctx, owner, repo, branch); ok && strings.EqualFold(head.SHA, d.SHA) {
				newRef = branch
			}
		}
		// An entry may record a ref containing @, but `uses:` can't name
		// one, so such a use keeps its SHA.
		if newRef == "" || newRef == d.Ref || strings.Contains(newRef, "@") {
			continue
		}
		rewrites[d.NWO+"@"+d.Ref] = d.NWO + "@" + newRef
		d.Ref = newRef
	}
	return rewrites, issues, nil
}

// LooksLikeSHA returns true when ref is a hex string of SHA-1 (40) or
// SHA-256 (64) length — i.e. the user wrote a bare commit hash.
func LooksLikeSHA(ref string) bool {
	n := len(ref)
	if n != 40 && n != 64 {
		return false
	}
	for _, c := range ref {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
			return false
		}
	}
	return true
}
