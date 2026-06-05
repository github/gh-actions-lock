package resolver

import (
	"fmt"
	"net/url"

	"github.com/github/gh-actions-pin/internal/cachekey"
)

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

// tagPeel records the outcome of a PeelTagObject lookup so repeated checks
// for the same SHA avoid additional API calls.
type tagPeel struct {
	commit string // commit the tag object peels to (empty when isTag is false)
	isTag  bool   // true when the SHA is an annotated tag object
}

// reachCacheEntry stores both the verdict and the human-readable detail so
// re-reads (e.g. across pre-warm + per-workflow phases) carry the original
// rationale instead of a generic "cached" placeholder.
type reachCacheEntry struct {
	status ReachabilityStatus
	detail string
}

// setReachCache stores a reachability verdict under cacheMu.
func (r *Resolver) setReachCache(key cachekey.Reach, status ReachabilityStatus, detail string) {
	r.cacheMu.Lock()
	r.reachCache[key] = reachCacheEntry{status: status, detail: detail}
	r.cacheMu.Unlock()
}

// setCompareCache stores a Compare verdict under cacheMu.
func (r *Resolver) setCompareCache(key cachekey.Compare, contains bool) {
	r.cacheMu.Lock()
	r.compareCache[key] = contains
	r.cacheMu.Unlock()
}

// setDefaultBranchCache stores a default-branch lookup under cacheMu.
func (r *Resolver) setDefaultBranchCache(key cachekey.Repo, name string) {
	r.cacheMu.Lock()
	r.defaultBranchCache[key] = name
	r.cacheMu.Unlock()
}

// setNamedBranch stores a single-branch lookup under cacheMu.
func (r *Resolver) setNamedBranch(key cachekey.NWOName, bh branchHead) {
	r.cacheMu.Lock()
	r.namedBranchCache[key] = bh
	r.cacheMu.Unlock()
}

// RepoIDs returns the numeric owner ID and repo ID for a NWO, querying
// the GitHub REST API on cache miss. Results are cached for the lifetime of
// the resolver.
func (r *Resolver) RepoIDs(owner, repo string) (int64, int64, error) {
	key := cachekey.ForRepo(owner, repo)
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
