package checks

import (
	parserlock "github.com/github/actions-lockfile/go/pkg/lockfile"
	"github.com/github/gh-actions-lock/internal/dep"
)

// Finding represents a single diagnosed issue (or clean bill) for a workflow.
type Finding struct {
	// WorkflowPath is the path to the workflow file.
	WorkflowPath string
	// Category of the finding.
	Category Category
	// Severity of the finding.
	Severity Severity
	// Confidence of the finding — see the Confidence type docs. Always
	// populated at construction; an empty value is a bug and the
	// no-empty-confidence test will catch it.
	Confidence Confidence
	// ActionRef is the action reference this finding relates to (nil for workflow-level findings).
	ActionRef *parserlock.ActionRef
	// Dependency is the existing pinned dep if any.
	Dependency *dep.Dependency
	// ParentNWO is the dep key of the direct action that pulls in this transitive dep (empty if direct).
	ParentNWO string
	// Detail is a human-readable explanation.
	Detail string
	// Remediation describes what the check command can do about it.
	Remediation string
	// ObservedSHA is the SHA the resolver got at scan time, recorded when
	// it differs from the pinned SHA (e.g. ref-moved, misleading-sha,
	// unreachable-pin).
	ObservedSHA string
	// DocURL points to docs explaining the finding. Populated by the
	// engine adapter so it's parity-aligned with the editor's
	// codeDescription link; "" when no URL is mapped.
	DocURL string
	// RecommendedTag is the most recent stable tag whose commit is
	// reachable from a branch, populated for unreachable-SHA findings
	// when one can be found. Empty otherwise.
	RecommendedTag string
	// RecommendedSHA is the commit SHA the recommended tag points to.
	RecommendedSHA string
	// RecommendedSearched is true when the release walk ran for this
	// finding (regardless of outcome). Lets renderers distinguish
	// "we didn't look" from "we looked and found nothing."
	RecommendedSearched bool
}

// InventoryEntry describes a single dependency with context.
type InventoryEntry struct {
	Dep    dep.Dependency
	File   string
	Direct bool
	// Parents lists the dep keys of parent composite actions that pull in this
	// transitive dependency. Empty for direct dependencies.
	Parents []string
}

// WorkflowReport aggregates all findings for a single workflow file.
type WorkflowReport struct {
	Path     string
	Findings []Finding
	// ActionRefs are all remote dependency roots attributed to the workflow,
	// including refs found inside in-repo `$/…` actions.
	ActionRefs []parserlock.ActionRef
	// RewriteRefs are the workflow-YAML refs eligible for source rewriting.
	RewriteRefs []parserlock.ActionRef
	// Deps are the existing pinned dependencies (nil if not pinned).
	Deps []dep.Dependency
	// Inventory lists all dependencies with direct/transitive classification.
	Inventory []InventoryEntry
	// ParseWarnings from ExtractActionRefs (e.g. malformed uses: lines).
	ParseWarnings []string
}

// NeedsAttention returns true if this workflow has any non-OK findings.
func (r *WorkflowReport) NeedsAttention() bool {
	for _, f := range r.Findings {
		if f.Category.IsInconclusive() {
			continue
		}
		switch f.Category {
		case Valid, RunOnly, LocalAction, MisleadingSHA, RefMoved, VersionRef, SelfRepositoryAction:
			continue
		default:
			return true
		}
	}
	return false
}

// CountByCategory returns the number of findings with the given category.
func (r *WorkflowReport) CountByCategory(c Category) int {
	n := 0
	for _, f := range r.Findings {
		if f.Category == c {
			n++
		}
	}
	return n
}

// IsValid returns true for findings that don't represent integrity violations.
func (f *Finding) IsValid() bool {
	if f.Severity == SeverityError {
		return false
	}
	if f.Category.IsInconclusive() {
		return true
	}
	switch f.Category {
	case Valid, RunOnly, LocalAction, ShaAsRef, RefMoved, VersionRef, OnboardingRequired, StaleWorkflow, SelfRepositoryAction:
		return true
	case NotPinned:
		return f.ActionRef == nil // workflow-level is a warning
	default:
		return false
	}
}

// IsWarning returns true for findings that should render as warnings (not errors).
func (f *Finding) IsWarning() bool {
	switch {
	case f.Category == ShaAsRef:
		return true
	case f.Category == RefMoved:
		return true
	case f.Category == LocalAction:
		return f.Severity != SeverityError
	case f.Category.IsInconclusive():
		return true
	case f.Category == NotPinned && f.ActionRef == nil:
		return true
	default:
		return false
	}
}

// DepKey returns a dependency identifier for display grouping.
func (f *Finding) DepKey() string {
	if f.Dependency != nil {
		return f.Dependency.Key()
	}
	if f.ActionRef != nil {
		return f.ActionRef.FullName() + "@" + f.ActionRef.Ref
	}
	return ""
}

// Report aggregates all workflow reports for a check run.
type Report struct {
	Workflows []WorkflowReport
	// RepoFindings are findings that apply to the repository as a whole
	// (not to any individual workflow).
	RepoFindings []Finding
}

// IsValid returns true if all workflows in the report pass validation.
func (r *Report) IsValid() bool {
	for _, wr := range r.Workflows {
		if !wr.IsValid() {
			return false
		}
	}
	for _, f := range r.RepoFindings {
		if !f.IsValid() {
			return false
		}
	}
	return true
}

// HasInconclusive reports whether the report contains any inconclusive
// findings (reachability-unknown, ancestry-unknown). These are treated as
// warnings by default, but callers that need a strict gate (e.g. --rescan)
// can use this to fail when verification couldn't complete.
func (r *Report) HasInconclusive() bool {
	for _, wr := range r.Workflows {
		for _, f := range wr.Findings {
			if f.Category.IsInconclusive() {
				return true
			}
		}
	}
	return false
}

// IsValid returns true if no findings represent integrity violations.
func (wr *WorkflowReport) IsValid() bool {
	for _, f := range wr.Findings {
		if !f.IsValid() {
			return false
		}
	}
	return true
}

// Summary returns counts by category across all workflows.
func (r *Report) Summary() map[Category]int {
	counts := make(map[Category]int)
	for _, wr := range r.Workflows {
		if len(wr.Findings) == 0 {
			continue
		}
		// For workflow-level categories (RunOnly, NotPinned), count the workflow once.
		// For dep-level categories, count each finding.
		for _, f := range wr.Findings {
			counts[f.Category]++
		}
	}
	return counts
}

// WorkflowsNeedingAttention returns only workflows with actionable findings.
func (r *Report) WorkflowsNeedingAttention() []WorkflowReport {
	var result []WorkflowReport
	for _, wr := range r.Workflows {
		if wr.NeedsAttention() {
			result = append(result, wr)
		}
	}
	return result
}
