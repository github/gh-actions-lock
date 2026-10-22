package format

import (
	"cmp"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/github/gh-actions-lock/internal/pipeline/checks"
	"github.com/github/gh-actions-lock/internal/ui"
)

// PresentResults renders human-readable output from a check report.
//
// Warning surfaces (RepoFindings, container images, not-pinned, sha-as-ref,
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
		// Fresh-tag nudges are appended after this render and shown by the
		// fix-mode pin summary, so they surface on a clean pin too.
		if f.Category == checks.FreshTag {
			continue
		}
		if f.Severity == checks.SeverityError {
			out.TermError("%s", f.Detail)
			if f.Remediation != "" {
				out.TermDetail("  %s", f.Remediation)
			}
			continue
		}
		out.TermWarn("%s", f.Detail)
		if u := f.DocURL(); u != "" {
			out.TermDetail("see: %s", out.TermLink(u, u))
		}
	}

	var failedCount int
	for _, wr := range report.Workflows {
		for _, f := range wr.Findings {
			if !f.IsValid() && !findingExcluded(f, exclude, willRemediate) {
				failedCount++
				break
			}
		}
	}
	checked := len(report.Workflows)

	if !valid && failedCount > 0 {
		renderErrorFindings(out, report, failedCount, checked, exclude, willRemediate)
	}

	renderWarnings(out, report, willRemediate)
}

func findingExcluded(f checks.Finding, exclude map[checks.Category]bool, willRemediate bool) bool {
	if f.Category == checks.NotPinned && !f.IsRemediableNotPinned() {
		// A non-remediable NotPinned can't be fixed by pinning.
		return false
	}
	return exclude[f.Category] || willRemediate && f.Category == checks.NotPinned
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
	report = blockingFirst(report)
	type depGroup struct {
		findings  []checks.Finding
		workflows []string
		seenWF    map[string]bool
	}
	var order []string
	groups := map[string]*depGroup{}
	var retry []checks.Finding
	var failedCount, checked, errCount int

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
			if f.Retryable {
				retry = append(retry, f)
				continue
			}
			errCount++
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
		renderRetryable(out, retry, report.RetriedFor)
		return false
	}

	out.TermBlank()
	verdict := fmt.Sprintf("%d of %d %s failed",
		failedCount, checked, ui.Pluralize(checked, "workflow", "workflows"))
	if pins := retryLabels(retry); len(pins) > 0 {
		verdict += fmt.Sprintf(": %d %s, %d %s couldn't be verified",
			errCount, ui.Pluralize(errCount, "error", "errors"),
			len(pins), ui.Pluralize(len(pins), "pin", "pins"))
	}
	out.TermError("%s", verdict)
	blocked := len(retry) > 0
	for _, key := range order {
		g := groups[key]
		out.TermBlank()
		for _, f := range g.findings {
			if IsAutoFixable(f.Category) && !f.RefMissing {
				hasFixable = true
			} else if f.Severity == checks.SeverityError {
				blocked = true
			}
			renderTermFindingDetail(out, f, key)
		}
		for _, wf := range g.workflows {
			out.TermDetail("  ↳ %s", out.TermDim(wf))
		}
	}
	renderRetryable(out, retry, report.RetriedFor)
	// A plain re-run writes nothing while an unfixable error remains.
	return hasFixable && !blocked
}

// now is stubbed by tests that render a rate-limit reset.
var now = time.Now

// renderRetryable prints the checks GitHub didn't answer as one block: they
// share a cause. retried is how long the run waited between retries.
func renderRetryable(out *ui.UI, ff []checks.Finding, retried time.Duration) {
	pins := retryLabels(ff)
	if len(pins) == 0 {
		return
	}
	// A rate limit decides when a re-run can succeed: name the latest reset.
	cause := ff[0]
	for _, f := range ff {
		if f.RateLimited && (!cause.RateLimited || f.RetryAt.After(cause.RetryAt)) {
			cause = f
		}
	}
	head := fmt.Sprintf("Couldn't verify %d %s: %s", len(pins), ui.Pluralize(len(pins), "pin", "pins"), cause.RetryReason)
	if !cause.RetryAt.IsZero() {
		head += fmt.Sprintf(" until %s UTC (in %s)", cause.RetryAt.UTC().Format("15:04"), untilReset(cause.RetryAt))
	}
	if retried >= time.Second {
		head += fmt.Sprintf(", after retrying for %s", retried.Round(time.Second))
	}
	out.TermBlank()
	out.TermError("%s", head)
	out.TermDetail("  %s", strings.Join(pins, ", "))
	out.TermDetail("  ↳ %s", checks.RetryAdvice(cause.RetryAt, cause.RateLimited))
}

// retryLabels names each pin, with the composite that uses it, or the
// workflow for a workflow-level check, once each.
func retryLabels(ff []checks.Finding) []string {
	var labels []string
	seen := map[string]bool{}
	for _, f := range ff {
		label := cmp.Or(f.DepKey(), f.WorkflowPath)
		if f.ParentNWO != "" {
			label += " (used by " + f.ParentNWO + ")"
		}
		if !seen[label] {
			seen[label] = true
			labels = append(labels, label)
		}
	}
	return labels
}

// untilReset is the wait until t, to the minute: "38m", "1h5m".
func untilReset(t time.Time) string {
	d := max(t.Sub(now()).Round(time.Minute), time.Minute)
	if d < time.Hour {
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
	return fmt.Sprintf("%dh%dm", int(d.Hours()), int(d.Minutes())%60)
}

// renderTermFindingDetail is the Term* twin of renderFindingDetail: it
// prints a single non-valid finding to the terminal (bypassing the
// narration log) so read-only modes surface the same detail as fix mode.
func renderTermFindingDetail(out *ui.UI, f checks.Finding, dep string) {
	label := findingLabel(f)
	icon := out.TermYellow(ui.IconWarning)
	if f.Severity == checks.SeverityError {
		icon = out.TermRed(ui.IconError)
	}
	out.TermDetail("%s %s %s", icon, out.TermDim(label), dep)
	out.TermDetail("  %s", cmp.Or(f.TermDetail, f.Detail))
	if f.Category == checks.UnreachablePin && !f.RefMissing && f.Dependency != nil {
		owner, repo := f.Dependency.OwnerRepo()
		if owner != "" {
			out.TermDetail("  ↳ %s", out.TermDim(DepReleaseURL(f.Dependency.Hostname, owner+"/"+repo, nil)))
		}
	}
	if (IsAlertedCategory(f.Category) || f.RefMissing) && f.Remediation != "" {
		out.TermDetail("  %s %s", out.TermYellow(ui.IconWarning), f.Remediation)
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
	if u := f.DocURL(); u != "" {
		out.TermDetail("  see: %s", out.DocLink(u))
	}
}

// findingLabel distinguishes ref states that share a category in JSON.
// Only a ref the lockfile recorded can no longer exist.
func findingLabel(f checks.Finding) string {
	switch {
	case f.TagMoved && f.Category == checks.RefMoved:
		return "Tag now points past locked commit"
	case f.RefRefused && f.Category == checks.UnreachablePin:
		return "Ref name refused"
	case (f.RefMissing || f.RefGone) && f.Category == checks.UnreachablePin && f.FreshRef:
		return "Not a tag or branch"
	case (f.RefMissing || f.RefGone) && f.Category == checks.UnreachablePin:
		return "Ref no longer exists"
	case f.RefMissing:
		return "Ref does not exist"
	}
	return categoryLabel(f.Category)
}

// categoryLabel returns a human-readable label for a finding category,
// e.g. "repo-renamed" -> "Repository renamed". Falls back to a
// hyphen-to-space title for unmapped categories so new slugs still read
// cleanly.
func categoryLabel(c checks.Category) string {
	switch c {
	case checks.NotPinned:
		return "Not pinned"
	case checks.MissingRef:
		return "Ref does not exist"
	case checks.InvalidWorkflow:
		return "Invalid workflow"
	case checks.RefChanged:
		return "Ref changed"
	case checks.MisleadingSHA:
		return "Misleading SHA"
	case checks.UnreachablePin:
		return "Pinned commit not in upstream history"
	case checks.TagCommitMismatch:
		return "Locked commit isn't the tag's commit"
	case checks.RepoRenamed:
		return "Repository renamed"
	case checks.RepoReplaced:
		return "Repository replaced (identity changed)"
	case checks.OwnerIDChanged:
		return "Repository owner identity changed"
	case checks.RepoUnavailable:
		return "Repository missing or inaccessible"
	case checks.Stale:
		return "Unused lockfile entry"
	case checks.IncompleteLockfile:
		return "Lockfile doesn't match locked manifest"
	}
	return strings.ReplaceAll(string(c), "-", " ")
}

// blockingFirst returns report with workflows blocked by a local-action error
// first, and those errors first within each workflow, so the run's real cause
// and its flags lead the output instead of an unrelated finding.
func blockingFirst(report *checks.Report) *checks.Report {
	isLocal := func(f checks.Finding) bool {
		return f.Category == checks.LocalAction && f.Severity == checks.SeverityError
	}
	out := *report
	out.Workflows = slices.Clone(report.Workflows)
	for i := range out.Workflows {
		ff := slices.Clone(out.Workflows[i].Findings)
		slices.SortStableFunc(ff, func(a, b checks.Finding) int { return boolRank(isLocal(a)) - boolRank(isLocal(b)) })
		out.Workflows[i].Findings = ff
	}
	slices.SortStableFunc(out.Workflows, func(a, b checks.WorkflowReport) int {
		return boolRank(slices.ContainsFunc(a.Findings, isLocal)) - boolRank(slices.ContainsFunc(b.Findings, isLocal))
	})
	return &out
}

func boolRank(b bool) int {
	if b {
		return 0
	}
	return 1
}

// renderErrorFindings groups error-level findings by dependency and prints
// per-dep detail lines followed by a category-count summary.
func renderErrorFindings(out *ui.UI, report *checks.Report, failedCount, checked int, exclude map[checks.Category]bool, willRemediate bool) {
	report = blockingFirst(report)
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
			if depKey == "" {
				depKey = wr.Path
			}
			if dg, ok := depMap[depKey]; ok {
				dg.findings = append(dg.findings, f)
			} else {
				depOrder = append(depOrder, depKey)
				depMap[depKey] = &depGroup{dep: depKey, findings: []checks.Finding{f}}
			}
		}
	}

	catCounts := map[checks.Category]int{}
	unverified := 0
	for _, dep := range depOrder {
		dg := depMap[dep]

		allSkip := true
		for _, f := range dg.findings {
			if findingExcluded(f, exclude, willRemediate) {
				continue
			}
			if f.Retryable {
				unverified++
			} else {
				catCounts[f.Category]++
			}
			if f.Category != checks.NotPinned || !f.IsRemediableNotPinned() {
				allSkip = false
			}
		}
		if allSkip {
			continue
		}

		// Self-hosted-runner findings are no longer generated; render
		// remaining non-excluded, non-not-pinned findings directly.
		for _, f := range dg.findings {
			if findingExcluded(f, exclude, willRemediate) ||
				f.IsRemediableNotPinned() {
				continue
			}
			renderFindingDetail(out, f, dep)
		}
	}

	parts := []string{}
	for _, cat := range []checks.Category{
		checks.RepoReplaced, checks.OwnerIDChanged, checks.RepoUnavailable, checks.RepoRenamed, checks.UnreachablePin, checks.TagCommitMismatch,
		checks.RefChanged, checks.NotPinned, checks.MissingRef, checks.InvalidWorkflow, checks.OnboardingRequired,
		checks.LocalAction, checks.InvalidSelfRepositoryRef,
		checks.Stale, checks.MisleadingSHA, checks.IncompleteLockfile,
	} {
		if n, ok := catCounts[cat]; ok {
			parts = append(parts, fmt.Sprintf("%d %s", n, string(cat)))
		}
	}
	if unverified > 0 {
		parts = append(parts, fmt.Sprintf("%d couldn't be verified", unverified))
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
	label := findingLabel(f)
	icon := out.Yellow(ui.IconWarning)
	if f.Severity == checks.SeverityError {
		icon = out.Red(ui.IconError)
	}
	out.Detail("%s %s %s", icon, out.Dim(label), dep)
	out.Detail("  %s", cmp.Or(f.TermDetail, f.Detail))
	if f.Category == checks.UnreachablePin && !f.RefMissing && f.Dependency != nil {
		owner, repo := f.Dependency.OwnerRepo()
		if owner != "" {
			out.Detail("  ↳ %s", out.Dim(DepReleaseURL(f.Dependency.Hostname, owner+"/"+repo, nil)))
		}
	}
	if (IsAlertedCategory(f.Category) || f.RefMissing) && f.Remediation != "" {
		out.Detail("  %s %s", out.Yellow(ui.IconWarning), f.Remediation)
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
	if u := f.DocURL(); u != "" {
		out.Detail("  see: %s", out.DocLink(u))
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
	localByWorkflow := map[string][]string{}
	localCount := 0
	for _, wr := range report.Workflows {
		for _, f := range wr.Findings {
			if f.Category == checks.LocalAction {
				if f.Severity == checks.SeverityError {
					continue
				}
				use := f.LocalPath
				if f.ParentNWO != "" {
					use += " (in " + f.ParentNWO + ")"
				}
				localByWorkflow[wr.Path] = append(localByWorkflow[wr.Path], use)
				localCount++
				continue
			}
			if f.IsWarning() {
				key := f.DepKey()
				if key == "" {
					key = f.WorkflowPath
				}
				// One dependency can draw warnings of different kinds (a
				// full-SHA `uses:` and its moved recorded tag); keep each.
				key += "\x00" + string(f.Category)
				if wg, ok := warnMap[key]; ok {
					wg.count++
					wg.workflows = append(wg.workflows, f.WorkflowPath)
				} else {
					warnOrder = append(warnOrder, key)
					warnMap[key] = &warningGroup{finding: f, count: 1, workflows: []string{f.WorkflowPath}}
				}
			}
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
		case f.Category == checks.RefMoved && !f.TagMoved:
			// Moved mutable refs are sticky; --relock advances them, so
			// there is nothing actionable to print here.
		case f.Category.IsInconclusive() &&
			strings.Contains(f.Remediation, "transitive dependency"):
			// transitive reachability unknown: silently swallowed
		default:
			otherDetailWarnings = append(otherDetailWarnings, key)
		}
	}

	if localCount > 0 {
		out.TermWarn("Cannot follow %d workspace-local %s", localCount, ui.Pluralize(localCount, "action", "actions"))
		for _, p := range slices.Sorted(maps.Keys(localByWorkflow)) {
			out.TermDetail("↳ %s: %s", p, strings.Join(localByWorkflow[p], ", "))
		}
		out.TermDetail("↳ %s", checks.LocalAction.DocURL())
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
		if f.Category.IsInconclusive() || f.TagMoved {
			label := depKey
			if label == "" {
				label = f.WorkflowPath
			}
			if f.TagMoved {
				out.TermWarn("%s %s", findingLabel(f), label)
				out.TermDetail("↳ %s", cmp.Or(f.TermDetail, f.Detail))
			} else {
				out.TermWarn("%s: %s", label, cmp.Or(f.TermDetail, f.Detail))
			}
			if f.Remediation != "" {
				out.TermDetail("↳ %s", f.Remediation)
			}
		}
	}
	renderRedirects(out, report, willRemediate)
	renderContainerImages(out, report)
}

// renderContainerImages prints one warning per unpinned image, listing
// each workflow that uses it and where.
func renderContainerImages(out *ui.UI, report *checks.Report) {
	var order []string
	byImage := map[string][]checks.Finding{}
	for _, wr := range report.Workflows {
		for _, f := range wr.Findings {
			if f.Category != checks.ContainerImage {
				continue
			}
			head := checks.ImageHeadline(f.ImageUses[0])
			if _, ok := byImage[head]; !ok {
				order = append(order, head)
			}
			byImage[head] = append(byImage[head], f)
		}
	}
	for _, head := range order {
		out.TermWarn("%s", head)
		var fixes []string
		for _, f := range byImage[head] {
			var where []string
			for _, u := range f.ImageUses {
				where = append(where, u.Where)
			}
			out.TermDetail("↳ used by %s: %s", f.WorkflowPath, strings.Join(where, ", "))
			if !slices.Contains(fixes, f.Remediation) {
				fixes = append(fixes, f.Remediation)
			}
		}
		for _, fix := range fixes {
			out.TermDetail("↳ %s", fix)
		}
		out.TermDetail("↳ %s", checks.ContainerImage.DocURL())
	}
}

// renderRedirects lists pins whose repository was renamed or transferred
// but kept its repo ID. The runner follows the redirect, so it's a warning
// with the replacement `uses:` line. A fix run rewrites workflow refs and
// reports that instead.
func renderRedirects(out *ui.UI, report *checks.Report, willRemediate bool) {
	for _, wr := range report.Workflows {
		for _, f := range wr.Findings {
			if f.Category == checks.RepoRenamed && f.IsWarning() && !willRemediate {
				out.TermWarn("%s: %s", f.WorkflowPath, f.Detail)
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
	case checks.UnreachablePin, checks.TagCommitMismatch, checks.MisleadingSHA, checks.OnboardingRequired, checks.RepoRenamed, checks.RepoReplaced, checks.OwnerIDChanged, checks.RepoUnavailable, checks.IncompleteLockfile:
		return true
	}
	return false
}

// IsAutoFixable reports whether a plain `gh actions-lock` run (no --no-fix)
// re-pins the finding without operator intervention. Only structural drift
// qualifies: a missing, changed, or orphaned lock entry. Integrity failures
// (unreachable-pin, misleading-sha) need investigation or --accept-moved, and
// a blocking local action needs a source change — so none of those should
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
