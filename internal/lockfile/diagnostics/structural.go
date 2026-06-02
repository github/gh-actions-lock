package diagnostics

import (
	"fmt"

	parserlock "github.com/github/gh-actions-pin/pkg/lockfile"
)

// formatUseName renders "owner/repo" or "owner/repo/path" for a uses:
// reference. Used for human-readable diagnostic messages — index/lookup
// keys are always at owner/repo granularity.
func formatUseName(owner, repo, path string) string {
	s := nwo(owner, repo)
	if path != "" {
		s += "/" + path
	}
	return s
}

// checkNotPinned emits NOT_PINNED for any uses: ref that has no matching
// lockfile entry. When the workflow has no lockfile entry at all and at
// least one tagged ref is present, a single workflow-level NOT_PINNED is
// emitted in addition to per-ref findings — surfaces aggregate it for
// "this whole workflow is unpinned" messaging.
func checkNotPinned(wf WorkflowInput, hasWorkflowEntry bool, depPins []parserlock.Pin, depIndex map[string]parserlock.Pin) []Finding {
	if len(wf.Uses) == 0 {
		return nil
	}

	// Build a same-action lookup (owner/repo → known) so REF_CHANGED can
	// win over NOT_PINNED for the "wrong ref" case.
	knownAction := make(map[string]bool, len(depPins))
	for _, p := range depPins {
		knownAction[nwo(p.Owner, p.Repo)] = true
	}

	var out []Finding
	taggedUnpinned := 0
	for _, u := range wf.Uses {
		// SHA-as-ref shape is reported under its own code, not as NOT_PINNED.
		if parserlock.IsFullSha(u.Ref) {
			continue
		}
		if _, ok := depIndex[u.IndexKey()]; ok {
			continue
		}
		if knownAction[nwo(u.Owner, u.Repo)] {
			// Lock entry exists for this action but at a different ref;
			// REF_CHANGED covers it.
			continue
		}
		taggedUnpinned++
		f := findingFromUse(wf, u)
		f.Code = CodeNotPinned
		f.Severity = SeverityError
		f.Message = fmt.Sprintf("used in workflow but not pinned in lockfile (%s@%s)", formatUseName(u.Owner, u.Repo, u.Path), u.Ref)
		f.Remediation = "pin with `gh actions-pin`"
		out = append(out, f)
	}

	if !hasWorkflowEntry && taggedUnpinned == 0 {
		// All uses were SHA-as-ref or already covered — no workflow-level
		// summary needed.
		return out
	}
	return out
}

// checkShaAsRef emits SHA_AS_REF for any uses: ref that is a bare SHA.
// It surfaces both bare-SHA uses with no lock entry and bare-SHA uses
// whose lock entry just mirrors the same SHA — the anti-pattern is the
// same in both cases.
func checkShaAsRef(wf WorkflowInput, depIndex map[string]parserlock.Pin) []Finding {
	var out []Finding
	for _, u := range wf.Uses {
		if !parserlock.IsFullSha(u.Ref) {
			continue
		}
		f := findingFromUse(wf, u)
		f.Code = CodeShaAsRef
		f.Severity = SeverityWarning
		f.Message = "pinned to a bare SHA without a symbolic ref — weakens supply-chain traceability"
		f.Remediation = fmt.Sprintf("pin to a tag instead: https://github.com/%s/releases", nwo(u.Owner, u.Repo))
		if locked, ok := depIndex[u.IndexKey()]; ok {
			f.LockedSha = locked.Hex
		} else {
			// Even without a lock entry, the ref string is the SHA, so
			// surface it as the locked value for downstream rendering.
			f.LockedSha = u.Ref
		}
		out = append(out, f)
	}
	return out
}

// checkRefChanged emits REF_CHANGED when the workflow's uses: ref differs
// from the lockfile entry's ref for the same action (owner/repo).
func checkRefChanged(wf WorkflowInput, depPins []parserlock.Pin) []Finding {
	if len(depPins) == 0 {
		return nil
	}
	// Group lock pins by same-action key. A single action may legitimately
	// have multiple pinned refs (e.g. v3 and v4 both in use across files),
	// so REF_CHANGED only fires when no pin matches the workflow's ref.
	pinsByAction := make(map[string][]parserlock.Pin, len(depPins))
	for _, p := range depPins {
		k := nwo(p.Owner, p.Repo)
		pinsByAction[k] = append(pinsByAction[k], p)
	}

	var out []Finding
	for _, u := range wf.Uses {
		if parserlock.IsFullSha(u.Ref) {
			continue
		}
		key := nwo(u.Owner, u.Repo)
		candidates, ok := pinsByAction[key]
		if !ok {
			continue
		}
		match := false
		for _, p := range candidates {
			if p.Ref == u.Ref {
				match = true
				break
			}
		}
		if match {
			continue
		}
		// Pick the first known pin to surface what the lockfile thinks the
		// ref is. Surfaces can render multiple candidates if they want.
		p := candidates[0]
		f := findingFromUse(wf, u)
		f.Code = CodeRefChanged
		f.Severity = SeverityError
		f.LockedSha = p.Hex
		f.Message = fmt.Sprintf("workflow uses ref %q but lockfile pins %q", u.Ref, p.Ref)
		f.Remediation = "re-run `gh actions-pin` to refresh the lockfile, or revert the uses: line"
		out = append(out, f)
	}
	return out
}

// checkStale emits STALE for lockfile dep entries that no uses: ref in
// the workflow references.
func checkStale(wf WorkflowInput, depPins []parserlock.Pin) []Finding {
	if len(depPins) == 0 {
		return nil
	}
	used := make(map[string]bool, len(wf.Uses))
	for _, u := range wf.Uses {
		used[u.IndexKey()] = true
	}

	var out []Finding
	for _, p := range depPins {
		if used[p.IndexKey()] {
			continue
		}
		f := findingFromPin(wf.Path, p)
		f.Code = CodeStale
		f.Severity = SeverityWarning
		f.Message = fmt.Sprintf("lockfile pins %s@%s but no uses: in this workflow references it", nwo(p.Owner, p.Repo), p.Ref)
		f.Remediation = "remove the entry or re-run `gh actions-pin`"
		out = append(out, f)
	}
	return out
}
