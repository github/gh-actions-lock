// Package checks implements the structural, misleading-sha, and
// resolver-bound validators run against parsed workflows.
package checks

// Category classifies the state of a workflow or action dependency. The string
// values are part of the schema surfaced to consumers (SARIF rule IDs, JSON
// output, doc URL slugs); the frozen-strings test guards against accidental
// renames.
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
	// or transient error). Fail-closed: emitted at error severity so an
	// unverifiable symbolic-ref pin blocks rather than passing as a
	// benign-but-known (ref-moved) or tampered (lockfile-forgery) move.
	AncestryUnknown Category = "ancestry-unknown"
	// ReachabilityUnknown is a legacy inconclusive diagnostic. It is no
	// longer emitted (the resolve funnel now produces the blocking
	// reachability-unverified instead); the const and string are retained
	// for schema-freeze compatibility and back-references. Do not emit it
	// in new code — it routes through the non-blocking inconclusive path.
	ReachabilityUnknown Category = "reachability-unknown"
	// UnresolvableCommit means a full-SHA pin does not resolve to any
	// object in the upstream repo — the commit is missing or unreachable.
	// Emitted only on a definitive determination (clean GraphQL null, no
	// SAML/rate-limit/path-scoped error in play), so it is blocking:
	// error/high. Transient verification failures become
	// reachability-unverified instead.
	UnresolvableCommit Category = "unresolvable-commit"
	// ReachabilityUnverified means a re-resolution failed for a reason we
	// could not prove definitive — rate limit, SAML/SSO block, transport
	// error, or a path-scoped GraphQL failure. Fail-closed: blocking
	// (error/low) so an unverifiable pin can't pass, but framed to
	// consumers as transient ("re-run") rather than "the pin is bad".
	ReachabilityUnverified Category = "reachability-unverified"
	// OnboardingRequired means a `check --no-onboard` run encountered a
	// workflow (or an action within one) that has no existing entry in the
	// lockfile. Under --no-onboard the tool refuses to add new entries: the
	// workflow/action is skipped and surfaced rather than silently pinned.
	// Already-tracked entries are still re-pinned. The operator must onboard
	// explicitly (run `gh actions-lock` without --no-onboard) to add it.
	OnboardingRequired Category = "onboarding-required"
	// VersionRef is an informational nudge: a dependency is pinned with a
	// ref that is not a full semver tag (e.g. v4, v3.1, main). Full semver
	// tags (v4.2.1) each resolve to exactly one commit, making the lock
	// comment durable across re-pins.
	VersionRef Category = "version-ref"
	// LocalAction means the workflow uses at least one local path action
	// (uses: ./some-path). Lockfile onboarding is not supported for
	// workflows that reference local actions — the entire workflow is
	// skipped.
	LocalAction Category = "local-action"
)

// IsInconclusive reports whether c is the legacy non-blocking inconclusive
// diagnostic. Under the fail-closed contract this set has collapsed to a
// single retained-but-unemitted member (ReachabilityUnknown); every live
// "couldn't verify" path now emits a blocking error category instead
// (unresolvable-commit, reachability-unverified, ancestry-unknown). Kept so
// legacy references and rendering stay valid.
func (c Category) IsInconclusive() bool {
	switch c {
	case ReachabilityUnknown:
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
	// ConfidenceLow marks a signal that could not be fully verified
	// (resolver failure, reachability inconclusive).
	ConfidenceLow Confidence = "low"
	// ConfidenceMedium marks a signal inferred from a fallback
	// (tag-object peel, ancestry unknown due to rate limit).
	ConfidenceMedium Confidence = "medium"
	// ConfidenceHigh marks a signal resting on authoritative data
	// (exact SHA comparison, upstream reachability answer).
	ConfidenceHigh Confidence = "high"
)
