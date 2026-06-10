package main

import (
	"fmt"
	"strings"

	"github.com/github/gh-actions-pin/cmd/gh-actions-pin/format"
	"github.com/github/gh-actions-pin/internal/pin"
	"github.com/github/gh-actions-pin/internal/pipeline"
	"github.com/github/gh-actions-pin/internal/pipeline/checks"
	"github.com/github/gh-actions-pin/internal/resolve"
	"github.com/github/gh-actions-pin/internal/ui"
)

// renderPinSummary prints the terminal summary after pin.Plan + pin.Commit.
// It groups pinned entries by NWO@Ref, shows investigation alerts, unresolved
// warnings, and the all-valid message when nothing changed.
func renderPinSummary(console *ui.UI, record *pin.Record, report *checks.Report, r *resolve.Resolver, skippedRescan int, hasInconclusive bool) error {
	pinned := record.Pinned()
	investigated := record.Investigated()

	if len(pinned) > 0 {
		renderPinnedEntries(console, pinned)
	}

	renderFullScanWarnings(console, pinned)

	if len(investigated) > 0 {
		renderInvestigationAlerts(console, investigated, r)
	}

	unresolvedEntries := record.Unresolved()
	if len(unresolvedEntries) > 0 {
		renderUnresolvedWarnings(console, unresolvedEntries)
	}

	total := len(report.Workflows)
	if len(pinned) == 0 && len(investigated) == 0 && len(unresolvedEntries) == 0 && !hasInconclusive {
		console.TermSuccess("All %d %s valid", total, ui.Pluralize(total, "workflow", "workflows"))
		if skippedRescan > 0 {
			console.TermDetail("Trusted lockfile for %d already-pinned %s; run `gh actions-pin --rescan` to re-verify reachability.",
				skippedRescan, ui.Pluralize(skippedRescan, "workflow", "workflows"))
		}
		return nil
	}

	if len(unresolvedEntries) == 0 && len(investigated) == 0 {
		return nil
	}

	if len(investigated) > 0 || len(unresolvedEntries) > 0 {
		return errSilent
	}
	return nil
}

// renderPinnedEntries prints the "Pinned N actions across M workflows" block,
// deduplicating entries by NWO@Ref.
func renderPinnedEntries(console *ui.UI, pinned []pin.Entry) {
	type groupedEntry struct {
		pin.Entry
		workflows []string
	}
	seen := map[string]int{} // NWO@Ref → index
	var grouped []groupedEntry
	var groupWFs []map[string]bool // per-group workflow dedup
	directCount := 0
	workflowSet := map[string]bool{} // distinct workflows, for the header count
	for _, e := range pinned {
		if !e.Direct {
			continue
		}
		key := e.NWO + "@" + e.Ref
		idx, ok := seen[key]
		if !ok {
			idx = len(grouped)
			seen[key] = idx
			grouped = append(grouped, groupedEntry{Entry: e})
			groupWFs = append(groupWFs, map[string]bool{})
			directCount++
		}
		for _, wf := range e.Workflows {
			if !groupWFs[idx][wf] {
				groupWFs[idx][wf] = true
				grouped[idx].workflows = append(grouped[idx].workflows, wf)
			}
			workflowSet[wf] = true
		}
	}
	if directCount == 0 {
		return
	}
	console.TermSuccess("Pinned %d %s across %d %s",
		directCount, ui.Pluralize(directCount, "action", "actions"),
		len(workflowSet), ui.Pluralize(len(workflowSet), "workflow", "workflows"))
	for _, g := range grouped {
		short := g.SHA
		if len(short) > 7 {
			short = short[:7]
		}
		label := g.NWO + "@" + g.Ref
		if short != "" {
			label = fmt.Sprintf("%s (%s)", label, short)
		}
		console.TermDetail("  %s", console.TermYellow(label))
		for _, wf := range g.workflows {
			console.TermDetail("    └─ %s", console.TermDim(wf))
		}
		if g.AutoFixedRef != "" {
			prev := g.AutoFixedRef
			if len(prev) > 7 {
				prev = prev[:7]
			}
			console.TermDetail("    %s re-pinned from unreachable %s to %s",
				console.TermYellow("!"), console.TermDim(prev), console.TermBold(g.Ref))
		}
	}
}

// renderFullScanWarnings prints a caution block for pins verified only via
// a full branch scan (not on a canonical branch).
func renderFullScanWarnings(console *ui.UI, pinned []pin.Entry) {
	var fullScanEntries []pin.Entry
	for _, e := range pinned {
		if e.FullScan {
			fullScanEntries = append(fullScanEntries, e)
		}
	}
	if len(fullScanEntries) == 0 {
		return
	}
	console.TermBlank()
	console.TermCaution("%d %s pinned but not on a canonical branch — verified via full branch scan",
		len(fullScanEntries), ui.Pluralize(len(fullScanEntries), "action", "actions"))
	for _, e := range fullScanEntries {
		console.TermDetail("  %s", console.TermYellow(e.NWO+"@"+e.Ref))
	}
}

// renderInvestigationAlerts prints error-level alerts for entries that
// require manual investigation (impostor commits, forgery, etc.).
// Entries sharing the same NWO@Ref are grouped so the action line
// appears once with all affected workflows listed underneath.
func renderInvestigationAlerts(console *ui.UI, investigated []pin.Entry, r *resolve.Resolver) {
	type investigateGroup struct {
		pin.Entry
		workflows []string
	}
	seen := map[string]int{} // NWO@Ref → index
	var groups []investigateGroup
	var groupWFs []map[string]bool
	for _, e := range investigated {
		key := e.NWO + "@" + e.Ref
		idx, ok := seen[key]
		if !ok {
			idx = len(groups)
			seen[key] = idx
			groups = append(groups, investigateGroup{Entry: e})
			groupWFs = append(groupWFs, map[string]bool{})
		}
		for _, wf := range e.Workflows {
			if !groupWFs[idx][wf] {
				groupWFs[idx][wf] = true
				groups[idx].workflows = append(groups[idx].workflows, wf)
			}
		}
	}

	console.TermBlank()

	// Use a specific header when all entries are impostor-commit;
	// fall back to a generic header when other issue types are mixed in.
	allImpostor := true
	for _, g := range groups {
		if g.Issue != string(checks.ImpostorCommit) {
			allImpostor = false
			break
		}
	}
	if allImpostor {
		console.TermError("%d %s %s maintainer action — pinned commit is not reachable from any branch",
			len(groups), ui.Pluralize(len(groups), "action", "actions"),
			ui.Pluralize(len(groups), "requires", "require"))
	} else {
		console.TermError("%d %s %s investigation — do not auto-pin",
			len(groups), ui.Pluralize(len(groups), "action", "actions"),
			ui.Pluralize(len(groups), "requires", "require"))
	}
	for _, g := range groups {
		dep := g.NWO + "@" + g.Ref
		console.TermDetail("  %s", console.TermLink(console.TermYellow(dep), format.DepReleaseURL(dep, r.IsKnownTagObject)))
		for _, wf := range g.workflows {
			console.TermDetail("    └─ %s", console.TermDim(wf))
		}
		if g.Suggestion != "" {
			console.TermDetail("    %s Suggested re-pin: %s",
				console.TermBold("→"), console.TermYellow(g.NWO+"@"+g.Suggestion))
		}
		if g.Issue == string(checks.ImpostorCommit) {
			console.TermDetail("    %s %s", console.TermYellow("!"), pipeline.ImpostorCommitContext)
			console.TermDetail("    %s %s", console.TermBold("→"), pipeline.PublisherEscalationCopy)
			console.TermDetail("    see: %s", console.TermLink(console.TermDim("Using tags for release management"), pipeline.PublisherTagReleasesDocURL))
		}
	}
}

// renderUnresolvedWarnings prints error-level output for actions whose
// refs could not be resolved (network errors, deleted repos, etc.).
// When multiple actions share the same root cause (e.g. SSO enforcement),
// they are grouped under a single explanation to avoid noisy repetition.
func renderUnresolvedWarnings(console *ui.UI, unresolvedEntries []pin.Entry) {
	type unresolvedGroup struct {
		nwo    string
		ref    string
		reason string
		wfs    []string
	}
	seenU := map[string]int{}
	var groups []unresolvedGroup
	var groupWFs []map[string]bool   // per-group workflow dedup
	affectedWFs := map[string]bool{} // distinct workflows, for the header count
	for _, e := range unresolvedEntries {
		key := e.NWO + "@" + e.Ref
		idx, ok := seenU[key]
		if !ok {
			idx = len(groups)
			seenU[key] = idx
			groups = append(groups, unresolvedGroup{nwo: e.NWO, ref: e.Ref, reason: e.Reason})
			groupWFs = append(groupWFs, map[string]bool{})
		}
		for _, wf := range e.Workflows {
			if !groupWFs[idx][wf] {
				groupWFs[idx][wf] = true
				groups[idx].wfs = append(groups[idx].wfs, wf)
			}
			affectedWFs[wf] = true
		}
	}
	console.TermBlank()
	console.TermError("%d %s could not be resolved — %d %s affected",
		len(groups), ui.Pluralize(len(groups), "action", "actions"),
		len(affectedWFs), ui.Pluralize(len(affectedWFs), "workflow", "workflows"))

	// Group actions by their cleaned reason + fix hint so identical errors
	// are shown once with all affected actions listed underneath.
	type reasonBucket struct {
		cleaned string
		fixHint string
		deps    []string // "NWO@Ref" labels
	}
	var bucketOrder []string
	buckets := map[string]*reasonBucket{}
	var noReasonDeps []string

	for _, g := range groups {
		if g.reason == "" {
			noReasonDeps = append(noReasonDeps, g.nwo+"@"+g.ref)
			continue
		}
		cleaned, fixHint := cleanUnresolvedReason(g.reason, g.nwo, g.ref)
		key := cleaned + "\x00" + fixHint
		if b, ok := buckets[key]; ok {
			b.deps = append(b.deps, g.nwo+"@"+g.ref)
		} else {
			bucketOrder = append(bucketOrder, key)
			buckets[key] = &reasonBucket{
				cleaned: cleaned,
				fixHint: fixHint,
				deps:    []string{g.nwo + "@" + g.ref},
			}
		}
	}

	for _, key := range bucketOrder {
		b := buckets[key]
		if len(b.deps) == 1 {
			console.TermDetail("  %s", console.TermYellow(b.deps[0]))
		} else {
			for _, dep := range b.deps {
				console.TermDetail("  %s", console.TermYellow(dep))
			}
		}
		if b.cleaned != "" {
			console.TermDetail("  %s", console.TermDim(b.cleaned))
		}
		if b.fixHint != "" {
			console.TermDetail("  %s", b.fixHint)
		}
	}

	for _, dep := range noReasonDeps {
		console.TermDetail("  %s", console.TermYellow(dep))
	}
}

// cleanUnresolvedReason strips redundant prefixes from an unresolved entry's
// reason and returns the cleaned text plus an optional actionable fix hint.
// The stripped prefixes ("resolution failed: ", "NWO@Ref: ") are noise because
// the action name is already printed on the line above, and the wrapper text
// adds nothing for the human reader.
func cleanUnresolvedReason(reason, nwo, ref string) (string, string) {
	if reason == "" {
		return "", ""
	}

	// Strip "resolution failed: " wrapper added by plan.go.
	reason = strings.TrimPrefix(reason, "resolution failed: ")

	// Strip any "owner/repo@ref: " prefix — this might be the current action
	// or a different action that caused the cascade failure. The action name
	// is already shown on the line above; cross-action refs are noise.
	reason = stripNWORefPrefix(reason)

	// Multi-line: prefer the detail line over the category label.
	if nl := strings.IndexByte(reason, '\n'); nl > 0 {
		first := reason[:nl]
		rest := strings.TrimSpace(reason[nl+1:])
		if strings.HasSuffix(first, ":") && rest != "" {
			reason = rest
		} else {
			reason = first
		}
	}
	if nl := strings.IndexByte(reason, '\n'); nl > 0 {
		reason = reason[:nl]
	}
	reason = strings.TrimSpace(reason)

	fixHint := extractFixHint(reason)

	// When we extracted a fix hint, trim the trailing "Authorize it at <url>
	// and retry" noise from the reason — our → line replaces it.
	if fixHint != "" {
		for _, sep := range []string{". Authorize it at", " Authorize it at"} {
			if i := strings.Index(reason, sep); i > 0 {
				reason = strings.TrimSpace(reason[:i])
				break
			}
		}
	}

	return reason, fixHint
}

// extractFixHint returns an actionable hint for common resolution errors.
// Returns "" when no actionable guidance can be inferred.
func extractFixHint(reason string) string {
	// SSO/SAML enforcement: extract the authorization URL.
	// Matches cli/cli's format: "Authorize in your web browser:  <url>"
	if strings.Contains(reason, "SSO authorization required") ||
		strings.Contains(reason, "SAML enforcement") {
		if url := extractURLWithPrefix(reason, "https://github.com/orgs/"); url != "" {
			return fmt.Sprintf("Authorize in your web browser:  %s", url)
		}
	}
	return ""
}

// extractURLWithPrefix finds the first URL in text starting with prefix.
func extractURLWithPrefix(text, prefix string) string {
	idx := strings.Index(text, prefix)
	if idx < 0 {
		return ""
	}
	end := idx
	for end < len(text) && text[end] != ' ' && text[end] != '\n' && text[end] != ')' {
		end++
	}
	return text[idx:end]
}

// stripNWORefPrefix removes a leading "owner/repo@ref: " pattern from s.
// The ref can be a tag (v4.3.1), branch, or full SHA. This handles both
// the current action's own prefix and cross-action references that appear
// when resolution cascades through a shared dependency.
func stripNWORefPrefix(s string) string {
	// Pattern: word/word@non-space-colon-terminated, e.g.:
	//   "actions/checkout@v4.3.1: SSO authorization..."
	//   "actions/checkout@de0fac2e...: SSO authorization..."
	atIdx := strings.IndexByte(s, '@')
	if atIdx < 0 {
		return s
	}
	// Verify there's a "/" before the "@" (NWO shape).
	slashIdx := strings.IndexByte(s[:atIdx], '/')
	if slashIdx < 0 {
		return s
	}
	// Find ": " after the "@" — that's the separator between ref and message.
	rest := s[atIdx+1:]
	colonIdx := strings.Index(rest, ": ")
	if colonIdx < 0 {
		return s
	}
	// Validate the ref portion has no spaces (it's a contiguous token).
	ref := rest[:colonIdx]
	if strings.ContainsAny(ref, " \t\n") {
		return s
	}
	return rest[colonIdx+2:]
}
