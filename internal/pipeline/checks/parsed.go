package checks

import (
	parserlock "github.com/github/actions-lockfile/go/pkg/lockfile"
	"github.com/github/gh-actions-lock/internal/dep"
)

// ParsedWorkflow holds the per-workflow parse result that both phases need.
// LoadErr / DepsErr capture early failures so DiagnoseParsed can surface them
// as findings without re-loading the file.
type ParsedWorkflow struct {
	Path string
	// Refs are all remote dependency roots attributed to the workflow. This
	// includes refs found inside in-repo `$/…` actions.
	Refs []parserlock.ActionRef
	// RewriteRefs are the refs that pinning may rewrite in source: the
	// workflow YAML plus the in-repo `$/…` action files it reaches.
	RewriteRefs []parserlock.ActionRef
	// SelfActionFiles are the in-repo action definition files reached through
	// step-level `$/…` refs. They are rewritten alongside the workflow.
	SelfActionFiles    []string
	LocalPaths         []string
	SelfRepositoryRefs []string
	// SelfRepositoryRefErrs holds malformed `$/…@ref` values (the invalid form).
	SelfRepositoryRefErrs        []string
	SelfRepositoryResolutionErrs []string
	ExistingDeps                 []dep.Dependency
	ParseWarnings                []string
	LoadErr                      error
	DepsErr                      error
	// Resolved instructs DiagnoseParsed to skip network-bound checks for a
	// workflow with structural blockers.
	Resolved bool
}
