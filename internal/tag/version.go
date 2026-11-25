package tag

import (
	"strings"

	parserlock "github.com/github/actions-lockfile/go/pkg/lockfile"
)

// ParseVersion extends parserlock.ParseSemVer to path-prefixed tags such as
// "actions/verify/v0.5.1", the Go-module style a multi-action repository uses
// to version each action separately. The path is folded into Prefix
// ("actions/verify/v") so MajorTag, MinorTag and IsFull keep working, and Raw
// keeps the full tag. Prefixed versions must end at the patch number or a
// "-"/"+" suffix, so branch names like "feature/2fa" don't parse.
func ParseVersion(name string) (parserlock.SemVer, bool) {
	i := strings.LastIndex(name, "/")
	if i < 0 {
		return parserlock.ParseSemVer(name)
	}
	sv, ok := parserlock.ParseSemVer(name[i+1:])
	if !ok || (sv.Rest != "" && !strings.ContainsAny(sv.Rest[:1], "-+")) {
		return parserlock.SemVer{}, false
	}
	sv.Prefix = name[:i+1] + sv.Prefix
	sv.Raw = name
	return sv, true
}

// Family identifies the tag series a version belongs to: "" for bare and
// v-prefixed tags, otherwise the path ("actions/verify/"). Versions are only
// comparable within one family, matching Dependabot's tag-prefix rule.
func Family(sv parserlock.SemVer) string {
	return strings.TrimSuffix(sv.Prefix, "v")
}
