package workflowfile

import "strings"

// selfRepoPrefix marks a self-referencing action `uses:` value. Such a
// reference resolves an action from the workflow's own repository at the
// running ref, so it is inherently pinned and carries no `@ref` suffix.
const selfRepoPrefix = "$/"

// IsSelfRepoAction reports whether a `uses:` value is a self-referencing
// action reference (`$/…`). These resolve against the defining repo at the
// running commit and need no lockfile SHA.
func IsSelfRepoAction(value string) bool {
	return strings.HasPrefix(strings.TrimSpace(value), selfRepoPrefix)
}

// SelfRepoRefHasVersion reports whether a `$/…` value carries an `@ref`
// suffix. A self-repo reference always resolves to the running ref, so any
// `@ref` is invalid — the malformed form `$/actions/foo@v1`.
func SelfRepoRefHasVersion(value string) bool {
	return IsSelfRepoAction(value) && strings.Contains(value, "@")
}
