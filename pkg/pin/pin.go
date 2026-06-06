// Package pin defines the interface contract for action pinning operations.
// Implementations resolve action refs to commit SHAs, discover containing
// refs, and verify reachability. The CLI fulfills these via GitHub REST API;
// a future server-side implementation will use spokesd/reposd.
package pin

import (
	"context"

	"github.com/github/gh-actions-pin/internal/lockfile"
	"github.com/github/gh-actions-pin/internal/resolve"
)

// Resolver resolves action refs to pinned SHAs and discovers containing refs.
type Resolver interface {
	// Resolve turns action refs into pinned dependencies with recursive
	// composite-action walking (forward: ref → SHA).
	Resolve(ctx context.Context, refs []lockfile.ActionRef) ([]lockfile.Dependency, resolve.ParentMap, error)

	// ReverseLookup discovers which tag/branch contains each dependency's
	// SHA (reverse: SHA → ref). Mutates deps in place (sets Tag, Branch,
	// Ref fields). Returns a rewrites map of old-key → new-key for any
	// deps whose canonical key changed.
	ReverseLookup(ctx context.Context, deps []lockfile.Dependency) (map[string]string, error)
}

// Auditor verifies that pinned SHAs are trustworthy.
type Auditor interface {
	// CheckReachability verifies that each dependency's SHA is reachable
	// from at least one branch of its canonical repository.
	CheckReachability(ctx context.Context, deps []lockfile.Dependency) ([]resolve.ReachabilityResult, error)
}
