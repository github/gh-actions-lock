package checks

import (
	"strings"

	parserlock "github.com/github/actions-lockfile/go/pkg/lockfile"
	"github.com/github/gh-actions-lock/internal/dep"
	"github.com/github/gh-actions-lock/internal/uses"
	"github.com/github/gh-actions-lock/internal/workflowfile"
)

// ParsedWorkflow holds the per-workflow parse result that both phases need.
// LoadErr / DepsErr capture early failures so DiagnoseParsed can surface them
// as findings without re-loading the file.
type ParsedWorkflow struct {
	Path string
	// Refs are all remote dependency roots attributed to the workflow. This
	// includes refs found inside in-repo `$/…` actions.
	Refs []parserlock.ActionRef
	// RewriteRefs are the refs that pinning may rewrite in the workflow file:
	// those no in-repo `$/…` action file it reaches also holds.
	RewriteRefs []parserlock.ActionRef
	// WorkflowRefs are the remote refs the workflow file itself writes.
	WorkflowRefs []parserlock.ActionRef
	// SelfActionRefs are the remote refs found inside the in-repo action
	// files reached through step-level `$/…` refs.
	SelfActionRefs []parserlock.ActionRef
	// SelfActionFiles maps workflowfile.RefFileKey of each SelfActionRefs
	// entry to the repo-relative action files that use it.
	SelfActionFiles map[string][]string
	// SHASpellings maps each `uses:` value that spells a full SHA in
	// uppercase, as the source has it, to its lowercase form, which Refs
	// carry so lockfile lookups see one spelling.
	SHASpellings map[string]string
	// LocalPaths are the workflow's own `./…` uses.
	LocalPaths []string
	// NestedLocalUses are `./…` uses inside the in-repo `$/…` actions it
	// reaches, with the action that contains each.
	NestedLocalUses    []workflowfile.LocalUse
	SelfRepositoryRefs []string
	// SelfRepositoryRefErrs holds malformed `$/…@ref` values (the invalid form).
	SelfRepositoryRefErrs        []string
	SelfRepositoryResolutionErrs []string
	ExistingDeps                 []dep.Dependency
	// Images are the mutable container images the workflow runs directly.
	Images  []uses.Image
	LoadErr error
	DepsErr error
	// Resolved, when true, instructs DiagnoseParsed to run this
	// workflow's diagnostics with a nil resolver. Network-bound checks
	// (ref-moved) are skipped and the engine relies on
	// purely structural validation against the on-disk lockfile. Caller
	// is asserting "this workflow is already fully resolved" — typically
	// set on the fast path when every direct ref in the workflow is
	// already recorded in the lockfile.
	Resolved bool
}

// PartitionRefs splits refs into recorded (matching a lockfile key
// exactly, the rule lockfile.RootUse applies) and unrecorded (need
// network resolution). A full-SHA use locked only under its tag is
// unrecorded, so it gets resolved and its closure checked. When an error
// prevented loading refs or deps, everything is unrecorded.
func (pw ParsedWorkflow) PartitionRefs() (recorded, unrecorded []parserlock.ActionRef) {
	if pw.LoadErr != nil || pw.DepsErr != nil {
		return nil, pw.Refs
	}
	if len(pw.Refs) == 0 {
		return nil, nil
	}
	haveDep := make(map[string]bool, len(pw.ExistingDeps))
	for _, d := range pw.ExistingDeps {
		haveDep[strings.ToLower(d.NWO)+"@"+d.Ref] = true
	}
	for _, r := range pw.Refs {
		if haveDep[strings.ToLower(r.Owner+"/"+r.Repo)+"@"+r.Ref] {
			recorded = append(recorded, r)
		} else {
			unrecorded = append(unrecorded, r)
		}
	}
	return recorded, unrecorded
}

// IsFullyRecorded returns true when every direct ref has a matching
// lockfile entry — the steady-state happy path.
func (pw ParsedWorkflow) IsFullyRecorded() bool {
	_, unrecorded := pw.PartitionRefs()
	return len(pw.Refs) == 0 || len(unrecorded) == 0
}

// RecordedDeps returns the subset of ExistingDeps whose NWO@Ref matches
// one of the given recorded refs.
func (pw ParsedWorkflow) RecordedDeps(recorded []parserlock.ActionRef) []dep.Dependency {
	refKeys := make(map[string]bool, len(recorded))
	for _, r := range recorded {
		refKeys[strings.ToLower(r.Owner+"/"+r.Repo)+"@"+r.Ref] = true
	}
	var out []dep.Dependency
	for _, d := range pw.ExistingDeps {
		if refKeys[strings.ToLower(d.NWO)+"@"+d.Ref] {
			out = append(out, d)
		}
	}
	return out
}
