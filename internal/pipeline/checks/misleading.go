package checks

import (
	"context"
	"fmt"
	"strings"

	parserlock "github.com/github/actions-lockfile/go/pkg/lockfile"
	"github.com/github/gh-actions-lock/internal/resolve"
)

// checkMisleadingSha emits MisleadingSHA when a uses: ref looks
// like a SHA but the resolver maps it to a different commit. Independent
// of whether a lock entry exists. An annotated-tag-object SHA pin
// (possibly through a chain of tag-of-tag) is content-addressed and
// safe even though the peeled commit differs from the ref string — those
// are skipped via PeelTagObject.
func checkMisleadingSha(ctx context.Context, pw ParsedWorkflow, r CheckResolver) []Finding {
	var out []Finding
	for _, ref := range pw.Refs {
		if !parserlock.IsFullSha(ref.Ref) {
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
		f := newRefFinding(pw, ref, MisleadingSHA, SeverityError, ConfidenceHigh)
		f.ObservedSHA = sha
		f.Dependency = synthDep(ref, ref.Ref)
		f.Detail = fmt.Sprintf("ref %s resolves to %s — the ref string looks like a SHA but isn't this commit", parserlock.ShortSHA(ref.Ref), parserlock.ShortSHA(sha))
		f.Remediation = "investigate — the ref may be a tag named after a SHA, not the SHA itself"
		out = append(out, f)
	}
	return out
}

// checkRefMovedAndForgery emits RefMoved when the upstream ref
// resolves to a different SHA than the lockfile. If CheckAncestry confirms
// the locked SHA is NOT an ancestor of the observed SHA, the finding is
// upgraded to LockfileForgery (mutually exclusive with ref-moved).
// When the observed SHA is itself unreachable from any branch of the
// upstream repo (tag-moved-to-fork-network), an additional
// lockfile-tampering claim is stronger.
func checkRefMovedAndForgery(ctx context.Context, pw ParsedWorkflow, depIndex map[string]lockedPin, r CheckResolver) []Finding {
	var out []Finding

	// Build a set of index keys already covered by direct refs so we don't
	// double-check them when iterating transitive deps below.
	directKeys := make(map[string]bool, len(pw.Refs))
	for _, ref := range pw.Refs {
		directKeys[parserlock.IndexKey(ref.Owner, ref.Repo, ref.Ref)] = true
	}

	// Check direct refs from the workflow file.
	for _, ref := range pw.Refs {
		if parserlock.IsFullSha(ref.Ref) {
			continue
		}
		pin, ok := depIndex[parserlock.IndexKey(ref.Owner, ref.Repo, ref.Ref)]
		if !ok {
			continue
		}
		if f, ok := checkOneRefMoved(ctx, pw, ref, pin, r); ok {
			out = append(out, f)
		}
	}

	// Check transitive deps (recorded in lockfile but not directly in the
	// workflow). These can also drift when upstream releases new versions.
	for indexKey, pin := range depIndex {
		if directKeys[indexKey] {
			continue
		}
		parsed, ok := parserlock.ParsePin(indexKey)
		if !ok {
			continue
		}
		if parserlock.IsFullSha(parsed.Ref) {
			continue
		}
		ref := parserlock.ActionRef{
			Owner: parsed.Owner,
			Repo:  parsed.Repo,
			Ref:   parsed.Ref,
		}
		if f, ok := checkOneRefMoved(ctx, pw, ref, pin, r); ok {
			out = append(out, f)
		}
	}

	return out
}

// checkOneRefMoved resolves a single ref and compares against the pinned SHA.
// Returns a finding and true if there's a mismatch; false if the ref is current.
func checkOneRefMoved(ctx context.Context, pw ParsedWorkflow, ref parserlock.ActionRef, pin lockedPin, r CheckResolver) (Finding, bool) {
	sha, ok := r.ResolveRef(ref.Owner, ref.Repo, ref.Ref)
	if !ok || sha == "" {
		return Finding{}, false
	}
	if strings.EqualFold(sha, pin.SHA()) {
		return Finding{}, false
	}
	ancestry, ancestryDetail := r.CheckAncestry(ctx, ref.Owner, ref.Repo, pin.SHA(), sha)
	f := newRefFinding(pw, ref, "", "", "")
	f.ObservedSHA = sha
	f.Dependency = synthDep(ref, pin.SHA())
	switch ancestry {
	case resolve.AncestryNotAncestor:
		f.Category = LockfileForgery
		f.Severity = SeverityError
		f.Confidence = ConfidenceHigh
		f.Detail = fmt.Sprintf("pinned %s is not an ancestor of %s — lockfile may have been tampered with", parserlock.ShortSHA(pin.SHA()), parserlock.ShortSHA(sha))
		f.Remediation = "investigate immediately — verify the lockfile entry against upstream history"
	case resolve.AncestryUnknown:
		// Compare API didn't reach a verdict — typically rate-limited even
		// after CheckAncestry's bounded retry. Fail-closed: emit at error
		// severity so an unverifiable move blocks rather than passing, while
		// keeping its own category so consumers can frame it as transient
		// ("retry") instead of "tampered".
		f.Category = AncestryUnknown
		f.Severity = SeverityError
		f.Confidence = ConfidenceMedium
		f.Detail = fmt.Sprintf("ref %s now resolves to %s, lockfile pins %s (ancestry check inconclusive%s)", ref.Ref, parserlock.ShortSHA(sha), parserlock.ShortSHA(pin.SHA()), suffixWith(ancestryDetail))
		f.Remediation = "retry when the Compare API is available to classify this as ref-moved or lockfile-forgery"
	default:
		f.Category = RefMoved
		f.Severity = SeverityWarning
		f.Confidence = ConfidenceHigh
		f.Detail = fmt.Sprintf("ref %s now resolves to %s, lockfile pins %s", ref.Ref, parserlock.ShortSHA(sha), parserlock.ShortSHA(pin.SHA()))
		f.Remediation = "re-run `gh actions-lock` to refresh the lock entry"
	}
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
