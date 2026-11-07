package ghapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"

	"github.com/cli/go-gh/v2/pkg/api"
)

// anonProbeCache caches per-owner results of unauthenticated access probes.
// true = anonymous access confirmed working, false = not accessible.
// Only definitive answers are cached; rate limits and 5xx are retried.
var anonProbeCache sync.Map // map[string]bool

// anonRateLimited marks owners whose anonymous fallback was last skipped
// because the unauthenticated rate limit was exhausted.
var anonRateLimited sync.Map // map[string]struct{}

// anonIPBlocked remembers the denial behind a fallback's later rate limit.
var anonIPBlocked sync.Map // map[string]bool

// SSORateLimitedError reports a token blocked by an organization policy whose
// anonymous fallback was rate-limited. The message names that policy so
// callers render the matching fix.
type SSORateLimitedError struct {
	Host, Owner string
	Err         error
	ipPolicy    bool
}

func (e *SSORateLimitedError) Error() string {
	message := ssoRequiredMessage(e.Host, e.Owner)
	if e.ipPolicy {
		message = fmt.Sprintf("GitHub refused the token for the %q organization: this IP address is not on the organization's IP allow list. Retry from an allowed IP address, or add this one to the allow list", e.Owner)
	}
	return message + " (the anonymous fallback for public repositories was rate-limited)"
}

func (e *SSORateLimitedError) Unwrap() error { return e.Err }

// SSOFallbackEligible reports whether the given owner's repos can be
// accessed anonymously when an organization policy (SAML enforcement or an IP
// allow list) blocks authenticated access. On first call for an owner, it
// probes the GitHub API with an unauthenticated request to determine
// accessibility, then caches a definitive result.
func (c *Client) SSOFallbackEligible(ctx context.Context, owner string) bool {
	if IsProxima(c.Hostname) {
		return false
	}
	key := c.anonBase() + "/" + owner
	if v, ok := anonProbeCache.Load(key); ok {
		return v.(bool)
	}

	// Probe: unauthenticated HEAD to /orgs/{owner} — 200 means the org
	// is publicly visible and its public repos are anonymously accessible.
	probeURL := fmt.Sprintf("%s/orgs/%s", c.anonBase(), url.PathEscape(owner))
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, probeURL, nil)
	if err != nil {
		// Construction error — don't cache, let next call retry.
		return false
	}
	req.Header.Set("Accept", "application/vnd.github.v3+json")

	resp, err := c.anonClient().Do(req)
	if err != nil {
		// Transport error (network, context canceled) — don't cache.
		return false
	}
	resp.Body.Close()

	anonRateLimited.Delete(key)
	switch {
	case resp.StatusCode == http.StatusOK:
		anonProbeCache.Store(key, true)
		return true
	case isRateLimited(resp):
		anonRateLimited.Store(key, struct{}{})
		return false
	case resp.StatusCode >= 500:
		return false
	}
	anonProbeCache.Store(key, false)
	return false
}

func isRateLimited(resp *http.Response) bool {
	switch resp.StatusCode {
	case http.StatusTooManyRequests:
		return true
	case http.StatusForbidden:
		return resp.Header.Get("X-RateLimit-Remaining") == "0" || resp.Header.Get("Retry-After") != ""
	}
	return false
}

// ssoErr upgrades an organization policy block to SSORateLimitedError when
// the anonymous fallback for owner was skipped because of a rate limit.
func (c *Client) ssoErr(owner string, err error) error {
	if _, limited := anonRateLimited.Load(c.anonBase() + "/" + owner); limited && organizationDenied(err) {
		return &SSORateLimitedError{Host: c.Hostname, Owner: owner, Err: err, ipPolicy: isIPAllowListDenial(err)}
	}
	return err
}

func (c *Client) recordPolicy(owner string, err error) {
	anonIPBlocked.Store(c.anonBase()+"/"+owner, isIPAllowListDenial(err))
}

func (c *Client) orgPolicyFallback(ctx context.Context, owner string, err error) bool {
	if !organizationDenied(err) || !c.SSOFallbackEligible(ctx, owner) {
		return false
	}
	c.recordPolicy(owner, err)
	return true
}

// Metadata may succeed authenticated; the fallback's own reads must still succeed.
// Record the policy even when metadata is cached and supplies no REST denial.
func (c *Client) repoFallbackEligible(ctx context.Context, owner, repo string, err error) bool {
	if IsProxima(c.Hostname) {
		return false
	}
	if !organizationDenied(err) {
		if code, _ := StatusCode(err); code != http.StatusUnauthorized {
			return false
		}
	} else {
		c.recordPolicy(owner, err)
	}
	_, metaErr := c.repoMetadata(ctx, owner, repo)
	return metaErr == nil
}

func organizationDenied(err error) bool {
	return IsSAMLEnforcement(err) || isIPAllowListDenial(err)
}

// GitHub interpolates the organization name before this stable denial clause.
const ipAllowListDenial = "organization has an IP allow list enabled"

// GitHub also reports this policy in HTTP-200 GraphQL FORBIDDEN items.
// Require structured evidence; a bare error message is not sufficient.
func isIPAllowListDenial(err error) bool {
	if rateLimited(err) {
		return false
	}
	var gqlErr *api.GraphQLError
	if errors.As(err, &gqlErr) {
		for _, item := range gqlErr.Errors {
			if item.Type == "FORBIDDEN" && strings.Contains(item.Message, ipAllowListDenial) {
				return true
			}
		}
		return false
	}
	code, ok := StatusCode(err)
	return ok && code == http.StatusForbidden && strings.Contains(err.Error(), ipAllowListDenial)
}

// Keep only the matching alias's policy evidence, never the whole envelope.
func ipAllowListAliasError(gqlErr *api.GraphQLError, alias string) error {
	if gqlErr == nil || graphQLRateLimited(gqlErr) {
		return nil
	}
	for _, item := range gqlErr.Errors {
		if len(item.Path) == 0 || item.Path[0] != alias {
			continue
		}
		scoped := &api.GraphQLError{Errors: []api.GraphQLErrorItem{item}}
		if isIPAllowListDenial(scoped) {
			return scoped
		}
	}
	return nil
}

// IPAllowListBlocked includes rate-limited fallbacks caused by an IP denial,
// unlike isIPAllowListDenial which determines retry eligibility.
func IPAllowListBlocked(err error) bool {
	if isIPAllowListDenial(err) {
		return true
	}
	var rateLimited *SSORateLimitedError
	return errors.As(err, &rateLimited) && rateLimited.ipPolicy
}

// IsSAMLEnforcement reports whether err represents a SAML/SSO enforcement
// block. It matches both REST 403s (api.HTTPError) and plain errors whose
// message indicates SAML enforcement (e.g. from the GraphQL resolution path).
func IsSAMLEnforcement(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	if strings.Contains(msg, "SAML enforcement") || strings.Contains(msg, "SAML SSO") {
		// For HTTPErrors, further verify it's a 403.
		if code, ok := StatusCode(err); ok {
			return code == http.StatusForbidden
		}
		// Plain errors from GraphQL SAML detection: trust the message.
		return true
	}
	return false
}

// anonBase returns the base URL for anonymous REST calls.
// For stub-server hostnames (containing a port), it uses the hostname
// directly without prepending "api.".
func (c *Client) anonBase() string {
	if c.anonBaseURL != "" {
		return c.anonBaseURL
	}
	host := c.Hostname
	if host == "" {
		host = "github.com"
	}
	// Stub servers use IP:port — don't prepend "api." for those.
	if strings.Contains(host, ":") {
		return fmt.Sprintf("https://%s", host)
	}
	return fmt.Sprintf("https://api.%s", host)
}

// anonClient returns the HTTP client for anonymous requests.
func (c *Client) anonClient() *http.Client {
	if c.anonHTTP != nil {
		return c.anonHTTP
	}
	return http.DefaultClient
}

// anonGet performs an unauthenticated GET and decodes JSON into dest.
func (c *Client) anonGet(ctx context.Context, path string, dest any) error {
	u := c.anonBase() + "/" + path
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/vnd.github.v3+json")

	resp, err := c.anonClient().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	parts := strings.SplitN(path, "/", 3)
	ownerKey := ""
	if len(parts) > 1 {
		ownerKey = c.anonBase() + "/" + parts[1]
		if !isRateLimited(resp) {
			anonRateLimited.Delete(ownerKey)
		}
	}
	if resp.StatusCode != http.StatusOK {
		herr := &api.HTTPError{StatusCode: resp.StatusCode, RequestURL: req.URL, Headers: resp.Header}
		if isRateLimited(resp) && !c.restOnly && ownerKey != "" {
			anonRateLimited.Store(ownerKey, struct{}{})
			blocked, _ := anonIPBlocked.Load(ownerKey)
			ipPolicy, _ := blocked.(bool)
			return &SSORateLimitedError{Host: c.Hostname, Owner: parts[1], Err: herr, ipPolicy: ipPolicy}
		}
		return c.publicErr(herr)
	}
	return json.NewDecoder(resp.Body).Decode(dest)
}

// anonListBranches fetches branches for a public repo without authentication.
func (c *Client) anonListBranches(ctx context.Context, owner, repo string) ([]BranchHead, error) {
	var all []BranchHead
	for page := 1; ; page++ {
		path := fmt.Sprintf("repos/%s/%s/branches?per_page=100&page=%d",
			url.PathEscape(owner), url.PathEscape(repo), page)
		var resp []struct {
			Name   string `json:"name"`
			Commit struct {
				SHA string `json:"sha"`
			} `json:"commit"`
			Protected bool `json:"protected"`
		}
		if err := c.anonGet(ctx, path, &resp); err != nil {
			return nil, fmt.Errorf("anonymous fallback listing branches for %s/%s: %w", owner, repo, err)
		}
		for _, b := range resp {
			all = append(all, BranchHead{Name: b.Name, SHA: b.Commit.SHA, Protected: b.Protected})
		}
		if len(resp) < 100 {
			break
		}
	}
	return all, nil
}

// anonListTags fetches tags for a public repo without authentication.
func (c *Client) anonListTags(ctx context.Context, owner, repo string) ([]TagEntry, error) {
	path := fmt.Sprintf("repos/%s/%s/tags?per_page=100",
		url.PathEscape(owner), url.PathEscape(repo))
	var resp []struct {
		Name   string `json:"name"`
		Commit struct {
			SHA string `json:"sha"`
		} `json:"commit"`
	}
	if err := c.anonGet(ctx, path, &resp); err != nil {
		return nil, fmt.Errorf("anonymous fallback listing tags for %s/%s: %w", owner, repo, err)
	}
	tags := make([]TagEntry, 0, len(resp))
	for _, t := range resp {
		tags = append(tags, TagEntry{Name: t.Name, SHA: t.Commit.SHA})
	}
	return tags, nil
}

// anonPeelTagObject determines whether sha is an annotated tag and, if so,
// peels it to the underlying commit using unauthenticated REST.
func (c *Client) anonPeelTagObject(ctx context.Context, owner, repo, sha string) (PeelTagObjectResult, error) {
	const maxDepth = 16 // ponytail: cap pathological REST walks; raise only for a real tag chain.
	current := sha
	seen := make(map[string]struct{})
	result := PeelTagObjectResult{}
	for depth := 0; depth < maxDepth; depth++ {
		if _, ok := seen[current]; ok {
			return result, fmt.Errorf("annotated tag cycle at %s", current)
		}
		seen[current] = struct{}{}

		tagPath := fmt.Sprintf("repos/%s/%s/git/tags/%s",
			url.PathEscape(owner), url.PathEscape(repo), url.PathEscape(current))
		var tagResp struct {
			Object struct {
				Type string `json:"type"`
				SHA  string `json:"sha"`
			} `json:"object"`
		}
		if err := c.anonGet(ctx, tagPath, &tagResp); err != nil {
			if depth > 0 {
				return result, fmt.Errorf("peeling annotated tag %s: %w", current, err)
			}
			break
		}

		result.Typename = "Tag"
		switch tagResp.Object.Type {
		case "commit":
			result.CommitOID = tagResp.Object.SHA
			return result, nil
		case "tag":
			if tagResp.Object.SHA == "" {
				return result, fmt.Errorf("annotated tag %s points to an empty tag SHA", current)
			}
			current = tagResp.Object.SHA
		default:
			return result, nil
		}
	}
	if result.Typename == "Tag" {
		return result, fmt.Errorf("annotated tag peel exceeded %d objects", maxDepth)
	}

	// Not a tag object — check if it's a commit directly.
	commitPath := fmt.Sprintf("repos/%s/%s/git/commits/%s",
		url.PathEscape(owner), url.PathEscape(repo), url.PathEscape(sha))
	var commitResp struct {
		SHA string `json:"sha"`
	}
	if err := c.anonGet(ctx, commitPath, &commitResp); err == nil {
		return PeelTagObjectResult{Typename: "Commit", CommitOID: commitResp.SHA}, nil
	}

	// Can't determine type — return zero result like the GraphQL fallback.
	return PeelTagObjectResult{}, nil
}

// anonCompareCommits reports whether sha is an ancestor of branchHeadSHA
// using unauthenticated REST.
func (c *Client) anonCompareCommits(ctx context.Context, owner, repo, sha, branchHeadSHA string) (bool, error) {
	path := comparePath(owner, repo, sha, branchHeadSHA)
	var resp compareResponse
	if err := c.anonGet(ctx, path, &resp); err != nil {
		return false, err
	}
	return strings.EqualFold(resp.MergeBaseCommit.SHA, sha), nil
}

// resolveAnonymous fetches the commit SHA and action.yml content for a
// single ref using unauthenticated REST calls. This only works for public
// repos and is used as a fallback when SSO blocks the authenticated path.
func (c *Client) resolveAnonymous(ctx context.Context, ref ActionFileRequest) ActionFileResult {
	result := ActionFileResult{
		Hostname: c.Hostname,
		Owner:    ref.Owner,
		Repo:     ref.Repo,
		Path:     ref.Path,
		Ref:      ref.Ref,
	}

	meta, err := c.repoMetadata(ctx, ref.Owner, ref.Repo)
	if err == nil {
		err = canonicalize(&result, meta.FullName)
	}
	if err != nil {
		result.Err = fmt.Errorf("anonymous fallback: %w", err)
		return result
	}

	base := c.anonBase()

	// Resolve ref → commit SHA via the commits endpoint.
	commitURL := fmt.Sprintf("%s/repos/%s/%s/commits/%s",
		base,
		url.PathEscape(result.Owner),
		url.PathEscape(result.Repo),
		url.PathEscape(ref.Ref),
	)
	sha, err := c.anonGetCommitSHA(ctx, commitURL)
	if err != nil {
		var missing *RefNotFoundError
		if errors.As(err, &missing) {
			missing.Owner, missing.Repo, missing.Ref = ref.Owner, ref.Repo, ref.Ref
		}
		result.Err = fmt.Errorf("anonymous fallback: %w", err)
		return result
	}
	result.CommitOID = sha

	// Fetch action.yml (try .yml first, then .yaml).
	ymlPath := "action.yml"
	yamlPath := "action.yaml"
	if ref.Path != "" {
		ymlPath = ref.Path + "/action.yml"
		yamlPath = ref.Path + "/action.yaml"
	}

	content, err := c.anonGetFileContent(ctx, base, result.Owner, result.Repo, sha, ymlPath)
	if err != nil {
		if code, _ := StatusCode(err); code != http.StatusNotFound {
			result.Err = err
			return result
		}
		// Try .yaml extension.
		content, err = c.anonGetFileContent(ctx, base, result.Owner, result.Repo, sha, yamlPath)
		if err != nil {
			// Reusable workflows have no action metadata; other failures
			// must not silently truncate a composite's dependency graph.
			if code, _ := StatusCode(err); code != http.StatusNotFound {
				result.Err = err
			} else if ref.Dockerfile {
				result.Dockerfile, result.Err = c.anonDockerfile(ctx, base, result.Owner, result.Repo, sha, strings.TrimSuffix(ymlPath, "action.yml"))
			}
			return result
		}
	}
	result.ActionYML = content
	return result
}

// anonDockerfile reports a Dockerfile or dockerfile in dir.
func (c *Client) anonDockerfile(ctx context.Context, base, owner, repo, sha, dir string) (bool, error) {
	for _, name := range []string{"Dockerfile", "dockerfile"} {
		_, err := c.anonGetFileContent(ctx, base, owner, repo, sha, dir+name)
		if err == nil {
			return true, nil
		}
		if code, _ := StatusCode(err); code != http.StatusNotFound {
			return false, err
		}
	}
	return false, nil
}

// anonGetCommitSHA fetches the commit SHA for a ref without authentication.
func (c *Client) anonGetCommitSHA(ctx context.Context, url string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/vnd.github.v3+json")

	resp, err := c.anonClient().Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusUnprocessableEntity {
		return "", &RefNotFoundError{Err: fmt.Errorf("HTTP %d resolving commit", resp.StatusCode)}
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("resolving commit: %w", c.publicErr(&api.HTTPError{StatusCode: resp.StatusCode, RequestURL: req.URL, Headers: resp.Header}))
	}

	var commit struct {
		SHA string `json:"sha"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&commit); err != nil {
		return "", fmt.Errorf("decoding commit response: %w", err)
	}
	if commit.SHA == "" {
		return "", fmt.Errorf("empty SHA in response")
	}
	return commit.SHA, nil
}

// anonGetFileContent fetches a file's content from a public repo without auth.
func (c *Client) anonGetFileContent(ctx context.Context, base, owner, repo, ref, path string) (string, error) {
	// Escape each segment of the file path individually to preserve slashes.
	escapedPath := escapeContentPath(path)
	u := fmt.Sprintf("%s/repos/%s/%s/contents/%s?ref=%s",
		base,
		url.PathEscape(owner),
		url.PathEscape(repo),
		escapedPath,
		url.QueryEscape(ref),
	)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/vnd.github.v3.raw")

	resp, err := c.anonClient().Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", c.publicErr(&api.HTTPError{StatusCode: resp.StatusCode, Message: fmt.Sprintf("fetching %s", path), Headers: resp.Header})
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("reading %s: %w", path, err)
	}
	return string(body), nil
}

// escapeContentPath URL-escapes each segment of a slash-delimited file path,
// preserving the slash separators.
func escapeContentPath(p string) string {
	segments := strings.Split(p, "/")
	for i, s := range segments {
		segments[i] = url.PathEscape(s)
	}
	return strings.Join(segments, "/")
}
