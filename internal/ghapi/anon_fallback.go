package ghapi

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
)

// ssoFallbackOwners lists orgs whose repos are public and can be resolved
// without authentication when the user's token is SAML-blocked. This avoids
// hard failures in environments (like Codespaces) where SSO authorization
// isn't possible for cross-enterprise orgs.
var ssoFallbackOwners = map[string]bool{
	"actions": true,
}

// SSOFallbackEligible reports whether the given owner is eligible for
// anonymous resolution when SSO blocks authenticated access.
func SSOFallbackEligible(owner string) bool {
	return ssoFallbackOwners[owner]
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

	host := c.Hostname
	if host == "" {
		host = "github.com"
	}
	base := c.anonBaseURL
	if base == "" {
		base = fmt.Sprintf("https://api.%s", host)
	}

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
		path, // already safe for path use
		url.QueryEscape(ref),
	)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return "", err
	}
	// Request raw content directly to avoid base64 decoding.
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
