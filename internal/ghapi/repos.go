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

// GetDefaultBranch returns the repo's default branch name (e.g. "main").
// On lookup failure an empty string is cached so subsequent callers don't
// retry. Results are coalesced via singleflight.
func (c *Client) GetDefaultBranch(ctx context.Context, owner, repo string) string {
	key := ForRepo(owner, repo)
	if name, ok := c.defaultBranchCache.Get(key); ok {
		return name
	}
	sfKey := key.String()
	v, _, _ := c.defaultBranchSF.Do(sfKey, func() (any, error) {
		if name, ok := c.defaultBranchCache.Get(key); ok {
			return name, nil
		}
		var resp struct {
			DefaultBranch string `json:"default_branch"`
		}
		path := fmt.Sprintf("repos/%s/%s", url.PathEscape(owner), url.PathEscape(repo))
		if err := c.rest.DoWithContext(ctx, http.MethodGet, path, nil, &resp); err != nil {
			c.defaultBranchCache.Put(key, "")
			return "", nil
		}
		c.defaultBranchCache.Put(key, resp.DefaultBranch)
		return resp.DefaultBranch, nil
	})
	return v.(string)
}

// GetBranchHead resolves a single branch's HEAD commit directly via the
// git/ref endpoint. Unlike ListBranches this is not subject to the paginated
// 300-branch cap. Results (including 404s) are cached. Returns ok=false on
// any error.
func (c *Client) GetBranchHead(ctx context.Context, owner, repo, name string) (BranchHead, bool) {
	if name == "" {
		return BranchHead{}, false
	}
	key := ForNWOName(owner, repo, name)
	if bh, ok := c.namedBranchCache.Get(key); ok {
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
	if err := c.rest.DoWithContext(ctx, http.MethodGet, path, nil, &resp); err != nil {
		var httpErr *api.HTTPError
		if errors.As(err, &httpErr) && httpErr.StatusCode == http.StatusNotFound {
			c.namedBranchCache.Put(key, BranchHead{}) // negative cache
		}
		return BranchHead{}, false
	}
	// A prefix (non-exact) match returns an array, decoding to an empty
	// object here; treat that as "not found" rather than guessing.
	if resp.Object.SHA == "" {
		return BranchHead{}, false
	}
	bh := BranchHead{Name: name, SHA: resp.Object.SHA}
	c.namedBranchCache.Put(key, bh)
	return bh, true
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
// memoized for the lifetime of the Client.
func (c *Client) CompareCommits(ctx context.Context, owner, repo, sha, branchHeadSHA string) (bool, error) {
	if strings.EqualFold(sha, branchHeadSHA) {
		return true, nil
	}
	key := ForCompare(owner, repo, sha, branchHeadSHA)
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
		return false, err
	}
	contains := strings.EqualFold(resp.MergeBaseCommit.SHA, sha)
	c.compareCache.Put(key, contains)
	return contains, nil
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

// RepoIDs returns the numeric owner ID and repo ID for a NWO, querying
// the GitHub REST API on cache miss.
func (c *Client) RepoIDs(ctx context.Context, owner, repo string) (int64, int64, error) {
	key := ForRepo(owner, repo)
	if ids, ok := c.repoIDsCache.Get(key); ok {
		return ids[0], ids[1], nil
	}
	var resp struct {
		ID    int64 `json:"id"`
		Owner struct {
			ID int64 `json:"id"`
		} `json:"owner"`
	}
	path := fmt.Sprintf("repos/%s/%s", url.PathEscape(owner), url.PathEscape(repo))
	if err := c.rest.DoWithContext(ctx, http.MethodGet, path, nil, &resp); err != nil {
		return 0, 0, fmt.Errorf("fetching %s: %w", path, err)
	}
	if resp.ID == 0 || resp.Owner.ID == 0 {
		return 0, 0, fmt.Errorf("%s returned zero IDs (owner=%d repo=%d)", path, resp.Owner.ID, resp.ID)
	}
	c.repoIDsCache.Put(key, [2]int64{resp.Owner.ID, resp.ID})
	return resp.Owner.ID, resp.ID, nil
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
