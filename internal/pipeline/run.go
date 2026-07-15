package pipeline

import (
	"context"
	"strings"

	"github.com/github/gh-actions-lock/internal/dep"
	"github.com/github/gh-actions-lock/internal/lockfile"
	"github.com/github/gh-actions-lock/internal/pinpool"
	"github.com/github/gh-actions-lock/internal/pipeline/checks"
	"github.com/github/gh-actions-lock/internal/profile"
	"github.com/github/gh-actions-lock/internal/resolve"
)

// RunOptions configures the Run pipeline.
type RunOptions struct {
	WorkflowPaths []string
	Resolver      *resolve.Resolver
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
	SkippedRescan int // already-pinned workflows trusted without network calls
}

// Run executes the full diagnostic pipeline: parse → trust-check →
// resolve → diagnose.
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
	for i := range parsed {
		// Structural blockers are terminal at diagnose time. Do not perform
		// unrelated network work for a workflow the planner must reject.
		if len(parsed[i].LocalPaths) > 0 ||
			len(parsed[i].SelfRepositoryRefErrs) > 0 ||
			len(parsed[i].SelfRepositoryResolutionErrs) > 0 {
			parsed[i].Resolved = true
			continue
		}
		if opts.Rescan {
			continue
		}
		recorded, unrecorded := parsed[i].PartitionRefs()
		if len(parsed[i].Refs) == 0 || len(unrecorded) == 0 {
			parsed[i].Resolved = true
			skippedRescan++
		} else {
			// Collect deps covered by recorded refs for cache
			// seeding. These refs have matching lockfile entries,
			// so their resolve results can be served from the
			// lockfile rather than the network.
			rd := parsed[i].RecordedDeps(recorded)
			seedDeps = append(seedDeps, rd...)
			for _, r := range recorded {
				recordedKeys[strings.ToLower(r.Owner+"/"+r.Repo)+"@"+r.Ref] = true
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
	// IsFullyRecorded, which skips resolution entirely. If the
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
	refs, _ := CollectUnrecordedResolvable(unresolved, recordedKeys)

	// Phase 2: Resolve.
	if r == nil {
		// No resolver means no network resolution.
		// Diagnose will still flag structural issues (not-pinned, etc.).
	} else {
		// Wire resolver progress hook.
		if opts.OnResolveProgress != nil {
			r.OnResolveProgress = opts.OnResolveProgress
		}

		if len(refs) > 0 {
			endResolve := prof.Phase("  resolve refs")
			_, _, _ = r.ResolveAllRecursive(ctx, refs)
			endResolve()
		}

		if ctx.Err() != nil {
			return nil, ctx.Err()
		}

		// Quiet resolver hooks before diagnostics (cache-only, no progress).
		r.OnResolveProgress = nil
	}

	if ctx.Err() != nil {
		return nil, ctx.Err()
	}

	// Phase 3: Diagnose.
	endDiag := prof.Phase("  diagnose (parallel)")
	report := DiagnoseParsed(ctx, parsed, r, opts.Store, opts.Pool)
	endDiag()
	valid := report.IsValid()

	return &RunResult{
		Report:        report,
		Valid:         valid,
		SkippedRescan: skippedRescan,
	}, nil
}
