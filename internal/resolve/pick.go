package resolve

import parserlock "github.com/github/actions-lockfile/go/pkg/lockfile"

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
// a SHA. Prefers hintRef, then highest semver, then lexicographic first.
func pickPreferredTag(candidates []string, hintRef string) string {
	if hit := hintMatch(candidates, hintRef); hit != "" {
		return hit
	}
	var best string
	var bestVer parserlock.SemVer
	haveSemver := false
	for _, c := range candidates {
		sv, ok := parserlock.ParseSemVer(c)
		if !ok {
			continue
		}
		if !haveSemver || sv.Greater(bestVer) {
			best, bestVer, haveSemver = c, sv, true
		}
	}
	if haveSemver {
		return best
	}
	return pickPreferred(candidates, hintRef, "")
}
