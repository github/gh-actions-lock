package doctor

import (
	"fmt"
	"os"

	"github.com/github/gh-actions-pin/internal/lockfile"
	"github.com/github/gh-actions-pin/internal/resolver"
)

// startWork shows a spinner (TTY only) for the network-heavy portion of a
// remediation and wires the resolver's progress callback to its detail line.
// Pair every call with stopWork (defer). On non-TTY outputs it is a no-op so
// CI logs stay clean. Prompts never overlap a spinner: apply* runs after any
// interactive prompt in its handler.
func (rem *Remediator) startWork(label string) {
	if !rem.output.IsTTY() {
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

	rem.startWork(fmt.Sprintf("Pinning %s", wr.Path))
	defer rem.stopWork()

	deps, err := rem.resolver.ResolveAllRecursive(wr.ActionRefs)
	if err != nil {
		return fmt.Errorf("resolving actions: %w", err)
	}
	directTracker := lockfile.NewDirectTracker(wr.ActionRefs, deps)

	// Check for impostor commits at pin time — don't let fork-network
	// commits get pinned in the first place. Fail closed: if we can't
	// verify reachability (e.g. branch_commits returns 429), refuse to
	// pin rather than silently accepting a potentially poisoned commit.
	//
	// When reachability is disabled via config, skip this gate entirely —
	// the branch_commits endpoint would return "disabled" for every dep,
	// which would block all pinning.
	if !rem.resolver.DisableReachability {
		reachResults := rem.resolver.CheckReachabilityAll(deps)
		for _, rr := range reachResults {
			switch rr.Status {
			case resolver.Unreachable:
				rem.output.Error("%s/%s@%s: %s", rr.Owner, rr.Repo, rr.Ref, rr.Detail)
				rem.Alerted++
				return fmt.Errorf("refusing to pin: impostor commit detected for %s/%s@%s", rr.Owner, rr.Repo, rr.Ref)
			case resolver.ReachabilityUnknown:
				rem.output.Error("%s/%s@%s: could not verify commit reachability — %s", rr.Owner, rr.Repo, rr.Ref, rr.Detail)
				rem.Alerted++
				return fmt.Errorf("refusing to pin: cannot verify reachability for %s/%s@%s (try again later)", rr.Owner, rr.Repo, rr.Ref)
			}
		}
	}

	// Narrow mutable version tags (v4, v4.2) to specific patch tags (v4.2.1)
	// so that REF_MOVED signals are meaningful — patch tags should never move.
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
		rem.Alerted++
		return fmt.Errorf("normalizing containing refs: %w", err)
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
	rem.resolver.RekeyParentMap(parentRewrites)

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

	if err := rem.store.Set(lockfile.WorkflowKeyFromPath(wr.Path), deps, rem.resolver.ParentMap(), directTracker.Keys(deps)); err != nil {
		return fmt.Errorf("recording dependencies in lockfile: %w", err)
	}

	rem.output.Success("Pinned %d dependencies in %s", len(deps), wr.Path)
	rem.Fixed++
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
		rem.Skipped++
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
	deps, err := rem.resolver.ResolveAllRecursive(refs)
	if err != nil {
		return fmt.Errorf("re-resolving after ref change: %w", err)
	}
	directTracker := lockfile.NewDirectTracker(refs, deps)
	if err := rem.normalizeAndRewrite(wr.Path, deps); err != nil {
		return err
	}
	if err := rem.store.Set(lockfile.WorkflowKeyFromPath(wr.Path), deps, rem.resolver.ParentMap(), directTracker.Keys(deps)); err != nil {
		return fmt.Errorf("recording dependencies in lockfile: %w", err)
	}

	rem.output.Success("Converted %s from SHA to %s and re-pinned", dep.NWO, tag)
	rem.Fixed++
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
	deps, err := rem.resolver.ResolveAllRecursive(refs)
	if err != nil {
		return fmt.Errorf("resolving actions: %w", err)
	}
	directTracker := lockfile.NewDirectTracker(refs, deps)

	if err := rem.normalizeAndRewrite(wr.Path, deps); err != nil {
		return err
	}

	if err := rem.store.Set(lockfile.WorkflowKeyFromPath(wr.Path), deps, rem.resolver.ParentMap(), directTracker.Keys(deps)); err != nil {
		return fmt.Errorf("recording dependencies in lockfile: %w", err)
	}

	rem.output.Success("Updated %s to latest resolution", dep.Key())
	rem.Fixed++
	return nil
}

// normalizeAndRewrite runs containing-ref discovery against deps, mutates
// dep.Tag/Branch/Ref in place, and rewrites the workflow file's uses: lines
// to the canonical refs. Returns an error if any commit has no containing
// branch (impostor signal).
func (rem *Remediator) normalizeAndRewrite(workflowPath string, deps []lockfile.Dependency) error {
	normRewrites, err := rem.resolver.NormalizeContaining(deps)
	if err != nil {
		rem.Alerted++
		return fmt.Errorf("normalizing containing refs: %w", err)
	}
	if len(normRewrites) == 0 {
		return nil
	}
	wf, err := lockfile.Load(workflowPath)
	if err != nil {
		return err
	}
	content, _, err := wf.RewriteActionRefs(normRewrites)
	if err != nil {
		return fmt.Errorf("rewriting refs to canonical tag/branch: %w", err)
	}
	if err := writeWorkflowFile(workflowPath, content); err != nil {
		return fmt.Errorf("writing file: %w", err)
	}
	return nil
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
