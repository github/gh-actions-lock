package pin

import (
	"cmp"
	"context"
	"fmt"
	"strings"

	parserlock "github.com/github/actions-lockfile/go/pkg/lockfile"
	"github.com/github/gh-actions-lock/internal/dep"
	"github.com/github/gh-actions-lock/internal/lockfile"
	"github.com/github/gh-actions-lock/internal/pinpool"
	"github.com/github/gh-actions-lock/internal/pipeline/checks"
	"github.com/github/gh-actions-lock/internal/refresh"
	"github.com/github/gh-actions-lock/internal/resolve"
	"github.com/github/gh-actions-lock/internal/tag"
	"github.com/github/gh-actions-lock/internal/workflowfile"
)

// PlanOptions configures the Plan pass.
type PlanOptions struct {
	Resolver  *resolve.Resolver
	Tagger    *tag.Lister
	Store     *lockfile.State
	Pool      *pinpool.Pool
	RepoOwner string // for same-owner narrowing skip
	RepoName  string
	// NoNarrow disables tag narrowing: mutable version refs (v4, v3.1)
	// are kept as the lock comment instead of being resolved to full
	// patch tags (v4.2.1). Bare-SHA reverse lookup still applies.
	NoNarrow bool

	// AcceptMoved treats ref-moved and unreachable-pin findings as
	// resolvable: affected deps are pruned from the inventory and
	// re-resolved to their current live SHA.
	AcceptMoved bool

	// Relock treats ref-moved findings (a branch or partial-version ref
	// that legitimately advanced) as resolvable, re-pinning them to the
	// current live SHA. Unlike AcceptMoved it leaves unreachable-pin
	// findings untouched so possible tampering stays a hard error.
	Relock bool

	// Refresh re-pins only the entries it selects, as Relock would. A
	// workflow that records one rebuilds its closure; its other entries
	// resolve from the lockfile seed, so they keep their commits.
	Refresh *refresh.Set

	// PartialScan reports that explicit workflow paths, rather than the full
	// workflow directory, define this run's mutation authority.
	PartialScan bool

	// prevImpreciseNWO is computed once in Plan() from the global lockfile
	// state. It holds lowercased NWOs that are already recorded with a
	// non-full-semver ref anywhere in the lockfile. Narrowing is skipped
	// for these to respect the user's prior precision choice and avoid
	// creating duplicate dep entries at different ref granularities.
	prevImpreciseNWO map[string]bool

	// retainedLockTargets excludes pins orphaned by this run's stale-ref
	// pruning, while retaining pins needed by unscanned or skipped workflows.
	retainedLockTargets map[string]bool

	// OnProgress is called at each phase boundary with a human-readable
	// label (e.g. "Resolving actions/checkout"). Nil means no progress.
	OnProgress func(phase string)
}

// Plan walks an actionmanager Report and produces a Record describing
// every pin action to take. It does network reads (resolve, reverse
// lookup, reachability) but no disk writes.
func Plan(ctx context.Context, report *checks.Report, opts PlanOptions) (*Record, error) {
	rec := &Record{}

	type indexedWR struct {
		idx int
		wr  checks.WorkflowReport
	}
	items := make([]indexedWR, len(report.Workflows))
	for i, wr := range report.Workflows {
		items[i] = indexedWR{idx: i, wr: wr}
	}

	// Build set of NWOs recorded with a non-semver ref as a *direct* pin.
	// Checked across all workflows (not per-WF) so two workflows referencing
	// the same action settle on the same ref precision — avoiding duplicate
	// dep entries in the lockfile. Transitive pins are excluded: their ref is
	// the composite author's choice, is never narrowed, and would otherwise
	// permanently veto narrowing every direct use of the same NWO.
	if opts.prevImpreciseNWO == nil && opts.Store != nil {
		opts.prevImpreciseNWO = impreciseDirectNWOs(opts.Store)
	}
	opts.retainedLockTargets = retainedLockTargets(report, opts)
	results := make([]planResult, len(report.Workflows))
	var planErr error
	poolErr := pinpool.RunTyped(opts.Pool, ctx, "Planning pins",
		items,
		func(iwr indexedWR) string { return "planning " + iwr.wr.Path },
		func(ctx context.Context, slot int, iwr indexedWR) error {
			status := func(s string) {
				opts.Pool.Reporter.SetWorkerStatus(slot, "→ "+s)
			}
			pr, err := planWorkflow(ctx, iwr.wr, opts, status)
			if err != nil {
				return fmt.Errorf("planning %s: %w", iwr.wr.Path, err)
			}
			for i := range pr.wplans {
				respellRewrites(&pr.wplans[i], iwr.wr.SHASpellings)
			}
			results[iwr.idx] = pr
			return nil
		},
	)
	if poolErr != nil {
		planErr = poolErr
	}

	targets := make(map[string]string)
	for _, pr := range results {
		for _, entry := range pr.entries {
			if planErr != nil || entry.SHA == "" ||
				entry.Resolution != Pinned && entry.Resolution != Verified {
				continue
			}
			key := strings.ToLower(entry.NWO) + "@" + entry.Ref
			target := entry.Hostname + "/" + entry.SHA
			if previous, ok := targets[key]; ok && !strings.EqualFold(previous, target) {
				planErr = fmt.Errorf("conflicting planned target %s resolves to both %s and %s", key, previous, target)
				continue
			}
			targets[key] = target
		}
		rec.Entries = append(rec.Entries, pr.entries...)
		rec.Workflows = append(rec.Workflows, pr.wplans...)
	}

	return rec, planErr
}

type planResult struct {
	entries []Entry
	wplans  []WorkflowPlan
}

func planWorkflow(ctx context.Context, wr checks.WorkflowReport, opts PlanOptions, status func(string)) (planResult, error) {
	var entries []Entry
	var wplans []WorkflowPlan
	if wr.SkipCommit || wr.BlockingResolverError {
		return planResult{entries: verifiedEntries(wr.Inventory, wr.Path)}, nil
	}
	for _, finding := range wr.Findings {
		if finding.Category == checks.InvalidSelfRepositoryRef {
			return planResult{}, nil
		}
	}
	rewriteRefs := wr.RewriteRefs
	if rewriteRefs == nil {
		rewriteRefs = wr.ActionRefs
	}
	// Drop stale inventory entries so a re-pin converges: the orphan leaves
	// workflows[path] and Save's GC removes its dependencies[] entry.
	inventory := pruneStaleInventory(wr.Inventory, wr.Findings, opts.AcceptMoved, opts.relocks)
	// --relock and --accept-moved rebuild the workflow's whole closure from
	// live manifests, even when no ref moved.
	rebuild := repinsMoved(opts, wr)

	if !wr.NeedsAttention() && !rebuild {
		entries = verifiedEntries(inventory, wr.Path)
		rw := narrowVerifiedEntries(ctx, entries, opts, rewriteRefs)
		if err := rejectPartialSelfActionRewrites(opts, wr.SelfActionRefs, rw); err != nil {
			return planResult{}, err
		}
		wplans = append(wplans, WorkflowPlan{Path: wr.Path, Rewrites: rw, SelfActionFiles: wr.SelfActionFiles})
		return planResult{entries: entries, wplans: wplans}, nil
	}

	// --relock must not rewrite a workflow that still carries an
	// unreachable-pin finding: the rebuild pulls the whole tree (including
	// the possibly-tampered unreachable dep, which may be transitive and
	// absent from the direct-only inventory) through ResolveAllRecursive,
	// and buildPinnedEntries would commit it at a live SHA before the
	// hard-error gate runs. Leave the workflow untouched from its original
	// inventory; the unreachable-pin error still fails the run.
	// --accept-moved deliberately accepts these, so it is exempt.
	if relockKeepsUnreachable(opts, wr) {
		entries = verifiedEntries(wr.Inventory, wr.Path)
		wplans = append(wplans, WorkflowPlan{Path: wr.Path, SelfActionFiles: wr.SelfActionFiles})
		return planResult{entries: entries, wplans: wplans}, nil
	}

	// Per-dep trust: recorded deps skip the network path.
	unrecordedRefs, inventorySHA := partitionByInventory(inventory, wr.ActionRefs)
	entries = verifiedEntries(inventory, wr.Path)

	// A rebuild resolves every root recursively, so moved transitive deps
	// are re-pinned and every composite's edges are read from its manifest.
	recorded := make(map[string]dep.Dependency, len(inventory))
	for _, inv := range inventory {
		recorded[checks.PruneKey(inv.Dep.NWO, inv.Dep.Ref, inv.Dep.SHA)] = inv.Dep
	}
	if rebuild {
		unrecordedRefs, inventorySHA = partitionByInventory(nil, wr.ActionRefs)
		entries = verifiedEntries(nil, wr.Path)
	}

	if len(unrecordedRefs) == 0 {
		rw := narrowVerifiedEntries(ctx, entries, opts, rewriteRefs)
		if err := rejectPartialSelfActionRewrites(opts, wr.SelfActionRefs, rw); err != nil {
			return planResult{}, err
		}
		wplans = append(wplans, WorkflowPlan{Path: wr.Path, Rewrites: rw, SelfActionFiles: wr.SelfActionFiles})
		return planResult{entries: entries, wplans: wplans}, nil
	}

	// Resolve live state for unrecorded refs only.
	status("resolving " + wr.Path)
	deps, parentMap, resolveErr := opts.Resolver.ResolveAllRecursive(ctx, unrecordedRefs)
	// `./…` uses inside remote composites are never followed; diagnose
	// reports them.
	_, resolveErr = resolve.SplitLocalPaths(resolveErr)
	if resolveErr != nil {
		// A resolved root is not pinnable when its transitive graph is incomplete.
		for _, ref := range unrecordedRefs {
			entries = append(entries, Entry{
				NWO: ref.NWO(), Ref: ref.Ref, Resolution: Unresolved,
				Reason: "resolution failed: " + resolveErr.Error(), Workflows: []string{wr.Path},
			})
		}
		return planResult{entries: entries}, nil
	}

	// Root refs are recorded directly under the workflow in the lockfile.
	// Workflow refs are the narrower subset eligible for source rewrites.
	// Keep separate trackers because a remote dependency found inside an
	// in-repo `$/…` action is a lockfile root but does not appear in the
	// workflow YAML.
	rootTracker := lockfile.NewDirectTracker(unrecordedRefs, deps)
	rewriteTracker := lockfile.NewDirectTracker(rewriteRefs, deps)

	// Narrow mutable version tags to exact patch tags.
	status("pinning " + wr.Path)
	rewrites := make(map[string]string)
	preservedDeps := make(map[int]bool)
	// A use a composite also declares keeps its ref: the composite's edge
	// and entry must name what its manifest names.
	for i, d := range deps {
		if _, declared := parentMap[d.Key()]; declared {
			preservedDeps[i] = true
		}
	}

	narrowDirectDeps(ctx, opts, deps, parentMap, rewriteTracker, rewrites, preservedDeps)
	reRecorded := preserveReRecorded(opts, deps, preservedDeps)
	// A rebuild leaves a root still at its recorded commit as a run that
	// trusts the lock would: its `uses:` and recorded ref stay.
	settled := make(map[int]bool)
	for i := range deps {
		d := &deps[i]
		inv, ok := recorded[checks.PruneKey(d.NWO, d.Ref, d.SHA)]
		if !rebuild || !ok || !rewriteTracker.IsDirect(i) {
			continue
		}
		preservedDeps[i] = true
		if !reRecorded[d.Key()] {
			d.Tag, d.Branch = inv.Tag, inv.Branch
			settled[i] = true
		}
	}

	// ReverseLookup canonicalizes each dep's ref while preserving the tags
	// narrowing chose and transitive deps' declared refs.
	rlRewrites, lookupIssues, err := reverseLookupRewrites(ctx, opts, wr, deps, rewriteTracker, preservedDeps, settled)
	if err != nil {
		return planResult{}, err
	}
	// Deps that ReverseLookup couldn't resolve (orphaned commits, bare SHAs
	// with no containing ref) become Unresolved entries rather than aborting.
	if len(lookupIssues) > 0 {
		skip := make(map[int]bool, len(lookupIssues))
		for _, issue := range lookupIssues {
			entries = append(entries, Entry{
				NWO:        issue.NWO,
				Ref:        issue.Ref,
				SHA:        issue.SHA,
				Resolution: Unresolved,
				Issue:      "reverse-lookup",
				Reason:     issue.Message,
				Workflows:  []string{wr.Path},
				RequiredBy: parentMap[issue.NWO+"@"+issue.Ref],
			})
			skip[issue.Index] = true
		}
		// Remove failed deps so they don't flow into pinning/commit.
		filtered := deps[:0]
		for i, d := range deps {
			if !skip[i] {
				filtered = append(filtered, d)
			}
		}
		deps = filtered
		// Rebuild the root tracker against the filtered slice.
		rootTracker = lockfile.NewDirectTracker(unrecordedRefs, deps)
	}
	for k, v := range rlRewrites {
		rewrites[k] = v
	}

	// Update parent map keys to reflect narrowed/normalized refs.
	parentRewrites := make(map[string]string)
	for i := range deps {
		dep := &deps[i]
		newKey := dep.Key()
		// Compare against original key before narrowing.
		for oldUses, newUses := range rewrites {
			if newUses == dep.NWO+"@"+dep.Ref {
				// The dep was rewritten from oldUses.
				parentRewrites[oldUses] = newKey
			}
		}
		_ = dep
	}
	if len(parentRewrites) > 0 {
		parentMap = dep.RekeyParentMap(parentMap, parentRewrites)
	}

	// Record workflow plan if there are rewrites.
	// Also narrow any verified (already-recorded) entries that have imprecise refs.
	if verifiedRW := narrowVerifiedEntries(ctx, entries, opts, rewriteRefs); len(verifiedRW) > 0 {
		for k, v := range verifiedRW {
			rewrites[k] = v
		}
	}
	required := transferRewrites(deps, rootTracker, wr.RewriteRefs, rewrites)
	if err := rejectPartialSelfActionRewrites(opts, wr.SelfActionRefs, rewrites); err != nil {
		return planResult{}, err
	}
	if len(rewrites) > 0 {
		wplans = append(wplans, WorkflowPlan{
			Path:             wr.Path,
			Rewrites:         rewrites,
			RequiredRewrites: required,
			SelfActionFiles:  wr.SelfActionFiles,
		})
	} else if len(wplans) == 0 {
		// Keep the workflow in the plan so its lockfile entry is updated.
		wplans = append(wplans, WorkflowPlan{Path: wr.Path, SelfActionFiles: wr.SelfActionFiles})
	}

	// Build entries for all pinned deps (skip any already emitted from inventory).
	for _, e := range buildPinnedEntries(opts, wr, deps, parentMap, rootTracker, inventorySHA) {
		e.ReRecorded = reRecorded[e.NWO+"@"+e.Ref]
		entries = append(entries, e)
	}

	// Record findings that are informational (misleading-sha).
	entries = append(entries, informationalEntries(wr)...)

	return planResult{entries: entries, wplans: wplans}, nil
}

// respellRewrites keys planned rewrites of a full-SHA `uses:` by the
// spelling the source has, since planning keys them by the lowercase one.
func respellRewrites(wp *WorkflowPlan, spellings map[string]string) {
	for source, canon := range spellings {
		if target, ok := workflowfile.LookupRewrite(canon, wp.Rewrites); ok {
			wp.Rewrites[source] = target
		}
		if required, ok := wp.RequiredRewrites[canon]; ok {
			wp.RequiredRewrites[source] = required
			delete(wp.RequiredRewrites, canon)
		}
	}
}

// transferRewrites moves each redirected root's `uses:` in this workflow
// to the canonical repository.
func transferRewrites(deps []dep.Dependency, rootTracker lockfile.DirectTracker, sources []parserlock.ActionRef, rewrites map[string]string) map[string]string {
	written := make(map[string]bool, len(sources))
	for _, ref := range sources {
		written[ref.FullName()+"@"+ref.Ref] = true
	}
	var required map[string]string
	for i, d := range deps {
		if !rootTracker.IsDirect(i) {
			continue
		}
		for _, ref := range d.OriginalRefs {
			oldUse := ref.FullName() + "@" + ref.Ref
			if !written[oldUse] {
				continue
			}
			newUse := d.NWO
			if ref.Path != "" {
				newUse += "/" + ref.Path
			}
			if required == nil {
				required = map[string]string{}
			}
			rewrites[oldUse] = newUse + "@" + d.Ref
			required[oldUse] = rewrites[oldUse]
		}
	}
	return required
}

func rejectPartialSelfActionRewrites(opts PlanOptions, selfActionRefs []parserlock.ActionRef, rewrites map[string]string) error {
	if !opts.PartialScan || len(rewrites) == 0 {
		return nil
	}
	selfKeys := actionRefKeys(selfActionRefs)
	for oldUses := range rewrites {
		ref := parserlock.ParseActionRef(oldUses)
		if ref == nil {
			continue
		}
		key := strings.ToLower(ref.Owner+"/"+ref.Repo) + "@" + ref.Ref
		if selfKeys[key] {
			return fmt.Errorf("cannot rewrite %s in a shared local action during a partial workflow scan; scan all workflows", key)
		}
	}
	return nil
}

// preserveReRecorded keeps the `uses:` of each full-SHA pin whose entry
// recorded the SHA itself as its ref: the run only re-records the entry's
// tag or branch. It returns the keys of those deps.
func preserveReRecorded(opts PlanOptions, deps []dep.Dependency, preservedDeps map[int]bool) map[string]bool {
	out := map[string]bool{}
	if opts.Store == nil {
		return out
	}
	for i, d := range deps {
		if !parserlock.IsFullSha(d.Ref) {
			continue
		}
		// A redirect names the repository by its new name; the entry may
		// still be recorded under the name the workflow uses.
		owner, repo := d.OwnerRepo()
		recorded := opts.Store.RecordsOwnSHA(owner, repo, d.Ref)
		for _, orig := range d.OriginalRefs {
			recorded = recorded || opts.Store.RecordsOwnSHA(orig.Owner, orig.Repo, d.Ref)
		}
		if recorded {
			preservedDeps[i] = true
			out[d.Key()] = true
		}
	}
	return out
}

// narrowDirectDeps rewrites direct partial semver refs to exact patch tags,
// leaving bare SHA and transitive refs for reverse lookup.
func narrowDirectDeps(ctx context.Context, opts PlanOptions, deps []dep.Dependency, parentMap dep.ParentMap, directTracker lockfile.DirectTracker, rewrites map[string]string, preservedDeps map[int]bool) {
	if opts.Tagger == nil {
		return
	}
	for i := range deps {
		dep := &deps[i]
		// Transitive deps come from a composite's action.yml; their ref
		// is the composite author's choice and never appears in our
		// workflow YAML. Narrowing it is pure churn and can invent refs
		// the composite never declared, so leave it verbatim.
		if !directTracker.IsDirect(i) || preservedDeps[i] {
			continue
		}
		owner, repo := dep.OwnerRepo()
		if owner == "" {
			continue
		}

		// ReverseLookup owns bare-SHA normalization unless --no-narrow protects it.
		if parserlock.IsFullSha(dep.Ref) {
			if opts.NoNarrow {
				preservedDeps[i] = true
			}
			continue
		}

		// Narrow to a full semver patch tag when the ref is already
		// version-shaped but imprecise (v4, v3.1). Non-version refs like
		// `main`, `canary`, or `releases/v4` are intentional — narrowing
		// them risks picking up unrelated semver tags from the repo.
		// Skip if --no-narrow or if the lockfile already recorded this
		// dep without a full semver ref (respect prior precision choice).
		nwoLower := strings.ToLower(dep.NWO)
		if opts.NoNarrow || opts.prevImpreciseNWO[nwoLower] {
			continue
		}
		sv, ok := parserlock.ParseSemVer(dep.Ref)
		if !ok {
			continue
		}
		if sv.IsFull() {
			continue
		}

		patchTag, err := opts.Tagger.BestPatchTagForSHA(ctx, owner, repo, dep.SHA, dep.Ref)
		if err != nil || patchTag == "" {
			continue
		}
		oldUses := dep.NWO + "@" + dep.Ref
		newUses := dep.NWO + "@" + patchTag
		// Narrowing onto another entry's key merges the two; skip it when
		// the merged entry would reach itself through other entries.
		if mergeMakesCycle(parentMap, oldUses, newUses) {
			continue
		}
		rewrites[oldUses] = newUses
		dep.Ref = patchTag
		preservedDeps[i] = true
	}
}

// mergeMakesCycle reports whether merging dep keys a and b into one entry
// would close a `uses:` cycle through other entries: one reaches the other
// by a path of two or more edges. A direct edge between them becomes a
// self-edge, which the lockfile drops.
func mergeMakesCycle(parentMap dep.ParentMap, a, b string) bool {
	norm := func(key string) string {
		nwo, ref, _ := strings.Cut(key, "@")
		return strings.ToLower(nwo) + "@" + lockfile.CanonicalRef(ref)
	}
	children := make(map[string][]string, len(parentMap))
	for child, parents := range parentMap {
		for _, p := range parents {
			children[norm(p)] = append(children[norm(p)], norm(child))
		}
	}
	a, b = norm(a), norm(b)
	if a == b {
		return false
	}
	reachesByDetour := func(from, to string) bool {
		seen := map[string]bool{from: true}
		var queue []string
		for _, c := range children[from] {
			if c != to && !seen[c] {
				seen[c] = true
				queue = append(queue, c)
			}
		}
		for len(queue) > 0 {
			n := queue[0]
			queue = queue[1:]
			for _, c := range children[n] {
				if c == to {
					return true
				}
				if !seen[c] {
					seen[c] = true
					queue = append(queue, c)
				}
			}
		}
		return false
	}
	return reachesByDetour(a, b) || reachesByDetour(b, a)
}

// reverseLookupRewrites canonicalizes refs via ReverseLookup, preserving
// narrowed refs, transitive declarations, and existing locked targets.
// It returns rewrites, indices of unresolvable deps, and any hard error.
func reverseLookupRewrites(ctx context.Context, opts PlanOptions, wr checks.WorkflowReport, deps []dep.Dependency, directTracker lockfile.DirectTracker, preservedDeps, settled map[int]bool) (map[string]string, []resolve.LookupIssue, error) {
	originalRefs := make([]string, len(deps))
	for i := range deps {
		originalRefs[i] = deps[i].Ref
	}

	// Settled deps keep their recorded ref; look up the rest.
	var lookup []dep.Dependency
	var index []int
	for i := range deps {
		if !settled[i] {
			lookup = append(lookup, deps[i])
			index = append(index, i)
		}
	}
	normRewrites, lookupIssues, err := opts.Resolver.ReverseLookup(ctx, lookup)
	if err != nil {
		return nil, nil, fmt.Errorf("reverse lookup: %w", err)
	}
	for j, i := range index {
		deps[i] = lookup[j]
	}
	for k := range lookupIssues {
		lookupIssues[k].Index = index[lookupIssues[k].Index]
	}
	// Composite refs must match their action.yml. A direct rewrite must not
	// reuse a ref locked to a different commit. Keep the discovered metadata.
	for i := range deps {
		if preservedDeps[i] || !directTracker.IsDirect(i) ||
			hasConflictingLockTarget(opts, deps[i].NWO, deps[i].Ref, deps[i].SHA) {
			deps[i].Ref = originalRefs[i]
			delete(normRewrites, deps[i].Key())
		}
	}
	return normRewrites, lookupIssues, nil
}

// buildPinnedEntries emits an entry for every resolved dep, marking it Verified
// when the lockfile already records the same SHA and Pinned otherwise. Deps
// already emitted from inventory under the same key and commit are skipped.
func buildPinnedEntries(opts PlanOptions, wr checks.WorkflowReport, deps []dep.Dependency, parentMap dep.ParentMap, directTracker lockfile.DirectTracker, inventorySHA map[string]bool) []Entry {
	// Load existing lockfile state so re-runs are noops for unchanged deps.
	existingSHA := make(map[string]string) // NWO@Ref -> SHA
	if opts.Store != nil {
		wfKey := workflowfile.KeyFromPath(wr.Path)
		if existing, err := opts.Store.Get(wfKey); err == nil {
			for _, d := range existing {
				existingSHA[d.Key()] = d.SHA
			}
		}
	}

	directKeys := directTracker.Keys(deps)
	// A parent with an edge to no dep here (a composite's use under a name
	// the run resolved to a new one) wasn't read whole: it keeps its
	// recorded edges.
	depKeys := make(map[string]bool, len(deps))
	for _, d := range deps {
		depKeys[d.Key()] = true
	}
	partial := make(map[string]bool)
	for child, parents := range parentMap {
		if !depKeys[child] {
			for _, p := range parents {
				partial[p] = true
			}
		}
	}
	var out []Entry
	for _, dep := range deps {
		// Each key needs its own entry: a ref that moved onto a commit a
		// sibling ref is locked at must still be written.
		if inventorySHA[checks.PruneKey(dep.NWO, dep.Ref, dep.SHA)] {
			continue // already emitted as Verified from inventory
		}
		depKey := dep.Key()
		parents := parentMap[depKey]
		res := Pinned
		if sha, ok := existingSHA[depKey]; ok && sha == dep.SHA {
			res = Verified
		}
		entry := Entry{
			Hostname:   dep.Hostname,
			NWO:        dep.NWO,
			Ref:        dep.Ref,
			SHA:        dep.SHA,
			Resolution: res,
			OnBranch:   dep.Branch,
			Tag:        dep.Tag,
			Workflows:  []string{wr.Path},
			RequiredBy: parents,
			Direct:     directKeys[depKey],
			FromLock:   dep.FromLock || partial[depKey],
		}
		for _, orig := range dep.OriginalRefs {
			entry.RenamedFrom = append(entry.RenamedFrom, orig.NWO()+"@"+orig.Ref)
		}
		out = append(out, entry)
	}
	return out
}

// informationalEntries records misleading-sha findings as Investigate
// entries. A ref-moved floating ref is sticky: its locked commit is still in
// the ref's history, so the run keeps it with a warning unless --relock or
// --accept-moved re-pins it.
func informationalEntries(wr checks.WorkflowReport) []Entry {
	var out []Entry
	for _, f := range wr.Findings {
		if f.Category == checks.MisleadingSHA {
			out = append(out, informationalEntry(f, wr.Path))
		}
	}
	return out
}

func informationalEntry(f checks.Finding, path string) Entry {
	nwo := ""
	ref := ""
	hostname := ""
	if f.ActionRef != nil {
		nwo = f.ActionRef.Owner + "/" + f.ActionRef.Repo
		ref = f.ActionRef.Ref
	}
	if f.Dependency != nil {
		hostname = f.Dependency.Hostname
	}
	return Entry{
		Hostname:    hostname,
		NWO:         nwo,
		Ref:         ref,
		ObservedSHA: f.ObservedSHA,
		Resolution:  Investigate,
		Issue:       string(f.Category),
		Reason:      f.Detail,
		Workflows:   []string{path},
	}
}

// partitionByInventory splits refs into those with a matching inventory
// entry (recorded) and those without (unrecorded). Returns the unrecorded
// refs and an NWO:SHA index for deduplicating resolved deps later.
func partitionByInventory(inventory []checks.InventoryEntry, refs []parserlock.ActionRef) (unrecorded []parserlock.ActionRef, shaSeen map[string]bool) {
	byKey := make(map[string]bool, len(inventory))
	shaSeen = make(map[string]bool, len(inventory))
	for _, inv := range inventory {
		nwo := strings.ToLower(inv.Dep.NWO)
		byKey[nwo+"@"+inv.Dep.Ref] = true
		shaSeen[checks.PruneKey(inv.Dep.NWO, inv.Dep.Ref, inv.Dep.SHA)] = true
	}
	for _, ref := range refs {
		key := strings.ToLower(ref.Owner+"/"+ref.Repo) + "@" + ref.Ref
		if !byKey[key] {
			unrecorded = append(unrecorded, ref)
		}
	}
	return unrecorded, shaSeen
}

// pruneStaleInventory drops inventory entries matching a pruning finding
// (see checks.PrunesDependency), so a fix-mode re-pin converges.
func pruneStaleInventory(inventory []checks.InventoryEntry, findings []checks.Finding, acceptMoved bool, relocks func(*dep.Dependency) bool) []checks.InventoryEntry {
	stale := make(map[string]bool)
	for _, f := range findings {
		if !checks.PrunesDependency(f, acceptMoved, f.Dependency != nil && relocks(f.Dependency)) {
			continue
		}
		d := f.Dependency
		stale[checks.PruneKey(d.NWO, d.Ref, d.SHA)] = true
	}
	if len(stale) == 0 {
		return inventory
	}
	out := make([]checks.InventoryEntry, 0, len(inventory))
	for _, inv := range inventory {
		if stale[checks.PruneKey(inv.Dep.NWO, inv.Dep.Ref, inv.Dep.SHA)] {
			continue
		}
		out = append(out, inv)
	}
	return out
}

// relocks reports whether the run re-pins d's entry.
func (opts PlanOptions) relocks(d *dep.Dependency) bool {
	return opts.Relock || opts.Refresh.Has(d.NWO, d.Ref)
}

// relocksWorkflow reports whether the run rebuilds wr's closure to re-pin
// its entries.
func (opts PlanOptions) relocksWorkflow(wr checks.WorkflowReport) bool {
	return opts.Relock || opts.Refresh.Workflow(workflowfile.KeyFromPath(wr.Path))
}

// repinsMoved reports whether this run re-resolves wr's benign ref-moved deps.
func repinsMoved(opts PlanOptions, wr checks.WorkflowReport) bool {
	return opts.relocksWorkflow(wr) || opts.AcceptMoved
}

// relockKeepsUnreachable reports whether a --relock leaves wr as recorded
// because it carries an unreachable pin.
func relockKeepsUnreachable(opts PlanOptions, wr checks.WorkflowReport) bool {
	return opts.relocksWorkflow(wr) && !opts.AcceptMoved && wr.CountByCategory(checks.UnreachablePin) > 0
}

// verifiedEntries builds Verified plan entries for every inventory item.
func verifiedEntries(inventory []checks.InventoryEntry, path string) []Entry {
	out := make([]Entry, len(inventory))
	for i, inv := range inventory {
		out[i] = Entry{
			NWO:        inv.Dep.NWO,
			Ref:        inv.Dep.Ref,
			SHA:        inv.Dep.SHA,
			Hostname:   inv.Dep.Hostname,
			Resolution: Verified,
			OnBranch:   inv.Dep.Branch,
			Tag:        inv.Dep.Tag,
			Workflows:  []string{path},
			Direct:     inv.Direct,
			RequiredBy: inv.Parents,
			FromLock:   true,
		}
	}
	return out
}

// narrowVerifiedEntries upgrades already-recorded direct deps to full semver
// tags when possible, returning the workflow-YAML rewrites. Skipped for
// --no-narrow, transitive deps, and refs the user kept imprecise (sticky v4).
func narrowVerifiedEntries(ctx context.Context, entries []Entry, opts PlanOptions, rewriteRefs []parserlock.ActionRef) map[string]string {
	if opts.NoNarrow || opts.Tagger == nil {
		return nil
	}
	rewriteRefKeys := actionRefKeys(rewriteRefs)
	rewrites := make(map[string]string)
	for i := range entries {
		e := &entries[i]
		if !e.Direct {
			continue
		}
		if !rewriteRefKeys[strings.ToLower(e.NWO)+"@"+e.Ref] {
			continue
		}
		if parserlock.IsFullSha(e.Ref) {
			// Rewriting `uses:` names the commit by its recorded ref, which is
			// lock data, not proof (#174): the ref must resolve, as the
			// backend resolves it (tag first), to exactly this commit now.
			// A moved tag or a containing branch would name another commit.
			newRef := cmp.Or(e.Tag, e.OnBranch)
			if newRef == "" || opts.Resolver == nil || hasConflictingLockTarget(opts, e.NWO, newRef, e.SHA) {
				continue
			}
			owner, repo := splitNWO(e.NWO)
			if ok, err := opts.Resolver.RefNamesCommit(ctx, owner, repo, newRef, e.SHA); err != nil || !ok {
				continue
			}
			oldRef := e.Ref
			for _, ref := range rewriteRefs {
				if strings.EqualFold(ref.Owner+"/"+ref.Repo, e.NWO) && ref.Ref == oldRef {
					rewrites[ref.FullName()+"@"+oldRef] = ref.FullName() + "@" + newRef
				}
			}
			e.Ref = newRef
			e.AutoFixedRef = oldRef
			continue
		}
		owner, repo := splitNWO(e.NWO)
		if owner == "" {
			continue
		}
		// Respect a prior imprecise precision choice, mirroring the
		// slow-path guard in narrowDirectDeps: a verified v4 entry the
		// user kept as v4 must not be narrowed on a no-op re-pin.
		if opts.prevImpreciseNWO[strings.ToLower(e.NWO)] {
			continue
		}
		// Only narrow refs that are already version-shaped but imprecise
		// (e.g. v4, v4.2). Non-version refs like `main`, `canary`, or
		// `releases/v4` are intentional choices — narrowing them could pick
		// up unrelated semver tags from the repo (e.g. framework releases
		// in a monorepo like vercel/next.js).
		sv, ok := parserlock.ParseSemVer(e.Ref)
		if !ok {
			continue
		}
		if sv.IsFull() {
			continue
		}
		patchTag, err := opts.Tagger.BestPatchTagForSHA(ctx, owner, repo, e.SHA, e.Ref)
		if err != nil || patchTag == "" {
			continue
		}
		oldRef := e.Ref
		oldUses := e.NWO + "@" + oldRef
		newUses := e.NWO + "@" + patchTag
		rewrites[oldUses] = newUses
		e.Ref = patchTag
		e.AutoFixedRef = oldRef
	}
	if len(rewrites) == 0 {
		return nil
	}
	return rewrites
}

func retainedLockTargets(report *checks.Report, opts PlanOptions) map[string]bool {
	if opts.Store == nil {
		return nil
	}
	staleByWorkflow := staleRoots(report, opts)

	// A stale direct pin can still be required by another workflow or by a
	// surviving composite. Only ignore targets absent from every closure.
	retained := make(map[string]bool)
	for _, wfKey := range opts.Store.WorkflowKeys() {
		live := func(p parserlock.Pin, _ parserlock.Action) bool {
			return !staleByWorkflow[wfKey][p.String()]
		}
		for _, locked := range opts.Store.Closure(wfKey, live) {
			retained[locked.Pin.String()] = true
		}
	}
	return retained
}

// staleRoots returns, per workflow key, the pin keys of the lock roots this
// run prunes (stale findings, see lockfile.UsedRoot), for workflows planning
// may write.
func staleRoots(report *checks.Report, opts PlanOptions) map[string]map[string]bool {
	staleByWorkflow := make(map[string]map[string]bool)
	for _, wr := range report.Workflows {
		if wr.SkipCommit || wr.BlockingResolverError || wr.CountByCategory(checks.InvalidSelfRepositoryRef) > 0 {
			continue
		}
		if relockKeepsUnreachable(opts, wr) {
			continue
		}
		stale := make(map[string]bool)
		for _, f := range wr.Findings {
			if f.Category == checks.Stale && f.Dependency != nil {
				stale[strings.ToLower(f.Dependency.NWO)+"@"+f.Dependency.Ref] = true
			}
		}
		staleByWorkflow[workflowfile.KeyFromPath(wr.Path)] = stale
	}
	return staleByWorkflow
}

func hasConflictingLockTarget(opts PlanOptions, nwo, ref, sha string) bool {
	if opts.Store == nil {
		return false
	}
	key := strings.ToLower(nwo) + "@" + ref
	if opts.retainedLockTargets != nil && !opts.retainedLockTargets[key] {
		return false
	}
	action, ok := opts.Store.File().Dependencies[key]
	if !ok {
		return false
	}
	_, targetSHA, ok := strings.Cut(action.Commit, "-")
	return ok && !strings.EqualFold(targetSHA, sha)
}

func actionRefKeys(refs []parserlock.ActionRef) map[string]bool {
	keys := make(map[string]bool, len(refs))
	for _, ref := range refs {
		keys[strings.ToLower(ref.Owner+"/"+ref.Repo)+"@"+ref.Ref] = true
	}
	return keys
}

// splitNWO splits "owner/repo" or "owner/repo/sub" into (owner, repo).
func splitNWO(nwo string) (string, string) {
	parts := strings.SplitN(nwo, "/", 3)
	if len(parts) < 2 {
		return "", ""
	}
	return parts[0], parts[1]
}

// impreciseDirectNWOs returns lowercased NWOs recorded with a non-full-semver
// ref as a direct pin of some workflow.
func impreciseDirectNWOs(store *lockfile.State) map[string]bool {
	out := make(map[string]bool)
	for _, wfKey := range store.WorkflowKeys() {
		deps, err := store.Get(wfKey)
		if err != nil {
			continue
		}
		for _, d := range deps {
			if sv, ok := parserlock.ParseSemVer(d.Ref); ok && !sv.IsFull() {
				out[strings.ToLower(d.NWO)] = true
			}
		}
	}
	return out
}
