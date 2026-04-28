package doctor

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/github/gh-actions-pin/internal/lockfile"
)

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
	// Full hex commit SHAs must never parse as versions — the regex can
	// match SHAs starting with a digit (e.g. "1e7e51e…" → major=1, junk rest).
	if lockfile.IsFullSHA(tag) {
		return semver{}, false
	}
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

// IsFullSemver returns true if the version has all three components (major.minor.patch)
// and no pre-release suffix. Tags like "v4" or "v4.2" return false.
func (s semver) IsFullSemver() bool {
	return s.Rest == "" && s.Raw != s.MajorTag() && s.Raw != s.MinorTag()
}

// IsMutableVersionTag returns true if ref looks like a mutable version tag
// (major-only like "v4" or minor-only like "v4.2") that should be narrowed
// to a specific patch version for pinning.
func IsMutableVersionTag(ref string) bool {
	// Full commit SHAs are never version tags — the semver regex can
	// accidentally match hex SHAs that start with a digit (e.g. "1e7e51e…"
	// parses as major=1 with junk suffix).
	if lockfile.IsFullSHA(ref) {
		return false
	}
	sv, ok := parseSemver(ref)
	if !ok {
		return false
	}
	return !sv.IsFullSemver()
}

// IsNarrowedVersion returns true if narrowed is a more specific patch version
// of mutable. For example: mutable="v4", narrowed="v4.1.0" → true.
// mutable="v4.2", narrowed="v4.2.1" → true. mutable="v4", narrowed="v5.0.0" → false.
func IsNarrowedVersion(mutable, narrowed string) bool {
	mv, mOK := parseSemver(mutable)
	nv, nOK := parseSemver(narrowed)
	if !mOK || !nOK {
		return false
	}
	if !nv.IsFullSemver() {
		return false
	}
	// Major must match.
	if mv.Major != nv.Major {
		return false
	}
	// If mutable specifies minor (e.g. "v4.2"), narrowed minor must match.
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
