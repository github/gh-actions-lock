package checks

import (
	"strings"

	parserlock "github.com/github/actions-lockfile/go/pkg/lockfile"
	"github.com/github/gh-actions-lock/internal/dep"
)

// ParsedWorkflow holds the per-workflow parse result that both phases need.
// LoadErr / DepsErr capture early failures so DiagnoseParsed can surface them
// as findings without re-loading the file.
type ParsedWorkflow struct {
	Path          string
	Refs          []parserlock.ActionRef
	LocalPaths    []string
	ExistingDeps  []dep.Dependency
	ParseWarnings []string
	LoadErr       error
	DepsErr       error
	// Resolved, when true, instructs DiagnoseParsed to run this
	// workflow's diagnostics with a nil resolver. Network-bound checks
	// (ref-moved, impostor-commit) are skipped and the engine relies on
	// purely structural validation against the on-disk lockfile. Caller
	// is asserting "this workflow is already fully resolved" — typically
	// set on the fast path when every direct ref in the workflow is
	// already recorded in the lockfile.
	Resolved bool
	// SkipReachWhenUnchanged, when true, instructs DiagnoseParsed to skip
	// the per-dep reachability network call for any ExistingDep whose
	// (NWO, Ref, SHA) matches an entry in the freshly-resolved live deps
	// for this workflow. A Reachable result is synthesized in place. This
	// is the per-workflow analogue of the cmd-level fast path: when at
	// least one direct ref is new/changed (so the workflow couldn't be
	// fully trusted), the remaining unchanged pins still don't need a
	// fresh network reachability sweep on every run. Callers should leave
	// this false when --rescan or an equivalent "verify everything" flag
	// is in effect.
	SkipReachWhenUnchanged bool
}

// PartitionRefs splits refs into recorded (matching a lockfile entry by
// NWO@Ref or NWO@SHA) and unrecorded (need network resolution). When an
// error prevented loading refs or deps, everything is unrecorded.
func (pw ParsedWorkflow) PartitionRefs() (recorded, unrecorded []parserlock.ActionRef) {
	if pw.LoadErr != nil || pw.DepsErr != nil {
		return nil, pw.Refs
	}
	if len(pw.Refs) == 0 {
		return nil, nil
	}
	haveDep := make(map[string]bool, len(pw.ExistingDeps)*2)
	for _, d := range pw.ExistingDeps {
		nwo := strings.ToLower(d.NWO)
		haveDep[nwo+"@"+d.Ref] = true
		if d.SHA != "" {
			haveDep[nwo+"@"+strings.ToLower(d.SHA)] = true
		}
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

// RecordedDeps returns the subset of ExistingDeps whose NWO@Ref or
// NWO@SHA matches one of the given recorded refs.
func (pw ParsedWorkflow) RecordedDeps(recorded []parserlock.ActionRef) []dep.Dependency {
	refKeys := make(map[string]bool, len(recorded))
	for _, r := range recorded {
		refKeys[strings.ToLower(r.Owner+"/"+r.Repo)+"@"+r.Ref] = true
	}
	var out []dep.Dependency
	for _, d := range pw.ExistingDeps {
		nwo := strings.ToLower(d.NWO)
		if refKeys[nwo+"@"+d.Ref] || refKeys[nwo+"@"+strings.ToLower(d.SHA)] {
			out = append(out, d)
		}
	}
	return out
}
