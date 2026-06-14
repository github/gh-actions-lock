package checks

import (
	"fmt"
	"strings"

	parserlock "github.com/github/actions-lockfile/go/pkg/lockfile"
	"github.com/github/gh-actions-lock/internal/dep"
)

// checkNotPinned emits NotPinned for any uses: ref that has no
// matching lockfile entry. SHA-shaped refs are reported under their own
// category. When the lockfile has an entry for the same action at a
// different ref, RefChanged wins.
func checkNotPinned(pw ParsedWorkflow, depPins []parserlock.Pin, depIndex map[string]parserlock.Pin) []Finding {
	if len(pw.Refs) == 0 {
		return nil
	}
	knownAction := make(map[string]bool, len(depPins))
	for _, p := range depPins {
		knownAction[nwoLower(p.Owner, p.Repo)] = true
	}
	var out []Finding
	for _, ref := range pw.Refs {
		if parserlock.IsFullSha(ref.Ref) {
			continue
		}
		if _, ok := depIndex[parserlock.IndexKey(ref.Owner, ref.Repo, ref.Ref)]; ok {
			continue
		}
		if knownAction[nwoLower(ref.Owner, ref.Repo)] {
			continue
		}
		f := newRefFinding(pw, ref, NotPinned, SeverityError, ConfidenceHigh)
		f.Detail = fmt.Sprintf("used in workflow but not pinned in lockfile (%s@%s)", formatUseName(ref.Owner, ref.Repo, ref.Path), ref.Ref)
		f.Remediation = "pin with `gh actions-lock`"
		out = append(out, f)
	}
	return out
}

// checkShaAsRef emits ShaAsRef for any uses: ref that is a bare
// commit SHA — both bare-SHA uses with no lock entry and bare-SHA uses
// whose lock entry just mirrors the same SHA. The anti-pattern (no
// human-readable ref) is the same in both cases.
func checkShaAsRef(pw ParsedWorkflow, depIndex map[string]parserlock.Pin) []Finding {
	var out []Finding
	for _, ref := range pw.Refs {
		if !parserlock.IsFullSha(ref.Ref) {
			continue
		}
		f := newRefFinding(pw, ref, ShaAsRef, SeverityWarning, ConfidenceHigh)
		f.Detail = "pinned to a bare SHA without a symbolic ref — weakens supply-chain traceability"
		f.Remediation = fmt.Sprintf("pin to a tag instead: https://github.com/%s/releases", nwoLower(ref.Owner, ref.Repo))
		lockedSha := ref.Ref
		if locked, ok := depIndex[parserlock.IndexKey(ref.Owner, ref.Repo, ref.Ref)]; ok {
			lockedSha = locked.Hex
		}
		f.Dependency = synthDep(ref, lockedSha)
		out = append(out, f)
	}
	return out
}

// checkRefChanged emits RefChanged when the workflow's uses: ref
// differs from the lockfile entry's ref for the same action (owner/repo).
// A single action may legitimately have multiple pinned refs across
// workflows, so this only fires when no pin matches the workflow's ref.
func checkRefChanged(pw ParsedWorkflow, depPins []parserlock.Pin) []Finding {
	if len(depPins) == 0 {
		return nil
	}
	pinsByAction := make(map[string][]parserlock.Pin, len(depPins))
	for _, p := range depPins {
		k := nwoLower(p.Owner, p.Repo)
		pinsByAction[k] = append(pinsByAction[k], p)
	}
	var out []Finding
	for _, ref := range pw.Refs {
		if parserlock.IsFullSha(ref.Ref) {
			continue
		}
		key := nwoLower(ref.Owner, ref.Repo)
		candidates, ok := pinsByAction[key]
		if !ok {
			continue
		}
		match := false
		for _, p := range candidates {
			if p.Ref == ref.Ref {
				match = true
				break
			}
		}
		if match {
			continue
		}
		p := candidates[0]
		f := newRefFinding(pw, ref, RefChanged, SeverityError, ConfidenceHigh)
		f.Detail = fmt.Sprintf("workflow uses ref %q but lockfile pins %q", ref.Ref, p.Ref)
		f.Remediation = "re-run `gh actions-lock` to refresh the lockfile, or revert the uses: line"
		f.Dependency = synthDep(ref, p.Hex)
		out = append(out, f)
	}
	return out
}

// checkStale emits Stale for lockfile dep entries that no uses:
// ref in the workflow references. If the workflow has already been
// rewritten to pin by SHA, the lockfile entry (keyed by the original tag)
// is still valid — surface keys both ways so we don't false-flag.
func checkStale(pw ParsedWorkflow, depPins []parserlock.Pin) []Finding {
	if len(depPins) == 0 {
		return nil
	}
	used := make(map[string]bool, len(pw.Refs))
	usedBySHA := make(map[string]bool, len(pw.Refs))
	for _, ref := range pw.Refs {
		used[parserlock.IndexKey(ref.Owner, ref.Repo, ref.Ref)] = true
		nwo := strings.ToLower(ref.Owner + "/" + ref.Repo)
		usedBySHA[nwo+"@"+strings.ToLower(ref.Ref)] = true
	}
	var out []Finding
	for _, p := range depPins {
		if used[p.IndexKey()] {
			continue
		}
		if p.Hex != "" {
			nwo := strings.ToLower(p.NWO)
			if usedBySHA[nwo+"@"+strings.ToLower(p.Hex)] {
				continue
			}
		}
		f := Finding{
			WorkflowPath: pw.Path,
			Category:     Stale,
			Severity:     SeverityWarning,
			Confidence:   ConfidenceHigh,
			Detail:       fmt.Sprintf("lockfile pins %s@%s but no uses: in this workflow references it", nwoLower(p.Owner, p.Repo), p.Ref),
			Remediation:  "remove the entry or re-run `gh actions-lock`",
			Dependency: &dep.Dependency{
				NWO: strings.ToLower(p.NWO),
				Ref: p.Ref,
				SHA: p.Hex,
			},
		}
		out = append(out, f)
	}
	return out
}
