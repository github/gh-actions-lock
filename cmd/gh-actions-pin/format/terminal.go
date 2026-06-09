package format

import (
	"fmt"
	"strings"

	"github.com/github/gh-actions-pin/internal/pipeline/checks"

	"github.com/github/gh-actions-pin/internal/pipeline"
	"github.com/github/gh-actions-pin/internal/ui"
)

// PresentResults renders human-readable output from a check report.
//
// Warning surfaces (RepoFindings, ParseWarnings, not-pinned, sha-as-ref,
// ref-moved, inconclusive) use the Term* family so they reach the
// terminal even when check.go has routed the narration log to
// io.Discard. The error block and the "All N valid" success line still
// use narration helpers — both are re-rendered by check.go's
// post-remediation Term* summary, so we'd duplicate output if we
// surfaced them here too.
func PresentResults(out *ui.UI, report *checks.Report, valid bool, willRemediate bool) {
	for _, f := range report.RepoFindings {
		out.TermWarn("%s", f.Detail)
		if f.DocURL != "" {
			out.TermDetail("see: %s", out.TermLink(f.DocURL, f.DocURL))
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
		renderErrorFindings(out, report, failedCount, checked)
	}

	renderWarnings(out, report, willRemediate)
}

// renderErrorFindings groups error-level findings by dependency and prints
// per-dep detail lines followed by a category-count summary.
func renderErrorFindings(out *ui.UI, report *checks.Report, failedCount, checked int) {
	type depGroup struct {
		dep      string
		findings []checks.Finding
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
				depMap[depKey] = &depGroup{dep: depKey, findings: []checks.Finding{f}}
			}
		}
	}

	catCounts := map[checks.Category]int{}
	for _, dep := range depOrder {
		dg := depMap[dep]

		allNotPinned := true
		for _, f := range dg.findings {
			catCounts[f.Category]++
			if f.Category != checks.NotPinned {
				allNotPinned = false
			}
		}
		if allNotPinned {
			continue
		}

		for _, f := range dg.findings {
			if f.Category == checks.NotPinned {
				continue
			}
			renderFindingDetail(out, f, dep)
		}
	}

	parts := []string{}
	for _, cat := range []checks.Category{
		checks.LockfileForgery,
		checks.RefChanged, checks.NotPinned,
		checks.Stale, checks.MisleadingSHA, checks.ImpostorCommit,
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

// renderFindingDetail prints a single non-valid finding with its category
// icon, detail text, remediation, and doc links.
func renderFindingDetail(out *ui.UI, f checks.Finding, dep string) {
	label := strings.ToUpper(string(f.Category))
	icon := "!"
	if IsAlertedCategory(f.Category) {
		icon = "✗"
	}
	out.Detail("%s %s %s", icon, out.Dim(label), dep)
	out.Detail("  %s", f.Detail)
	if f.Category == checks.LockfileForgery && f.Dependency != nil {
		owner, repo := f.Dependency.OwnerRepo()
		if owner != "" {
			out.Detail("  → %s", out.Dim(fmt.Sprintf("https://github.com/%s/%s/releases", owner, repo)))
		}
	}
	if IsAlertedCategory(f.Category) && f.Remediation != "" {
		out.Detail("  %s %s", out.Bold("⚠"), f.Remediation)
	}
	if f.RecommendedTag != "" {
		nwo := ""
		if f.Dependency != nil {
			nwo = f.Dependency.NWO
		}
		sha := f.RecommendedSHA
		if len(sha) > 7 {
			sha = sha[:7]
		}
		out.Detail("  %s Suggested re-pin: %s@%s (%s) — latest release reachable from a branch",
			out.Bold("→"), nwo, f.RecommendedTag, sha)
	}
	if f.Category == checks.ImpostorCommit {
		out.Detail("  %s %s", out.Yellow("!"), pipeline.ImpostorCommitContext)
		out.Detail("  %s %s", out.Bold("→"), pipeline.PublisherEscalationCopy)
		out.Detail("  see: %s", out.DocLink(pipeline.PublisherTagReleasesDocURL))
	}
	if f.DocURL != "" {
		out.Detail("  see: %s", out.DocLink(f.DocURL))
	}
}

// warningGroup deduplicates warnings by dep key across workflows.
type warningGroup struct {
	finding   checks.Finding
	count     int
	workflows []string
}

// renderWarnings collects, deduplicates, and prints all warning-level findings.
func renderWarnings(out *ui.UI, report *checks.Report, willRemediate bool) {
	var warnOrder []string
	warnMap := map[string]*warningGroup{}
	for _, wr := range report.Workflows {
		for _, f := range wr.Findings {
			if f.IsWarning() {
				if f.DepKey() == "" && f.Category == checks.ReachabilityUnknown &&
					strings.HasPrefix(f.Detail, "could not re-resolve actions") {
					continue
				}
				key := f.DepKey()
				if key == "" {
					key = f.WorkflowPath
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
			out.TermWarn("%s: %s", wr.Path, pw)
		}
	}

	// Triage warnings into buckets.
	var unpinnedWorkflows, bareSHADeps, otherDetailWarnings []string
	for _, key := range warnOrder {
		wg := warnMap[key]
		f := wg.finding
		switch {
		case f.Category == checks.NotPinned && f.ActionRef == nil:
			unpinnedWorkflows = append(unpinnedWorkflows, f.WorkflowPath)
		case f.Category == checks.ShaAsRef:
			isTransitive := f.Dependency != nil && f.ActionRef == nil
			if !isTransitive {
				bareSHADeps = append(bareSHADeps, key)
			}
		case f.Category == checks.RefMoved:
			// TODO: surface ref-moved warnings once the `gh actions-pin
			// update` path exists. Today the guidance ("run gh actions-pin
			// to update") is wrong — a plain re-run trusts the lockfile and
			// repins nothing; only --rescan even detects the movement. Until
			// there's a command that actually advances a moved ref, swallow
			// these rather than print misleading instructions.
		case f.Category.IsInconclusive() &&
			strings.Contains(f.Remediation, "transitive dependency"):
			// transitive reachability unknown: silently swallowed
		default:
			otherDetailWarnings = append(otherDetailWarnings, key)
		}
	}

	if len(unpinnedWorkflows) > 0 {
		out.TermWarn("%d %s not yet pinned",
			len(unpinnedWorkflows),
			ui.Pluralize(len(unpinnedWorkflows), "workflow", "workflows"))
		if willRemediate {
			out.TermDetail("↳ resolving below")
		} else {
			out.TermDetail("↳ run `gh actions-pin` to pin them")
		}
	}
	if len(bareSHADeps) > 0 && !willRemediate {
		out.TermWarn("%d %s pinned to a bare SHA without a tag ref",
			len(bareSHADeps),
			ui.Pluralize(len(bareSHADeps), "action is", "actions are"))
		out.TermDetail("↳ run `gh actions-pin` to pin to tagged releases")
	}
	for _, key := range otherDetailWarnings {
		wg := warnMap[key]
		f := wg.finding
		depKey := f.DepKey()
		if f.Category.IsInconclusive() && f.Severity == checks.SeverityWarning {
			label := depKey
			if label == "" {
				label = f.WorkflowPath
			}
			out.TermWarn("%s: %s", label, f.Detail)
			if f.Remediation != "" {
				out.TermDetail("↳ %s", f.Remediation)
			}
		}
	}
}

// IsAlertedCategory reports whether a finding category has no auto-fix and
// requires human investigation (already surfaced in PresentResults — the
// remediator should not re-print it in non-interactive mode).
func IsAlertedCategory(c checks.Category) bool {
	switch c {
	case checks.ImpostorCommit, checks.LockfileForgery, checks.MisleadingSHA:
		return true
	}
	return false
}

// WorkflowsForDep returns workflow paths whose findings reference the given
// dependency key (ordered as they appear in the report, deduplicated).
func WorkflowsForDep(report *checks.Report, depKey string) []string {
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

// PresentUpdateSummary renders the human-readable outcome of an `update` run
// (non-JSON mode). update always writes, so a successful relock is reported as
// applied.
func PresentUpdateSummary(console *ui.UI, res UpdateResult) {
	switch {
	case len(res.Updated) > 0:
		for _, u := range res.Updated {
			console.TermNeutral("Updated %s: %s → %s", u.NWO, u.OldRef, u.NewRef)
		}
		console.TermNeutral("Saved %d %s.", len(res.Workflows), ui.Pluralize(len(res.Workflows), "workflow", "workflows"))
	case len(res.Findings) > 0:
		// Findings but no updates: the run was blocked (e.g. onboarding-required
		// or a relock-invariant violation), not a clean no-op.
		console.TermNeutral("No changes applied; see findings below.")
	default:
		console.TermNeutral("No changes — every targeted workflow is already current.")
	}
	for _, f := range res.Findings {
		console.TermDetail("%s: %s — %s", f.Severity, f.Category, f.Detail)
	}
}
