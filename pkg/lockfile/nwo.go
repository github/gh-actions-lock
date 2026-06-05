package lockfile

import "strings"

// SplitNWO splits an owner/repo (Name-With-Owner) string into its two
// components. It returns ok=false for inputs that don't carry both an
// owner and a repo segment: the empty string, anything without a slash,
// a leading slash ("/repo"), and a trailing slash without a repo
// ("owner/").
//
// For inputs with extra path segments ("owner/repo/sub/..."), only the
// first two segments are returned; the remainder is dropped. This
// matches Dependency.OwnerRepo and the lockfile's repo-granularity
// pin grammar (sub-action paths are graph traversal details, not pin
// identity).
//
// SplitNWO does not validate the owner/repo character set — use
// ParseActionRef when parsing a verbatim `uses:` value where stricter
// charset rules apply.
func SplitNWO(nwo string) (owner, repo string, ok bool) {
	slashIdx := strings.IndexByte(nwo, '/')
	if slashIdx <= 0 || slashIdx == len(nwo)-1 {
		return "", "", false
	}
	owner = nwo[:slashIdx]
	rest := nwo[slashIdx+1:]
	if i := strings.IndexByte(rest, '/'); i > 0 {
		repo = rest[:i]
	} else {
		repo = rest
	}
	return owner, repo, true
}
