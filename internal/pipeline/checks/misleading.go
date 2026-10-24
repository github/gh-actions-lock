package checks

import (
	"context"
	"fmt"
	"strings"
	"time"

	parserlock "github.com/github/actions-lockfile/go/pkg/lockfile"
	"github.com/github/gh-actions-lock/internal/ghapi"
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
// upgraded to UnreachablePin (mutually exclusive with ref-moved).
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
	ancestry, ancestryDetail, cause := r.CheckAncestry(ctx, ref.Owner, ref.Repo, pin.SHA(), sha)
	f := newRefFinding(pw, ref, "", "", "")
	f.ObservedSHA = sha
	f.Dependency = synthDep(ref, pin.SHA())
	switch {
	case ancestry == resolve.AncestryNotAncestor && cause != nil:
		// GitHub refused the comparison: the move is unproven either way.
		f.Category = RepoUnavailable
		f.Severity = SeverityError
		f.Confidence = ConfidenceHigh
		f.Detail = fmt.Sprintf("ref %s now resolves to %s, lockfile pins %s, and GitHub refused to compare them: %s", ref.Ref, parserlock.ShortSHA(sha), parserlock.ShortSHA(pin.SHA()), ghapi.Brief(cause))
		f.Remediation = fmt.Sprintf("use a token that can read %s/%s, then re-run", ref.Owner, ref.Repo)
	case ancestry == resolve.AncestryNotAncestor:
		f.Category = UnreachablePin
		f.Severity = SeverityError
		f.Confidence = ConfidenceHigh
		f.Detail = NotInHistoryDetail(pin.SHA(), ref.Owner+"/"+ref.Repo, ref.Ref, sha)
		f.Remediation = AcceptMovedRemediation
	case ancestry == resolve.AncestryUnknown:
		f.Category = AncestryUnknown
		f.Confidence = ConfidenceMedium
		f.Detail = fmt.Sprintf("ref %s now resolves to %s, lockfile pins %s (ancestry check inconclusive%s)", ref.Ref, parserlock.ShortSHA(sha), parserlock.ShortSHA(pin.SHA()), suffixWith(ancestryDetail))
		f.MarkRetryable(cause)
	default:
		f.MarkRefMoved()
	}
	return f, true
}

// MarkRefMoved makes f, a ref-moved check of f.Dependency whose ref now
// resolves to f.ObservedSHA, the warning for a commit confirmed in the
// ref's history.
func (f *Finding) MarkRefMoved() {
	f.Category = RefMoved
	f.Severity = SeverityWarning
	f.Confidence = ConfidenceHigh
	f.Retryable, f.RetryReason, f.RetryAt, f.RateLimited, f.AnonymousLimited = false, "", time.Time{}, false, false
	f.Detail = fmt.Sprintf("ref %s now resolves to %s, lockfile pins %s", f.Dependency.Ref, parserlock.ShortSHA(f.ObservedSHA), parserlock.ShortSHA(f.Dependency.SHA))
	target := f.Dependency.NWO + "@" + f.Dependency.Ref
	if f.ParentNWO != "" {
		// Only re-resolving the composite re-reads its pin of the ref.
		target = f.ParentNWO
	}
	f.Remediation = fmt.Sprintf("run `gh actions-lock update %s` to lock its current commit", target)
}

// AcceptMovedRemediation is the single next step for a pin the runner
// can't trust: update refuses these, so the override must be explicit.
const AcceptMovedRemediation = "verify it, then run `gh actions-lock --accept-moved`"

// UnreachableRemediation is the next step for a pin whose recorded ref no
// longer vouches for its commit. --accept-moved repairs only an entry the
// workflow alone holds; one a composite holds keeps the composite's
// recorded entry, so only changing that composite (holder, as NWO@ref)
// helps.
func UnreachableRemediation(holder, nwo string) string {
	if holder != "" {
		return fmt.Sprintf("upgrade or replace %s with a version whose pin of %s can be verified, or stop using it", holder, nwo)
	}
	return AcceptMovedRemediation
}

// TagCommitMismatchRemediation is the next step for the exact-tag entry key
// (NWO@ref) locked behind its tag. holder (NWO@ref) is the composite that
// pins it, or "" for the workflow's own use. Updating a composite rebuilds
// its closure from its manifest, so it repairs a held entry too; only the
// workflow's own use can instead move to another release.
func TagCommitMismatchRemediation(holder, key string) string {
	if holder != "" {
		return fmt.Sprintf("run `gh actions-lock update %s` to lock the tag's commit", holder)
	}
	return fmt.Sprintf("run `gh actions-lock update %s` to lock the tag's commit, or change `uses:` to the release you want", key)
}

// NotInHistoryDetail describes a locked commit missing from upstream
// history. It can't tell a force-push from a forged lockfile, so it names
// both. live, when known, is where ref points now.
func NotInHistoryDetail(locked, nwo, ref, live string) string {
	d := fmt.Sprintf("locked commit %s is not in %s's history: it was force-pushed away, or the lockfile was edited", parserlock.ShortSHA(locked), nwo)
	if live != "" {
		d += fmt.Sprintf("; %s currently points at %s", ref, parserlock.ShortSHA(live))
	}
	return d
}

// suffixWith renders an optional detail as ": <detail>" for inline
// concatenation, returning empty when detail is empty.
func suffixWith(detail string) string {
	if detail == "" {
		return ""
	}
	return ": " + detail
}
