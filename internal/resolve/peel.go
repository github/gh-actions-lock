package resolve

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"

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
func (r *Resolver) PeelTagObject(ctx context.Context, owner, repo, sha string) (commit string, ok bool) {
	return r.peelTagObject(ctx, owner, repo, sha)
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

func (r *Resolver) peelTagObject(ctx context.Context, owner, repo, sha string) (string, bool) {
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
	if err := r.client.DoWithContext(ctx, tagObjectPeelQuery, vars, &resp); err != nil {
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

// ListBranches returns all branches with their HEAD SHAs for a repo, using
// the documented REST API (GET /repos/{owner}/{repo}/branches). Results are
// cached per owner/repo. Paginates up to 3 pages (300 branches) to bound
// the number of API calls.
func (r *Resolver) ListBranches(ctx context.Context, owner, repo string) ([]BranchHead, error) {
	key := cachekey.ForRepo(owner, repo)
	if cached, ok := r.branchListCache.get(key); ok {
		return cached, nil
	}
	var all []BranchHead
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
		if err := r.restClient.DoWithContext(ctx, http.MethodGet, path, nil, &resp); err != nil {
			return nil, fmt.Errorf("listing branches for %s/%s: %w", owner, repo, err)
		}
		for _, b := range resp {
			all = append(all, BranchHead{Name: b.Name, SHA: b.Commit.SHA, Protected: b.Protected})
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
func (r *Resolver) ListTagsForRepo(ctx context.Context, owner, repo string) ([]TagEntry, error) {
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
	if err := r.restClient.DoWithContext(ctx, http.MethodGet, path, nil, &resp); err != nil {
		return nil, fmt.Errorf("listing tags for %s/%s: %w", owner, repo, err)
	}
	tags := make([]TagEntry, 0, len(resp))
	for _, t := range resp {
		tags = append(tags, TagEntry{Name: t.Name, SHA: t.Commit.SHA})
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
func OrderedBranches(branches []BranchHead, hintBranch, hintRef, defaultBranch string) []BranchHead {
	priority := make(map[string]bool)
	var result []BranchHead
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
	protected := make([]BranchHead, 0, len(branches))
	unprotected := make([]BranchHead, 0, len(branches))
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
func (r *Resolver) GetDefaultBranch(ctx context.Context, owner, repo string) string {
	key := cachekey.ForRepo(owner, repo)
	if name, ok := r.defaultBranchCache.get(key); ok {
		return name
	}
	var resp struct {
		DefaultBranch string `json:"default_branch"`
	}
	path := fmt.Sprintf("repos/%s/%s", url.PathEscape(owner), url.PathEscape(repo))
	if err := r.restClient.DoWithContext(ctx, http.MethodGet, path, nil, &resp); err != nil {
		r.defaultBranchCache.put(key, "")
		return ""
	}
	r.defaultBranchCache.put(key, resp.DefaultBranch)
	return resp.DefaultBranch
}

// escapeBranchPath percent-escapes each slash-delimited segment of a ref name
// while preserving the slashes themselves, so names like "releases/v4" form a
// valid git/ref path.
func EscapeBranchPath(name string) string {
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
func (r *Resolver) GetBranchHead(ctx context.Context, owner, repo, name string) (BranchHead, bool) {
	if name == "" {
		return BranchHead{}, false
	}
	key := cachekey.ForNWOName(owner, repo, name)
	if bh, ok := r.namedBranchCache.get(key); ok {
		return bh, bh.Name != ""
	}
	path := fmt.Sprintf("repos/%s/%s/git/ref/heads/%s",
		url.PathEscape(owner), url.PathEscape(repo), EscapeBranchPath(name))
	var resp struct {
		Ref    string `json:"ref"`
		Object struct {
			SHA  string `json:"sha"`
			Type string `json:"type"`
		} `json:"object"`
	}
	if err := r.restClient.DoWithContext(ctx, http.MethodGet, path, nil, &resp); err != nil {
		var httpErr *api.HTTPError
		if errors.As(err, &httpErr) && httpErr.StatusCode == http.StatusNotFound {
			r.namedBranchCache.put(key, BranchHead{}) // negative cache
		}
		return BranchHead{}, false
	}
	// A prefix (non-exact) match returns an array, decoding to an empty
	// object here; treat that as "not found" rather than guessing.
	if resp.Object.SHA == "" {
		return BranchHead{}, false
	}
	bh := BranchHead{Name: name, SHA: resp.Object.SHA}
	r.namedBranchCache.put(key, bh)
	return bh, true
}

// listProtectedBranches returns the repo's protected branches via
// GET /repos/{owner}/{repo}/branches?protected=true. Best-effort: any error
// yields whatever was collected so far (possibly empty). Cached per owner/repo.
func (r *Resolver) ListProtectedBranches(ctx context.Context, owner, repo string) []BranchHead {
	key := cachekey.ForRepo(owner, repo)
	if cached, ok := r.protectedBranchCache.get(key); ok {
		return cached
	}
	var all []BranchHead
	for page := 1; page <= 3; page++ {
		path := fmt.Sprintf("repos/%s/%s/branches?protected=true&per_page=100&page=%d",
			url.PathEscape(owner), url.PathEscape(repo), page)
		var resp []struct {
			Name   string `json:"name"`
			Commit struct {
				SHA string `json:"sha"`
			} `json:"commit"`
		}
		if err := r.restClient.DoWithContext(ctx, http.MethodGet, path, nil, &resp); err != nil {
			break // best-effort
		}
		for _, b := range resp {
			all = append(all, BranchHead{Name: b.Name, SHA: b.Commit.SHA, Protected: true})
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
func (r *Resolver) MatchingHeadRefs(ctx context.Context, owner, repo, prefix string) []BranchHead {
	path := fmt.Sprintf("repos/%s/%s/git/matching-refs/heads/%s",
		url.PathEscape(owner), url.PathEscape(repo), EscapeBranchPath(prefix))
	var resp []struct {
		Ref    string `json:"ref"`
		Object struct {
			SHA  string `json:"sha"`
			Type string `json:"type"`
		} `json:"object"`
	}
	if err := r.restClient.DoWithContext(ctx, http.MethodGet, path, nil, &resp); err != nil {
		return nil // best-effort
	}
	var out []BranchHead
	for _, ref := range resp {
		name := strings.TrimPrefix(ref.Ref, "refs/heads/")
		if name == ref.Ref || name == "" || ref.Object.SHA == "" {
			continue
		}
		out = append(out, BranchHead{Name: name, SHA: ref.Object.SHA})
	}
	return out
}

// listReleaseBranches returns release/v* branches (the canonical action
// publication branches) by matching heads/v and heads/release. Best-effort;
// cached per owner/repo.
func (r *Resolver) ListReleaseBranches(ctx context.Context, owner, repo string) []BranchHead {
	key := cachekey.ForRepo(owner, repo)
	if cached, ok := r.releaseBranchCache.get(key); ok {
		return cached
	}
	seen := make(map[string]bool)
	var all []BranchHead
	for _, prefix := range []string{"v", "release"} {
		for _, bh := range r.MatchingHeadRefs(ctx, owner, repo, prefix) {
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
func (r *Resolver) LikelyBranches(ctx context.Context, owner, repo, sha, ref, defaultBranch string) []BranchHead {
	seen := make(map[string]bool)
	var out []BranchHead
	addNamed := func(name string) {
		if name == "" || seen[name] {
			return
		}
		if bh, ok := r.GetBranchHead(ctx, owner, repo, name); ok {
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
func (r *Resolver) DiscoverContaining(ctx context.Context, owner, repo, sha, hintRef string) (tag, branch string, err error) {
	return r.DiscoverContainingDefault(ctx, owner, repo, sha, hintRef, r.GetDefaultBranch(ctx, owner, repo))
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
func (r *Resolver) DiscoverContainingDefault(ctx context.Context, owner, repo, sha, hintRef, defaultBranch string) (tag, branch string, err error) {
	// Phase 1: canonical "likely" branches, fetched directly.
	likely := r.LikelyBranches(ctx, owner, repo, sha, hintRef, defaultBranch)
	branch, err = r.findContainingBranch(ctx, owner, repo, sha, hintRef, defaultBranch, likely)
	if err != nil {
		return "", "", err
	}

	// Phase 2: full paginated scan, only on a phase-1 miss.
	if branch == "" {
		branches, lerr := r.ListBranches(ctx, owner, repo)
		if lerr != nil {
			return "", "", lerr
		}
		protectedBranches := make([]BranchHead, 0, len(branches))
		for _, b := range branches {
			if b.Protected {
				protectedBranches = append(protectedBranches, b)
			}
		}
		branch, err = r.findContainingBranch(ctx, owner, repo, sha, hintRef, defaultBranch, protectedBranches)
		if err != nil {
			return "", "", err
		}
		if branch == "" {
			branch, err = r.findContainingBranch(ctx, owner, repo, sha, hintRef, defaultBranch, branches)
			if err != nil {
				return "", "", err
			}
		}
	}

	if branch == "" {
		return "", "", &ImpostorError{NWO: owner + "/" + repo, Ref: hintRef, SHA: sha}
	}

	// Discover tags pointing at sha.
	allTags, err := r.ListTagsForRepo(ctx, owner, repo)
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
func (r *Resolver) findContainingBranch(ctx context.Context, owner, repo, sha, hintRef, defaultBranch string, candidates []BranchHead) (string, error) {
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
	for _, b := range OrderedBranches(candidates, hintBranch, hintRef, defaultBranch) {
		r.Progress("scanning %s/%s branch %s", owner, repo, b.Name)
		ok, err := r.BranchContainsCommit(ctx, owner, repo, sha, b.SHA)
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
func (r *Resolver) NormalizeContaining(ctx context.Context, deps []lockfile.Dependency) (map[string]string, error) {
	rewrites := map[string]string{}
	for i := range deps {
		d := &deps[i]
		owner, repo := d.OwnerRepo()
		if owner == "" || repo == "" {
			continue
		}
		r.Progress("resolving %s@%s", d.NWO, d.Ref)
		tag, branch, err := r.DiscoverContaining(ctx, owner, repo, d.SHA, d.Ref)
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
