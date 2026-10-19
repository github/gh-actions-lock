package pipeline

import (
	"context"
	"fmt"
	"strings"

	parserlock "github.com/github/actions-lockfile/go/pkg/lockfile"
	"github.com/github/gh-actions-lock/internal/dep"
	"github.com/github/gh-actions-lock/internal/ghapi"
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
	// AcceptMoved explicitly permits replacing unreachable recorded pins.
	AcceptMoved bool
	// Verify also checks that each pin on a mutable ref is still in that
	// ref's history and that locked manifests match the recorded graph.
	Verify bool

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
	var resolved []dep.Dependency
	var parents map[string][]string

	// Phase 2: Resolve.
	if r != nil {
		if opts.OnResolveProgress != nil {
			r.OnResolveProgress = opts.OnResolveProgress
		}

		if len(refs) > 0 {
			endResolve := prof.Phase("  resolve refs")
			var err error
			resolved, parents, err = r.ResolveAllRecursive(ctx, refs)
			endResolve()
			// Behind the proxy a pin that can't be resolved for a transient
			// reason is a retry, not a not-pinned finding.
			if err != nil && ghapi.DependabotProxy() && ghapi.IsTransient(err) {
				return nil, fmt.Errorf("could not resolve actions: %w", err)
			}
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
	for i := range report.Workflows {
		wr := &report.Workflows[i]
		for j := range wr.Findings {
			f := &wr.Findings[j]
			if f.IncompleteLockfile {
				f.Remediable = opts.Relock
				if !opts.Relock {
					wr.SkipCommit = true
				}
			}
		}
	}
	endDiag()

	// Phase 4: Parity. Refreshing cannot excuse a repository identity change
	// or an unreachable recorded commit, including transitive pins.
	if r != nil {
		keep := func(checks.Finding, parserlock.Pin) bool { return true }
		if opts.Relock {
			keep = func(f checks.Finding, p parserlock.Pin) bool {
				c := f.Category
				return c == checks.RepoRenamed || c == checks.RepoReplaced || c == checks.RepoUnavailable ||
					c == checks.UnreachablePin && (!opts.AcceptMoved || parserlock.IsFullSha(p.Ref) || f.RefMissing)
			}
		}
		endParity := prof.Phase("  parity check")
		err := checkParity(ctx, r, parsed, opts.Store, report, recordedKeys, resolved, parents, keep)
		endParity()
		// A cancelled request surfaces as an inconclusive lookup; report the
		// cancellation, not the failure it caused.
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if err != nil {
			return nil, err
		}
	}

	if opts.Verify && r != nil && opts.Store != nil {
		if err := checkLockedManifests(ctx, r, parsed, opts.Store, report); err != nil {
			return nil, err
		}
	}
	return &RunResult{Report: report, Valid: report.IsValid()}, nil
}
