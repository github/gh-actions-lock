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
	bcCache           map[string]bcCacheEntry // owner/repo/sha → cached branch_commits result
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
		bcCache:           make(map[string]bcCacheEntry),
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
// CheckReachability verifies that a dependency's commit is reachable from a
// branch in the repository, catching fork-network injection attacks.
//
// Uses the undocumented branch_commits endpoint (same approach as zizmor):
// resolve ref → SHA, then check if that SHA appears on any branch. Fork
// commits have branches=[] in the upstream repo.
//
// See: https://docs.zizmor.sh/audits/#impostor-commit
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
		r.reachCache[cacheKey] = result.Status
		return result
	}

	// For bare-SHA refs, check the SHA directly. For named refs, resolve
	// the ref to its current SHA first.
	checkRef := ref
	if lockfile.IsFullSHA(ref) {
		checkRef = sha
	}

	result.Status, result.Detail = r.branchCommitsCheck(owner, repo, checkRef)

	// Annotate bare-SHA refs with guidance to prefer tags.
	if lockfile.IsFullSHA(ref) {
		switch result.Status {
		case Reachable:
			result.Detail = "pinned to a bare SHA; commit is on a branch but origin cannot be verified at job runtime — prefer pinning to a tag"
		case Unreachable:
			result.Detail = "pinned to a bare SHA; commit is NOT on any branch — possible fork-network commit"
		case ReachabilityUnknown:
			result.Detail = fmt.Sprintf(
				"pinned to a bare SHA — unable to verify commit origin (%s); pin to a tag instead: https://github.com/%s/%s/releases",
				result.Detail, owner, repo)
		}
	}

	// Cache all results including Unknown — within a single CLI run, re-hitting
	// the same endpoint after a 429 just makes the rate limit worse.
	r.reachCache[cacheKey] = result.Status
	return result
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

// bcCacheEntry holds a cached branch_commits result. A nil resp with a non-empty
// errDetail indicates a failed request (429, 500, network error, decode error).
type bcCacheEntry struct {
	resp      *branchCommitsResponse
	errDetail string
}

// branchCommitsCheck uses the undocumented branch_commits endpoint (same
// approach as zizmor) to verify that a ref's current target is contained in
// at least one branch of the repository. Fork-network commits appear in the
// fork network's commit graph but have branches=[] in the upstream repo.
//
// Why not the documented Compare API? Compare requires a known base ref
// (e.g. refs/heads/main...{sha}), and for tags — the common pinning target —
// the tag itself is the attack vector (comparing a tampered tag against its
// own commit is trivially "identical"). You'd need to enumerate all branches
// and compare each one, which is O(branches) API calls.
//
// branch_commits answers "is this SHA on ANY branch?" in a single call.
// It wraps spokesd ListReferences with RefListOptions.contains under the hood.
//
// To stop relying on this undocumented endpoint, GitHub would need to expose
// a documented "commit contains" API (e.g. GET /repos/{owner}/{repo}/commits/{sha}/refs
// or a `contains` parameter on the existing list-branches endpoint).
// See: https://docs.zizmor.sh/audits/#impostor-commit
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

	// Step 2: check bcCache for this SHA to avoid duplicate HTTP calls.
	bcKey := owner + "/" + repo + "/" + commitResp.SHA
	if cached, ok := r.bcCache[bcKey]; ok {
		if cached.resp == nil {
			return ReachabilityUnknown, cached.errDetail
		}
		if len(cached.resp.Branches) == 0 {
			return Unreachable, fmt.Sprintf(
				"ref %s resolves to %s which is not on any branch in %s/%s — possible fork-network injection",
				ref, commitResp.SHA[:12], owner, repo)
		}
		branchNames := make([]string, len(cached.resp.Branches))
		for i, b := range cached.resp.Branches {
			branchNames[i] = b.Branch
		}
		return Reachable, fmt.Sprintf("commit is on branch(es): %s", strings.Join(branchNames, ", "))
	}

	// Step 3: call branch_commits on the web host (not the API host)
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
		detail := fmt.Sprintf("branch_commits request failed: %s", err)
		r.bcCache[bcKey] = bcCacheEntry{errDetail: detail}
		return ReachabilityUnknown, detail
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		detail := fmt.Sprintf("branch_commits returned HTTP %d", resp.StatusCode)
		r.bcCache[bcKey] = bcCacheEntry{errDetail: detail}
		return ReachabilityUnknown, detail
	}

	var bcResp branchCommitsResponse
	if err := json.NewDecoder(resp.Body).Decode(&bcResp); err != nil {
		detail := fmt.Sprintf("could not decode branch_commits response: %s", err)
		r.bcCache[bcKey] = bcCacheEntry{errDetail: detail}
		return ReachabilityUnknown, detail
	}
	r.bcCache[bcKey] = bcCacheEntry{resp: &bcResp}

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

// AncestryStatus represents whether a pinned SHA is a legitimate ancestor of the live SHA.
type AncestryStatus int

const (
	// AncestryConfirmed means the pinned SHA is an ancestor of the live SHA.
	AncestryConfirmed AncestryStatus = iota
	// AncestryNotAncestor means the pinned SHA is NOT an ancestor — possible forgery.
	AncestryNotAncestor
	// AncestryUnknown means the check could not be completed (rate limit, API error).
	AncestryUnknown
)

// compareResponse is the subset of the GitHub Compare API response we need.
type compareResponse struct {
	Status          string `json:"status"`
	MergeBaseCommit struct {
		SHA string `json:"sha"`
	} `json:"merge_base_commit"`
}

// CheckAncestry uses the Compare API to test whether pinnedSHA is an ancestor
// of liveSHA. This detects lockfile forgery: if someone injects a SHA that was
// never in the ref's lineage, merge_base(pinned, live) ≠ pinned.
func (r *Resolver) CheckAncestry(owner, repo, pinnedSHA, liveSHA string) (AncestryStatus, string) {
	path := fmt.Sprintf("repos/%s/%s/compare/%s...%s",
		owner, repo, url.PathEscape(pinnedSHA), url.PathEscape(liveSHA))

	var resp compareResponse
	err := r.restClient.Get(path, &resp)
	if err != nil {
		var httpErr *api.HTTPError
		if errors.As(err, &httpErr) {
			switch httpErr.StatusCode {
			case http.StatusNotFound:
				return AncestryNotAncestor, "commit not found in repository"
			case http.StatusConflict: // 409 — no common ancestor
				return AncestryNotAncestor, "no common ancestor between pinned and live SHA"
			case http.StatusForbidden, http.StatusTooManyRequests:
				detail := fmt.Sprintf("rate limited (HTTP %d)", httpErr.StatusCode)
				if reset := httpErr.Headers.Get("X-RateLimit-Reset"); reset != "" {
					detail += "; resets at " + reset
				}
				return AncestryUnknown, detail
			default:
				return AncestryUnknown, fmt.Sprintf("API error (HTTP %d): %s", httpErr.StatusCode, httpErr.Message)
			}
		}
		return AncestryUnknown, err.Error()
	}

	if strings.EqualFold(resp.MergeBaseCommit.SHA, pinnedSHA) {
		return AncestryConfirmed, fmt.Sprintf("pinned SHA is ancestor of live SHA (compare: %s)", resp.Status)
	}
	return AncestryNotAncestor, fmt.Sprintf("merge base is %s, not the pinned SHA — possible lockfile forgery or upstream history rewrite", resp.MergeBaseCommit.SHA[:12])
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
