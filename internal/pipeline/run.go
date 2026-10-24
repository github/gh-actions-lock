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
	"github.com/github/gh-actions-lock/internal/refresh"
	"github.com/github/gh-actions-lock/internal/resolve"
	"github.com/github/gh-actions-lock/internal/workflowfile"
)

// RunOptions configures the Run pipeline.
type RunOptions struct {
	WorkflowPaths []string
	Resolver      *resolve.Resolver
	Store         *lockfile.State
	Pool          *pinpool.Pool
	// Relock re-resolves every ref live instead of trusting the lockfile.
	Relock bool
	// Refresh re-resolves only the entries it selects, as Relock would;
	// every other entry stays as recorded.
	Refresh *refresh.Set
	// AcceptMoved accepts recorded pins whose ref no longer vouches for
	// them; it implies Relock.
	AcceptMoved bool
	// Verify also checks that each pin on a mutable ref is still in that
	// ref's history, at one Compare call per ref that moved.
	Verify bool
	// ReadOnly runs never write, so recorded entries are checked even under
	// Relock.
	ReadOnly bool

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
	// The rebuild of an affected workflow resolves every use, and a use
	// with a subpath resolves under its own cache key; Dedup would fold
	// these into the repository's seed.
	var pathSeeds []dep.Dependency
	recordedKeys := make(map[string]bool)
	for i := range parsed {
		// Structural blockers are terminal at diagnose time. Do not perform
		// unrelated network work for a workflow the planner must reject.
		if len(parsed[i].SelfRepositoryRefErrs) > 0 ||
			len(parsed[i].SelfRepositoryResolutionErrs) > 0 {
			parsed[i].Resolved = true
			continue
		}
		if opts.Relock {
			continue
		}
		// A workflow that records an entry update selects resolves again,
		// but its other entries stay seeded, each under the path its use
		// names, so only the selected ones move.
		affected := opts.Refresh.Workflow(workflowfile.KeyFromPath(parsed[i].Path))
		if !affected && (len(parsed[i].Refs) == 0 || parsed[i].IsFullyRecorded()) {
			parsed[i].Resolved = true
			continue
		}
		recorded, _ := parsed[i].PartitionRefs()
		for _, d := range parsed[i].RecordedDeps(recorded) {
			if !lockfile.Seedable(d) || opts.Refresh.Has(d.NWO, d.Ref) {
				continue
			}
			seedDeps = append(seedDeps, d)
			if affected {
				pathSeeds = append(pathSeeds, subpathSeeds(d, recorded)...)
			}
		}
		for _, rr := range recorded {
			if !opts.Refresh.Has(rr.Owner+"/"+rr.Repo, rr.Ref) {
				recordedKeys[strings.ToLower(rr.Owner+"/"+rr.Repo)+"@"+rr.Ref] = true
			}
		}
	}
	if r != nil && len(seedDeps) > 0 {
		r.SeedFromLockfile(dep.Dedup(seedDeps))
		r.SeedFromLockfile(pathSeeds)
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
			if cerr := ghapi.ConfigError(err); cerr != nil {
				return nil, cerr
			}
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
	endDiag()

	// Phase 4: Parity.
	if r != nil {
		mode := parityMode{verify: opts.Verify, relock: opts.Relock, refresh: opts.Refresh, acceptMoved: opts.AcceptMoved, readOnly: opts.ReadOnly}
		endParity := prof.Phase("  parity check")
		err := checkParity(ctx, r, parsed, opts.Store, report, recordedKeys, resolved, parents, mode)
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

	if r != nil {
		report.RetriedFor = r.GHClient().RetriedFor()
	}
	return &RunResult{Report: report, Valid: report.IsValid()}, nil
}

// subpathSeeds returns d once for each use in recorded that names it with a
// subpath (owner/repo/sub@ref).
func subpathSeeds(d dep.Dependency, recorded []parserlock.ActionRef) []dep.Dependency {
	var out []dep.Dependency
	for _, rr := range recorded {
		if rr.Path != "" && rr.Ref == d.Ref && strings.EqualFold(rr.Owner+"/"+rr.Repo, d.NWO) {
			pd := d
			pd.Path = rr.Path
			out = append(out, pd)
		}
	}
	return out
}
