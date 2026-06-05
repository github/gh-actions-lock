package doctor

import (
	"github.com/github/gh-actions-pin/internal/lockfile"
	"github.com/github/gh-actions-pin/internal/resolver"
)

// checkResolver is the surface the resolver-bound checks need. It exists
// solely so the checks can be exercised with a stub in tests; the
// production implementation is *prewarmedResolver.
//
// The interface is unexported because nothing outside the doctor package
// implements it.
type checkResolver interface {
	// ResolveRef returns the live SHA for owner/repo@ref. ok=false means
	// the resolver could not answer (network failure, unknown ref); checks
	// fail open on that.
	ResolveRef(owner, repo, ref string) (sha string, ok bool)
	// PeelTagObject reports whether a hex SHA names an annotated tag
	// object (or chain of tag-of-tag) and, if so, returns the commit OID
	// it ultimately points at. Used to distinguish a content-addressed
	// tag-object pin (legitimate) from a branch named after a SHA (the
	// forgery shape we want to flag).
	PeelTagObject(owner, repo, sha string) (commit string, ok bool)
	// CheckAncestry asks whether candidate is an ancestor of head.
	CheckAncestry(owner, repo, candidate, head string) resolver.AncestryStatus
	// CheckReachability asks whether sha is reachable from ref's history.
	CheckReachability(owner, repo, sha, ref string) resolver.ReachabilityStatus
}

// prewarmedResolver adapts *resolver.Resolver to checkResolver. Ref
// resolutions and reachability are pre-computed (cheaper than re-querying
// for every check call); ancestry and tag-object peels stay on-demand and
// delegate to the resolver's own cache.
type prewarmedResolver struct {
	inner *resolver.Resolver
	refs  map[string]string                      // owner/repo@ref -> sha
	reach map[string]resolver.ReachabilityStatus // owner/repo@sha@ref -> status
}

// newPrewarmedResolver primes the adapter with the live resolution of
// refs and a pre-computed reachability sweep. Pass live==nil when
// ResolveAllRecursive failed; checks that need a ref will fail open.
func newPrewarmedResolver(r *resolver.Resolver, live []lockfile.Dependency, reach []resolver.ReachabilityResult) *prewarmedResolver {
	a := &prewarmedResolver{
		inner: r,
		refs:  make(map[string]string, len(live)),
		reach: make(map[string]resolver.ReachabilityStatus, len(reach)),
	}
	for _, d := range live {
		a.refs[d.NWO+"@"+d.Ref] = d.SHA
	}
	for _, rr := range reach {
		key := rr.Owner + "/" + rr.Repo + "@" + rr.SHA + "@" + rr.Ref
		a.reach[key] = rr.Status
	}
	return a
}

func (a *prewarmedResolver) ResolveRef(owner, repo, ref string) (string, bool) {
	sha, ok := a.refs[owner+"/"+repo+"@"+ref]
	return sha, ok
}

func (a *prewarmedResolver) PeelTagObject(owner, repo, sha string) (string, bool) {
	if a.inner == nil {
		return "", false
	}
	return a.inner.PeelTagObject(owner, repo, sha)
}

func (a *prewarmedResolver) CheckAncestry(owner, repo, candidate, head string) resolver.AncestryStatus {
	if a.inner == nil {
		return resolver.AncestryUnknown
	}
	s, _ := a.inner.CheckAncestry(owner, repo, candidate, head)
	return s
}

func (a *prewarmedResolver) CheckReachability(owner, repo, sha, ref string) resolver.ReachabilityStatus {
	if s, ok := a.reach[owner+"/"+repo+"@"+sha+"@"+ref]; ok {
		return s
	}
	return resolver.ReachabilityUnknown
}
