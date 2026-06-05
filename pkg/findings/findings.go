// Package findings is the staged-for-extraction vocabulary of diagnostic
// categories, severities, and confidence levels used by gh-actions-pin and
// any future consumer that wants to reason about action-pinning findings
// without taking a dependency on the CLI's internals.
//
// This package is intentionally pure: it imports nothing outside the
// standard library. No HTTP, no filesystem, no go-gh, no internal/*.
// Finding is a data struct — constructors do no validation, no network
// calls, no I/O.
//
// Stability contract:
//   - Category, Severity, and Confidence string values are part of the
//     public schema. Renaming any of them is a breaking change. The
//     TestCategoryStringsAreFrozen / TestSeverityStringsAreFrozen /
//     TestConfidenceStringsAreFrozen tests guard against accidental
//     renames.
//   - Finding struct fields are additive-only post-cut. Adding fields is
//     fine; renaming or removing one is a breaking change.
//   - Severity and Confidence are deliberately separate axes: how bad
//     the issue would be if real, vs. how certain we are that it is real.
package findings

// Category classifies the state of a workflow or individual action
// dependency. The string values are semver-protected (see package
// docs).
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
)

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

// Confidence describes how strongly the producer stands behind a
// finding, modeled on zizmor's audit output. It complements Severity
// (how bad the issue would be if real) by capturing how certain we are
// that the issue IS real. The three levels are deliberately coarse —
// finer gradations invite bikeshedding without helping consumers act.
type Confidence string

const (
	// ConfidenceLow signals the finding is informational or rests on
	// a signal that could not be fully verified (resolver/network
	// failure, reachability inconclusive). Treat as a hint, not a
	// verdict.
	ConfidenceLow Confidence = "low"
	// ConfidenceMedium signals the finding rests on a fallback
	// inference — no authoritative answer was available (rate
	// limit, partial API response) and the finding was inferred
	// from secondary shape (tag-object peel, AncestryUnknown).
	// Likely real but worth double-checking.
	ConfidenceMedium Confidence = "medium"
	// ConfidenceHigh signals the finding rests on direct,
	// authoritative data: a string mismatch the user can verify by
	// eye, an exact SHA comparison, or a reachability answer from
	// the upstream API. Act on these.
	ConfidenceHigh Confidence = "high"
)

// Location identifies where in a source artifact a finding originated.
// All fields are optional: an empty Location is valid for findings that
// don't bind to a file (e.g. repo-level findings).
type Location struct {
	// URI is the location's source identifier — typically a
	// repo-relative path to a workflow YAML or lockfile YAML. May
	// also be a URL for non-file sources.
	URI string
	// Line is the 1-based line number; 0 means unspecified.
	Line int
	// Column is the 1-based column number; 0 means unspecified.
	Column int
}

// Subject identifies the action dependency a finding is about.
// All fields are optional; populate what the producer knows.
type Subject struct {
	// NWO is the owner/repo (or owner/repo/path for subdirectory
	// actions) the finding is about.
	NWO string
	// Ref is the workflow-declared ref (tag, branch, or SHA) for
	// the action.
	Ref string
	// SHA is the resolved commit SHA for the action, when known.
	SHA string
}

// Finding is a single diagnosed issue (or clean bill) emitted by a
// producer. It is pure data: no methods, no validation. Producers
// populate the fields they know; consumers tolerate empty fields.
//
// This struct is the staged-for-extraction public shape. Internal
// producers (internal/doctor) carry richer per-finding context on
// their own Finding type and translate to this shape at the boundary
// when needed.
type Finding struct {
	// Category is the kind of finding (see Category constants).
	Category Category
	// Severity is how serious the finding is if real.
	Severity Severity
	// Confidence is how certain the producer is that the finding is
	// real.
	Confidence Confidence
	// Message is a human-readable explanation of the finding.
	Message string
	// Remediation describes how the operator can resolve the
	// finding.
	Remediation string
	// Location identifies where in a source artifact the finding
	// originated. May be empty for repo-level findings.
	Location Location
	// Subject identifies the action dependency the finding is
	// about. May be empty for findings not tied to an action.
	Subject Subject
	// ObservedSHA is the SHA the resolver got at scan time,
	// recorded when it differs from the pinned SHA (e.g.
	// ref-moved, misleading-sha, lockfile-forgery). Empty when
	// not applicable.
	ObservedSHA string
	// HelpURI points to documentation explaining the finding.
	// Empty when no URL is mapped.
	HelpURI string
}

// Report aggregates findings emitted by a single producer run. Future
// aggregate fields (counts, summary, metadata) may be added — the
// struct is additive-only post-cut.
type Report struct {
	// Findings is the flat list of findings the producer emitted.
	Findings []Finding
}
