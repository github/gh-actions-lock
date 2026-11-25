package resolve

import (
	parserlock "github.com/github/actions-lockfile/go/pkg/lockfile"

	"github.com/github/gh-actions-lock/internal/tag"
)

// hintMatch returns hintRef if it is non-empty and present in candidates.
func hintMatch(candidates []string, hintRef string) string {
	if hintRef == "" {
		return ""
	}
	for _, c := range candidates {
		if c == hintRef {
			return c
		}
	}
	return ""
}

// pickPreferred returns hintRef if it appears in candidates, else
// defaultPick if present, else the lexicographically-first candidate.
func pickPreferred(candidates []string, hintRef, defaultPick string) string {
	if len(candidates) == 0 {
		return ""
	}
	if hit := hintMatch(candidates, hintRef); hit != "" {
		return hit
	}
	if defaultPick != "" {
		for _, c := range candidates {
			if c == defaultPick {
				return c
			}
		}
	}
	best := candidates[0]
	for _, c := range candidates[1:] {
		if c < best {
			best = c
		}
	}
	return best
}

// pickPreferredTag selects the canonical tag from the set of tags pointing at
// a SHA. Priority: hintRef (if present), then full semver (v1.2.3 — no splat),
// then any semver (including major-only like v4), then lexicographic first.
// Semver ranking only considers the hint's tag-prefix family (unprefixed
// when the hint has none), so "actions/verify/v0.5.1" never outranks "v1".
func pickPreferredTag(candidates []string, hintRef string) string {
	if hit := hintMatch(candidates, hintRef); hit != "" {
		return hit
	}
	var bestFull string
	var bestFullVer parserlock.SemVer
	var bestAny string
	var bestAnyVer parserlock.SemVer
	haveFull, haveAny := false, false
	family := ""
	if hint, ok := tag.ParseVersion(hintRef); ok {
		family = tag.Family(hint)
	}
	for _, c := range candidates {
		sv, ok := tag.ParseVersion(c)
		if !ok || tag.Family(sv) != family {
			continue
		}
		if sv.IsFull() && (!haveFull || sv.Greater(bestFullVer)) {
			bestFull, bestFullVer, haveFull = c, sv, true
		}
		if !haveAny || sv.Greater(bestAnyVer) {
			bestAny, bestAnyVer, haveAny = c, sv, true
		}
	}
	if haveFull {
		return bestFull
	}
	if haveAny {
		return bestAny
	}
	return pickPreferred(candidates, hintRef, "")
}
