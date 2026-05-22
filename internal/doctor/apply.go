package doctor

import (
	"fmt"
	"os"

	"github.com/github/gh-actions-pin/internal/lockfile"
	"github.com/github/gh-actions-pin/internal/resolver"
)

// isSHARef returns true if ref looks like a full commit SHA (40 or 64 hex chars).
func isSHARef(ref string) bool {
	return lockfile.IsFullSHA(ref)
}

// LooksLikeVersion returns true if ref starts with "v" followed by a digit.
func LooksLikeVersion(ref string) bool {
	if len(ref) < 2 {
		return false
	}
	return ref[0] == 'v' && ref[1] >= '0' && ref[1] <= '9'
}

// applyPin runs the full pin flow on an unpinned workflow.
func (rem *Remediator) applyPin(wr WorkflowReport) error {
	wf, err := lockfile.Load(wr.Path)
	if err != nil {
		return err
	}

	deps, err := rem.resolver.ResolveAllRecursive(wr.ActionRefs)
	if err != nil {
		return fmt.Errorf("resolving actions: %w", err)
	}

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

	// Update parent map keys to reflect narrowed refs.
	rem.resolver.RekeyParentMap(parentRewrites)

	// If we have rewrites, update the uses: lines in the workflow first.
	if len(rewrites) > 0 {
		content, _, err := wf.RewriteActionRefs(rewrites)
		if err != nil {
			return fmt.Errorf("rewriting refs to patch versions: %w", err)
		}
		if err := os.WriteFile(wr.Path, content, 0o644); err != nil {
			return fmt.Errorf("writing file: %w", err)
		}
	}

	if err := rem.store.Set(lockfile.WorkflowKeyFromPath(wr.Path), deps); err != nil {
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
	if err := os.WriteFile(wr.Path, content, 0o644); err != nil {
		return fmt.Errorf("writing file: %w", err)
	}

	// Re-load, re-extract, re-resolve, write to store.
	wf2, err := lockfile.Load(wr.Path)
	if err != nil {
		return err
	}
	refs, _, _ := wf2.ExtractActionRefs()
	deps, err := rem.resolver.ResolveAllRecursive(refs)
	if err != nil {
		return fmt.Errorf("re-resolving after ref change: %w", err)
	}
	if err := rem.store.Set(lockfile.WorkflowKeyFromPath(wr.Path), deps); err != nil {
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
	deps, err := rem.resolver.ResolveAllRecursive(refs)
	if err != nil {
		return fmt.Errorf("resolving actions: %w", err)
	}

	if err := rem.store.Set(lockfile.WorkflowKeyFromPath(wr.Path), deps); err != nil {
		return fmt.Errorf("recording dependencies in lockfile: %w", err)
	}

	rem.output.Success("Updated %s to latest resolution", dep.Key())
	rem.Fixed++
	return nil
}
