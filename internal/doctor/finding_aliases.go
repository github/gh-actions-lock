package doctor

import (
	"github.com/github/gh-actions-pin/pkg/findings"
)

// This file owns the boundary between internal/doctor and pkg/findings.
//
// Type aliases avoid string-conversion churn while preserving the historical
// Category-prefixed names used by internal call sites.

// Category classifies the state of a workflow or individual action dependency.
type Category = findings.Category

// Severity indicates how serious a finding is.
type Severity = findings.Severity

// Confidence describes how strongly the check stands behind a finding.
// See pkg/findings.Confidence for the full description.
type Confidence = findings.Confidence

// Category constants — aliases for pkg/findings categories under the
// historical `Category…` naming.
const (
	CategoryNotPinned       = findings.NotPinned
	CategorySHAAsRef        = findings.ShaAsRef
	CategoryStale           = findings.Stale
	CategoryRefChanged      = findings.RefChanged
	CategoryImpostorCommit  = findings.ImpostorCommit
	CategoryMisleadingSHA   = findings.MisleadingSHA
	CategoryLockfileForgery = findings.LockfileForgery
	CategoryRefMoved        = findings.RefMoved
	CategoryValid           = findings.Valid
	CategoryRunOnly         = findings.RunOnly
	// CategoryAncestryUnknown — Compare API couldn't classify the
	// pinned SHA's relationship to the ref's history (typically a
	// rate-limit fallback). Diagnostic; non-blocking.
	CategoryAncestryUnknown = findings.AncestryUnknown
	// CategoryReachabilityUnknown — branch_commits couldn't decide
	// whether the pinned SHA is still reachable upstream (resolver
	// failure, rate limit). Diagnostic; non-blocking.
	CategoryReachabilityUnknown = findings.ReachabilityUnknown
	// CategoryOnboardingRequired — set by `upgrade --no-onboard` when
	// the targeted workflow has no existing entry in lockfile.workflows{}.
	CategoryOnboardingRequired = findings.OnboardingRequired
)

// Severity constants — aliases for pkg/findings severities.
const (
	SeverityOK      = findings.SeverityOK
	SeverityInfo    = findings.SeverityInfo
	SeverityWarning = findings.SeverityWarning
	SeverityError   = findings.SeverityError
)

// Confidence constants — aliases for pkg/findings confidence levels.
const (
	ConfidenceLow    = findings.ConfidenceLow
	ConfidenceMedium = findings.ConfidenceMedium
	ConfidenceHigh   = findings.ConfidenceHigh
)
