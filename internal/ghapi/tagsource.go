package ghapi

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// RepoTag is a tag name paired with the commit SHA it resolves to, as
// returned by the repos/tags endpoint (annotated tags dereferenced).
type RepoTag struct {
	Name string
	SHA  string
}

// RepoTags lists a repository's tags (up to 100), dereferencing annotated
// tags to their target commit SHA.
func (c *Client) RepoTags(ctx context.Context, owner, repo string) ([]RepoTag, error) {
	path := fmt.Sprintf("repos/%s/%s/tags?per_page=100",
		url.PathEscape(owner), url.PathEscape(repo))

	var apiTags []struct {
		Name   string `json:"name"`
		Commit struct {
			SHA string `json:"sha"`
		} `json:"commit"`
	}
	if err := c.rest.DoWithContext(ctx, http.MethodGet, path, nil, &apiTags); err != nil {
		return nil, fmt.Errorf("fetching tags for %s/%s: %w", owner, repo, err)
	}

	out := make([]RepoTag, 0, len(apiTags))
	for _, t := range apiTags {
		out = append(out, RepoTag{Name: t.Name, SHA: t.Commit.SHA})
	}
	return out, nil
}

// TagObjectRef is a tag ref paired with the SHA and type of the object it
// points at: the tag object SHA for annotated tags, the commit SHA for
// lightweight tags. Name has the refs/tags/ prefix stripped.
type TagObjectRef struct {
	Name       string
	ObjectSHA  string
	ObjectType string
}

// MatchingTagRefs lists raw tag refs (up to 100) without dereferencing
// annotated tag objects, so callers can recover the tag object SHA that
// immutable release pins resolve to.
func (c *Client) MatchingTagRefs(ctx context.Context, owner, repo string) ([]TagObjectRef, error) {
	path := fmt.Sprintf("repos/%s/%s/git/matching-refs/tags?per_page=100",
		url.PathEscape(owner), url.PathEscape(repo))

	var refs []struct {
		Ref    string `json:"ref"`
		Object struct {
			SHA  string `json:"sha"`
			Type string `json:"type"`
		} `json:"object"`
	}
	if err := c.rest.DoWithContext(ctx, http.MethodGet, path, nil, &refs); err != nil {
		return nil, fmt.Errorf("fetching tag refs for %s/%s: %w", owner, repo, err)
	}

	out := make([]TagObjectRef, 0, len(refs))
	for _, r := range refs {
		out = append(out, TagObjectRef{
			Name:       strings.TrimPrefix(r.Ref, "refs/tags/"),
			ObjectSHA:  r.Object.SHA,
			ObjectType: r.Object.Type,
		})
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
		return nil, fmt.Errorf("fetching releases for %s/%s: %w", owner, repo, err)
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
// push time.
func (c *Client) RepoMetadata(ctx context.Context, owner, repo string) (RepoMetadata, error) {
	path := fmt.Sprintf("repos/%s/%s",
		url.PathEscape(owner), url.PathEscape(repo))

	var result struct {
		DefaultBranch string `json:"default_branch"`
		Visibility    string `json:"visibility"`
		PushedAt      string `json:"pushed_at"`
	}
	if err := c.rest.DoWithContext(ctx, http.MethodGet, path, nil, &result); err != nil {
		return RepoMetadata{}, fmt.Errorf("fetching metadata for %s/%s: %w", owner, repo, err)
	}
	return RepoMetadata(result), nil
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
		return "", fmt.Errorf("resolving %s/%s@%s: %w", owner, repo, ref, err)
	}
	return result.SHA, nil
}
