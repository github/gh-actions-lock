package ghapi

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
)

// anonProbeCache caches per-owner results of anonymous access probes.
// true = anonymous access confirmed working, false = not accessible.
var anonProbeCache sync.Map // map[string]bool

// SSOFallbackEligible reports whether the given owner's repos can be
// accessed anonymously when SSO blocks authenticated access. On first
// call for an owner, it probes the GitHub API with an unauthenticated
// request to determine accessibility, then caches the result.
func (c *Client) SSOFallbackEligible(ctx context.Context, owner string) bool {
	key := c.anonBase() + "/" + owner
	if v, ok := anonProbeCache.Load(key); ok {
		return v.(bool)
	}

	// Probe: unauthenticated HEAD to /orgs/{owner} — 200 means the org
	// is publicly visible and its public repos are anonymously accessible.
	probeURL := fmt.Sprintf("%s/orgs/%s", c.anonBase(), url.PathEscape(owner))
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, probeURL, nil)
	if err != nil {
		anonProbeCache.Store(key, false)
		return false
	}
	req.Header.Set("Accept", "application/vnd.github.v3+json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		anonProbeCache.Store(key, false)
		return false
	}
	resp.Body.Close()

	eligible := resp.StatusCode == http.StatusOK
	anonProbeCache.Store(key, eligible)
	return eligible
}

// IsSAMLEnforcement reports whether err is a 403 caused by SAML/SSO
// enforcement (as opposed to rate limiting or other permission issues).
func IsSAMLEnforcement(err error) bool {
	if err == nil {
		return false
	}
	code, ok := StatusCode(err)
	if !ok || code != http.StatusForbidden {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "SAML enforcement") || strings.Contains(msg, "SAML SSO")
}

// anonBaseURL returns the base URL for anonymous REST calls.
func (c *Client) anonBase() string {
	if c.anonBaseURL != "" {
		return c.anonBaseURL
	}
	host := c.Hostname
	if host == "" {
		host = "github.com"
	}
	return fmt.Sprintf("https://api.%s", host)
}

// anonGet performs an unauthenticated GET and decodes JSON into dest.
func (c *Client) anonGet(ctx context.Context, path string, dest any) error {
	u := c.anonBase() + "/" + path
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/vnd.github.v3+json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d from %s", resp.StatusCode, path)
	}
	return json.NewDecoder(resp.Body).Decode(dest)
}

// anonListBranches fetches branches for a public repo without authentication.
func (c *Client) anonListBranches(ctx context.Context, owner, repo string) ([]BranchHead, error) {
	var all []BranchHead
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

// anonCompareCommits reports whether sha is an ancestor of branchHeadSHA
// using unauthenticated REST.
func (c *Client) anonCompareCommits(ctx context.Context, owner, repo, sha, branchHeadSHA string) (bool, error) {
	path := fmt.Sprintf("repos/%s/%s/compare/%s...%s",
		url.PathEscape(owner), url.PathEscape(repo),
		url.PathEscape(sha), url.PathEscape(branchHeadSHA))
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
		Owner: ref.Owner,
		Repo:  ref.Repo,
		Path:  ref.Path,
		Ref:   ref.Ref,
	}

	base := c.anonBase()

	// Resolve ref → commit SHA via the commits endpoint.
	commitURL := fmt.Sprintf("%s/repos/%s/%s/commits/%s",
		base,
		url.PathEscape(ref.Owner),
		url.PathEscape(ref.Repo),
		url.PathEscape(ref.Ref),
	)
	sha, err := anonGetCommitSHA(ctx, commitURL)
	if err != nil {
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

	content, err := anonGetFileContent(ctx, base, ref.Owner, ref.Repo, sha, ymlPath)
	if err != nil {
		// Try .yaml extension.
		content, err = anonGetFileContent(ctx, base, ref.Owner, ref.Repo, sha, yamlPath)
		if err != nil {
			// Not fatal — some actions don't have action.yml (reusable workflows).
			return result
		}
	}
	result.ActionYML = content
	return result
}

// anonGetCommitSHA fetches the commit SHA for a ref without authentication.
func anonGetCommitSHA(ctx context.Context, url string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/vnd.github.v3+json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("HTTP %d resolving commit", resp.StatusCode)
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
func anonGetFileContent(ctx context.Context, base, owner, repo, ref, path string) (string, error) {
	u := fmt.Sprintf("%s/repos/%s/%s/contents/%s?ref=%s",
		base,
		url.PathEscape(owner),
		url.PathEscape(repo),
		path,
		url.QueryEscape(ref),
	)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/vnd.github.v3.raw")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("HTTP %d fetching %s", resp.StatusCode, path)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("reading %s: %w", path, err)
	}
	return string(body), nil
}
