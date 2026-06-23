package checks

import (
	"context"
	"strings"

	parserlock "github.com/github/actions-lockfile/go/pkg/lockfile"
	"github.com/github/gh-actions-lock/internal/dep"
	"github.com/github/gh-actions-lock/internal/workflowfile"
)

// RunChecks evaluates all enabled validators against the given parsed
// workflow and returns findings in catalog order. The lockfile snapshot
// scopes the structural checks; the resolver enables the resolver-bound
// checks (misleading-sha, ref-moved, forgery, impostor). When r is nil,
// resolver-bound checks are skipped silently.
//
// Returned findings have their primitive fields populated, plus
// ActionRef (for direct uses) and Dependency (for ref-tied entries).
// DocURL and ParentNWO are attached by the caller (diagnoseOneParsed)
// because they need lookup tables runChecks doesn't carry.
func RunChecks(ctx context.Context, pw ParsedWorkflow, lf parserlock.File, r CheckResolver) []Finding {
	wfEntry, _ := lf.LookupWorkflow(workflowfile.KeyFromPath(pw.Path))
	depPins, depIndex := parseWorkflowDeps(wfEntry, lf.Dependencies)

	var out []Finding
	out = append(out, checkNotPinned(pw, depPins, depIndex)...)
	out = append(out, checkShaAsRef(pw, depIndex)...)
	out = append(out, checkRefChanged(pw, depPins)...)
	out = append(out, checkStale(pw, depPins)...)

	if r != nil {
		out = append(out, checkMisleadingSha(ctx, pw, r)...)
		refMoved := checkRefMovedAndForgery(ctx, pw, depIndex, r)
		out = append(out, refMoved...)
		out = append(out, checkImpostorCommit(pw, depIndex, r, collectForgeryKeys(refMoved))...)
	}
	return out
}

// lockedPin pairs a parsed Pin with the commit hash from the lockfile's
// Action.Commit field. Pin keys no longer embed the hash (v0.0.2 schema),
// so the SHA must be retrieved from the Action metadata.
type lockedPin struct {
	parserlock.Pin
	Commit string // full "algo-hex" from Action.Commit
}

// SHA returns just the hex portion of the commit (strips "algo-" prefix).
func (lp lockedPin) SHA() string {
	if idx := strings.Index(lp.Commit, "-"); idx >= 0 {
		return lp.Commit[idx+1:]
	}
	return lp.Commit
}

// parseWorkflowDeps decodes a workflow's dependency pin strings into Pins
// plus an index keyed by "owner/repo@ref" carrying commit info from the
// lockfile. Unparseable entries are dropped silently.
func parseWorkflowDeps(rawDeps []string, deps map[string]parserlock.Action) ([]lockedPin, map[string]lockedPin) {
	pins := make([]lockedPin, 0, len(rawDeps))
	idx := make(map[string]lockedPin, len(rawDeps))
	for _, raw := range rawDeps {
		pin, ok := parserlock.ParsePin(raw)
		if !ok {
			continue
		}
		lp := lockedPin{Pin: pin}
		if action, found := deps[raw]; found {
			lp.Commit = action.Commit
		}
		pins = append(pins, lp)
		idx[pin.IndexKey()] = lp
	}
	return pins, idx
}

// collectForgeryKeys returns the set of IndexKeys flagged as forgery so
// the impostor check can skip them.
func collectForgeryKeys(ff []Finding) map[string]bool {
	if len(ff) == 0 {
		return nil
	}
	out := make(map[string]bool)
	for _, f := range ff {
		if f.Category != LockfileForgery || f.ActionRef == nil {
			continue
		}
		ar := f.ActionRef
		out[parserlock.IndexKey(ar.Owner, ar.Repo, ar.Ref)] = true
	}
	return out
}

// newRefFinding builds a Finding with the common header fields populated
// from a uses: ref. Category/Severity can be empty when the caller fills
// them in based on a downstream branch (e.g. ref-moved vs forgery).
// Confidence is required at construction — see Finding.Confidence.
func newRefFinding(pw ParsedWorkflow, ref parserlock.ActionRef, cat Category, sev Severity, conf Confidence) Finding {
	refCopy := ref
	return Finding{
		WorkflowPath: pw.Path,
		Category:     cat,
		Severity:     sev,
		Confidence:   conf,
		ActionRef:    &refCopy,
	}
}

// synthDep builds a dep.Dependency from an ActionRef + locked SHA.
// Used by checks that surface lock-state but don't have a real
// Dependency pointer from the store.
func synthDep(ref parserlock.ActionRef, sha string) *dep.Dependency {
	return &dep.Dependency{
		NWO:  ref.Owner + "/" + ref.Repo,
		Path: ref.Path,
		Ref:  ref.Ref,
		SHA:  sha,
	}
}

// nwoLower returns lowercased "owner/repo". Trivial but used in several
// validators; centralized so the casing rule lives in one place.
func nwoLower(owner, repo string) string {
	return strings.ToLower(owner) + "/" + strings.ToLower(repo)
}

// formatUseName renders "owner/repo" or "owner/repo/path" for human
// messages. Index/lookup keys are always at owner/repo granularity.
func formatUseName(owner, repo, path string) string {
	s := nwoLower(owner, repo)
	if path != "" {
		s += "/" + path
	}
	return s
}
