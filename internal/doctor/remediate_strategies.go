package doctor

import (
	"fmt"
	"net/url"
	"os"
	"os/exec"

	"github.com/github/gh-actions-pin/internal/cachekey"
	"github.com/github/gh-actions-pin/internal/lockfile"
	"github.com/github/gh-actions-pin/internal/ui"
)

func (rem *Remediator) remediateWorkflow(wr WorkflowReport) error {
	// Auto-fix impostor findings that have a sane-release suggestion before
	// any per-finding remediation: rewrite the workflow's uses: lines, then
	// re-pin from the rewritten file. This converts what would otherwise be
	// a "needs investigation" alert into a successful pin against the next
	// reachable release. The substitution may cross a major version (e.g.
	// v1.25.0 → v3.0.3), so the end-of-run summary surfaces it as
	// "auto-pinned — review for sanity". When *any* impostor finding for the
	// workflow lacks a suggestion (or is transitive) we don't auto-fix any
	// of them: the consumer needs the alert anyway, and a half-rewritten
	// workflow would obscure the unfixable cases.
	if rem.tryAutoFixImpostors(&wr) {
		// All impostor findings handled — defer to Pass B so the
		// rewrite+pin happens inside a pinpool worker slot alongside
		// SHA-as-ref and NotPinned, instead of blocking Pass A on a
		// synchronous applyPin (which is what froze the spinner on
		// next.js-class repos).
		return rem.submitPin(wr)
	}

	headerPrinted := false
	ensureHeader := func() {
		if !headerPrinted {
			rem.output.Header("%s", wr.Path)
			headerPrinted = true
		}
	}

	// In interactive mode, always show the header.
	if rem.prompter.IsInteractive() {
		ensureHeader()
	}

	first := true
	for _, finding := range wr.Findings {
		if finding.Category == CategoryValid || finding.Category == CategoryRunOnly || finding.Category == CategoryRefMoved || finding.Category.IsInconclusive() {
			continue
		}

		// Suppress stale alerts on deps that were already auto-converted from
		// a SHA pin to a canonical tag earlier in this run. This is the
		// common case for actions like actions/github-script where a single
		// uses: line trips both sha-as-ref (which auto-pins to a tag) and
		// misleading-sha (which alerts because the ref was an annotated tag
		// SHA, not a commit). Once the ref is rewritten the alert no longer
		// describes the file on disk.
		if finding.Dependency != nil && rem.shaConvertedForNWO(finding.Dependency.NWO) {
			switch finding.Category {
			case CategoryMisleadingSHA, CategoryImpostorCommit, CategoryLockfileForgery:
				continue
			}
		}

		// For non-interactive sha-as-ref, check if this dep was already printed.
		// If so, skip silently (no header, no blank line).
		if !rem.prompter.IsInteractive() && finding.Category == CategorySHAAsRef {
			if finding.Dependency != nil {
				if _, seen := rem.state.choices[finding.Dependency.Key()]; seen {
					rem.Skipped++
					continue
				}
			}
		}

		// Alerted-only categories (Impostor/Forgery/Misleading) were already
		// fully presented by presentCheckResults in non-interactive mode.
		// Just register the alert here without re-printing the per-workflow
		// header or finding details.
		if !rem.prompter.IsInteractive() {
			switch finding.Category {
			case CategoryImpostorCommit, CategoryLockfileForgery, CategoryMisleadingSHA:
				rem.Alerted++
				rem.addAlertedDep(finding)
				continue
			}
		}

		ensureHeader()
		if !first {
			rem.output.Blank()
		}
		first = false

		switch finding.Category {
		case CategoryNotPinned:
			// Re-read workflow from disk — earlier SHA→tag conversions may have
			// changed refs since diagnosis time.
			if wf, err := lockfile.Load(wr.Path); err == nil {
				if freshRefs, _, _ := wf.ExtractActionRefs(); len(freshRefs) > 0 {
					wr.ActionRefs = freshRefs
				}
			}
			if err := rem.handleNotPinned(wr); err != nil {
				return err
			}
			return nil // NotPinned is workflow-level, one pass is enough.

		case CategorySHAAsRef:
			if err := rem.handleSHAAsRef(wr, finding); err != nil {
				return err
			}

		case CategoryStale:
			if err := rem.handleStale(wr, finding); err != nil {
				return err
			}

		case CategoryRefChanged:
			if err := rem.handleRefChanged(wr, finding); err != nil {
				return err
			}

		case CategoryImpostorCommit:
			rem.output.Error("%s", finding.Detail)
			rem.output.Hint("This may indicate a fork-network injection attack. Do not auto-fix.")
			rem.Alerted++
			rem.addAlertedDep(finding)

		case CategoryLockfileForgery:
			rem.output.Error("lockfile-forgery %s: %s", rem.depKey(finding), finding.Detail)
			rem.output.Hint("The pinned SHA was never in this ref's lineage — possible lockfile tampering.")
			rem.Alerted++
			rem.addAlertedDep(finding)

		case CategoryMisleadingSHA:
			rem.output.Error("misleading-sha %s: %s", rem.depKey(finding), finding.Detail)
			rem.output.Hint("This ref may be a deceptive branch or tag name masquerading as a commit hash.")
			rem.Alerted++
			rem.addAlertedDep(finding)
		}
	}

	if headerPrinted {
		rem.output.Blank()
	}
	return nil
}

func (rem *Remediator) handleNotPinned(wr WorkflowReport) error {
	rem.output.Warning("%d %s found but not pinned", len(wr.ActionRefs), ui.Pluralize(len(wr.ActionRefs), "action", "actions"))
	if docURL := DocURLFor(CategoryNotPinned); docURL != "" {
		rem.output.Detail("  see: %s", rem.output.DocLink(docURL))
	}

	if !rem.prompter.IsInteractive() {
		// Non-interactive: auto-pin all refs (ref→SHA is deterministic).
		rem.state.markRefsApproved(wr.ActionRefs)
		return rem.submitPin(wr)
	}

	// For internal repos, offer the default branch as an alternative ref.
	wr = rem.offerDefaultBranch(wr)

	// If all refs in this workflow were already approved in a prior workflow, auto-apply.
	if rem.state.allRefsApproved(wr.ActionRefs) {
		rem.output.Detail("  ↳ all actions already approved — auto-pinning")
		return rem.submitPin(wr)
	}

	// Resolve all refs to show the SHAs they'll pin to.
	rem.startWork(rem.workLabel(fmt.Sprintf("Resolving %s", wr.Path)))
	resolved, _, _ := rem.resolver.ResolveAllRecursive(rem.ctx, wr.ActionRefs)
	rem.stopWork()
	shaByKey := make(map[string]string)
	for _, dep := range resolved {
		shaByKey[dep.Key()] = dep.SHA
	}

	// Review each action one at a time. Auto-apply prior choices and internal
	// actions silently; prompt for each external action.
	var approved []lockfile.ActionRef
	for _, ref := range wr.ActionRefs {
		key := ref.FullName() + "@" + ref.Ref // display key (preserves sub-action path)
		depKey := ref.NWO() + "@" + ref.Ref   // dep.Key() form (NWO@Ref, runner-flat)

		// Prior choice — auto-apply without prompting.
		if rem.state.approvedRefs[refKey(ref)] {
			sha, ok := shaByKey[depKey]
			if !ok || sha == "" {
				rem.output.Detail("  %s  (could not resolve)", key)
				rem.markUnresolved(key)
				continue
			}
			rem.output.Detail("  %s → %s  %s", key, sha[:12], rem.output.Dim("↩ prior choice"))
			approved = append(approved, ref)
			continue
		}

		// Internal (same-owner) action — auto-apply without prompting.
		if rem.isSameOwner(ref.Owner) {
			sha, ok := shaByKey[depKey]
			if !ok || sha == "" {
				rem.output.Detail("  %s  (could not resolve)", key)
				rem.markUnresolved(key)
				continue
			}
			label := ""
			if info, err := rem.tagLister.GetRepoInfo(rem.ctx, ref.Owner, ref.Repo); err == nil {
				label = info.VisibilityLabel()
				if ref.Ref == info.DefaultBranch {
					label += " · default branch"
				}
				if age := FormatTagAge(info.PushedAt); age != "" {
					label += " · last push " + age
				}
			}
			rem.output.Detail("  %s → %s  %s", key, sha[:12], rem.output.Dim(label))
			approved = append(approved, ref)
			continue
		}

		// External action — auto-pin when there's a clear default, prompt otherwise.
		sha, ok := shaByKey[depKey]
		if !ok {
			rem.output.Detail("  %s  (could not resolve)", key)
			rem.markUnresolved(key)
			continue
		}

		// Already a full SHA — immutable and pinned by construction. Record it
		// without prompting; surface the matching release tag if we can find one.
		if lockfile.IsFullSha(ref.Ref) {
			commitURL := fmt.Sprintf("https://github.com/%s/%s/commit/%s", ref.Owner, ref.Repo, sha)
			shaLabel := rem.output.Hyperlink(sha[:12], commitURL)
			if tag, err := rem.tagLister.BestPatchTagForSHA(rem.ctx, ref.Owner, ref.Repo, sha); err == nil && tag != "" {
				tagURL := fmt.Sprintf("https://github.com/%s/%s/releases/tag/%s", ref.Owner, ref.Repo, tag)
				rem.output.Detail("  %s → %s → %s  %s", key, tag, shaLabel, rem.output.Dim(rem.output.Hyperlink("release", tagURL)))
			} else {
				rem.output.Detail("  %s → %s  %s", key, shaLabel, rem.output.Dim("already pinned"))
			}
			rem.state.approvedRefs[refKey(ref)] = true
			approved = append(approved, ref)
			continue
		}

		displayTag := ref.Ref
		autoPin := false

		// Case 1: Already a full semver tag (v4.3.1) — good default, verify it's a real tag.
		if sv, svOK := lockfile.ParseVersion(ref.Ref); svOK && sv.IsFull() {
			if rem.tagLister.LookupTag(rem.ctx, ref.Owner, ref.Repo, ref.Ref) != nil {
				autoPin = true
			}
		}

		// Case 2: Mutable tag (v4, v4.2) — auto-pin if there's exactly one matching patch tag.
		if !autoPin && IsMutableVersionTag(ref.Ref) {
			if uniqueTag, err := rem.tagLister.UniquePatchTagForRef(rem.ctx, ref.Owner, ref.Repo, sha, ref.Ref); err == nil && uniqueTag != "" {
				displayTag = uniqueTag
				autoPin = true
			}
		}

		if autoPin {
			tagURL := fmt.Sprintf("https://github.com/%s/%s/releases/tag/%s", ref.Owner, ref.Repo, displayTag)
			tagLink := rem.output.Dim(rem.output.Hyperlink("release", tagURL))
			if ti := rem.tagLister.LookupTag(rem.ctx, ref.Owner, ref.Repo, displayTag); ti != nil && ti.IsImmutable {
				tagLink = rem.output.Dim("🔒 " + rem.output.Hyperlink("immutable release", tagURL))
			}
			// Show verifiable SHA match: tag resolves to the same commit.
			commitURL := fmt.Sprintf("https://github.com/%s/%s/commit/%s", ref.Owner, ref.Repo, sha)
			shaLabel := rem.output.Hyperlink(sha[:12], commitURL)
			if displayTag != ref.Ref {
				rem.output.Detail("  %s → %s → %s  %s", key, displayTag, shaLabel, tagLink)
			} else {
				rem.output.Detail("  %s → %s  %s", key, shaLabel, tagLink)
			}
			// Record both original and narrowed ref for cascade.
			rem.state.approvedRefs[refKey(ref)] = true
			if displayTag != ref.Ref {
				narrowedRef := ref
				narrowedRef.Ref = displayTag
				rem.state.approvedRefs[refKey(narrowedRef)] = true
			}
			approved = append(approved, ref)
			continue
		}

		// Fall-through case — typically a branch ref (e.g. `main`). Auto-pin
		// to the resolved SHA without prompting; if we can find a release tag
		// pointing at the same SHA, surface it as a narrowing hint.
		commitURL := fmt.Sprintf("https://github.com/%s/%s/commit/%s", ref.Owner, ref.Repo, sha)
		shaLabel := rem.output.Hyperlink(sha[:12], commitURL)
		if tag, err := rem.tagLister.BestPatchTagForSHA(rem.ctx, ref.Owner, ref.Repo, sha); err == nil && tag != "" {
			tagURL := fmt.Sprintf("https://github.com/%s/%s/releases/tag/%s", ref.Owner, ref.Repo, tag)
			tagLink := rem.output.Dim(rem.output.Hyperlink("release", tagURL))
			if ti := rem.tagLister.LookupTag(rem.ctx, ref.Owner, ref.Repo, tag); ti != nil && ti.IsImmutable {
				tagLink = rem.output.Dim("🔒 " + rem.output.Hyperlink("immutable release", tagURL))
			}
			rem.output.Detail("  %s → %s → %s  %s", key, tag, shaLabel, tagLink)
		} else {
			rem.output.Detail("  %s → %s", key, shaLabel)
		}
		rem.state.approvedRefs[refKey(ref)] = true
		approved = append(approved, ref)
	}

	if len(approved) == 0 {
		rem.Skipped++
		return nil
	}

	wr.ActionRefs = approved
	rem.state.markRefsApproved(approved)
	return rem.submitPin(wr)
}

// offerDefaultBranch checks each action ref for same-owner repos (internal
// actions) and switches bare SHA refs to the default branch. Named refs
// (tags, branches, versions) are preserved as-is.
// Returns a (possibly modified) copy of the WorkflowReport with updated refs.
func (rem *Remediator) offerDefaultBranch(wr WorkflowReport) WorkflowReport {
	updated := make([]lockfile.ActionRef, 0, len(wr.ActionRefs))
	for _, ref := range wr.ActionRefs {
		if !rem.isSameOwner(ref.Owner) {
			updated = append(updated, ref)
			continue
		}

		info, err := rem.tagLister.GetRepoInfo(rem.ctx, ref.Owner, ref.Repo)
		if err != nil {
			updated = append(updated, ref)
			continue
		}

		// Already targeting the default branch — nothing to offer.
		if ref.Ref == info.DefaultBranch {
			updated = append(updated, ref)
			continue
		}

		// Bare SHA → swap to default branch. Named refs stay as-is.
		if lockfile.IsFullSha(ref.Ref) {
			rem.output.Detail("  %s: using %s (default branch) instead of %s",
				ref.FullName(), info.DefaultBranch, ref.Ref)
			ref.Ref = info.DefaultBranch
			updated = append(updated, ref)
			continue
		}

		// Named ref (tag, branch, version) — preserve what the user wrote.
		updated = append(updated, ref)
	}

	wr.ActionRefs = updated
	return wr
}

func (rem *Remediator) handleSHAAsRef(wr WorkflowReport, finding Finding) error {
	dep := finding.Dependency

	owner, repo := dep.OwnerRepo()

	// Make the SHA a clickable link to the commit on GitHub.
	commitURL := fmt.Sprintf("https://github.com/%s/%s/commit/%s", owner, repo, dep.SHA)
	depLabel := dep.NWO + "@" + rem.output.Hyperlink(dep.SHA[:12], commitURL)

	if !rem.prompter.IsInteractive() && owner == "" {
		rem.output.Warning("%s: %s", depLabel, finding.Detail)
		rem.skipDep(dep)
		return nil
	}

	rem.output.Warning("%s: %s", depLabel, finding.Detail)

	if owner == "" {
		rem.Skipped++
		return nil
	}

	// Session memory: reuse prior internal ref choice for same-owner repos (any SHA).
	if rem.isSameOwner(owner) {
		nwo := owner + "/" + repo
		if priorRef, ok := rem.state.internalRefChoices[cachekey.ForRepo(owner, repo)]; ok {
			rem.output.Detail("  ↳ reusing prior choice for %s: %s", nwo, priorRef)
			return rem.applySHAToTag(wr, dep, owner, repo, priorRef)
		}
	}

	// Session memory: if we already chose a tag for this exact dep, auto-apply.
	if priorTag, ok := rem.state.recallChoice(dep); ok {
		rem.output.Detail("  ↳ reusing prior choice: %s", priorTag)
		return rem.applySHAToTag(wr, dep, owner, repo, priorTag)
	}

	// Try to find which tags this SHA already belongs to.
	suggestions, err := rem.tagLister.SuggestTagsForSHA(rem.ctx, owner, repo, dep.SHA)
	if err != nil {
		rem.output.Warning("could not fetch tags: %s", err)
		rem.Skipped++
		return nil
	}

	// Smart default for internal (same-owner) repos: if the SHA already
	// belongs to a tag, auto-pick it — no need to prompt. If no tag match,
	// fall back to the default branch.
	if rem.isSameOwner(owner) {
		// Prefer a tag that directly points at this SHA.
		for _, s := range suggestions {
			if s.Preferred {
				tag := s.Tag
				tagURL := TagURL(owner, repo, tag.Name)
				tagLink := rem.output.Dim(rem.output.Hyperlink("tag", tagURL))
				commitURL := fmt.Sprintf("https://github.com/%s/%s/commit/%s", owner, repo, dep.SHA)
				shaLabel := rem.output.Hyperlink(dep.SHA[:12], commitURL)
				rem.output.Detail("  ↳ already installed to %s (%s)  %s", tag.Name, shaLabel, tagLink)
				rem.state.internalRefChoices[cachekey.ForRepo(owner, repo)] = tag.Name
				rem.state.recordChoice(dep, tag.Name)
				return rem.applySHAToTag(wr, dep, owner, repo, tag.Name)
			}
		}
		// No tag match — use default branch.
		if info, err := rem.tagLister.GetRepoInfo(rem.ctx, owner, repo); err == nil {
			rem.output.Detail("  ↳ using %s (default branch) for %s/%s", info.DefaultBranch, owner, repo)
			rem.state.internalRefChoices[cachekey.ForRepo(owner, repo)] = info.DefaultBranch
			return rem.applySHAToTag(wr, dep, owner, repo, info.DefaultBranch)
		}
	}

	// Smart default: for external repos, if exactly one full-semver tag points
	// at this SHA, auto-pick it.
	if len(suggestions) > 0 && !rem.isSameOwner(owner) {
		var fullSemverTags []TagSuggestion
		for _, s := range suggestions {
			sv, ok := lockfile.ParseVersion(s.Tag.Name)
			if ok && sv.IsFull() {
				fullSemverTags = append(fullSemverTags, s)
			}
		}
		if len(fullSemverTags) == 1 {
			tag := fullSemverTags[0].Tag
			tagURL := TagURL(owner, repo, tag.Name)
			tagLink := rem.output.Dim(rem.output.Hyperlink("release", tagURL))
			if tag.IsImmutable {
				tagLink = rem.output.Dim("🔒 " + rem.output.Hyperlink("immutable release", tagURL))
			}
			// Show verifiable SHA match: tag points at the same commit.
			commitURL := fmt.Sprintf("https://github.com/%s/%s/commit/%s", owner, repo, dep.SHA)
			shaLabel := rem.output.Hyperlink(dep.SHA[:12], commitURL)
			rem.output.Detail("  ↳ auto-pinning to %s (%s)  %s", tag.Name, shaLabel, tagLink)
			rem.state.recordChoice(dep, tag.Name)
			return rem.applySHAToTag(wr, dep, owner, repo, tag.Name)
		}
	}

	// If we found tags for this SHA, present smart suggestions.
	if len(suggestions) > 0 {
		if !rem.prompter.IsInteractive() {
			// Multiple tags match — can't auto-pick, need human choice.
			rem.skipDep(dep)
			return nil
		}
		return rem.handleSHAWithSuggestions(wr, finding, suggestions, owner, repo)
	}

	// No tag matches this SHA — this is an unreleased commit. Be loud.
	noTagCommitURL := fmt.Sprintf("https://github.com/%s/%s/commit/%s", owner, repo, dep.SHA)
	shaLink := rem.output.Hyperlink(dep.SHA[:12], noTagCommitURL)
	releasesURL := fmt.Sprintf("https://github.com/%s/%s/releases", owner, repo)
	releasesLink := rem.output.Hyperlink("releases", releasesURL)
	rem.output.Error("  commit %s does not belong to any release — you are running unreleased code", shaLink)
	rem.output.Detail("  ↳ pin to a tagged release instead: %s", releasesLink)
	if !rem.prompter.IsInteractive() {
		rem.skipDep(dep)
		return nil
	}
	return rem.handleSHATagPicker(wr, finding, owner, repo)
}

func (rem *Remediator) handleSHAWithSuggestions(wr WorkflowReport, finding Finding, suggestions []TagSuggestion, owner, repo string) error {
	dep := finding.Dependency

	// Build picker — full semver first (recommended), then major tags.
	reordered := reorderSuggestions(suggestions)
	if !rem.isSameOwner(owner) {
		var filtered []TagSuggestion
		for _, s := range reordered {
			if !s.Tag.IsMajor {
				filtered = append(filtered, s)
			}
		}
		reordered = filtered
	}

	options := make([]string, 0, len(reordered)+3)
	for i, s := range reordered {
		recommend := i == 0 && !s.Tag.IsMajor && !rem.isSameOwner(owner)
		options = append(options, rem.tagLabel(owner, repo, pickerTag{
			Name:        s.Tag.Name,
			IsInstalled: s.Preferred,
			IsImmutable: s.Tag.IsImmutable,
			IsRelease:   s.Tag.IsRelease,
			IsMajor:     s.Tag.IsMajor,
		}, recommend))
	}

	var defaultBranchIdx int
	options, defaultBranchIdx = rem.defaultBranchOption(options, owner, repo)
	tagCount := len(reordered)

	// Exactly one suggested tag and no default-branch alternative: no real
	// choice to make, so auto-pin it instead of prompting.
	if tagCount == 1 && defaultBranchIdx == -1 {
		selectedTag := reordered[0].Tag
		tagURL := TagURL(owner, repo, selectedTag.Name)
		tagLink := rem.output.Dim(rem.output.Hyperlink("release", tagURL))
		if selectedTag.IsImmutable {
			tagLink = rem.output.Dim("🔒 " + rem.output.Hyperlink("immutable release", tagURL))
		}
		rem.output.Detail("  ↳ only one tag available — pinning to %s  %s", selectedTag.Name, tagLink)
		if rem.isSameOwner(owner) {
			rem.state.internalRefChoices[cachekey.ForRepo(owner, repo)] = selectedTag.Name
		}
		if err := rem.applySHAToTag(wr, dep, owner, repo, selectedTag.Name); err != nil {
			return err
		}
		rem.offerApplyAll(dep, selectedTag.Name)
		return nil
	}

	result, err := rem.runPicker(
		rem.pinPromptTitle(dep.NWO, owner, repo),
		options, tagCount, defaultBranchIdx,
		pickerSentinels{ShowAll: true},
	)
	if err != nil {
		return err
	}

	switch result.Action {
	case pickerSkip:
		rem.Skipped++
		return nil
	case pickerShowAll:
		return rem.handleSHATagPicker(wr, finding, owner, repo)
	case pickerDefaultBranch:
		info, _ := rem.tagLister.GetRepoInfo(rem.ctx, owner, repo)
		rem.state.internalRefChoices[cachekey.ForRepo(owner, repo)] = info.DefaultBranch
		if err := rem.applySHAToTag(wr, dep, owner, repo, info.DefaultBranch); err != nil {
			return err
		}
		rem.offerApplyAll(dep, info.DefaultBranch)
		return nil
	default:
		selectedTag := reordered[result.TagIndex].Tag
		if rem.isSameOwner(owner) {
			rem.state.internalRefChoices[cachekey.ForRepo(owner, repo)] = selectedTag.Name
		}
		if err := rem.applySHAToTag(wr, dep, owner, repo, selectedTag.Name); err != nil {
			return err
		}
		rem.offerApplyAll(dep, selectedTag.Name)
		return nil
	}
}

func (rem *Remediator) handleSHATagPicker(wr WorkflowReport, finding Finding, owner, repo string) error {
	dep := finding.Dependency

	curated, err := rem.tagLister.CuratePickerTags(rem.ctx, owner, repo, dep.SHA)
	if err != nil {
		rem.output.Warning("could not fetch tags: %s", err)
		rem.Skipped++
		return nil
	}

	if len(curated) == 0 {
		rem.output.Warning("no tags found for %s/%s", owner, repo)
		rem.Skipped++
		return nil
	}

	options := make([]string, 0, len(curated)+3)
	for _, pt := range curated {
		options = append(options, rem.tagLabel(owner, repo, pickerTag{
			Name:        pt.Tag.Name,
			IsInstalled: pt.Installed,
			IsImmutable: pt.Tag.IsImmutable,
			IsRelease:   pt.Tag.IsRelease,
			IsMajor:     pt.Tag.IsMajor,
		}, false))
	}

	var defaultBranchIdx int
	options, defaultBranchIdx = rem.defaultBranchOption(options, owner, repo)
	tagCount := len(curated)

	// Exactly one tag and no default-branch alternative: there's no real
	// choice for the user to make (the only other options are "open releases"
	// and "skip"). Auto-pin it and narrate, rather than forcing a prompt.
	if tagCount == 1 && defaultBranchIdx == -1 {
		selectedTag := curated[0].Tag
		tagURL := TagURL(owner, repo, selectedTag.Name)
		tagLink := rem.output.Dim(rem.output.Hyperlink("release", tagURL))
		if selectedTag.IsImmutable {
			tagLink = rem.output.Dim("🔒 " + rem.output.Hyperlink("immutable release", tagURL))
		}
		rem.output.Detail("  ↳ only one tag available — pinning to %s  %s", selectedTag.Name, tagLink)
		if rem.isSameOwner(owner) {
			rem.state.internalRefChoices[cachekey.ForRepo(owner, repo)] = selectedTag.Name
		}
		if err := rem.applySHAToTag(wr, dep, owner, repo, selectedTag.Name); err != nil {
			return err
		}
		rem.offerApplyAll(dep, selectedTag.Name)
		return nil
	}

	// External repo (no default-branch alternative) with a full-semver
	// release at the top of the curated list: pick it. The user's SHA
	// doesn't map to any tag; the latest stable release is the safe default
	// and asking them to pick from a list of N versions is pure friction.
	// `gh actions-pin upgrade` lets them shift to a different version later.
	if defaultBranchIdx == -1 && len(curated) > 0 {
		top := curated[0].Tag
		if sv, ok := lockfile.ParseVersion(top.Name); ok && sv.IsFull() && top.IsRelease {
			tagURL := TagURL(owner, repo, top.Name)
			tagLink := rem.output.Dim(rem.output.Hyperlink("release", tagURL))
			if top.IsImmutable {
				tagLink = rem.output.Dim("🔒 " + rem.output.Hyperlink("immutable release", tagURL))
			}
			rem.output.Detail("  ↳ pinning to latest release %s  %s", top.Name, tagLink)
			if err := rem.applySHAToTag(wr, dep, owner, repo, top.Name); err != nil {
				return err
			}
			rem.offerApplyAll(dep, top.Name)
			return nil
		}
	}

	releasesURL := fmt.Sprintf("https://github.com/%s/%s/releases", owner, repo)
	result, err := rem.runPicker(
		rem.pinPromptTitle(owner+"/"+repo, owner, repo),
		options, tagCount, defaultBranchIdx,
		pickerSentinels{OpenReleases: releasesURL},
	)
	if err != nil {
		return err
	}

	switch result.Action {
	case pickerSkip:
		rem.Skipped++
		return nil
	case pickerOpenReleases:
		rem.output.Info("Opening releases page...")
		openBrowser(releasesURL)
		rem.Skipped++
		return nil
	case pickerDefaultBranch:
		info, _ := rem.tagLister.GetRepoInfo(rem.ctx, owner, repo)
		rem.state.internalRefChoices[cachekey.ForRepo(owner, repo)] = info.DefaultBranch
		if err := rem.applySHAToTag(wr, dep, owner, repo, info.DefaultBranch); err != nil {
			return err
		}
		rem.offerApplyAll(dep, info.DefaultBranch)
		return nil
	default:
		selectedTag := curated[result.TagIndex].Tag
		if rem.isSameOwner(owner) {
			rem.state.internalRefChoices[cachekey.ForRepo(owner, repo)] = selectedTag.Name
		}
		if err := rem.applySHAToTag(wr, dep, owner, repo, selectedTag.Name); err != nil {
			return err
		}
		rem.offerApplyAll(dep, selectedTag.Name)
		return nil
	}
}

func (rem *Remediator) handleRefChanged(wr WorkflowReport, finding Finding) error {
	dep := finding.Dependency
	newRef := ""
	if finding.ActionRef != nil {
		newRef = finding.ActionRef.Ref
	}
	rem.output.Warning("%s: %s", dep.Key(), finding.Detail)

	// If the workflow's new ref no longer exists upstream (e.g. someone
	// typo'd or pointed at a deleted tag), don't silently re-resolve to a
	// missing ref. Divert into the full tag picker so the user can choose
	// a real tag, open the releases page, or skip.
	owner, repo := dep.OwnerRepo()
	if rem.prompter.IsInteractive() && owner != "" && newRef != "" && !lockfile.IsFullSha(newRef) {
		if rem.tagLister.LookupTag(rem.ctx, owner, repo, newRef) == nil {
			rem.output.Detail("  ref %q no longer exists upstream — pick a valid tag instead", newRef)
			return rem.handleSHATagPicker(wr, finding, owner, repo)
		}
	}

	// The workflow is the source of truth: just pin what it asks for,
	// without prompting the user to confirm a "more specific" version.
	return rem.applyReResolve(wr, dep)
}

func (rem *Remediator) handleStale(wr WorkflowReport, finding Finding) error {
	dep := finding.Dependency
	rem.output.Detail("%s: no longer in workflow — cleaning up", dep.Key())

	// Auto-clean: re-resolve rewrites the lockfile without orphaned deps.
	return rem.applyReResolve(wr, dep)
}

// openBrowser attempts to open a URL in the user's browser. The URL is
// validated to ensure it's a well-formed https:// link before being
// passed to the platform launcher; this matters because the URL is
// constructed from action owner/repo strings that originate in workflow
// YAML (i.e. user input). We also pin the launcher to a hardcoded literal
// so the command is never shell-interpreted.
func openBrowser(rawURL string) {
	u, err := url.Parse(rawURL)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return
	}
	cmd := "open"
	if _, err := os.Stat("/usr/bin/xdg-open"); err == nil {
		cmd = "xdg-open"
	}
	// #nosec G204 — cmd is a hardcoded literal (open / xdg-open); the URL
	// is parsed and required to be https with a non-empty host above.
	proc := exec.Command(cmd, u.String())
	_ = proc.Start()
}
