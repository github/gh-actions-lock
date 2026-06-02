package diagnostics

import "strings"

// nwo returns lowercased "owner/repo". Trivial, but used in several
// validators.
func nwo(owner, repo string) string {
	return strings.ToLower(owner) + "/" + strings.ToLower(repo)
}
