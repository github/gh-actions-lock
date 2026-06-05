package doctor

import (
	"strings"

	"github.com/github/gh-actions-pin/internal/lockfile"
	parserlock "github.com/github/gh-actions-pin/pkg/lockfile"
)

// runChecks evaluates all enabled validators against the given parsed
// workflow and returns findings in catalog order. The lockfile snapshot
// scopes the structural checks; the resolver enables the resolver-bound
// checks (misleading_sha, ref_moved, forgery, impostor). When r is nil,
// resolver-bound checks are skipped silently.
//
// Returned findings have their primitive fields populated, plus
// ActionRef (for direct uses) and Dependency (for ref-tied entries).
// DocURL and ParentNWO are attached by the caller (diagnoseOneParsed)
// because they need lookup tables runChecks doesn't carry.
func runChecks(pw ParsedWorkflow, lf parserlock.File, r checkResolver) []Finding {
	wfEntry, _ := lf.LookupWorkflow(lockfile.WorkflowKeyFromPath(pw.Path))
	depPins, depIndex := parseWorkflowDeps(wfEntry)

	var out []Finding
	out = append(out, checkNotPinned(pw, depPins, depIndex)...)
	out = append(out, checkShaAsRef(pw, depIndex)...)
	out = append(out, checkRefChanged(pw, depPins)...)
	out = append(out, checkStale(pw, depPins)...)

	if r != nil {
		out = append(out, checkMisleadingSha(pw, r)...)
		refMoved := checkRefMovedAndForgery(pw, depIndex, r)
		out = append(out, refMoved...)
		out = append(out, checkImpostorCommit(pw, depIndex, r, collectForgeryKeys(refMoved))...)
	}
	return out
}

// parseWorkflowDeps decodes a workflow's dependency pin strings into Pins
// plus an index keyed by "owner/repo@ref". Unparseable entries are
// dropped silently — they're surfaced separately by parserlock.Parse callers.
func parseWorkflowDeps(rawDeps []string) ([]parserlock.Pin, map[string]parserlock.Pin) {
	pins := make([]parserlock.Pin, 0, len(rawDeps))
	idx := make(map[string]parserlock.Pin, len(rawDeps))
	for _, raw := range rawDeps {
		pin, ok := parserlock.ParsePin(raw)
		if !ok {
			continue
		}
		pins = append(pins, pin)
		idx[pin.IndexKey()] = pin
	}
	return pins, idx
}

// collectForgeryKeys returns the set of IndexKeys flagged as forgery so
// the impostor check can skip them.
func collectForgeryKeys(findings []Finding) map[string]bool {
	if len(findings) == 0 {
		return nil
	}
	out := make(map[string]bool)
	for _, f := range findings {
		if f.Category != CategoryLockfileForgery || f.ActionRef == nil {
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

// synthDep builds a lockfile.Dependency from an ActionRef + locked SHA.
// Used by checks that surface lock-state but don't have a real
// Dependency pointer from the store.
func synthDep(ref parserlock.ActionRef, sha string) *lockfile.Dependency {
	return &lockfile.Dependency{
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

// shortSha returns the first 12 chars of a SHA, or the SHA itself if shorter.
func shortSha(s string) string {
	if len(s) <= 12 {
		return s
	}
	return s[:12]
}
