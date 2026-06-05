package lockfile

import "strings"

// usesIndexKey parses a workflow step `uses:` value into the canonical
// lockfile IndexKey ("owner/repo@ref"). Sub-action paths
// ("owner/repo/path@ref") collapse to "owner/repo@ref" because the
// lockfile keys on the repository — matching the runner's tarball download
// identity. Local actions ("./...", ".\\...") and "docker://" references
// are not lockable; ok=false. Malformed inputs also return ok=false.
func usesIndexKey(uses string) (string, bool) {
	if strings.HasPrefix(uses, "./") || strings.HasPrefix(uses, ".\\") || strings.HasPrefix(uses, "docker://") {
		return "", false
	}
	atIdx := strings.LastIndexByte(uses, '@')
	if atIdx <= 0 || atIdx == len(uses)-1 {
		return "", false
	}
	repoPath, ref := uses[:atIdx], uses[atIdx+1:]
	owner, repo, ok := SplitNWO(repoPath)
	if !ok {
		return "", false
	}
	return IndexKey(owner, repo, ref), true
}
