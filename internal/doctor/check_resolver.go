package doctor

import (
	"context"

	"github.com/github/gh-actions-pin/internal/audit"
	"github.com/github/gh-actions-pin/internal/cachekey"
	"github.com/github/gh-actions-pin/internal/lockfile"
	"github.com/github/gh-actions-pin/internal/resolve"
)

// checkResolver is the surface the resolver-bound checks need. The
// production implementation is *prewarmedResolver; tests use stubs.
type checkResolver interface {
	// ResolveRef returns the live SHA for owner/repo@ref. ok=false means
	// the resolver could not answer (network failure, unknown ref); checks
	// fail open on that.
	ResolveRef(owner, repo, ref string) (sha string, ok bool)
	// PeelTagObject reports whether a hex SHA names an annotated tag
	// object (or chain of tag-of-tag) and, if so, returns the commit OID
	// it ultimately points at.
	PeelTagObject(ctx context.Context, owner, repo, sha string) (commit string, ok bool)
	// CheckAncestry asks whether candidate is an ancestor of head and
	// returns a short human-readable detail alongside the status — the
	// rate-limit or compare-base detail callers surface to operators.
	CheckAncestry(ctx context.Context, owner, repo, candidate, head string) (resolve.AncestryStatus, string)
	// CheckReachability asks whether sha is reachable from ref's history.
	CheckReachability(owner, repo, sha, ref string) resolve.ReachabilityStatus
}

// prewarmedResolver adapts *resolve.Resolver to checkResolver. Ref
// resolutions and reachability are pre-computed; ancestry and tag-object
// peels stay on-demand and delegate to the resolver's own cache.
type prewarmedResolver struct {
	inner   *resolve.Resolver
	auditor *audit.Auditor
	refs    map[cachekey.NWORef]string                     // (owner/repo, ref) -> sha
	reach   map[cachekey.Reach]resolve.ReachabilityStatus // (owner/repo, sha, ref) -> status
}

// newPrewarmedResolver primes the adapter with the live resolution of
// refs and a pre-computed reachability sweep. Pass live==nil when
// ResolveAllRecursive failed; checks that need a ref will fail open.
// extraReach carries reach results for SHAs outside the canonical
// lockfile sweep — typically the observed SHA of a moved ref.
func newPrewarmedResolver(r *resolve.Resolver, live []lockfile.Dependency, reach []resolve.ReachabilityResult, extraReach ...[]resolve.ReachabilityResult) *prewarmedResolver {
	extras := 0
	for _, e := range extraReach {
		extras += len(e)
	}
	a := &prewarmedResolver{
		inner:   r,
		auditor: audit.New(r),
		refs:    make(map[cachekey.NWORef]string, len(live)),
		reach:   make(map[cachekey.Reach]resolve.ReachabilityStatus, len(reach)+extras),
	}
	for _, d := range live {
		owner, repo := d.OwnerRepo()
		a.refs[cachekey.ForNWORef(owner, repo, d.Ref)] = d.SHA
	}
	for _, rr := range reach {
		a.reach[cachekey.ForReach(rr.Owner, rr.Repo, rr.SHA, rr.Ref)] = rr.Status
	}
	for _, batch := range extraReach {
		for _, rr := range batch {
			a.reach[cachekey.ForReach(rr.Owner, rr.Repo, rr.SHA, rr.Ref)] = rr.Status
		}
	}
	return a
}

func (a *prewarmedResolver) ResolveRef(owner, repo, ref string) (string, bool) {
	sha, ok := a.refs[cachekey.ForNWORef(owner, repo, ref)]
	return sha, ok
}

func (a *prewarmedResolver) PeelTagObject(ctx context.Context, owner, repo, sha string) (string, bool) {
	if a.inner == nil {
		return "", false
	}
	return a.inner.PeelTagObject(ctx, owner, repo, sha)
}

func (a *prewarmedResolver) CheckAncestry(ctx context.Context, owner, repo, candidate, head string) (resolve.AncestryStatus, string) {
	if a.auditor == nil {
		return resolve.AncestryUnknown, ""
	}
	return a.auditor.CheckAncestry(ctx, owner, repo, candidate, head)
}

func (a *prewarmedResolver) CheckReachability(owner, repo, sha, ref string) resolve.ReachabilityStatus {
	if s, ok := a.reach[cachekey.ForReach(owner, repo, sha, ref)]; ok {
		return s
	}
	return resolve.ReachabilityUnknown
}
