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
	// Relock re-resolves every ref live instead of trusting the lockfile.
	Relock bool

	// Resolver UX hooks — set these for interactive spinner mode.
	OnResolveProgress func(done, total int)
	// Profile receives phase timing when profiling is enabled.
	Profile *profile.Session
}

// RunResult bundles the pipeline output.
type RunResult struct {
	Report *checks.Report
	Valid  bool
}

// Run executes the full diagnostic pipeline: parse → resolve unlocked refs →
// diagnose → parity-check locked pins.
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

	// Locked refs are sticky: they resolve from the lockfile, and only refs
	// without an entry hit the network. Seeded entries carry no action.yml,
	// so the recursive walk stops at them; their recorded closure is
	// covered by the parity check below instead.
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
		if opts.Relock {
			continue
		}
		if len(parsed[i].Refs) == 0 || parsed[i].IsFullyRecorded() {
			parsed[i].Resolved = true
			continue
		}
		recorded, _ := parsed[i].PartitionRefs()
		seedDeps = append(seedDeps, parsed[i].RecordedDeps(recorded)...)
		for _, rr := range recorded {
			recordedKeys[strings.ToLower(rr.Owner+"/"+rr.Repo)+"@"+rr.Ref] = true
		}
	}
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
	if r != nil {
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

	// Phase 3: Diagnose.
	endDiag := prof.Phase("  diagnose (parallel)")
	report := DiagnoseParsed(ctx, parsed, r, opts.Store, opts.Pool)
	endDiag()

	// Phase 4: Parity. Under --relock the entries are about to be replaced,
	// so only repository identity, which re-resolving can't fix, matters.
	if r != nil {
		keep := func(checks.Category) bool { return true }
		if opts.Relock {
			keep = func(c checks.Category) bool { return c == checks.RepoMoved }
		}
		endParity := prof.Phase("  parity check")
		checkParity(ctx, r, parsed, opts.Store, report, recordedKeys, keep)
		endParity()
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
	}

	return &RunResult{Report: report, Valid: report.IsValid()}, nil
}
