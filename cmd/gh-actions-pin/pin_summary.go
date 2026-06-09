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
func renderPinSummary(console *ui.UI, record *pin.Record, report *checks.Report, r *resolve.Resolver, skippedRescan int) error {
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
	if len(pinned) == 0 && len(investigated) == 0 && len(unresolvedEntries) == 0 {
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

	if len(investigated) > 0 {
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
	console.TermError("%d %s %s maintainer action — pinned commit is not reachable from any branch",
		len(groups), ui.Pluralize(len(groups), "action", "actions"),
		ui.Pluralize(len(groups), "requires", "require"))
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
			console.TermDetail("    %s %s", console.TermBold("→"), pipeline.PublisherEscalationCopy)
			console.TermDetail("    see: %s", console.TermLink(console.TermDim("Using tags for release management"), pipeline.PublisherTagReleasesDocURL))
		}
	}
}

// renderUnresolvedWarnings prints caution-level warnings for actions whose
// refs could not be resolved (network errors, deleted repos, etc.).
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
	console.TermCaution("%d %s could not be resolved — %d %s affected",
		len(groups), ui.Pluralize(len(groups), "action", "actions"),
		len(affectedWFs), ui.Pluralize(len(affectedWFs), "workflow", "workflows"))
	for _, g := range groups {
		console.TermDetail("  %s", console.TermYellow(g.nwo+"@"+g.ref))
		if g.reason != "" {
			reason := g.reason
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
			console.TermDetail("    %s", console.TermDim(reason))
		}
	}
}
