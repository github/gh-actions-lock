package audit

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/cli/go-gh/v2/pkg/api"
	"github.com/github/gh-actions-pin/internal/resolve"
)

// Ancestry retry tuning. Vars rather than consts so tests can dial the
// budget down without a clock-injection harness.
var (
	ancestryMaxAttempts = 3
	ancestryRetryBudget = 30 * time.Second
	ancestryBackoff     = []time.Duration{250 * time.Millisecond, 500 * time.Millisecond, 1 * time.Second}
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
// Rate-limit responses (HTTP 429 always; HTTP 403 with rate-limit headers)
// are retried up to ancestryMaxAttempts, honoring X-RateLimit-Reset when
// present. The cumulative wait is capped at ancestryRetryBudget.
func (a *Auditor) CheckAncestry(ctx context.Context, owner, repo, pinnedSHA, liveSHA string) (resolve.AncestryStatus, string) {
	path := fmt.Sprintf("repos/%s/%s/compare/%s...%s",
		url.PathEscape(owner), url.PathEscape(repo),
		url.PathEscape(pinnedSHA), url.PathEscape(liveSHA))

	nowFn := a.r.NowFn()
	sleepFn := a.r.SleepFn()
	deadline := nowFn().Add(ancestryRetryBudget)
	var lastDetail string

	for attempt := 0; attempt < ancestryMaxAttempts; attempt++ {
		var resp compareResponse
		err := a.r.RestClient().DoWithContext(ctx, http.MethodGet, path, nil, &resp)
		if err == nil {
			if strings.EqualFold(resp.MergeBaseCommit.SHA, pinnedSHA) {
				return resolve.AncestryConfirmed, fmt.Sprintf("pinned SHA is ancestor of live SHA (compare: %s)", resp.Status)
			}
			return resolve.AncestryNotAncestor, fmt.Sprintf("merge base is %s, not the pinned SHA — possible lockfile forgery or upstream history rewrite", shortHex(resp.MergeBaseCommit.SHA))
		}

		var httpErr *api.HTTPError
		if !errors.As(err, &httpErr) {
			return resolve.AncestryUnknown, err.Error()
		}
		switch httpErr.StatusCode {
		case http.StatusNotFound:
			return resolve.AncestryNotAncestor, "commit not found in repository"
		case http.StatusConflict:
			return resolve.AncestryNotAncestor, "no common ancestor between pinned and live SHA"
		}
		if !isRateLimited(httpErr) {
			return resolve.AncestryUnknown, fmt.Sprintf("API error (HTTP %d): %s", httpErr.StatusCode, httpErr.Message)
		}

		lastDetail = rateLimitDetail(httpErr)
		if attempt == ancestryMaxAttempts-1 {
			return resolve.AncestryUnknown, fmt.Sprintf("%s; retry budget exhausted after %d attempts", lastDetail, ancestryMaxAttempts)
		}
		wait := nextRetryWait(httpErr, attempt, nowFn())
		if !nowFn().Add(wait).Before(deadline) && wait > 0 {
			return resolve.AncestryUnknown, fmt.Sprintf("%s; retry budget (%s) would be exceeded after %d attempts", lastDetail, ancestryRetryBudget, attempt+1)
		}
		if wait > 0 {
			sleepFn(ctx, wait)
		}
		if ctx.Err() != nil {
			return resolve.AncestryUnknown, "ancestry check canceled"
		}
	}
	return resolve.AncestryUnknown, lastDetail
}

// isRateLimited reports whether an HTTPError carries the shape of a GitHub
// rate-limit (or secondary-rate-limit) response.
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

// rateLimitDetail formats the human-readable explanation.
func rateLimitDetail(httpErr *api.HTTPError) string {
	detail := fmt.Sprintf("rate limited (HTTP %d)", httpErr.StatusCode)
	if reset := httpErr.Headers.Get("X-RateLimit-Reset"); reset != "" {
		detail += "; resets at " + reset
	}
	return detail
}

// nextRetryWait picks the sleep before the next Compare API attempt.
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
// if shorter.
func shortHex(sha string) string {
	if len(sha) <= 12 {
		return sha
	}
	return sha[:12]
}
