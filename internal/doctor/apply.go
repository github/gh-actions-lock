package doctor

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/github/gh-actions-pin/internal/lockfile"
	"github.com/github/gh-actions-pin/internal/resolver"
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

	deps, parentMap, err := rem.resolver.ResolveAllRecursive(wr.ActionRefs)
	if err != nil {
		return fmt.Errorf("resolving actions: %w", err)
	}
	directTracker := lockfile.NewDirectTracker(wr.ActionRefs, deps)

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
	reachResults := rem.resolver.CheckReachabilityAll(deps)
	for _, rr := range reachResults {
		switch rr.Status {
		case resolver.Unreachable:
			rem.alertImpostor(wr.Path, rr.Owner, rr.Repo, rr.Ref,
				fmt.Sprintf("refusing to pin: impostor commit detected for %s/%s@%s — %s", rr.Owner, rr.Repo, rr.Ref, rr.Detail))
			return errWorkflowAlerted
		case resolver.ReachabilityUnknown:
			rem.alertWorkflow(wr.Path, rr.Owner+"/"+rr.Repo+"@"+rr.Ref,
				"couldn't verify the SHA is reachable (API or network issue) — retry before pinning",
				fmt.Sprintf("refusing to pin: cannot verify reachability for %s/%s@%s (try again later) — %s", rr.Owner, rr.Repo, rr.Ref, rr.Detail))
			return errWorkflowAlerted
		case resolver.Reachable:
			// Reachable, but only after falling back to a full branch
			// scan: the commit is not on a canonical branch. Pin it, but
			// flag it so the summary can surface it in red.
			if rr.FullScanUsed {
				rem.recordFullScanDep(rr.Owner + "/" + rr.Repo + "@" + rr.Ref)
			}
		}
	}

	// Narrow mutable version tags (v4, v4.2) to specific patch tags (v4.2.1)
	// so that ref-moved signals are meaningful — patch tags should never move.
	// Skip narrowing for same-owner internal repos — broad tags are fine
	// within your own org's private actions. Public repos always narrow.
	//
	// Narrowing happens BEFORE containing-discovery so that the canonical
	// ref recorded by NormalizeContaining reflects the post-narrow tag, and
	// so any rewrite NormalizeContaining adds (for SHA pins) doesn't collide
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
			info, err := rem.tagLister.GetRepoInfo(owner, repo)
			if err == nil && info.IsInternal() {
				continue
			}
		}
		patchTag, err := rem.tagLister.BestPatchTagForSHA(owner, repo, dep.SHA)
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
	normRewrites, err := rem.resolver.NormalizeContaining(deps)
	if err != nil {
		var imp *resolver.ImpostorError
		if errors.As(err, &imp) {
			owner, repo, _ := lockfile.SplitNWO(imp.NWO)
			rem.alertImpostor(wr.Path, owner, repo, imp.Ref, imp.Error())
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
	parentMap = resolver.RekeyParentMap(parentMap, parentRewrites)

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

	if err := rem.store.Set(lockfile.WorkflowKeyFromPath(wr.Path), deps, parentMap, directTracker.Keys(deps)); err != nil {
		return fmt.Errorf("recording dependencies in lockfile: %w", err)
	}

	rem.output.Success("Pinned %d dependencies in %s", len(deps), wr.Path)
	rem.incFixed()
	return nil
}

// applySHAToTag rewrites a uses: line from @SHA to @tag and re-resolves.
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

	// Write the rewritten content, then re-parse and re-resolve to get correct lockfile.
	if err := writeWorkflowFile(wr.Path, content); err != nil {
		return fmt.Errorf("writing file: %w", err)
	}

	// Re-load, re-extract, re-resolve, write to store.
	wf2, err := lockfile.Load(wr.Path)
	if err != nil {
		return err
	}
	rem.startWork(fmt.Sprintf("Re-pinning %s", dep.NWO))
	defer rem.stopWork()
	refs, _, _ := wf2.ExtractActionRefs()
	deps, parentMap, err := rem.resolver.ResolveAllRecursive(refs)
	if err != nil {
		return fmt.Errorf("re-resolving after ref change: %w", err)
	}
	directTracker := lockfile.NewDirectTracker(refs, deps)
	parentMap, err = rem.normalizeAndRewrite(wr.Path, deps, parentMap)
	if err != nil {
		return err
	}
	if err := rem.store.Set(lockfile.WorkflowKeyFromPath(wr.Path), deps, parentMap, directTracker.Keys(deps)); err != nil {
		return fmt.Errorf("recording dependencies in lockfile: %w", err)
	}

	rem.output.Success("Converted %s from SHA to %s and re-pinned", dep.NWO, tag)
	rem.incFixed()
	return nil
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
	deps, parentMap, err := rem.resolver.ResolveAllRecursive(refs)
	if err != nil {
		return fmt.Errorf("resolving actions: %w", err)
	}
	directTracker := lockfile.NewDirectTracker(refs, deps)

	parentMap, err = rem.normalizeAndRewrite(wr.Path, deps, parentMap)
	if err != nil {
		return err
	}

	if err := rem.store.Set(lockfile.WorkflowKeyFromPath(wr.Path), deps, parentMap, directTracker.Keys(deps)); err != nil {
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
func (rem *Remediator) normalizeAndRewrite(workflowPath string, deps []lockfile.Dependency, parentMap resolver.ParentMap) (resolver.ParentMap, error) {
	preNormKeys := make([]string, len(deps))
	for i, d := range deps {
		preNormKeys[i] = d.Key()
	}
	normRewrites, err := rem.resolver.NormalizeContaining(deps)
	if err != nil {
		var imp *resolver.ImpostorError
		if errors.As(err, &imp) {
			owner, repo, _ := lockfile.SplitNWO(imp.NWO)
			rem.alertImpostor(workflowPath, owner, repo, imp.Ref, imp.Error())
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
	parentMap = resolver.RekeyParentMap(parentMap, parentRewrites)
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

// AutoFixAlertedImpostors walks the alerted-impostor state populated during
// Remediate and rewrites uses: lines for any dep that already has a
// sane-release suggestion attached. Each matched workflow is rewritten to
// the suggested tag and re-pinned via applyPin. Successful fixes are moved
// from the alerted summary into AutoFixedImpostors so the end-of-run output
// can announce them as "auto-pinned — review for sanity" instead of
// continuing to flag them as unfixable. Workflows whose re-pin fails (e.g.
// the suggested tag is also unreachable) are left in the alerted set with
// the rewritten file on disk so the user can inspect manually.
func (rem *Remediator) AutoFixAlertedImpostors() {
	if len(rem.AlertedSuggestions) == 0 {
		return
	}

	type pending struct {
		owner, repo, oldRef string
		newTag, newSHA      string
		depKey              string
	}

	rem.mu.Lock()
	byWorkflow := map[string][]pending{}
	for depKey, sug := range rem.AlertedSuggestions {
		ar := lockfile.ParseActionRef(depKey)
		if ar == nil {
			continue
		}
		owner := ar.Owner
		repo := ar.Repo
		oldRef := ar.Ref
		newTag, newSHA := sug, ""
		if sp := strings.IndexByte(sug, ' '); sp >= 0 {
			newTag = sug[:sp]
			newSHA = sug[sp+1:]
		}
		if newTag == "" {
			continue
		}
		for _, wp := range rem.AlertedWorkflows[depKey] {
			byWorkflow[wp] = append(byWorkflow[wp], pending{owner, repo, oldRef, newTag, newSHA, depKey})
		}
	}
	rem.mu.Unlock()

	if len(byWorkflow) == 0 {
		return
	}

	fixedKeys := map[string]bool{}
	for wp, fixes := range byWorkflow {
		wf, err := lockfile.Load(wp)
		if err != nil {
			continue
		}
		refs, _, _ := wf.ExtractActionRefs()
		rewrites := map[string]string{}
		for _, fx := range fixes {
			for _, ar := range refs {
				if ar.Owner == fx.owner && ar.Repo == fx.repo {
					rewrites[ar.Raw] = fx.owner + "/" + fx.repo + "@" + fx.newTag
				}
			}
		}
		if len(rewrites) == 0 {
			continue
		}
		content, changed, err := wf.RewriteActionRefs(rewrites)
		if err != nil || changed == 0 {
			continue
		}
		if err := writeWorkflowFile(wp, content); err != nil {
			continue
		}
		wf2, err := lockfile.Load(wp)
		if err != nil {
			continue
		}
		newRefs, _, _ := wf2.ExtractActionRefs()
		if err := rem.applyPin(WorkflowReport{Path: wp, ActionRefs: newRefs}); err != nil {
			// Re-pin failed (e.g. suggested tag is also unreachable).
			// Leave the rewritten file in place; the alert remains so the
			// user notices.
			continue
		}
		for _, fx := range fixes {
			rem.recordAutoFixedImpostor(wp, fx.owner+"/"+fx.repo, fx.oldRef, fx.newTag, fx.newSHA)
			fixedKeys[fx.depKey] = true
		}
	}

	if len(fixedKeys) == 0 {
		return
	}

	rem.mu.Lock()
	defer rem.mu.Unlock()
	for k := range fixedKeys {
		delete(rem.AlertedSuggestions, k)
		delete(rem.AlertedReasons, k)
		delete(rem.AlertedWorkflows, k)
		delete(rem.AlertedSearched, k)
	}
	if len(rem.AlertedDeps) > 0 {
		kept := rem.AlertedDeps[:0]
		for _, d := range rem.AlertedDeps {
			if !fixedKeys[d] {
				kept = append(kept, d)
			}
		}
		rem.AlertedDeps = kept
	}
	delta := len(fixedKeys)
	if delta > rem.Alerted {
		delta = rem.Alerted
	}
	rem.Alerted -= delta
	rem.Fixed += delta
}
