package resolver

import (
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

// compareResponse is the subset of the GitHub Compare API response we need.
type compareResponse struct {
	Status          string `json:"status"`
	MergeBaseCommit struct {
		SHA string `json:"sha"`
	} `json:"merge_base_commit"`
}

// CheckAncestry uses the Compare API to test whether pinnedSHA is an ancestor
// of liveSHA. This detects lockfile forgery: if someone injects a SHA that was
// never in the ref's lineage, merge_base(pinned, live) ≠ pinned.
func (r *Resolver) CheckAncestry(owner, repo, pinnedSHA, liveSHA string) (AncestryStatus, string) {
	path := fmt.Sprintf("repos/%s/%s/compare/%s...%s",
		url.PathEscape(owner), url.PathEscape(repo),
		url.PathEscape(pinnedSHA), url.PathEscape(liveSHA))

	var resp compareResponse
	err := r.restClient.Get(path, &resp)
	if err != nil {
		var httpErr *api.HTTPError
		if errors.As(err, &httpErr) {
			switch httpErr.StatusCode {
			case http.StatusNotFound:
				return AncestryNotAncestor, "commit not found in repository"
			case http.StatusConflict: // 409 — no common ancestor
				return AncestryNotAncestor, "no common ancestor between pinned and live SHA"
			case http.StatusForbidden, http.StatusTooManyRequests:
				detail := fmt.Sprintf("rate limited (HTTP %d)", httpErr.StatusCode)
				if reset := httpErr.Headers.Get("X-RateLimit-Reset"); reset != "" {
					detail += "; resets at " + reset
				}
				return AncestryUnknown, detail
			default:
				return AncestryUnknown, fmt.Sprintf("API error (HTTP %d): %s", httpErr.StatusCode, httpErr.Message)
			}
		}
		return AncestryUnknown, err.Error()
	}

	if strings.EqualFold(resp.MergeBaseCommit.SHA, pinnedSHA) {
		return AncestryConfirmed, fmt.Sprintf("pinned SHA is ancestor of live SHA (compare: %s)", resp.Status)
	}
	return AncestryNotAncestor, fmt.Sprintf("merge base is %s, not the pinned SHA — possible lockfile forgery or upstream history rewrite", resp.MergeBaseCommit.SHA[:12])
}

// branchContainsCommit reports whether sha is on the lineage of branchHeadSHA
// using the documented Compare API. A 404 or 422 response (unrelated histories
// or missing commit) is treated as a non-error false return. Results are
// memoized in compareCache for the lifetime of the Resolver.
func (r *Resolver) branchContainsCommit(owner, repo, sha, branchHeadSHA string) (bool, error) {
	if strings.EqualFold(sha, branchHeadSHA) {
		return true, nil
	}
	key := cachekey.ForCompare(owner, repo, sha, branchHeadSHA)
	r.cacheMu.Lock()
	v, ok := r.compareCache[key]
	r.cacheMu.Unlock()
	if ok {
		return v, nil
	}
	path := fmt.Sprintf("repos/%s/%s/compare/%s...%s",
		url.PathEscape(owner), url.PathEscape(repo),
		url.PathEscape(sha), url.PathEscape(branchHeadSHA))
	var resp compareResponse
	if err := r.restClient.Get(path, &resp); err != nil {
		var httpErr *api.HTTPError
		if errors.As(err, &httpErr) &&
			(httpErr.StatusCode == http.StatusNotFound || httpErr.StatusCode == http.StatusUnprocessableEntity) {
			r.setCompareCache(key, false)
			return false, nil // unrelated histories or missing commit
		}
		return false, err
	}
	// sha is an ancestor of branchHeadSHA iff the merge base IS sha.
	contains := strings.EqualFold(resp.MergeBaseCommit.SHA, sha)
	r.setCompareCache(key, contains)
	return contains, nil
}
