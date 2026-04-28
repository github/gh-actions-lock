package lockfile

import (
	"fmt"
	"regexp"
	"strconv"
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
	if IsFullSHA(tag) {
		return Semver{}, false
	}
	m := semverRE.FindStringSubmatch(tag)
	if m == nil {
		return Semver{}, false
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

// IsStable returns true if the tag has no pre-release suffix or trailing junk.
func (s Semver) IsStable() bool { return s.Rest == "" }

// IsFullSemver returns true if the version has all three components
// (major.minor.patch) and no pre-release suffix. Tags like "v4" or "v4.2"
// return false.
func (s Semver) IsFullSemver() bool {
	return s.Rest == "" && s.Raw != s.MajorTag() && s.Raw != s.MinorTag()
}
