package resolver

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"sync"

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

// syncMap is a lock-paired map keyed by K. Each cache on Resolver owns one
// instance so the lock and the data it protects sit together at the
// declaration site — every callsite reads a single mutex that guards a
// single map. The zero value is usable; the underlying map is allocated on
// first put so partial composite literals (common in tests) don't panic.
type syncMap[K comparable, V any] struct {
	mu sync.Mutex
	m  map[K]V
}

func (c *syncMap[K, V]) get(k K) (V, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	v, ok := c.m[k]
	return v, ok
}

func (c *syncMap[K, V]) put(k K, v V) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m == nil {
		c.m = make(map[K]V)
	}
	c.m[k] = v
}

// RepoIDs returns the numeric owner ID and repo ID for a NWO, querying
// the GitHub REST API on cache miss. Results are cached for the lifetime of
// the resolver.
func (r *Resolver) RepoIDs(ctx context.Context, owner, repo string) (int64, int64, error) {
	key := cachekey.ForRepo(owner, repo)
	if ids, ok := r.repoIDsCache.get(key); ok {
		return ids[0], ids[1], nil
	}
	var resp struct {
		ID    int64 `json:"id"`
		Owner struct {
			ID int64 `json:"id"`
		} `json:"owner"`
	}
	path := fmt.Sprintf("repos/%s/%s", url.PathEscape(owner), url.PathEscape(repo))
	if err := r.restClient.DoWithContext(ctx, http.MethodGet, path, nil, &resp); err != nil {
		return 0, 0, fmt.Errorf("fetching %s: %w", path, err)
	}
	if resp.ID == 0 || resp.Owner.ID == 0 {
		return 0, 0, fmt.Errorf("%s returned zero IDs (owner=%d repo=%d)", path, resp.Owner.ID, resp.ID)
	}
	r.repoIDsCache.put(key, [2]int64{resp.Owner.ID, resp.ID})
	return resp.Owner.ID, resp.ID, nil
}
