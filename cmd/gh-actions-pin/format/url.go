package format

import "strings"

// TagObjectCheck reports whether the given SHA is a known annotated-tag
// object in owner/repo. Used by DepReleaseURL so it can pick /tree/<sha>
// over /commit/<sha> for tag objects (which 404 under /commit/). Pass nil
// to skip the check and always use /commit/<sha>. *resolver.Resolver's
// IsKnownTagObject method satisfies the signature directly.
type TagObjectCheck func(owner, repo, sha string) bool

// DepReleaseURL derives a GitHub URL from a dep key of the form
// "owner/repo[/path]@ref". Commit-SHA pins link to /commit/<sha> (the
// diff view). Annotated-tag-object SHAs link to /tree/<sha> instead —
// /commit/<tagobject-sha> returns 404 because the tag object is not a
// commit. Non-SHA refs link to /releases/tag/<ref>. A nil isTagObject
// (or one that returns false) falls back to the plain /commit/<sha> path.
func DepReleaseURL(dep string, isTagObject TagObjectCheck) string {
	nwo := dep
	ref := ""
	if i := strings.IndexByte(dep, '@'); i >= 0 {
		nwo = dep[:i]
		ref = dep[i+1:]
	}
	parts := strings.SplitN(nwo, "/", 3)
	if len(parts) < 2 {
		return ""
	}
	base := "https://github.com/" + parts[0] + "/" + parts[1]
	if ref != "" && isHexSHA(ref) {
		if isTagObject != nil && isTagObject(parts[0], parts[1], ref) {
			return base + "/tree/" + ref
		}
		return base + "/commit/" + ref
	}
	if ref != "" {
		return base + "/releases/tag/" + ref
	}
	return base + "/releases"
}

// isHexSHA reports whether s looks like a full 40-character hex commit SHA.
func isHexSHA(s string) bool {
	if len(s) != 40 {
		return false
	}
	for _, c := range s {
		switch {
		case c >= '0' && c <= '9', c >= 'a' && c <= 'f', c >= 'A' && c <= 'F':
		default:
			return false
		}
	}
	return true
}
