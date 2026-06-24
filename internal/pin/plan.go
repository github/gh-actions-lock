package pin

import (
	"context"
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

	// AcceptMoved treats ref-moved and lockfile-forgery findings as
	// resolvable: affected deps are pruned from the inventory and
	// re-resolved to their current live SHA.
	AcceptMoved bool

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
	inventory := pruneStaleInventory(wr.Inventory, wr.Findings, opts.AcceptMoved)

	if !wr.NeedsAttention() {
		entries = verifiedEntries(inventory, wr.Path)
		rw := narrowVerifiedEntries(ctx, entries, opts)
		wplans = append(wplans, WorkflowPlan{Path: wr.Path, Rewrites: rw})
		return planResult{entries: entries, wplans: wplans}, nil
	}

	// Per-dep trust: recorded deps skip the network path.
	unrecordedRefs, inventorySHA := partitionByInventory(inventory, wr.ActionRefs)
	entries = verifiedEntries(inventory, wr.Path)

	if len(unrecordedRefs) == 0 {
		rw := narrowVerifiedEntries(ctx, entries, opts)
		wplans = append(wplans, WorkflowPlan{Path: wr.Path, Rewrites: rw})
		return planResult{entries: entries, wplans: wplans}, nil
	}

	// Resolve live state for unrecorded refs only.
	status("resolving " + wr.Path)
	deps, parentMap, resolveErr := opts.Resolver.ResolveAllRecursive(ctx, unrecordedRefs)
	if resolveErr != nil {
		entries = append(entries, unresolvedEntries(wr, unrecordedRefs, deps, resolveErr)...)
		if len(deps) == 0 {
			wplans = append(wplans, WorkflowPlan{Path: wr.Path})
			return planResult{entries: entries, wplans: wplans}, nil
		}
		// Fall through with partial deps to pin what we can.
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

	narrowDirectDeps(ctx, opts, deps, directTracker, rewrites, narrowedNWOs)

	// ReverseLookup canonicalizes each dep's ref while preserving the tags
	// narrowing chose and transitive deps' declared refs.
	rlRewrites, lookupIssues, err := reverseLookupRewrites(ctx, opts, wr, deps, directTracker, narrowedNWOs)
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
		// Rebuild direct tracker against the filtered slice.
		directTracker = lockfile.NewDirectTracker(unrecordedRefs, deps)
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
	} else if len(wplans) == 0 {
		// No rewrites and no plan entry yet — still include the workflow
		// so EnsureSentinel can be applied during commit.
		wplans = append(wplans, WorkflowPlan{Path: wr.Path})
	}

	// Build entries for all pinned deps (skip any already emitted from inventory).
	entries = append(entries, buildPinnedEntries(opts, wr, deps, parentMap, directTracker, inventorySHA)...)

	// Record findings that are informational (ref-moved, misleading-sha).
	entries = append(entries, informationalEntries(wr)...)

	return planResult{entries: entries, wplans: wplans}, nil
}

// unresolvedEntries flags findings whose refs were attempted but failed to
// resolve. On a partial failure deps holds the refs that did resolve, so only
// the genuine misses (attempted and not in deps) are marked Unresolved.
func unresolvedEntries(wr checks.WorkflowReport, unrecordedRefs []parserlock.ActionRef, deps []dep.Dependency, resolveErr error) []Entry {
	resolved := make(map[string]bool, len(deps))
	for _, d := range deps {
		resolved[strings.ToLower(d.NWO+"@"+d.Ref)] = true
	}
	attempted := make(map[string]bool, len(unrecordedRefs))
	for _, ref := range unrecordedRefs {
		attempted[strings.ToLower(ref.Owner+"/"+ref.Repo+"@"+ref.Ref)] = true
	}
	var out []Entry
	for _, f := range wr.Findings {
		if f.ActionRef == nil {
			continue
		}
		key := strings.ToLower(f.ActionRef.Owner + "/" + f.ActionRef.Repo + "@" + f.ActionRef.Ref)
		if !attempted[key] || resolved[key] {
			continue
		}
		out = append(out, Entry{
			NWO:        f.ActionRef.Owner + "/" + f.ActionRef.Repo,
			Ref:        f.ActionRef.Ref,
			Resolution: Unresolved,
			Issue:      string(f.Category),
			Reason:     fmt.Sprintf("resolution failed: %s", resolveErr),
			Workflows:  []string{wr.Path},
		})
	}
	return out
}

// narrowDirectDeps rewrites direct deps' mutable refs to precise tags (bare SHA
// or partial/non-semver ref -> full patch tag), leaving transitive deps verbatim.
// Each rewrite mutates deps[i].Ref and records the old->new uses and narrowed NWO.
func narrowDirectDeps(ctx context.Context, opts PlanOptions, deps []dep.Dependency, directTracker lockfile.DirectTracker, rewrites map[string]string, narrowedNWOs map[string]bool) {
	if opts.Tagger == nil {
		return
	}
	for i := range deps {
		dep := &deps[i]
		// Transitive deps come from a composite's action.yml; their ref
		// is the composite author's choice and never appears in our
		// workflow YAML. Narrowing it is pure churn and can invent refs
		// the composite never declared, so leave it verbatim.
		if !directTracker.IsDirect(i) {
			continue
		}
		owner, repo := dep.OwnerRepo()
		if owner == "" {
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
		// No exact tag match - if the repo publishes semver releases,
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

// reverseLookupRewrites canonicalizes dep refs via ReverseLookup (SHA -> tag/
// branch), restoring refs that narrowing or a transitive dep already fixed.
// Returns the rewrites map, indices of unresolvable deps, and any hard error.
func reverseLookupRewrites(ctx context.Context, opts PlanOptions, wr checks.WorkflowReport, deps []dep.Dependency, directTracker lockfile.DirectTracker, narrowedNWOs map[string]bool) (map[string]string, []resolve.LookupIssue, error) {
	// Save narrowed refs before ReverseLookup - it may overwrite dep.Ref
	// with a branch name, but we want to keep the semver tag narrowing chose.
	narrowedRefs := make(map[int]string)
	for i := range deps {
		nwo := strings.ToLower(deps[i].NWO)
		if narrowedNWOs[nwo] {
			narrowedRefs[i] = deps[i].Ref
		}
	}

	// Snapshot transitive deps' declared refs before ReverseLookup mutates
	// them. We restore symbolic refs (tags/branches) afterward but let
	// ReverseLookup's discovered ref stick when the original is a bare SHA.
	transitiveOrigRefs := make(map[int]string)
	for i := range deps {
		if !directTracker.IsDirect(i) {
			transitiveOrigRefs[i] = deps[i].Ref
		}
	}

	// ReverseLookup: SHA -> containing tag/branch. Rewrites refs to canonical form.
	normRewrites, lookupIssues, err := opts.Resolver.ReverseLookup(ctx, deps)
	if err != nil {
		return nil, nil, fmt.Errorf("reverse lookup: %w", err)
	}
	// Restore narrowed refs that ReverseLookup may have overwritten.
	for i, ref := range narrowedRefs {
		deps[i].Ref = ref
	}
	// Restore transitive deps' declared refs — we don't own the composite's
	// action.yml so the lockfile key must match what it declares. The
	// discovered tag/branch is preserved in dep.Tag/dep.Branch and used by
	// state.Set for the lockfile ref: field.
	transitiveRewriteKeys := make(map[string]bool)
	for i := range deps {
		if directTracker.IsDirect(i) {
			continue
		}
		origRef := transitiveOrigRefs[i]
		deps[i].Ref = origRef
		transitiveRewriteKeys[deps[i].NWO+"@"+origRef] = true
	}
	rewrites := make(map[string]string)
	for k, v := range normRewrites {
		if transitiveRewriteKeys[k] {
			continue
		}
		if at := strings.Index(k, "@"); at > 0 {
			nwo := strings.ToLower(k[:at])
			if narrowedNWOs[nwo] {
				continue
			}
		}
		rewrites[k] = v
	}
	return rewrites, lookupIssues, nil
}

// buildPinnedEntries emits an entry for every resolved dep, marking it Verified
// when the lockfile already records the same SHA and Pinned otherwise. Deps
// already emitted from inventory (by NWO:SHA) are skipped.
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
	var out []Entry
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
			Workflows:  []string{wr.Path},
			RequiredBy: parents,
			Direct:     directKeys[depKey],
		}
		out = append(out, entry)
	}
	return out
}

// informationalEntries records ref-moved and misleading-sha findings as
// Investigate entries.
func informationalEntries(wr checks.WorkflowReport) []Entry {
	var out []Entry
	for _, f := range wr.Findings {
		switch f.Category {
		case checks.MisleadingSHA, checks.RefMoved:
			nwo := ""
			ref := ""
			if f.ActionRef != nil {
				nwo = f.ActionRef.Owner + "/" + f.ActionRef.Repo
				ref = f.ActionRef.Ref
			}
			out = append(out, Entry{
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
	return out
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
func pruneStaleInventory(inventory []checks.InventoryEntry, findings []checks.Finding, acceptMoved bool) []checks.InventoryEntry {
	stale := make(map[string]bool)
	for _, f := range findings {
		switch {
		case f.Category == checks.Stale && f.Dependency != nil:
			d := f.Dependency
			stale[strings.ToLower(d.NWO+"@"+d.Ref+":"+d.SHA)] = true
		case acceptMoved && (f.Category == checks.LockfileForgery || f.Category == checks.RefMoved) && f.Dependency != nil:
			d := f.Dependency
			stale[strings.ToLower(d.NWO+"@"+d.Ref+":"+d.SHA)] = true
		}
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

// narrowVerifiedEntries upgrades already-recorded direct deps to full semver
// tags when possible, returning the workflow-YAML rewrites. Skipped for
// --no-narrow, transitive deps, and refs the user kept imprecise (sticky v4).
func narrowVerifiedEntries(ctx context.Context, entries []Entry, opts PlanOptions) map[string]string {
	if opts.NoNarrow || opts.Tagger == nil {
		return nil
	}
	rewrites := make(map[string]string)
	for i := range entries {
		e := &entries[i]
		if !e.Direct {
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

// splitNWO splits "owner/repo" or "owner/repo/sub" into (owner, repo).
func splitNWO(nwo string) (string, string) {
	parts := strings.SplitN(nwo, "/", 3)
	if len(parts) < 2 {
		return "", ""
	}
	return parts[0], parts[1]
}
