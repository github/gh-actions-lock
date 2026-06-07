package checks

import (
	"github.com/github/gh-actions-pin/internal/dep"
	parserlock "github.com/github/actions-lockfile/go/pkg/lockfile"
)

// ParsedWorkflow holds the per-workflow parse result that both phases need.
// LoadErr / DepsErr capture early failures so DiagnoseParsed can surface them
// as findings without re-loading the file.
type ParsedWorkflow struct {
	Path          string
	Refs          []parserlock.ActionRef
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
