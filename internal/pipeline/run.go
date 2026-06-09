package pipeline

import (
	"context"

	parserlock "github.com/github/actions-lockfile/go/pkg/lockfile"
	"github.com/github/gh-actions-pin/internal/dep"
	"github.com/github/gh-actions-pin/internal/lockfile"
	"github.com/github/gh-actions-pin/internal/pinpool"
	"github.com/github/gh-actions-pin/internal/pipeline/checks"
	"github.com/github/gh-actions-pin/internal/profile"
	"github.com/github/gh-actions-pin/internal/resolve"
	"github.com/github/gh-actions-pin/internal/tag"
)

// RunOptions configures the Run pipeline.
type RunOptions struct {
	WorkflowPaths []string
	Resolver      *resolve.Resolver
	Tagger        *tag.Lister
	Store         *lockfile.State
	Pool          *pinpool.Pool
	Rescan        bool // re-verify all pins end-to-end

	// Resolver UX hooks — set these for interactive spinner mode.
	OnResolveProgress func(done, total int)
	// Profile receives phase timing when profiling is enabled.
	Profile *profile.Session
}

// RunResult bundles the pipeline output.
type RunResult struct {
	Report        *checks.Report
	Valid         bool
	SkippedRescan int
}

// Run executes the full diagnostic pipeline: parse → trust-check →
// resolve → reachability pre-warm → diagnose → enrich impostors.
func Run(ctx context.Context, opts RunOptions) (*RunResult, error) {
	r := opts.Resolver
	prof := opts.Profile

	// Phase 1: Parse.
	endParse := prof.Phase("  parse workflows")
	parsed := ParseAll(opts.WorkflowPaths, opts.Store)
	endParse()

	if ctx.Err() != nil {
		return nil, ctx.Err()
	}

	// Fast path: trust fully-recorded workflows. For partially-recorded
	// workflows, seed the resolver cache with recorded deps so only
	// unrecorded refs hit the network.
	skippedRescan := 0
	var seedDeps []dep.Dependency
	recordedKeys := make(map[string]bool)
	if !opts.Rescan {
		for i := range parsed {
			recorded, unrecorded := partitionRefs(parsed[i])
			if len(parsed[i].Refs) == 0 || len(unrecorded) == 0 {
				parsed[i].Resolved = true
				skippedRescan++
			} else {
				parsed[i].SkipReachWhenUnchanged = true
				// Collect deps covered by recorded refs for cache
				// seeding. These refs have matching lockfile entries,
				// so their resolve + reachability results can be
				// served from the lockfile rather than the network.
				rd := recordedDeps(parsed[i], recorded)
				seedDeps = append(seedDeps, rd...)
				for _, r := range recorded {
					recordedKeys[r.Owner+"/"+r.Repo+"@"+r.Ref] = true
				}
			}
		}
	}

	// Seed the resolver cache with lockfile entries for recorded deps
	// in partially-recorded workflows. This makes the pipeline
	// self-sufficient: diagnoseOneParsed re-resolves ALL refs per
	// workflow, and seeded entries become free cache hits.
	//
	// Trust boundary: seeded entries have no actionYML, so the BFS in
	// ResolveAllRecursive won't discover new transitive deps through
	// them. This is intentional — the same trust model as
	// isFullyRecorded, which skips resolution entirely. If the
	// lockfile's transitive closure is incomplete, --rescan detects it.
	if r != nil && len(seedDeps) > 0 {
		r.SeedFromLockfile(dep.Dedup(seedDeps))
	}

	// Collect unresolved workflows for network work.
	var unresolved []checks.ParsedWorkflow
	for _, pw := range parsed {
		if !pw.Resolved {
			unresolved = append(unresolved, pw)
		}
	}
	refs, deps := CollectUnrecordedResolvable(unresolved, recordedKeys)

	// Phase 2: Resolve.
	if r == nil {
		// No resolver means no network resolution or reachability.
		// Diagnose will still flag structural issues (not-pinned, etc.).
	} else {
		// Wire resolver progress hook.
		if opts.OnResolveProgress != nil {
			r.OnResolveProgress = opts.OnResolveProgress
		}

		if len(refs) > 0 {
			endResolve := prof.Phase("  resolve refs")
			// First call warms the resolver's cache; results are consumed
			// indirectly by the reachability phase and diagnose via cache
			// lookups. The live deps are re-fetched from cache below.
			_, _, _ = r.ResolveAllRecursive(ctx, refs)
			endResolve()
		}

		if ctx.Err() != nil {
			return nil, ctx.Err()
		}

		// Phase 3: Pre-warm reachability across all unresolved workflows.
		var reachDeps, liveMoved, liveDirect []dep.Dependency
		if opts.Rescan {
			reachDeps = deps
			if len(unresolved) > 0 {
				live, _, _ := r.ResolveAllRecursive(ctx, refs)
				liveMoved = CollectLiveMovedReachDeps(unresolved, live)
				liveDirect = CollectLiveDirectReachDeps(unresolved, live)
			}
		} else {
			live, _, _ := r.ResolveAllRecursive(ctx, refs)
			// Merge recorded deps into the live set so CollectReachDeps
			// treats them as confirmed at their lockfile SHAs, preventing
			// unnecessary reachability network checks.
			if len(seedDeps) > 0 {
				live = append(live, dep.Dedup(seedDeps)...)
			}
			reachDeps = CollectReachDeps(unresolved, live)
			liveMoved = CollectLiveMovedReachDeps(unresolved, live)
			liveDirect = CollectLiveDirectReachDeps(unresolved, live)
		}

		if ctx.Err() != nil {
			return nil, ctx.Err()
		}

		if len(reachDeps) > 0 || len(liveMoved) > 0 || len(liveDirect) > 0 {
			endReach := prof.Phase("  reachability pre-warm")
			if len(reachDeps) > 0 {
				_ = r.CheckReachabilityAll(ctx, reachDeps)
			}
			if ctx.Err() == nil && len(liveMoved) > 0 {
				_ = r.CheckReachabilityAll(ctx, liveMoved)
			}
			if ctx.Err() == nil && len(liveDirect) > 0 {
				_ = r.CheckReachabilityAll(ctx, liveDirect)
			}
			endReach()
		}

		// Quiet resolver hooks before diagnostics (cache-only, no progress).
		r.OnResolveProgress = nil
	}

	if ctx.Err() != nil {
		return nil, ctx.Err()
	}

	// Phase 4: Diagnose.
	endDiag := prof.Phase("  diagnose (parallel)")
	report := DiagnoseParsed(ctx, parsed, r, opts.Store, opts.Pool)
	endDiag()
	valid := report.IsValid()

	if ctx.Err() != nil {
		return nil, ctx.Err()
	}

	// Phase 5: Enrich impostor findings with recommended release suggestions.
	if opts.Tagger != nil && hasImpostorFindings(report) {
		checks.EnrichImpostorFindings(ctx, report, opts.Tagger, r, opts.Pool)
	}

	return &RunResult{
		Report:        report,
		Valid:         valid,
		SkippedRescan: skippedRescan,
	}, nil
}

// partitionRefs splits a workflow's refs into recorded (have a matching
// lockfile entry) and unrecorded (need network resolution). The split
// uses NWO@Ref identity — the same granularity the lockfile pins at.
// When an error prevented loading refs or deps, everything is unrecorded.
func partitionRefs(pw checks.ParsedWorkflow) (recorded, unrecorded []parserlock.ActionRef) {
	if pw.LoadErr != nil || pw.DepsErr != nil {
		return nil, pw.Refs
	}
	if len(pw.Refs) == 0 {
		return nil, nil
	}
	haveDep := make(map[string]bool, len(pw.ExistingDeps))
	for _, d := range pw.ExistingDeps {
		haveDep[d.NWO+"@"+d.Ref] = true
	}
	for _, r := range pw.Refs {
		if haveDep[r.Owner+"/"+r.Repo+"@"+r.Ref] {
			recorded = append(recorded, r)
		} else {
			unrecorded = append(unrecorded, r)
		}
	}
	return recorded, unrecorded
}

// isFullyRecorded returns true when every direct ref in the workflow has a
// matching lockfile entry — the steady-state happy path.
func isFullyRecorded(pw checks.ParsedWorkflow) bool {
	_, unrecorded := partitionRefs(pw)
	return len(pw.Refs) == 0 || len(unrecorded) == 0
}

// recordedDeps returns the subset of pw.ExistingDeps whose NWO@Ref matches
// one of the recorded refs. These are the lockfile entries that correspond
// to refs the workflow already has pinned.
func recordedDeps(pw checks.ParsedWorkflow, recorded []parserlock.ActionRef) []dep.Dependency {
	refKeys := make(map[string]bool, len(recorded))
	for _, r := range recorded {
		refKeys[r.Owner+"/"+r.Repo+"@"+r.Ref] = true
	}
	var out []dep.Dependency
	for _, d := range pw.ExistingDeps {
		if refKeys[d.Key()] {
			out = append(out, d)
		}
	}
	return out
}

func hasImpostorFindings(r *checks.Report) bool {
	for _, wr := range r.Workflows {
		for _, f := range wr.Findings {
			if f.Category == checks.ImpostorCommit {
				return true
			}
		}
	}
	return false
}
