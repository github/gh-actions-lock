// Package diagnostics is the lockfile diagnostic engine shared across
// surfaces: the gh-actions-pin CLI, the workflow language service, and any
// future host (web editor, server-side validators).
//
// The package is deliberately decoupled from the workflow parser: it takes
// pre-extracted uses: references as input rather than parsed templates, so
// it can travel with the lockfile package across hosts.
package diagnostics

// Code is the stable string identifier for a diagnostic class. The TS twin
// must use the same strings.
type Code string

const (
	// CodeNotPinned: a workflow uses: reference has no entry in the lockfile.
	CodeNotPinned Code = "not_pinned"
	// CodeShaAsRef: a uses: ref is a bare commit SHA with no symbolic ref.
	CodeShaAsRef Code = "sha_as_ref"
	// CodeRefChanged: workflow uses: ref differs from the lock entry's ref
	// for the same owner/repo[/path].
	CodeRefChanged Code = "ref_changed"
	// CodeStale: lockfile contains a workflow dep entry that no uses:
	// reference in that workflow points at.
	CodeStale Code = "stale"
	// CodeTransitiveUnlocked: a composite-action chain reaches an action
	// with no lock entry. Requires an ActionFileProvider.
	CodeTransitiveUnlocked Code = "transitive_unlocked"
	// CodeMisleadingSha: a ref looks like a SHA but the resolver maps it to
	// a different commit. Resolver-bound.
	CodeMisleadingSha Code = "misleading_sha"
	// CodeRefMoved: the upstream ref now resolves to a different SHA than
	// the one in the lockfile. Resolver-bound; legitimate for mutable tags.
	CodeRefMoved Code = "ref_moved"
	// CodeLockfileForgery: the locked SHA is not an ancestor of the live
	// SHA for the ref. Indicates tampering or history rewrite.
	CodeLockfileForgery Code = "lockfile_forgery"
	// CodeImposterCommit: the locked SHA is not reachable from the ref's
	// history (fork-network injection). Resolver-bound.
	CodeImposterCommit Code = "imposter_commit"
)

// Severity is a hint to the host about how to surface the finding. Hosts
// may remap (CLI exit codes, LSP DiagnosticSeverity) but the engine emits
// a consistent default.
type Severity string

const (
	SeverityInfo    Severity = "info"
	SeverityWarning Severity = "warning"
	SeverityError   Severity = "error"
)

// Position is an optional line/column range in the workflow file. Hosts
// that derived uses: refs from a parser can populate it; callers using a
// regex scanner can leave it zero.
type Position struct {
	StartLine   int
	StartColumn int
	EndLine     int
	EndColumn   int
}

// Finding is a single diagnostic.
type Finding struct {
	Code         Code
	Severity     Severity
	WorkflowPath string

	// Owner, Repo, Path, Ref identify the action the finding is about. Path
	// is the in-repo sub-path for monorepo actions (empty for root actions).
	// The lockfile pin grammar keys at owner/repo level — Path is preserved
	// here for source-location surfacing only.
	Owner string
	Repo  string
	Path  string
	Ref   string

	// LockedSha is the SHA recorded in the lockfile entry, if present.
	LockedSha string
	// LiveSha is the resolver-derived upstream SHA, if available.
	LiveSha string
	// ParentNwo is the dep key of the direct action that pulls in this
	// transitive dependency. Empty for direct findings.
	ParentNwo string

	Position    Position
	Message     string
	Remediation string
}

// Nwo returns "owner/repo" without the optional path segment.
func (f Finding) Nwo() string {
	return f.Owner + "/" + f.Repo
}
