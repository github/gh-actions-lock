package doctor

import (
	"github.com/github/gh-actions-pin/pkg/findings"
)

// This file owns the boundary between internal/doctor (CLI-internal
// diagnostic engine) and pkg/findings (the staged-for-extraction
// vocabulary package). External callers — cmd/, internal/runlog,
// internal/doctor's own checks — must consume these aliases instead of
// importing pkg/findings directly so a single file changes when the
// vocab moves to its own module.
//
// Type aliases are used (rather than wrapper types) because Category,
// Severity, and Confidence are shape-stable string vocabularies: they
// are intentionally part of pkg/findings' public contract. Wrapping
// would force string-conversion churn at every call site for zero
// change-isolation benefit.
//
// The Category constants keep their `Category…` prefix here (e.g.
// CategoryNotPinned) to match the long-standing internal call sites;
// pkg/findings exposes the same values under shorter names
// (findings.NotPinned) for new external consumers.

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
