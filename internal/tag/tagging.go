package tag

import (
	"context"
	"fmt"

	"time"

	parserlock "github.com/github/actions-lockfile/go/pkg/lockfile"
)

// Suggestion is a tag paired with why it's being suggested.
type Suggestion struct {
	Tag       Info
	Reason    string
	Preferred bool // true if this tag points directly at the pinned SHA
}

// PickerTag is a tag formatted for the interactive picker.
type PickerTag struct {
	Tag       Info
	Label     string // formatted display label
	Installed bool   // true if this tag matches the currently pinned SHA
}

// BestPatchTagForSHA returns the highest full-semver patch tag pointing at the
// given SHA, or "" if none exists. This is used to narrow mutable version refs
// (like "v4") to a specific patch version (like "v4.2.1") when pinning.
func (tl *Lister) BestPatchTagForSHA(ctx context.Context, owner, repo, sha string) (string, error) {
	matching, err := tl.TagsForSHA(ctx, owner, repo, sha)
	if err != nil {
		return "", err
	}

	var best parserlock.SemVer
	bestFound := false
	for _, t := range matching {
		if t.IsMajor {
			continue
		}
		sv, ok := parserlock.ParseSemVer(t.Name)
		if !ok || !sv.IsFull() {
			continue
		}
		if !bestFound || sv.Major > best.Major ||
			(sv.Major == best.Major && sv.Minor > best.Minor) ||
			(sv.Major == best.Major && sv.Minor == best.Minor && sv.Patch > best.Patch) {
			best = sv
			bestFound = true
		}
	}

	if !bestFound {
		return "", nil
	}
	return best.Raw, nil
}

// UniquePatchTagForRef returns the sole full-semver patch tag that matches the
// given ref's family, or "" if the choice is ambiguous (0 or 2+ candidates).
// For "v9" it only considers v9.x.y tags; for "v4.2" only v4.2.x tags.
// This is used for auto-pinning: if there's exactly one obvious patch tag,
// we can pin without prompting.
func (tl *Lister) UniquePatchTagForRef(ctx context.Context, owner, repo, sha, ref string) (string, error) {
	refSV, refOK := parserlock.ParseSemVer(ref)
	if !refOK {
		return "", nil
	}

	matching, err := tl.TagsForSHA(ctx, owner, repo, sha)
	if err != nil {
		return "", err
	}

	var candidates []parserlock.SemVer
	for _, t := range matching {
		if t.IsMajor {
			continue
		}
		sv, ok := parserlock.ParseSemVer(t.Name)
		if !ok || !sv.IsFull() {
			continue
		}
		// Must be in the same family as the original ref.
		if sv.Major != refSV.Major {
			continue
		}
		// If original ref specifies minor (e.g. "v4.2"), patch must match that minor.
		if ref != refSV.MajorTag() && sv.Minor != refSV.Minor {
			continue
		}
		candidates = append(candidates, sv)
	}

	if len(candidates) != 1 {
		return "", nil
	}
	return candidates[0].Raw, nil
}

// TagsForSHA returns all tags whose commit SHA matches the given SHA.
func (tl *Lister) TagsForSHA(ctx context.Context, owner, repo, sha string) ([]Info, error) {
	all, err := tl.ListTags(ctx, owner, repo)
	if err != nil {
		return nil, err
	}
	var matched []Info
	for _, t := range all {
		if t.MatchesSHA(sha) {
			matched = append(matched, t)
		}
	}
	return matched, nil
}

// SuggestTagsForSHA returns a curated set of tag suggestions for a pinned SHA.
// It includes exact-match tags, plus major/minor family alternatives when the
// match is version-like. Returns at most 5 suggestions.
func (tl *Lister) SuggestTagsForSHA(ctx context.Context, owner, repo, sha string) ([]Suggestion, error) {
	matching, err := tl.TagsForSHA(ctx, owner, repo, sha)
	if err != nil {
		return nil, err
	}

	if len(matching) == 0 {
		return nil, nil
	}

	var suggestions []Suggestion

	// Find the best semver match to derive family tags.
	var bestSV parserlock.SemVer
	bestFound := false
	for _, t := range matching {
		if sv, ok := parserlock.ParseSemVer(t.Name); ok && sv.Rest == "" && !t.IsMajor {
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
		suggestions = append(suggestions, Suggestion{
			Tag:       t,
			Reason:    "exact match for pinned SHA",
			Preferred: true,
		})
		seen[t.Name] = true
	}

	// Suggest major/minor family tags if the best match is semver-ish.
	if bestFound {
		allTags, _ := tl.ListTags(ctx, owner, repo)

		// Look for the major tag (e.g. v4).
		majorName := bestSV.MajorTag()
		if !seen[majorName] {
			for _, t := range allTags {
				if t.Name == majorName {
					suggestions = append(suggestions, Suggestion{
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
					suggestions = append(suggestions, Suggestion{
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

// CuratePickerTags returns a short list of the most useful tags for a picker.
// Shows the latest patch per major version (up to 3 majors), marks the one
// matching pinnedSHA as "installed", and only puts 📦 on that one.
func (tl *Lister) CuratePickerTags(ctx context.Context, owner, repo, pinnedSHA string) ([]PickerTag, error) {
	all, err := tl.ListTags(ctx, owner, repo)
	if err != nil {
		return nil, err
	}
	if len(all) == 0 {
		return nil, nil
	}

	// Pick the latest full-version tag per major (skip major-only tags, pre-releases).
	type majorBucket struct {
		major int
		tag   Info
	}
	seen := make(map[int]bool)
	var buckets []majorBucket

	for _, t := range all {
		if t.IsMajor {
			continue
		}
		sv, ok := parserlock.ParseSemVer(t.Name)
		if !ok || sv.Rest != "" {
			continue
		}
		// Skip tags younger than the cooldown period.
		if tl.isTagTooNew(owner, repo, t.Name) && !t.MatchesSHA(pinnedSHA) {
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
		installed := b.tag.MatchesSHA(pinnedSHA)
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
			if t.MatchesSHA(pinnedSHA) && !t.IsMajor {
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

// URL returns a clickable GitHub releases tag URL.
func URL(owner, repo, tag string) string {
	return fmt.Sprintf("https://github.com/%s/%s/releases/tag/%s", owner, repo, tag)
}

// ReorderSuggestions puts full semver tags first (recommended for pinning),
// then major-only tags last. Within full semver: immutable releases first,
// then regular releases, then plain tags.
func ReorderSuggestions(suggestions []Suggestion) []Suggestion {
	out := make([]Suggestion, 0, len(suggestions))
	// Immutable releases first.
	for _, s := range suggestions {
		if !s.Tag.IsMajor && s.Tag.IsImmutable {
			out = append(out, s)
		}
	}
	// Regular releases.
	for _, s := range suggestions {
		if !s.Tag.IsMajor && s.Tag.IsRelease && !s.Tag.IsImmutable {
			out = append(out, s)
		}
	}
	// Non-release full version tags.
	for _, s := range suggestions {
		if !s.Tag.IsMajor && !s.Tag.IsRelease {
			out = append(out, s)
		}
	}
	// Major-only tags last.
	for _, s := range suggestions {
		if s.Tag.IsMajor {
			out = append(out, s)
		}
	}
	return out
}
