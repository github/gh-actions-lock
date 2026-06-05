package main

import (
	"fmt"
	"strings"

	"github.com/github/gh-actions-pin/internal/doctor"
	"github.com/github/gh-actions-pin/internal/lockfile"
	"github.com/github/gh-actions-pin/internal/runlog"
)

// provenanceOutcomes carries the per-action resolution sets collected by the
// remediator, keyed by dependency key (NWO@Ref). The ordered slices preserve
// the remediator's first-seen order so synthesized actions land in the report
// in the same order the live summary used to emit them.
type provenanceOutcomes struct {
	alerted          map[string]bool
	skipped          map[string]bool
	unresolved       map[string]bool
	fullScan         map[string]bool
	reasons          map[string]string
	suggestions      map[string]string
	alertedWorkflows map[string][]string
	alertedOrder     []string
	skippedOrder     []string
	unresolvedOrder  []string
}

func newProvenanceOutcomes(alerted, skipped, unresolved, fullScan []string, reasons, suggestions map[string]string, alertedWorkflows map[string][]string) provenanceOutcomes {
	set := func(keys []string) map[string]bool {
		m := make(map[string]bool, len(keys))
		for _, k := range keys {
			m[k] = true
		}
		return m
	}
	if reasons == nil {
		reasons = map[string]string{}
	}
	if suggestions == nil {
		suggestions = map[string]string{}
	}
	if alertedWorkflows == nil {
		alertedWorkflows = map[string][]string{}
	}
	return provenanceOutcomes{
		alerted:          set(alerted),
		skipped:          set(skipped),
		unresolved:       set(unresolved),
		fullScan:         set(fullScan),
		reasons:          reasons,
		suggestions:      suggestions,
		alertedWorkflows: alertedWorkflows,
		alertedOrder:     alerted,
		skippedOrder:     skipped,
		unresolvedOrder:  unresolved,
	}
}

// buildProvenanceReport assembles the action-centric provenance document from
// the diagnosis report, the final lockfile state, and the remediator outcomes.
// Actions are deduplicated across workflows: each unique action@ref appears
// once with the list of workflows that reference it. autoFixed records any
// auto-fix rewrites (one entry per workflow+action) so downstream consumers
// (Dependabot, audit tooling) can see what was changed without diffing the
// workflow file.
func buildProvenanceReport(report *doctor.Report, store *lockfile.Store, valid bool, repo *runlog.RepoInfo, out provenanceOutcomes, autoFixed []doctor.AutoFixedImpostor) *runlog.Report {
	finalSHA := make(map[string]lockfile.Dependency)
	for _, d := range store.AllDeps() {
		finalSHA[d.Key()] = d
	}

	issueByKey := make(map[string]string)
	addIssue := func(f doctor.Finding) {
		k := f.DepKey()
		if k == "" || f.IsValid() {
			return
		}
		if _, ok := issueByKey[k]; !ok {
			issueByKey[k] = string(f.Category)
		}
	}
	for _, f := range report.RepoFindings {
		addIssue(f)
	}
	for _, wr := range report.Workflows {
		for _, f := range wr.Findings {
			addIssue(f)
		}
	}

	type agg struct {
		action runlog.Action
		wfSeen map[string]bool
		pSeen  map[string]bool
	}
	seen := make(map[string]*agg)
	var order []string

	upsert := func(key, nwo, ref string, direct bool, workflow string, parents []string) *agg {
		g, ok := seen[key]
		if !ok {
			g = &agg{
				action: runlog.Action{NWO: nwo, Ref: ref, Direct: direct},
				wfSeen: map[string]bool{},
				pSeen:  map[string]bool{},
			}
			seen[key] = g
			order = append(order, key)
		}
		if direct {
			g.action.Direct = true
		}
		if workflow != "" && !g.wfSeen[workflow] {
			g.wfSeen[workflow] = true
			g.action.Workflows = append(g.action.Workflows, workflow)
		}
		for _, p := range parents {
			if p != "" && !g.pSeen[p] {
				g.pSeen[p] = true
				g.action.RequiredBy = append(g.action.RequiredBy, p)
			}
		}
		return g
	}

	// Primary pass: every action that parsed into a workflow's inventory.
	for _, wr := range report.Workflows {
		for _, inv := range wr.Inventory {
			upsert(inv.Dep.Key(), inv.Dep.NWO, inv.Dep.Ref, inv.Direct, wr.Path, inv.Parents)
		}
	}

	// Secondary pass: actions that never made it into the lockfile inventory
	// (e.g. impostor/unreachable commits flagged for investigation) live only
	// in the findings. Fold them in so the record accounts for every action.
	foldFinding := func(f doctor.Finding) {
		key := f.DepKey()
		if key == "" {
			return
		}
		nwo, ref, sha := findingIdentity(f)
		var parents []string
		if f.ParentNWO != "" {
			parents = []string{f.ParentNWO}
		}
		g := upsert(key, nwo, ref, f.ParentNWO == "", f.WorkflowPath, parents)
		if g.action.SHA == "" {
			g.action.SHA = sha
		}
		// Record the resolver's live SHA whenever a finding surfaces one
		// that doesn't match the recorded SHA — makes misleading-sha and
		// ref-moved entries falsifiable rather than echoing the input ref.
		if f.ObservedSHA != "" && g.action.ObservedSHA == "" && !equalFoldHex(f.ObservedSHA, g.action.SHA) {
			g.action.ObservedSHA = f.ObservedSHA
		}
	}
	for _, f := range report.RepoFindings {
		foldFinding(f)
	}
	for _, wr := range report.Workflows {
		for _, f := range wr.Findings {
			foldFinding(f)
		}
	}

	// Tertiary pass: guarantee an action exists for every key the remediator
	// flagged. Alerted and unresolved keys can use the display form (NWO with
	// sub-action path, or the original uses-ref) that never matches an
	// inventory or finding entry keyed by the resolved Dependency.Key(). Without
	// this, a security alert could have no action to render. Synthesize a bare
	// action per missing key; classifyAction below assigns its resolution.
	ensure := func(keys []string) {
		for _, key := range keys {
			if _, ok := seen[key]; ok {
				continue
			}
			nwo, ref := splitDepKey(key)
			upsert(key, nwo, ref, true, "", nil)
		}
	}
	ensure(out.alertedOrder)
	ensure(out.unresolvedOrder)
	ensure(out.skippedOrder)

	var actions []runlog.Action
	var sum runlog.Summary
	for _, key := range order {
		a := seen[key].action
		if d, ok := finalSHA[key]; ok {
			a.SHA = d.SHA
			a.HashAlgo = d.HashAlgo
		}
		a.Issue = issueByKey[key]
		classifyAction(&a, key, out)

		// For alerted actions, prefer the remediator's blocked-workflow list so
		// the investigation block renders exactly the paths the live summary did
		// (alertWorkflow precedence), even when the action was synthesized.
		if a.Resolution == runlog.ResolutionInvestigate {
			if wfs := out.alertedWorkflows[key]; len(wfs) > 0 {
				a.Workflows = wfs
			}
		}

		switch a.Resolution {
		case runlog.ResolutionPinned:
			sum.Pinned++
		case runlog.ResolutionAlreadyPinned:
			sum.AlreadyPinned++
		case runlog.ResolutionInvestigate:
			sum.Investigate++
		case runlog.ResolutionSkipped:
			sum.Skipped++
		case runlog.ResolutionUnresolved:
			sum.Unresolved++
		}
		if out.fullScan[key] {
			sum.FullScan++
		}
		actions = append(actions, a)
	}

	sum.Workflows = len(report.Workflows)
	sum.Actions = len(actions)
	sum.Valid = valid

	var autoFixedOut []runlog.AutoFix
	for _, fx := range autoFixed {
		autoFixedOut = append(autoFixedOut, runlog.AutoFix{
			Workflow: fx.Workflow,
			NWO:      fx.NWO,
			FromRef:  fx.OldRef,
			FromSHA:  fx.OldSHA,
			ToRef:    fx.NewTag,
			ToSHA:    fx.NewSHA,
			Reason:   "impostor pin rewritten to reachable release",
		})
	}

	return &runlog.Report{
		Tool:      runlog.ToolInfo{Name: "gh-actions-pin", Version: cliVersion()},
		Repo:      repo,
		Summary:   sum,
		Actions:   actions,
		AutoFixed: autoFixedOut,
	}
}

// classifyAction sets the Resolution, How, and CanonicalBranch provenance for a
// single action based on the remediator outcomes and its final lockfile state.
func classifyAction(a *runlog.Action, key string, out provenanceOutcomes) {
	switch {
	case out.alerted[key]:
		a.Resolution = runlog.ResolutionInvestigate
		a.How = out.reasons[key]
		a.Reason = out.reasons[key]
		a.Suggestion = out.suggestions[key]
		a.Escalate = out.reasons[key] == doctor.ReasonImpostorOffBranch
	case out.unresolved[key]:
		a.Resolution = runlog.ResolutionUnresolved
		a.ResolveFailed = true
		a.How = "ref could not be resolved to a commit"
	case out.skipped[key]:
		a.Resolution = runlog.ResolutionSkipped
		a.How = "left unchanged this run"
	case out.fullScan[key]:
		f := false
		a.Resolution = runlog.ResolutionPinned
		a.CanonicalBranch = &f
		a.How = fmt.Sprintf("locked %s to %s — commit verified via full branch scan (not on a canonical branch)", a.Ref, shortProvSHA(a.SHA))
	case a.SHA != "":
		if a.Issue != "" {
			a.Resolution = runlog.ResolutionPinned
			a.How = fmt.Sprintf("locked %s to %s", a.Ref, shortProvSHA(a.SHA))
		} else {
			a.Resolution = runlog.ResolutionAlreadyPinned
			a.How = fmt.Sprintf("already locked to %s", shortProvSHA(a.SHA))
		}
	default:
		a.Resolution = runlog.ResolutionUnresolved
		a.How = "no commit SHA on record"
	}
}

// findingIdentity derives the action NWO, ref, and best-known SHA from a
// finding, preferring the resolved dependency over the raw `uses:` ref.
func findingIdentity(f doctor.Finding) (nwo, ref, sha string) {
	if f.Dependency != nil {
		nwo = f.Dependency.NWO
		ref = f.Dependency.Ref
		sha = f.Dependency.SHA
	}
	if f.ActionRef != nil {
		if nwo == "" {
			nwo = f.ActionRef.NWO()
		}
		if ref == "" {
			ref = f.ActionRef.Ref
		}
		if sha == "" && isHexSHA(f.ActionRef.Ref) {
			sha = f.ActionRef.Ref
		}
	}
	if sha == "" && f.ObservedSHA != "" {
		sha = f.ObservedSHA
	}
	return nwo, ref, sha
}

// splitDepKey splits a dependency key "NWO@Ref" into its NWO and ref halves on
// the final '@'. Refs never contain '@', so the last separator is the boundary;
// the NWO half preserves any sub-action path the key carried.
func splitDepKey(key string) (nwo, ref string) {
	if i := strings.LastIndexByte(key, '@'); i >= 0 {
		return key[:i], key[i+1:]
	}
	return key, ""
}

// isHexSHA reports whether s looks like a full 40-character hex commit SHA.
func isHexSHA(s string) bool {
	if len(s) != 40 {
		return false
	}
	for _, c := range s {
		switch {
		case c >= '0' && c <= '9', c >= 'a' && c <= 'f', c >= 'A' && c <= 'F':
		default:
			return false
		}
	}
	return true
}

// equalFoldHex reports whether two hex SHAs are equal ignoring case. Empty
// strings never compare equal — a missing SHA isn't a match.
func equalFoldHex(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		ca, cb := a[i], b[i]
		if ca >= 'A' && ca <= 'Z' {
			ca += 'a' - 'A'
		}
		if cb >= 'A' && cb <= 'Z' {
			cb += 'a' - 'A'
		}
		if ca != cb {
			return false
		}
	}
	return true
}

// shortProvSHA abbreviates a commit SHA for readable provenance copy.
func shortProvSHA(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}
