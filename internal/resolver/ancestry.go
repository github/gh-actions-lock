package resolver

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

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

// Ancestry retry tuning. Exposed as vars (not consts) so tests can dial
// the budget down to keep retry-loop assertions snappy without dragging
// in a full clock-injection harness for every case.
var (
	// ancestryMaxAttempts is the total number of Compare API attempts —
	// one initial request plus up to (max-1) retries on rate-limit
	// responses.
	ancestryMaxAttempts = 3
	// ancestryRetryBudget caps the cumulative wall-clock spent sleeping
	// between retries. We bail rather than honor an X-RateLimit-Reset
	// that would push us past the budget.
	ancestryRetryBudget = 30 * time.Second
	// ancestryBackoff is the per-attempt expo fallback used when no
	// usable rate-limit header is present (index N = wait before attempt
	// N+1 in this slice). Indices past the end clamp to the last value.
	ancestryBackoff = []time.Duration{250 * time.Millisecond, 500 * time.Millisecond, 1 * time.Second}
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
//
// Compare API rate limits (HTTP 429 always, HTTP 403 when accompanied by
// the documented rate-limit headers) are retried up to ancestryMaxAttempts
// total attempts, honoring X-RateLimit-Reset when present and otherwise
// falling back to ancestryBackoff. The cumulative wait is capped at
// ancestryRetryBudget — a reset timestamp beyond the remaining budget
// short-circuits to AncestryUnknown with an exhausted-budget detail
// rather than burning the whole budget on a call that would still fail.
func (r *Resolver) CheckAncestry(owner, repo, pinnedSHA, liveSHA string) (AncestryStatus, string) {
	path := fmt.Sprintf("repos/%s/%s/compare/%s...%s",
		url.PathEscape(owner), url.PathEscape(repo),
		url.PathEscape(pinnedSHA), url.PathEscape(liveSHA))

	deadline := r.nowFn().Add(ancestryRetryBudget)
	var lastDetail string

	for attempt := 0; attempt < ancestryMaxAttempts; attempt++ {
		var resp compareResponse
		err := r.restClient.Get(path, &resp)
		if err == nil {
			if strings.EqualFold(resp.MergeBaseCommit.SHA, pinnedSHA) {
				return AncestryConfirmed, fmt.Sprintf("pinned SHA is ancestor of live SHA (compare: %s)", resp.Status)
			}
			return AncestryNotAncestor, fmt.Sprintf("merge base is %s, not the pinned SHA — possible lockfile forgery or upstream history rewrite", shortHex(resp.MergeBaseCommit.SHA))
		}

		var httpErr *api.HTTPError
		if !errors.As(err, &httpErr) {
			return AncestryUnknown, err.Error()
		}
		switch httpErr.StatusCode {
		case http.StatusNotFound:
			return AncestryNotAncestor, "commit not found in repository"
		case http.StatusConflict:
			return AncestryNotAncestor, "no common ancestor between pinned and live SHA"
		}
		if !isRateLimited(httpErr) {
			return AncestryUnknown, fmt.Sprintf("API error (HTTP %d): %s", httpErr.StatusCode, httpErr.Message)
		}

		lastDetail = rateLimitDetail(httpErr)
		if attempt == ancestryMaxAttempts-1 {
			return AncestryUnknown, fmt.Sprintf("%s; retry budget exhausted after %d attempts", lastDetail, ancestryMaxAttempts)
		}
		wait := nextRetryWait(httpErr, attempt, r.nowFn())
		if !r.nowFn().Add(wait).Before(deadline) && wait > 0 {
			return AncestryUnknown, fmt.Sprintf("%s; retry budget (%s) would be exceeded after %d attempts", lastDetail, ancestryRetryBudget, attempt+1)
		}
		if wait > 0 {
			r.sleepFn(wait)
		}
	}
	// Loop invariant guarantees one of the returns above fires; this is
	// defensive for future refactors.
	return AncestryUnknown, lastDetail
}

// isRateLimited reports whether an HTTPError carries the shape of a GitHub
// rate-limit (or secondary-rate-limit) response. 429 is always such; 403
// only when accompanied by one of the standard rate-limit headers, so
// SSO / private-repo 403s are not retried.
func isRateLimited(httpErr *api.HTTPError) bool {
	if httpErr.StatusCode == http.StatusTooManyRequests {
		return true
	}
	if httpErr.StatusCode != http.StatusForbidden {
		return false
	}
	if httpErr.Headers.Get("Retry-After") != "" {
		return true
	}
	if httpErr.Headers.Get("X-RateLimit-Reset") != "" && httpErr.Headers.Get("X-RateLimit-Remaining") == "0" {
		return true
	}
	return false
}

// rateLimitDetail formats the human-readable explanation surfaced through
// to Finding.Detail. Keeps it to status code + reset timestamp; nothing
// from the response body that could leak environment-specific text.
func rateLimitDetail(httpErr *api.HTTPError) string {
	detail := fmt.Sprintf("rate limited (HTTP %d)", httpErr.StatusCode)
	if reset := httpErr.Headers.Get("X-RateLimit-Reset"); reset != "" {
		detail += "; resets at " + reset
	}
	return detail
}

// nextRetryWait picks the sleep before the next Compare API attempt.
// Prefers X-RateLimit-Reset (Unix epoch seconds) when present and in the
// future, then Retry-After (delta seconds), and finally the expo
// ancestryBackoff schedule. The +1s pad on the reset target absorbs
// clock skew between us and the API edge.
func nextRetryWait(httpErr *api.HTTPError, attempt int, now time.Time) time.Duration {
	if reset := httpErr.Headers.Get("X-RateLimit-Reset"); reset != "" {
		if ts, err := strconv.ParseInt(reset, 10, 64); err == nil {
			if d := time.Unix(ts, 0).Add(1 * time.Second).Sub(now); d > 0 {
				return d
			}
		}
	}
	if ra := httpErr.Headers.Get("Retry-After"); ra != "" {
		if secs, err := strconv.Atoi(ra); err == nil && secs > 0 {
			return time.Duration(secs) * time.Second
		}
	}
	idx := attempt
	if idx >= len(ancestryBackoff) {
		idx = len(ancestryBackoff) - 1
	}
	return ancestryBackoff[idx]
}

// shortHex returns the first 12 chars of a hex SHA, or the whole string
// if shorter. Guards against the panic shape of slicing a malformed
// response body.
func shortHex(sha string) string {
	if len(sha) <= 12 {
		return sha
	}
	return sha[:12]
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
