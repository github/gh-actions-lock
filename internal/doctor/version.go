package doctor

import (
	"strings"

	"github.com/github/gh-actions-pin/internal/lockfile"
)

// IsMutableVersionTag returns true if ref looks like a mutable version tag
// (major-only like "v4" or minor-only like "v4.2") that should be narrowed
// to a specific patch version for pinning.
func IsMutableVersionTag(ref string) bool {
	sv, ok := lockfile.ParseVersion(ref)
	if !ok {
		return false
	}
	return !sv.IsFull()
}

// IsNarrowedVersion returns true if narrowed is a more specific patch version
// of mutable. For example: mutable="v4", narrowed="v4.1.0" → true.
// mutable="v4.2", narrowed="v4.2.1" → true. mutable="v4", narrowed="v5.0.0" → false.
func IsNarrowedVersion(mutable, narrowed string) bool {
	mv, mOK := lockfile.ParseVersion(mutable)
	nv, nOK := lockfile.ParseVersion(narrowed)
	if !mOK || !nOK {
		return false
	}
	if !nv.IsFull() {
		return false
	}
	if mv.Major != nv.Major {
		return false
	}
	if mutable != mv.MajorTag() && mv.Minor != nv.Minor {
		return false
	}
	return true
}

// IsUpgrade returns true if moving from currentRef to latestRef is a real
// version upgrade. Returns false for noops where the current ref is already
// at or more specific than the latest (e.g. v4.0.0 → v4, v3.1.1 → v3).
func IsUpgrade(currentRef, latestRef string) bool {
	if currentRef == latestRef {
		return false
	}
	cur, curOK := lockfile.ParseVersion(currentRef)
	lat, latOK := lockfile.ParseVersion(latestRef)
	if !curOK || !latOK {
		if !latOK {
			return false
		}
		return true
	}
	if lat.Rest != "" {
		return false
	}
	if lat.Major < cur.Major {
		return false
	}
	if lat.Major == cur.Major {
		if lat.Minor == 0 && lat.Patch == 0 && cur.Minor >= 0 {
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
