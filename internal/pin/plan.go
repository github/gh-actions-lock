package pin

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	parserlock "github.com/github/actions-lockfile/go/pkg/lockfile"
	"github.com/github/gh-actions-pin/internal/dep"
	"github.com/github/gh-actions-pin/internal/lockfile"
	"github.com/github/gh-actions-pin/internal/pinpool"
	"github.com/github/gh-actions-pin/internal/pipeline/checks"
	"github.com/github/gh-actions-pin/internal/resolve"
	"github.com/github/gh-actions-pin/internal/tag"
	"github.com/github/gh-actions-pin/internal/workflowfile"
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

	results := make([]planResult, len(report.Workflows))
	var planErr error
	poolErr := pinpool.RunTyped(opts.Pool, ctx, "Planning pins",
		items,
		func(iwr indexedWR) string { return iwr.wr.Path },
		func(ctx context.Context, _ int, iwr indexedWR) error {
			pr, err := planWorkflow(ctx, iwr.wr, opts)
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

func planWorkflow(ctx context.Context, wr checks.WorkflowReport, opts PlanOptions) (planResult, error) {
	var entries []Entry
	var wplans []WorkflowPlan

	if !wr.NeedsAttention() {
		// Already pinned and valid — record as verified.
		for _, inv := range wr.Inventory {
			entries = append(entries, Entry{
				NWO:        inv.Dep.NWO,
				Ref:        inv.Dep.Ref,
				SHA:        inv.Dep.SHA,
				Resolution: Verified,
				Workflows:  []string{wr.Path},
				Direct:     inv.Direct,
				RequiredBy: inv.Parents,
			})
		}
		return planResult{entries: entries, wplans: wplans}, nil
	}

	// Resolve live state for this workflow's refs.
	deps, parentMap, resolveErr := opts.Resolver.ResolveAllRecursive(ctx, wr.ActionRefs)
	if resolveErr != nil {
		// Partial failure: some refs resolved (in deps), others didn't.
		// Build a set of resolved NWO@Ref keys so we can continue with
		// the successful ones and only mark the failures as unresolved.
		resolved := make(map[string]bool, len(deps))
		for _, d := range deps {
			resolved[strings.ToLower(d.NWO+"@"+d.Ref)] = true
		}
		for _, f := range wr.Findings {
			if f.ActionRef == nil {
				continue
			}
			key := strings.ToLower(f.ActionRef.Owner + "/" + f.ActionRef.Repo + "@" + f.ActionRef.Ref)
			if resolved[key] {
				continue // this ref resolved fine; process it below
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
	directTracker := lockfile.NewDirectTracker(wr.ActionRefs, deps)

	// Narrow mutable version tags to patch tags, and resolve bare-SHA refs
	// to a symbolic tag when one exists.
	rewrites := make(map[string]string)
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
				if err != nil || patchTag == "" {
					continue
				}
				oldUses := dep.NWO + "@" + dep.Ref
				newUses := dep.NWO + "@" + patchTag
				rewrites[oldUses] = newUses
				dep.Ref = patchTag
				continue
			}

			// Mutable version tags (v4, v3.1): narrow to patch release.
			sv, ok := parserlock.ParseSemVer(dep.Ref)
			if !ok || !sv.IsMutable() {
				continue
			}
			patchTag, err := opts.Tagger.BestPatchTagForSHA(ctx, owner, repo, dep.SHA)
			if err != nil || patchTag == "" {
				continue
			}
			oldUses := dep.NWO + "@" + dep.Ref
			newUses := dep.NWO + "@" + patchTag
			rewrites[oldUses] = newUses
			dep.Ref = patchTag
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
	for k, v := range normRewrites {
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

	// Build entries for all pinned deps.
	directKeys := directTracker.Keys(deps)
	for _, dep := range deps {
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
