package doctor

import (
	"context"
	"strings"

	"github.com/github/gh-actions-pin/internal/lockfile"
	"github.com/github/gh-actions-pin/internal/lockfile/diagnostics"
	"github.com/github/gh-actions-pin/internal/resolver"
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
	// tagger, when non-nil, is consulted to recognize legitimate
	// annotated-tag-object SHA pins. It is queried lazily per
	// (owner, repo) so we only fetch tags for repos that actually appear
	// in the workflow set being checked.
	tagger *TagLister
}

// newEngineResolver primes the adapter with the live resolution of refs and
// a pre-computed reachability sweep. Pass live==nil when ResolveAllRecursive
// has failed; the engine will fall back to Unknown and skip resolver-bound
// checks for affected refs. Pass reach==nil to disable reachability checks.
// Pass tagger==nil to disable annotated-tag-object SHA recognition (the
// engine will then treat tag-object SHA pins as MISLEADING_SHA — fine for
// tests / disk-only modes).
func newEngineResolver(r *resolver.Resolver, live []lockfile.Dependency, reach []resolver.ReachabilityResult, tagger *TagLister) *engineResolver {
	a := &engineResolver{
		inner:  r,
		refs:   make(map[string]string, len(live)),
		reach:  map[string]diagnostics.ReachabilityStatus{},
		tagger: tagger,
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
	// If the input ref looks like a SHA but doesn't match the peeled
	// commit, see if it matches an annotated tag object pointing at this
	// commit. Pinning to a tag object SHA is a legitimate immutable-pin
	// pattern — surface it so checkMisleadingSha doesn't fire.
	if a.tagger != nil && !strings.EqualFold(sha, ref) {
		if tags, err := a.tagger.ListTags(owner, repo); err == nil {
			for _, t := range tags {
				if t.TagObjectSHA == "" {
					continue
				}
				if !strings.EqualFold(t.TagObjectSHA, ref) {
					continue
				}
				if !strings.EqualFold(t.SHA, sha) {
					continue
				}
				res.TagObjectSHA = t.TagObjectSHA
				break
			}
		}
	}
	return res
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
