package doctor

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/cli/go-gh/v2/pkg/api"
	"gopkg.in/yaml.v3"
)

// TagInfo represents a tag with optional release metadata.
type TagInfo struct {
	Name        string // e.g. "v4.2.2"
	SHA         string // commit SHA the tag points to (dereferenced for annotated tags)
	IsRelease   bool   // true if a GitHub Release exists for this tag
	IsImmutable bool   // true if the release is marked immutable (tag can't be moved/deleted)
	IsMajor     bool   // true if this looks like a major-only tag (e.g. "v4")
}

// RepoInfo holds repository metadata relevant for pinning decisions.
type RepoInfo struct {
	DefaultBranch string // e.g. "main"
	Visibility    string // "public", "private", or "internal"
	PushedAt      string // ISO 8601 timestamp of last push
}

// VisibilityLabel returns a human-readable label for display in prompts.
func (ri RepoInfo) VisibilityLabel() string {
	switch ri.Visibility {
	case "private", "internal":
		return "🏠 internal"
	default:
		return "public"
	}
}

// IsInternal returns true for private or internal repos — those where
// pinning to the default branch is a reasonable option.
func (ri RepoInfo) IsInternal() bool {
	return ri.Visibility == "private" || ri.Visibility == "internal"
}

// TagLister fetches tags and release metadata for action repos.
type TagLister struct {
	client       *api.RESTClient
	cache        map[string][]TagInfo
	repoCache    map[string]*RepoInfo
	releaseDates map[string]map[string]string // owner/repo → tag → published_at
	cooldown     CooldownConfig
}

// CooldownConfig controls how old a tag must be before we recommend it.
// Tags with a known release date younger than the threshold are excluded
// from suggestions and curated picks.
type CooldownConfig struct {
	DefaultDays int            // global default (0 = no filtering)
	RepoOverrides map[string]int // "owner/repo" → days override
}

// CooldownDays returns the cooldown period for a given repo.
func (c CooldownConfig) CooldownDays(owner, repo string) int {
	if days, ok := c.RepoOverrides[owner+"/"+repo]; ok {
		return days
	}
	return c.DefaultDays
}

// NewTagLister creates a TagLister with the given REST client.
func NewTagLister(client *api.RESTClient) *TagLister {
	return &TagLister{
		client:       client,
		cache:        make(map[string][]TagInfo),
		repoCache:    make(map[string]*RepoInfo),
		releaseDates: make(map[string]map[string]string),
		cooldown:     LoadCooldownConfig(),
	}
}

// ListTags fetches tags for an action repo, enriched with release metadata.
// Results are cached per owner/repo.
func (tl *TagLister) ListTags(owner, repo string) ([]TagInfo, error) {
	key := owner + "/" + repo
	if cached, ok := tl.cache[key]; ok {
		return cached, nil
	}

	tags, err := tl.fetchTags(owner, repo)
	if err != nil {
		return nil, err
	}

	releaseTagSet, err := tl.fetchReleaseTags(owner, repo)
	if err != nil {
		// Non-fatal — releases are optional enrichment.
		releaseTagSet = make(map[string]releaseInfo)
	}

	for i := range tags {
		if ri, ok := releaseTagSet[tags[i].Name]; ok {
			tags[i].IsRelease = ri.IsRelease
			tags[i].IsImmutable = ri.IsImmutable
		}
		tags[i].IsMajor = isMajorTag(tags[i].Name)
	}

	// Sort: latest semver first, major tags last.
	sort.Slice(tags, func(i, j int) bool {
		if tags[i].IsMajor != tags[j].IsMajor {
			return !tags[i].IsMajor
		}
		return tags[i].Name > tags[j].Name
	})

	tl.cache[key] = tags
	return tags, nil
}

// LookupTag returns the TagInfo for a specific tag name, or nil if not found.
// Uses the cached tag list from ListTags.
func (tl *TagLister) LookupTag(owner, repo, tagName string) *TagInfo {
	all, err := tl.ListTags(owner, repo)
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
func (tl *TagLister) LatestStableTag(owner, repo string) (string, error) {
	all, err := tl.ListTags(owner, repo)
	if err != nil {
		return "", err
	}
	for _, t := range all {
		if t.IsMajor {
			continue
		}
		sv, ok := parseSemver(t.Name)
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

func (tl *TagLister) fetchTags(owner, repo string) ([]TagInfo, error) {
	// Use the repos/tags endpoint — it dereferences annotated tags automatically.
	path := fmt.Sprintf("repos/%s/%s/tags?per_page=100",
		url.PathEscape(owner), url.PathEscape(repo))

	var apiTags []struct {
		Name   string `json:"name"`
		Commit struct {
			SHA string `json:"sha"`
		} `json:"commit"`
	}

	if err := tl.client.Get(path, &apiTags); err != nil {
		return nil, fmt.Errorf("fetching tags for %s/%s: %w", owner, repo, err)
	}

	tags := make([]TagInfo, 0, len(apiTags))
	for _, t := range apiTags {
		tags = append(tags, TagInfo{
			Name: t.Name,
			SHA:  t.Commit.SHA,
		})
	}
	return tags, nil
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

func (tl *TagLister) fetchReleaseTags(owner, repo string) (map[string]releaseInfo, error) {
	path := fmt.Sprintf("repos/%s/%s/releases?per_page=30",
		url.PathEscape(owner), url.PathEscape(repo))

	var releases []struct {
		TagName     string `json:"tag_name"`
		PublishedAt string `json:"published_at"`
		Immutable   bool   `json:"immutable"`
	}

	if err := tl.client.Get(path, &releases); err != nil {
		return nil, err
	}

	set := make(map[string]releaseInfo, len(releases))
	for _, rel := range releases {
		set[rel.TagName] = releaseInfo{IsRelease: true, IsImmutable: rel.Immutable}
	}

	// Cache release dates.
	key := owner + "/" + repo
	if _, ok := tl.releaseDates[key]; !ok {
		tl.releaseDates[key] = make(map[string]string)
	}
	for _, rel := range releases {
		if rel.PublishedAt != "" {
			tl.releaseDates[key][rel.TagName] = rel.PublishedAt
		}
	}
	return set, nil
}

// ReleaseDate returns the published_at date for a tag, if available.
func (tl *TagLister) ReleaseDate(owner, repo, tag string) string {
	key := owner + "/" + repo
	if dates, ok := tl.releaseDates[key]; ok {
		return dates[tag]
	}
	return ""
}

// LoadCooldownConfig reads cooldown settings from ~/.config/gh-actions-pin/config.yml.
// Returns sensible defaults (3 days) if the file doesn't exist or is malformed.
func LoadCooldownConfig() CooldownConfig {
	cfg := CooldownConfig{
		DefaultDays:   3,
		RepoOverrides: make(map[string]int),
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return cfg
	}
	data, err := os.ReadFile(filepath.Join(home, ".config", "gh-actions-pin", "config.yml"))
	if err != nil {
		return cfg
	}

	var file struct {
		CooldownDays int            `yaml:"cooldown_days"`
		Repos        map[string]struct {
			CooldownDays int `yaml:"cooldown_days"`
		} `yaml:"repos"`
	}
	if err := yaml.Unmarshal(data, &file); err != nil {
		return cfg
	}
	if file.CooldownDays > 0 {
		cfg.DefaultDays = file.CooldownDays
	}
	for nwo, repoCfg := range file.Repos {
		if repoCfg.CooldownDays >= 0 {
			cfg.RepoOverrides[nwo] = repoCfg.CooldownDays
		}
	}
	return cfg
}

// isTagTooNew returns true if the tag's release date is younger than the cooldown period.
// Tags without a known release date are never filtered (we can't determine their age).
func (tl *TagLister) isTagTooNew(owner, repo, tag string) bool {
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

// GetRepoInfo fetches repository visibility and default branch. Cached per owner/repo.
func (tl *TagLister) GetRepoInfo(owner, repo string) (*RepoInfo, error) {
	key := owner + "/" + repo
	if cached, ok := tl.repoCache[key]; ok {
		return cached, nil
	}

	path := fmt.Sprintf("repos/%s/%s",
		url.PathEscape(owner), url.PathEscape(repo))

	var result struct {
		DefaultBranch string `json:"default_branch"`
		Visibility    string `json:"visibility"`
		PushedAt      string `json:"pushed_at"`
	}
	if err := tl.client.Get(path, &result); err != nil {
		return nil, err
	}

	info := &RepoInfo{
		DefaultBranch: result.DefaultBranch,
		Visibility:    result.Visibility,
		PushedAt:      result.PushedAt,
	}
	tl.repoCache[key] = info
	return info, nil
}

// BranchHeadSHA returns the latest commit SHA on the given branch.
func (tl *TagLister) BranchHeadSHA(owner, repo, branch string) (string, error) {
	path := fmt.Sprintf("repos/%s/%s/commits/%s",
		url.PathEscape(owner), url.PathEscape(repo), url.PathEscape(branch))
	var result struct {
		SHA string `json:"sha"`
	}
	if err := tl.client.Get(path, &result); err != nil {
		return "", err
	}
	return result.SHA, nil
}
