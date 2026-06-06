package resolve

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/cli/go-gh/v2/pkg/api"
	"github.com/github/gh-actions-pin/internal/cachekey"
)

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

// compareResponse is the subset of the GitHub Compare API response we need
// for BranchContainsCommit.
type compareResponse struct {
	Status          string `json:"status"`
	MergeBaseCommit struct {
		SHA string `json:"sha"`
	} `json:"merge_base_commit"`
}

// BranchContainsCommit reports whether sha is on the lineage of branchHeadSHA
// using the documented Compare API. A 404 or 422 response (unrelated histories
// or missing commit) is treated as a non-error false return. Results are
// memoized in compareCache for the lifetime of the Resolver.
func (r *Resolver) BranchContainsCommit(ctx context.Context, owner, repo, sha, branchHeadSHA string) (bool, error) {
	if strings.EqualFold(sha, branchHeadSHA) {
		return true, nil
	}
	key := cachekey.ForCompare(owner, repo, sha, branchHeadSHA)
	if v, ok := r.compareCache.get(key); ok {
		return v, nil
	}
	path := fmt.Sprintf("repos/%s/%s/compare/%s...%s",
		url.PathEscape(owner), url.PathEscape(repo),
		url.PathEscape(sha), url.PathEscape(branchHeadSHA))
	var resp compareResponse
	if err := r.restClient.DoWithContext(ctx, http.MethodGet, path, nil, &resp); err != nil {
		var httpErr *api.HTTPError
		if errors.As(err, &httpErr) &&
			(httpErr.StatusCode == http.StatusNotFound || httpErr.StatusCode == http.StatusUnprocessableEntity) {
			r.compareCache.put(key, false)
			return false, nil
		}
		return false, err
	}
	contains := strings.EqualFold(resp.MergeBaseCommit.SHA, sha)
	r.compareCache.put(key, contains)
	return contains, nil
}
