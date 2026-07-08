package checks

import (
	"context"

	"github.com/github/gh-actions-lock/internal/dep"
	"github.com/github/gh-actions-lock/internal/ghapi"
	"github.com/github/gh-actions-lock/internal/resolve"
)

// CheckResolver is the surface the resolver-bound checks need. The
// production implementation is *prewarmedResolver; tests use stubs.
type CheckResolver interface {
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
}

// prewarmedResolver adapts *resolve.Resolver to CheckResolver. Ref
// resolutions are pre-computed; ancestry and tag-object peels stay
// on-demand and delegate to the resolver's own cache.
type prewarmedResolver struct {
	inner *resolve.Resolver
	refs  map[ghapi.NWORef]string // (owner/repo, ref) -> sha
}

// NewPrewarmedResolver primes the adapter with the live resolution of
// refs. Pass live==nil when ResolveAllRecursive failed; checks that
// need a ref will fail open.
func NewPrewarmedResolver(r *resolve.Resolver, live []dep.Dependency) *prewarmedResolver {
	a := &prewarmedResolver{
		inner: r,
		refs:  make(map[ghapi.NWORef]string, len(live)),
	}
	for _, d := range live {
		owner, repo := d.OwnerRepo()
		a.refs[ghapi.ForNWORef(owner, repo, d.Ref)] = d.SHA
	}
	return a
}

func (a *prewarmedResolver) ResolveRef(owner, repo, ref string) (string, bool) {
	sha, ok := a.refs[ghapi.ForNWORef(owner, repo, ref)]
	return sha, ok
}

func (a *prewarmedResolver) PeelTagObject(ctx context.Context, owner, repo, sha string) (string, bool) {
	if a.inner == nil {
		return "", false
	}
	return a.inner.PeelTagObject(ctx, owner, repo, sha)
}

func (a *prewarmedResolver) CheckAncestry(ctx context.Context, owner, repo, candidate, head string) (resolve.AncestryStatus, string) {
	if a.inner == nil {
		return resolve.AncestryUnknown, ""
	}
	return a.inner.CheckAncestry(ctx, owner, repo, candidate, head)
}
