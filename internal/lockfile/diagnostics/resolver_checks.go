package diagnostics

import (
	"context"
	"fmt"
	"strings"

	parserlock "github.com/github/gh-actions-pin/pkg/lockfile"
)

// checkMisleadingSha emits MISLEADING_SHA when a uses: ref looks like a
// SHA but the resolver maps it to a different commit. This is independent
// of whether a lock entry exists.
func checkMisleadingSha(ctx context.Context, wf WorkflowInput, r Resolver) []Finding {
	var out []Finding
	for _, u := range wf.Uses {
		if !parserlock.IsFullSha(u.Ref) {
			continue
		}
		res := r.ResolveRef(ctx, u.Owner, u.Repo, u.Ref)
		if res.Status != RefStatusResolved || res.Sha == "" {
			continue
		}
		// MISLEADING_SHA cares about exactly one shape: a SHA-shaped ref
		// that resolves to a commit via a *mutable* path (a branch named
		// after a SHA). Commit-OID pins and annotated-tag-object pins
		// (including chains) are content-addressed and safe even when
		// res.Sha differs from the input — the host marks those Immutable.
		if res.Immutable {
			continue
		}
		if strings.EqualFold(res.Sha, u.Ref) {
			continue
		}
		f := findingFromUse(wf, u)
		f.Code = CodeMisleadingSha
		f.Severity = SeverityError
		f.LiveSha = res.Sha
		f.LockedSha = u.Ref
		f.Message = fmt.Sprintf("ref %s resolves to %s — the ref string looks like a SHA but isn't this commit", shortSha(u.Ref), shortSha(res.Sha))
		f.Remediation = "investigate — the ref may be a tag named after a SHA, not the SHA itself"
		out = append(out, f)
	}
	return out
}

// checkRefMovedAndForgery emits REF_MOVED when the upstream ref resolves
// to a different SHA than the lockfile. If CheckAncestry confirms the
// locked SHA is NOT an ancestor of the live SHA, the finding is upgraded
// to LOCKFILE_FORGERY (mutually exclusive).
func checkRefMovedAndForgery(ctx context.Context, wf WorkflowInput, lf parserlock.File, depIndex map[string]parserlock.Pin, r Resolver) []Finding {
	var out []Finding
	for _, u := range wf.Uses {
		if parserlock.IsFullSha(u.Ref) {
			continue // covered by misleading_sha
		}
		pin, ok := depIndex[u.IndexKey()]
		if !ok {
			continue // no lock entry → not_pinned territory
		}
		res := r.ResolveRef(ctx, u.Owner, u.Repo, u.Ref)
		if res.Status != RefStatusResolved || res.Sha == "" {
			continue // fail open on resolver Unknown / NotFound
		}
		if strings.EqualFold(res.Sha, pin.Hex) {
			continue // lock is current
		}

		// SHA drift detected. Ancestry call decides REF_MOVED vs FORGERY.
		ancestry := r.CheckAncestry(ctx, u.Owner, u.Repo, pin.Hex, res.Sha)
		f := findingFromUse(wf, u)
		f.LockedSha = pin.Hex
		f.LiveSha = res.Sha
		switch ancestry {
		case AncestryNotAncestor:
			f.Code = CodeLockfileForgery
			f.Severity = SeverityError
			f.Message = fmt.Sprintf("pinned %s is not an ancestor of %s — lockfile may have been tampered with", shortSha(pin.Hex), shortSha(res.Sha))
			f.Remediation = "investigate immediately — verify the lockfile entry against upstream history"
		default:
			f.Code = CodeRefMoved
			f.Severity = SeverityWarning
			f.Message = fmt.Sprintf("ref %s now resolves to %s, lockfile pins %s", u.Ref, shortSha(res.Sha), shortSha(pin.Hex))
			if ancestry == AncestryUnknown {
				f.Message += " (ancestry check inconclusive)"
			}
			f.Remediation = "re-run `gh actions-pin` to refresh the lock entry"
		}
		_ = lf // reserved for future cross-workflow inspection
		out = append(out, f)
	}
	return out
}

// checkImposterCommit emits IMPOSTER_COMMIT when the locked SHA is not
// reachable from the ref's history. Skips entries already covered by a
// LOCKFILE_FORGERY finding (the two are mutually exclusive — forgery is
// the stronger signal).
func checkImposterCommit(ctx context.Context, wf WorkflowInput, lf parserlock.File, depIndex map[string]parserlock.Pin, r Resolver, forgeryKeys map[string]bool) []Finding {
	if len(depIndex) == 0 {
		return nil
	}
	var out []Finding
	for _, u := range wf.Uses {
		pin, ok := depIndex[u.IndexKey()]
		if !ok {
			continue
		}
		if parserlock.IsFullSha(u.Ref) {
			continue
		}
		if forgeryKeys[u.IndexKey()] {
			continue
		}
		status := r.CheckReachability(ctx, u.Owner, u.Repo, pin.Hex, u.Ref)
		if status != ReachabilityUnreachable {
			continue
		}
		f := findingFromUse(wf, u)
		f.Code = CodeImposterCommit
		f.Severity = SeverityError
		f.LockedSha = pin.Hex
		f.Message = fmt.Sprintf("locked %s is not reachable from %s — classic fork-network imposter-commit shape", shortSha(pin.Hex), u.Ref)
		f.Remediation = "investigate immediately — the lockfile entry may have been injected"
		out = append(out, f)
	}
	_ = lf
	return out
}
