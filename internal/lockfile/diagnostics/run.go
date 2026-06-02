package diagnostics

import (
	"context"

	parserlock "github.com/github/gh-actions-pin/pkg/lockfile"
)

// Options configures a Run.
type Options struct {
	// Resolver enables the resolver-bound validators (misleading_sha,
	// ref_moved, lockfile_forgery, imposter_commit). When nil, those
	// validators are skipped silently.
	Resolver Resolver

	// ActionFileProvider enables the transitive_unlocked validator. When
	// nil, that validator is skipped.
	ActionFileProvider ActionFileProvider
}

// Run is the engine entrypoint. It evaluates all enabled validators
// against the given lockfile and workflow inputs and returns a flat list
// of findings. The order is stable: findings for earlier workflows come
// first, and within a workflow the order tracks the catalog order in
// codes.go.
//
// Run never returns an error: validators that can't answer (no resolver,
// resolver returned Unknown, action file fetch failed) skip silently. The
// caller's host integration is responsible for surfacing degraded modes.
func Run(ctx context.Context, lf parserlock.File, workflows []WorkflowInput, opts Options) []Finding {
	var findings []Finding
	for _, wf := range workflows {
		findings = append(findings, runOne(ctx, lf, wf, opts)...)
	}
	return findings
}

func runOne(ctx context.Context, lf parserlock.File, wf WorkflowInput, opts Options) []Finding {
	var out []Finding

	// Build lockfile views once per workflow.
	wfEntry, hasWorkflowEntry := lf.LookupWorkflow(wf.Path)
	depPins, depIndex := parseWorkflowDeps(wfEntry)

	// 1. Structural: NOT_PINNED + SHA_AS_REF + REF_CHANGED.
	out = append(out, checkNotPinned(wf, hasWorkflowEntry, depPins, depIndex)...)
	out = append(out, checkShaAsRef(wf, depIndex)...)
	out = append(out, checkRefChanged(wf, depPins)...)

	// 2. Structural: STALE.
	out = append(out, checkStale(wf, depPins)...)

	// 3. Resolver-bound checks.
	if opts.Resolver != nil {
		out = append(out, checkMisleadingSha(ctx, wf, opts.Resolver)...)
		refMoved := checkRefMovedAndForgery(ctx, wf, lf, depIndex, opts.Resolver)
		out = append(out, refMoved...)
		forgeryKeys := collectForgeryKeys(wf, refMoved)
		out = append(out, checkImposterCommit(ctx, wf, lf, depIndex, opts.Resolver, forgeryKeys)...)
	}

	return out
}

// collectForgeryKeys returns the set of uses: index keys already flagged
// LOCKFILE_FORGERY so the imposter_commit check can skip them.
func collectForgeryKeys(wf WorkflowInput, findings []Finding) map[string]bool {
	if len(findings) == 0 {
		return nil
	}
	byKey := make(map[string]UsesRef, len(wf.Uses))
	for _, u := range wf.Uses {
		byKey[u.IndexKey()] = u
	}
	out := make(map[string]bool)
	for _, f := range findings {
		if f.Code != CodeLockfileForgery {
			continue
		}
		u := UsesRef{Owner: f.Owner, Repo: f.Repo, Path: f.Path, Ref: f.Ref}
		if _, ok := byKey[u.IndexKey()]; ok {
			out[u.IndexKey()] = true
		}
	}
	return out
}

// parseWorkflowDeps decodes a workflow's dependency pin strings into Pins
// plus an index keyed by "owner/repo@ref". Unparseable entries are
// dropped silently — they're surfaced separately by parserlock.Parse callers.
func parseWorkflowDeps(rawDeps []string) ([]parserlock.Pin, map[string]parserlock.Pin) {
	pins := make([]parserlock.Pin, 0, len(rawDeps))
	idx := make(map[string]parserlock.Pin, len(rawDeps))
	for _, raw := range rawDeps {
		pin, ok := parserlock.ParsePin(raw)
		if !ok {
			continue
		}
		pins = append(pins, pin)
		idx[pin.IndexKey()] = pin
	}
	return pins, idx
}

// findingFromUse builds the common header fields from a UsesRef.
func findingFromUse(wf WorkflowInput, u UsesRef) Finding {
	return Finding{
		WorkflowPath: wf.Path,
		Owner:        u.Owner,
		Repo:         u.Repo,
		Path:         u.Path,
		Ref:          u.Ref,
		Position:     u.Position,
	}
}

// findingFromPin builds the common header fields from a lock entry pin.
// The lockfile pin grammar has no path segment, so Path is left empty.
func findingFromPin(workflowPath string, p parserlock.Pin) Finding {
	return Finding{
		WorkflowPath: workflowPath,
		Owner:        p.Owner,
		Repo:         p.Repo,
		Ref:          p.Ref,
		LockedSha:    p.Hex,
	}
}

// shortSha returns the first 12 chars of a SHA, or the SHA itself if shorter.
func shortSha(s string) string {
	if len(s) <= 12 {
		return s
	}
	return s[:12]
}
