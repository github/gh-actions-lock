package ghapi

import (
	"regexp"
	"strings"
	"unicode/utf8"
)

// ValidRefName reports whether name is a short tag or branch name that
// `git check-ref-format --allow-onelevel` accepts, minus a leading "-".
// A name that fails it may be a revision expression (main~1, v1^, v1@{1})
// that a commit-ish lookup would evaluate instead of resolving literally.
// The rules match actions-lockfile's RefName so the CLI and library agree.
func ValidRefName(name string) bool {
	if name == "" || !utf8.ValidString(name) {
		return false
	}
	if name == "@" || name[0] == '-' || name[0] == '/' || strings.HasSuffix(name, "/") || strings.HasSuffix(name, ".") {
		return false
	}
	if strings.Contains(name, "..") || strings.Contains(name, "@{") || strings.Contains(name, "//") {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		if c < 0x20 || c == 0x7f {
			return false
		}
		switch c {
		case ' ', '~', '^', ':', '?', '*', '[', '\\':
			return false
		}
	}
	for _, comp := range strings.Split(name, "/") {
		if strings.HasPrefix(comp, ".") || strings.HasSuffix(comp, ".lock") {
			return false
		}
	}
	return true
}

// describeSuffix matches the "-g<hex>" ending of a `git describe` name.
var describeSuffix = regexp.MustCompile(`-g[0-9a-fA-F]{4,}$`)

// DescribeName reports whether name is a valid ref name shaped like
// `git describe` output, which the Actions backend refuses.
func DescribeName(name string) bool {
	return ValidRefName(name) && describeSuffix.MatchString(name)
}

// RecordableRefName reports whether name can be recorded as a lock entry's
// ref: a short tag or branch name the Actions backend accepts, not shaped
// like a full commit SHA and not a refs/ name. lockfile.ValidEntryRef and
// every reverse-lookup candidate use it.
func RecordableRefName(name string) bool {
	if name == "" || strings.HasPrefix(name, "refs/") || fullSHAShaped(name) {
		return false
	}
	return AcceptedRefName(name)
}

func fullSHAShaped(s string) bool {
	if len(s) != 40 && len(s) != 64 {
		return false
	}
	for _, r := range s {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f' || r >= 'A' && r <= 'F') {
			return false
		}
	}
	return true
}

// AcceptedRefName reports whether name is a ref name the Actions backend
// accepts: a valid ref name that isn't shaped like `git describe` output and
// has no @, which the backend's `uses:` grammar rejects.
func AcceptedRefName(name string) bool {
	return ValidRefName(name) && !describeSuffix.MatchString(name) && !strings.Contains(name, "@")
}
