package format

import (
	"fmt"
	"strings"

	"github.com/github/gh-actions-pin/internal/doctor"
	"github.com/github/gh-actions-pin/internal/ui"
)

// PresentResults renders human-readable output from a doctor report.
func PresentResults(out *ui.UI, report *doctor.Report, valid bool, willRemediate bool) {
	for _, f := range report.RepoFindings {
		out.Warning("%s", f.Detail)
		if f.DocURL != "" {
			out.Detail("  see: %s", out.DocLink(f.DocURL))
		}
	}

	var validCount, failedCount int
	for _, wr := range report.Workflows {
		if wr.IsValid() {
			validCount++
		} else {
			failedCount++
		}
	}
	checked := validCount + failedCount

	if valid && checked > 0 {
		out.Success("All %d %s valid", checked, ui.Pluralize(checked, "workflow", "workflows"))
	} else if checked > 0 {
		// Collect error findings grouped by dependency.
		type depGroup struct {
			dep      string
			findings []doctor.Finding
		}
		var depOrder []string
		depMap := map[string]*depGroup{}

		for _, wr := range report.Workflows {
			for _, f := range wr.Findings {
				if f.IsValid() {
					continue
				}
				depKey := f.DepKey()
				if dg, ok := depMap[depKey]; ok {
					dg.findings = append(dg.findings, f)
				} else {
					depOrder = append(depOrder, depKey)
					depMap[depKey] = &depGroup{dep: depKey, findings: []doctor.Finding{f}}
				}
			}
		}

		// Count categories; render per-dep detail only for non-NOT_PINNED.
		catCounts := map[doctor.Category]int{}
		for _, dep := range depOrder {
			dg := depMap[dep]

			// Tally and check if this dep is NOT_PINNED only.
			allNotPinned := true
			for _, f := range dg.findings {
				catCounts[f.Category]++
				if f.Category != doctor.CategoryNotPinned {
					allNotPinned = false
				}
			}
			if allNotPinned {
				continue // pure aggregation — no per-dep output
			}

			// Render per-dep detail for actionable categories.
			for _, f := range dg.findings {
				if f.Category == doctor.CategoryNotPinned {
					continue // skip NOT_PINNED lines in mixed groups too
				}
				label := strings.ToUpper(string(f.Category))
				icon := "!"
				if IsAlertedCategory(f.Category) {
					icon = "✗"
				}
				out.Detail("%s %s %s", icon, out.Dim(label), dep)
				out.Detail("  %s", f.Detail)
				if f.Category == doctor.CategoryLockfileForgery && f.Dependency != nil {
					owner, repo := f.Dependency.OwnerRepo()
					if owner != "" {
						out.Detail("  → %s", out.Dim(fmt.Sprintf("https://github.com/%s/%s/releases", owner, repo)))
					}
				}
				if IsAlertedCategory(f.Category) && f.Remediation != "" {
					out.Detail("  %s %s", out.Bold("⚠"), f.Remediation)
				}
				if f.SaneSuggestionTag != "" {
					nwo := ""
					if f.Dependency != nil {
						nwo = f.Dependency.NWO
					}
					sha := f.SaneSuggestionSHA
					if len(sha) > 7 {
						sha = sha[:7]
					}
					out.Detail("  %s suggested re-pin: %s@%s (%s) — latest release reachable from a branch",
						out.Bold("→"), nwo, f.SaneSuggestionTag, sha)
				} else if f.SaneSuggestionSearched {
					out.Detail("  %s no recent release was reachable from a branch — escalate to the action publisher",
						out.Bold("→"))
				}
				if f.SaneSuggestionSearched {
					out.Detail("  publishers: %s", out.DocLink(doctor.PublisherTagReleasesDocURL))
				}
				if f.DocURL != "" {
					out.Detail("  see: %s", out.DocLink(f.DocURL))
				}
			}
		}

		parts := []string{}
		for _, cat := range []doctor.Category{
			doctor.CategoryLockfileForgery,
			doctor.CategoryRefChanged, doctor.CategoryNotPinned,
			doctor.CategoryStale, doctor.CategoryMisleadingSHA, doctor.CategoryImpostorCommit,
		} {
			if n, ok := catCounts[cat]; ok {
				parts = append(parts, fmt.Sprintf("%d %s", n, string(cat)))
			}
		}
		out.Error("%d of %d %s failed: %s",
			failedCount, checked,
			ui.Pluralize(checked, "workflow", "workflows"),
			strings.Join(parts, ", "))
		out.Blank()
	}

	// Warnings — deduplicate by dep key.
	type warningGroup struct {
		finding   doctor.Finding
		count     int
		workflows []string
	}
	var warnOrder []string
	warnMap := map[string]*warningGroup{}
	for _, wr := range report.Workflows {
		for _, f := range wr.Findings {
			if f.IsWarning() {
				key := f.DepKey()
				if key == "" {
					key = f.WorkflowPath // workflow-level warnings
				}
				if wg, ok := warnMap[key]; ok {
					wg.count++
					wg.workflows = append(wg.workflows, f.WorkflowPath)
				} else {
					warnOrder = append(warnOrder, key)
					warnMap[key] = &warningGroup{finding: f, count: 1, workflows: []string{f.WorkflowPath}}
				}
			}
		}
		for _, pw := range wr.ParseWarnings {
			out.Warning("%s: %s", wr.Path, pw)
		}
	}

	// Collect workflow-level NOT_PINNED warnings separately for collapsing.
	var unpinnedWorkflows []string
	var otherWarnings []string
	for _, key := range warnOrder {
		wg := warnMap[key]
		f := wg.finding
		if f.Category == doctor.CategoryNotPinned && f.ActionRef == nil {
			unpinnedWorkflows = append(unpinnedWorkflows, f.WorkflowPath)
		} else {
			otherWarnings = append(otherWarnings, key)
		}
	}
	if len(unpinnedWorkflows) > 0 {
		if willRemediate {
			out.Warning("%d %s not yet pinned — resolving below",
				len(unpinnedWorkflows),
				ui.Pluralize(len(unpinnedWorkflows), "workflow", "workflows"))
		} else {
			out.Warning("%d %s not yet pinned (run `gh actions-pin` to fix)",
				len(unpinnedWorkflows),
				ui.Pluralize(len(unpinnedWorkflows), "workflow", "workflows"))
		}
	}
	// Separate SHA_AS_REF warnings into direct (aggregate) and transitive (suppressed).
	// TODO: Transitive deps pinned to bare SHAs are silently swallowed for now.
	// We need to figure out how to coexist better with composite actions that
	// don't use dependency pinning — warning on every transitive dep is noisy
	// and not actionable by the consumer. Revisit when we have a story for
	// composite action authors to adopt pinning.
	// Collect REF_MOVED warnings for compact display.
	var bareSHADeps []string
	var refMovedWarnings []string
	var otherDetailWarnings []string
	for _, key := range otherWarnings {
		wg := warnMap[key]
		f := wg.finding
		if f.Category == doctor.CategorySHAAsRef {
			isTransitive := f.Dependency != nil && f.ActionRef == nil
			if !isTransitive {
				bareSHADeps = append(bareSHADeps, key)
			}
			// transitive SHA_AS_REF: silently swallowed (see TODO above)
		} else if f.Category == doctor.CategoryRefMoved {
			refMovedWarnings = append(refMovedWarnings, key)
		} else if f.Category == doctor.CategoryValid && f.Severity == doctor.SeverityWarning &&
			strings.Contains(f.Remediation, "transitive dependency") {
			// transitive reachability unknown: silently swallowed (see TODO above)
		} else {
			otherDetailWarnings = append(otherDetailWarnings, key)
		}
	}
	if len(bareSHADeps) > 0 {
		out.Warning("%d %s pinned to a bare SHA without a tag ref",
			len(bareSHADeps),
			ui.Pluralize(len(bareSHADeps), "action is", "actions are"))
		if willRemediate {
			out.Detail("  ↳ resolving below")
		} else {
			out.Detail("  ↳ run `gh actions-pin upgrade` to pin to tagged releases")
		}
	}
	if len(refMovedWarnings) > 0 {
		out.Warning("%d %s moved upstream — run `gh actions-pin upgrade` to update",
			len(refMovedWarnings),
			ui.Pluralize(len(refMovedWarnings), "ref has", "refs have"))
		for _, key := range refMovedWarnings {
			wg := warnMap[key]
			f := wg.finding
			out.Detail("  ↳ %s: %s", key, f.Detail)
			if f.Dependency != nil && f.ObservedSHA != "" {
				owner, repo := f.Dependency.OwnerRepo()
				if owner != "" {
					out.Detail("    %s", out.Dim(fmt.Sprintf("https://github.com/%s/%s/compare/%s...%s", owner, repo, f.Dependency.SHA[:12], f.ObservedSHA[:12])))
				}
			}
		}
	}
	for _, key := range otherDetailWarnings {
		wg := warnMap[key]
		f := wg.finding
		depKey := f.DepKey()
		if f.Category == doctor.CategoryValid && f.Severity == doctor.SeverityWarning {
			label := depKey
			if label == "" {
				label = f.WorkflowPath
			}
			out.Warning("%s: %s", label, f.Detail)
		}
	}
}

// IsAlertedCategory reports whether a finding category has no auto-fix and
// requires human investigation (already surfaced in PresentResults — the
// remediator should not re-print it in non-interactive mode).
func IsAlertedCategory(c doctor.Category) bool {
	switch c {
	case doctor.CategoryImpostorCommit, doctor.CategoryLockfileForgery, doctor.CategoryMisleadingSHA:
		return true
	}
	return false
}

// WorkflowsForDep returns workflow paths whose findings reference the given
// dependency key (ordered as they appear in the report, deduplicated).
func WorkflowsForDep(report *doctor.Report, depKey string) []string {
	seen := map[string]bool{}
	var out []string
	for _, wr := range report.Workflows {
		for _, f := range wr.Findings {
			if f.DepKey() == depKey && !seen[wr.Path] {
				seen[wr.Path] = true
				out = append(out, wr.Path)
			}
		}
	}
	return out
}
