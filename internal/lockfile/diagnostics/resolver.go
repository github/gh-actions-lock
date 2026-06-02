package diagnostics

import "context"

// RefStatus is the outcome of resolving a symbolic ref to a SHA.
type RefStatus int

const (
	// RefStatusUnknown: the resolver could not answer (network failure,
	// rate-limit, no auth, no resolver configured). Hosts should fail open.
	RefStatusUnknown RefStatus = iota
	// RefStatusResolved: ref was resolved to a SHA.
	RefStatusResolved
	// RefStatusNotFound: the ref does not exist upstream.
	RefStatusNotFound
)

// RefResult is the resolver's answer for ResolveRef.
type RefResult struct {
	Status  RefStatus
	Sha     string // populated when Status == RefStatusResolved
	RefType string // optional: "tag" | "branch" | "commit"
}

// AncestryStatus is the outcome of CheckAncestry(candidate, head).
type AncestryStatus int

const (
	AncestryUnknown AncestryStatus = iota
	// AncestryConfirmed: candidate is an ancestor of head.
	AncestryConfirmed
	// AncestryNotAncestor: candidate is NOT an ancestor of head — likely
	// tampering or history rewrite.
	AncestryNotAncestor
)

// ReachabilityStatus is the outcome of CheckReachability(sha, ref).
type ReachabilityStatus int

const (
	ReachabilityUnknown ReachabilityStatus = iota
	// ReachabilityReachable: sha is reachable from the ref's history.
	ReachabilityReachable
	// ReachabilityUnreachable: sha exists in the repository network but is
	// not in this ref's ancestry (classic imposter-commit shape).
	ReachabilityUnreachable
)

// Resolver answers the three questions the engine needs to do
// resolver-bound checks. All methods take a context so hosts can apply
// timeouts. Implementations should return *Unknown for any failure rather
// than an error; the engine has no error path here.
type Resolver interface {
	ResolveRef(ctx context.Context, owner, repo, ref string) RefResult
	CheckAncestry(ctx context.Context, owner, repo, candidateSha, headSha string) AncestryStatus
	CheckReachability(ctx context.Context, owner, repo, sha, ref string) ReachabilityStatus
}

// ActionFileProvider fetches action.yml (or action.yaml) contents for a
// given action at a given ref. Used by the transitive validator. Return
// (nil, nil) for "no action file at this path" (e.g. node-action without a
// composite); return (nil, err) for transport failures (engine treats as
// unknown and skips).
type ActionFileProvider interface {
	GetActionFile(ctx context.Context, owner, repo, path, ref string) ([]byte, error)
}
