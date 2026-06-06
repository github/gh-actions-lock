// Package audit verifies that pinned commit SHAs are trustworthy:
// reachable from a branch of the canonical repository and ancestrally
// related to the live ref they claim to pin. It wraps a resolve.Resolver
// for data fetching and caching.
package audit

import (
	"github.com/github/gh-actions-pin/internal/resolve"
)

// Re-export status types so callers can use audit.Reachable etc. without
// importing resolve alongside audit.
type (
	ReachabilityStatus = resolve.ReachabilityStatus
	ReachabilityResult = resolve.ReachabilityResult
	AncestryStatus     = resolve.AncestryStatus
)

// Re-export constants.
const (
	Reachable           = resolve.Reachable
	Unreachable         = resolve.Unreachable
	ReachabilityUnknown = resolve.ReachabilityUnknown

	AncestryConfirmed   = resolve.AncestryConfirmed
	AncestryNotAncestor = resolve.AncestryNotAncestor
	AncestryUnknown     = resolve.AncestryUnknown
)

// reachabilityConcurrency bounds how many per-dependency reachability checks
// run in parallel in CheckReachabilityAll.
const reachabilityConcurrency = 8

// Auditor provides reachability and ancestry verification on top of a
// Resolver's data-fetching and caching infrastructure.
type Auditor struct {
	r *resolve.Resolver
}

// New creates an Auditor backed by the given Resolver.
func New(r *resolve.Resolver) *Auditor {
	return &Auditor{r: r}
}
