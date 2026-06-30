package ghapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"

	"github.com/cli/go-gh/v2/pkg/api"
)

// BranchHead holds a branch name, the SHA of its HEAD commit, and whether
// the branch has branch-protection rules enabled in the upstream repo.
type BranchHead struct {
	Name      string
	SHA       string
	Protected bool
}

// TagEntry holds a tag name and the commit SHA it points at.
type TagEntry struct {
	Name string
	SHA  string
}

// ListBranches returns all branches with their HEAD SHAs for a repo.
// Paginates up to 3 pages (300 branches). Results are cached per owner/repo
// and coalesced via singleflight.
func (c *Client) ListBranches(ctx context.Context, owner, repo string) ([]BranchHead, error) {
	key := ForRepo(owner, repo)
	if cached, ok := c.branchListCache.Get(key); ok {
		return cached, nil
	}
	sfKey := key.String()
	v, err, _ := c.branchListSF.Do(sfKey, func() (any, error) {
		if cached, ok := c.branchListCache.Get(key); ok {
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
			if err := c.rest.DoWithContext(ctx, http.MethodGet, path, nil, &resp); err != nil {
				if IsSAMLEnforcement(err) && SSOFallbackEligible(owner) {
					return c.anonListBranches(ctx, owner, repo)
				}
				return nil, fmt.Errorf("listing branches for %s/%s: %w", owner, repo, err)
			}
			for _, b := range resp {
				all = append(all, BranchHead{Name: b.Name, SHA: b.Commit.SHA, Protected: b.Protected})
			}
			if len(resp) < 100 {
				break
			}
		}
		c.branchListCache.Put(key, all)
		return all, nil
	})
	if err != nil {
		return nil, err
	}
	return v.([]BranchHead), nil
}

// ListTags returns all tags with their commit SHAs for a repo (first page,
// up to 100). Results are cached per owner/repo and coalesced via singleflight.
func (c *Client) ListTags(ctx context.Context, owner, repo string) ([]TagEntry, error) {
	key := ForRepo(owner, repo)
	if cached, ok := c.tagListCache.Get(key); ok {
		return cached, nil
	}
	sfKey := key.String()
	type result struct {
		tags []TagEntry
		err  error
	}
	v, _, _ := c.tagListSF.Do(sfKey, func() (any, error) {
		if cached, ok := c.tagListCache.Get(key); ok {
			return result{tags: cached}, nil
		}
		path := fmt.Sprintf("repos/%s/%s/tags?per_page=100",
			url.PathEscape(owner), url.PathEscape(repo))
		var resp []struct {
			Name   string `json:"name"`
			Commit struct {
				SHA string `json:"sha"`
			} `json:"commit"`
		}
		if err := c.rest.DoWithContext(ctx, http.MethodGet, path, nil, &resp); err != nil {
			if IsSAMLEnforcement(err) && SSOFallbackEligible(owner) {
				tags, anonErr := c.anonListTags(ctx, owner, repo)
				if anonErr != nil {
					return result{err: anonErr}, nil
				}
				c.tagListCache.Put(key, tags)
				return result{tags: tags}, nil
			}
			return result{err: fmt.Errorf("listing tags for %s/%s: %w", owner, repo, err)}, nil
		}
		tags := make([]TagEntry, 0, len(resp))
		for _, t := range resp {
			tags = append(tags, TagEntry{Name: t.Name, SHA: t.Commit.SHA})
		}
		c.tagListCache.Put(key, tags)
		return result{tags: tags}, nil
	})
	res := v.(result)
	return res.tags, res.err
}

// repoMeta is the subset of repos/{owner}/{repo} the tool needs: the default
// branch, the numeric owner and repo IDs (lockfile write), and the visibility
// and last-push time (tag freshness/immutability checks).
type repoMeta struct {
	DefaultBranch string
	OwnerID       int64
	RepoID        int64
	Visibility    string
	PushedAt      string
}

// repoMetadata fetches repos/{owner}/{repo} at most once per run, coalescing
// concurrent callers via singleflight and caching the result. GetDefaultBranch,
// RepoIDs, and RepoMetadata all derive from it, so a repo costs one round-trip
// instead of one per consumer. The request runs under a cancel-free context:
// callers fan out under scan/errgroup contexts that cancel on first
// match/error, and a coalesced caller's cancellation must not abort the shared
// fetch for the others waiting on it.
func (c *Client) repoMetadata(ctx context.Context, owner, repo string) (repoMeta, error) {
	key := ForRepo(owner, repo)
	if m, ok := c.repoMetaCache.Get(key); ok {
		return m, nil
	}
	v, err, _ := c.repoMetaSF.Do(key.String(), func() (any, error) {
		if m, ok := c.repoMetaCache.Get(key); ok {
			return m, nil
		}
		var resp struct {
			DefaultBranch string `json:"default_branch"`
			Visibility    string `json:"visibility"`
			PushedAt      string `json:"pushed_at"`
			ID            int64  `json:"id"`
			Owner         struct {
				ID int64 `json:"id"`
			} `json:"owner"`
		}
		path := fmt.Sprintf("repos/%s/%s", url.PathEscape(owner), url.PathEscape(repo))
		if err := c.rest.DoWithContext(ctx, http.MethodGet, path, nil, &resp); err != nil {
			if IsSAMLEnforcement(err) && SSOFallbackEligible(owner) {
				if anonErr := c.anonGet(ctx, path, &resp); anonErr != nil {
					return repoMeta{}, fmt.Errorf("anonymous fallback fetching %s: %w", path, anonErr)
				}
			} else {
				return repoMeta{}, fmt.Errorf("fetching %s: %w", path, err)
			}
		}
		m := repoMeta{
			DefaultBranch: resp.DefaultBranch,
			OwnerID:       resp.Owner.ID,
			RepoID:        resp.ID,
			Visibility:    resp.Visibility,
			PushedAt:      resp.PushedAt,
		}
		c.repoMetaCache.Put(key, m)
		return m, nil
	})
	if err != nil {
		return repoMeta{}, err
	}
	return v.(repoMeta), nil
}

// GetDefaultBranch returns the repo's default branch name (e.g. "main"), or
// "" if the lookup fails. Backed by the shared repoMetadata fetch.
func (c *Client) GetDefaultBranch(ctx context.Context, owner, repo string) string {
	m, err := c.repoMetadata(ctx, owner, repo)
	if err != nil {
		return ""
	}
	return m.DefaultBranch
}

// GetBranchHead resolves a single branch's HEAD commit directly via the
// git/ref endpoint. Unlike ListBranches this is not subject to the paginated
// 300-branch cap. Results (including 404s) are cached and concurrent lookups
// are coalesced via singleflight. Returns ok=false on any error.
func (c *Client) GetBranchHead(ctx context.Context, owner, repo, name string) (BranchHead, bool) {
	if name == "" {
		return BranchHead{}, false
	}
	key := ForNWOName(owner, repo, name)
	if bh, ok := c.namedBranchCache.Get(key); ok {
		return bh, bh.Name != ""
	}
	v, _, _ := c.namedBranchSF.Do(key.String(), func() (any, error) {
		if bh, ok := c.namedBranchCache.Get(key); ok {
			return bh, nil
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
		if err := c.rest.DoWithContext(ctx, http.MethodGet, path, nil, &resp); err != nil {
			var httpErr *api.HTTPError
			if errors.As(err, &httpErr) && httpErr.StatusCode == http.StatusNotFound {
				c.namedBranchCache.Put(key, BranchHead{}) // negative cache
			}
			return BranchHead{}, nil
		}
		// A prefix (non-exact) match returns an array, decoding to an empty
		// object here; treat that as "not found" rather than guessing.
		if resp.Object.SHA == "" {
			return BranchHead{}, nil
		}
		bh := BranchHead{Name: name, SHA: resp.Object.SHA}
		c.namedBranchCache.Put(key, bh)
		return bh, nil
	})
	bh := v.(BranchHead)
	return bh, bh.Name != ""
}

// ListProtectedBranches returns the repo's protected branches. Best-effort:
// any error yields whatever was collected so far (possibly empty). Results
// are cached per owner/repo and coalesced via singleflight.
func (c *Client) ListProtectedBranches(ctx context.Context, owner, repo string) []BranchHead {
	key := ForRepo(owner, repo)
	if cached, ok := c.protectedBranchCache.Get(key); ok {
		return cached
	}
	sfKey := key.String()
	v, _, _ := c.protectedBranchSF.Do(sfKey, func() (any, error) {
		if cached, ok := c.protectedBranchCache.Get(key); ok {
			return cached, nil
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
			if err := c.rest.DoWithContext(ctx, http.MethodGet, path, nil, &resp); err != nil {
				break // best-effort
			}
			for _, b := range resp {
				all = append(all, BranchHead{Name: b.Name, SHA: b.Commit.SHA, Protected: true})
			}
			if len(resp) < 100 {
				break
			}
		}
		c.protectedBranchCache.Put(key, all)
		return all, nil
	})
	if v == nil {
		return nil
	}
	return v.([]BranchHead)
}

// MatchingHeadRefs returns branches whose names start with prefix via the
// git/matching-refs endpoint. Best-effort: any error yields nil.
func (c *Client) MatchingHeadRefs(ctx context.Context, owner, repo, prefix string) []BranchHead {
	path := fmt.Sprintf("repos/%s/%s/git/matching-refs/heads/%s",
		url.PathEscape(owner), url.PathEscape(repo), escapeBranchPath(prefix))
	var resp []struct {
		Ref    string `json:"ref"`
		Object struct {
			SHA  string `json:"sha"`
			Type string `json:"type"`
		} `json:"object"`
	}
	if err := c.rest.DoWithContext(ctx, http.MethodGet, path, nil, &resp); err != nil {
		return nil
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

// compareResponse is the subset of the GitHub Compare API response we need.
type compareResponse struct {
	Status          string `json:"status"`
	MergeBaseCommit struct {
		SHA string `json:"sha"`
	} `json:"merge_base_commit"`
}

// CompareCommits reports whether sha is on the lineage of branchHeadSHA
// using the Compare API. A 404 or 422 response (unrelated histories or
// missing commit) is treated as a non-error false return. Results are
// memoized for the lifetime of the Client and concurrent identical
// comparisons are coalesced via singleflight. The request runs under a
// cancel-free context so that one fanned-out caller's cancellation (the
// reachability scan cancels siblings on first match) cannot abort the shared
// comparison the others are waiting on.
func (c *Client) CompareCommits(ctx context.Context, owner, repo, sha, branchHeadSHA string) (bool, error) {
	if strings.EqualFold(sha, branchHeadSHA) {
		return true, nil
	}
	key := ForCompare(owner, repo, sha, branchHeadSHA)
	if v, ok := c.compareCache.Get(key); ok {
		return v, nil
	}
	v, err, _ := c.compareSF.Do(key.String(), func() (any, error) {
		if v, ok := c.compareCache.Get(key); ok {
			return v, nil
		}
		path := fmt.Sprintf("repos/%s/%s/compare/%s...%s",
			url.PathEscape(owner), url.PathEscape(repo),
			url.PathEscape(sha), url.PathEscape(branchHeadSHA))
		var resp compareResponse
		if err := c.rest.DoWithContext(ctx, http.MethodGet, path, nil, &resp); err != nil {
			var httpErr *api.HTTPError
			if errors.As(err, &httpErr) &&
				(httpErr.StatusCode == http.StatusNotFound || httpErr.StatusCode == http.StatusUnprocessableEntity) {
				c.compareCache.Put(key, false)
				return false, nil
			}
			if IsSAMLEnforcement(err) && SSOFallbackEligible(owner) {
				contains, anonErr := c.anonCompareCommits(ctx, owner, repo, sha, branchHeadSHA)
				if anonErr == nil {
					c.compareCache.Put(key, contains)
					return contains, nil
				}
				return false, anonErr
			}
			return false, err
		}
		contains := strings.EqualFold(resp.MergeBaseCommit.SHA, sha)
		c.compareCache.Put(key, contains)
		return contains, nil
	})
	if err != nil {
		return false, err
	}
	return v.(bool), nil
}

// CompareRefs returns the Compare API status and merge-base SHA for
// base...head. Unlike CompareCommits it surfaces the raw verdict and the
// underlying error (including *api.HTTPError) so callers can distinguish
// ancestry, forgery, and inconclusive results. Not cached: ancestry checks
// key on distinct base/head pairs that rarely repeat within a run.
func (c *Client) CompareRefs(ctx context.Context, owner, repo, base, head string) (status, mergeBaseSHA string, err error) {
	path := fmt.Sprintf("repos/%s/%s/compare/%s...%s",
		url.PathEscape(owner), url.PathEscape(repo),
		url.PathEscape(base), url.PathEscape(head))
	var resp compareResponse
	if err := c.rest.DoWithContext(ctx, http.MethodGet, path, nil, &resp); err != nil {
		return "", "", err
	}
	return resp.Status, resp.MergeBaseCommit.SHA, nil
}

// RepoIDs returns the numeric owner ID and repo ID for a NWO. Backed by the
// shared repoMetadata fetch, so it shares a single repos/{owner}/{repo}
// round-trip with GetDefaultBranch.
func (c *Client) RepoIDs(ctx context.Context, owner, repo string) (int64, int64, error) {
	m, err := c.repoMetadata(ctx, owner, repo)
	if err != nil {
		return 0, 0, err
	}
	if m.OwnerID == 0 || m.RepoID == 0 {
		return 0, 0, fmt.Errorf("repos/%s/%s returned zero IDs (owner=%d repo=%d)",
			owner, repo, m.OwnerID, m.RepoID)
	}
	return m.OwnerID, m.RepoID, nil
}

// OrderedBranches returns branches in tiered order so the most trust-bearing
// candidates are compared first:
//
//  1. hintBranch (previously recorded in lockfile for this commit)
//  2. hintRef (ref the user wrote in the workflow)
//  3. defaultBranch (e.g. main / master)
//  4. protected branches, lex-sorted within tier
//  5. unprotected branches, lex-sorted within tier
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
