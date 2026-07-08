package ghapi

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
)

// RepoTag is a tag name paired with the commit SHA it resolves to, as
// returned by the repos/tags endpoint (annotated tags dereferenced).
type RepoTag struct {
	Name string
	SHA  string
}

// RepoTags lists a repository's tags (up to 100) with the commit SHA each
// resolves to. Delegates to ListTags so it shares the cached, singleflight-
// coalesced tag fetch rather than issuing a second identical request.
func (c *Client) RepoTags(ctx context.Context, owner, repo string) ([]RepoTag, error) {
	entries, err := c.ListTags(ctx, owner, repo)
	if err != nil {
		return nil, fmt.Errorf("fetching tags for %s/%s: %w", owner, repo, err)
	}
	out := make([]RepoTag, 0, len(entries))
	for _, t := range entries {
		out = append(out, RepoTag{Name: t.Name, SHA: t.SHA})
	}
	return out, nil
}

// RepoRelease holds release metadata for a tag.
type RepoRelease struct {
	TagName     string
	PublishedAt string // ISO 8601 date
	Immutable   bool
}

// Releases lists a repository's most recent releases (up to 30).
func (c *Client) Releases(ctx context.Context, owner, repo string) ([]RepoRelease, error) {
	path := fmt.Sprintf("repos/%s/%s/releases?per_page=30",
		url.PathEscape(owner), url.PathEscape(repo))

	var releases []struct {
		TagName     string `json:"tag_name"`
		PublishedAt string `json:"published_at"`
		Immutable   bool   `json:"immutable"`
	}
	if err := c.rest.DoWithContext(ctx, http.MethodGet, path, nil, &releases); err != nil {
		if IsSAMLEnforcement(err) && c.SSOFallbackEligible(ctx, owner) {
			if anonErr := c.anonGet(ctx, path, &releases); anonErr != nil {
				return nil, fmt.Errorf("anonymous fallback fetching releases for %s/%s: %w", owner, repo, anonErr)
			}
		} else {
			return nil, fmt.Errorf("fetching releases for %s/%s: %w", owner, repo, err)
		}
	}

	out := make([]RepoRelease, 0, len(releases))
	for _, rel := range releases {
		out = append(out, RepoRelease(rel))
	}
	return out, nil
}

// RepoMetadata holds repository-level metadata relevant for pinning decisions.
type RepoMetadata struct {
	DefaultBranch string
	Visibility    string // "public", "private", or "internal"
	PushedAt      string // ISO 8601 timestamp of last push
}

// RepoMetadata fetches a repository's default branch, visibility, and last
// push time. Delegates to the shared repoMetadata fetch so it shares the
// cached, singleflight-coalesced repos/{owner}/{repo} round-trip with
// GetDefaultBranch and RepoIDs.
func (c *Client) RepoMetadata(ctx context.Context, owner, repo string) (RepoMetadata, error) {
	m, err := c.repoMetadata(ctx, owner, repo)
	if err != nil {
		return RepoMetadata{}, fmt.Errorf("fetching metadata for %s/%s: %w", owner, repo, err)
	}
	return RepoMetadata{
		DefaultBranch: m.DefaultBranch,
		Visibility:    m.Visibility,
		PushedAt:      m.PushedAt,
	}, nil
}

// CommitSHA resolves a ref (branch, tag, or SHA) to its commit SHA via the
// repos/commits endpoint.
func (c *Client) CommitSHA(ctx context.Context, owner, repo, ref string) (string, error) {
	path := fmt.Sprintf("repos/%s/%s/commits/%s",
		url.PathEscape(owner), url.PathEscape(repo), url.PathEscape(ref))

	var result struct {
		SHA string `json:"sha"`
	}
	if err := c.rest.DoWithContext(ctx, http.MethodGet, path, nil, &result); err != nil {
		if IsSAMLEnforcement(err) && c.SSOFallbackEligible(ctx, owner) {
			if anonErr := c.anonGet(ctx, path, &result); anonErr != nil {
				return "", fmt.Errorf("anonymous fallback resolving %s/%s@%s: %w", owner, repo, ref, anonErr)
			}
		} else {
			return "", fmt.Errorf("resolving %s/%s@%s: %w", owner, repo, ref, err)
		}
	}
	return result.SHA, nil
}
