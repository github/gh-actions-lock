package ghapi

import (
	"bytes"
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

// Evidence about one lookup (the denial that sent it to the anonymous
// fallback, a rate-limited or failed probe) travels with that lookup, as a
// context value or a returned error. Owner-wide maps would let a concurrent
// lookup for the same owner overwrite it, so only definitive, idempotent
// answers live in anonProbeCache.

// policyKey carries, on the context of a lookup that took the anonymous
// fallback, whether an IP allow list (rather than SAML) refused its token.
type policyKey struct{}

func withPolicy(ctx context.Context, denial error) context.Context {
	return context.WithValue(ctx, policyKey{}, isIPAllowListDenial(denial))
}

func ipPolicyOf(ctx context.Context) bool {
	ip, _ := ctx.Value(policyKey{}).(bool)
	return ip
}

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
		// Owner is the requested owner, which after a transfer is not the
		// organization whose allow list refused the token: don't name it.
		message = "GitHub refused the token: this IP address is not on the organization's IP allow list. Retry from an allowed IP address, or add this one to the allow list"
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
	ok, _ := c.fallbackProbe(ctx, owner)
	return ok
}

// fallbackProbe is SSOFallbackEligible with the reason it could not tell: a
// rate-limited or failed probe returns its error, which says nothing about the
// owner and may clear on a retry. A definitive "no" returns none.
func (c *Client) fallbackProbe(ctx context.Context, owner string) (bool, error) {
	if IsProxima(c.Hostname) {
		return false, nil
	}
	key := c.anonBase() + "/" + owner
	if v, ok := anonProbeCache.Load(key); ok {
		return v.(bool), nil
	}

	// Probe: unauthenticated HEAD to /orgs/{owner} — 200 means the org
	// is publicly visible and its public repos are anonymously accessible.
	probeURL := fmt.Sprintf("%s/orgs/%s", c.anonBase(), url.PathEscape(owner))
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, probeURL, nil)
	if err != nil {
		// Construction error — don't cache, let next call retry.
		return false, nil
	}
	req.Header.Set("Accept", "application/vnd.github.v3+json")

	resp, err := c.probeClient().Do(req)
	if err != nil {
		// Transport error (network, context canceled) — don't cache.
		if IsTransient(err) {
			return false, err
		}
		return false, nil
	}
	resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusOK:
		anonProbeCache.Store(key, true)
		return true, nil
	case isRateLimited(resp), resp.StatusCode >= 500:
		return false, &api.HTTPError{StatusCode: resp.StatusCode, RequestURL: req.URL, Headers: resp.Header}
	}
	anonProbeCache.Store(key, false)
	return false, nil
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

// policyFailure reports why an organization policy block was final, given the
// failure (cause) that kept this lookup from the anonymous fallback: a
// rate-limited or failed probe or read. A rate limit upgrades the block to
// SSORateLimitedError; for an IP allow list the error then carries the
// rate-limited response, whose reset time is the only thing that can change
// the outcome. An IP allow list block whose fallback hit an outage reports the
// outage instead. SAML blocks keep their own error, and so does any block
// whose fallback was refused for another reason.
func (c *Client) policyFailure(owner string, denial, cause error) error {
	if !organizationDenied(denial) || cause == nil {
		return denial
	}
	ip := isIPAllowListDenial(denial)
	switch {
	case IsRateLimited(cause):
		if !ip {
			return &SSORateLimitedError{Host: c.Hostname, Owner: owner, Err: denial}
		}
		// Reuse a rate limit already reported as one only when it records an
		// IP policy: the read that hit it may have been refused by SAML, a
		// different block than the one that sent this lookup to the fallback.
		var limited *SSORateLimitedError
		if errors.As(cause, &limited) && limited.ipPolicy {
			return cause
		}
		return &SSORateLimitedError{Host: c.Hostname, Owner: owner, Err: cause, ipPolicy: true}
	case ip && IsTransient(cause):
		return fmt.Errorf("anonymous fallback for %q unavailable: %w", owner, cause)
	}
	return denial
}

// orgPolicyFallback decides whether a lookup that denial blocked takes the
// anonymous fallback. It returns a nil error and the context the anonymous
// reads must use (it carries which policy refused the token), or the error
// to report instead.
func (c *Client) orgPolicyFallback(ctx context.Context, owner, repo string, denial error) (context.Context, error) {
	if !organizationDenied(denial) {
		return ctx, denial
	}
	ctx = withPolicy(ctx, denial)
	ok, cause := c.policyFallbackEligible(ctx, owner, repo, denial)
	if ok {
		return ctx, nil
	}
	return ctx, c.policyFailure(owner, denial, cause)
}

// policyFallbackEligible reports whether owner/repo can be read anonymously
// after denial blocked the token. Beyond an organization owner (see
// SSOFallbackEligible), an IP allow list also covers a public repository whose
// requested owner is no organization: one transferred from a user or another
// owner, where REST followed the redirect into the restricted organization.
// The repository itself is then the probe. An error says why a probe could not
// tell.
func (c *Client) policyFallbackEligible(ctx context.Context, owner, repo string, denial error) (bool, error) {
	ok, cause := c.fallbackProbe(ctx, owner)
	if ok || cause != nil {
		return ok, cause
	}
	if !isIPAllowListDenial(denial) || IsProxima(c.Hostname) {
		return false, nil
	}
	// Only a definitive "not an organization" justifies another request.
	if v, cached := anonProbeCache.Load(c.anonBase() + "/" + owner); !cached || v.(bool) {
		return false, nil
	}
	return c.publicRepoReadable(ctx, owner, repo)
}

// publicRepoReadable reports whether an unauthenticated request can read
// owner/repo, following redirects. Only definitive answers are cached. When
// the read could not be made (an outage or rate limit), err says why.
func (c *Client) publicRepoReadable(ctx context.Context, owner, repo string) (ok bool, err error) {
	key := c.anonBase() + "/" + owner + "/" + repo
	if v, ok := anonProbeCache.Load(key); ok {
		return v.(bool), nil
	}
	path := fmt.Sprintf("repos/%s/%s", url.PathEscape(owner), url.PathEscape(repo))
	err = c.anonGet(ctx, path, &struct{}{})
	if err == nil {
		anonProbeCache.Store(key, true)
		return true, nil
	}
	if code, _ := StatusCode(err); code == http.StatusNotFound {
		anonProbeCache.Store(key, false)
		return false, nil
	}
	if !IsTransient(err) {
		return false, nil
	}
	return false, err
}

// repoFallback reports whether owner/repo can be read anonymously after err
// blocked the authenticated path: an organization policy block or a GraphQL
// 401. Metadata may succeed authenticated; the fallback's own reads must still
// succeed. It returns the context the anonymous reads must use, which carries
// which policy refused the token.
//
// When an IP allow list is the block, metadata that an authenticated read
// (or the cache of one) supplied proves nothing about what the anonymous reads
// that follow will see, so a credential-free read of the repository must also
// succeed. If a read fails with an outage or rate limit, that failure is
// returned as cause: it can clear on a retry, unlike the denial err, which
// would be reported as final (see policyFailure). Any other failure means the
// repository is not readable and err stands.
func (c *Client) repoFallback(ctx context.Context, owner, repo string, err error) (_ context.Context, eligible bool, cause error) {
	if IsProxima(c.Hostname) {
		return ctx, false, nil
	}
	if !organizationDenied(err) {
		if code, _ := StatusCode(err); code != http.StatusUnauthorized {
			return ctx, false, nil
		}
	}
	// Also for a 401, so a later rate limit isn't blamed on a policy.
	ctx = withPolicy(ctx, err)
	meta, metaErr := c.repoMetadata(ctx, owner, repo)
	if metaErr != nil {
		if IsTransient(metaErr) {
			return ctx, false, metaErr
		}
		return ctx, false, nil
	}
	if isIPAllowListDenial(err) && !meta.anonymous {
		// Metadata that an authenticated read supplied (or the cache kept)
		// doesn't show the anonymous reads that follow will see the
		// repository: require a credential-free one.
		ok, anonErr := c.publicRepoReadable(ctx, owner, repo)
		if !ok {
			return ctx, false, anonErr
		}
	}
	return ctx, true, nil
}

// fallbackFailure is the error to report when owner's anonymous fallback was
// not taken after repoFallback returned cause.
func (c *Client) fallbackFailure(owner string, denial, cause error) error {
	return c.policyFailure(owner, denial, cause)
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

// anonDo sends req on the anonymous client. The github.com public client of a
// *.ghe.com run may carry a stored or configured token, which an organization's
// IP allow list refuses even for public repositories; such a denial is retried
// once with no credentials, which GitHub accepts for public reads. req must
// have no body.
//
// retried reports that resp answers the credential-free retry, so errors made
// from it must not claim the token was used (see anonHTTPErr).
func (c *Client) anonDo(req *http.Request) (resp *http.Response, retried bool, err error) {
	resp, err = c.anonClient().Do(req)
	if err != nil || c.noCredHTTP == nil || resp.StatusCode != http.StatusForbidden || isRateLimited(resp) {
		return resp, false, err
	}
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	resp.Body.Close()
	if readErr == nil && strings.Contains(string(body), ipAllowListDenial) {
		resp, err = c.noCredHTTP.Do(req)
		return resp, true, err
	}
	resp.Body = io.NopCloser(bytes.NewReader(body))
	return resp, false, nil
}

// probeClient sends eligibility probes, which never need a credential: a
// public client's token would only have an IP allow list refuse the probe of
// an organization whose public repositories are readable without it.
func (c *Client) probeClient() *http.Client {
	if c.noCredHTTP != nil {
		return c.noCredHTTP
	}
	return c.anonClient()
}

// anonReadFailed is the error for a read that failed after the anonymous
// fallback was taken for denial. Anonymous reads cannot see a private
// repository, so under an IP allow list a 404 or 422 may mean the repository
// isn't public rather than that the ref or commit is missing. The
// repository's own anonymous metadata decides: when it isn't readable the
// denial stands (or the metadata's outage, which a retry may clear), and only
// for a readable repository is the 404 or 422 a verdict on the ref.
func (c *Client) anonReadFailed(ctx context.Context, owner, repo string, denial, anonErr error) error {
	if code, _ := StatusCode(anonErr); !isIPAllowListDenial(denial) || (code != http.StatusNotFound && code != http.StatusUnprocessableEntity) {
		return anonErr
	}
	if _, ok, cause := c.repoFallback(ctx, owner, repo, denial); !ok {
		return c.fallbackFailure(owner, denial, cause)
	}
	return anonErr
}

// anonHTTPErr is the error for a non-200 anonymous response. A response to
// the credential-free retry of an IP allow list denial is anonymous whatever
// the client's credential, and a rate limit on it keeps the policy that put
// the lookup on the anonymous limit.
func (c *Client) anonHTTPErr(owner string, herr *api.HTTPError, retried bool) error {
	if !retried {
		return c.publicErr(herr)
	}
	var err error = herr
	if IsRateLimited(herr) {
		err = &SSORateLimitedError{Host: c.Hostname, Owner: owner, Err: herr, ipPolicy: true}
	}
	return &publicError{auth: publicAnonymous, err: err}
}

// anonGet performs an unauthenticated GET and decodes JSON into dest. (A
// github.com public client with a token retries an IP allow list denial
// without it: see anonDo.)
func (c *Client) anonGet(ctx context.Context, path string, dest any) error {
	u := c.anonBase() + "/" + path
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/vnd.github.v3+json")

	resp, retried, err := c.anonDo(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	owner := ""
	if parts := strings.SplitN(path, "/", 3); len(parts) > 1 {
		owner = parts[1]
	}
	if resp.StatusCode != http.StatusOK {
		herr := &api.HTTPError{StatusCode: resp.StatusCode, RequestURL: req.URL, Headers: resp.Header}
		if isRateLimited(resp) && !c.restOnly && owner != "" {
			return &SSORateLimitedError{Host: c.Hostname, Owner: owner, Err: herr, ipPolicy: ipPolicyOf(ctx) || retried}
		}
		return c.anonHTTPErr(owner, herr, retried)
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
// repos and is used as a fallback when an organization policy (SSO or an IP
// allow list) blocks the authenticated path.
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
	sha, err := c.anonGetCommitSHA(ctx, result.Owner, commitURL)
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
func (c *Client) anonGetCommitSHA(ctx context.Context, owner, url string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/vnd.github.v3+json")

	resp, retried, err := c.anonDo(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusUnprocessableEntity {
		return "", &RefNotFoundError{Err: fmt.Errorf("HTTP %d resolving commit", resp.StatusCode)}
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("resolving commit: %w", c.anonHTTPErr(owner, &api.HTTPError{StatusCode: resp.StatusCode, RequestURL: req.URL, Headers: resp.Header}, retried))
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

	resp, retried, err := c.anonDo(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", c.anonHTTPErr(owner, &api.HTTPError{StatusCode: resp.StatusCode, Message: fmt.Sprintf("fetching %s", path), Headers: resp.Header}, retried)
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
