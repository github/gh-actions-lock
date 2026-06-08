package resolve

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/github/gh-actions-pin/internal/ghapi"
)

// ReachabilityStatus represents the result of a commit reachability check.
type ReachabilityStatus string

const (
	// Reachable means the SHA is confirmed on the ref's lineage.
	Reachable ReachabilityStatus = "reachable"
	// Unreachable means the SHA is confirmed NOT on the ref's lineage
	// (e.g. it exists only in a fork network).
	Unreachable ReachabilityStatus = "unreachable"
	// ReachabilityUnknown means the check could not be completed
	// (timeout, rate limit, API error).
	ReachabilityUnknown ReachabilityStatus = "unknown"
)

// ReachabilityResult holds the outcome of a single reachability check.
type ReachabilityResult struct {
	Owner  string
	Repo   string
	Ref    string
	SHA    string
	DepKey string // full dependency key (e.g. "actions/cache/save@v4")
	Status ReachabilityStatus
	Detail string // human-readable detail (e.g. compare status or error)
	// FullScanUsed is true when the commit was not found in the canonical
	// "likely" branch set (default, protected, release/v*, literal ref,
	// lockfile hint) and the check had to fall back to scanning every branch
	// in the repo. Even when the commit is ultimately Reachable, a full-scan
	// fallback means it is not on a canonical branch — a notable signal worth
	// surfacing to the user.
	FullScanUsed bool
}

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

// CheckAncestry uses the Compare API to test whether pinnedSHA is an ancestor
// of liveSHA. This detects lockfile forgery: if someone injects a SHA that was
// never in the ref's lineage, merge_base(pinned, live) ≠ pinned.
func (r *Resolver) CheckAncestry(ctx context.Context, owner, repo, pinnedSHA, liveSHA string) (AncestryStatus, string) {
	if strings.EqualFold(pinnedSHA, liveSHA) {
		return AncestryConfirmed, "pinned SHA equals live SHA"
	}

	status, mergeBaseSHA, err := r.gh.CompareRefs(ctx, owner, repo, pinnedSHA, liveSHA)
	if err != nil {
		code, ok := ghapi.StatusCode(err)
		if !ok {
			return AncestryUnknown, err.Error()
		}
		switch code {
		case http.StatusNotFound:
			return AncestryNotAncestor, "commit not found in repository"
		case http.StatusConflict:
			return AncestryNotAncestor, "no common ancestor between pinned and live SHA"
		default:
			return AncestryUnknown, fmt.Sprintf("API error (HTTP %d): %s", code, err.Error())
		}
	}

	if strings.EqualFold(mergeBaseSHA, pinnedSHA) {
		return AncestryConfirmed, fmt.Sprintf("pinned SHA is ancestor of live SHA (compare: %s)", status)
	}
	return AncestryNotAncestor, fmt.Sprintf("merge base is %s, not the pinned SHA — possible lockfile forgery or upstream history rewrite", shortHex(mergeBaseSHA))
}

func shortHex(sha string) string {
	if len(sha) <= 12 {
		return sha
	}
	return sha[:12]
}
