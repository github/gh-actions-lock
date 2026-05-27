// Package resolver resolves action refs to commit SHAs via the GitHub GraphQL
// API and recursively discovers transitive dependencies from composite actions.
package resolver

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
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
	hostname          string
	MaxRecursionDepth int
	cache             map[string]resolvedEntry
	latestRefCache    map[string]string
	reachCache        map[string]ReachabilityStatus
	branchListCache   map[string][]branchHead // "owner/repo" → cached branch list
	tagListCache      map[string][]tagEntry   // "owner/repo" → cached tag list
	repoIDsCache      map[string][2]int64     // owner/repo → [ownerID, repoID]
	defaultBranchCache map[string]string      // "owner/repo" → cached default branch name ("" = lookup failed / unknown)
	// parentMap tracks child dep key → parent dep keys from last ResolveAllRecursive call.
	parentMap map[string][]string
	// checkReachFn overrides the default branch-discovery check (for tests).
	checkReachFn func(owner, repo, sha, ref string) (ReachabilityStatus, string)

	// DisableReachability skips the CheckReachability security audit entirely.
	// When true, CheckReachability returns ReachabilityUnknown immediately.
	// Branch discovery for lockfile metadata (NormalizeContaining) is unaffected.
	DisableReachability bool

	// ProgressFn, if set, is invoked with a short human-readable status
	// string when a slow operation begins (per-dependency discovery,
	// per-branch Compare scan). Used by the CLI to update an active
	// spinner so users get feedback during long scans. Safe to leave nil.
	ProgressFn func(detail string)
}

// progress fires ProgressFn with a formatted message when set.
func (r *Resolver) progress(format string, args ...any) {
	if r.ProgressFn == nil {
		return
	}
	r.ProgressFn(fmt.Sprintf(format, args...))
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
		hostname:          hostname,
		MaxRecursionDepth: DefaultMaxRecursionDepth,
		cache:             make(map[string]resolvedEntry),
		latestRefCache:    make(map[string]string),
		reachCache:        make(map[string]ReachabilityStatus),
		branchListCache:   make(map[string][]branchHead),
		tagListCache:      make(map[string][]tagEntry),
		repoIDsCache:      make(map[string][2]int64),
		defaultBranchCache: make(map[string]string),
	}, nil
}

// RepoIDs returns the numeric owner ID and repo ID for a NWO, querying
// the GitHub REST API on cache miss. Results are cached for the lifetime of
// the resolver.
func (r *Resolver) RepoIDs(owner, repo string) (int64, int64, error) {
	key := owner + "/" + repo
	if ids, ok := r.repoIDsCache[key]; ok {
		return ids[0], ids[1], nil
	}
	var resp struct {
		ID    int64 `json:"id"`
		Owner struct {
			ID int64 `json:"id"`
		} `json:"owner"`
	}
	path := fmt.Sprintf("repos/%s/%s", owner, repo)
	if err := r.restClient.Get(path, &resp); err != nil {
		return 0, 0, fmt.Errorf("fetching %s: %w", path, err)
	}
	if resp.ID == 0 || resp.Owner.ID == 0 {
		return 0, 0, fmt.Errorf("%s returned zero IDs (owner=%d repo=%d)", path, resp.Owner.ID, resp.ID)
	}
	r.repoIDsCache[key] = [2]int64{resp.Owner.ID, resp.ID}
	return resp.Owner.ID, resp.ID, nil
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

// HasReachabilityFunc reports whether a test reachability function has been injected.
func (r *Resolver) HasReachabilityFunc() bool {
	return r.checkReachFn != nil
}

// CheckReachability verifies that the pinned SHA is reachable from at least
// one branch of owner/repo, using the documented REST APIs (list-branches +
// compare for ancestry). This catches fork-network injection where a SHA
// exists in GitHub's shared object store but is not part of the canonical
// repository's history.
//
// DisableReachability bypasses the check entirely (useful for GHES instances
// or environments where the additional API calls are undesirable). When
// bypassed, the result status is ReachabilityUnknown. Branch discovery for
// lockfile metadata (NormalizeContaining) is unaffected by this flag.
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

	// Allow tests to inject a fake implementation.
	if r.checkReachFn != nil {
		result.Status, result.Detail = r.checkReachFn(owner, repo, sha, ref)
		r.reachCache[cacheKey] = result.Status
		return result
	}

	if r.DisableReachability {
		result.Status = ReachabilityUnknown
		result.Detail = "reachability check disabled via config"
		r.reachCache[cacheKey] = result.Status
		return result
	}

	branches, err := r.listBranches(owner, repo)
	if err != nil {
		result.Status = ReachabilityUnknown
		result.Detail = fmt.Sprintf("could not list branches for %s/%s: %s", owner, repo, err)
		r.reachCache[cacheKey] = result.Status
		return result
	}
	protectedBranches := make([]branchHead, 0, len(branches))
	for _, b := range branches {
		if b.Protected {
			protectedBranches = append(protectedBranches, b)
		}
	}
	defaultBranch := r.getDefaultBranch(owner, repo)

	// Walk protected first, then fall back to all branches.
	foundBranch := r.reachabilityScan(owner, repo, sha, ref, protectedBranches, defaultBranch)
	if foundBranch == "" {
		foundBranch = r.reachabilityScan(owner, repo, sha, ref, branches, defaultBranch)
	}

	if foundBranch != "" {
		result.Status = Reachable
		if lockfile.IsFullSHA(ref) {
			result.Detail = fmt.Sprintf("pinned to a bare SHA; commit is on branch %s but origin cannot be verified at job runtime — prefer pinning to a tag", foundBranch)
		} else {
			result.Detail = fmt.Sprintf("commit is on branch %s", foundBranch)
		}
	} else {
		result.Status = Unreachable
		if lockfile.IsFullSHA(ref) {
			result.Detail = "pinned to a bare SHA; commit is NOT on any branch — possible fork-network commit"
		} else {
			result.Detail = fmt.Sprintf("commit %s not found on any branch of %s/%s — possible fork-network injection",
				shortSha(sha), owner, repo)
		}
	}

	r.reachCache[cacheKey] = result.Status
	return result
}

// reachabilityScan walks candidates (in orderedBranches tier order),
// returning the first branch whose HEAD is sha or whose lineage contains
// sha as an ancestor via the Compare API. Compare errors (e.g.
// rate-limited, 5xx) are non-fatal: the branch is skipped.
func (r *Resolver) reachabilityScan(owner, repo, sha, ref string, candidates []branchHead, defaultBranch string) string {
	if len(candidates) == 0 {
		return ""
	}
	// Fast path: exact HEAD match.
	for _, b := range candidates {
		if strings.EqualFold(b.SHA, sha) {
			return b.Name
		}
	}
	// Slow path: ancestry via Compare API in tier order.
	for _, b := range orderedBranches(candidates, ref, defaultBranch) {
		r.progress("scanning %s/%s branch %s", owner, repo, b.Name)
		ok, err := r.branchContainsCommit(owner, repo, sha, b.SHA)
		if err != nil {
			continue
		}
		if ok {
			return b.Name
		}
	}
	return ""
}

// branchHead holds a branch name, the SHA of its HEAD commit, and whether
// the branch has branch-protection rules enabled in the upstream repo.
type branchHead struct {
	Name      string
	SHA       string
	Protected bool
}

// tagEntry holds a tag name and the commit SHA it points at.
type tagEntry struct {
	Name string
	SHA  string
}

// listBranches returns all branches with their HEAD SHAs for a repo, using
// the documented REST API (GET /repos/{owner}/{repo}/branches). Results are
// cached per owner/repo. Paginates up to 3 pages (300 branches) to bound
// the number of API calls.
func (r *Resolver) listBranches(owner, repo string) ([]branchHead, error) {
	key := owner + "/" + repo
	if cached, ok := r.branchListCache[key]; ok {
		return cached, nil
	}
	var all []branchHead
	for page := 1; page <= 3; page++ {
		path := fmt.Sprintf("repos/%s/%s/branches?per_page=100&page=%d",
			url.PathEscape(owner), url.PathEscape(repo), page)
		var resp []struct {
			Name   string `json:"name"`
			Commit struct {
				SHA string `json:"sha"`
			} `json:"commit"`
			Protected bool `json:"protected"`
		}
		if err := r.restClient.Get(path, &resp); err != nil {
			return nil, fmt.Errorf("listing branches for %s/%s: %w", owner, repo, err)
		}
		for _, b := range resp {
			all = append(all, branchHead{Name: b.Name, SHA: b.Commit.SHA, Protected: b.Protected})
		}
		if len(resp) < 100 {
			break // last page
		}
	}
	r.branchListCache[key] = all
	return all, nil
}

// listTagsForRepo returns all tags with their commit SHAs for a repo, using
// the documented REST API (GET /repos/{owner}/{repo}/tags). Results are
// cached per owner/repo.
func (r *Resolver) listTagsForRepo(owner, repo string) ([]tagEntry, error) {
	key := owner + "/" + repo
	if cached, ok := r.tagListCache[key]; ok {
		return cached, nil
	}
	path := fmt.Sprintf("repos/%s/%s/tags?per_page=100",
		url.PathEscape(owner), url.PathEscape(repo))
	var resp []struct {
		Name   string `json:"name"`
		Commit struct {
			SHA string `json:"sha"`
		} `json:"commit"`
	}
	if err := r.restClient.Get(path, &resp); err != nil {
		return nil, fmt.Errorf("listing tags for %s/%s: %w", owner, repo, err)
	}
	tags := make([]tagEntry, 0, len(resp))
	for _, t := range resp {
		tags = append(tags, tagEntry{Name: t.Name, SHA: t.Commit.SHA})
	}
	r.tagListCache[key] = tags
	return tags, nil
}

// branchContainsCommit reports whether sha is on the lineage of branchHeadSHA
// using the documented Compare API. A 404 or 422 response (unrelated histories
// or missing commit) is treated as a non-error false return.
func (r *Resolver) branchContainsCommit(owner, repo, sha, branchHeadSHA string) (bool, error) {
	if strings.EqualFold(sha, branchHeadSHA) {
		return true, nil
	}
	path := fmt.Sprintf("repos/%s/%s/compare/%s...%s",
		owner, repo, url.PathEscape(sha), url.PathEscape(branchHeadSHA))
	var resp compareResponse
	if err := r.restClient.Get(path, &resp); err != nil {
		var httpErr *api.HTTPError
		if errors.As(err, &httpErr) &&
			(httpErr.StatusCode == http.StatusNotFound || httpErr.StatusCode == http.StatusUnprocessableEntity) {
			return false, nil // unrelated histories or missing commit
		}
		return false, err
	}
	// sha is an ancestor of branchHeadSHA iff the merge base IS sha.
	return strings.EqualFold(resp.MergeBaseCommit.SHA, sha), nil
}

// orderedBranches returns branches in tiered order so the most
// trust-bearing candidates are compared first when walking the slow path:
//
//  1. hintRef (the ref the user literally wrote in the workflow)
//  2. defaultBranch (e.g. main / master)
//  3. protected branches (branch-protection rules in upstream), lex within tier
//  4. unprotected branches, lex within tier
//
// Tiers 1 and 2 are skipped when empty or absent from branches.
func orderedBranches(branches []branchHead, hintRef, defaultBranch string) []branchHead {
	priority := make(map[string]bool)
	var result []branchHead
	for _, name := range []string{hintRef, defaultBranch} {
		if name == "" || priority[name] {
			continue
		}
		for _, b := range branches {
			if b.Name == name {
				result = append(result, b)
				priority[b.Name] = true
				break
			}
		}
	}
	protected := make([]branchHead, 0, len(branches))
	unprotected := make([]branchHead, 0, len(branches))
	for _, b := range branches {
		if priority[b.Name] {
			continue
		}
		if b.Protected {
			protected = append(protected, b)
		} else {
			unprotected = append(unprotected, b)
		}
	}
	sort.Slice(protected, func(i, j int) bool { return protected[i].Name < protected[j].Name })
	sort.Slice(unprotected, func(i, j int) bool { return unprotected[i].Name < unprotected[j].Name })
	result = append(result, protected...)
	result = append(result, unprotected...)
	return result
}

// getDefaultBranch returns the repo's default branch name (e.g. "main"),
// looked up via GET /repos/{owner}/{repo} and cached for the lifetime of
// the resolver. On lookup failure an empty string is cached so subsequent
// callers don't retry.
func (r *Resolver) getDefaultBranch(owner, repo string) string {
	key := owner + "/" + repo
	if name, ok := r.defaultBranchCache[key]; ok {
		return name
	}
	var resp struct {
		DefaultBranch string `json:"default_branch"`
	}
	path := fmt.Sprintf("repos/%s/%s", url.PathEscape(owner), url.PathEscape(repo))
	if err := r.restClient.Get(path, &resp); err != nil {
		r.defaultBranchCache[key] = ""
		return ""
	}
	r.defaultBranchCache[key] = resp.DefaultBranch
	return resp.DefaultBranch
}

// DiscoverContaining returns (tag, branch) for sha in owner/repo, using
// the documented REST APIs: list-branches (with compare for ancestry) and
// list-tags. Results are cached for the lifetime of the resolver.
//
// Selection rules:
//   - If hintRef is one of the discovered tags it wins; otherwise the first
//     tag (lexicographic) is picked. tag may be empty when no tag points at sha.
//   - Protected branches are searched first (hintRef → default → lex).
//     If no protected branch contains sha, the search falls back to all
//     branches in the same tier order.
//   - branch is REQUIRED to be non-empty; an error is returned otherwise
//     (impostor / fork-network signal).
//
// hintRef may be empty (e.g. for bare-SHA pins). The repo's default branch
// is discovered automatically via GET /repos/{owner}/{repo} (cached).
func (r *Resolver) DiscoverContaining(owner, repo, sha, hintRef string) (tag, branch string, err error) {
	return r.DiscoverContainingDefault(owner, repo, sha, hintRef, r.getDefaultBranch(owner, repo))
}

// DiscoverContainingDefault is DiscoverContaining with an explicit hint at
// the repository's default branch (e.g. "main"). When the discovered branch
// set contains defaultBranch it is preferred over lexicographic ordering.
//
// Branch search is protected-first: only protected branches are tried in
// the first pass. If none contain sha, the search falls back to ALL
// branches.
func (r *Resolver) DiscoverContainingDefault(owner, repo, sha, hintRef, defaultBranch string) (tag, branch string, err error) {
	branches, err := r.listBranches(owner, repo)
	if err != nil {
		return "", "", err
	}
	if len(branches) == 0 {
		return "", "", fmt.Errorf("%s/%s@%s is not on any branch — fork-network / impostor signal; refusing to pin", owner, repo, shortSha(sha))
	}
	protectedBranches := make([]branchHead, 0, len(branches))
	for _, b := range branches {
		if b.Protected {
			protectedBranches = append(protectedBranches, b)
		}
	}

	// First pass: protected branches only.
	branch, err = r.findContainingBranch(owner, repo, sha, hintRef, defaultBranch, protectedBranches)
	if err != nil {
		return "", "", err
	}
	if branch == "" {
		// Fallback: relax to all branches.
		branch, err = r.findContainingBranch(owner, repo, sha, hintRef, defaultBranch, branches)
		if err != nil {
			return "", "", err
		}
	}

	if branch == "" {
		return "", "", fmt.Errorf("%s/%s@%s is not on any branch — fork-network / impostor signal; refusing to pin", owner, repo, shortSha(sha))
	}

	// Discover tags pointing at sha.
	allTags, err := r.listTagsForRepo(owner, repo)
	if err != nil {
		return "", "", err
	}
	var tagNames []string
	for _, t := range allTags {
		if strings.EqualFold(t.SHA, sha) {
			tagNames = append(tagNames, t.Name)
		}
	}
	tag = pickPreferred(tagNames, hintRef, "")

	return tag, branch, nil
}

// findContainingBranch returns the name of the first branch (from the given
// candidate set, in orderedBranches tier order) whose HEAD is sha or which
// contains sha as an ancestor via the Compare API. Returns "" if none
// match. Returns a non-nil error only for unexpected transport failures
// from the Compare API (404/422 are treated as "not contained").
func (r *Resolver) findContainingBranch(owner, repo, sha, hintRef, defaultBranch string, candidates []branchHead) (string, error) {
	if len(candidates) == 0 {
		return "", nil
	}
	// Fast path: exact HEAD match.
	var exact []string
	for _, b := range candidates {
		if strings.EqualFold(b.SHA, sha) {
			exact = append(exact, b.Name)
		}
	}
	if len(exact) > 0 {
		return pickPreferred(exact, hintRef, defaultBranch), nil
	}
	// Slow path: ancestry via Compare API in tier order.
	for _, b := range orderedBranches(candidates, hintRef, defaultBranch) {
		r.progress("scanning %s/%s branch %s", owner, repo, b.Name)
		ok, err := r.branchContainsCommit(owner, repo, sha, b.SHA)
		if err != nil {
			return "", fmt.Errorf("comparing %s with %s/%s branch %s: %w",
				shortSha(sha), owner, repo, b.Name, err)
		}
		if ok {
			return b.Name, nil
		}
	}
	return "", nil
}

// pickPreferred returns hintRef if it appears in candidates, else
// defaultPick if non-empty and present, else the lexicographically-first
// candidate, else "".
func pickPreferred(candidates []string, hintRef, defaultPick string) string {
	if len(candidates) == 0 {
		return ""
	}
	for _, c := range candidates {
		if c == hintRef && hintRef != "" {
			return c
		}
	}
	if defaultPick != "" {
		for _, c := range candidates {
			if c == defaultPick {
				return c
			}
		}
	}
	best := candidates[0]
	for _, c := range candidates[1:] {
		if c < best {
			best = c
		}
	}
	return best
}

func shortSha(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	return s
}

// NormalizeContaining runs DiscoverContaining for every entry in deps,
// populates dep.Tag and dep.Branch, and computes the canonical @ref.
// When the canonical ref differs from dep.Ref the change is recorded in
// the returned rewrites map (keyed by "owner/repo[/path]@old-ref" →
// "owner/repo[/path]@new-ref") and dep.Ref is updated in place. Callers
// should pass the resulting map to lockfile.File.RewriteActionRefs to
// mutate the workflow uses: lines.
//
// Ref selection: if the original ref was itself a discovered branch (user
// wrote e.g. @main), that branch is preserved as the key ref. Otherwise
// the discovered tag wins (if any), falling back to the containing branch.
//
// Returns an error from the first dep whose discovery fails (e.g. commit
// not on any branch — fail closed). On error rewrites is nil and earlier
// in-place mutations may have already happened; the caller should treat
// the deps slice as tainted.
func (r *Resolver) NormalizeContaining(deps []lockfile.Dependency) (map[string]string, error) {
	rewrites := map[string]string{}
	for i := range deps {
		d := &deps[i]
		owner, repo := d.OwnerRepo()
		if owner == "" || repo == "" {
			continue
		}
		r.progress("resolving %s@%s", d.NWO, d.Ref)
		tag, branch, err := r.DiscoverContaining(owner, repo, d.SHA, d.Ref)
		if err != nil {
			return nil, fmt.Errorf("%s@%s: %w", d.NWO, d.Ref, err)
		}
		d.Tag = tag
		d.Branch = branch
		// Prefer the user's explicit branch ref when the workflow file
		// already names a branch (e.g. @main stays @main even when tags
		// also exist). Otherwise prefer the discovered tag, falling back
		// to the containing branch.
		newRef := tag
		if branch == d.Ref {
			// hintRef matched a branch — user wrote a branch ref.
			newRef = branch
		}
		if newRef == "" {
			newRef = branch
		}
		if newRef == "" || newRef == d.Ref {
			continue
		}
		rewrites[d.NWO+"@"+d.Ref] = d.NWO + "@" + newRef
		d.Ref = newRef
	}
	return rewrites, nil
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

		r.progress("checking reachability %s@%s", dep.NWO, dep.Ref)
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
		// Keep partial results: per-ref failures are surfaced via err, but
		// successful resolutions in `deps` should not be discarded — downstream
		// renderers degrade gracefully per-ref instead of marking everything
		// unresolved.
		allDeps = append(allDeps, deps...)
		if err != nil {
			return dedup(allDeps), err
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
	var batchErr error
	for i := 0; i < len(uncached); i += MaxBatchSize {
		end := i + MaxBatchSize
		if end > len(uncached) {
			end = len(uncached)
		}
		deps, ymls, err := r.resolveWithActionYMLBatch(uncached[i:end])
		// Keep partial batch results: per-ref failures shouldn't discard
		// successful resolutions from the same batch.
		freshDeps = append(freshDeps, deps...)
		freshYMLs = append(freshYMLs, ymls...)
		if err != nil {
			batchErr = err
			break
		}
	}

	for i, dep := range freshDeps {
		r.cache[dep.Key()] = resolvedEntry{dep: dep, actionYML: freshYMLs[i]}
	}

	// Build allDeps from cached refs + freshly-resolved ones. Refs that failed
	// to resolve are simply absent — callers see them missing rather than
	// receiving an empty slice for the whole workflow.
	resolvedFresh := make(map[string]int, len(freshDeps))
	for i, dep := range freshDeps {
		resolvedFresh[dep.Key()] = i
	}
	for i, ref := range refs {
		key := cacheKey(ref)
		if cachedIdx[i] {
			entry := r.cache[key]
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
