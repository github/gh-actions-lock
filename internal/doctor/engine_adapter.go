package doctor

import (
	"context"
	"strings"

	"github.com/github/gh-actions-pin/internal/lockfile"
	"github.com/github/gh-actions-pin/internal/lockfile/diagnostics"
	"github.com/github/gh-actions-pin/internal/resolver"
	parserlock "github.com/github/gh-actions-pin/pkg/lockfile"
)

// engineResolver adapts gh-actions-pin's *resolver.Resolver to the workflow
// parser's diagnostics.Resolver interface.
//
// Ref resolutions and reachability results are pre-computed eagerly (cheaper
// than re-querying for every engine call); ancestry lookups stay on-demand
// because they are infrequent (one per REF_MOVED finding) and we want the
// resolver's own cache to handle dedupe.
type engineResolver struct {
	inner *resolver.Resolver
	refs  map[string]string                         // owner/repo@ref -> sha
	reach map[string]diagnostics.ReachabilityStatus // owner/repo@sha@ref -> status
}

// newEngineResolver primes the adapter with the live resolution of refs and
// a pre-computed reachability sweep. Pass live==nil when ResolveAllRecursive
// has failed; the engine will fall back to Unknown and skip resolver-bound
// checks for affected refs. Pass reach==nil to disable reachability checks.
func newEngineResolver(r *resolver.Resolver, live []lockfile.Dependency, reach []resolver.ReachabilityResult) *engineResolver {
	a := &engineResolver{
		inner: r,
		refs:  make(map[string]string, len(live)),
		reach: map[string]diagnostics.ReachabilityStatus{},
	}
	for _, d := range live {
		a.refs[d.NWO+"@"+d.Ref] = d.SHA
	}
	for _, rr := range reach {
		key := rr.Owner + "/" + rr.Repo + "@" + rr.SHA + "@" + rr.Ref
		a.reach[key] = mapReachability(rr.Status)
	}
	return a
}

func (a *engineResolver) ResolveRef(_ context.Context, owner, repo, ref string) diagnostics.RefResult {
	sha, ok := a.refs[owner+"/"+repo+"@"+ref]
	if !ok {
		return diagnostics.RefResult{Status: diagnostics.RefStatusUnknown}
	}
	res := diagnostics.RefResult{Status: diagnostics.RefStatusResolved, Sha: sha}
	res.Immutable = classifyImmutable(a.inner, owner, repo, ref, sha)
	return res
}

// classifyImmutable reports whether a uses: ref is content-addressed and
// therefore safe even if it does not equal the resolved commit. Only
// meaningful for hex-shaped inputs; non-hex refs (tag/branch names) are
// outside the MISLEADING_SHA / SHARefMismatch scope entirely so we return
// true to short-circuit those callers — they have their own mutability
// signals (REF_MOVED, IMPOSTER_COMMIT).
//
// Hex inputs are immutable when:
//   - the input equals the resolved commit OID (it IS the commit), or
//   - the input is an annotated tag object SHA (including chains) that
//     peels to the resolved commit.
//
// Hex inputs that don't match either path are mutable — typically a
// branch named after a SHA, the only shape worth flagging.
func classifyImmutable(r *resolver.Resolver, owner, repo, ref, sha string) bool {
	if !parserlock.IsFullSha(ref) {
		return true
	}
	if strings.EqualFold(ref, sha) {
		return true
	}
	if r == nil {
		return false
	}
	commit, ok := r.PeelTagObject(owner, repo, ref)
	if !ok {
		return false
	}
	return strings.EqualFold(commit, sha)
}

func (a *engineResolver) CheckAncestry(_ context.Context, owner, repo, candidateSha, headSha string) diagnostics.AncestryStatus {
	if a.inner == nil {
		return diagnostics.AncestryUnknown
	}
	s, _ := a.inner.CheckAncestry(owner, repo, candidateSha, headSha)
	switch s {
	case resolver.AncestryConfirmed:
		return diagnostics.AncestryConfirmed
	case resolver.AncestryNotAncestor:
		return diagnostics.AncestryNotAncestor
	default:
		return diagnostics.AncestryUnknown
	}
}

func (a *engineResolver) CheckReachability(_ context.Context, owner, repo, sha, ref string) diagnostics.ReachabilityStatus {
	if s, ok := a.reach[owner+"/"+repo+"@"+sha+"@"+ref]; ok {
		return s
	}
	return diagnostics.ReachabilityUnknown
}

func mapReachability(s resolver.ReachabilityStatus) diagnostics.ReachabilityStatus {
	switch s {
	case resolver.Reachable:
		return diagnostics.ReachabilityReachable
	case resolver.Unreachable:
		return diagnostics.ReachabilityUnreachable
	default:
		return diagnostics.ReachabilityUnknown
	}
}
