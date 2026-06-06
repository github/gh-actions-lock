package doctor

import (
	"errors"
	"fmt"
	"os"

	"github.com/github/gh-actions-pin/internal/audit"
	"github.com/github/gh-actions-pin/internal/lockfile"
	"github.com/github/gh-actions-pin/internal/resolve"
)

// directUsesFor returns the original `uses:` text (ActionRef.Raw) for the
// direct dep matching (owner, repo, ref), or "" if no direct ActionRef in
// this workflow matches. The auto-fix path needs Raw to feed
// File.RewriteActionRefs, which keys rewrites on the verbatim uses string;
// transitive impostors (matched only via parent composite actions) return
// "" so callers can fall back to alerting.
func directUsesFor(refs []lockfile.ActionRef, owner, repo, ref string) string {
	for _, ar := range refs {
		if ar.Owner == owner && ar.Repo == repo && ar.Ref == ref {
			return ar.Raw
		}
	}
	return ""
}

// startWork shows a spinner (TTY only) for the network-heavy portion of a
// remediation and wires the resolver's progress callback to its detail line.
// Pair every call with stopWork (defer). On non-TTY outputs it is a no-op so
// CI logs stay clean. Prompts never overlap a spinner: apply* runs after any
// interactive prompt in its handler.
func (rem *Remediator) startWork(label string) {
	if !rem.output.IsTTY() {
		return
	}
	// The parallel pin pool owns the spinner detail (per-worker rows) and
	// must not be perturbed by individual applyPin calls toggling the global
	// resolver progress callback.
	if rem.parallel {
		return
	}
	// A session-wide spinner already owns the screen; leave it running so we
	// don't blank the line between workflows. Its label and detail are driven
	// by the Remediate loop and the resolver progress callback.
	if rem.sessionProgress {
		return
	}
	rem.output.StartProgress(label)
	rem.resolver.ProgressFn = rem.output.UpdateProgress
}

func (rem *Remediator) stopWork() {
	if !rem.output.IsTTY() {
		return
	}
	if rem.parallel {
		return
	}
	if rem.sessionProgress {
		// Keep the session spinner alive; just clear stale detail so it
		// doesn't linger into the next workflow's label.
		rem.output.UpdateProgress("")
		return
	}
	rem.resolver.ProgressFn = nil
	rem.output.StopProgress()
}

// applyPin runs the full pin flow on an unpinned workflow.
func (rem *Remediator) applyPin(wr WorkflowReport) error {
	wf, err := lockfile.Load(wr.Path)
	if err != nil {
		return err
	}

	rem.startWork(rem.workLabel(fmt.Sprintf("Pinning %s", wr.Path)))
	defer rem.stopWork()

	deps, parentMap, err := rem.resolver.ResolveAllRecursive(rem.ctx, wr.ActionRefs)
	if err != nil {
		return fmt.Errorf("resolving actions: %w", err)
	}
	// Check for impostor commits at pin time — don't let fork-network
	// commits get pinned in the first place. Fail closed: if we can't
	// verify reachability (e.g. list-branches returns an error), refuse
	// to pin rather than silently accepting a potentially poisoned commit.
	//
	// Auto-fix runs upstream in remediateWorkflow before applyPin is
	// called: by the time we get here, the workflow file has already been
	// rewritten if a sane-release substitution was available. Anything
	// still reaching this loop with Status == Unreachable is unfixable
	// (no suggestion, transitive impostor, etc.) and must alert.
	//
	// Containment is per-dependency, not per-workflow: a workflow that
	// mixes one impostor with healthy actions drops only the impostor and
	// still pins the benign siblings. Skipping the whole workflow on the
	// first bad dep left those siblings out of the lockfile, so a second
	// run kept "finding" and re-pinning them — the non-convergence bug.
	reachResults := audit.New(rem.resolver).CheckReachabilityAll(rem.ctx, deps)
	badKeys := make(map[string]bool)
	for _, rr := range reachResults {
		depKey := rr.Owner + "/" + rr.Repo + "@" + rr.Ref
		switch rr.Status {
		case resolve.Unreachable:
			// Defense-in-depth invariant: diagnose's live-direct sweep
			// (liveReachImpostorFindings) is the primary detector for
			// this shape and would have produced a CategoryImpostorCommit
			// finding before remediation. Hitting this branch means a
			// resolver cache disagreement let one slip through; alert so
			// the run reports it, then drop it from the closure.
			rem.alertWorkflow(wr.Path, depKey, reasonForCategory(CategoryImpostorCommit),
				fmt.Sprintf("refusing to pin: impostor commit detected for %s/%s@%s — %s", rr.Owner, rr.Repo, rr.Ref, rr.Detail))
			// Diagnose-time impostor findings carry enrichment from
			// EnrichImpostorFindings (SaneSuggestionSearched, optional
			// SaneSuggestionTag). The apply-time alertWorkflow path doesn't
			// have a Finding handle, so we look up the matching Finding here
			// to preserve the renderer's "→ no recent release was
			// reachable — escalate" / "→ suggested: re-pin" line.
			rem.mergeEnrichmentForAlert(wr.Findings, rr.Owner+"/"+rr.Repo, rr.Ref)
			badKeys[depKey] = true
		case resolve.ReachabilityUnknown:
			rem.alertWorkflow(wr.Path, depKey,
				"couldn't verify the SHA is reachable (API or network issue) — retry before pinning",
				fmt.Sprintf("refusing to pin: cannot verify reachability for %s/%s@%s (try again later) — %s", rr.Owner, rr.Repo, rr.Ref, rr.Detail))
			badKeys[depKey] = true
		case resolve.Reachable:
			// Reachable, but only after falling back to a full branch
			// scan: the commit is not on a canonical branch. Pin it, but
			// flag it so the summary can surface it in red.
			if rr.FullScanUsed {
				rem.recordFullScanDep(depKey)
			}
		}
	}

	if len(badKeys) > 0 {
		deps, parentMap = dropDeps(deps, parentMap, badKeys)
		// Every resolved dep failed the gate — nothing benign is left to
		// pin, so skip the lockfile write. The alerts above already fired.
		if len(deps) == 0 {
			return errWorkflowAlerted
		}
	}

	directTracker := lockfile.NewDirectTracker(wr.ActionRefs, deps)

	// Narrow mutable version tags (v4, v4.2) to specific patch tags (v4.2.1)
	// so that ref-moved signals are meaningful — patch tags should never move.
	// Skip narrowing for same-owner internal repos — broad tags are fine
	// within your own org's private actions. Public repos always narrow.
	//
	// Narrowing happens BEFORE containing-discovery so that the canonical
	// ref recorded by ReverseLookup reflects the post-narrow tag, and
	// so any rewrite ReverseLookup adds (for SHA pins) doesn't collide
	// with the narrowing rewrite chain.
	rewrites := make(map[string]string)
	parentRewrites := make(map[string]string)
	for i := range deps {
		dep := &deps[i]
		if !IsMutableVersionTag(dep.Ref) {
			continue
		}
		owner, repo := dep.OwnerRepo()
		if owner == "" {
			continue
		}
		if rem.isSameOwner(owner) {
			info, err := rem.tagLister.GetRepoInfo(rem.ctx, owner, repo)
			if err == nil && info.IsInternal() {
				continue
			}
		}
		patchTag, err := rem.tagLister.BestPatchTagForSHA(rem.ctx, owner, repo, dep.SHA)
		if err != nil || patchTag == "" {
			continue
		}
		oldUses := dep.NWO + "@" + dep.Ref
		newUses := dep.NWO + "@" + patchTag
		rewrites[oldUses] = newUses
		parentRewrites[dep.Key()] = dep.NWO + "@" + patchTag
		rem.output.Detail("  %s → %s (pinning to patch version)", dep.Ref, patchTag)
		dep.Ref = patchTag
	}

	// Discover containing tag/branch for every resolved commit and rewrite
	// uses: lines to the canonical ref (tag-if-present-else-branch). Fails
	// closed if a SHA has no containing branch — that's the impostor signal.
	preNormKeys := make([]string, len(deps))
	for i, d := range deps {
		preNormKeys[i] = d.Key()
	}
	normRewrites, err := rem.resolver.ReverseLookup(rem.ctx, deps)
	if err != nil {
		var imp *resolve.ImpostorError
		if errors.As(err, &imp) {
			depKey := imp.NWO + "@" + imp.Ref
			rem.alertWorkflow(wr.Path, depKey, reasonForCategory(CategoryImpostorCommit), imp.Error())
			rem.mergeEnrichmentForAlert(wr.Findings, imp.NWO, imp.Ref)
			return errWorkflowAlerted
		}
		return fmt.Errorf("%s: normalizing containing refs: %w", wr.Path, err)
	}
	for k, v := range normRewrites {
		rewrites[k] = v
	}
	for i := range deps {
		if newKey := deps[i].Key(); newKey != preNormKeys[i] {
			parentRewrites[preNormKeys[i]] = newKey
		}
	}

	// Update parent map keys to reflect narrowed refs.
	parentMap = resolve.RekeyParentMap(parentMap, parentRewrites)

	// If we have rewrites, update the uses: lines in the workflow first.
	if len(rewrites) > 0 {
		content, _, err := wf.RewriteActionRefs(rewrites)
		if err != nil {
			return fmt.Errorf("rewriting refs to patch versions: %w", err)
		}
		if err := writeWorkflowFile(wr.Path, content); err != nil {
			return fmt.Errorf("writing file: %w", err)
		}
	}

	if err := rem.store.Set(rem.ctx, lockfile.WorkflowKeyFromPath(wr.Path), deps, parentMap, directTracker.Keys(deps)); err != nil {
		return fmt.Errorf("recording dependencies in lockfile: %w", err)
	}

	rem.output.Success("Pinned %d dependencies in %s", len(deps), wr.Path)
	rem.incFixed()
	return nil
}

// dropDeps removes every dependency whose NWO@Ref key is in drop from the
// closure and prunes the parent map (both as a child entry and from every
// parent's child list). It keys on Dependency.Key(), the same NWO@Ref form
// the parent map uses, so a reachability-failed dep can be contained without
// discarding the workflow's benign siblings. Neither input is mutated.
func dropDeps(deps []lockfile.Dependency, parentMap resolve.ParentMap, drop map[string]bool) ([]lockfile.Dependency, resolve.ParentMap) {
	kept := make([]lockfile.Dependency, 0, len(deps))
	for _, d := range deps {
		if drop[d.Key()] {
			continue
		}
		kept = append(kept, d)
	}
	if len(parentMap) == 0 {
		return kept, parentMap
	}
	pruned := make(resolve.ParentMap, len(parentMap))
	for child, parents := range parentMap {
		if drop[child] {
			continue
		}
		np := make([]string, 0, len(parents))
		for _, p := range parents {
			if drop[p] {
				continue
			}
			np = append(np, p)
		}
		pruned[child] = np
	}
	return kept, pruned
}

// applySHAToTag rewrites a uses: line from @SHA to @tag and hands the
// rewritten workflow to the pin pool. The file write and ref re-extract run
// here in Pass A so callers' session-state side effects (recordChoice,
// internalRefChoices, offerApplyAll) stay synchronous and visible to the
// next finding's shaConvertedForNWO check; the network-bound resolve +
// reachability + normalize + store.Set are deferred to the pinpool worker
// in Pass B via submitPin. That's what lets the per-worker "→ path" rows
// render on SHA-as-ref-heavy repos instead of one stuck spinner detail.
//
// Errors from the cheap rewrite (file load, write, ref extract) still abort
// Pass A immediately. Errors from the deferred pin surface in Pass B after
// the pool drains — same flow as handleNotPinned.
func (rem *Remediator) applySHAToTag(wr WorkflowReport, dep *lockfile.Dependency, owner, repo, tag string) error {
	wf, err := lockfile.Load(wr.Path)
	if err != nil {
		return err
	}

	// Build replacement: old "owner/repo@sha" → "owner/repo@tag"
	oldUses := dep.NWO + "@" + dep.Ref
	newUses := dep.NWO + "@" + tag

	content, changed, err := wf.RewriteActionRefs(map[string]string{oldUses: newUses})
	if err != nil {
		return fmt.Errorf("rewriting action refs: %w", err)
	}
	if changed == 0 {
		rem.output.Warning("could not find %s in workflow to rewrite", oldUses)
		rem.incSkipped()
		return nil
	}

	if err := writeWorkflowFile(wr.Path, content); err != nil {
		return fmt.Errorf("writing file: %w", err)
	}

	// Re-extract refs from the rewritten file so the deferred applyPin
	// resolves the *new* (tag) ref, not the SHA we just rewrote away from.
	// Hard-fail on a failed reload or an empty extract: we successfully
	// changed the file on disk, so silently falling back to stale refs
	// would let the worker record a lockfile that contradicts the
	// workflow. An empty result after a confirmed rewrite (changed > 0)
	// means the parser disagrees with itself between Rewrite and Extract
	// — bail instead of pinning nothing.
	wf2, err := lockfile.Load(wr.Path)
	if err != nil {
		return fmt.Errorf("reloading after rewrite: %w", err)
	}
	refs, _, _ := wf2.ExtractActionRefs()
	if len(refs) == 0 {
		return fmt.Errorf("%s: no action refs after rewrite to %s", wr.Path, newUses)
	}

	return rem.submitPin(WorkflowReport{Path: wr.Path, ActionRefs: refs})
}

// applyReResolve re-resolves a single stale dependency.
func (rem *Remediator) applyReResolve(wr WorkflowReport, dep *lockfile.Dependency) error {
	wf, err := lockfile.Load(wr.Path)
	if err != nil {
		return err
	}

	refs, _, _ := wf.ExtractActionRefs()
	rem.startWork(fmt.Sprintf("Re-resolving %s", dep.NWO))
	defer rem.stopWork()
	deps, parentMap, err := rem.resolver.ResolveAllRecursive(rem.ctx, refs)
	if err != nil {
		return fmt.Errorf("resolving actions: %w", err)
	}
	directTracker := lockfile.NewDirectTracker(refs, deps)

	parentMap, err = rem.normalizeAndRewrite(wr.Path, deps, parentMap, wr.Findings)
	if err != nil {
		return err
	}

	if err := rem.store.Set(rem.ctx, lockfile.WorkflowKeyFromPath(wr.Path), deps, parentMap, directTracker.Keys(deps)); err != nil {
		return fmt.Errorf("recording dependencies in lockfile: %w", err)
	}

	rem.output.Success("Updated %s to latest resolution", dep.Key())
	rem.incFixed()
	return nil
}

// normalizeAndRewrite runs containing-ref discovery against deps, mutates
// dep.Tag/Branch/Ref in place, rewrites the workflow file's uses: lines to
// the canonical refs, and rekeys parentMap to reflect any ref changes. Both
// `deps` and `parentMap` may be mutated; callers should treat them as live
// after the call and pass the (rekeyed) parentMap on to store.Set.
// Returns an error if any commit has no containing branch (impostor signal).
//
// findings is the diagnose-time finding list for the workflow (typically
// wr.Findings); it's used to preserve enrichment on any impostor alert
// emitted from this path. Pass nil if no findings are available — the
// enrichment merge becomes a no-op.
func (rem *Remediator) normalizeAndRewrite(workflowPath string, deps []lockfile.Dependency, parentMap resolve.ParentMap, findings []Finding) (resolve.ParentMap, error) {
	preNormKeys := make([]string, len(deps))
	for i, d := range deps {
		preNormKeys[i] = d.Key()
	}
	normRewrites, err := rem.resolver.ReverseLookup(rem.ctx, deps)
	if err != nil {
		var imp *resolve.ImpostorError
		if errors.As(err, &imp) {
			depKey := imp.NWO + "@" + imp.Ref
			rem.alertWorkflow(workflowPath, depKey, reasonForCategory(CategoryImpostorCommit), imp.Error())
			rem.mergeEnrichmentForAlert(findings, imp.NWO, imp.Ref)
			return parentMap, errWorkflowAlerted
		}
		return parentMap, fmt.Errorf("%s: normalizing containing refs: %w", workflowPath, err)
	}
	parentRewrites := make(map[string]string, len(deps))
	for i := range deps {
		if newKey := deps[i].Key(); newKey != preNormKeys[i] {
			parentRewrites[preNormKeys[i]] = newKey
		}
	}
	parentMap = resolve.RekeyParentMap(parentMap, parentRewrites)
	if len(normRewrites) == 0 {
		return parentMap, nil
	}
	wf, err := lockfile.Load(workflowPath)
	if err != nil {
		return parentMap, err
	}
	content, _, err := wf.RewriteActionRefs(normRewrites)
	if err != nil {
		return parentMap, fmt.Errorf("rewriting refs to canonical tag/branch: %w", err)
	}
	if err := writeWorkflowFile(workflowPath, content); err != nil {
		return parentMap, fmt.Errorf("writing file: %w", err)
	}
	return parentMap, nil
}

// applyImpostorRewrites loads a workflow, applies the rewrites map, writes
// it back, and reloads the action refs so the caller can re-pin against
// the new content. Used by the pre-pin impostor auto-fix path
// (tryAutoFixImpostors, driven by check-phase findings with
// SaneSuggestionTag).
//
// Returns the refreshed ActionRefs together with applied=true when the
// rewrite actually changed the file. An empty rewrites map or a
// zero-change rewrite is a no-op and returns (nil, false, nil); on any I/O
// error the file is left in whatever state the failure produced and the
// error is returned.
func (rem *Remediator) applyImpostorRewrites(workflowPath string, rewrites map[string]string) ([]lockfile.ActionRef, bool, error) {
	if len(rewrites) == 0 {
		return nil, false, nil
	}
	wf, err := lockfile.Load(workflowPath)
	if err != nil {
		return nil, false, err
	}
	content, changed, err := wf.RewriteActionRefs(rewrites)
	if err != nil {
		return nil, false, err
	}
	if changed == 0 {
		return nil, false, nil
	}
	if err := writeWorkflowFile(workflowPath, content); err != nil {
		return nil, false, err
	}
	wf2, err := lockfile.Load(workflowPath)
	if err != nil {
		return nil, false, err
	}
	refs, _, _ := wf2.ExtractActionRefs()
	return refs, true, nil
}

// writeWorkflowFile overwrites an existing workflow file, preserving its
// current permission bits. Pinning must not silently widen a restrictive
// mode (e.g. 0600 → 0644). Falls back to 0o644 when the file's mode can't
// be determined (e.g. it doesn't yet exist).
func writeWorkflowFile(path string, content []byte) error {
	mode := os.FileMode(0o644)
	if info, err := os.Stat(path); err == nil {
		mode = info.Mode().Perm()
	}
	return os.WriteFile(path, content, mode)
}
