package diagnostics

import parserlock "github.com/github/gh-actions-pin/pkg/lockfile"

// UsesRef is a single `uses:` reference extracted from a workflow. How the
// caller derived it (regex, parser walk, anything else) is its own
// business — the engine sees only the resolved owner/repo/path/ref tuple
// plus an optional source position.
type UsesRef struct {
	Owner    string
	Repo     string
	Path     string // empty for root actions
	Ref      string
	Position Position
}

// IndexKey returns "owner/repo@ref" with the owner+repo lowercased to
// match the lockfile pin grammar. The Path segment is intentionally
// excluded: the lockfile keys at repository granularity, so two
// sub-actions in the same repository at the same ref share an index key
// (and a lock entry).
func (u UsesRef) IndexKey() string {
	return parserlock.IndexKey(u.Owner, u.Repo, u.Ref)
}

// WorkflowInput pairs a workflow path with the uses: refs found in it.
type WorkflowInput struct {
	Path string
	Uses []UsesRef
}
