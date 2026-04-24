package doctor

import (
	"errors"
	"fmt"
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
	Write       bool   // --write: auto-apply safe fixes without prompting
	Interactive bool   // true when stderr is a TTY
	RepoOwner   string // owner of the repo being scanned (for same-owner detection)
}

// Remediator walks through findings and applies fixes interactively.
type Remediator struct {
	prompter  Prompter
	resolver  *resolver.Resolver
	tagLister *TagLister
	output    *ui.UI
	opts      RemediateOptions

	// Session memory: maps "owner/repo@SHA" → chosen tag name.
	// When the same dep appears in multiple workflows, auto-apply the prior choice.
	choices map[string]string

	// Internal repo ref choices: maps "owner/repo" → chosen ref (e.g. "main" or "v2").
	// Applied across all workflows for same-owner actions.
	internalRefChoices map[string]string

	// Approved refs: tracks "owner/repo@ref" that the user already approved for pinning.
	// When all refs in a workflow were already approved, auto-apply without prompting.
	approvedRefs map[string]bool

	// How many remaining occurrences of each choiceKey across all workflows.
	remaining map[string]int

	// Counters for summary.
	Fixed   int
	Skipped int
	Alerted int
}

// NewRemediator creates a new Remediator.
func NewRemediator(p Prompter, r *resolver.Resolver, client *api.RESTClient, out *ui.UI, opts RemediateOptions) *Remediator {
	return &Remediator{
		prompter:           p,
		resolver:           r,
		tagLister:          NewTagLister(client),
		output:             out,
		opts:               opts,
		choices:            make(map[string]string),
		internalRefChoices: make(map[string]string),
		approvedRefs:       make(map[string]bool),
	}
}

// isSameOwner returns true if the action's owner matches the repo being scanned,
// meaning it's an internal/first-party action where default-branch pinning is sensible.
func (rem *Remediator) isSameOwner(actionOwner string) bool {
	return rem.opts.RepoOwner != "" && strings.EqualFold(rem.opts.RepoOwner, actionOwner)
}

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

// offerApplyAll checks if this dep appears in more workflows and auto-applies
// the same choice everywhere. No prompt needed — same dep, same tag, just do it.
func (rem *Remediator) offerApplyAll(dep *lockfile.Dependency, tag string) {
	key := rem.choiceKey(dep)
	rem.remaining[key]--
	others := rem.remaining[key]
	if others <= 0 {
		return
	}

	rem.output.Detail("  ↳ applying %s to %d remaining %s", tag, others, ui.Pluralize(others, "file", "files"))
	rem.recordChoice(dep, tag)
}

// Remediate walks through a report and handles each workflow that needs attention.
func (rem *Remediator) Remediate(report *Report) error {
	actionable := report.WorkflowsNeedingAttention()
	if len(actionable) == 0 {
		return nil
	}

	// Pre-scan: count how many times each dep appears so we can offer "apply to all".
	rem.remaining = make(map[string]int)
	for _, wr := range actionable {
		for _, f := range wr.Findings {
			if f.Category == CategorySHAAsRef && f.Dependency != nil {
				rem.remaining[rem.choiceKey(f.Dependency)]++
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

func (rem *Remediator) repoNWO(f Finding) string {
	if f.Dependency != nil {
		parts := strings.SplitN(f.Dependency.NWO, "/", 3)
		if len(parts) >= 2 {
			return parts[0] + "/" + parts[1]
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
	rem.output.Header("%s", wr.Path)

	first := true
	for _, finding := range wr.Findings {
		if finding.Category == CategoryValid || finding.Category == CategoryRunOnly {
			continue
		}

		if !first {
			fmt.Fprintln(os.Stderr)
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

		case CategoryTampered:
			rem.output.Error("%s: %s", finding.Dependency.Key(), finding.Detail)
			rem.output.Hint("Ref has moved — investigate before updating. Use `gh actions-pin upgrade` manually.")
			rem.Alerted++

		case CategoryUnreachable:
			rem.output.Error("%s", finding.Detail)
			rem.output.Hint("This may indicate a fork-network injection attack. Do not auto-fix.")
			rem.Alerted++
		}
	}

	fmt.Fprintln(os.Stderr)
	return nil
}

func (rem *Remediator) handleNotPinned(wr WorkflowReport) error {
	rem.output.Warning("%d %s found but not pinned", len(wr.ActionRefs), ui.Pluralize(len(wr.ActionRefs), "action", "actions"))

	if rem.opts.Write {
		rem.markRefsApproved(wr.ActionRefs)
		return rem.applyPin(wr)
	}

	if !rem.prompter.IsInteractive() {
		rem.output.Hint("run `gh actions-pin doctor %s`", wr.Path)
		rem.Skipped++
		return nil
	}

	// For internal repos, offer the default branch as an alternative ref.
	wr = rem.offerDefaultBranch(wr)

	// If all refs in this workflow were already approved in a prior workflow, auto-apply.
	if rem.allRefsApproved(wr.ActionRefs) {
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
		key := ref.FullName() + "@" + ref.Ref

		// Prior choice — auto-apply without prompting.
		if rem.approvedRefs[refKey(ref)] {
			sha := shaByKey[key]
			rem.output.Detail("  %s → %s  %s", key, sha[:12], rem.output.Dim("↩ prior choice"))
			approved = append(approved, ref)
			continue
		}

		// Internal (same-owner) action — auto-apply without prompting.
		if rem.isSameOwner(ref.Owner) {
			sha := shaByKey[key]
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
		sha, ok := shaByKey[key]
		if !ok {
			rem.output.Detail("  %s  (could not resolve)", key)
			continue
		}

		displayTag := ref.Ref
		autoPin := false

		// Case 1: Already a full semver tag (v4.3.1) — good default, verify it's a real tag.
		if sv, svOK := parseSemver(ref.Ref); svOK && sv.IsFullSemver() {
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
			rem.approvedRefs[refKey(ref)] = true
			if displayTag != ref.Ref {
				narrowedRef := ref
				narrowedRef.Ref = displayTag
				rem.approvedRefs[refKey(narrowedRef)] = true
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
	rem.markRefsApproved(approved)
	return rem.applyPin(wr)
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

// offerDefaultBranch checks each action ref for same-owner repos (internal
// actions) and switches bare SHA or non-semver refs to the default branch.
// For refs that already look like a version tag, offers a choice.
// Uses session memory so the user is only asked once per owner/repo.
// Returns a (possibly modified) copy of the WorkflowReport with updated refs.
func (rem *Remediator) offerDefaultBranch(wr WorkflowReport) WorkflowReport {
	updated := make([]lockfile.ActionRef, 0, len(wr.ActionRefs))
	for _, ref := range wr.ActionRefs {
		if !rem.isSameOwner(ref.Owner) {
			updated = append(updated, ref)
			continue
		}

		nwo := ref.Owner + "/" + ref.Repo

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

		// Session memory: reuse prior choice for this repo, but only for
		// non-version refs. Version-ish refs must match the workflow uses: line.
		if !LooksLikeVersion(ref.Ref) {
			if priorRef, ok := rem.internalRefChoices[nwo]; ok {
				if priorRef != ref.Ref {
					rem.output.Detail("  ↳ reusing prior choice for %s: %s", ref.FullName(), priorRef)
					ref.Ref = priorRef
				}
				updated = append(updated, ref)
				continue
			}
		}

		// Bare SHA or non-version ref on a same-owner repo → use default branch.
		if isSHARef(ref.Ref) || !LooksLikeVersion(ref.Ref) {
			rem.output.Detail("  %s: using %s (default branch) instead of %s",
				ref.FullName(), info.DefaultBranch, ref.Ref)
			ref.Ref = info.DefaultBranch
			rem.internalRefChoices[nwo] = info.DefaultBranch
			updated = append(updated, ref)
			continue
		}

		// Version-ish ref on a same-owner repo — pin as-is. Changing the ref
		// here without rewriting the workflow uses: line would create a mismatch.
		updated = append(updated, ref)
	}

	wr.ActionRefs = updated
	return wr
}

// isSHARef returns true if ref looks like a full commit SHA (40 hex chars).
func isSHARef(ref string) bool {
	if len(ref) != 40 {
		return false
	}
	for _, c := range ref {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
			return false
		}
	}
	return true
}

// LooksLikeVersion returns true if ref starts with "v" followed by a digit.
func LooksLikeVersion(ref string) bool {
	if len(ref) < 2 {
		return false
	}
	return ref[0] == 'v' && ref[1] >= '0' && ref[1] <= '9'
}

func (rem *Remediator) handleSHAAsRef(wr WorkflowReport, finding Finding) error {
	dep := finding.Dependency

	parts := strings.SplitN(dep.NWO, "/", 3)
	owner, repo := "", ""
	if len(parts) >= 2 {
		owner, repo = parts[0], parts[1]
	}

	// Make the SHA a clickable link to the commit on GitHub.
	commitURL := fmt.Sprintf("https://github.com/%s/%s/commit/%s", owner, repo, dep.SHA)
	depLabel := dep.NWO + "@" + rem.output.Hyperlink(dep.SHA[:12], commitURL)
	rem.output.Warning("%s: %s", depLabel, finding.Detail)

	if !rem.prompter.IsInteractive() {
		rem.output.Hint("%s", finding.Remediation)
		rem.Skipped++
		return nil
	}

	// Batch mode can't choose a tag — skip.
	if rem.opts.Write {
		rem.output.Skip("%s: requires interactive tag selection", dep.Key())
		rem.Skipped++
		return nil
	}

	if owner == "" {
		rem.Skipped++
		return nil
	}

	// Session memory: reuse prior internal ref choice for same-owner repos (any SHA).
	if rem.isSameOwner(owner) {
		nwo := owner + "/" + repo
		if priorRef, ok := rem.internalRefChoices[nwo]; ok {
			rem.output.Detail("  ↳ reusing prior choice for %s: %s", nwo, priorRef)
			return rem.applySHAToTag(wr, dep, owner, repo, priorRef)
		}
	}

	// Session memory: if we already chose a tag for this exact dep, auto-apply.
	if priorTag, ok := rem.recallChoice(dep); ok {
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

	// Smart default: for external repos, if exactly one full-semver tag points
	// at this SHA, auto-pick it. Internal repos always prompt — release patterns
	// vary (some follow tags, others follow main).
	if len(suggestions) > 0 && !rem.isSameOwner(owner) {
		var fullSemverTags []TagSuggestion
		for _, s := range suggestions {
			sv, ok := parseSemver(s.Tag.Name)
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
			rem.recordChoice(dep, tag.Name)
			return rem.applySHAToTag(wr, dep, owner, repo, tag.Name)
		}
	}

	// If we found tags for this SHA, present smart suggestions.
	if len(suggestions) > 0 {
		return rem.handleSHAWithSuggestions(wr, finding, suggestions, owner, repo)
	}

	// No tag matches this SHA — fall back to the full tag picker.
	return rem.handleSHATagPicker(wr, finding, owner, repo)
}

func (rem *Remediator) handleSHAWithSuggestions(wr WorkflowReport, finding Finding, suggestions []TagSuggestion, owner, repo string) error {
	dep := finding.Dependency

	// Build picker — full semver first (recommended), then major tags.
	options := make([]string, 0, len(suggestions)+3)
	reordered := reorderSuggestions(suggestions)
	// For external repos, drop major-only tags — we always pin to patch versions.
	if !rem.isSameOwner(owner) {
		var filtered []TagSuggestion
		for _, s := range reordered {
			if !s.Tag.IsMajor {
				filtered = append(filtered, s)
			}
		}
		reordered = filtered
	}
	for i, s := range reordered {
		// Make only the tag name a clickable link.
		tagURL := TagURL(owner, repo, s.Tag.Name)
		label := rem.output.Hyperlink(s.Tag.Name, tagURL)
		if s.Preferred {
			label += "  📦 installed"
		}
		if !rem.isSameOwner(owner) {
			if s.Tag.IsImmutable {
				label += "  🔒 immutable"
			} else if s.Tag.IsRelease {
				label += "  (release)"
			}
		}
		if i == 0 && !s.Tag.IsMajor && !rem.isSameOwner(owner) {
			label += "  (recommended)"
		}
		if age := FormatTagAge(rem.tagLister.ReleaseDate(owner, repo, s.Tag.Name)); age != "" {
			label += "  " + age
		}
		options = append(options, label)
	}

	// For same-owner repos, offer the default branch.
	defaultBranchIdx := -1
	if rem.isSameOwner(owner) {
		if info, err := rem.tagLister.GetRepoInfo(owner, repo); err == nil {
			branchURL := fmt.Sprintf("https://github.com/%s/%s/tree/%s", owner, repo, info.DefaultBranch)
			label := rem.output.Hyperlink(info.DefaultBranch, branchURL) + "  (default branch)"
			if age := FormatTagAge(info.PushedAt); age != "" {
				label += "  last push " + age
			}
			options = append(options, label)
			defaultBranchIdx = len(options) - 1
		}
	}

	options = append(options, "Show all tags")
	options = append(options, "Skip this action")

	idx, err := rem.prompter.Select(
		rem.pinPromptTitle(dep.NWO, owner, repo),
		options,
	)
	if err != nil {
		if errors.Is(err, ErrAborted) {
			return ErrAborted
		}
		rem.Skipped++
		return nil
	}
	if idx < 0 || idx >= len(options) {
		rem.Skipped++
		return nil
	}

	if idx == len(options)-1 {
		rem.Skipped++
		return nil
	}
	if idx == len(options)-2 {
		// Fall through to full tag picker.
		return rem.handleSHATagPicker(wr, finding, owner, repo)
	}
	if idx == defaultBranchIdx {
		info, _ := rem.tagLister.GetRepoInfo(owner, repo)
		if rem.isSameOwner(owner) {
			rem.internalRefChoices[owner+"/"+repo] = info.DefaultBranch
		}
		if err := rem.applySHAToTag(wr, dep, owner, repo, info.DefaultBranch); err != nil {
			return err
		}
		rem.offerApplyAll(dep, info.DefaultBranch)
		return nil
	}

	selectedTag := reordered[idx].Tag
	if rem.isSameOwner(owner) {
		rem.internalRefChoices[owner+"/"+repo] = selectedTag.Name
	}
	if err := rem.applySHAToTag(wr, dep, owner, repo, selectedTag.Name); err != nil {
		return err
	}
	rem.offerApplyAll(dep, selectedTag.Name)
	return nil
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
		// Make only the tag name a clickable link; append decorators after.
		tagURL := TagURL(owner, repo, pt.Tag.Name)
		label := rem.output.Hyperlink(pt.Tag.Name, tagURL)
		if pt.Installed {
			label += "  📦 installed"
		}
		if !rem.isSameOwner(owner) {
			if pt.Tag.IsImmutable {
				label += "  🔒 immutable"
			} else if pt.Tag.IsRelease {
				label += "  (release)"
			}
		}
		if age := FormatTagAge(rem.tagLister.ReleaseDate(owner, repo, pt.Tag.Name)); age != "" {
			label += "  " + age
		}
		options = append(options, label)
	}

	// For same-owner repos, offer the default branch.
	defaultBranchIdx := -1
	if rem.isSameOwner(owner) {
		if info, err := rem.tagLister.GetRepoInfo(owner, repo); err == nil {
			branchURL := fmt.Sprintf("https://github.com/%s/%s/tree/%s", owner, repo, info.DefaultBranch)
			label := rem.output.Hyperlink(info.DefaultBranch, branchURL) + "  (default branch)"
			if age := FormatTagAge(info.PushedAt); age != "" {
				label += "  last push " + age
			}
			options = append(options, label)
			defaultBranchIdx = len(options) - 1
		}
	}

	options = append(options, fmt.Sprintf("Open releases → https://github.com/%s/%s/releases", owner, repo))
	options = append(options, "Skip this action")

	idx, err := rem.prompter.Select(
		rem.pinPromptTitle(owner+"/"+repo, owner, repo),
		options,
	)
	if err != nil {
		if errors.Is(err, ErrAborted) {
			return ErrAborted
		}
		rem.Skipped++
		return nil
	}
	if idx < 0 || idx >= len(options) {
		rem.Skipped++
		return nil
	}

	if idx == len(options)-1 {
		rem.Skipped++
		return nil
	}
	if idx == len(options)-2 {
		rem.output.Info("Opening releases page...")
		openBrowser(fmt.Sprintf("https://github.com/%s/%s/releases", owner, repo))
		rem.Skipped++
		return nil
	}
	if idx == defaultBranchIdx {
		info, _ := rem.tagLister.GetRepoInfo(owner, repo)
		if rem.isSameOwner(owner) {
			rem.internalRefChoices[owner+"/"+repo] = info.DefaultBranch
		}
		if err := rem.applySHAToTag(wr, dep, owner, repo, info.DefaultBranch); err != nil {
			return err
		}
		rem.offerApplyAll(dep, info.DefaultBranch)
		return nil
	}

	selectedTag := curated[idx].Tag
	if rem.isSameOwner(owner) {
		rem.internalRefChoices[owner+"/"+repo] = selectedTag.Name
	}
	if err := rem.applySHAToTag(wr, dep, owner, repo, selectedTag.Name); err != nil {
		return err
	}
	rem.offerApplyAll(dep, selectedTag.Name)
	return nil
}

func (rem *Remediator) handleStale(wr WorkflowReport, finding Finding) error {
	dep := finding.Dependency
	rem.output.Warning("%s: %s", dep.Key(), finding.Detail)

	if !rem.prompter.IsInteractive() {
		rem.output.Hint("%s", finding.Remediation)
		rem.Skipped++
		return nil
	}

	if rem.opts.Write {
		rem.output.Skip("%s: stale upgrade requires confirmation", dep.Key())
		rem.Skipped++
		return nil
	}

	ok, err := rem.prompter.Confirm(
		fmt.Sprintf("Update %s to latest resolution?", dep.Key()), true)
	if err != nil {
		if errors.Is(err, ErrAborted) {
			return ErrAborted
		}
		rem.Skipped++
		return nil
	}
	if !ok {
		rem.Skipped++
		return nil
	}

	// Re-resolve and rewrite.
	return rem.applyReResolve(wr, dep)
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
	// so that TAMPERED signals are meaningful — patch tags should never move.
	rewrites := make(map[string]string)
	for i := range deps {
		dep := &deps[i]
		if !IsMutableVersionTag(dep.Ref) {
			continue
		}
		// Skip internal actions pinned to default branch.
		if rem.isSameOwner(strings.SplitN(dep.NWO, "/", 3)[0]) {
			continue
		}
		parts := strings.SplitN(dep.NWO, "/", 3)
		if len(parts) < 2 {
			continue
		}
		patchTag, err := rem.tagLister.BestPatchTagForSHA(parts[0], parts[1], dep.SHA)
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

// openBrowser attempts to open a URL in the user's browser.
func openBrowser(url string) {
	// Use the open command on macOS, xdg-open on Linux.
	// Best-effort — don't fail the doctor flow if it doesn't work.
	cmd := "open"
	if _, err := os.Stat("/usr/bin/xdg-open"); err == nil {
		cmd = "xdg-open"
	}
	// #nosec G204 — URL is constructed from known repo owner/name, not user input.
	proc := exec.Command(cmd, url)
	_ = proc.Start()
}

// reorderSuggestions puts full semver tags first (recommended for pinning),
// then major-only tags last. Within full semver: immutable releases first,
// then regular releases, then plain tags.
func reorderSuggestions(suggestions []TagSuggestion) []TagSuggestion {
	out := make([]TagSuggestion, 0, len(suggestions))
	// Immutable releases first.
	for _, s := range suggestions {
		if !s.Tag.IsMajor && s.Tag.IsImmutable {
			out = append(out, s)
		}
	}
	// Regular releases.
	for _, s := range suggestions {
		if !s.Tag.IsMajor && s.Tag.IsRelease && !s.Tag.IsImmutable {
			out = append(out, s)
		}
	}
	// Non-release full version tags.
	for _, s := range suggestions {
		if !s.Tag.IsMajor && !s.Tag.IsRelease {
			out = append(out, s)
		}
	}
	// Major-only tags last.
	for _, s := range suggestions {
		if s.Tag.IsMajor {
			out = append(out, s)
		}
	}
	return out
}
