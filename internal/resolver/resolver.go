// Package resolver resolves action refs to commit SHAs via the GitHub GraphQL
// API and recursively discovers transitive dependencies from composite actions.
package resolver

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/cli/go-gh/v2/pkg/api"
	"github.com/github/gh-actions-pin/internal/lockfile"
)

// ReachabilityStatus represents the result of a commit reachability check.
type ReachabilityStatus string

const (
	// Reachable means the SHA is confirmed on the ref's lineage.
	Reachable ReachabilityStatus = "reachable"
	// Unreachable means the SHA is confirmed NOT on the ref's lineage
	// (e.g. it exists only in a fork network).
	Unreachable ReachabilityStatus = "unreachable"
	// ReachabilityUnknown means the check could not be completed
	// (timeout, rate limit, API error).
	ReachabilityUnknown ReachabilityStatus = "unknown"
)

// DefaultMaxRecursionDepth matches the runner's composite action recursion limit.
const DefaultMaxRecursionDepth = 10

// MaxBatchSize is the maximum number of action refs per GraphQL query.
const MaxBatchSize = 20

type resolvedEntry struct {
	dep       lockfile.Dependency
	actionYML string
}

// ReachabilityResult holds the outcome of a single reachability check.
type ReachabilityResult struct {
	Owner  string
	Repo   string
	Ref    string
	SHA    string
	DepKey string // full dependency key (e.g. "actions/cache/save@v4")
	Status ReachabilityStatus
	Detail string // human-readable detail (e.g. compare status or error)
}

// Resolver resolves action refs to commit SHAs.
type Resolver struct {
	client            *api.GraphQLClient
	restClient        *api.RESTClient
	httpTransport     http.RoundTripper // raw transport for non-API requests (branch_commits)
	authToken         string            // auth token for non-API requests
	hostname          string
	MaxRecursionDepth int
	cache             map[string]resolvedEntry
	latestRefCache    map[string]string
	reachCache        map[string]ReachabilityStatus
	// parentMap tracks child dep key → parent dep keys from last ResolveAllRecursive call.
	parentMap map[string][]string
	// checkReachFn overrides the default REST-based reachability check (for tests).
	checkReachFn func(owner, repo, sha, ref string) (ReachabilityStatus, string)
}

// ParentMap returns the child dep key → parent dep keys mapping from the last ResolveAllRecursive call.
func (r *Resolver) ParentMap() map[string][]string {
	if r.parentMap == nil {
		return map[string][]string{}
	}
	return r.parentMap
}

// RekeyParentMap updates parentMap keys and values after dependency refs have
// been rewritten (e.g. tag narrowing v4 → v4.3.1, or PreserveRefs restoring
// a previous tag). Both child keys and parent values are remapped.
func (r *Resolver) RekeyParentMap(rewrites map[string]string) {
	if len(rewrites) == 0 || len(r.parentMap) == 0 {
		return
	}
	updated := make(map[string][]string, len(r.parentMap))
	for childKey, parents := range r.parentMap {
		newChild := childKey
		if rk, ok := rewrites[childKey]; ok {
			newChild = rk
		}
		newParents := make([]string, len(parents))
		for i, p := range parents {
			if rk, ok := rewrites[p]; ok {
				newParents[i] = rk
			} else {
				newParents[i] = p
			}
		}
		updated[newChild] = newParents
	}
	r.parentMap = updated
}

// New creates a resolver using the authenticated gh context.
func New(hostname string) (*Resolver, error) {
	return NewWithOptions(api.ClientOptions{Host: hostname})
}

// NewWithOptions creates a resolver using the provided client options.
func NewWithOptions(opts api.ClientOptions) (*Resolver, error) {
	hostname := opts.Host
	if hostname == "" {
		hostname = "github.com"
	}
	opts.Host = hostname

	// Wrap the transport with retry logic for transient 5xx/429 errors.
	if opts.Transport == nil {
		opts.Transport = newRetryTransport(http.DefaultTransport, 3)
	}

	client, err := api.NewGraphQLClient(opts)
	if err != nil {
		return nil, err
	}

	restClient, err := api.NewRESTClient(opts)
	if err != nil {
		return nil, err
	}

	return &Resolver{
		client:            client,
		restClient:        restClient,
		httpTransport:     opts.Transport,
		authToken:         opts.AuthToken,
		hostname:          hostname,
		MaxRecursionDepth: DefaultMaxRecursionDepth,
		cache:             make(map[string]resolvedEntry),
		latestRefCache:    make(map[string]string),
		reachCache:        make(map[string]ReachabilityStatus),
	}, nil
}

// NewWithTransport creates a resolver with a custom HTTP transport and a
// placeholder auth token. Intended for tests that stub HTTP responses.
func NewWithTransport(hostname string, transport http.RoundTripper) (*Resolver, error) {
	return NewWithOptions(api.ClientOptions{
		AuthToken:    "test-placeholder-token",
		Host:         hostname,
		Transport:    transport,
		LogIgnoreEnv: true,
	})
}

// Hostname returns the GitHub host the resolver is targeting.
func (r *Resolver) Hostname() string {
	return r.hostname
}

// SetCheckReachabilityFunc overrides the default REST-based reachability check.
// Intended for tests.
func (r *Resolver) SetCheckReachabilityFunc(fn func(owner, repo, sha, ref string) (ReachabilityStatus, string)) {
	r.checkReachFn = fn
}

// CheckReachability verifies that a resolved SHA is on the lineage of the
// given ref within the repository. This catches fork-network injection where
// a SHA exists in GitHub's shared object store but is not actually part of
// the canonical repository's history.
//
// Uses the GitHub Compare API and checks merge_base identity:
//   - merge_base == pinnedSHA → Reachable (SHA is a true ancestor of ref)
//   - merge_base != pinnedSHA → Unreachable (fork/imposter commit)
//   - 404 (no common ancestor or not found) → Unreachable
//   - 403/429 (rate limit) or other error → Unknown
//
// When ref is itself a raw SHA (the "uses: owner/repo@SHA" anti-pattern),
// the compare becomes {sha}...{sha} which trivially returns "identical" and
// cannot detect fork commits. In this case, a warning is returned instead.
func (r *Resolver) CheckReachability(owner, repo, sha, ref string) ReachabilityResult {
	result := ReachabilityResult{
		Owner: owner,
		Repo:  repo,
		Ref:   ref,
		SHA:   sha,
	}

	cacheKey := owner + "/" + repo + "/" + sha + "/" + ref
	if status, ok := r.reachCache[cacheKey]; ok {
		result.Status = status
		result.Detail = "cached"
		return result
	}

	// Allow tests to inject a fake implementation
	if r.checkReachFn != nil {
		result.Status, result.Detail = r.checkReachFn(owner, repo, sha, ref)
		if result.Status != ReachabilityUnknown {
			r.reachCache[cacheKey] = result.Status
		}
		return result
	}

	// SHA-as-ref: compare/{sha}...{sha} is trivially identical and cannot
	// detect fork commits. Instead, check against the repo's default branch
	// which proves the commit exists in the canonical repo's history. Note:
	// Note: runtime enforcement for bare-SHA refs is TBD — it's expensive.
	if lockfile.IsFullSHA(ref) {
		status, detail := r.apiReachabilityCheck(owner, repo, sha, "HEAD")
		switch status {
		case Reachable:
			result.Status = Reachable
			result.Detail = "pinned to a bare SHA; commit is reachable from default branch but origin cannot be verified at job runtime — prefer pinning to a tag"
			r.reachCache[cacheKey] = result.Status
		case Unreachable:
			result.Status = Unreachable
			result.Detail = "pinned to a bare SHA; commit is NOT reachable from default branch — possible fork-network commit"
			r.reachCache[cacheKey] = result.Status
		default:
			result.Status = ReachabilityUnknown
			result.Detail = fmt.Sprintf(
				"pinned to a bare SHA — unable to verify commit origin (%s); pin to a tag instead: https://github.com/%s/%s/releases",
				detail, owner, repo)
		}
		return result
	}

	status, detail := r.apiReachabilityCheck(owner, repo, sha, ref)
	result.Status = status
	result.Detail = detail

	// When the ancestry check passes for a named ref, verify the ref's
	// current target is actually contained in a branch of this repository
	// — not just reachable through the fork network. The Compare API
	// transparently spans the fork network, so a fork commit that preserves
	// lineage (normal git commit with an upstream parent) passes the
	// ancestry check. We catch this using the undocumented branch_commits
	// endpoint (same approach as zizmor): if the resolved SHA has no
	// branches in this repo, it's a fork-network commit.
	//
	// See: https://docs.zizmor.sh/audits/#impostor-commit
	if status == Reachable {
		containStatus, containDetail := r.branchCommitsCheck(owner, repo, ref)
		if containStatus == Unreachable {
			result.Status = Unreachable
			result.Detail = containDetail
		}
		// If containment check is Unknown (rate limit etc), keep the
		// Reachable result — we don't downgrade on uncertainty.
	}

	if result.Status != ReachabilityUnknown {
		r.reachCache[cacheKey] = result.Status
	}
	return result
}

// compareResponse is the subset of the GitHub Compare API response we need.
type compareResponse struct {
	MergeBaseCommit struct {
		SHA string `json:"sha"`
	} `json:"merge_base_commit"`
	Status string `json:"status"`
}

// apiReachabilityCheck uses the GitHub Compare API to verify that sha is an
// ancestor of ref. The key insight: merge_base(ancestor, descendant) == ancestor.
// If the merge_base is NOT the pinned SHA, the commit lives on the fork network.
func (r *Resolver) apiReachabilityCheck(owner, repo, sha, ref string) (ReachabilityStatus, string) {
	path := fmt.Sprintf("repos/%s/%s/compare/%s...%s",
		owner, repo, url.PathEscape(sha), url.PathEscape(ref))

	var resp compareResponse
	err := r.restClient.Get(path, &resp)
	if err != nil {
		var httpErr *api.HTTPError
		if errors.As(err, &httpErr) {
			switch {
			case httpErr.StatusCode == http.StatusNotFound:
				return Unreachable, "no common ancestor or commit not found"
			case httpErr.StatusCode == http.StatusForbidden || httpErr.StatusCode == http.StatusTooManyRequests:
				detail := fmt.Sprintf("rate limited (HTTP %d)", httpErr.StatusCode)
				if reset := httpErr.Headers.Get("X-RateLimit-Reset"); reset != "" {
					detail += "; resets at " + reset
				}
				return ReachabilityUnknown, detail
			default:
				return ReachabilityUnknown, fmt.Sprintf("API error (HTTP %d): %s", httpErr.StatusCode, httpErr.Message)
			}
		}
		return ReachabilityUnknown, err.Error()
	}

	if resp.MergeBaseCommit.SHA == sha {
		return Reachable, "ancestor of " + ref + " (compare: " + resp.Status + ")"
	}
	return Unreachable, fmt.Sprintf("merge base is %s, not the pinned SHA — likely a fork-network commit", resp.MergeBaseCommit.SHA[:12])
}

// branchCommitsResponse is the subset of the undocumented branch_commits
// endpoint response we need. The endpoint lives on the web host (not the
// API host) and returns which branches and tags contain a given commit.
type branchCommitsResponse struct {
	Branches []struct {
		Branch string `json:"branch"`
	} `json:"branches"`
	Tags []string `json:"tags"`
}

// branchCommitsCheck uses the undocumented branch_commits endpoint (same
// approach as zizmor) to verify that a ref's current target is contained in
// at least one branch of the repository. Fork-network commits appear in the
// fork network's commit graph but have branches=[] in the upstream repo.
//
// Flow: resolve ref → SHA via REST API, then GET /{owner}/{repo}/branch_commits/{sha}.
func (r *Resolver) branchCommitsCheck(owner, repo, ref string) (ReachabilityStatus, string) {
	// Step 1: resolve ref to its current SHA
	var commitResp struct {
		SHA string `json:"sha"`
	}
	commitPath := fmt.Sprintf("repos/%s/%s/commits/%s", owner, repo, url.PathEscape(ref))
	if err := r.restClient.Get(commitPath, &commitResp); err != nil {
		return ReachabilityUnknown, fmt.Sprintf("could not resolve ref %s: %s", ref, err)
	}
	if commitResp.SHA == "" {
		return ReachabilityUnknown, fmt.Sprintf("could not resolve ref %s: empty SHA", ref)
	}

	// Step 2: call branch_commits on the web host (not the API host)
	webHost := r.hostname
	bcURL := fmt.Sprintf("https://%s/%s/%s/branch_commits/%s", webHost, owner, repo, commitResp.SHA)

	req, err := http.NewRequest("GET", bcURL, nil)
	if err != nil {
		return ReachabilityUnknown, fmt.Sprintf("could not build branch_commits request: %s", err)
	}
	req.Header.Set("Accept", "application/json")
	if r.authToken != "" {
		req.Header.Set("Authorization", "token "+r.authToken)
	}

	transport := r.httpTransport
	if transport == nil {
		transport = http.DefaultTransport
	}
	resp, err := transport.RoundTrip(req)
	if err != nil {
		return ReachabilityUnknown, fmt.Sprintf("branch_commits request failed: %s", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return ReachabilityUnknown, fmt.Sprintf("branch_commits returned HTTP %d", resp.StatusCode)
	}

	var bcResp branchCommitsResponse
	if err := json.NewDecoder(resp.Body).Decode(&bcResp); err != nil {
		return ReachabilityUnknown, fmt.Sprintf("could not decode branch_commits response: %s", err)
	}

	if len(bcResp.Branches) == 0 {
		return Unreachable, fmt.Sprintf(
			"ref %s resolves to %s which is not on any branch in %s/%s — possible fork-network injection",
			ref, commitResp.SHA[:12], owner, repo)
	}

	branchNames := make([]string, len(bcResp.Branches))
	for i, b := range bcResp.Branches {
		branchNames[i] = b.Branch
	}
	return Reachable, fmt.Sprintf("commit is on branch(es): %s", strings.Join(branchNames, ", "))
}

// CheckReachabilityAll runs reachability checks on a batch of dependencies,
// deduplicating by owner/repo/sha/ref.
func (r *Resolver) CheckReachabilityAll(deps []lockfile.Dependency) []ReachabilityResult {
	var results []ReachabilityResult
	seen := make(map[string]bool)

	for _, dep := range deps {
		owner, repo := dep.OwnerRepo()
		if owner == "" {
			continue
		}

		key := dep.NWO + "/" + dep.SHA + "/" + dep.Ref
		if seen[key] {
			continue
		}
		seen[key] = true

		result := r.CheckReachability(owner, repo, dep.SHA, dep.Ref)
		result.DepKey = dep.Key()
		results = append(results, result)
	}

	return results
}

// LatestRef returns the highest stable tag for an action repository.
func (r *Resolver) LatestRef(owner, repo string) (string, error) {
	key := owner + "/" + repo
	if ref, ok := r.latestRefCache[key]; ok {
		return ref, nil
	}

	query := fmt.Sprintf(`query {
  repository(owner: %q, name: %q) {
    refs(refPrefix: "refs/tags/", first: 100) {
      nodes {
        name
      }
    }
  }
}`, owner, repo)

	var data struct {
		Repository *struct {
			Refs struct {
				Nodes []struct {
					Name string `json:"name"`
				} `json:"nodes"`
			} `json:"refs"`
		} `json:"repository"`
	}
	if err := r.client.Do(query, nil, &data); err != nil {
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

	r.latestRefCache[key] = best
	return best, nil
}

func cacheKey(ref lockfile.ActionRef) string {
	return ref.FullName() + "@" + ref.Ref
}

// ResolveAllRecursive resolves action refs and recursively discovers transitive
// dependencies from composite actions by reading their action.yml via GraphQL.
func (r *Resolver) ResolveAllRecursive(refs []lockfile.ActionRef) ([]lockfile.Dependency, error) {
	seen := make(map[string]bool)
	var allDeps []lockfile.Dependency
	r.parentMap = make(map[string][]string)

	pending := refs
	depth := 0

	for len(pending) > 0 {
		if depth >= r.MaxRecursionDepth {
			return allDeps, fmt.Errorf("composite action recursion exceeded max depth %d", r.MaxRecursionDepth)
		}

		var toResolve []lockfile.ActionRef
		for _, ref := range pending {
			key := ref.FullName() + "@" + ref.Ref
			if !seen[key] {
				seen[key] = true
				toResolve = append(toResolve, ref)
			}
		}

		if len(toResolve) == 0 {
			break
		}

		deps, actionYMLs, err := r.resolveWithActionYML(toResolve)
		if err != nil {
			return allDeps, err
		}
		allDeps = append(allDeps, deps...)

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

			parentKey := deps[i].Key()
			for _, use := range meta.NestedUses {
				if actionRef := lockfile.ParseActionRef(use); actionRef != nil {
					childKey := actionRef.FullName() + "@" + actionRef.Ref
					// Track all parents, deduplicating.
					parents := r.parentMap[childKey]
					found := false
					for _, p := range parents {
						if p == parentKey {
							found = true
							break
						}
					}
					if !found {
						r.parentMap[childKey] = append(parents, parentKey)
					}
					nextPending = append(nextPending, *actionRef)
				}
			}
		}

		pending = nextPending
		depth++
	}

	return dedup(allDeps), nil
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

func (r *Resolver) resolveWithActionYML(refs []lockfile.ActionRef) ([]lockfile.Dependency, []string, error) {
	var allDeps []lockfile.Dependency
	var allYMLs []string
	var uncached []lockfile.ActionRef

	cachedIdx := make(map[int]bool)
	for i, ref := range refs {
		if _, ok := r.cache[cacheKey(ref)]; ok {
			cachedIdx[i] = true
		} else {
			uncached = append(uncached, ref)
		}
	}

	var freshDeps []lockfile.Dependency
	var freshYMLs []string
	for i := 0; i < len(uncached); i += MaxBatchSize {
		end := i + MaxBatchSize
		if end > len(uncached) {
			end = len(uncached)
		}
		deps, ymls, err := r.resolveWithActionYMLBatch(uncached[i:end])
		if err != nil {
			return allDeps, allYMLs, err
		}
		freshDeps = append(freshDeps, deps...)
		freshYMLs = append(freshYMLs, ymls...)
	}

	for i, dep := range freshDeps {
		r.cache[dep.Key()] = resolvedEntry{dep: dep, actionYML: freshYMLs[i]}
	}

	freshIdx := 0
	for i, ref := range refs {
		if cachedIdx[i] {
			entry := r.cache[cacheKey(ref)]
			allDeps = append(allDeps, entry.dep)
			allYMLs = append(allYMLs, entry.actionYML)
		} else {
			allDeps = append(allDeps, freshDeps[freshIdx])
			allYMLs = append(allYMLs, freshYMLs[freshIdx])
			freshIdx++
		}
	}

	return allDeps, allYMLs, nil
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

func (r *Resolver) resolveWithActionYMLBatch(refs []lockfile.ActionRef) ([]lockfile.Dependency, []string, error) {
	query, aliasMap := buildResolveWithFileQuery(refs)

	var data map[string]json.RawMessage
	err := r.client.Do(query, nil, &data)
	if err != nil {
		var gqlErr *api.GraphQLError
		if !errors.As(err, &gqlErr) {
			return nil, nil, err
		}
	}

	return parseResolveWithFileResponse(data, refs, aliasMap)
}

func buildResolveWithFileQuery(refs []lockfile.ActionRef) (string, map[string]int) {
	aliasMap := make(map[string]int)
	var sb strings.Builder
	sb.WriteString("query {")

	for i, ref := range refs {
		alias := fmt.Sprintf("a%d", i)
		aliasMap[alias] = i

		ymlPath := "action.yml"
		yamlPath := "action.yaml"
		if ref.Path != "" {
			ymlPath = ref.Path + "/action.yml"
			yamlPath = ref.Path + "/action.yaml"
		}

		fmt.Fprintf(&sb, " %s: repository(owner: %q, name: %q) {", alias, ref.Owner, ref.Repo)
		sb.WriteString(" nameWithOwner")
		fmt.Fprintf(&sb, " object(expression: %q) {", ref.Ref)
		sb.WriteString(" ... on Commit { oid")
		fmt.Fprintf(&sb, " file: file(path: %q) { object { ... on Blob { text } } }", ymlPath)
		fmt.Fprintf(&sb, " fileYaml: file(path: %q) { object { ... on Blob { text } } }", yamlPath)
		sb.WriteString(" }")
		sb.WriteString(" }")
		sb.WriteString(" }")
	}

	sb.WriteString(" }")
	return sb.String(), aliasMap
}

func parseResolveWithFileResponse(data map[string]json.RawMessage, refs []lockfile.ActionRef, aliasMap map[string]int) ([]lockfile.Dependency, []string, error) {
	var deps []lockfile.Dependency
	var ymls []string
	var errs []string

	for alias, idx := range aliasMap {
		ref := refs[idx]
		raw, ok := data[alias]
		if !ok {
			errs = append(errs, fmt.Sprintf("%s@%s: not found in response", ref.NWO(), ref.Ref))
			continue
		}
		if string(raw) == "null" {
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
			NWO: ref.FullName(),
			Ref: ref.Ref,
			SHA: repo.Object.OID,
		}
		deps = append(deps, dep)

		var yml string
		if repo.Object.File != nil && repo.Object.File.Object != nil {
			yml = repo.Object.File.Object.Text
		} else if repo.Object.FileYAML != nil && repo.Object.FileYAML.Object != nil {
			yml = repo.Object.FileYAML.Object.Text
		}
		ymls = append(ymls, yml)
	}

	if len(errs) > 0 {
		return deps, ymls, fmt.Errorf("resolution errors:\n  %s", strings.Join(errs, "\n  "))
	}

	return deps, ymls, nil
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

		sv, ok := lockfile.ParseSemver(tag)
		if !ok || !sv.IsStable() {
			continue
		}

		if sv.Raw == sv.MajorTag() && sv.Major > bestMajor {
			bestMajor = sv.Major
			bestMajorTag = tag
		}

		version := sv.Version()
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
