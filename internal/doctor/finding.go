package doctor

import "github.com/github/gh-actions-pin/internal/lockfile"

// Category classifies the state of a workflow or individual action dependency.
type Category string

const (
	// CategoryNotPinned means the workflow has action refs but no dependencies: section.
	CategoryNotPinned Category = "not_pinned"
	// CategorySHAAsRef means a dependency is pinned to a bare SHA with no tag ref.
	CategorySHAAsRef Category = "sha_as_ref"
	// CategoryStale means the pinned SHA no longer matches what the ref resolves to.
	CategoryStale Category = "stale"
	// CategoryRefChanged means the uses: ref was manually changed (e.g. v6.2.0 → v6).
	CategoryRefChanged Category = "ref_changed"
	// CategoryUnreachable means the pinned SHA is not reachable from its ref.
	CategoryUnreachable Category = "unreachable"
	// CategoryTampered means the ref was changed in the workflow but not in the lockfile.
	CategoryTampered Category = "tampered"
	// CategoryValid means the dependency is pinned and verified.
	CategoryValid Category = "valid"
	// CategoryRunOnly means the workflow has no action refs (only run: steps).
	CategoryRunOnly Category = "run_only"
)

// Severity indicates how serious a finding is.
type Severity string

const (
	SeverityOK      Severity = "ok"
	SeverityInfo    Severity = "info"
	SeverityWarning Severity = "warning"
	SeverityError   Severity = "error"
)

// Finding represents a single diagnosed issue (or clean bill) for a workflow.
type Finding struct {
	// WorkflowPath is the path to the workflow file.
	WorkflowPath string
	// Category of the finding.
	Category Category
	// Severity of the finding.
	Severity Severity
	// ActionRef is the action reference this finding relates to (nil for workflow-level findings).
	ActionRef *lockfile.ActionRef
	// Dependency is the existing pinned dep if any.
	Dependency *lockfile.Dependency
	// Detail is a human-readable explanation.
	Detail string
	// Remediation describes what doctor can do about it.
	Remediation string
}

// WorkflowReport aggregates all findings for a single workflow file.
type WorkflowReport struct {
	Path     string
	Findings []Finding
	// ActionRefs are all action references found in the workflow.
	ActionRefs []lockfile.ActionRef
	// Deps are the existing pinned dependencies (nil if not pinned).
	Deps []lockfile.Dependency
}

// NeedsAttention returns true if this workflow has any non-OK findings.
func (r *WorkflowReport) NeedsAttention() bool {
	for _, f := range r.Findings {
		switch f.Category {
		case CategoryValid, CategoryRunOnly:
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

// Report aggregates all workflow reports for a doctor run.
type Report struct {
	Workflows []WorkflowReport
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
