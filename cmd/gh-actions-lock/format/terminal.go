package format

import (
	"fmt"
	"sort"
	"strings"

	"github.com/github/gh-actions-lock/internal/pipeline/checks"
	"github.com/github/gh-actions-lock/internal/ui"
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
func PresentResults(out *ui.UI, report *checks.Report, valid bool, willRemediate bool, excludeCategories ...checks.Category) {
	exclude := make(map[checks.Category]bool, len(excludeCategories))
	for _, c := range excludeCategories {
		exclude[c] = true
	}
	for _, f := range report.RepoFindings {
		out.TermWarn("%s", f.Detail)
		if f.DocURL != "" {
			out.TermDetail("see: %s", out.TermLink(f.DocURL, f.DocURL))
		}
	}

	// A workflow whose only non-valid findings are dep-level NotPinned is
	// not failing when this run is about to pin it — it's remediable, not
	// broken. Don't count it as failed (and drop not-pinned from the
	// error block below), otherwise a plain onboarding run reports
	// "N of N workflows failed" for work it's actively doing.
	var failedCount int
	sawOtherFailure := false
	for i := range report.Workflows {
		wr := &report.Workflows[i]
		if wr.IsValid() || isRemediableOnly(wr, willRemediate) {
			continue
		}
		failedCount++
		for _, f := range wr.Findings {
			if !f.IsValid() && f.Category != checks.NotPinned {
				sawOtherFailure = true
			}
		}
	}
	checked := len(report.Workflows)

	if failedCount > 0 {
		// Suppress not-pinned from the failure summary only when another
		// category already explains the failure. If not-pinned is the sole
		// reason a workflow failed — e.g. a fatal load/deps error that
		// diagnose.go reports as workflow-level NotPinned with no
		// ActionRef — keep it so the summary isn't left blank.
		if willRemediate && sawOtherFailure {
			exclude[checks.NotPinned] = true
		}
		renderErrorFindings(out, report, failedCount, checked, exclude)
	}

	renderWarnings(out, report, willRemediate)
}

// isRemediableOnly reports whether a workflow's only integrity problems
// are unpinned dependencies that this run is about to pin. Such a
// workflow is being remediated, not failing, so it must not be counted
// or labeled as failed. Returns false in read-only modes
// (willRemediate == false), where not-pinned is a genuine coverage gap.
func isRemediableOnly(wr *checks.WorkflowReport, willRemediate bool) bool {
	if !willRemediate {
		return false
	}
	sawRemediable := false
	for _, f := range wr.Findings {
		if f.IsValid() {
			continue
		}
		// Only a dep-level NotPinned (an actual uses: ref this run will
		// pin) is remediable. A NotPinned with no ActionRef is a
		// workflow-level fatal error (failed to load / read deps), which
		// remediation cannot fix, so the workflow is genuinely failing.
		if f.Category != checks.NotPinned || f.ActionRef == nil {
			return false
		}
		sawRemediable = true
	}
	return sawRemediable
}

// PresentReadOnlyFailures renders error-level findings directly to the
// terminal for read-only modes (--no-fix / --verify). Those modes return
// before the fix-mode summary (renderPinSummary) runs, so the error block
// that renderErrorFindings emits via the narration helpers is discarded
// when the log sink is io.Discard. This uses the Term* family instead so
// failures actually reach the operator.
//
// It reports whether any failing finding is auto-fixable (i.e. not an
// investigation-only category) so the caller can decide whether the
// "re-run to fix" hint is honest.
func PresentReadOnlyFailures(out *ui.UI, report *checks.Report) (hasFixable bool) {
	type depGroup struct {
		findings  []checks.Finding
		workflows []string
		seenWF    map[string]bool
	}
	var order []string
	groups := map[string]*depGroup{}
	var failedCount, checked int

	for i := range report.Workflows {
		wr := &report.Workflows[i]
		checked++
		if wr.IsValid() {
			continue
		}
		failedCount++
		for _, f := range wr.Findings {
			if f.IsValid() {
				continue
			}
			key := f.DepKey()
			if key == "" {
				key = wr.Path
			}
			g, ok := groups[key]
			if !ok {
				g = &depGroup{seenWF: map[string]bool{}}
				groups[key] = g
				order = append(order, key)
			}
			g.findings = append(g.findings, f)
			if wr.Path != "" && !g.seenWF[wr.Path] {
				g.seenWF[wr.Path] = true
				g.workflows = append(g.workflows, wr.Path)
			}
		}
	}

	if len(order) == 0 {
		return false
	}

	out.TermBlank()
	out.TermError("%d of %d %s failed verification",
		failedCount, checked, ui.Pluralize(checked, "workflow", "workflows"))
	for _, key := range order {
		g := groups[key]
		out.TermBlank()
		for _, f := range g.findings {
			if IsAutoFixable(f.Category) {
				hasFixable = true
			}
			renderTermFindingDetail(out, f, key)
		}
		for _, wf := range g.workflows {
			out.TermDetail("  ↳ %s", out.TermDim(wf))
		}
	}
	return hasFixable
}

// renderTermFindingDetail is the Term* twin of renderFindingDetail: it
// prints a single non-valid finding to the terminal (bypassing the
// narration log) so read-only modes surface the same detail as fix mode.
func renderTermFindingDetail(out *ui.UI, f checks.Finding, dep string) {
	label := categoryLabel(f.Category)
	icon := "!"
	if IsAlertedCategory(f.Category) {
		icon = "✗"
	}
	out.TermDetail("%s %s %s", icon, out.TermDim(label), dep)
	out.TermDetail("  %s", f.Detail)
	if f.Category == checks.UnreachablePin && f.Dependency != nil {
		owner, repo := f.Dependency.OwnerRepo()
		if owner != "" {
			out.TermDetail("  ↳ %s", out.TermDim(fmt.Sprintf("https://github.com/%s/%s/releases", owner, repo)))
		}
	}
	if IsAlertedCategory(f.Category) && f.Remediation != "" {
		out.TermDetail("  %s %s", ui.IconWarning, f.Remediation)
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
		out.TermDetail("  ↳ Suggested pin: %s@%s (%s) — newest release currently on that branch",
			nwo, f.RecommendedTag, sha)
	}
	if f.DocURL != "" {
		out.TermDetail("  see: %s", out.TermDim(out.TermLink("how to fix this", f.DocURL)))
	}
}

// categoryLabel returns a human-readable label for a finding category,
// e.g. "unreachable-pin" -> "Unreachable pin". Falls back to a
// hyphen-to-space title for unmapped categories so new slugs still read
// cleanly.
func categoryLabel(c checks.Category) string {
	switch c {
	case checks.NotPinned:
		return "Not pinned"
	case checks.RefChanged:
		return "Ref changed"
	case checks.MisleadingSHA:
		return "Misleading SHA"
	case checks.UnreachablePin:
		return "Unreachable pin"
	case checks.Stale:
		return "Unused lockfile entry"
	}
	return strings.ReplaceAll(string(c), "-", " ")
}

// renderErrorFindings groups error-level findings by dependency and prints
// per-dep detail lines followed by a category-count summary.
func renderErrorFindings(out *ui.UI, report *checks.Report, failedCount, checked int, exclude map[checks.Category]bool) {
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

		allSkip := true
		for _, f := range dg.findings {
			if exclude[f.Category] {
				continue
			}
			catCounts[f.Category]++
			if f.Category != checks.NotPinned {
				allSkip = false
			}
		}
		if allSkip {
			continue
		}

		// Self-hosted-runner findings are no longer generated; render
		// remaining non-excluded, non-not-pinned findings directly.
		for _, f := range dg.findings {
			if f.Category == checks.NotPinned || exclude[f.Category] {
				continue
			}
			renderFindingDetail(out, f, dep)
		}
	}

	parts := []string{}
	for _, cat := range []checks.Category{
		checks.UnreachablePin,
		checks.RefChanged, checks.NotPinned, checks.OnboardingRequired,
		checks.LocalAction,
		checks.Stale, checks.MisleadingSHA,
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
	if f.Category == checks.UnreachablePin && f.Dependency != nil {
		owner, repo := f.Dependency.OwnerRepo()
		if owner != "" {
			out.Detail("  ↳ %s", out.Dim(fmt.Sprintf("https://github.com/%s/%s/releases", owner, repo)))
		}
	}
	if IsAlertedCategory(f.Category) && f.Remediation != "" {
		out.Detail("  %s %s", ui.IconWarning, f.Remediation)
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
		out.Detail("  ↳ Suggested re-pin: %s@%s (%s) — latest release reachable from a branch",
			nwo, f.RecommendedTag, sha)
	}
	if f.DocURL != "" {
		out.Detail("  see: %s", out.DocLink(f.DocURL))
	}
}

// workflowName extracts the workflow filename from a path like
// ".github/workflows/ci.yml".
func workflowName(path string) string {
	if i := strings.LastIndex(path, "/"); i >= 0 {
		return path[i+1:]
	}
	return path
}

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
	var unpinnedWorkflows, localActionWorkflows, bareSHADeps, otherDetailWarnings []string
	for _, key := range warnOrder {
		wg := warnMap[key]
		f := wg.finding
		switch {
		case f.Category == checks.LocalAction:
			localActionWorkflows = append(localActionWorkflows, f.WorkflowPath)
		case f.Category == checks.NotPinned && f.ActionRef == nil:
			unpinnedWorkflows = append(unpinnedWorkflows, f.WorkflowPath)
		case f.Category == checks.ShaAsRef:
			isTransitive := f.Dependency != nil && f.ActionRef == nil
			if !isTransitive {
				bareSHADeps = append(bareSHADeps, key)
			}
		case f.Category == checks.RefMoved:
			// TODO: surface ref-moved warnings once the `gh actions-lock
			// update` path exists. Today the guidance ("run gh actions-lock
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

	if len(localActionWorkflows) > 0 {
		out.TermCaution("%d %s skipped — local path actions are not yet supported",
			len(localActionWorkflows),
			ui.Pluralize(len(localActionWorkflows), "workflow", "workflows"))
		var localNames []string
		for _, p := range localActionWorkflows {
			localNames = append(localNames, p)
		}
		sort.Strings(localNames)
		out.TermDetail("↳ %s", strings.Join(localNames, ", "))
	}
	if len(unpinnedWorkflows) > 0 {
		out.TermWarn("%d %s not yet pinned",
			len(unpinnedWorkflows),
			ui.Pluralize(len(unpinnedWorkflows), "workflow", "workflows"))
		if willRemediate {
			out.TermDetail("↳ resolving below")
		} else {
			out.TermDetail("↳ run `gh actions-lock` to pin them")
		}
	}
	if len(bareSHADeps) > 0 && !willRemediate {
		out.TermWarn("%d %s pinned to a bare SHA without a tag ref",
			len(bareSHADeps),
			ui.Pluralize(len(bareSHADeps), "action is", "actions are"))
		out.TermDetail("↳ run `gh actions-lock` to pin to tagged releases")
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
	case checks.UnreachablePin, checks.MisleadingSHA, checks.OnboardingRequired:
		return true
	}
	return false
}

// IsAutoFixable reports whether a plain `gh actions-lock` run (no --no-fix)
// re-pins the finding without operator intervention. Only structural drift
// qualifies: a missing, changed, or orphaned lock entry. Integrity failures
// (unreachable-pin, misleading-sha) need investigation or --accept-moved, and
// local-path actions aren't supported at all — so none of those should
// trigger the "Re-run without --no-fix to apply fixes" hint.
func IsAutoFixable(c checks.Category) bool {
	switch c {
	case checks.NotPinned, checks.RefChanged, checks.Stale:
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
