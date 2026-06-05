package lockfile

import (
	"fmt"
	"regexp"
	"strconv"

	parserlock "github.com/github/gh-actions-pin/pkg/lockfile"
)

// Version holds parsed version components.
type Version struct {
	Prefix string // "v" or ""
	Major  int
	Minor  int
	Patch  int
	Rest   string // anything after patch (e.g. "-beta.1")
	Raw    string
}

var versionRE = regexp.MustCompile(`^(v?)(\d+)(?:\.(\d+))?(?:\.(\d+))?(.*)$`)

// ParseVersion parses a version tag into its components. Returns false if the
// tag doesn't look like a version (or is a full SHA that happens to start with
// a digit).
func ParseVersion(tag string) (Version, bool) {
	if parserlock.IsFullSha(tag) {
		return Version{}, false
	}
	m := versionRE.FindStringSubmatch(tag)
	if m == nil {
		return Version{}, false
	}
	// Reject components that overflow int: Atoi returns MaxInt on range
	// error, which would let a crafted tag (e.g. "999...9.0.0") masquerade
	// as the highest version.
	major, err := strconv.Atoi(m[2])
	if err != nil {
		return Version{}, false
	}
	minor := 0
	if m[3] != "" {
		if minor, err = strconv.Atoi(m[3]); err != nil {
			return Version{}, false
		}
	}
	patch := 0
	if m[4] != "" {
		if patch, err = strconv.Atoi(m[4]); err != nil {
			return Version{}, false
		}
	}
	return Version{
		Prefix: m[1],
		Major:  major,
		Minor:  minor,
		Patch:  patch,
		Rest:   m[5],
		Raw:    tag,
	}, true
}

func (s Version) MajorTag() string { return fmt.Sprintf("%s%d", s.Prefix, s.Major) }
func (s Version) MinorTag() string { return fmt.Sprintf("%s%d.%d", s.Prefix, s.Major, s.Minor) }

// Greater reports whether s should be preferred over o: higher
// major.minor.patch wins; on a tie a stable version beats a pre-release, a
// v-prefixed tag beats the same bare version, then a lexicographic compare of
// the raw tags provides a deterministic final tie-break.
func (s Version) Greater(o Version) bool {
	sv := [3]int{s.Major, s.Minor, s.Patch}
	ov := [3]int{o.Major, o.Minor, o.Patch}
	for i := 0; i < 3; i++ {
		if sv[i] != ov[i] {
			return sv[i] > ov[i]
		}
	}
	if s.IsStable() != o.IsStable() {
		return s.IsStable()
	}
	if (s.Prefix == "v") != (o.Prefix == "v") {
		return s.Prefix == "v"
	}
	return s.Raw > o.Raw
}

// IsStable returns true if the tag has no pre-release suffix or trailing junk.
func (s Version) IsStable() bool { return s.Rest == "" }

// IsFull returns true if the version has all three components
// (major.minor.patch) and no pre-release suffix. Tags like "v4" or "v4.2"
// return false.
func (s Version) IsFull() bool {
	return s.Rest == "" && s.Raw != s.MajorTag() && s.Raw != s.MinorTag()
}
