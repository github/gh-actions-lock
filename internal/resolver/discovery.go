package resolver

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/cli/go-gh/v2/pkg/api"
	"github.com/github/gh-actions-pin/internal/cachekey"
	"github.com/github/gh-actions-pin/internal/lockfile"
)

// LatestRef returns the highest stable tag for an action repository.
func (r *Resolver) LatestRef(owner, repo string) (string, error) {
	key := cachekey.ForRepo(owner, repo)
	r.cacheMu.Lock()
	ref, ok := r.latestRefCache[key]
	r.cacheMu.Unlock()
	if ok {
		return ref, nil
	}

	query := `query($owner: String!, $name: String!) {
  repository(owner: $owner, name: $name) {
    refs(refPrefix: "refs/tags/", first: 100) {
      nodes {
        name
      }
    }
  }
}`

	var data struct {
		Repository *struct {
			Refs struct {
				Nodes []struct {
					Name string `json:"name"`
				} `json:"nodes"`
			} `json:"refs"`
		} `json:"repository"`
	}
	if err := r.client.Do(query, map[string]any{"owner": owner, "name": repo}, &data); err != nil {
		return "", err
	}
	if data.Repository == nil {
		return "", fmt.Errorf("%s: repository not found or not accessible", key)
	}

	tags := make([]string, 0, len(data.Repository.Refs.Nodes))
	for _, node := range data.Repository.Refs.Nodes {
		tags = append(tags, node.Name)
	}

	best := selectLatestTag(tags)
	if best == "" {
		return "", fmt.Errorf("%s: no tags available to upgrade", key)
	}

	r.cacheMu.Lock()
	r.latestRefCache[key] = best
	r.cacheMu.Unlock()
	return best, nil
}

func cacheKey(ref lockfile.ActionRef) cachekey.ActionRef {
	return cachekey.ForActionRef(ref.Owner, ref.Repo, ref.Path, ref.Ref)
}

// ResolveAllRecursive resolves action refs and recursively discovers transitive
// dependencies from composite actions by reading their action.yml via GraphQL.
// The returned ParentMap (child dep key → parent dep keys) is owned by the
// caller and safe to mutate or hold across concurrent resolver calls.
func (r *Resolver) ResolveAllRecursive(refs []lockfile.ActionRef) ([]lockfile.Dependency, ParentMap, error) {
	seen := make(map[cachekey.ActionRef]bool)
	var allDeps []lockfile.Dependency
	parentMap := make(ParentMap)

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
		r.cacheMu.Lock()
		for _, ref := range refs {
			if _, ok := r.cache[cacheKey(ref)]; !ok {
				firstWave++
			}
		}
		r.cacheMu.Unlock()
		resolveTotal.Store(int64(firstWave))
		r.fireResolveProgress(0, firstWave)
	}

	for len(pending) > 0 {
		if depth >= r.MaxRecursionDepth {
			return allDeps, parentMap, fmt.Errorf("composite action recursion exceeded max depth %d", r.MaxRecursionDepth)
		}

		var toResolve []lockfile.ActionRef
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

		var deps []lockfile.Dependency
		var actionYMLs []string
		var err error
		if r.WorkerProgressFn != nil {
			deps, actionYMLs, err = r.resolveWithActionYMLParallel(toResolve, depth, &resolveDone, &resolveTotal)
		} else {
			deps, actionYMLs, err = r.resolveWithActionYML(toResolve)
		}
		// Keep partial results: per-ref failures are surfaced via err, but
		// successful resolutions in `deps` should not be discarded — downstream
		// renderers degrade gracefully per-ref instead of marking everything
		// unresolved.
		allDeps = append(allDeps, deps...)
		if err != nil {
			return dedup(allDeps), parentMap, err
		}

		var nextPending []lockfile.ActionRef
		for i := range deps {
			yml := actionYMLs[i]
			if yml == "" {
				continue
			}

			meta, parseErr := lockfile.ParseActionMeta(yml)
			if parseErr != nil || meta.Execution != lockfile.ExecComposite {
				continue
			}

			// parentMap is keyed at runner-download granularity (NWO@Ref =
			// Dependency.Key()). BFS traversal above is path-aware via
			// cacheKey/seen{} so we visit every sub-action's action.yml,
			// but the recorded edges flatten subpaths back into one node
			// per tarball — same model the runner uses.
			parentKey := deps[i].Key()
			for _, use := range meta.NestedUses {
				actionRef := lockfile.ParseActionRef(use)
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

	return dedup(allDeps), parentMap, nil
}

func dedup(deps []lockfile.Dependency) []lockfile.Dependency {
	seen := make(map[string]bool)
	var out []lockfile.Dependency
	for _, dep := range deps {
		if !seen[dep.Key()] {
			seen[dep.Key()] = true
			out = append(out, dep)
		}
	}
	return out
}

// resolveWithActionYMLParallel resolves refs one-per-worker so the UI can show
// a stable [done/total] counter and per-worker "→ NWO@Ref" / "✓ NWO" rows.
// Cached refs short-circuit without a worker. Used when WorkerProgressFn is set.
//
// resolveDone and resolveTotal are rolling counters owned by ResolveAllRecursive
// that span every BFS depth, so a single non-jumping progress bar can cover
// direct + transitive resolution as one phase.
func (r *Resolver) resolveWithActionYMLParallel(refs []lockfile.ActionRef, depth int, resolveDone, resolveTotal *atomic.Int64) ([]lockfile.Dependency, []string, error) {
	type resolveResult struct {
		dep lockfile.Dependency
		yml string
		ok  bool
	}
	results := make([]resolveResult, len(refs))

	var uncachedIdx []int
	r.cacheMu.Lock()
	for i, ref := range refs {
		if entry, ok := r.cache[cacheKey(ref)]; ok {
			results[i] = resolveResult{dep: entry.dep, yml: entry.actionYML, ok: true}
		} else {
			uncachedIdx = append(uncachedIdx, i)
		}
	}
	r.cacheMu.Unlock()

	flatten := func() ([]lockfile.Dependency, []string) {
		var deps []lockfile.Dependency
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
		r.fireResolveProgress(int(resolveDone.Load()), int(newTotal))
	}

	limit := reachabilityConcurrency
	if limit > total {
		limit = total
	}
	slots := make(chan int, limit)
	for i := 0; i < limit; i++ {
		slots <- i
	}

	var (
		wg       sync.WaitGroup
		firstErr error
		errMu    sync.Mutex
	)

	for _, idx := range uncachedIdx {
		wg.Add(1)
		slot := <-slots
		go func(i int, ref lockfile.ActionRef, slot int) {
			defer wg.Done()
			defer func() { slots <- slot }()

			r.WorkerProgressFn(slot, "→ "+ref.NWO()+"@"+ref.Ref)

			deps, ymls, keys, err := r.resolveWithActionYMLBatch([]lockfile.ActionRef{ref})
			if err != nil {
				errMu.Lock()
				if firstErr == nil {
					firstErr = err
				}
				errMu.Unlock()
			}
			if len(deps) > 0 {
				r.cacheMu.Lock()
				for j, dep := range deps {
					r.cache[keys[j]] = resolvedEntry{dep: dep, actionYML: ymls[j]}
				}
				r.cacheMu.Unlock()
				results[i] = resolveResult{dep: deps[0], yml: ymls[0], ok: true}
			}

			done := resolveDone.Add(1)
			r.WorkerProgressFn(slot, "✓ "+ref.NWO())
			r.fireResolveProgress(int(done), int(resolveTotal.Load()))
		}(idx, refs[idx], slot)
	}
	wg.Wait()

	deps, ymls := flatten()
	return deps, ymls, firstErr
}

func (r *Resolver) resolveWithActionYML(refs []lockfile.ActionRef) ([]lockfile.Dependency, []string, error) {
	var allDeps []lockfile.Dependency
	var allYMLs []string
	var uncached []lockfile.ActionRef

	cachedIdx := make(map[int]bool)
	r.cacheMu.Lock()
	for i, ref := range refs {
		if _, ok := r.cache[cacheKey(ref)]; ok {
			cachedIdx[i] = true
		} else {
			uncached = append(uncached, ref)
		}
	}
	r.cacheMu.Unlock()

	var freshDeps []lockfile.Dependency
	var freshYMLs []string
	var freshKeys []cachekey.ActionRef
	var batchErr error
	for i := 0; i < len(uncached); i += MaxBatchSize {
		end := i + MaxBatchSize
		if end > len(uncached) {
			end = len(uncached)
		}
		deps, ymls, keys, err := r.resolveWithActionYMLBatch(uncached[i:end])
		// Keep partial batch results: per-ref failures shouldn't discard
		// successful resolutions from the same batch.
		freshDeps = append(freshDeps, deps...)
		freshYMLs = append(freshYMLs, ymls...)
		freshKeys = append(freshKeys, keys...)
		if err != nil {
			batchErr = err
			break
		}
	}

	// Store fresh resolutions in the cache keyed by cacheKey (FullName@Ref).
	// This preserves per-sub-action entries (e.g. actions/cache/save vs
	// actions/cache/restore) since their action.yml paths differ.
	r.cacheMu.Lock()
	for i, dep := range freshDeps {
		r.cache[freshKeys[i]] = resolvedEntry{dep: dep, actionYML: freshYMLs[i]}
	}
	r.cacheMu.Unlock()

	// Build allDeps from cached refs + freshly-resolved ones. Refs that failed
	// to resolve are simply absent — callers see them missing rather than
	// receiving an empty slice for the whole workflow.
	resolvedFresh := make(map[cachekey.ActionRef]int, len(freshDeps))
	for i := range freshDeps {
		resolvedFresh[freshKeys[i]] = i
	}
	for i, ref := range refs {
		key := cacheKey(ref)
		if cachedIdx[i] {
			r.cacheMu.Lock()
			entry := r.cache[key]
			r.cacheMu.Unlock()
			allDeps = append(allDeps, entry.dep)
			allYMLs = append(allYMLs, entry.actionYML)
			continue
		}
		if fi, ok := resolvedFresh[key]; ok {
			allDeps = append(allDeps, freshDeps[fi])
			allYMLs = append(allYMLs, freshYMLs[fi])
		}
	}

	return allDeps, allYMLs, batchErr
}

type repoResponse struct {
	NameWithOwner string `json:"nameWithOwner"`
	Object        *struct {
		OID  string `json:"oid"`
		File *struct {
			Object *struct {
				Text string `json:"text"`
			} `json:"object"`
		} `json:"file"`
		FileYAML *struct {
			Object *struct {
				Text string `json:"text"`
			} `json:"object"`
		} `json:"fileYaml"`
	} `json:"object"`
}

func (r *Resolver) resolveWithActionYMLBatch(refs []lockfile.ActionRef) ([]lockfile.Dependency, []string, []cachekey.ActionRef, error) {
	query, vars, aliasMap := buildResolveWithFileQuery(refs)

	var data map[string]json.RawMessage
	err := r.client.Do(query, vars, &data)
	var gqlErr *api.GraphQLError
	if err != nil {
		if !errors.As(err, &gqlErr) {
			return nil, nil, nil, err
		}
	}

	return parseResolveWithFileResponse(data, refs, aliasMap, gqlErr, r.hostname)
}

// buildResolveWithFileQuery emits a GraphQL query that resolves each ref's
// commit OID and action.{yml,yaml} blob in a single round-trip. All
// untrusted inputs (owner, repo, ref, path) are passed via GraphQL
// variables rather than interpolated with %q so that a YAML-supplied
// value like `"\n  malicious: query { viewer { login } }"` cannot escape
// the string literal and inject sibling fields.
func buildResolveWithFileQuery(refs []lockfile.ActionRef) (string, map[string]any, map[string]int) {
	aliasMap := make(map[string]int, len(refs))
	vars := make(map[string]any, len(refs)*5)

	var decl strings.Builder
	var body strings.Builder
	decl.WriteString("query(")
	body.WriteString(") {")

	for i, ref := range refs {
		alias := fmt.Sprintf("a%d", i)
		aliasMap[alias] = i

		ownerVar := fmt.Sprintf("owner%d", i)
		nameVar := fmt.Sprintf("name%d", i)
		exprVar := fmt.Sprintf("expr%d", i)
		ymlVar := fmt.Sprintf("yml%d", i)
		yamlVar := fmt.Sprintf("yaml%d", i)

		ymlPath := "action.yml"
		yamlPath := "action.yaml"
		if ref.Path != "" {
			ymlPath = ref.Path + "/action.yml"
			yamlPath = ref.Path + "/action.yaml"
		}

		vars[ownerVar] = ref.Owner
		vars[nameVar] = ref.Repo
		// Peel through annotated tags with `^{commit}`. Without it, an
		// annotated tag's `object(expression:)` returns a Tag object, not a
		// Commit — the `... on Commit` fragment doesn't match and `oid` comes
		// back empty. The peel is a no-op for branches, SHAs, and lightweight
		// tags, so we can apply it unconditionally.
		vars[exprVar] = ref.Ref + "^{commit}"
		vars[ymlVar] = ymlPath
		vars[yamlVar] = yamlPath

		if i > 0 {
			decl.WriteString(", ")
		}
		fmt.Fprintf(&decl, "$%s: String!, $%s: String!, $%s: String!, $%s: String!, $%s: String!",
			ownerVar, nameVar, exprVar, ymlVar, yamlVar)

		fmt.Fprintf(&body, " %s: repository(owner: $%s, name: $%s) {", alias, ownerVar, nameVar)
		body.WriteString(" nameWithOwner")
		fmt.Fprintf(&body, " object(expression: $%s) {", exprVar)
		body.WriteString(" ... on Commit { oid")
		fmt.Fprintf(&body, " file: file(path: $%s) { object { ... on Blob { text } } }", ymlVar)
		fmt.Fprintf(&body, " fileYaml: file(path: $%s) { object { ... on Blob { text } } }", yamlVar)
		body.WriteString(" }")
		body.WriteString(" }")
		body.WriteString(" }")
	}

	body.WriteString(" }")
	return decl.String() + body.String(), vars, aliasMap
}

func parseResolveWithFileResponse(data map[string]json.RawMessage, refs []lockfile.ActionRef, aliasMap map[string]int, gqlErr *api.GraphQLError, hostname string) ([]lockfile.Dependency, []string, []cachekey.ActionRef, error) {
	var deps []lockfile.Dependency
	var ymls []string
	var keys []cachekey.ActionRef
	var errs []string

	samlOwners := samlBlockedOwners(gqlErr, refs, aliasMap)

	for alias, idx := range aliasMap {
		ref := refs[idx]
		raw, ok := data[alias]
		if !ok {
			if samlOwners[ref.Owner] {
				errs = append(errs, fmt.Sprintf("%s@%s: %s", ref.NWO(), ref.Ref, ssoRequiredMessage(hostname, ref.Owner)))
				continue
			}
			errs = append(errs, fmt.Sprintf("%s@%s: not found in response", ref.NWO(), ref.Ref))
			continue
		}
		if string(raw) == "null" {
			if samlOwners[ref.Owner] {
				errs = append(errs, fmt.Sprintf("%s@%s: %s", ref.NWO(), ref.Ref, ssoRequiredMessage(hostname, ref.Owner)))
				continue
			}
			errs = append(errs, fmt.Sprintf("%s@%s: repository not found or not accessible", ref.NWO(), ref.Ref))
			continue
		}

		var repo repoResponse
		if err := json.Unmarshal(raw, &repo); err != nil {
			errs = append(errs, fmt.Sprintf("%s@%s: failed to parse: %v", ref.NWO(), ref.Ref, err))
			continue
		}

		if repo.Object == nil || repo.Object.OID == "" {
			errs = append(errs, fmt.Sprintf("%s@%s: ref %q does not exist", ref.NWO(), ref.Ref, ref.Ref))
			continue
		}

		dep := lockfile.Dependency{
			NWO:  ref.NWO(),
			Path: ref.Path,
			Ref:  ref.Ref,
			SHA:  repo.Object.OID,
		}
		deps = append(deps, dep)
		keys = append(keys, cacheKey(ref))

		var yml string
		if repo.Object.File != nil && repo.Object.File.Object != nil {
			yml = repo.Object.File.Object.Text
		} else if repo.Object.FileYAML != nil && repo.Object.FileYAML.Object != nil {
			yml = repo.Object.FileYAML.Object.Text
		}
		ymls = append(ymls, yml)
	}

	if len(errs) > 0 {
		return deps, ymls, keys, fmt.Errorf("resolution errors:\n  %s", strings.Join(errs, "\n  "))
	}

	return deps, ymls, keys, nil
}

// samlBlockedOwners returns the set of repository owners whose resolution
// failed an organization SAML SSO enforcement check. GitHub's GraphQL API
// reports these as per-alias FORBIDDEN errors carrying
// extensions.saml_failure == true alongside a null data entry for that
// alias; without this mapping the null entry is indistinguishable from a
// genuinely missing repository. The GraphQL alias in each error's Path is
// translated back to its owner via aliasMap + refs.
func samlBlockedOwners(gqlErr *api.GraphQLError, refs []lockfile.ActionRef, aliasMap map[string]int) map[string]bool {
	if gqlErr == nil {
		return nil
	}
	owners := make(map[string]bool)
	for _, e := range gqlErr.Errors {
		if blocked, _ := e.Extensions["saml_failure"].(bool); !blocked {
			continue
		}
		if len(e.Path) == 0 {
			continue
		}
		alias, ok := e.Path[0].(string)
		if !ok {
			continue
		}
		idx, ok := aliasMap[alias]
		if !ok || idx < 0 || idx >= len(refs) {
			continue
		}
		owners[refs[idx].Owner] = true
	}
	return owners
}

// ssoRequiredMessage builds an actionable error directing the user to
// authorize their token for an SSO-protected organization, rather than
// collapsing the failure into a generic "not found" message.
func ssoRequiredMessage(hostname, owner string) string {
	host := hostname
	if host == "" {
		host = "github.com"
	}
	return fmt.Sprintf("SSO authorization required: your token is not authorized for the %q organization (SAML enforcement). Authorize it at https://%s/orgs/%s/sso and retry", owner, host, owner)
}

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

		sv, ok := lockfile.ParseVersion(tag)
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
