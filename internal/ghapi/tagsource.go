package ghapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/cli/go-gh/v2/pkg/api"
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
	c, err := c.ForRepo(ctx, owner, repo)
	if err != nil {
		return nil, err
	}
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
			return nil, fmt.Errorf("fetching releases for %s/%s: %w", owner, repo, c.ssoErr(owner, err))
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
	// FullName is the canonical OWNER/REPO, which differs from the requested
	// name after a rename or transfer.
	FullName      string
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
		FullName:      m.FullName,
		DefaultBranch: m.DefaultBranch,
		Visibility:    m.Visibility,
		PushedAt:      m.PushedAt,
	}, nil
}

// CommitSHA resolves a ref (branch, tag, or SHA) to its commit SHA via the
// repos/commits endpoint.
func (c *Client) CommitSHA(ctx context.Context, owner, repo, ref string) (string, error) {
	c, err := c.ForRepo(ctx, owner, repo)
	if err != nil {
		return "", err
	}
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
			return "", fmt.Errorf("resolving %s/%s@%s: %w", owner, repo, ref, c.ssoErr(owner, err))
		}
	}
	return result.SHA, nil
}

// CommitDate returns the committer timestamp of an exact commit, not its push time.
func (c *Client) CommitDate(ctx context.Context, owner, repo, sha string) (string, error) {
	c, err := c.ForRepo(ctx, owner, repo)
	if err != nil {
		return "", err
	}
	path := fmt.Sprintf("repos/%s/%s/commits/%s",
		url.PathEscape(owner), url.PathEscape(repo), url.PathEscape(sha))
	var result struct {
		SHA    string `json:"sha"`
		Commit struct {
			Committer struct {
				Date string `json:"date"`
			} `json:"committer"`
		} `json:"commit"`
	}
	if err := c.rest.DoWithContext(ctx, http.MethodGet, path, nil, &result); err != nil {
		if IsSAMLEnforcement(err) && c.SSOFallbackEligible(ctx, owner) {
			if err := c.anonGet(ctx, path, &result); err != nil {
				return "", fmt.Errorf("fetching commit date for %s/%s@%s: %w", owner, repo, sha, err)
			}
		} else {
			return "", fmt.Errorf("fetching commit date for %s/%s@%s: %w", owner, repo, sha, c.ssoErr(owner, err))
		}
	}
	if !strings.EqualFold(result.SHA, sha) {
		return "", fmt.Errorf("commit date response for %s/%s@%s identified %q", owner, repo, sha, result.SHA)
	}
	return result.Commit.Committer.Date, nil
}

// ReleaseByTag looks up a release outside the recent-release listing.
// A 404 means no visible release; other failures must not become missing dates.
func (c *Client) ReleaseByTag(ctx context.Context, owner, repo, tag string) (*RepoRelease, error) {
	c, err := c.ForRepo(ctx, owner, repo)
	if err != nil {
		return nil, err
	}
	path := fmt.Sprintf("repos/%s/%s/releases/tags/%s",
		url.PathEscape(owner), url.PathEscape(repo), url.PathEscape(tag))
	var result struct {
		TagName     string `json:"tag_name"`
		PublishedAt string `json:"published_at"`
		Immutable   bool   `json:"immutable"`
	}
	if err := c.rest.DoWithContext(ctx, http.MethodGet, path, nil, &result); err != nil {
		if IsSAMLEnforcement(err) && c.SSOFallbackEligible(ctx, owner) {
			err = c.anonGet(ctx, path, &result)
		}
		if err != nil {
			var httpErr *api.HTTPError
			if errors.As(err, &httpErr) && httpErr.StatusCode == http.StatusNotFound {
				return nil, nil
			}
			return nil, fmt.Errorf("fetching release for %s/%s@%s: %w", owner, repo, tag, c.ssoErr(owner, err))
		}
	}
	if result.TagName != tag {
		return nil, fmt.Errorf("release response for %s/%s@%s identified %q", owner, repo, tag, result.TagName)
	}
	return &RepoRelease{TagName: result.TagName, PublishedAt: result.PublishedAt, Immutable: result.Immutable}, nil
}
