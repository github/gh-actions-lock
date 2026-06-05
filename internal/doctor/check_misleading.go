package doctor

import (
	"context"
	"fmt"
	"strings"

	"github.com/github/gh-actions-pin/internal/lockfile"
	"github.com/github/gh-actions-pin/internal/resolver"
)

// checkMisleadingSha emits CategoryMisleadingSHA when a uses: ref looks
// like a SHA but the resolver maps it to a different commit. Independent
// of whether a lock entry exists. An annotated-tag-object SHA pin
// (possibly through a chain of tag-of-tag) is content-addressed and
// safe even though the peeled commit differs from the ref string — those
// are skipped via PeelTagObject.
func checkMisleadingSha(ctx context.Context, pw ParsedWorkflow, r checkResolver) []Finding {
	var out []Finding
	for _, ref := range pw.Refs {
		if !lockfile.IsFullSha(ref.Ref) {
			continue
		}
		sha, ok := r.ResolveRef(ref.Owner, ref.Repo, ref.Ref)
		if !ok || sha == "" {
			continue
		}
		if strings.EqualFold(sha, ref.Ref) {
			continue
		}
		if peeled, ok := r.PeelTagObject(ctx, ref.Owner, ref.Repo, ref.Ref); ok && strings.EqualFold(peeled, sha) {
			continue
		}
		// Ref string is SHA-shaped, resolver returned a different commit,
		// and PeelTagObject ruled out the legitimate tag-object shape.
		f := newRefFinding(pw, ref, CategoryMisleadingSHA, SeverityError, ConfidenceHigh)
		f.ObservedSHA = sha
		f.Dependency = synthDep(ref, ref.Ref)
		f.Detail = fmt.Sprintf("ref %s resolves to %s — the ref string looks like a SHA but isn't this commit", shortSha(ref.Ref), shortSha(sha))
		f.Remediation = "investigate — the ref may be a tag named after a SHA, not the SHA itself"
		out = append(out, f)
	}
	return out
}

// checkRefMovedAndForgery emits CategoryRefMoved when the upstream ref
// resolves to a different SHA than the lockfile. If CheckAncestry confirms
// the locked SHA is NOT an ancestor of the observed SHA, the finding is
// upgraded to CategoryLockfileForgery (mutually exclusive with ref-moved).
// When the observed SHA is itself unreachable from any branch of the
// upstream repo (tag-moved-to-fork-network), an additional
// CategoryImpostorCommit finding is emitted alongside ref-moved /
// ancestry-unknown. Forgery suppresses the observed-SHA impostor: the
// lockfile-tampering claim is stronger.
func checkRefMovedAndForgery(ctx context.Context, pw ParsedWorkflow, depIndex map[string]lockfile.Pin, r checkResolver) []Finding {
	var out []Finding
	for _, ref := range pw.Refs {
		if lockfile.IsFullSha(ref.Ref) {
			continue
		}
		pin, ok := depIndex[lockfile.IndexKey(ref.Owner, ref.Repo, ref.Ref)]
		if !ok {
			continue
		}
		sha, ok := r.ResolveRef(ref.Owner, ref.Repo, ref.Ref)
		if !ok || sha == "" {
			continue
		}
		if strings.EqualFold(sha, pin.Hex) {
			continue
		}
		ancestry, ancestryDetail := r.CheckAncestry(ctx, ref.Owner, ref.Repo, pin.Hex, sha)
		f := newRefFinding(pw, ref, "", "", "")
		f.ObservedSHA = sha
		f.Dependency = synthDep(ref, pin.Hex)
		switch ancestry {
		case resolver.AncestryNotAncestor:
			// Compare API gave an authoritative not-an-ancestor verdict.
			// Forgery wins: don't double-flag with an observed-SHA
			// impostor finding.
			f.Category = CategoryLockfileForgery
			f.Severity = SeverityError
			f.Confidence = ConfidenceHigh
			f.Detail = fmt.Sprintf("pinned %s is not an ancestor of %s — lockfile may have been tampered with", shortSha(pin.Hex), shortSha(sha))
			f.Remediation = "investigate immediately — verify the lockfile entry against upstream history"
			out = append(out, f)
		case resolver.AncestryUnknown:
			// Compare API didn't reach a verdict — typically rate-limited
			// even after CheckAncestry's bounded retry. Surface as its
			// own category so consumers don't conflate inconclusive with
			// benign, and append the resolver's detail so the operator
			// sees why.
			f.Category = CategoryAncestryUnknown
			f.Severity = SeverityWarning
			f.Confidence = ConfidenceMedium
			f.Detail = fmt.Sprintf("ref %s now resolves to %s, lockfile pins %s (ancestry check inconclusive%s)", ref.Ref, shortSha(sha), shortSha(pin.Hex), suffixWith(ancestryDetail))
			f.Remediation = "retry when the Compare API is available to classify this as ref-moved or lockfile-forgery"
			out = append(out, f)
			// Inconclusive ancestry doesn't block a branch_commits check
			// on the observed SHA.
			if imp, ok := liveRefImpostorFinding(pw, ref, sha, r); ok {
				out = append(out, imp)
			}
		default:
			// AncestryConfirmed: routine release.
			f.Category = CategoryRefMoved
			f.Severity = SeverityWarning
			f.Confidence = ConfidenceHigh
			f.Detail = fmt.Sprintf("ref %s now resolves to %s, lockfile pins %s", ref.Ref, shortSha(sha), shortSha(pin.Hex))
			f.Remediation = "re-run `gh actions-pin` to refresh the lock entry"
			out = append(out, f)
			if imp, ok := liveRefImpostorFinding(pw, ref, sha, r); ok {
				out = append(out, imp)
			}
		}
	}
	return out
}

// liveRefImpostorFinding returns an impostor-commit finding when the
// observed SHA is not reachable from any branch of the upstream repo
// (the tag-hijacked-to-fork-network shape). Unknown reachability fails
// open. Caller must suppress this in the forgery branch.
func liveRefImpostorFinding(pw ParsedWorkflow, ref lockfile.ActionRef, observedSHA string, r checkResolver) (Finding, bool) {
	status := r.CheckReachability(ref.Owner, ref.Repo, observedSHA, ref.Ref)
	if status != resolver.Unreachable {
		return Finding{}, false
	}
	f := newRefFinding(pw, ref, CategoryImpostorCommit, SeverityError, ConfidenceHigh)
	f.ObservedSHA = observedSHA
	f.Dependency = synthDep(ref, observedSHA)
	f.Detail = fmt.Sprintf("ref %s now resolves to %s — not on any branch of %s/%s (fork-network injection)", ref.Ref, shortSha(observedSHA), ref.Owner, ref.Repo)
	f.Remediation = "investigate immediately — the upstream ref has been moved to a commit that is not in this repo's branch history"
	return f, true
}

// suffixWith renders an optional detail as ": <detail>" for inline
// concatenation, returning empty when detail is empty.
func suffixWith(detail string) string {
	if detail == "" {
		return ""
	}
	return ": " + detail
}

// checkImpostorCommit emits CategoryImpostorCommit when the locked SHA is
// not reachable from the ref's history. Skips entries already covered by
// a forgery finding (forgery is the stronger signal).
func checkImpostorCommit(pw ParsedWorkflow, depIndex map[string]lockfile.Pin, r checkResolver, forgeryKeys map[string]bool) []Finding {
	if len(depIndex) == 0 {
		return nil
	}
	var out []Finding
	for _, ref := range pw.Refs {
		pin, ok := depIndex[lockfile.IndexKey(ref.Owner, ref.Repo, ref.Ref)]
		if !ok {
			continue
		}
		if lockfile.IsFullSha(ref.Ref) {
			continue
		}
		if forgeryKeys[lockfile.IndexKey(ref.Owner, ref.Repo, ref.Ref)] {
			continue
		}
		status := r.CheckReachability(ref.Owner, ref.Repo, pin.Hex, ref.Ref)
		if status != resolver.Unreachable {
			continue
		}
		// branch_commits gave an authoritative answer: the locked SHA is
		// not on any branch of the upstream repo (fork-network impostor).
		f := newRefFinding(pw, ref, CategoryImpostorCommit, SeverityError, ConfidenceHigh)
		f.Dependency = synthDep(ref, pin.Hex)
		f.Detail = fmt.Sprintf("locked %s is not reachable from %s — classic fork-network impostor-commit shape", shortSha(pin.Hex), ref.Ref)
		f.Remediation = "investigate immediately — the lockfile entry may have been injected"
		out = append(out, f)
	}
	return out
}
