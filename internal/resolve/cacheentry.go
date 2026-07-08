// Cache value types for the resolver's in-memory caches. The keys are the
// identity types in package ghapi (ActionRef, Reach, NWOSha); these are the
// values they map to. They live here, next to the domain logic that produces
// them, because each carries a resolve-local type (dep.Dependency,
// ReachabilityStatus) and cannot move to a shared package without a cycle.

package resolve

import "github.com/github/gh-actions-lock/internal/dep"

// resolvedEntry is the domain cache value for a resolved action ref.
type resolvedEntry struct {
	dep       dep.Dependency
	actionYML string
}

// tagPeel records the outcome of a PeelTagObject lookup so repeated checks
// for the same SHA avoid additional API calls.
type tagPeel struct {
	commit string // commit the tag object peels to (empty when isTag is false)
	isTag  bool   // true when the SHA is an annotated tag object
}
