// Package findings holds the diagnostic vocabulary (categories,
// severities, confidence levels) emitted by gh-actions-pin.
//
// The Category, Severity, and Confidence string values are part of the
// public schema; renaming one is a breaking change and the frozen-string
// tests in this package guard against it. Finding struct fields are
// additive-only after the initial cut.
package findings

// Category classifies the state of a workflow or action dependency.
type Category string

const (
	// NotPinned means the workflow has action refs but no
	// corresponding dependencies entry in the lockfile.
	NotPinned Category = "not-pinned"
	// ShaAsRef means a dependency is pinned to a bare SHA with no
	// human-readable tag ref alongside it.
	ShaAsRef Category = "sha-as-ref"
	// RefChanged means the workflow uses: ref was manually changed
	// (e.g. v6.2.0 → v6) and the lockfile no longer matches.
	RefChanged Category = "ref-changed"
	// RefMoved means the upstream tag now resolves to a different
	// SHA than what the lockfile has recorded.
	RefMoved Category = "ref-moved"
	// Stale means the pinned SHA no longer matches what the ref
	// resolves to today.
	Stale Category = "stale"
	// ImpostorCommit means the pinned SHA is not in the ref's git
	// history (possible fork-network commit). Matches zizmor's
	// impostor-commit audit ID.
	ImpostorCommit Category = "impostor-commit"
	// MisleadingSHA means a ref looks like a SHA but resolves to a
	// different commit.
	MisleadingSHA Category = "misleading-sha"
	// LockfileForgery means the pinned SHA is not an ancestor of the
	// current ref — the lockfile entry was likely injected or
	// tampered with.
	LockfileForgery Category = "lockfile-forgery"
	// Valid means the dependency is pinned and verified.
	Valid Category = "valid"
	// RunOnly means the workflow has no action refs (only run:
	// steps), so pinning is not applicable.
	RunOnly Category = "run-only"
	// AncestryUnknown means the Compare API couldn't decide whether
	// the pinned SHA is in the ref's history (typically rate-limited
	// or transient error). Non-blocking diagnostic: we know the SHAs
	// differ but can't classify the move as benign-but-known
	// (ref-moved) vs. tampered (lockfile-forgery).
	AncestryUnknown Category = "ancestry-unknown"
	// ReachabilityUnknown means branch_commits couldn't decide
	// whether the pinned SHA is still reachable from any branch in
	// the upstream repo (resolver failure, GraphQL rate limit, etc).
	// Non-blocking diagnostic: surfaced so consumers can retry rather
	// than treating the dep as verified.
	ReachabilityUnknown Category = "reachability-unknown"
	// OnboardingRequired means an `upgrade --no-onboard` run targeted
	// a workflow that has no existing entry in `lockfile.workflows{}`.
	// The CLI refuses to silently add it during a dependency-update
	// run; the operator must onboard the workflow explicitly before
	// re-running upgrade.
	OnboardingRequired Category = "onboarding-required"
)

// IsInconclusive reports whether c represents a diagnostic that
// couldn't reach a verdict (network/rate-limit fallback). These are
// surfaced as warnings but are not blocking: consumers (e.g.
// Dependabot FindingMapper) treat them as "scan inconclusive, retry"
// rather than "lockfile is bad".
func (c Category) IsInconclusive() bool {
	switch c {
	case AncestryUnknown, ReachabilityUnknown:
		return true
	}
	return false
}

// Severity indicates how serious a finding is if it represents a real
// problem. Pair with Confidence to express how strongly the tool stands
// behind the call.
type Severity string

const (
	// SeverityOK means the finding represents a clean state — no
	// action needed.
	SeverityOK Severity = "ok"
	// SeverityInfo is purely informational and does not require
	// action.
	SeverityInfo Severity = "info"
	// SeverityWarning indicates a concern worth surfacing but not
	// blocking on.
	SeverityWarning Severity = "warning"
	// SeverityError indicates a blocking issue the operator must
	// resolve.
	SeverityError Severity = "error"
)

// Confidence is how certain the producer is the finding is real,
// modeled on zizmor's audit output.
type Confidence string

const (
	// ConfidenceLow: signal could not be fully verified (resolver
	// failure, reachability inconclusive).
	ConfidenceLow Confidence = "low"
	// ConfidenceMedium: inferred from a fallback (tag-object peel,
	// ancestry unknown due to rate limit).
	ConfidenceMedium Confidence = "medium"
	// ConfidenceHigh: rests on authoritative data (exact SHA
	// comparison, upstream reachability answer).
	ConfidenceHigh Confidence = "high"
)

// Location identifies where in a source artifact a finding originated.
// All fields are optional; an empty Location is valid for repo-level
// findings that don't bind to a file.
type Location struct {
	// URI is typically a repo-relative path to a workflow or lockfile.
	URI string
	// Line is 1-based; 0 means unspecified.
	Line int
	// Column is 1-based; 0 means unspecified.
	Column int
}

// Subject identifies the action dependency a finding is about. All
// fields are optional; populate what the producer knows.
type Subject struct {
	// NWO is owner/repo, or owner/repo/path for subdirectory actions.
	NWO string
	// Ref is the workflow-declared ref (tag, branch, or SHA).
	Ref string
	// SHA is the resolved commit SHA, when known.
	SHA string
}

// Finding is a single diagnosed issue (or clean bill) emitted by a
// producer. Producers populate the fields they know; consumers
// tolerate empty fields.
//
// Internal producers (internal/doctor) carry richer per-finding
// context on their own Finding type and translate to this shape at
// the boundary.
type Finding struct {
	Category    Category
	Severity    Severity
	Confidence  Confidence
	Message     string
	Remediation string
	Location    Location
	Subject     Subject
	// ObservedSHA is the SHA the resolver got at scan time, recorded
	// when it differs from the pinned SHA (ref-moved, misleading-sha,
	// lockfile-forgery).
	ObservedSHA string
	// HelpURI points to documentation explaining the finding.
	HelpURI string
}

// Report aggregates findings from a single producer run. Fields are
// additive-only.
type Report struct {
	Findings []Finding
}
