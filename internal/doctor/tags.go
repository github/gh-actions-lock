package doctor

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
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

// semver holds parsed version components.
type semver struct {
	Prefix string // "v" or ""
	Major  int
	Minor  int
	Patch  int
	Rest   string // anything after patch (e.g. "-beta.1")
	Raw    string
}

var semverRE = regexp.MustCompile(`^(v?)(\d+)(?:\.(\d+))?(?:\.(\d+))?(.*)$`)

func parseSemver(tag string) (semver, bool) {
	m := semverRE.FindStringSubmatch(tag)
	if m == nil {
		return semver{}, false
	}
	major, _ := strconv.Atoi(m[2])
	minor := 0
	if m[3] != "" {
		minor, _ = strconv.Atoi(m[3])
	}
	patch := 0
	if m[4] != "" {
		patch, _ = strconv.Atoi(m[4])
	}
	return semver{
		Prefix: m[1],
		Major:  major,
		Minor:  minor,
		Patch:  patch,
		Rest:   m[5],
		Raw:    tag,
	}, true
}

func (s semver) MajorTag() string { return fmt.Sprintf("%s%d", s.Prefix, s.Major) }
func (s semver) MinorTag() string { return fmt.Sprintf("%s%d.%d", s.Prefix, s.Major, s.Minor) }

// IsUpgrade returns true if moving from currentRef to latestRef is a real
// version upgrade. Returns false for noops where the current ref is already
// at or more specific than the latest (e.g. v4.0.0 → v4, v3.1.1 → v3).
func IsUpgrade(currentRef, latestRef string) bool {
	if currentRef == latestRef {
		return false
	}
	cur, curOK := parseSemver(currentRef)
	lat, latOK := parseSemver(latestRef)
	if !curOK || !latOK {
		// If we can't parse the latest as semver, it's not a valid upgrade target.
		if !latOK {
			return false
		}
		// Can't parse current but latest is valid — assume it's an upgrade.
		return true
	}
	// Skip pre-release in latest.
	if lat.Rest != "" {
		return false
	}
	// If latest major < current major, not an upgrade.
	if lat.Major < cur.Major {
		return false
	}
	// Same major — check if latest is just a less-specific version of current.
	if lat.Major == cur.Major {
		if lat.Minor == 0 && lat.Patch == 0 && cur.Minor >= 0 {
			// latest=v4, current=v4.x.y → noop (major tag covers current)
			// But also handle latest=v4 when the parsed minor/patch are 0
			// because "v4" has no minor/patch components.
			if latestRef == lat.MajorTag() {
				return false
			}
		}
		if lat.Minor == cur.Minor && lat.Patch <= cur.Patch {
			return false
		}
		if lat.Minor < cur.Minor {
			return false
		}
	}
	return true
}

// RepoInfo holds repository metadata relevant for pinning decisions.
type RepoInfo struct {
	DefaultBranch string // e.g. "main"
	Visibility    string // "public", "private", or "internal"
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

// TagsForSHA returns all tags whose commit SHA matches the given SHA.
func (tl *TagLister) TagsForSHA(owner, repo, sha string) ([]TagInfo, error) {
	all, err := tl.ListTags(owner, repo)
	if err != nil {
		return nil, err
	}
	var matched []TagInfo
	for _, t := range all {
		if strings.EqualFold(t.SHA, sha) {
			matched = append(matched, t)
		}
	}
	return matched, nil
}

// SuggestTagsForSHA returns a curated set of tag suggestions for a pinned SHA.
// It includes exact-match tags, plus major/minor family alternatives when the
// match is version-like. Returns at most 5 suggestions.
func (tl *TagLister) SuggestTagsForSHA(owner, repo, sha string) ([]TagSuggestion, error) {
	matching, err := tl.TagsForSHA(owner, repo, sha)
	if err != nil {
		return nil, err
	}

	if len(matching) == 0 {
		return nil, nil
	}

	var suggestions []TagSuggestion

	// Find the best semver match to derive family tags.
	var bestSV semver
	bestFound := false
	for _, t := range matching {
		if sv, ok := parseSemver(t.Name); ok && sv.Rest == "" && !t.IsMajor {
			if !bestFound || sv.Major > bestSV.Major ||
				(sv.Major == bestSV.Major && sv.Minor > bestSV.Minor) ||
				(sv.Major == bestSV.Major && sv.Minor == bestSV.Minor && sv.Patch > bestSV.Patch) {
				bestSV = sv
				bestFound = true
			}
		}
	}

	seen := make(map[string]bool)

	// Add exact-match tags (non-major, non-minor-only) first.
	for _, t := range matching {
		if t.IsMajor {
			continue
		}
		if len(suggestions) >= 3 {
			break
		}
		suggestions = append(suggestions, TagSuggestion{
			Tag:       t,
			Reason:    "exact match for pinned SHA",
			Preferred: true,
		})
		seen[t.Name] = true
	}

	// Suggest major/minor family tags if the best match is semver-ish.
	if bestFound {
		allTags, _ := tl.ListTags(owner, repo)

		// Look for the major tag (e.g. v4).
		majorName := bestSV.MajorTag()
		if !seen[majorName] {
			for _, t := range allTags {
				if t.Name == majorName {
					suggestions = append(suggestions, TagSuggestion{
						Tag:    t,
						Reason: fmt.Sprintf("major tag (tracks latest %s.x.x)", majorName),
					})
					seen[majorName] = true
					break
				}
			}
		}

		// Look for a minor tag (e.g. v4.2) — not all repos have these.
		minorName := bestSV.MinorTag()
		if !seen[minorName] {
			for _, t := range allTags {
				if t.Name == minorName {
					suggestions = append(suggestions, TagSuggestion{
						Tag:    t,
						Reason: fmt.Sprintf("minor tag (tracks latest %s.x)", minorName),
					})
					seen[minorName] = true
					break
				}
			}
		}
	}

	// Cap at 5 total.
	if len(suggestions) > 5 {
		suggestions = suggestions[:5]
	}

	return suggestions, nil
}

// PickerTag is a tag formatted for the interactive picker.
type PickerTag struct {
	Tag       TagInfo
	Label     string // formatted display label
	Installed bool   // true if this tag matches the currently pinned SHA
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

// CuratePickerTags returns a short list of the most useful tags for a picker.
// Shows the latest patch per major version (up to 3 majors), marks the one
// matching pinnedSHA as "installed", and only puts 📦 on that one.
func (tl *TagLister) CuratePickerTags(owner, repo, pinnedSHA string) ([]PickerTag, error) {
	all, err := tl.ListTags(owner, repo)
	if err != nil {
		return nil, err
	}
	if len(all) == 0 {
		return nil, nil
	}

	// Pick the latest full-version tag per major (skip major-only tags, pre-releases).
	type majorBucket struct {
		major int
		tag   TagInfo
	}
	seen := make(map[int]bool)
	var buckets []majorBucket

	for _, t := range all {
		if t.IsMajor {
			continue
		}
		sv, ok := parseSemver(t.Name)
		if !ok || sv.Rest != "" {
			continue
		}
		// Skip tags younger than the cooldown period.
		if tl.isTagTooNew(owner, repo, t.Name) && !strings.EqualFold(t.SHA, pinnedSHA) {
			continue
		}
		if !seen[sv.Major] {
			seen[sv.Major] = true
			buckets = append(buckets, majorBucket{major: sv.Major, tag: t})
		}
		if len(buckets) >= 3 {
			break
		}
	}

	// Build picker entries.
	var result []PickerTag
	for _, b := range buckets {
		installed := strings.EqualFold(b.tag.SHA, pinnedSHA)
		result = append(result, PickerTag{
			Tag:       b.tag,
			Label:     b.tag.Name,
			Installed: installed,
		})
	}

	// If pinnedSHA matches a tag that isn't in our buckets, prepend it.
	pinnedFound := false
	for _, pt := range result {
		if pt.Installed {
			pinnedFound = true
			break
		}
	}
	if !pinnedFound {
		for _, t := range all {
			if strings.EqualFold(t.SHA, pinnedSHA) && !t.IsMajor {
				label := t.Name + "  📦 installed"
				result = append([]PickerTag{{
					Tag:       t,
					Label:     label,
					Installed: true,
				}}, result...)
				break
			}
		}
	}

	return result, nil
}

// TagSuggestion is a tag paired with why it's being suggested.
type TagSuggestion struct {
	Tag       TagInfo
	Reason    string
	Preferred bool // true if this tag points directly at the pinned SHA
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

// isMajorTag returns true if the tag looks like a major-only version (e.g. "v4", "v12").
func isMajorTag(tag string) bool {
	tag = strings.TrimPrefix(tag, "v")
	for _, c := range tag {
		if c < '0' || c > '9' {
			return false
		}
	}
	return len(tag) > 0
}

// FormatTagAge returns a relative age string like "3d ago" from an ISO 8601 timestamp.
func FormatTagAge(isoDate string) string {
	if isoDate == "" {
		return ""
	}
	t, err := time.Parse(time.RFC3339, isoDate)
	if err != nil {
		return ""
	}
	d := time.Since(t)
	switch {
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	case d < 30*24*time.Hour:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	case d < 365*24*time.Hour:
		return fmt.Sprintf("%dmo ago", int(d.Hours()/(24*30)))
	default:
		return fmt.Sprintf("%dy ago", int(d.Hours()/(24*365)))
	}
}

// TagURL returns a clickable GitHub releases tag URL.
func TagURL(owner, repo, tag string) string {
	return fmt.Sprintf("https://github.com/%s/%s/releases/tag/%s", owner, repo, tag)
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
	}
	if err := tl.client.Get(path, &result); err != nil {
		return nil, err
	}

	info := &RepoInfo{
		DefaultBranch: result.DefaultBranch,
		Visibility:    result.Visibility,
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
