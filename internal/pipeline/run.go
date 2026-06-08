package pipeline

import (
	"context"

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

	// Fast path: trust fully-recorded workflows.
	skippedRescan := 0
	if !opts.Rescan {
		for i := range parsed {
			if isFullyRecorded(parsed[i]) {
				parsed[i].Resolved = true
				skippedRescan++
			} else {
				parsed[i].SkipReachWhenUnchanged = true
			}
		}
	}

	// Collect unresolved workflows for network work.
	var unresolved []checks.ParsedWorkflow
	for _, pw := range parsed {
		if !pw.Resolved {
			unresolved = append(unresolved, pw)
		}
	}
	refs, deps := CollectResolvable(unresolved)

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

// isFullyRecorded returns true when every direct ref in the workflow has a
// matching lockfile entry — the steady-state happy path.
func isFullyRecorded(pw checks.ParsedWorkflow) bool {
	if pw.LoadErr != nil || pw.DepsErr != nil {
		return false
	}
	if len(pw.Refs) == 0 {
		return true
	}
	haveDep := make(map[string]bool, len(pw.ExistingDeps))
	for _, d := range pw.ExistingDeps {
		haveDep[d.NWO+"@"+d.Ref] = true
	}
	for _, r := range pw.Refs {
		if !haveDep[r.Owner+"/"+r.Repo+"@"+r.Ref] {
			return false
		}
	}
	return true
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
