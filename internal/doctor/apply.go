package doctor

import (
	"fmt"
	"os"

	"github.com/github/gh-actions-pin/internal/lockfile"
)

// choiceKey returns a stable key for session memory: "owner/repo@SHA".
func (rem *Remediator) choiceKey(dep *lockfile.Dependency) string {
	return dep.NWO + "@" + dep.SHA
}

// recordChoice saves a tag choice for a dep so it can be auto-applied later.
func (rem *Remediator) recordChoice(dep *lockfile.Dependency, tag string) {
	rem.choices[rem.choiceKey(dep)] = tag
}

// recallChoice returns (tag, true) if we already made a choice for this dep.
func (rem *Remediator) recallChoice(dep *lockfile.Dependency) (string, bool) {
	tag, ok := rem.choices[rem.choiceKey(dep)]
	return tag, ok
}

// refKey returns a session memory key for an unpinned action ref: "owner/repo@ref".
func refKey(ref lockfile.ActionRef) string {
	return ref.FullName() + "@" + ref.Ref
}

// markRefsApproved records all action refs as approved for auto-pinning.
func (rem *Remediator) markRefsApproved(refs []lockfile.ActionRef) {
	for _, ref := range refs {
		rem.approvedRefs[refKey(ref)] = true
	}
}

// allRefsApproved returns true if every ref was already approved in a prior workflow.
func (rem *Remediator) allRefsApproved(refs []lockfile.ActionRef) bool {
	if len(refs) == 0 {
		return false
	}
	for _, ref := range refs {
		if !rem.approvedRefs[refKey(ref)] {
			return false
		}
	}
	return true
}

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

	// Narrow mutable version tags (v4, v4.2) to specific patch tags (v4.2.1)
	// so that REF_MOVED signals are meaningful — patch tags should never move.
	// Skip narrowing for same-owner internal repos — broad tags are fine
	// within your own org's private actions. Public repos always narrow.
	rewrites := make(map[string]string)
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
		rem.output.Detail("  %s → %s (pinning to patch version)", dep.Ref, patchTag)
		dep.Ref = patchTag
	}

	// If we have rewrites, update the uses: lines in the workflow first.
	if len(rewrites) > 0 {
		content, _, err := wf.RewriteActionRefs(rewrites)
		if err != nil {
			return fmt.Errorf("rewriting refs to patch versions: %w", err)
		}
		if err := os.WriteFile(wr.Path, content, 0o644); err != nil {
			return fmt.Errorf("writing file: %w", err)
		}
		// Re-load after rewrite so WriteDependencies sees the updated content.
		wf, err = lockfile.Load(wr.Path)
		if err != nil {
			return err
		}
	}

	written, err := wf.WriteDependencies(deps)
	if err != nil {
		return fmt.Errorf("writing dependencies: %w", err)
	}
	if err := os.WriteFile(wr.Path, written, 0o644); err != nil {
		return fmt.Errorf("writing file: %w", err)
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

	// Re-load, re-extract, re-resolve, re-write lockfile.
	wf2, err := lockfile.Load(wr.Path)
	if err != nil {
		return err
	}
	refs, _, _ := wf2.ExtractActionRefs()
	deps, err := rem.resolver.ResolveAllRecursive(refs)
	if err != nil {
		return fmt.Errorf("re-resolving after ref change: %w", err)
	}
	written, err := wf2.WriteDependencies(deps)
	if err != nil {
		return fmt.Errorf("writing dependencies: %w", err)
	}
	if err := os.WriteFile(wr.Path, written, 0o644); err != nil {
		return fmt.Errorf("writing file: %w", err)
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

	written, err := wf.WriteDependencies(deps)
	if err != nil {
		return fmt.Errorf("writing dependencies: %w", err)
	}
	if err := os.WriteFile(wr.Path, written, 0o644); err != nil {
		return fmt.Errorf("writing file: %w", err)
	}

	rem.output.Success("Updated %s to latest resolution", dep.Key())
	rem.Fixed++
	return nil
}
