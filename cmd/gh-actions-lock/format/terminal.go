package format

import (
	"fmt"
	"sort"
	"strings"

	"github.com/github/gh-actions-lock/internal/pipeline/checks"

	"github.com/github/gh-actions-lock/internal/pipeline"
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

	if !valid && checked > 0 {
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

		// Self-hosted-runner findings share the same empty dep key;
		// render them as a deduplicated group showing affected workflows.
		var selfHostedFindings []checks.Finding
		for _, f := range dg.findings {
			if f.Category == checks.NotPinned {
				continue
			}
			if f.Category == checks.SelfHostedRunner {
				selfHostedFindings = append(selfHostedFindings, f)
				continue
			}
			renderFindingDetail(out, f, dep)
		}
		if len(selfHostedFindings) > 0 {
			renderSelfHostedGroup(out, selfHostedFindings)
		}
	}

	parts := []string{}
	for _, cat := range []checks.Category{
		checks.LockfileForgery,
		checks.RefChanged, checks.NotPinned, checks.OnboardingRequired,
		checks.LocalAction,
		checks.SelfHostedRunner, checks.ExpressionRunner,
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
	if f.Category == checks.ImpostorCommit {
		out.Detail("  %s %s", ui.IconWarning, pipeline.ImpostorCommitContext)
		out.Detail("  ↳ %s", pipeline.PublisherEscalationCopy)
		out.Detail("  see: %s", out.DocLink(pipeline.PublisherTagReleasesDocURL))
	}
	if f.DocURL != "" {
		out.Detail("  see: %s", out.DocLink(f.DocURL))
	}
}

// renderSelfHostedGroup prints a deduplicated block for self-hosted-runner
// findings, listing each affected workflow and its non-hosted labels.
func renderSelfHostedGroup(out *ui.UI, findings []checks.Finding) {
	label := "SELF-HOSTED-RUNNER"
	icon := "!"
	if IsAlertedCategory(checks.SelfHostedRunner) {
		icon = "✗"
	}
	out.Detail("%s %s", icon, out.Dim(label))
	for _, f := range findings {
		wfName := workflowName(f.WorkflowPath)
		out.Detail("  %s: %s", out.Bold(wfName), f.Detail)
	}
	if IsAlertedCategory(checks.SelfHostedRunner) && findings[0].Remediation != "" {
		out.Detail("  %s %s", ui.IconWarning, findings[0].Remediation)
	}
	labelSet := map[string]bool{}
	for _, f := range findings {
		for _, l := range extractBracketedLabels(f.Detail) {
			labelSet[l] = true
		}
	}
	if len(labelSet) > 0 {
		var labels []string
		for l := range labelSet {
			labels = append(labels, l)
		}
		sort.Strings(labels)
		out.Detail("  ↳ re-run with --allow-runners %s", strings.Join(labels, ","))
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

// extractBracketedLabels pulls comma-separated items from the first
// [...] group in s. Returns nil if no brackets are found.
// Template expressions like ${{ matrix.os }} are excluded — they can't
// be passed as literal --allow-runners values.
func extractBracketedLabels(s string) []string {
	start := strings.Index(s, "[")
	end := strings.Index(s, "]")
	if start < 0 || end <= start {
		return nil
	}
	inner := s[start+1 : end]
	var labels []string
	for _, l := range strings.Split(inner, ",") {
		l = strings.TrimSpace(l)
		if l != "" && !isExpression(l) {
			labels = append(labels, l)
		}
	}
	return labels
}

// isExpression reports whether s is a GitHub Actions template expression.
func isExpression(s string) bool {
	return strings.Contains(s, "${{")
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
	var unpinnedWorkflows, localActionWorkflows, selfHostedRunnerWorkflows, expressionRunnerWorkflows, bareSHADeps, otherDetailWarnings []string
	for _, key := range warnOrder {
		wg := warnMap[key]
		f := wg.finding
		switch {
		case f.Category == checks.LocalAction:
			localActionWorkflows = append(localActionWorkflows, f.WorkflowPath)
		case f.Category == checks.SelfHostedRunner:
			selfHostedRunnerWorkflows = append(selfHostedRunnerWorkflows, f.WorkflowPath)
		case f.Category == checks.ExpressionRunner:
			expressionRunnerWorkflows = append(expressionRunnerWorkflows, f.WorkflowPath)
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
	}
	if len(selfHostedRunnerWorkflows) > 0 {
		out.TermCaution("%d %s skipped — non-hosted runner labels are not supported",
			len(selfHostedRunnerWorkflows),
			ui.Pluralize(len(selfHostedRunnerWorkflows), "workflow", "workflows"))
		// Collect distinct labels from findings for the remediation hint.
		labelSet := map[string]bool{}
		for _, key := range warnOrder {
			wg := warnMap[key]
			if wg.finding.Category != checks.SelfHostedRunner {
				continue
			}
			for _, l := range extractBracketedLabels(wg.finding.Detail) {
				labelSet[l] = true
			}
		}
		if len(labelSet) > 0 {
			var labels []string
			for l := range labelSet {
				labels = append(labels, l)
			}
			sort.Strings(labels)
			out.TermDetail("↳ if these are org-hosted larger runners, re-run with --allow-runners %s",
				strings.Join(labels, ","))
		} else {
			out.TermDetail("↳ if these are org-hosted larger runners, re-run with --allow-runners <label>")
		}
	}
	if len(expressionRunnerWorkflows) > 0 {
		out.TermCaution("%d %s skipped — runs-on uses expressions that can't be resolved statically",
			len(expressionRunnerWorkflows),
			ui.Pluralize(len(expressionRunnerWorkflows), "workflow", "workflows"))
		var wfNames []string
		for _, p := range expressionRunnerWorkflows {
			wfNames = append(wfNames, workflowName(p))
		}
		sort.Strings(wfNames)
		out.TermDetail("↳ %s — if the matrix resolves to hosted runners, pin manually",
			strings.Join(wfNames, ", "))
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
	case checks.ImpostorCommit, checks.LockfileForgery, checks.MisleadingSHA, checks.OnboardingRequired:
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
