package lockfile

// Semver is gh-actions-pin's bespoke version parser. The Go stdlib has no
// semver package; golang.org/x/mod/semver exists but enforces strict
// semver 2.0 (rejects bare "v4" or "v4.2"), drops the v prefix, and
// doesn't expose MajorTag/MinorTag re-pinning helpers. Action tags
// routinely use bare-major refs and we need the "v" preserved so
// rewritten refs match the publisher's tag scheme — so we roll our own.

import (
	"fmt"
	"regexp"
	"strconv"

	parserlock "github.com/github/gh-actions-pin/pkg/lockfile"
)

// Semver holds parsed version components.
type Semver struct {
	Prefix string // "v" or ""
	Major  int
	Minor  int
	Patch  int
	Rest   string // anything after patch (e.g. "-beta.1")
	Raw    string
}

var semverRE = regexp.MustCompile(`^(v?)(\d+)(?:\.(\d+))?(?:\.(\d+))?(.*)$`)

// ParseSemver parses a version tag into its components. Returns false if the
// tag doesn't look like a version (or is a full SHA that happens to start with
// a digit).
func ParseSemver(tag string) (Semver, bool) {
	if parserlock.IsFullSha(tag) {
		return Semver{}, false
	}
	m := semverRE.FindStringSubmatch(tag)
	if m == nil {
		return Semver{}, false
	}
	// Reject components that overflow int: Atoi returns MaxInt on range
	// error, which would let a crafted tag (e.g. "999...9.0.0") masquerade
	// as the highest version.
	major, err := strconv.Atoi(m[2])
	if err != nil {
		return Semver{}, false
	}
	minor := 0
	if m[3] != "" {
		if minor, err = strconv.Atoi(m[3]); err != nil {
			return Semver{}, false
		}
	}
	patch := 0
	if m[4] != "" {
		if patch, err = strconv.Atoi(m[4]); err != nil {
			return Semver{}, false
		}
	}
	return Semver{
		Prefix: m[1],
		Major:  major,
		Minor:  minor,
		Patch:  patch,
		Rest:   m[5],
		Raw:    tag,
	}, true
}

func (s Semver) MajorTag() string { return fmt.Sprintf("%s%d", s.Prefix, s.Major) }
func (s Semver) MinorTag() string { return fmt.Sprintf("%s%d.%d", s.Prefix, s.Major, s.Minor) }

// Version returns the [3]int tuple for comparison.
func (s Semver) Version() [3]int { return [3]int{s.Major, s.Minor, s.Patch} }

// Greater reports whether s should be preferred over o: higher
// major.minor.patch wins; on a tie a stable version beats a pre-release, a
// v-prefixed tag beats the same bare version, then a lexicographic compare of
// the raw tags provides a deterministic final tie-break.
func (s Semver) Greater(o Semver) bool {
	sv, ov := s.Version(), o.Version()
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
func (s Semver) IsStable() bool { return s.Rest == "" }

// IsFullSemver returns true if the version has all three components
// (major.minor.patch) and no pre-release suffix. Tags like "v4" or "v4.2"
// return false.
func (s Semver) IsFullSemver() bool {
	return s.Rest == "" && s.Raw != s.MajorTag() && s.Raw != s.MinorTag()
}
