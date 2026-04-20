// Package resolver resolves action refs to commit SHAs via the GitHub GraphQL
// API and recursively discovers transitive dependencies from composite actions.
package resolver

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/cli/go-gh/v2/pkg/api"
	"github.com/github/gh-actions-pin/internal/lockfile"
)

// DefaultMaxRecursionDepth matches the runner's composite action recursion limit.
const DefaultMaxRecursionDepth = 10

// MaxBatchSize is the maximum number of action refs per GraphQL query.
const MaxBatchSize = 20

type resolvedEntry struct {
	dep       lockfile.Dependency
	actionYML string
}

// Resolver resolves action refs to commit SHAs.
type Resolver struct {
	client            *api.GraphQLClient
	hostname          string
	MaxRecursionDepth int
	cache             map[string]resolvedEntry
	latestRefCache    map[string]string
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

	client, err := api.NewGraphQLClient(opts)
	if err != nil {
		return nil, err
	}

	return &Resolver{
		client:            client,
		hostname:          hostname,
		MaxRecursionDepth: DefaultMaxRecursionDepth,
		cache:             make(map[string]resolvedEntry),
		latestRefCache:    make(map[string]string),
	}, nil
}

// NewWithTransport creates a resolver with a custom HTTP transport. This is
// primarily useful for tests that want cli/cli-style HTTP stubbing.
func NewWithTransport(hostname string, transport http.RoundTripper) (*Resolver, error) {
	return NewWithOptions(api.ClientOptions{
		AuthToken:    "test-token",
		Host:         hostname,
		Transport:    transport,
		LogIgnoreEnv: true,
	})
}

// Hostname returns the GitHub host the resolver is targeting.
func (r *Resolver) Hostname() string {
	return r.hostname
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

			for _, use := range meta.NestedUses {
				if actionRef := lockfile.ParseActionRef(use); actionRef != nil {
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
		escapedRef := strings.ReplaceAll(ref.Ref, `"`, `\"`)

		ymlPath := "action.yml"
		yamlPath := "action.yaml"
		if ref.Path != "" {
			ymlPath = ref.Path + "/action.yml"
			yamlPath = ref.Path + "/action.yaml"
		}

		fmt.Fprintf(&sb, " %s: repository(owner: %q, name: %q) {", alias, ref.Owner, ref.Repo)
		sb.WriteString(" nameWithOwner")
		fmt.Fprintf(&sb, " object(expression: %q) {", escapedRef)
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

var stableTagRE = regexp.MustCompile(`^v?(\d+)(?:\.(\d+))?(?:\.(\d+))?$`)

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

		matches := stableTagRE.FindStringSubmatch(tag)
		if matches == nil {
			continue
		}

		major := parseInt(matches[1])
		minor := parseInt(matches[2])
		patch := parseInt(matches[3])

		if matches[2] == "" && matches[3] == "" && major > bestMajor {
			bestMajor = major
			bestMajorTag = tag
		}

		version := [3]int{major, minor, patch}
		if compareVersion(version, bestVersion) > 0 {
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

func parseInt(s string) int {
	if s == "" {
		return 0
	}
	n, _ := strconv.Atoi(s)
	return n
}

func compareVersion(left, right [3]int) int {
	for i := 0; i < len(left); i++ {
		if left[i] > right[i] {
			return 1
		}
		if left[i] < right[i] {
			return -1
		}
	}
	return 0
}
