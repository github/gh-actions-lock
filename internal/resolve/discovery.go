package resolve

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"

	parserlock "github.com/github/actions-lockfile/go/pkg/lockfile"
	"github.com/github/gh-actions-pin/internal/dep"
	"github.com/github/gh-actions-pin/internal/ghapi"
	"github.com/github/gh-actions-pin/internal/pinpool"
)

// LatestRef returns the highest stable tag for an action repository.
func (r *Resolver) LatestRef(ctx context.Context, owner, repo string) (string, error) {
	key := ghapi.ForRepo(owner, repo)
	if ref, ok := r.latestRefCache.Get(key); ok {
		return ref, nil
	}

	entries, err := r.gh.ListTags(ctx, owner, repo)
	if err != nil {
		return "", err
	}

	tags := make([]string, 0, len(entries))
	for _, e := range entries {
		tags = append(tags, e.Name)
	}

	best := selectLatestTag(tags)
	if best == "" {
		return "", fmt.Errorf("%s: no tags available to upgrade", key)
	}

	r.latestRefCache.Put(key, best)
	return best, nil
}

func cacheKey(ref parserlock.ActionRef) ghapi.ActionRef {
	return ghapi.ForActionRef(ref.Owner, ref.Repo, ref.Path, ref.Ref)
}

// ResolveAllRecursive resolves action refs and recursively discovers transitive
// dependencies from composite actions by reading their action.yml via GraphQL.
// The returned ParentMap (child dep key → parent dep keys) is owned by the
// caller and safe to mutate or hold across concurrent resolver calls.
func (r *Resolver) ResolveAllRecursive(ctx context.Context, refs []parserlock.ActionRef) ([]dep.Dependency, dep.ParentMap, error) {
	seen := make(map[ghapi.ActionRef]bool)
	var allDeps []dep.Dependency
	parentMap := make(dep.ParentMap)

	pending := refs
	depth := 0

	// Rolling counters spanning every BFS depth. Total grows as transitive
	// refs are discovered; done grows as workers complete. The caller renders
	// a single non-jumping bar over the union of work — "transitive" is not a
	// distinct top-level phase, just deeper edges in the same graph.
	var resolveDone atomic.Int64
	var resolveTotal atomic.Int64
	// Fire an initial 0/N callback so the UI shows the bar immediately at the
	// known size of the first wave (refs the caller passed in).
	if r.OnResolveProgress != nil {
		// Compute first-wave uncached size up-front for an accurate initial total.
		firstWave := 0
		for _, ref := range refs {
			if _, ok := r.cache.Get(cacheKey(ref)); !ok {
				firstWave++
			}
		}
		resolveTotal.Store(int64(firstWave))
		r.FireResolveProgress(0, firstWave)
	}

	for len(pending) > 0 {
		if depth >= r.MaxRecursionDepth {
			return allDeps, parentMap, fmt.Errorf("composite action recursion exceeded max depth %d", r.MaxRecursionDepth)
		}

		var toResolve []parserlock.ActionRef
		for _, ref := range pending {
			key := cacheKey(ref)
			if !seen[key] {
				seen[key] = true
				toResolve = append(toResolve, ref)
			}
		}

		if len(toResolve) == 0 {
			break
		}

		var deps []dep.Dependency
		var actionYMLs []string
		var err error
		deps, actionYMLs, err = r.resolveWithActionYMLParallel(ctx, toResolve, depth, &resolveDone, &resolveTotal)
		// Keep partial results: per-ref failures are surfaced via err, but
		// successful resolutions in `deps` should not be discarded — downstream
		// renderers degrade gracefully per-ref instead of marking everything
		// unresolved.
		allDeps = append(allDeps, deps...)
		if err != nil {
			return dep.Dedup(allDeps), parentMap, err
		}

		var nextPending []parserlock.ActionRef
		for i := range deps {
			yml := actionYMLs[i]
			if yml == "" {
				continue
			}

			meta, parseErr := parserlock.ParseActionMeta(yml)
			if parseErr != nil || meta.Execution != parserlock.ExecComposite {
				continue
			}

			// parentMap is keyed at runner-download granularity (NWO@Ref =
			// Dependency.Key()). BFS traversal above is path-aware via
			// cacheKey/seen{} so we visit every sub-action's action.yml,
			// but the recorded edges flatten subpaths back into one node
			// per tarball — same model the runner uses.
			parentKey := deps[i].Key()
			for _, use := range meta.NestedUses {
				actionRef := parserlock.ParseActionRef(use)
				if actionRef == nil {
					continue
				}
				childKey := actionRef.NWO() + "@" + actionRef.Ref
				// Same-tarball edge: a composite whose `uses:` names another
				// subpath in its own repo+ref. At runner-download granularity
				// this is not a new transitive dependency (same tarball, same
				// SHA), so we must not record a parentMap edge — once subpaths
				// collapse to NWO@Ref it would become a self-edge. But we must
				// still descend into the sibling sub-action's action.yml: it
				// can pull in cross-repo transitive deps the parent never
				// references directly (e.g. nested-composite → simple-composite
				// → other-repo). The path-aware seen{} set keys on FullName@Ref
				// and so prevents re-resolving an exact self-reference, making
				// this enqueue loop-safe.
				if childKey == parentKey {
					nextPending = append(nextPending, *actionRef)
					continue
				}
				// Track all parents, deduplicating.
				parents := parentMap[childKey]
				found := false
				for _, p := range parents {
					if p == parentKey {
						found = true
						break
					}
				}
				if !found {
					parentMap[childKey] = append(parents, parentKey)
				}
				nextPending = append(nextPending, *actionRef)
			}
		}

		pending = nextPending
		depth++
	}

	return dep.Dedup(allDeps), parentMap, nil
}

// resolveWithActionYMLParallel resolves refs one-per-worker via the shared pool
// so the UI can show a stable [done/total] counter and per-worker status rows.
// Cached refs short-circuit without a worker.
//
// resolveDone and resolveTotal are rolling counters owned by ResolveAllRecursive
// that span every BFS depth, so a single non-jumping progress bar can cover
// direct + transitive resolution as one phase.
func (r *Resolver) resolveWithActionYMLParallel(ctx context.Context, refs []parserlock.ActionRef, depth int, resolveDone, resolveTotal *atomic.Int64) ([]dep.Dependency, []string, error) {
	type resolveResult struct {
		dep dep.Dependency
		yml string
		ok  bool
	}
	results := make([]resolveResult, len(refs))

	var uncachedIdx []int
	for i, ref := range refs {
		if entry, ok := r.cache.Get(cacheKey(ref)); ok {
			results[i] = resolveResult{dep: entry.dep, yml: entry.actionYML, ok: true}
		} else {
			uncachedIdx = append(uncachedIdx, i)
		}
	}

	flatten := func() ([]dep.Dependency, []string) {
		var deps []dep.Dependency
		var ymls []string
		for _, res := range results {
			if !res.ok {
				continue
			}
			deps = append(deps, res.dep)
			ymls = append(ymls, res.yml)
		}
		return deps, ymls
	}

	total := len(uncachedIdx)
	if total == 0 {
		deps, ymls := flatten()
		return deps, ymls, nil
	}

	// Grow the rolling resolve total by the new uncached refs at this depth.
	// First-wave size was preseeded by ResolveAllRecursive, so only count
	// deeper depths here to avoid double-counting.
	if depth > 0 {
		newTotal := resolveTotal.Add(int64(total))
		r.FireResolveProgress(int(resolveDone.Load()), int(newTotal))
	}

	type indexedRef struct {
		idx int
		ref parserlock.ActionRef
	}
	items := make([]indexedRef, len(uncachedIdx))
	for i, idx := range uncachedIdx {
		items[i] = indexedRef{idx: idx, ref: refs[idx]}
	}

	poolErr := pinpool.RunTyped(r.Pool, ctx, "Resolving actions",
		items,
		func(ir indexedRef) string { return ir.ref.NWO() + "@" + ir.ref.Ref },
		func(ctx context.Context, _ int, ir indexedRef) error {
			ref := ir.ref
			input := ghapi.ActionFileRequest{
				Owner: ref.Owner, Repo: ref.Repo,
				Path: ref.Path, Ref: ref.Ref,
			}
			res := r.gh.ResolveActionFiles(ctx, []ghapi.ActionFileRequest{input})
			if len(res) > 0 && res[0].Err == nil {
				d := dep.Dependency{
					NWO:  res[0].Owner + "/" + res[0].Repo,
					Path: res[0].Path,
					Ref:  res[0].Ref,
					SHA:  res[0].CommitOID,
				}
				key := cacheKey(ref)
				r.cache.Put(key, resolvedEntry{dep: d, actionYML: res[0].ActionYML})
				results[ir.idx] = resolveResult{dep: d, yml: res[0].ActionYML, ok: true}
			}
			done := resolveDone.Add(1)
			r.FireResolveProgress(int(done), int(resolveTotal.Load()))
			if len(res) > 0 && res[0].Err != nil {
				return fmt.Errorf("%s@%s: %w", input.NWO(), ref.Ref, res[0].Err)
			}
			return nil
		},
	)

	deps, ymls := flatten()
	return deps, ymls, poolErr
}

// selectLatestTag returns the highest semver tag from a list of tag names.
// Prefers vN (major-only) over vN.M.P when a matching major exists.
func selectLatestTag(tags []string) string {
	seen := make(map[string]struct{}, len(tags))
	bestMajor := -1
	bestMajorTag := ""
	bestVersion := [3]int{-1, -1, -1}
	bestVersionTag := ""
	bestFallback := ""

	for _, tag := range tags {
		tag = strings.TrimSpace(tag)
		if tag == "" {
			continue
		}
		if _, ok := seen[tag]; ok {
			continue
		}
		seen[tag] = struct{}{}
		if tag > bestFallback {
			bestFallback = tag
		}

		sv, ok := parserlock.ParseSemVer(tag)
		if !ok || !sv.IsStable() {
			continue
		}

		if sv.Raw == sv.MajorTag() && sv.Major > bestMajor {
			bestMajor = sv.Major
			bestMajorTag = tag
		}

		version := [3]int{sv.Major, sv.Minor, sv.Patch}
		if version[0] > bestVersion[0] ||
			(version[0] == bestVersion[0] && version[1] > bestVersion[1]) ||
			(version[0] == bestVersion[0] && version[1] == bestVersion[1] && version[2] > bestVersion[2]) {
			bestVersion = version
			bestVersionTag = tag
		}
	}

	if bestMajorTag != "" {
		return bestMajorTag
	}
	if bestVersionTag != "" {
		return bestVersionTag
	}
	return bestFallback
}
