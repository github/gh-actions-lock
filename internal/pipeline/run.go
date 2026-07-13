package pipeline

import (
	"context"
	"strings"

	parserlock "github.com/github/actions-lockfile/go/pkg/lockfile"
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
	SkippedRescan int // mutable recorded refs (v4, branches) trusted without a live re-check
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
	//
	// Immutable full-semver pins (e.g. v4.2.1) are NOT trusted blindly:
	// they're routed through live resolution + ancestry so a stale or
	// unreachable pin is caught on the default path, not just under
	// --rescan. Mutable recorded refs (v4, v4.2, branches) legitimately
	// move, so they stay trusted (seeded from the lockfile) until --rescan.
	skippedRescan := 0
	var seedDeps []dep.Dependency
	recordedKeys := make(map[string]bool)
	if !opts.Rescan {
		for i := range parsed {
			plan := planFastPath(parsed[i])
			// Mutable recorded refs are trusted without a live re-check
			// (surfaced in the summary so the operator can --rescan them).
			skippedRescan += len(plan.mutableRefs)
			if plan.resolved {
				parsed[i].Resolved = true
				continue
			}
			// Seed only the mutable recorded deps so they resolve from
			// the lockfile (trusted); immutable and unrecorded refs are
			// left to resolve live from the network.
			rd := parsed[i].RecordedDeps(plan.mutableRefs)
			seedDeps = append(seedDeps, rd...)
			for _, rr := range plan.mutableRefs {
				recordedKeys[strings.ToLower(rr.Owner+"/"+rr.Repo)+"@"+rr.Ref] = true
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

// fastPathPlan describes how the pre-resolution fast path treats one
// recorded workflow.
type fastPathPlan struct {
	// resolved is true when the workflow needs no live resolution: it has
	// no refs, is a local-path action, or every recorded ref is a trusted
	// mutable pin.
	resolved bool
	// mutableRefs are recorded refs (v4, v4.2, branches) trusted from the
	// lockfile without a live re-check.
	mutableRefs []parserlock.ActionRef
}

// planFastPath decides, without touching the network, whether a parsed
// workflow can skip live resolution and which of its recorded refs are
// trusted mutable pins. Immutable full-semver pins (v4.2.1) are never
// trusted blindly: their presence forces live resolution so a stale or
// unreachable pin is caught on the default path, not just under --rescan.
func planFastPath(pw checks.ParsedWorkflow) fastPathPlan {
	// Local-path workflows are handled at diagnose time; don't waste
	// network calls resolving their refs.
	if len(pw.LocalPaths) > 0 {
		return fastPathPlan{resolved: true}
	}
	recorded, unrecorded := pw.PartitionRefs()

	var mutable []parserlock.ActionRef
	immutableCount := 0
	for _, rr := range recorded {
		if checks.IsImmutableRef(rr.Ref) {
			immutableCount++
		} else {
			mutable = append(mutable, rr)
		}
	}

	resolved := len(pw.Refs) == 0 || (len(unrecorded) == 0 && immutableCount == 0)
	return fastPathPlan{resolved: resolved, mutableRefs: mutable}
}
