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
	"github.com/github/gh-actions-lock/internal/workflowfile"
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
	// --rescan. Mutable recorded refs (v4, v4.2, branches) legitimately move,
	// so they stay trusted after a cheap repository identity check confirms
	// the NWO.
	skippedRescan := 0
	fastPlans := make([]fastPathPlan, len(parsed))
	identityRefs := make([][]repositoryIdentityRef, len(parsed))
	var lockSnapshot parserlock.File
	if opts.Store != nil {
		lockSnapshot = opts.Store.File()
	}
	for i := range parsed {
		if len(parsed[i].LocalPaths) == 0 &&
			len(parsed[i].SelfRepositoryRefErrs) == 0 &&
			len(parsed[i].SelfRepositoryResolutionErrs) == 0 {
			fastPlans[i] = planFastPath(parsed[i])
			identityRefs[i] = repositoryIdentityRefs(parsed[i].Path, lockSnapshot)
		}
	}
	repositoryIdentities := lookupRepositoryIdentities(ctx, r, opts.Pool, identityRefs)
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
		plan := fastPlans[i]
		trustedMutable := plan.mutableRefs[:0]
		for _, ref := range plan.mutableRefs {
			identity := repositoryIdentities[ghapi.ForRepo(ref.Owner, ref.Repo)]
			if identity.matches(ref.NWO(), lockedRepoID(identityRefs[i], ref)) {
				trustedMutable = append(trustedMutable, ref)
			}
		}
		for _, item := range identityRefs[i] {
			if item.Parent == "" {
				continue
			}
			identity := repositoryIdentities[ghapi.ForRepo(item.Ref.Owner, item.Ref.Repo)]
			if !identity.matches(item.Ref.NWO(), item.RepoID) {
				trustedMutable = nil
				break
			}
		}
		plan.resolved = plan.resolved && len(trustedMutable) == len(plan.mutableRefs)
		plan.mutableRefs = trustedMutable
		// Mutable recorded refs are trusted without live action resolution
		// after the repository identity check above.
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
	appendKnownRepositoryIdentityFindings(report, identityRefs, repositoryIdentities)
	endDiag()
	valid := report.IsValid()

	return &RunResult{
		Report:        report,
		Valid:         valid,
		SkippedRescan: skippedRescan,
	}, nil
}

type repositoryIdentityRef struct {
	Ref    parserlock.ActionRef
	Parent string
	RepoID int64
}

func repositoryIdentityRefs(path string, file parserlock.File) []repositoryIdentityRef {
	var refs []repositoryIdentityRef
	index := make(map[ghapi.NWORef]int)
	add := func(ref parserlock.ActionRef, parent string, repoID int64) {
		key := ghapi.ForNWORef(ref.Owner, ref.Repo, ref.Ref)
		if i, ok := index[key]; ok {
			if parent != "" {
				refs[i].Parent = parent
			}
			if repoID != 0 {
				refs[i].RepoID = repoID
			}
			return
		}
		index[key] = len(refs)
		refs = append(refs, repositoryIdentityRef{Ref: ref, Parent: parent, RepoID: repoID})
	}
	seen := make(map[string]bool)
	var walk func(string, string)
	walk = func(pinKey, parent string) {
		if seen[pinKey] {
			return
		}
		seen[pinKey] = true
		pin, ok := parserlock.ParsePin(pinKey)
		if !ok {
			return
		}
		action := file.Dependencies[pinKey]
		add(parserlock.ActionRef{Owner: pin.Owner, Repo: pin.Repo, Ref: pin.Ref}, parent, action.RepoID)
		for _, child := range action.Uses {
			walk(child, pinKey)
		}
	}
	for _, root := range file.Workflows[workflowfile.KeyFromPath(path)] {
		walk(root, "")
	}
	return refs
}

type repositoryIdentity struct {
	canonical string
	repoID    int64
}

func (i repositoryIdentity) matches(nwo string, repoID int64) bool {
	return i.canonical != "" && strings.EqualFold(i.canonical, nwo) &&
		(repoID == 0 || i.repoID == repoID)
}

func lockedRepoID(refs []repositoryIdentityRef, ref parserlock.ActionRef) int64 {
	key := ghapi.ForNWORef(ref.Owner, ref.Repo, ref.Ref)
	for _, item := range refs {
		if ghapi.ForNWORef(item.Ref.Owner, item.Ref.Repo, item.Ref.Ref) == key {
			return item.RepoID
		}
	}
	return 0
}

func lookupRepositoryIdentities(ctx context.Context, r *resolve.Resolver, pool *pinpool.Pool, workflows [][]repositoryIdentityRef) map[ghapi.Repo]repositoryIdentity {
	type indexedRef struct {
		idx int
		ref parserlock.ActionRef
	}
	var repos []indexedRef
	seen := make(map[ghapi.Repo]bool)
	for _, identities := range workflows {
		for _, item := range identities {
			ref := item.Ref
			key := ghapi.ForRepo(ref.Owner, ref.Repo)
			if !seen[key] {
				seen[key] = true
				repos = append(repos, indexedRef{idx: len(repos), ref: ref})
			}
		}
	}
	results := make([]repositoryIdentity, len(repos))
	if r != nil {
		_ = pinpool.RunTyped(pool, ctx, "", repos,
			func(indexedRef) string { return "" },
			func(ctx context.Context, _ int, item indexedRef) error {
				canonical, err := r.CanonicalNWO(ctx, item.ref.Owner, item.ref.Repo)
				if err == nil {
					results[item.idx].canonical = canonical
					_, repoID, idErr := r.RepoIDs(ctx, item.ref.Owner, item.ref.Repo)
					if idErr == nil {
						results[item.idx].repoID = repoID
					}
				}
				return nil
			},
		)
	}
	identities := make(map[ghapi.Repo]repositoryIdentity, len(repos))
	for _, item := range repos {
		identities[ghapi.ForRepo(item.ref.Owner, item.ref.Repo)] = results[item.idx]
	}
	return identities
}

func appendKnownRepositoryIdentityFindings(report *checks.Report, workflows [][]repositoryIdentityRef, identities map[ghapi.Repo]repositoryIdentity) {
	for i := range report.Workflows {
		wr := &report.Workflows[i]
		for _, item := range workflows[i] {
			ref := item.Ref
			identity := identities[ghapi.ForRepo(ref.Owner, ref.Repo)]
			if identity.canonical == "" {
				continue
			}
			if strings.EqualFold(identity.canonical, ref.NWO()) {
				if item.RepoID != 0 && identity.repoID != 0 && identity.repoID != item.RepoID {
					wr.Findings = append(wr.Findings, checks.Finding{
						WorkflowPath: wr.Path,
						Category:     checks.RepositoryChanged,
						Severity:     checks.SeverityError,
						Confidence:   checks.ConfidenceHigh,
						ActionRef:    &ref,
						Detail:       fmt.Sprintf("repository %s has repository ID %d, but the lockfile records %d; the repository was deleted or replaced", ref.NWO(), identity.repoID, item.RepoID),
						Remediation:  "review the replacement repository, then remove its lockfile entry and run `gh actions-lock` to trust it",
					})
				}
				continue
			}
			if resolvedTransfer(wr.ResolvedDeps, ref) {
				continue
			}
			known := dep.Dependency{
				NWO:          identity.canonical,
				Ref:          ref.Ref,
				OriginalRefs: []parserlock.ActionRef{ref},
			}
			wr.ResolvedDeps = append(wr.ResolvedDeps, known)
			if item.Parent != "" {
				if wr.ResolvedParents == nil {
					wr.ResolvedParents = make(dep.ParentMap)
				}
				wr.ResolvedParents[ref.NWO()+"@"+ref.Ref] = []string{item.Parent}
			}
			appendTransferredRepositoryFindings(wr, []dep.Dependency{known}, wr.ResolvedParents)
		}
	}
}

func resolvedTransfer(deps []dep.Dependency, original parserlock.ActionRef) bool {
	for _, d := range deps {
		for _, ref := range d.OriginalRefs {
			if strings.EqualFold(ref.NWO(), original.NWO()) && ref.Ref == original.Ref {
				return true
			}
		}
	}
	return false
}

// fastPathPlan describes how the pre-resolution fast path treats one
// recorded workflow.
type fastPathPlan struct {
	// resolved is true when the workflow needs no live resolution: it has
	// no refs, is a local-path action, or every recorded ref is a trusted
	// mutable pin.
	resolved bool
	// mutableRefs are recorded refs (v4, v4.2, branches) eligible for trust
	// from the lockfile after their repository identities are validated.
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
