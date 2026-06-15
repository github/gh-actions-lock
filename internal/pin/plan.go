package pin

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	parserlock "github.com/github/actions-lockfile/go/pkg/lockfile"
	"github.com/github/gh-actions-lock/internal/dep"
	"github.com/github/gh-actions-lock/internal/lockfile"
	"github.com/github/gh-actions-lock/internal/pinpool"
	"github.com/github/gh-actions-lock/internal/pipeline/checks"
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
	Version   string // CLI version for the record
	// NoNarrow disables tag narrowing: mutable version refs (v4, v3.1)
	// are kept as the lock comment instead of being resolved to full
	// patch tags (v4.2.1). Bare-SHA reverse lookup still applies.
	NoNarrow bool

	// prevImpreciseNWO is computed once in Plan() from the global lockfile
	// state. It holds lowercased NWOs that are already recorded with a
	// non-full-semver ref anywhere in the lockfile. Narrowing is skipped
	// for these to respect the user's prior precision choice and avoid
	// creating duplicate dep entries at different ref granularities.
	prevImpreciseNWO map[string]bool

	// OnProgress is called at each phase boundary with a human-readable
	// label (e.g. "Resolving actions/checkout"). Nil means no progress.
	OnProgress func(phase string)
}

// Plan walks an actionmanager Report and produces a Record describing
// every pin action to take. It does network reads (resolve, reverse
// lookup, reachability) but no disk writes.
func Plan(ctx context.Context, report *checks.Report, opts PlanOptions) (*Record, error) {
	rec := &Record{
		Version: opts.Version,
		Created: time.Now(),
	}

	type indexedWR struct {
		idx int
		wr  checks.WorkflowReport
	}
	items := make([]indexedWR, len(report.Workflows))
	for i, wr := range report.Workflows {
		items[i] = indexedWR{idx: i, wr: wr}
	}

	// Build set of NWOs globally recorded with a non-semver ref. Checked
	// across all workflows (not per-WF) so two workflows referencing the
	// same action settle on the same ref precision — avoiding duplicate
	// dep entries in the lockfile.
	if opts.prevImpreciseNWO == nil && opts.Store != nil {
		opts.prevImpreciseNWO = make(map[string]bool)
		for _, d := range opts.Store.AllDeps() {
			sv, ok := parserlock.ParseSemVer(d.Ref)
			if ok && !sv.IsFull() {
				opts.prevImpreciseNWO[strings.ToLower(d.NWO)] = true
			}
		}
	}

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
			results[iwr.idx] = pr
			return nil
		},
	)
	if poolErr != nil {
		planErr = poolErr
	}

	for _, pr := range results {
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

	// Drop stale inventory entries so a re-pin converges: the orphan leaves
	// workflows[path] and Save's GC removes its dependencies[] entry.
	inventory := pruneStaleInventory(wr.Inventory, wr.Findings)

	if !wr.NeedsAttention() {
		entries = verifiedEntries(inventory, wr.Path)
		if rw := narrowVerifiedEntries(ctx, entries, opts); len(rw) > 0 {
			wplans = append(wplans, WorkflowPlan{Path: wr.Path, Rewrites: rw})
		}
		return planResult{entries: entries, wplans: wplans}, nil
	}

	// Per-dep trust: recorded deps skip the network path.
	unrecordedRefs, inventorySHA := partitionByInventory(inventory, wr.ActionRefs)
	entries = verifiedEntries(inventory, wr.Path)

	if len(unrecordedRefs) == 0 {
		if rw := narrowVerifiedEntries(ctx, entries, opts); len(rw) > 0 {
			wplans = append(wplans, WorkflowPlan{Path: wr.Path, Rewrites: rw})
		}
		return planResult{entries: entries, wplans: wplans}, nil
	}

	// Resolve live state for unrecorded refs only.
	status("resolving " + wr.Path)
	deps, parentMap, resolveErr := opts.Resolver.ResolveAllRecursive(ctx, unrecordedRefs)
	if resolveErr != nil {
		// Partial failure: some refs resolved (in deps), others didn't.
		// Build a set of resolved NWO@Ref keys so we can continue with
		// the successful ones and only mark the failures as unresolved.
		resolved := make(map[string]bool, len(deps))
		for _, d := range deps {
			resolved[strings.ToLower(d.NWO+"@"+d.Ref)] = true
		}
		// Only mark findings as unresolved if they were actually attempted
		// (i.e., part of unrecordedRefs). Recorded refs were never sent to
		// ResolveAllRecursive and should not be marked as failures.
		attempted := make(map[string]bool, len(unrecordedRefs))
		for _, ref := range unrecordedRefs {
			attempted[strings.ToLower(ref.Owner+"/"+ref.Repo+"@"+ref.Ref)] = true
		}
		for _, f := range wr.Findings {
			if f.ActionRef == nil {
				continue
			}
			key := strings.ToLower(f.ActionRef.Owner + "/" + f.ActionRef.Repo + "@" + f.ActionRef.Ref)
			if !attempted[key] || resolved[key] {
				continue
			}
			entries = append(entries, Entry{
				NWO:        f.ActionRef.Owner + "/" + f.ActionRef.Repo,
				Ref:        f.ActionRef.Ref,
				Resolution: Unresolved,
				Issue:      string(f.Category),
				Reason:     fmt.Sprintf("resolution failed: %s", resolveErr),
				Workflows:  []string{wr.Path},
			})
		}
		if len(deps) == 0 {
			return planResult{entries: entries, wplans: wplans}, nil
		}
		// Fall through with partial deps to pin what we can.
	}

	// Reachability gate — drop impostors, auto-fix when a sane release exists.
	status("verifying " + wr.Path)
	reachResults := opts.Resolver.CheckReachabilityAll(ctx, deps)
	badKeys := make(map[string]bool)
	autoFixed := make(map[string]string)       // new dep key → original ref
	autoFixRewrites := make(map[string]string) // old uses → new uses (for YAML rewrite)
	for _, rr := range reachResults {
		depKey := rr.Owner + "/" + rr.Repo + "@" + rr.Ref
		switch rr.Status {
		case resolve.Unreachable:
			// Look for a recommended release to auto-repin.
			var recTag, recSHA string
			if f := findFinding(wr.Findings, rr.Owner+"/"+rr.Repo, rr.Ref); f != nil && f.RecommendedTag != "" {
				recTag, recSHA = f.RecommendedTag, f.RecommendedSHA
			} else if opts.Tagger != nil {
				recTag, recSHA = checks.FindRecommendedRelease(ctx, opts.Tagger, opts.Resolver, opts.Pool, rr.Owner, rr.Repo)
			}

			if recTag != "" {
				// Rewrite the dep in place to the recommended release so it
				// stays in the pinning pipeline instead of being dropped.
				nwo := rr.Owner + "/" + rr.Repo
				newKey := nwo + "@" + recTag
				autoFixed[newKey] = rr.Ref
				autoFixRewrites[nwo+"@"+rr.Ref] = nwo + "@" + recTag
				for i := range deps {
					if deps[i].Key() == depKey {
						deps[i].Ref = recTag
						if recSHA != "" {
							deps[i].SHA = recSHA
						}
						break
					}
				}
				continue // don't mark as bad — dep stays in pipeline
			}

			entries = append(entries, Entry{
				NWO:        rr.Owner + "/" + rr.Repo,
				Ref:        rr.Ref,
				SHA:        rr.SHA,
				Resolution: Investigate,
				Issue:      string(checks.ImpostorCommit),
				Reason:     rr.Detail,
				Workflows:  []string{wr.Path},
			})
			badKeys[depKey] = true
		case resolve.ReachabilityUnknown:
			entries = append(entries, Entry{
				NWO:        rr.Owner + "/" + rr.Repo,
				Ref:        rr.Ref,
				SHA:        rr.SHA,
				Resolution: Skipped,
				Issue:      "reachability_unknown",
				Reason:     rr.Detail,
				Workflows:  []string{wr.Path},
			})
			badKeys[depKey] = true
		}
	}

	if len(badKeys) > 0 {
		deps, parentMap = dropDeps(deps, parentMap, badKeys)
		if len(deps) == 0 {
			return planResult{entries: entries, wplans: wplans}, nil
		}
	}

	// Track reachability metadata for pinned entries.
	fullScanDeps := make(map[string]bool)
	for _, rr := range reachResults {
		depKey := rr.Owner + "/" + rr.Repo + "@" + rr.Ref
		if badKeys[depKey] {
			continue
		}
		if rr.FullScanUsed {
			fullScanDeps[depKey] = true
		}
	}

	// Snapshot direct-dep matching before narrowing/ReverseLookup mutate
	// dep.Ref — the tracker records index-aligned booleans at construction,
	// then Keys() reads post-mutation refs. Must be built while deps and
	// ActionRefs still share the same ref strings.
	directTracker := lockfile.NewDirectTracker(unrecordedRefs, deps)

	// Narrow mutable version tags to patch tags, and resolve bare-SHA refs
	// to a symbolic tag when one exists.
	status("pinning " + wr.Path)
	rewrites := make(map[string]string)
	narrowedNWOs := make(map[string]bool) // NWOs where narrowing chose a tag
	for k, v := range autoFixRewrites {
		rewrites[k] = v
	}

	if opts.Tagger != nil {
		for i := range deps {
			dep := &deps[i]
			owner, repo := dep.OwnerRepo()
			if owner == "" {
				continue
			}

			// Skip narrowing for same-owner internal repos.
			isInternal := false
			if opts.RepoOwner != "" && owner == opts.RepoOwner {
				info, err := opts.Tagger.GetRepoInfo(ctx, owner, repo)
				if err == nil && info.IsInternal() {
					isInternal = true
				}
			}
			if isInternal {
				continue
			}

			// Bare-SHA refs: find a tag pointing at the same commit.
			if parserlock.IsFullSha(dep.Ref) {
				patchTag, err := opts.Tagger.BestPatchTagForSHA(ctx, owner, repo, dep.SHA)
				if err != nil {
					continue
				}
				if patchTag == "" {
					patchTag, err = opts.Tagger.BestAncestorTag(ctx, owner, repo, dep.SHA)
					if err != nil || patchTag == "" {
						continue
					}
				}
				oldUses := dep.NWO + "@" + dep.Ref
				newUses := dep.NWO + "@" + patchTag
				rewrites[oldUses] = newUses
				dep.Ref = patchTag
				narrowedNWOs[strings.ToLower(dep.NWO)] = true
				continue
			}

			// Narrow to a full semver patch tag when possible. Covers
			// partial semver (v4, v3.1) and non-semver refs (main, master).
			// Skip if --no-narrow or if the lockfile already recorded this
			// dep without a full semver ref (respect prior precision choice).
			nwoLower := strings.ToLower(dep.NWO)
			if opts.NoNarrow || opts.prevImpreciseNWO[nwoLower] {
				continue
			}
			sv, ok := parserlock.ParseSemVer(dep.Ref)
			if ok && sv.IsFull() {
				continue
			}

			patchTag, err := opts.Tagger.BestPatchTagForSHA(ctx, owner, repo, dep.SHA)
			if err != nil {
				continue
			}
			// No exact tag match — if the repo publishes semver releases,
			// walk back to the latest tag that's an ancestor of this SHA.
			if patchTag == "" {
				patchTag, err = opts.Tagger.BestAncestorTag(ctx, owner, repo, dep.SHA)
				if err != nil || patchTag == "" {
					continue
				}
			}
			oldUses := dep.NWO + "@" + dep.Ref
			newUses := dep.NWO + "@" + patchTag
			rewrites[oldUses] = newUses
			dep.Ref = patchTag
			narrowedNWOs[nwoLower] = true
		}
	}

	// Save narrowed refs before ReverseLookup — it may overwrite dep.Ref
	// with a branch name, but we want to keep the semver tag narrowing chose.
	narrowedRefs := make(map[int]string)
	for i := range deps {
		nwo := strings.ToLower(deps[i].NWO)
		if narrowedNWOs[nwo] {
			narrowedRefs[i] = deps[i].Ref
		}
	}

	// ReverseLookup: SHA → containing tag/branch. Rewrites refs to canonical form.
	normRewrites, err := opts.Resolver.ReverseLookup(ctx, deps)
	if err != nil {
		var imp *resolve.ImpostorError
		if errors.As(err, &imp) {
			entries = append(entries, Entry{
				NWO:        imp.NWO,
				Ref:        imp.Ref,
				Resolution: Investigate,
				Issue:      string(checks.ImpostorCommit),
				Reason:     imp.Error(),
				Workflows:  []string{wr.Path},
			})
			return planResult{entries: entries, wplans: wplans}, nil
		}
		return planResult{}, fmt.Errorf("reverse lookup: %w", err)
	}
	// Restore narrowed refs that ReverseLookup may have overwritten.
	for i, ref := range narrowedRefs {
		deps[i].Ref = ref
	}
	for k, v := range normRewrites {
		if at := strings.Index(k, "@"); at > 0 {
			nwo := strings.ToLower(k[:at])
			if narrowedNWOs[nwo] {
				continue
			}
		}
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
	if verifiedRW := narrowVerifiedEntries(ctx, entries, opts); len(verifiedRW) > 0 {
		for k, v := range verifiedRW {
			rewrites[k] = v
		}
	}
	if len(rewrites) > 0 {
		wplans = append(wplans, WorkflowPlan{
			Path:     wr.Path,
			Rewrites: rewrites,
		})
	}

	// Load existing lockfile state so re-runs are noops for unchanged deps.
	existingSHA := make(map[string]string) // NWO@Ref → SHA
	if opts.Store != nil {
		wfKey := workflowfile.KeyFromPath(wr.Path)
		if existing, err := opts.Store.Get(wfKey); err == nil {
			for _, d := range existing {
				existingSHA[d.Key()] = d.SHA
			}
		}
	}

	// Build entries for all pinned deps (skip any already emitted from inventory).
	directKeys := directTracker.Keys(deps)
	for _, dep := range deps {
		nwoSHA := strings.ToLower(dep.NWO) + ":" + strings.ToLower(dep.SHA)
		if inventorySHA[nwoSHA] {
			continue // already emitted as Verified from inventory
		}
		depKey := dep.Key()
		parents := parentMap[depKey]
		res := Pinned
		if sha, ok := existingSHA[depKey]; ok && sha == dep.SHA {
			res = Verified
		}
		entry := Entry{
			NWO:        dep.NWO,
			Ref:        dep.Ref,
			SHA:        dep.SHA,
			Resolution: res,
			OnBranch:   dep.Branch,
			Tag:        dep.Tag,
			FullScan:   fullScanDeps[dep.NWO+"@"+dep.Ref],
			Workflows:  []string{wr.Path},
			RequiredBy: parents,
			Direct:     directKeys[depKey],
		}
		if orig, ok := autoFixed[depKey]; ok {
			entry.AutoFixedRef = orig
			entry.Direct = true       // auto-fixed deps are always direct uses
			entry.Resolution = Pinned // auto-fix is always a new pin
		}
		entries = append(entries, entry)
	}

	// Record findings that are informational (ref-moved, misleading-sha).
	for _, f := range wr.Findings {
		switch f.Category {
		case checks.MisleadingSHA, checks.RefMoved:
			nwo := ""
			ref := ""
			if f.ActionRef != nil {
				nwo = f.ActionRef.Owner + "/" + f.ActionRef.Repo
				ref = f.ActionRef.Ref
			}
			entries = append(entries, Entry{
				NWO:         nwo,
				Ref:         ref,
				ObservedSHA: f.ObservedSHA,
				Resolution:  Investigate,
				Issue:       string(f.Category),
				Reason:      f.Detail,
				Workflows:   []string{wr.Path},
			})
		}
	}

	return planResult{entries: entries, wplans: wplans}, nil
}

func findFinding(findings []checks.Finding, nwo, ref string) *checks.Finding {
	var best *checks.Finding
	for i := range findings {
		f := &findings[i]
		if f.ActionRef != nil && f.ActionRef.Owner+"/"+f.ActionRef.Repo == nwo && f.ActionRef.Ref == ref {
			if f.RecommendedTag != "" {
				return f
			}
			if best == nil {
				best = f
			}
		}
		if f.Dependency != nil && f.Dependency.NWO == nwo && f.Dependency.Ref == ref {
			if f.RecommendedTag != "" {
				return f
			}
			if best == nil {
				best = f
			}
		}
	}
	return best
}

func dropDeps(deps []dep.Dependency, pm dep.ParentMap, bad map[string]bool) ([]dep.Dependency, dep.ParentMap) {
	var kept []dep.Dependency
	for _, d := range deps {
		if !bad[d.Key()] {
			kept = append(kept, d)
		}
	}
	newPM := make(dep.ParentMap)
	for k, v := range pm {
		if !bad[k] {
			newPM[k] = v
		}
	}
	return kept, newPM
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
		shaSeen[nwo+":"+strings.ToLower(inv.Dep.SHA)] = true
	}
	for _, ref := range refs {
		key := strings.ToLower(ref.Owner+"/"+ref.Repo) + "@" + ref.Ref
		if !byKey[key] {
			unrecorded = append(unrecorded, ref)
		}
	}
	return unrecorded, shaSeen
}

// pruneStaleInventory drops inventory entries matching a stale finding (a pin
// the workflow no longer references), so a fix-mode re-pin converges.
func pruneStaleInventory(inventory []checks.InventoryEntry, findings []checks.Finding) []checks.InventoryEntry {
	stale := make(map[string]bool)
	for _, f := range findings {
		if f.Category != checks.Stale || f.Dependency == nil {
			continue
		}
		d := f.Dependency
		stale[strings.ToLower(d.NWO+"@"+d.Ref+":"+d.SHA)] = true
	}
	if len(stale) == 0 {
		return inventory
	}
	out := make([]checks.InventoryEntry, 0, len(inventory))
	for _, inv := range inventory {
		key := strings.ToLower(inv.Dep.NWO + "@" + inv.Dep.Ref + ":" + inv.Dep.SHA)
		if stale[key] {
			continue
		}
		out = append(out, inv)
	}
	return out
}

// verifiedEntries builds Verified plan entries for every inventory item.
func verifiedEntries(inventory []checks.InventoryEntry, path string) []Entry {
	out := make([]Entry, len(inventory))
	for i, inv := range inventory {
		out[i] = Entry{
			NWO:        inv.Dep.NWO,
			Ref:        inv.Dep.Ref,
			SHA:        inv.Dep.SHA,
			Resolution: Verified,
			Workflows:  []string{path},
			Direct:     inv.Direct,
			RequiredBy: inv.Parents,
		}
	}
	return out
}

// narrowVerifiedEntries upgrades already-recorded deps from imprecise refs
// (main, v4, etc.) to full semver tags when possible. Returns rewrites for
// the workflow YAML. Skipped when --no-narrow is set.
func narrowVerifiedEntries(ctx context.Context, entries []Entry, opts PlanOptions) map[string]string {
	if opts.NoNarrow || opts.Tagger == nil {
		return nil
	}
	rewrites := make(map[string]string)
	for i := range entries {
		e := &entries[i]
		owner, repo := splitNWO(e.NWO)
		if owner == "" {
			continue
		}
		// Skip same-owner internal repos.
		if opts.RepoOwner != "" && owner == opts.RepoOwner {
			info, err := opts.Tagger.GetRepoInfo(ctx, owner, repo)
			if err == nil && info.IsInternal() {
				continue
			}
		}
		// Already full semver — nothing to do.
		sv, ok := parserlock.ParseSemVer(e.Ref)
		if ok && sv.IsFull() {
			continue
		}
		// Try exact tag match, then ancestor fallback.
		patchTag, err := opts.Tagger.BestPatchTagForSHA(ctx, owner, repo, e.SHA)
		if err != nil {
			continue
		}
		if patchTag == "" {
			patchTag, err = opts.Tagger.BestAncestorTag(ctx, owner, repo, e.SHA)
			if err != nil || patchTag == "" {
				continue
			}
		}
		oldUses := e.NWO + "@" + e.Ref
		newUses := e.NWO + "@" + patchTag
		rewrites[oldUses] = newUses
		e.Ref = patchTag
		e.AutoFixedRef = oldUses
	}
	if len(rewrites) == 0 {
		return nil
	}
	return rewrites
}

// splitNWO splits "owner/repo" or "owner/repo/sub" into (owner, repo).
func splitNWO(nwo string) (string, string) {
	parts := strings.SplitN(nwo, "/", 3)
	if len(parts) < 2 {
		return "", ""
	}
	return parts[0], parts[1]
}
