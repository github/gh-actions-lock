package checks

import (
	"fmt"
	"strings"

	parserlock "github.com/github/actions-lockfile/go/pkg/lockfile"
	"github.com/github/gh-actions-lock/internal/dep"
	"github.com/github/gh-actions-lock/internal/lockfile"
	"github.com/github/gh-actions-lock/internal/workflowfile"
)

func checkRecordedClosure(pw ParsedWorkflow, file parserlock.File) []Finding {
	direct := make(map[string]bool, len(pw.Refs))
	for _, ref := range pw.Refs {
		direct[parserlock.IndexKey(ref.Owner, ref.Repo, ref.Ref)] = true
	}
	for _, d := range pw.RecordedDeps(pw.Refs) {
		owner, repo := d.OwnerRepo()
		direct[parserlock.IndexKey(owner, repo, d.Ref)] = true
	}
	_, missing := lockfile.InspectClosure(file, workflowfile.KeyFromPath(pw.Path), func(pin parserlock.Pin) bool {
		return direct[pin.IndexKey()]
	})
	var out []Finding
	for _, m := range missing {
		f := Finding{
			WorkflowPath:       pw.Path,
			Category:           NotPinned,
			Severity:           SeverityError,
			Confidence:         ConfidenceHigh,
			IncompleteLockfile: true,
			TrackedAction:      true,
			Detail:             fmt.Sprintf("lockfile references %s but its dependency metadata is missing", m.Key),
			Remediation:        "restore the missing entry from version control, or run `gh actions-lock --relock` to resolve the graph again",
		}
		if pin, ok := parserlock.ParsePin(m.Key); ok {
			f.Dependency = &dep.Dependency{NWO: pin.NWO, Ref: pin.Ref}
		}
		if parent, ok := parserlock.ParsePin(m.Parent); ok {
			f.ParentNWO = parent.NWO + "@" + parent.Ref
			f.Detail += fmt.Sprintf(" (used by %s)", f.ParentNWO)
		}
		out = append(out, f)
	}
	return out
}

// checkUntracked emits NotPinned for a workflow with no uses: refs that
// has no lockfile entry. Lockfile enforcement requires every workflow to be
// listed, dependencies or not; workflows with refs already get per-ref
// NotPinned findings.
func checkUntracked(pw ParsedWorkflow, tracked bool) []Finding {
	if tracked || len(pw.Refs) > 0 {
		return nil
	}
	return []Finding{{
		WorkflowPath: pw.Path,
		Category:     NotPinned,
		Severity:     SeverityError,
		Confidence:   ConfidenceHigh,
		Remediable:   true,
		Detail:       "workflow has no lockfile entry",
		Remediation:  "run `gh actions-lock` to add it",
	}}
}

// checkNotPinned emits NotPinned for any uses: ref that has no
// matching lockfile entry. When the lockfile has an entry for the same
// action at a different ref, RefChanged wins, except for bare SHAs: an
// unlocked SHA was never history-checked (it may be a fork commit served
// under the upstream name), so it is NotPinned even if a sibling ref of
// the same repo is locked.
func checkNotPinned(pw ParsedWorkflow, depPins []lockedPin, depIndex map[string]lockedPin) []Finding {
	if len(pw.Refs) == 0 {
		return nil
	}
	knownAction := make(map[string]bool, len(depPins))
	lockedSHA := make(map[string]bool, len(depPins))
	for _, p := range depPins {
		knownAction[nwoLower(p.Owner, p.Repo)] = true
		lockedSHA[nwoLower(p.Owner, p.Repo)+"@"+strings.ToLower(p.SHA())] = true
	}
	var out []Finding
	for _, ref := range pw.Refs {
		if _, ok := depIndex[parserlock.IndexKey(ref.Owner, ref.Repo, ref.Ref)]; ok {
			continue
		}
		if parserlock.IsFullSha(ref.Ref) {
			if lockedSHA[nwoLower(ref.Owner, ref.Repo)+"@"+strings.ToLower(ref.Ref)] {
				continue
			}
		} else if knownAction[nwoLower(ref.Owner, ref.Repo)] {
			continue
		}
		f := newRefFinding(pw, ref, NotPinned, SeverityError, ConfidenceHigh)
		f.Detail = fmt.Sprintf("used in workflow but not pinned in lockfile (%s@%s)", formatUseName(ref.Owner, ref.Repo, ref.Path), ref.Ref)
		f.Remediation = "pin with `gh actions-lock`"
		f.TrackedAction = knownAction[nwoLower(ref.Owner, ref.Repo)]
		out = append(out, f)
	}
	return out
}

// checkShaAsRef emits ShaAsRef for any uses: ref that is a bare
// commit SHA — both bare-SHA uses with no lock entry and bare-SHA uses
// whose lock entry just mirrors the same SHA. The anti-pattern (no
// human-readable ref) is the same in both cases.
func checkShaAsRef(pw ParsedWorkflow, depIndex map[string]lockedPin) []Finding {
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
			lockedSha = locked.SHA()
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
func checkRefChanged(pw ParsedWorkflow, depPins []lockedPin) []Finding {
	if len(depPins) == 0 {
		return nil
	}
	pinsByAction := make(map[string][]lockedPin, len(depPins))
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
		f.Dependency = synthDep(ref, p.SHA())
		out = append(out, f)
	}
	return out
}

// checkStale emits Stale for lockfile dep entries that no uses:
// ref in the workflow references. If the workflow has already been
// rewritten to pin by SHA, the lockfile entry (keyed by the original tag)
// is still valid — surface keys both ways so we don't false-flag.
// Stale is an error: a frozen check requires the lockfile to match the
// workflows exactly, and a fix run prunes the entry.
func checkStale(pw ParsedWorkflow, depPins []lockedPin) []Finding {
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
		if p.SHA() != "" {
			nwo := strings.ToLower(p.NWO)
			if usedBySHA[nwo+"@"+strings.ToLower(p.SHA())] {
				continue
			}
		}
		f := Finding{
			WorkflowPath: pw.Path,
			Category:     Stale,
			Severity:     SeverityError,
			Confidence:   ConfidenceHigh,
			Detail:       fmt.Sprintf("lockfile pins %s@%s but no uses: in this workflow references it", nwoLower(p.Owner, p.Repo), p.Ref),
			Remediation:  "run `gh actions-lock` to prune the unused entry",
			Dependency: &dep.Dependency{
				NWO: strings.ToLower(p.NWO),
				Ref: p.Ref,
				SHA: p.SHA(),
			},
		}
		out = append(out, f)
	}
	return out
}
