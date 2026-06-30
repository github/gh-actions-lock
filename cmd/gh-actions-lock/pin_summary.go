package main

import (
	"context"
	"fmt"
	"strings"

	parserlock "github.com/github/actions-lockfile/go/pkg/lockfile"
	"github.com/github/gh-actions-lock/cmd/gh-actions-lock/format"
	"github.com/github/gh-actions-lock/internal/pin"
	"github.com/github/gh-actions-lock/internal/pipeline/checks"
	"github.com/github/gh-actions-lock/internal/resolve"
	"github.com/github/gh-actions-lock/internal/ui"
)

// reportHasUnfixableErrors returns true when the report contains error-
// severity findings that the autofix cannot resolve. Pinning resolves
// not-pinned findings, so those are expected in the pre-fix report and
// don't count. LocalAction and LockfileForgery errors are unfixable --
// the workflow or lockfile must be investigated.
func reportHasUnfixableErrors(report *checks.Report, acceptMoved bool) bool {
	for _, wr := range report.Workflows {
		for _, f := range wr.Findings {
			if f.Severity != checks.SeverityError {
				continue
			}
			switch f.Category {
			case checks.LocalAction:
				return true
			case checks.LockfileForgery:
				if !acceptMoved {
					return true
				}
			case checks.UnresolvableCommit, checks.ReachabilityUnverified, checks.AncestryUnknown:
				// Fail-closed: a definitively unreachable pin, an
				// unverifiable re-resolution, and an inconclusive ancestry
				// verdict all block — in JSON mode too (this gate feeds the
				// --json exit path, bypassing the terminal-only rescan gate).
				return true
			}
		}
	}
	return false
}

// reportHasNonInvestigatedUnfixableErrors is like reportHasUnfixableErrors
// but only matches categories that renderInvestigationAlerts does NOT
// handle (LocalAction). Use this to gate the PresentResults call so
// lockfile-forgery findings don't trigger a redundant (and stale) error
// summary.
func reportHasNonInvestigatedUnfixableErrors(report *checks.Report) bool {
	for _, wr := range report.Workflows {
		for _, f := range wr.Findings {
			if f.Severity != checks.SeverityError {
				continue
			}
			if f.Category == checks.LocalAction {
				return true
			}
		}
	}
	return false
}

// renderPinSummary prints the terminal summary after pin.Plan + pin.Commit.
// It groups pinned entries by NWO@Ref, shows investigation alerts, unresolved
// warnings, and the all-valid message when nothing changed.
func renderPinSummary(ctx context.Context, console *ui.UI, record *pin.Record, report *checks.Report, r *resolve.Resolver, skippedRescan int, hasInconclusive bool, refusedLabels []string, noNarrow bool, acceptMoved bool, originalVersion string) error {
	pinned := record.Pinned()
	investigated := record.Investigated()
	narrowed := record.Narrowed()

	if len(pinned) > 0 {
		console.TermBlank()
		renderPinnedEntries(console, pinned)
	}

	if len(narrowed) > 0 && len(pinned) == 0 {
		console.TermBlank()
	}
	if len(narrowed) > 0 {
		renderNarrowedEntries(console, narrowed)
	}

	renderFullScanWarnings(console, pinned)
	if !noNarrow {
		renderVersionRefNudge(ctx, console, record, r)
	}

	if len(investigated) > 0 {
		renderInvestigationAlerts(console, investigated, r)
	}

	unresolvedEntries := record.Unresolved()
	if len(unresolvedEntries) > 0 {
		renderUnresolvedWarnings(console, unresolvedEntries)
	}

	total := len(report.Workflows)
	if total == 0 {
		console.TermNeutral("No workflows to check")
		return nil
	}

	// Surface lockfile schema upgrade when the on-disk version differs from
	// the current binary's version (e.g. v0.0.1 → v0.0.2).
	if originalVersion != "" && originalVersion != parserlock.Version {
		console.TermDetail("Upgraded lockfile schema %s → %s", originalVersion, parserlock.Version)
	}

	onboardingRefused := len(refusedLabels)
	allClean := len(pinned) == 0 && len(investigated) == 0 && len(unresolvedEntries) == 0
	hasUnfixable := reportHasUnfixableErrors(report, acceptMoved)
	if allClean && !hasUnfixable && onboardingRefused == 0 && !hasInconclusive {
		console.TermBlank()
		console.TermSuccess("All %d %s valid", total, ui.Pluralize(total, "workflow", "workflows"))
		if skippedRescan > 0 {
			console.TermDetail("Trusted lockfile for %d already-pinned %s; run `gh actions-lock --rescan` to re-verify reachability.",
				skippedRescan, ui.Pluralize(skippedRescan, "workflow", "workflows"))
		}
		return nil
	}

	if onboardingRefused > 0 {
		console.TermBlank()
		console.TermCaution("%d onboarding-required %s skipped — re-run without --no-onboard to add %s",
			onboardingRefused, ui.Pluralize(onboardingRefused, "entry", "entries"),
			ui.Pluralize(onboardingRefused, "it", "them"))
		for _, label := range refusedLabels {
			console.TermDetail("  %s", console.TermYellow(label))
		}
	}

	// Surface error-severity findings that the autofix can't resolve
	// (e.g. local-action on an already-onboarded workflow).
	// PresentResults already rendered these during the diagnose phase,
	// but the narration log was attached (discarded in terminal mode)
	// so they didn't reach stderr. Temporarily detach the log so the
	// findings surface on the terminal.
	//
	// Only trigger for categories NOT already rendered by
	// renderInvestigationAlerts (which handles lockfile-forgery).
	// Without this gate PresentResults would also emit a stale summary
	// line counting pre-fix not-pinned findings.
	if reportHasNonInvestigatedUnfixableErrors(report) {
		console.SetLog(nil)
		format.PresentResults(console, report, false, false,
			checks.LockfileForgery)
	}

	if len(investigated) > 0 || len(unresolvedEntries) > 0 || hasUnfixable {
		return errSilent
	}
	return nil
}

// renderNarrowedEntries shows refs that were upgraded from mutable (main, v4)
// to full semver (v6.0.2) on already-pinned workflows.
func renderNarrowedEntries(console *ui.UI, narrowed []pin.Entry) {
	console.TermSuccess("Narrowed %d %s to full semver",
		len(narrowed), ui.Pluralize(len(narrowed), "ref", "refs"))
	for _, e := range narrowed {
		console.TermDetail(" %s@%s → %s", e.NWO, e.AutoFixedRef, e.Ref)
	}
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
	// First pass: collect direct entries into groups.
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
	// Second pass: merge workflow attributions from transitive entries whose
	// NWO@Ref matches an existing direct group. This ensures that a dep
	// discovered via composite expansion shows the consuming workflow.
	// Collect purely transitive entries (no direct counterpart) separately.
	type transitiveEntry struct {
		pin.Entry
		parentLabel string
	}
	var purelyTransitive []transitiveEntry
	transitiveKeys := map[string]bool{}
	for _, e := range pinned {
		if e.Direct {
			continue
		}
		key := e.NWO + "@" + e.Ref
		idx, ok := seen[key]
		if ok {
			for _, wf := range e.Workflows {
				if !groupWFs[idx][wf] {
					groupWFs[idx][wf] = true
					grouped[idx].workflows = append(grouped[idx].workflows, wf)
				}
				workflowSet[wf] = true
			}
			continue
		}
		if transitiveKeys[key] {
			continue
		}
		transitiveKeys[key] = true
		parent := ""
		if len(e.RequiredBy) > 0 {
			parent = e.RequiredBy[0]
		}
		purelyTransitive = append(purelyTransitive, transitiveEntry{
			Entry:       e,
			parentLabel: parent,
		})
	}
	if directCount == 0 && len(purelyTransitive) == 0 {
		return
	}
	transitiveLabel := ""
	if len(purelyTransitive) > 0 {
		transitiveLabel = fmt.Sprintf(" (+ %d transitive)", len(purelyTransitive))
	}
	console.TermSuccess("Pinned %d %s%s across %d %s",
		directCount, ui.Pluralize(directCount, "action", "actions"),
		transitiveLabel,
		len(workflowSet), ui.Pluralize(len(workflowSet), "workflow", "workflows"))
	for _, g := range grouped {
		short := g.SHA
		if len(short) > 7 {
			short = short[:7]
		}
		var label string
		if looksLikeSHA(g.Ref) {
			label = g.NWO + "@" + short
		} else {
			label = g.NWO + "@" + g.Ref
			if short != "" {
				label = fmt.Sprintf("%s (%s)", label, short)
			}
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
	if len(purelyTransitive) > 0 {
		console.TermBlank()
		console.TermDetail("Transitive dependencies (from composite actions):")
	}
	for _, te := range purelyTransitive {
		short := te.SHA
		if len(short) > 7 {
			short = short[:7]
		}
		// When the ref IS the full SHA (composite actions pin by commit),
		// just show NWO@short instead of the redundant full-sha (short).
		var label string
		if te.Ref == te.SHA || (len(te.Ref) >= 40 && te.Ref == te.SHA[:len(te.Ref)]) {
			label = te.NWO + "@" + short
		} else {
			label = te.NWO + "@" + te.Ref
			if short != "" {
				label = fmt.Sprintf("%s (%s)", label, short)
			}
		}
		via := ""
		if te.parentLabel != "" {
			via = fmt.Sprintf(" via %s", te.parentLabel)
		}
		console.TermDetail("  %s%s", console.TermDim(label), console.TermDim(via))
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
// require manual investigation (forgery, orphaned commits, etc.).
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

	console.TermError("%d %s %s investigation — do not auto-pin",
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
			console.TermBlank()
			for _, hint := range strings.Split(b.fixHint, "\n") {
				console.TermDetail("%s", hint)
			}
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
	if strings.Contains(reason, "SSO authorization required") ||
		strings.Contains(reason, "SAML enforcement") {
		return strings.Join(ssoFixHints(), "\n")
	}
	return ""
}

// reportHasSSO returns true if any finding in the report contains
// SSO/SAML error text, indicating the user needs to authorize their token.
func reportHasSSO(report *checks.Report) bool {
	for _, wr := range report.Workflows {
		for _, f := range wr.Findings {
			if strings.Contains(f.Detail, "SSO authorization required") ||
				strings.Contains(f.Detail, "SAML enforcement") {
				return true
			}
		}
	}
	return false
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

// renderVersionRefNudge prints an informational nudge when entries are pinned
// with refs that are not full semver tags (v4.2.1). Full semver tags each
// resolve to exactly one commit, making the lock comment durable across
// re-pins. Only shown for repos that actually have semver releases.
func renderVersionRefNudge(ctx context.Context, console *ui.UI, record *pin.Record, r *resolve.Resolver) {
	type nudgeEntry struct {
		key       string // NWO@Ref
		latest    string // best full-semver tag, e.g. v1.2.3
		workflows []string
	}

	seen := map[string]*nudgeEntry{}
	// Cache per-repo so we don't call ListTags twice for the same repo.
	repoLatest := map[string]string{} // NWO → latest full semver (or "" if none)

	for _, e := range record.Entries {
		if e.Resolution != pin.Pinned && e.Resolution != pin.Verified {
			continue
		}
		if !e.Direct {
			continue
		}
		sv, ok := parserlock.ParseSemVer(e.Ref)
		if ok && sv.IsFull() {
			continue
		}
		// Only nudge for partial semver refs (v4, v3.1) — not arbitrary
		// branch names like canary, main, nightly. A ref must at least
		// parse as semver (partial) to be nudge-worthy.
		if !ok {
			continue
		}
		key := e.NWO + "@" + e.Ref
		if ne, exists := seen[key]; exists {
			for _, wf := range e.Workflows {
				ne.workflows = append(ne.workflows, wf)
			}
			continue
		}

		latest, cached := repoLatest[e.NWO]
		if !cached {
			latest = latestFullSemverTag(ctx, r, e.NWO)
			repoLatest[e.NWO] = latest
		}
		if latest == "" {
			continue // no semver releases — nothing to suggest
		}
		// Don't suggest a downgrade: if the user is on v3.4, only nudge
		// if the latest full semver is v3.4.x or higher.
		if latestSV, latestOK := parserlock.ParseSemVer(latest); latestOK {
			if !latestSV.Greater(sv) {
				continue
			}
		}
		seen[key] = &nudgeEntry{key: key, latest: latest, workflows: e.Workflows}
	}
	if len(seen) == 0 {
		return
	}
	entries := make([]*nudgeEntry, 0, len(seen))
	for _, ne := range seen {
		entries = append(entries, ne)
	}
	console.TermBlank()
	console.TermWarn("%d %s pinned without a full semver tag",
		len(entries), ui.Pluralize(len(entries), "action", "actions"))
	for _, ne := range entries {
		console.TermDetail("  %s %s latest: %s",
			console.TermYellow(ne.key),
			console.TermBold("→"),
			console.TermYellow(ne.latest))
		for _, wf := range ne.workflows {
			console.TermDetail("    %s", wf)
		}
	}
	console.TermDetail("  Update the uses: line in your workflow to the full version to lock precisely.")
}

// latestFullSemverTag returns the highest full semver tag (vX.Y.Z) for
// the given NWO, or "" if the repo has no semver releases or the lookup
// fails.
func latestFullSemverTag(ctx context.Context, r *resolve.Resolver, nwo string) string {
	if r == nil {
		return ""
	}
	owner, repo, _ := parserlock.SplitNWO(nwo)
	if owner == "" {
		return ""
	}
	tags, err := r.ListTagsForRepo(ctx, owner, repo)
	if err != nil {
		return ""
	}
	type semverTriple struct{ major, minor, patch int }
	var best semverTriple
	bestTag := ""
	for _, t := range tags {
		sv, ok := parserlock.ParseSemVer(t.Name)
		if !ok || !sv.IsFull() || !sv.IsStable() {
			continue
		}
		cur := semverTriple{sv.Major, sv.Minor, sv.Patch}
		if bestTag == "" ||
			cur.major > best.major ||
			(cur.major == best.major && cur.minor > best.minor) ||
			(cur.major == best.major && cur.minor == best.minor && cur.patch > best.patch) {
			best = cur
			bestTag = sv.Raw
		}
	}
	return bestTag
}

// looksLikeSHA returns true when ref is a hex string of SHA-1 (40) or
// SHA-256 (64) length.
func looksLikeSHA(ref string) bool {
	n := len(ref)
	if n != 40 && n != 64 {
		return false
	}
	for _, c := range ref {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
			return false
		}
	}
	return true
}
