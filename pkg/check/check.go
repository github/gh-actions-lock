// Package check defines the Input fact bundle and Check interface that rule
// implementations operate over.
//
// Public reachability string values and exported field/method shapes are frozen
// by tests in this package.
package check

import (
	"github.com/github/gh-actions-pin/pkg/findings"
	"github.com/github/gh-actions-pin/pkg/lockfile"
)

// Input is the deterministic fact bundle a Check operates over. All
// fields are optional from the consumer's perspective: a Check that
// requires facts it didn't receive must degrade explicitly — emit a
// low-confidence informational finding or skip — rather than reach for
// hidden state.
type Input struct {
	// Workflows is the per-workflow extracted facts: path plus the
	// positioned action refs the parser found. May be empty.
	Workflows []WorkflowFacts
	// Lockfile is the parsed lockfile (nil when no lockfile exists or
	// the producer chose not to supply one).
	Lockfile *lockfile.File
	// Resolutions are the resolver-side facts (live SHAs, reachability,
	// per-action metadata). Producers without a resolver may leave
	// fields zero; checks must tolerate that.
	Resolutions ResolutionFacts
	// Options shapes evaluator behavior. The zero value selects defaults.
	Options Options
}

// WorkflowFacts captures the extracted facts about a single workflow file.
type WorkflowFacts struct {
	// Path is the repo-relative path to the workflow YAML file.
	Path string
	// ActionRefs is the flat list of `uses:` references the parser
	// extracted from this workflow, in source order.
	ActionRefs []ActionRefFact
}

// ActionRefFact pairs a parsed action ref with its source location so
// findings can point back at the originating workflow line.
type ActionRefFact struct {
	// Ref is the parsed `uses:` value.
	Ref lockfile.ActionRef
	// Location is where the ref was declared in the workflow source.
	// Empty when the producer did not record positions.
	Location findings.Location
}

// ResolutionFacts bundles the resolver-side facts a check may consume.
// Producers without a resolver leave fields zero; checks degrade accordingly.
type ResolutionFacts struct {
	// ResolvedRefs holds the live resolution of each `uses:` ref the
	// producer asked the resolver about. May be empty.
	ResolvedRefs []ResolvedRef
	// Reachability holds the per-dependency reachability check
	// results. May be empty.
	Reachability []Reachability
	// Metas maps a canonical action identity to the parsed subset of
	// its action.yml. The key format is "owner/repo[/path]@ref" — the
	// /path component is included because nested sub-actions in the
	// same repo+ref have distinct action.yml files. May be nil.
	Metas map[string]lockfile.ActionMeta
}

// ResolvedRef is the live resolution of a single `uses:` reference: the
// commit SHA the ref pointed at when the resolver ran, plus optional
// enrichment (tag, branch, repo identifiers) the resolver may have
// discovered along the way.
type ResolvedRef struct {
	// NWO is the owner/repo (no path) of the action.
	NWO string
	// Path is the optional sub-action subpath as written in `uses:`
	// (e.g. "save" for actions/cache/save). Empty for top-level
	// actions.
	Path string
	// Ref is the workflow-declared ref (tag, branch, or SHA) the
	// resolution was performed for.
	Ref string
	// SHA is the resolved commit hash. Empty when the resolver could
	// not produce one.
	SHA string
	// HashAlgo names the hash function used for SHA ("sha1" or
	// "sha256"). Empty when the producer did not record it; consumers
	// may detect from SHA length as a fallback.
	HashAlgo string
	// Tag is the discovered release/tag pointing at SHA, if any.
	// Optional enrichment populated by pin-time discovery.
	Tag string
	// Branch is the discovered branch containing SHA. Optional
	// enrichment populated by pin-time discovery.
	Branch string
	// OwnerID is the GitHub numeric owner ID. 0 means unknown (not
	// recorded by the producer).
	OwnerID int64
	// RepoID is the GitHub numeric repo ID. 0 means unknown (not
	// recorded by the producer).
	RepoID int64
}

// ReachabilityStatus reports whether a pinned SHA is on the lineage of a named
// ref. The string values are part of the public schema.
type ReachabilityStatus string

const (
	// Reachable means the SHA is confirmed on the ref's lineage.
	Reachable ReachabilityStatus = "reachable"
	// Unreachable means the SHA is confirmed NOT on the ref's
	// lineage — for example, it exists only in a fork network.
	Unreachable ReachabilityStatus = "unreachable"
	// ReachabilityUnknown means the check could not be completed
	// (timeout, rate limit, API error, or the producer did not run
	// the check).
	ReachabilityUnknown ReachabilityStatus = "unknown"
)

// Reachability is the outcome of a single (NWO, Ref, SHA) reachability
// check. Producers without a resolver omit Reachability entries
// entirely; checks that depend on reachability treat absence as
// ReachabilityUnknown.
type Reachability struct {
	// NWO is the owner/repo (no path) the check was performed for.
	NWO string
	// Ref is the workflow-declared ref the SHA should be reachable
	// from.
	Ref string
	// SHA is the pinned commit hash whose reachability was checked.
	SHA string
	// Status is the check outcome (see the ReachabilityStatus
	// constants).
	Status ReachabilityStatus
	// Detail is a human-readable explanation (e.g. compare-API
	// status, error message). May be empty.
	Detail string
	// FullScanUsed is true when the commit was not found in the
	// canonical "likely" branch set (default branch, protected
	// branches, release/v*, literal ref, lockfile hint) and the
	// resolver had to fall back to scanning every branch in the
	// repo. Even when Status is Reachable, a full-scan fallback
	// means the commit is not on a canonical branch — a notable
	// signal worth surfacing.
	FullScanUsed bool
}

// Options shapes evaluator behavior. The zero value selects defaults.
type Options struct{}

// Check is the unit of rule evaluation. Given the same Input, it must produce
// the same findings without network, filesystem, or wall-clock dependency.
type Check interface {
	// Name returns the stable kebab-case identifier for this check.
	// Names align with findings.Category values where a rule
	// produces a single category (e.g. "not-pinned", "sha-as-ref"),
	// but a single Check may emit findings from multiple
	// categories.
	Name() string
	// Evaluate runs the check against input and returns any findings
	// it produced. A nil or empty slice means the check found
	// nothing — equivalent to a clean bill for this rule. Evaluate
	// must not retain references to slices or maps inside input
	// beyond the call; consumers are free to reuse the bundle.
	Evaluate(input Input) []findings.Finding
}
