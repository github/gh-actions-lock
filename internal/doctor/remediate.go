package doctor

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"strings"

	"github.com/cli/go-gh/v2/pkg/api"
	"github.com/github/gh-actions-pin/internal/lockfile"
	"github.com/github/gh-actions-pin/internal/resolver"
	"github.com/github/gh-actions-pin/internal/ui"
)

// RemediateOptions controls the remediation flow.
type RemediateOptions struct {
	Interactive bool   // true when stderr is a TTY
	RepoOwner   string // owner of the repo being scanned (for same-owner detection)
	RepoName    string // name of the repo being scanned (used for repo-level remediations)
}

// Remediator walks through findings and applies fixes interactively.
type Remediator struct {
	prompter  Prompter
	resolver  *resolver.Resolver
	tagLister *TagLister
	client    *api.RESTClient
	store     *lockfile.Store
	output    *ui.UI
	opts      RemediateOptions

	state sessionState

	// How many remaining occurrences of each choiceKey across all workflows.
	remaining map[string]int

	// Counters for summary.
	Fixed          int
	Skipped        int
	Alerted        int
	Unresolved     int
	SkippedDeps    []string // unique dep keys that were skipped (for summary)
	AlertedDeps    []string // dep keys that triggered security alerts (deduplicated)
	UnresolvedDeps []string // dep keys whose ref could not be resolved (e.g. bad tag)
}

func (rem *Remediator) markUnresolved(key string) {
	for _, k := range rem.UnresolvedDeps {
		if k == key {
			return
		}
	}
	rem.UnresolvedDeps = append(rem.UnresolvedDeps, key)
	rem.Unresolved++
}

// NewRemediator creates a new Remediator.
func NewRemediator(p Prompter, r *resolver.Resolver, client *api.RESTClient, store *lockfile.Store, out *ui.UI, opts RemediateOptions) *Remediator {
	return &Remediator{
		prompter:  p,
		resolver:  r,
		tagLister: NewTagLister(client),
		client:    client,
		store:     store,
		output:    out,
		opts:      opts,
		state:     newSessionState(),
	}
}

// isSameOwner returns true if the action's owner matches the repo being scanned,
// meaning it's an internal/first-party action where default-branch pinning is sensible.
func (rem *Remediator) isSameOwner(actionOwner string) bool {
	return rem.opts.RepoOwner != "" && strings.EqualFold(rem.opts.RepoOwner, actionOwner)
}

// offerApplyAll checks if this dep appears in more workflows and auto-applies
// the same choice everywhere. No prompt needed — same dep, same tag, just do it.
func (rem *Remediator) offerApplyAll(dep *lockfile.Dependency, tag string) {
	key := choiceKey(dep)
	rem.remaining[key]--
	others := rem.remaining[key]
	if others <= 0 {
		return
	}

	rem.output.Detail("  ↳ applying %s to %d remaining %s", tag, others, ui.Pluralize(others, "file", "files"))
	rem.state.recordChoice(dep, tag)
}

// Remediate walks through a report and handles each workflow that needs attention.
func (rem *Remediator) Remediate(report *Report) error {
	actionable := report.WorkflowsNeedingAttention()

	// Pre-scan: count how many times each dep appears so we can offer "apply to all".
	rem.remaining = make(map[string]int)
	for _, wr := range actionable {
		for _, f := range wr.Findings {
			if f.Category == CategorySHAAsRef && f.Dependency != nil {
				rem.remaining[choiceKey(f.Dependency)]++
			}
		}
	}

	for _, wr := range actionable {
		if err := rem.remediateWorkflow(wr); err != nil {
			return err
		}
	}

	return nil
}

func (rem *Remediator) depKey(f Finding) string {
	if f.Dependency != nil {
		return f.Dependency.Key()
	}
	if f.ActionRef != nil {
		return f.ActionRef.FullName() + "@" + f.ActionRef.Ref
	}
	return ""
}

func (rem *Remediator) addAlertedDep(f Finding) {
	key := rem.depKey(f)
	for _, k := range rem.AlertedDeps {
		if k == key {
			return
		}
	}
	rem.AlertedDeps = append(rem.AlertedDeps, key)
}

// skipDep records a dependency as skipped (needs interactive resolution).
func (rem *Remediator) skipDep(dep *lockfile.Dependency) {
	key := dep.Key()
	rem.output.Skip("%s: requires interactive tag selection", key)
	rem.state.choices[key] = "skipped"
	rem.SkippedDeps = append(rem.SkippedDeps, key)
	rem.Skipped++
}

func (rem *Remediator) repoNWO(f Finding) string {
	if f.Dependency != nil {
		owner, repo := f.Dependency.OwnerRepo()
		if owner != "" {
			return owner + "/" + repo
		}
	}
	if f.ActionRef != nil {
		return f.ActionRef.Owner + "/" + f.ActionRef.Repo
	}
	return ""
}

// pinPromptTitle returns the Select prompt title annotated with repo visibility.
func (rem *Remediator) pinPromptTitle(nwo, owner, repo string) string {
	title := fmt.Sprintf("Pin %s to which tag?", nwo)
	if info, err := rem.tagLister.GetRepoInfo(owner, repo); err == nil {
		title += fmt.Sprintf("  (%s)", info.VisibilityLabel())
	}
	return title
}

func (rem *Remediator) remediateWorkflow(wr WorkflowReport) error {
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
		if finding.Category == CategoryValid || finding.Category == CategoryRunOnly || finding.Category == CategoryRefMoved {
			continue
		}

		// For non-interactive SHA_AS_REF, check if this dep was already printed.
		// If so, skip silently (no header, no blank line).
		if !rem.prompter.IsInteractive() && finding.Category == CategorySHAAsRef {
			if finding.Dependency != nil {
				if _, seen := rem.state.choices[finding.Dependency.Key()]; seen {
					rem.Skipped++
					continue
				}
			}
		}

		// Alerted-only categories (Imposter/Forgery/Misleading) were already
		// fully presented by presentCheckResults in non-interactive mode.
		// Just register the alert here without re-printing the per-workflow
		// header or finding details.
		if !rem.prompter.IsInteractive() {
			switch finding.Category {
			case CategoryImposterCommit, CategoryLockfileForgery, CategoryMisleadingSHA:
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

		case CategoryImposterCommit:
			rem.output.Error("%s", finding.Detail)
			rem.output.Hint("This may indicate a fork-network injection attack. Do not auto-fix.")
			rem.Alerted++
			rem.addAlertedDep(finding)

		case CategoryLockfileForgery:
			rem.output.Error("LOCKFILE_FORGERY %s: %s", rem.depKey(finding), finding.Detail)
			rem.output.Hint("The pinned SHA was never in this ref's lineage — possible lockfile tampering.")
			rem.Alerted++
			rem.addAlertedDep(finding)

		case CategoryMisleadingSHA:
			rem.output.Error("MISLEADING_SHA %s: %s", rem.depKey(finding), finding.Detail)
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
		rem.output.Detail("  see: %s", rem.output.Dim(rem.output.Hyperlink("docs", docURL)))
	}

	if !rem.prompter.IsInteractive() {
		// Non-interactive: auto-pin all refs (ref→SHA is deterministic).
		rem.state.markRefsApproved(wr.ActionRefs)
		return rem.applyPin(wr)
	}

	// For internal repos, offer the default branch as an alternative ref.
	wr = rem.offerDefaultBranch(wr)

	// If all refs in this workflow were already approved in a prior workflow, auto-apply.
	if rem.state.allRefsApproved(wr.ActionRefs) {
		rem.output.Detail("  ↳ all actions already approved — auto-pinning")
		return rem.applyPin(wr)
	}

	// Resolve all refs to show the SHAs they'll pin to.
	resolved, _ := rem.resolver.ResolveAllRecursive(wr.ActionRefs)
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
			if info, err := rem.tagLister.GetRepoInfo(ref.Owner, ref.Repo); err == nil {
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

		displayTag := ref.Ref
		autoPin := false

		// Case 1: Already a full semver tag (v4.3.1) — good default, verify it's a real tag.
		if sv, svOK := lockfile.ParseSemver(ref.Ref); svOK && sv.IsFullSemver() {
			if rem.tagLister.LookupTag(ref.Owner, ref.Repo, ref.Ref) != nil {
				autoPin = true
			}
		}

		// Case 2: Mutable tag (v4, v4.2) — auto-pin if there's exactly one matching patch tag.
		if !autoPin && IsMutableVersionTag(ref.Ref) {
			if uniqueTag, err := rem.tagLister.UniquePatchTagForRef(ref.Owner, ref.Repo, sha, ref.Ref); err == nil && uniqueTag != "" {
				displayTag = uniqueTag
				autoPin = true
			}
		}

		if autoPin {
			tagURL := fmt.Sprintf("https://github.com/%s/%s/releases/tag/%s", ref.Owner, ref.Repo, displayTag)
			tagLink := rem.output.Dim(rem.output.Hyperlink("release", tagURL))
			if ti := rem.tagLister.LookupTag(ref.Owner, ref.Repo, displayTag); ti != nil && ti.IsImmutable {
				tagLink = rem.output.Dim("🔒 " + rem.output.Hyperlink("immutable release", tagURL))
			}
			// Show verifiable SHA match: tag resolves to the same commit.
			commitURL := fmt.Sprintf("https://github.com/%s/%s/commit/%s", ref.Owner, ref.Repo, sha)
			shaLabel := rem.output.Hyperlink(sha[:12], commitURL)
			if displayTag != ref.Ref {
				rem.output.Detail("    %s → %s → %s  %s", key, displayTag, shaLabel, tagLink)
			} else {
				rem.output.Detail("    %s → %s  %s", key, shaLabel, tagLink)
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

		// Ambiguous case — prompt the user.
		narrowHint := ""
		if IsMutableVersionTag(ref.Ref) {
			if patchTag, err := rem.tagLister.BestPatchTagForSHA(ref.Owner, ref.Repo, sha); err == nil && patchTag != "" {
				narrowHint = fmt.Sprintf(" → %s", patchTag)
				displayTag = patchTag
			}
		}

		tagLink := ""
		tagURL := fmt.Sprintf("https://github.com/%s/%s/releases/tag/%s", ref.Owner, ref.Repo, displayTag)
		if ti := rem.tagLister.LookupTag(ref.Owner, ref.Repo, displayTag); ti != nil && ti.IsImmutable {
			tagLink = "  " + rem.output.Dim("🔒 "+rem.output.Hyperlink("immutable release", tagURL))
		} else {
			tagLink = "  " + rem.output.Dim(rem.output.Hyperlink("release", tagURL))
		}

		rem.output.Detail("  %s%s → %s%s", key, narrowHint, sha[:12], tagLink)

		ok, err := rem.prompter.Confirm(fmt.Sprintf("Pin %s?", ref.FullName()+"@"+displayTag), true)
		if err != nil {
			if errors.Is(err, ErrAborted) {
				return ErrAborted
			}
			continue
		}
		if ok {
			approved = append(approved, ref)
		} else {
			rem.output.Skip("skipped %s", ref.FullName())
		}
	}

	if len(approved) == 0 {
		rem.Skipped++
		return nil
	}

	wr.ActionRefs = approved
	rem.state.markRefsApproved(approved)
	return rem.applyPin(wr)
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

		info, err := rem.tagLister.GetRepoInfo(ref.Owner, ref.Repo)
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
		if lockfile.IsFullSHA(ref.Ref) {
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
		if priorRef, ok := rem.state.internalRefChoices[nwo]; ok {
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
	suggestions, err := rem.tagLister.SuggestTagsForSHA(owner, repo, dep.SHA)
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
				nwo := owner + "/" + repo
				rem.state.internalRefChoices[nwo] = tag.Name
				rem.state.recordChoice(dep, tag.Name)
				return rem.applySHAToTag(wr, dep, owner, repo, tag.Name)
			}
		}
		// No tag match — use default branch.
		if info, err := rem.tagLister.GetRepoInfo(owner, repo); err == nil {
			rem.output.Detail("  ↳ using %s (default branch) for %s/%s", info.DefaultBranch, owner, repo)
			nwo := owner + "/" + repo
			rem.state.internalRefChoices[nwo] = info.DefaultBranch
			return rem.applySHAToTag(wr, dep, owner, repo, info.DefaultBranch)
		}
	}

	// Smart default: for external repos, if exactly one full-semver tag points
	// at this SHA, auto-pick it.
	if len(suggestions) > 0 && !rem.isSameOwner(owner) {
		var fullSemverTags []TagSuggestion
		for _, s := range suggestions {
			sv, ok := lockfile.ParseSemver(s.Tag.Name)
			if ok && sv.IsFullSemver() {
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
		info, _ := rem.tagLister.GetRepoInfo(owner, repo)
		rem.state.internalRefChoices[owner+"/"+repo] = info.DefaultBranch
		if err := rem.applySHAToTag(wr, dep, owner, repo, info.DefaultBranch); err != nil {
			return err
		}
		rem.offerApplyAll(dep, info.DefaultBranch)
		return nil
	default:
		selectedTag := reordered[result.TagIndex].Tag
		if rem.isSameOwner(owner) {
			rem.state.internalRefChoices[owner+"/"+repo] = selectedTag.Name
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

	curated, err := rem.tagLister.CuratePickerTags(owner, repo, dep.SHA)
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
		info, _ := rem.tagLister.GetRepoInfo(owner, repo)
		rem.state.internalRefChoices[owner+"/"+repo] = info.DefaultBranch
		if err := rem.applySHAToTag(wr, dep, owner, repo, info.DefaultBranch); err != nil {
			return err
		}
		rem.offerApplyAll(dep, info.DefaultBranch)
		return nil
	default:
		selectedTag := curated[result.TagIndex].Tag
		if rem.isSameOwner(owner) {
			rem.state.internalRefChoices[owner+"/"+repo] = selectedTag.Name
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
	if rem.prompter.IsInteractive() && owner != "" && newRef != "" && !lockfile.IsFullSHA(newRef) {
		if rem.tagLister.LookupTag(owner, repo, newRef) == nil {
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
