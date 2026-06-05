package doctor

import (
	"github.com/github/gh-actions-pin/internal/lockfile"
)

// Category classifies the state of a workflow or individual action dependency.
type Category string

const (
	// CategoryNotPinned means the workflow has action refs but no dependencies: section.
	// Sibling of zizmor's `unpinned-uses`, but our semantics differ (we
	// flag the lockfile mismatch, not the workflow-level unpinned ref),
	// so we keep our own ID.
	CategoryNotPinned Category = "not-pinned"
	// CategorySHAAsRef means a dependency is pinned to a bare SHA with no tag ref.
	CategorySHAAsRef Category = "sha-as-ref"
	// CategoryStale means the pinned SHA no longer matches what the ref resolves to.
	CategoryStale Category = "stale"
	// CategoryRefChanged means the uses: ref was manually changed (e.g. v6.2.0 → v6).
	CategoryRefChanged Category = "ref-changed"
	// CategoryImpostorCommit means the pinned SHA is not in the ref's git history (possible fork-network commit).
	// Matches zizmor's `impostor-commit` audit ID.
	CategoryImpostorCommit Category = "impostor-commit"
	// CategoryMisleadingSHA means a ref looks like a SHA but resolves to a different commit.
	CategoryMisleadingSHA Category = "misleading-sha"
	// CategoryLockfileForgery means the pinned SHA is not an ancestor of the
	// current ref — the lockfile entry was likely injected or tampered with.
	CategoryLockfileForgery Category = "lockfile-forgery"
	// CategoryRefMoved means the upstream tag now resolves to a different SHA than what's locked.
	CategoryRefMoved Category = "ref-moved"
	// CategoryValid means the dependency is pinned and verified.
	CategoryValid Category = "valid"
	// CategoryRunOnly means the workflow has no action refs (only run: steps).
	CategoryRunOnly Category = "run-only"
)

// Confidence describes how strongly the check stands behind a finding,
// modeled on zizmor's audit output. It complements Severity (how bad the
// issue would be if real) by capturing how certain we are that it IS
// real. The three levels are deliberately coarse — finer gradations
// invite bikeshedding without helping consumers act.
type Confidence string

const (
	// ConfidenceLow signals the finding is informational or rests on a
	// signal we couldn't fully verify (e.g. resolver/network failure,
	// reachability inconclusive). Treat as a hint, not a verdict.
	ConfidenceLow Confidence = "low"
	// ConfidenceMedium signals the finding rests on a fallback inference
	// — we couldn't get an authoritative answer (rate limit, partial
	// API response) and inferred from secondary shape (e.g. tag-object
	// peel, AncestryUnknown). Likely real but worth double-checking.
	ConfidenceMedium Confidence = "medium"
	// ConfidenceHigh signals the finding rests on direct, authoritative
	// data: a string mismatch the user can verify by eye, an exact SHA
	// comparison, or an ancestry/reachability answer we got from the
	// upstream API. Act on these.
	ConfidenceHigh Confidence = "high"
)

// Severity indicates how serious a finding is.
type Severity string

const (
	// SeverityOK means the finding represents a clean state — no action needed.
	SeverityOK Severity = "ok"
	// SeverityInfo is purely informational and does not require action.
	SeverityInfo Severity = "info"
	// SeverityWarning indicates a concern worth surfacing but not blocking on.
	SeverityWarning Severity = "warning"
	// SeverityError indicates a blocking issue the operator must resolve.
	SeverityError Severity = "error"
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
	ActionRef *lockfile.ActionRef
	// Dependency is the existing pinned dep if any.
	Dependency *lockfile.Dependency
	// ParentNWO is the dep key of the direct action that pulls in this transitive dep (empty if direct).
	ParentNWO string
	// Detail is a human-readable explanation.
	Detail string
	// Remediation describes what doctor can do about it.
	Remediation string
	// ObservedSHA is the SHA the resolver got at scan time, recorded when
	// it differs from the pinned SHA (e.g. ref-moved, misleading-sha,
	// lockfile-forgery).
	ObservedSHA string
	// DocURL points to docs explaining the finding. Populated by the
	// engine adapter so it's parity-aligned with the editor's
	// codeDescription link; "" when no URL is mapped.
	DocURL string
	// SaneSuggestionTag is the most recent stable tag whose commit is
	// reachable from a branch, populated for unreachable-SHA findings
	// (CategoryImpostorCommit) when one can be found. Empty otherwise.
	SaneSuggestionTag string
	// SaneSuggestionSHA is the commit SHA the suggested tag points to.
	SaneSuggestionSHA string
	// SaneSuggestionSearched is true when the sane-release walk ran for
	// this finding (regardless of outcome). Lets renderers distinguish
	// "we didn't look" from "we looked and found nothing" — the latter
	// signals the publisher's whole release flow detaches commits and
	// warrants escalation rather than a re-pin.
	SaneSuggestionSearched bool
}

// InventoryEntry describes a single dependency with context.
type InventoryEntry struct {
	Dep    lockfile.Dependency
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
	// ActionRefs are all action references found in the workflow.
	ActionRefs []lockfile.ActionRef
	// Deps are the existing pinned dependencies (nil if not pinned).
	Deps []lockfile.Dependency
	// Inventory lists all dependencies with direct/transitive classification.
	Inventory []InventoryEntry
	// ParseWarnings from ExtractActionRefs (e.g. malformed uses: lines).
	ParseWarnings []string
}

// NeedsAttention returns true if this workflow has any non-OK findings.
func (r *WorkflowReport) NeedsAttention() bool {
	for _, f := range r.Findings {
		switch f.Category {
		case CategoryValid, CategoryRunOnly, CategoryMisleadingSHA, CategoryRefMoved:
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
	switch f.Category {
	case CategoryValid, CategoryRunOnly, CategorySHAAsRef, CategoryRefMoved:
		return true
	case CategoryNotPinned:
		return f.ActionRef == nil // workflow-level is a warning
	default:
		return false
	}
}

// IsWarning returns true for findings that should render as warnings (not errors).
func (f *Finding) IsWarning() bool {
	switch {
	case f.Category == CategorySHAAsRef:
		return true
	case f.Category == CategoryRefMoved:
		return true
	case f.Category == CategoryValid && f.Severity == SeverityWarning:
		return true
	case f.Category == CategoryNotPinned && f.ActionRef == nil:
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

// Report aggregates all workflow reports for a doctor run.
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
