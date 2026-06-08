package tag

import (
	"context"
	"sort"
	"strings"
	"time"

	parserlock "github.com/github/actions-lockfile/go/pkg/lockfile"
	"github.com/github/gh-actions-pin/internal/ghapi"
	"github.com/github/gh-actions-pin/internal/syncmap"
	"golang.org/x/sync/singleflight"
)

// Info represents a tag with optional release metadata.
type Info struct {
	Name         string // e.g. "v4.2.2"
	SHA          string // commit SHA the tag points to (dereferenced for annotated tags)
	TagObjectSHA string // annotated tag object SHA; immutable release pins resolve to this, not the commit
	IsRelease    bool   // true if a GitHub Release exists for this tag
	IsImmutable  bool   // true if the release is marked immutable (tag can't be moved/deleted)
	IsMajor      bool   // true if this looks like a major-only tag (e.g. "v4")
}

// MatchesSHA reports whether the given pinned SHA identifies this tag. It
// matches both the dereferenced commit SHA and, for annotated tags, the tag
// object SHA. Immutable releases are pinned to the tag object SHA, so callers
// must compare against both to recognize such pins.
func (t Info) MatchesSHA(sha string) bool {
	if sha == "" {
		return false
	}
	if strings.EqualFold(t.SHA, sha) {
		return true
	}
	return t.TagObjectSHA != "" && strings.EqualFold(t.TagObjectSHA, sha)
}

// RepoInfo holds repository metadata relevant for pinning decisions.
type RepoInfo struct {
	DefaultBranch string // e.g. "main"
	Visibility    string // "public", "private", or "internal"
	PushedAt      string // ISO 8601 timestamp of last push
}

// IsInternal returns true for private or internal repos — those where
// pinning to the default branch is a reasonable option.
func (ri RepoInfo) IsInternal() bool {
	return ri.Visibility == "private" || ri.Visibility == "internal"
}

// Lister fetches tags and release metadata for action repos.
//
// Safe for concurrent use. The caches are syncmap.Map (the same primitive
// ghapi.Client and resolve.Resolver use); network fetches run outside any
// lock and are deduplicated per-repo via singleflight, so workers fetching
// different repos run in parallel while concurrent calls for the same repo
// collapse into one request.
type Lister struct {
	client       *ghapi.Client
	tagCache     syncmap.Map[ghapi.Repo, []Info]
	repoCache    syncmap.Map[ghapi.Repo, *RepoInfo]
	releaseDates syncmap.Map[ghapi.Repo, map[string]string] // owner/repo → tag → published_at
	cooldown     CooldownConfig
	tagsSF       singleflight.Group
	repoSF       singleflight.Group
}

// NewLister creates a Lister with the given API client and cooldown config.
func NewLister(client *ghapi.Client, cooldown CooldownConfig) *Lister {
	return &Lister{
		client:   client,
		cooldown: cooldown,
	}
}

// ListTags fetches tags for an action repo, enriched with release metadata.
// Results are cached per owner/repo. Network IO runs without the cache
// mutex held; concurrent calls for the same repo are coalesced via
// singleflight, so multiple pin workers fetching different repos proceed
// in parallel.
func (tl *Lister) ListTags(ctx context.Context, owner, repo string) ([]Info, error) {
	key := ghapi.ForRepo(owner, repo)
	if cached, ok := tl.tagCache.Get(key); ok {
		return cached, nil
	}

	res, err, _ := tl.tagsSF.Do(key.String(), func() (any, error) {
		// Re-check cache after acquiring singleflight slot in case a peer
		// completed the fetch while we were waiting.
		if cached, ok := tl.tagCache.Get(key); ok {
			return cached, nil
		}

		tags, err := tl.fetchTags(ctx, owner, repo)
		if err != nil {
			return nil, err
		}

		// Enrich with tag-object SHAs so we can recognize immutable-release pins,
		// which target the annotated tag object rather than the peeled commit.
		if objSHAs, err := tl.fetchTagObjectSHAs(ctx, owner, repo); err == nil {
			for i := range tags {
				if obj, ok := objSHAs[tags[i].Name]; ok && !strings.EqualFold(obj, tags[i].SHA) {
					tags[i].TagObjectSHA = obj
				}
			}
		}

		releaseTagSet, err := tl.fetchReleaseTags(ctx, owner, repo)
		if err != nil {
			// Non-fatal — releases are optional enrichment.
			releaseTagSet = make(map[string]releaseInfo)
		}

		for i := range tags {
			if ri, ok := releaseTagSet[tags[i].Name]; ok {
				tags[i].IsRelease = ri.IsRelease
				tags[i].IsImmutable = ri.IsImmutable
			}
			if sv, ok := parserlock.ParseSemVer(tags[i].Name); ok {
				tags[i].IsMajor = sv.IsMajorOnly()
			}
		}

		// Sort: latest semver first, major tags last.
		sort.Slice(tags, func(i, j int) bool {
			if tags[i].IsMajor != tags[j].IsMajor {
				return !tags[i].IsMajor
			}
			// Semver-aware ordering so v10 sorts ahead of v9 (string
			// compare would invert them). Non-semver tags fall back to
			// lexical and sort after semver tags.
			svi, oki := parserlock.ParseSemVer(tags[i].Name)
			svj, okj := parserlock.ParseSemVer(tags[j].Name)
			if oki && okj {
				if svi.Greater(svj) {
					return true
				}
				if svj.Greater(svi) {
					return false
				}
				return tags[i].Name > tags[j].Name
			}
			if oki != okj {
				return oki
			}
			return tags[i].Name > tags[j].Name
		})

		tl.tagCache.Put(key, tags)
		return tags, nil
	})
	if err != nil {
		return nil, err
	}
	return res.([]Info), nil
}

// LookupTag returns the Info for a specific tag name, or nil if not found.
// Uses the cached tag list from ListTags.
func (tl *Lister) LookupTag(ctx context.Context, owner, repo, tagName string) *Info {
	all, err := tl.ListTags(ctx, owner, repo)
	if err != nil {
		return nil
	}
	for i := range all {
		if all[i].Name == tagName {
			return &all[i]
		}
	}
	return nil
}

// LatestStableTag returns the latest non-major stable tag that passes cooldown.
// It skips major-only tags (e.g. "v4"), pre-release tags, and tags younger
// than the cooldown period. Returns ("", nil) if no suitable tag is found.
func (tl *Lister) LatestStableTag(ctx context.Context, owner, repo string) (string, error) {
	all, err := tl.ListTags(ctx, owner, repo)
	if err != nil {
		return "", err
	}
	for _, t := range all {
		if t.IsMajor {
			continue
		}
		sv, ok := parserlock.ParseSemVer(t.Name)
		if !ok || sv.Rest != "" {
			continue // skip pre-release, non-semver
		}
		if tl.isTagTooNew(owner, repo, t.Name) {
			continue
		}
		return t.Name, nil
	}
	return "", nil
}

func (tl *Lister) fetchTags(ctx context.Context, owner, repo string) ([]Info, error) {
	repoTags, err := tl.client.RepoTags(ctx, owner, repo)
	if err != nil {
		return nil, err
	}
	tags := make([]Info, 0, len(repoTags))
	for _, t := range repoTags {
		tags = append(tags, Info{Name: t.Name, SHA: t.SHA})
	}
	return tags, nil
}

// fetchTagObjectSHAs returns a map of tag name → the SHA of the underlying git
// object the ref points at (the tag object SHA for annotated tags, the commit
// SHA for lightweight tags). Immutable releases are pinned to the annotated
// tag object SHA, which the repos/tags endpoint dereferences away, so we read
// the raw refs here to recover it.
func (tl *Lister) fetchTagObjectSHAs(ctx context.Context, owner, repo string) (map[string]string, error) {
	refs, err := tl.client.MatchingTagRefs(ctx, owner, repo)
	if err != nil {
		return nil, err
	}
	out := make(map[string]string, len(refs))
	for _, r := range refs {
		if r.Name == "" || r.ObjectSHA == "" {
			continue
		}
		out[r.Name] = r.ObjectSHA
	}
	return out, nil
}

// ReleaseInfo holds release metadata for a tag.
type ReleaseInfo struct {
	TagName     string
	PublishedAt string // ISO 8601 date
}

// releaseInfo holds the release/immutable status for a tag.
type releaseInfo struct {
	IsRelease   bool
	IsImmutable bool
}

func (tl *Lister) fetchReleaseTags(ctx context.Context, owner, repo string) (map[string]releaseInfo, error) {
	releases, err := tl.client.Releases(ctx, owner, repo)
	if err != nil {
		return nil, err
	}

	set := make(map[string]releaseInfo, len(releases))
	for _, rel := range releases {
		set[rel.TagName] = releaseInfo{IsRelease: true, IsImmutable: rel.Immutable}
	}

	// Cache release dates. Build the per-tag map locally and store it in one
	// Put so the cache entry is written atomically (no read-modify-write).
	dates := make(map[string]string, len(releases))
	for _, rel := range releases {
		if rel.PublishedAt != "" {
			dates[rel.TagName] = rel.PublishedAt
		}
	}
	tl.releaseDates.Put(ghapi.ForRepo(owner, repo), dates)
	return set, nil
}

// ReleaseDate returns the published_at date for a tag, if available.
func (tl *Lister) ReleaseDate(owner, repo, tag string) string {
	if dates, ok := tl.releaseDates.Get(ghapi.ForRepo(owner, repo)); ok {
		return dates[tag]
	}
	return ""
}

// isTagTooNew returns true if the tag's release date is younger than the cooldown period.
// Tags without a known release date are never filtered (we can't determine their age).
func (tl *Lister) isTagTooNew(owner, repo, tag string) bool {
	days := tl.cooldown.CooldownDays(owner, repo)
	if days <= 0 {
		return false
	}
	isoDate := tl.ReleaseDate(owner, repo, tag)
	if isoDate == "" {
		return false
	}
	t, err := time.Parse(time.RFC3339, isoDate)
	if err != nil {
		return false
	}
	return time.Since(t) < time.Duration(days)*24*time.Hour
}

// GetRepoInfo fetches repository visibility and default branch. Cached per
// owner/repo. Concurrent calls for different repos run in parallel; calls
// for the same repo are coalesced via singleflight.
func (tl *Lister) GetRepoInfo(ctx context.Context, owner, repo string) (*RepoInfo, error) {
	key := ghapi.ForRepo(owner, repo)
	if cached, ok := tl.repoCache.Get(key); ok {
		return cached, nil
	}

	res, err, _ := tl.repoSF.Do(key.String(), func() (any, error) {
		if cached, ok := tl.repoCache.Get(key); ok {
			return cached, nil
		}

		meta, err := tl.client.RepoMetadata(ctx, owner, repo)
		if err != nil {
			return nil, err
		}

		info := &RepoInfo{
			DefaultBranch: meta.DefaultBranch,
			Visibility:    meta.Visibility,
			PushedAt:      meta.PushedAt,
		}
		tl.repoCache.Put(key, info)
		return info, nil
	})
	if err != nil {
		return nil, err
	}
	return res.(*RepoInfo), nil
}

// BranchHeadSHA returns the latest commit SHA on the given branch.
func (tl *Lister) BranchHeadSHA(ctx context.Context, owner, repo, branch string) (string, error) {
	return tl.client.CommitSHA(ctx, owner, repo, branch)
}
