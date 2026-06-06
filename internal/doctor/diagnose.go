package doctor

import (
	"context"
	"fmt"
	"strings"

	"github.com/github/gh-actions-pin/internal/audit"
	"github.com/github/gh-actions-pin/internal/cachekey"
	"github.com/github/gh-actions-pin/internal/lockfile"
	"github.com/github/gh-actions-pin/internal/resolve"
)

// Diagnose scans workflows and produces findings for each.
//
// It is a backward-compatible wrapper around ParseAll, resolver pre-warming,
// and DiagnoseParsed. Newer callers can drive those phases directly to control
// UI progress.
func Diagnose(ctx context.Context, paths []string, r *resolve.Resolver, store *lockfile.Store, onWorkflow ...func(done, total int, path string)) *Report {
	var onScan func(done, total int, path string)
	if len(onWorkflow) > 0 {
		onScan = onWorkflow[0]
	}
	parsed := ParseAll(paths, store, onScan)
	if r != nil {
		refs, deps := CollectResolvable(parsed)
		if len(refs) > 0 {
			_, _, _ = r.ResolveAllRecursive(ctx, refs)
		}
		if len(deps) > 0 {
			_ = audit.New(r).CheckReachabilityAll(ctx, deps)
		}
	}
	return DiagnoseParsed(ctx, parsed, r, store)
}

// ParsedWorkflow holds the per-workflow parse result that both phases need.
// LoadErr / DepsErr capture early failures so DiagnoseParsed can surface them
// as findings without re-loading the file.
type ParsedWorkflow struct {
	Path          string
	Refs          []lockfile.ActionRef
	ExistingDeps  []lockfile.Dependency
	ParseWarnings []string
	LoadErr       error
	DepsErr       error
	// Resolved, when true, instructs DiagnoseParsed to run this
	// workflow's diagnostics with a nil resolver. Network-bound checks
	// (ref-moved, impostor-commit) are skipped and the engine relies on
	// purely structural validation against the on-disk lockfile. Caller
	// is asserting "this workflow is already fully resolved" — typically
	// set on the fast path when every direct ref in the workflow is
	// already recorded in the lockfile.
	Resolved bool
	// SkipReachWhenUnchanged, when true, instructs DiagnoseParsed to skip
	// the per-dep reachability network call for any ExistingDep whose
	// (NWO, Ref, SHA) matches an entry in the freshly-resolved live deps
	// for this workflow. A Reachable result is synthesized in place. This
	// is the per-workflow analogue of the cmd-level fast path: when at
	// least one direct ref is new/changed (so the workflow couldn't be
	// fully trusted), the remaining unchanged pins still don't need a
	// fresh network reachability sweep on every run. Callers should leave
	// this false when --rescan or an equivalent "verify everything" flag
	// is in effect.
	SkipReachWhenUnchanged bool
}

// ParseAll loads and parses every workflow path, returning a slice in input
// order. onScan, if non-nil, fires with 1-based progress before each workflow
// is parsed so the UI can render [i/N] without leaking resolver detail.
func ParseAll(paths []string, store *lockfile.Store, onScan func(done, total int, path string)) []ParsedWorkflow {
	total := len(paths)
	out := make([]ParsedWorkflow, 0, total)
	for i, path := range paths {
		if onScan != nil {
			onScan(i+1, total, path)
		}
		pw := ParsedWorkflow{Path: path}
		wf, err := lockfile.Load(path)
		if err != nil {
			pw.LoadErr = err
			out = append(out, pw)
			continue
		}
		pw.Refs, _, pw.ParseWarnings = wf.ExtractActionRefs()
		if len(pw.Refs) > 0 {
			wfKey := lockfile.WorkflowKeyFromPath(path)
			deps, depsErr := store.Get(wfKey)
			if depsErr != nil {
				pw.DepsErr = depsErr
			} else {
				pw.ExistingDeps = deps
			}
		}
		out = append(out, pw)
	}
	return out
}

// CollectResolvable returns the deduplicated union of refs and existing deps
// across all parsed workflows. Use the returned slices to pre-warm the
// resolver caches once before per-workflow diagnostics.
func CollectResolvable(parsed []ParsedWorkflow) ([]lockfile.ActionRef, []lockfile.Dependency) {
	seenRef := make(map[cachekey.ActionRef]bool)
	var refs []lockfile.ActionRef
	for _, pw := range parsed {
		for _, ref := range pw.Refs {
			key := cachekey.ForActionRef(ref.Owner, ref.Repo, ref.Path, ref.Ref)
			if seenRef[key] {
				continue
			}
			seenRef[key] = true
			refs = append(refs, ref)
		}
	}
	seenDep := make(map[string]bool)
	var deps []lockfile.Dependency
	for _, pw := range parsed {
		for _, dep := range pw.ExistingDeps {
			key := dep.Key()
			if seenDep[key] {
				continue
			}
			seenDep[key] = true
			deps = append(deps, dep)
		}
	}
	return refs, deps
}

// DiagnoseParsed runs the engine diagnostics for each pre-parsed workflow.
// Assumes the resolver caches have already been warmed (calls into the
// resolver will hit cache and stay silent). Returns a Report aggregating per-
// workflow findings in input order.
func DiagnoseParsed(ctx context.Context, parsed []ParsedWorkflow, r *resolve.Resolver, store *lockfile.Store) *Report {
	report := &Report{}
	for _, pw := range parsed {
		effR := r
		if pw.Resolved {
			// Disk-only validation: this workflow is already fully resolved,
			// so no network round-trips. Engine falls back to structural-only
			// checks for this entry.
			effR = nil
		}
		report.Workflows = append(report.Workflows, diagnoseOneParsed(ctx, pw, effR, store))
	}
	return report
}

func diagnoseOneParsed(ctx context.Context, pw ParsedWorkflow, r *resolve.Resolver, store *lockfile.Store) WorkflowReport {
	wr := WorkflowReport{Path: pw.Path}

	if pw.LoadErr != nil {
		wr.Findings = append(wr.Findings, Finding{
			WorkflowPath: pw.Path,
			Category:     CategoryNotPinned,
			Severity:     SeverityError,
			// High: the YAML failed to load — concrete, file-level fact.
			Confidence: ConfidenceHigh,
			Detail:     fmt.Sprintf("failed to load workflow: %s", pw.LoadErr),
			DocURL:     DocURLFor(CategoryNotPinned),
		})
		return wr
	}

	wr.ActionRefs = pw.Refs
	wr.ParseWarnings = pw.ParseWarnings

	if len(pw.Refs) == 0 {
		wr.Findings = append(wr.Findings, Finding{
			WorkflowPath: pw.Path,
			Category:     CategoryRunOnly,
			Severity:     SeverityOK,
			Confidence:   ConfidenceHigh,
			Detail:       "no action references found",
		})
		return wr
	}

	if pw.DepsErr != nil {
		wr.Findings = append(wr.Findings, Finding{
			WorkflowPath: pw.Path,
			Category:     CategoryNotPinned,
			Severity:     SeverityError,
			Confidence:   ConfidenceHigh,
			Detail:       fmt.Sprintf("failed to read dependencies: %s", pw.DepsErr),
			Remediation:  "fix or regenerate the dependencies: section with `gh actions-pin`",
			DocURL:       DocURLFor(CategoryNotPinned),
		})
		return wr
	}
	wr.Deps = pw.ExistingDeps

	directNWOs := make(map[cachekey.Repo]bool, len(pw.Refs))
	for _, ref := range pw.Refs {
		directNWOs[cachekey.ForRepo(ref.Owner, ref.Repo)] = true
	}

	// Resolve live state: hits cache when ParseAll's caller pre-warmed the
	// resolver. Failure degrades to structural-only checks for any refs that
	// couldn't be resolved — partial results are kept.
	var liveDeps []lockfile.Dependency
	var resolvedParents resolve.ParentMap
	if r != nil {
		var resolveErr error
		liveDeps, resolvedParents, resolveErr = r.ResolveAllRecursive(ctx, pw.Refs)
		if resolveErr != nil {
			// Low: we're surfacing the resolver failure itself, not a
			// verdict about any specific dependency.
			wr.Findings = append(wr.Findings, Finding{
				WorkflowPath: pw.Path,
				Category:     CategoryReachabilityUnknown,
				Severity:     SeverityWarning,
				Confidence:   ConfidenceLow,
				Detail:       fmt.Sprintf("could not re-resolve actions: %s", resolveErr),
			})
		}
	}

	for _, dep := range pw.ExistingDeps {
		owner, repo := dep.OwnerRepo()
		wr.Inventory = append(wr.Inventory, InventoryEntry{
			Dep:    dep,
			File:   pw.Path,
			Direct: directNWOs[cachekey.ForRepo(owner, repo)],
		})
	}
	parentMap := map[string][]string{}
	if r != nil {
		parentMap = resolvedParents
		populateInventoryParents(wr.Inventory, parentMap)
	}

	var reach []resolve.ReachabilityResult
	var a *audit.Auditor
	if r != nil {
		a = audit.New(r)
	}
	if r != nil && len(pw.ExistingDeps) > 0 {
		toCheck, trusted := partitionReachByLive(pw.ExistingDeps, liveDeps, pw.SkipReachWhenUnchanged)
		reach = trusted
		if len(toCheck) > 0 {
			reach = append(reach, a.CheckReachabilityAll(ctx, toCheck)...)
		}
	}
	// Independent sweep for LIVE SHAs whose tag has moved: the
	// tag-hijacked-to-fork-network shape is invisible to the locked-SHA
	// sweep above (the lockfile entry is still legitimate; the live
	// SHA is the impostor). Kept separate so the result map's
	// (NWO, Ref, SHA) keys don't shadow the lockfile sweep — they
	// share NWO@Ref dep keys, which would confuse
	// reachabilityComplementFindings if mixed into `reach`.
	var liveMovedReach []resolve.ReachabilityResult
	if r != nil && len(liveDeps) > 0 && len(pw.ExistingDeps) > 0 {
		if moved := liveMovedDeps(pw.ExistingDeps, liveDeps); len(moved) > 0 {
			liveMovedReach = a.CheckReachabilityAll(ctx, moved)
		}
	}
	// Pin-time parity sweep: any (NWO, Ref, LIVE SHA) that neither the
	// locked-SHA sweep nor the tag-moved sweep covers gets a fresh reach
	// check here. Catches the NotPinned-direct impostor case and any
	// transitive composite live dep that isn't in the lockfile yet. With
	// this in place, applyPin's reach-loop Unreachable branch becomes a
	// fail-loud invariant rather than a primary detection path.
	var liveDirectReach []resolve.ReachabilityResult
	if r != nil && len(liveDeps) > 0 {
		if extra := liveDirectReachDeps(pw, liveDeps); len(extra) > 0 {
			liveDirectReach = a.CheckReachabilityAll(ctx, extra)
		}
	}
	var checkR checkResolver
	if r != nil && liveDeps != nil {
		checkR = newPrewarmedResolver(r, liveDeps, reach, liveMovedReach, liveDirectReach)
	}
	rawFindings := runChecks(ctx, pw, store.File(), checkR)

	depByKey := indexDeps(pw.ExistingDeps)
	for _, f := range rawFindings {
		if f.Category == CategoryStale && isTransitivePin(f, depByKey, parentMap) {
			continue
		}
		attachParent(&f, depByKey, directNWOs, parentMap)
		f.DocURL = DocURLFor(f.Category)
		wr.Findings = append(wr.Findings, f)
	}

	if len(reach) > 0 {
		wr.Findings = append(wr.Findings, reachabilityComplementFindings(pw.Path, reach, pw.ExistingDeps, directNWOs, parentMap, wr.Findings)...)
	}
	if len(liveDirectReach) > 0 {
		wr.Findings = append(wr.Findings, liveReachImpostorFindings(pw.Path, liveDirectReach, liveDeps, directNWOs, parentMap, wr.Findings)...)
	}

	if !hasIssues(wr.Findings) {
		wr.Findings = append(wr.Findings, Finding{
			WorkflowPath: pw.Path,
			Category:     CategoryValid,
			Severity:     SeverityOK,
			Confidence:   ConfidenceHigh,
			Detail:       "all dependencies pinned and verified",
		})
	}

	return wr
}

func indexDeps(deps []lockfile.Dependency) map[string]lockfile.Dependency {
	out := make(map[string]lockfile.Dependency, len(deps))
	for _, dep := range deps {
		out[dep.Key()] = dep
	}
	return out
}

// attachParent looks up the dep's composite-expansion parents (if any)
// and surfaces the first one as Finding.ParentNWO. Direct (workflow-level)
// uses don't get a parent attached even if one exists in the graph.
//
// Findings emitted by runChecks already carry an ActionRef for direct uses
// and a Dependency synthesized from the workflow ref / lockfile pin. This
// is purely about pointing the user at the composite that pulled in a
// transitively-pinned dep.
func attachParent(f *Finding, depByKey map[string]lockfile.Dependency, directNWOs map[cachekey.Repo]bool, parentMap map[string][]string) {
	if f.Dependency == nil {
		return
	}
	owner, repo := f.Dependency.OwnerRepo()
	if directNWOs[cachekey.ForRepo(owner, repo)] {
		return
	}
	// Prefer the dep snapshot from the workflow's ExistingDeps (it has the
	// canonical NWO casing the parent map keys with). Synthesised deps
	// already match — but the indexed lookup is cheap regardless.
	key := f.Dependency.Key()
	if dep, ok := depByKey[key]; ok {
		key = dep.Key()
	}
	if parents := parentMap[key]; len(parents) > 0 {
		f.ParentNWO = parents[0]
	}
}

// isTransitivePin reports whether the finding refers to a dep reached via
// composite expansion (i.e. has parents in the parent map).
func isTransitivePin(f Finding, depByKey map[string]lockfile.Dependency, parentMap map[string][]string) bool {
	if f.Dependency == nil {
		return false
	}
	if _, ok := depByKey[f.Dependency.Key()]; !ok {
		return false
	}
	return len(parentMap[f.Dependency.Key()]) > 0
}

// CollectReachDeps returns the deduplicated union of existing deps across the
// given parsed workflows that will need a fresh reachability network check
// once diagnostics runs. It mirrors the per-workflow partition diagnose
// performs internally (see partitionReachByLive) but operates over the union,
// so callers can pre-warm CheckReachabilityAll once across every unresolved
// workflow instead of paying the per-workflow repo-warmup + per-dep
// concurrency cost serially. Pass live as the result of a single
// ResolveAllRecursive over the union of refs (the resolver cache makes the
// per-workflow re-lookups inside diagnose free).
func CollectReachDeps(parsed []ParsedWorkflow, live []lockfile.Dependency) []lockfile.Dependency {
	if len(parsed) == 0 {
		return nil
	}
	liveSHA := make(map[string]string, len(live))
	for _, d := range live {
		liveSHA[d.Key()] = d.SHA
	}
	seen := make(map[string]bool)
	var out []lockfile.Dependency
	for _, pw := range parsed {
		if !pw.SkipReachWhenUnchanged {
			// When unchanged-skip isn't active (e.g. --rescan), the per-
			// workflow path will check every existing dep. Mirror that so
			// pre-warm sees the full set.
			for _, d := range pw.ExistingDeps {
				if seen[d.Key()] {
					continue
				}
				seen[d.Key()] = true
				out = append(out, d)
			}
			continue
		}
		for _, d := range pw.ExistingDeps {
			sha, ok := liveSHA[d.Key()]
			if ok && strings.EqualFold(sha, d.SHA) {
				continue
			}
			if seen[d.Key()] {
				continue
			}
			seen[d.Key()] = true
			out = append(out, d)
		}
	}
	return out
}

// CollectLiveMovedReachDeps returns the deduplicated set of synthetic
// dependencies (NWO, Ref + LIVE SHA) for which a reachability check
// should be pre-warmed. Each entry pairs an existing lockfile dep with
// the LIVE SHA it currently resolves to, when they differ — the input
// that lets the engine emit CategoryImpostorCommit for the
// tag-hijacked-to-fork-network shape. Pass live as the result of a
// single ResolveAllRecursive over the union of refs.
func CollectLiveMovedReachDeps(parsed []ParsedWorkflow, live []lockfile.Dependency) []lockfile.Dependency {
	if len(parsed) == 0 || len(live) == 0 {
		return nil
	}
	liveSHA := make(map[string]string, len(live))
	liveDep := make(map[string]lockfile.Dependency, len(live))
	for _, d := range live {
		liveSHA[d.Key()] = d.SHA
		liveDep[d.Key()] = d
	}
	seen := make(map[cachekey.Reach]bool)
	var out []lockfile.Dependency
	for _, pw := range parsed {
		for _, d := range pw.ExistingDeps {
			ls, ok := liveSHA[d.Key()]
			if !ok || strings.EqualFold(ls, d.SHA) {
				continue
			}
			synthetic := d
			synthetic.SHA = ls
			// Prefer the live dep's NWO casing if the live resolve has
			// one — it's the canonical one returned by the API.
			if ld, ok := liveDep[d.Key()]; ok && ld.NWO != "" {
				synthetic.NWO = ld.NWO
			}
			owner, repo := synthetic.OwnerRepo()
			k := cachekey.ForReach(owner, repo, synthetic.SHA, synthetic.Ref)
			if seen[k] {
				continue
			}
			seen[k] = true
			out = append(out, synthetic)
		}
	}
	return out
}

// liveDirectReachDeps returns live-resolved deps whose (NWO, Ref, SHA)
// isn't already covered by the locked-SHA sweep (partitionReachByLive) or
// the tag-moved sweep (liveMovedDeps), so the engine can give them a
// fresh reachability check before pinning. Covers two pin-time impostor
// shapes that the existing diagnose paths miss:
//
//   - NotPinned workflow: no ExistingDep at all, so the locked-SHA sweep
//     never runs. Without this, applyPin's reach loop is the only thing
//     catching these — diagnose now fires the CategoryImpostorCommit
//     finding pre-pin so the auto-fix runs via tryAutoFixImpostors.
//   - Transitive composite dep that ResolveAllRecursive discovered but
//     isn't yet in the lockfile. The locked-SHA sweep can't see it; the
//     live-moved sweep only fires when an ExistingDep exists for the same
//     dep key with a different SHA.
//
// Dedup by cachekey.Reach across direct + transitive entries.
func liveDirectReachDeps(pw ParsedWorkflow, live []lockfile.Dependency) []lockfile.Dependency {
	if len(live) == 0 {
		return nil
	}
	covered := make(map[cachekey.Reach]bool, len(pw.ExistingDeps)+len(live))
	existingByDepKey := make(map[string]lockfile.Dependency, len(pw.ExistingDeps))
	for _, d := range pw.ExistingDeps {
		owner, repo := d.OwnerRepo()
		covered[cachekey.ForReach(owner, repo, d.SHA, d.Ref)] = true
		existingByDepKey[d.Key()] = d
	}
	for _, d := range live {
		ed, ok := existingByDepKey[d.Key()]
		if !ok || strings.EqualFold(ed.SHA, d.SHA) {
			continue
		}
		owner, repo := d.OwnerRepo()
		covered[cachekey.ForReach(owner, repo, d.SHA, d.Ref)] = true
	}
	seen := make(map[cachekey.Reach]bool, len(live))
	var out []lockfile.Dependency
	for _, d := range live {
		owner, repo := d.OwnerRepo()
		k := cachekey.ForReach(owner, repo, d.SHA, d.Ref)
		if covered[k] || seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, d)
	}
	return out
}

// CollectLiveDirectReachDeps is the cmd-level pre-warm analogue of
// liveDirectReachDeps. Returns the deduplicated set of synthetic live
// deps across all parsed workflows that need a fresh reachability check
// because they're outside both the locked-SHA and live-moved sweeps. On
// a fully steady-state lockfile this is empty; on a brand-new repo (no
// lockfile yet) it's the full live set.
func CollectLiveDirectReachDeps(parsed []ParsedWorkflow, live []lockfile.Dependency) []lockfile.Dependency {
	if len(parsed) == 0 || len(live) == 0 {
		return nil
	}
	covered := make(map[cachekey.Reach]bool)
	existingByDepKey := make(map[string]lockfile.Dependency)
	for _, pw := range parsed {
		for _, d := range pw.ExistingDeps {
			owner, repo := d.OwnerRepo()
			covered[cachekey.ForReach(owner, repo, d.SHA, d.Ref)] = true
			existingByDepKey[d.Key()] = d
		}
	}
	for _, d := range live {
		ed, ok := existingByDepKey[d.Key()]
		if !ok || strings.EqualFold(ed.SHA, d.SHA) {
			continue
		}
		owner, repo := d.OwnerRepo()
		covered[cachekey.ForReach(owner, repo, d.SHA, d.Ref)] = true
	}
	seen := make(map[cachekey.Reach]bool, len(live))
	var out []lockfile.Dependency
	for _, d := range live {
		owner, repo := d.OwnerRepo()
		k := cachekey.ForReach(owner, repo, d.SHA, d.Ref)
		if covered[k] || seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, d)
	}
	return out
}

// liveMovedDeps is the per-workflow analogue of CollectLiveMovedReachDeps.
// Returns synthetic (NWO, Ref, LIVE SHA) deps for any existing dep whose
// live resolve differs from the recorded SHA.
func liveMovedDeps(existing, live []lockfile.Dependency) []lockfile.Dependency {
	if len(existing) == 0 || len(live) == 0 {
		return nil
	}
	liveSHA := make(map[string]string, len(live))
	liveDep := make(map[string]lockfile.Dependency, len(live))
	for _, d := range live {
		liveSHA[d.Key()] = d.SHA
		liveDep[d.Key()] = d
	}
	seen := make(map[cachekey.Reach]bool)
	var out []lockfile.Dependency
	for _, d := range existing {
		ls, ok := liveSHA[d.Key()]
		if !ok || strings.EqualFold(ls, d.SHA) {
			continue
		}
		synthetic := d
		synthetic.SHA = ls
		if ld, ok := liveDep[d.Key()]; ok && ld.NWO != "" {
			synthetic.NWO = ld.NWO
		}
		owner, repo := synthetic.OwnerRepo()
		k := cachekey.ForReach(owner, repo, synthetic.SHA, synthetic.Ref)
		if seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, synthetic)
	}
	return out
}

// partitionReachByLive splits existing deps into the set that needs a fresh
// reachability network check and the set that can be synthesized as
// Reachable because the freshly-resolved live deps confirm the recorded
// (NWO, Ref, SHA) is still what the ref resolves to right now.
//
// When skipUnchanged is false, every existing dep goes to toCheck. This
// is the --rescan path: re-verify every recorded pin against current
// upstream branches.
func partitionReachByLive(existing, live []lockfile.Dependency, skipUnchanged bool) (toCheck []lockfile.Dependency, trusted []resolve.ReachabilityResult) {
	if !skipUnchanged || len(live) == 0 {
		return existing, nil
	}
	liveSHA := make(map[string]string, len(live))
	for _, d := range live {
		liveSHA[d.Key()] = d.SHA
	}
	for _, d := range existing {
		sha, ok := liveSHA[d.Key()]
		if !ok || !strings.EqualFold(sha, d.SHA) {
			toCheck = append(toCheck, d)
			continue
		}
		owner, repo := d.OwnerRepo()
		trusted = append(trusted, resolve.ReachabilityResult{
			Owner:  owner,
			Repo:   repo,
			Ref:    d.Ref,
			SHA:    d.SHA,
			DepKey: d.Key(),
			Status: resolve.Reachable,
			Detail: "lockfile entry unchanged and live resolve confirms SHA — prior reachability verification retained",
		})
	}
	return toCheck, trusted
}

// reachabilityComplementFindings covers the cases the engine doesn't:
//   - Impostor for transitive (composite-expanded) deps the engine never
//     visits because they aren't in workflow uses.
//   - Reachability-Unknown warnings for all deps (engine fails open on
//     Unknown). Direct + transitive both get a warning so the user knows
//     the check was inconclusive.
func reachabilityComplementFindings(
	path string,
	reach []resolve.ReachabilityResult,
	deps []lockfile.Dependency,
	directNWOs map[cachekey.Repo]bool,
	parentMap map[string][]string,
	existing []Finding,
) []Finding {
	if len(reach) == 0 {
		return nil
	}

	forgeryKeys := map[string]bool{}
	for _, f := range existing {
		if f.Category == CategoryLockfileForgery && f.Dependency != nil {
			forgeryKeys[f.Dependency.Key()] = true
		}
	}

	depByKey := make(map[string]lockfile.Dependency, len(deps))
	for _, d := range deps {
		depByKey[d.Key()] = d
	}

	var out []Finding
	for _, rr := range reach {
		dep, ok := depByKey[rr.DepKey]
		if !ok {
			continue
		}
		depCopy := dep
		owner, repo := dep.OwnerRepo()
		direct := directNWOs[cachekey.ForRepo(owner, repo)]
		parent := ""
		if parents := parentMap[rr.DepKey]; len(parents) > 0 {
			parent = parents[0]
		}
		switch rr.Status {
		case resolve.Unreachable:
			if direct {
				continue // engine emits impostor for direct uses
			}
			if forgeryKeys[rr.DepKey] {
				continue
			}
			// High: branch_commits returned an authoritative
			// "unreachable" for this transitive pin.
			out = append(out, Finding{
				WorkflowPath: path,
				Category:     CategoryImpostorCommit,
				Severity:     SeverityError,
				Confidence:   ConfidenceHigh,
				Dependency:   &depCopy,
				ParentNWO:    parent,
				Detail:       rr.Detail,
				Remediation:  "investigate immediately — the lockfile entry may have been injected",
				DocURL:       DocURLFor(CategoryImpostorCommit),
			})
		case resolve.ReachabilityUnknown:
			remediation := "transitive dependency pinned to a bare SHA — reachability cannot be verified"
			if direct {
				remediation = "reachability check inconclusive — retry when network/API is available"
			}
			// Low: we couldn't get a reachability answer at all.
			out = append(out, Finding{
				WorkflowPath: path,
				Category:     CategoryReachabilityUnknown,
				Severity:     SeverityWarning,
				Confidence:   ConfidenceLow,
				Dependency:   &depCopy,
				ParentNWO:    parent,
				Detail:       rr.Detail,
				Remediation:  remediation,
			})
		}
	}
	return out
}

// liveReachImpostorFindings emits CategoryImpostorCommit for live-resolved
// SHAs that come back Unreachable from the live-direct sweep. Operates on
// synthetic live deps (not pw.ExistingDeps), so it fires for unpinned and
// transitive-not-in-lockfile cases that reachabilityComplementFindings
// (keyed on existing deps) can't see.
//
// Suppresses duplicates against any prior impostor/forgery finding for the
// same dep key — the engine's checkImpostorCommit may have already emitted
// for a direct ref via the live-ref-vs-locked compare in check_misleading.
func liveReachImpostorFindings(
	path string,
	reach []resolve.ReachabilityResult,
	live []lockfile.Dependency,
	directNWOs map[cachekey.Repo]bool,
	parentMap map[string][]string,
	existing []Finding,
) []Finding {
	if len(reach) == 0 {
		return nil
	}
	covered := map[string]bool{}
	for _, f := range existing {
		if f.Dependency == nil {
			continue
		}
		switch f.Category {
		case CategoryImpostorCommit, CategoryLockfileForgery:
			covered[f.Dependency.Key()] = true
		}
	}
	liveByReachKey := make(map[cachekey.Reach]lockfile.Dependency, len(live))
	for _, d := range live {
		owner, repo := d.OwnerRepo()
		liveByReachKey[cachekey.ForReach(owner, repo, d.SHA, d.Ref)] = d
	}
	var out []Finding
	for _, rr := range reach {
		if rr.Status != resolve.Unreachable {
			continue
		}
		dep, ok := liveByReachKey[cachekey.ForReach(rr.Owner, rr.Repo, rr.SHA, rr.Ref)]
		if !ok {
			continue
		}
		if covered[dep.Key()] {
			continue
		}
		depCopy := dep
		owner, repo := dep.OwnerRepo()
		direct := directNWOs[cachekey.ForRepo(owner, repo)]
		parent := ""
		if !direct {
			if parents := parentMap[dep.Key()]; len(parents) > 0 {
				parent = parents[0]
			}
		}
		detail := rr.Detail
		if detail == "" {
			detail = fmt.Sprintf("live resolve of %s/%s@%s → %s is not reachable from any branch", owner, repo, dep.Ref, dep.SHA)
		}
		out = append(out, Finding{
			WorkflowPath: path,
			Category:     CategoryImpostorCommit,
			Severity:     SeverityError,
			Confidence:   ConfidenceHigh,
			Dependency:   &depCopy,
			ParentNWO:    parent,
			Detail:       detail,
			Remediation:  "investigate immediately — the live ref resolves to a commit that is not reachable from any branch",
			DocURL:       DocURLFor(CategoryImpostorCommit),
		})
		// Mark covered so a second reach result for the same dep doesn't double-emit.
		covered[dep.Key()] = true
	}
	return out
}

func hasIssues(findings []Finding) bool {
	for _, f := range findings {
		if f.Severity == SeverityError {
			return true
		}
		if f.Category.IsInconclusive() {
			continue
		}
		if f.Category != CategoryValid && f.Category != CategoryRunOnly && f.Severity == SeverityWarning {
			return true
		}
	}
	return false
}

func populateInventoryParents(inventory []InventoryEntry, parentMap map[string][]string) {
	for i := range inventory {
		if inventory[i].Direct || len(inventory[i].Parents) > 0 {
			continue
		}
		parents := parentMap[inventory[i].Dep.Key()]
		if len(parents) > 0 {
			inventory[i].Parents = append([]string(nil), parents...)
		}
	}
}
