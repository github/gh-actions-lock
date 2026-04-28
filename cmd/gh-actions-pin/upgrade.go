package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/MakeNowJust/heredoc"
	"github.com/cli/go-gh/v2/pkg/api"
	"github.com/cli/go-gh/v2/pkg/repository"
	"github.com/github/gh-actions-pin/internal/doctor"
	"github.com/github/gh-actions-pin/internal/lockfile"
	"github.com/github/gh-actions-pin/internal/resolver"
	"github.com/github/gh-actions-pin/internal/ui"
	"github.com/spf13/cobra"
)

type upgradeOptions struct {
	WorkflowPaths []string
	Actions       []string
	FromRef       string
	Version       string
	Write         bool
	Diff          bool
	Hostname      string
	JSONFields    string
	Prompter      doctor.Prompter
}

type upgradeTarget struct {
	Match      string
	CurrentRef string
	TargetRef  string
}

type jsonUpgradeChange struct {
	NWO    string   `json:"nwo"`
	OldRef string   `json:"old_ref"`
	NewRef string   `json:"new_ref"`
	OldSHA string   `json:"old_sha,omitempty"`
	NewSHA string   `json:"new_sha,omitempty"`
	Files  []string `json:"files"`
}

type jsonUpgradeResult struct {
	Updated []jsonUpgradeChange `json:"updated"`
}

func newUpgradeCmd() *cobra.Command {
	opts := &upgradeOptions{}

	cmd := &cobra.Command{
		Use:   "upgrade [<workflow-path>...]",
		Args:  cobra.ArbitraryArgs,
		Short: "Upgrade action versions and re-lock dependencies",
		Long: heredoc.Doc(`
			Upgrade action refs to newer versions and re-lock all direct and
			transitive dependencies with their new commit SHAs.

			With no flags, runs interactively: scans all workflows, shows which
			actions have newer versions available with links to release notes,
			and lets you pick which to upgrade.

			Pass --action to target specific actions non-interactively. Each
			action is upgraded to its latest stable tag by default. Use --version
			to target a specific version, or specify it inline as owner/repo@ref.
			Use --from to limit upgrades to actions currently on a specific ref.

			In non-interactive mode, changes are applied by default. Pass
			--write=false to preview only.
		`),
		Example: heredoc.Doc(`
			# Interactive: pick which actions to upgrade
			$ gh actions-pin upgrade

			# Upgrade checkout to the latest stable tag
			$ gh actions-pin upgrade --action actions/checkout

			# Upgrade to a specific version
			$ gh actions-pin upgrade --action actions/checkout --version v5

			# Upgrade only checkout refs currently on v5 to v6
			$ gh actions-pin upgrade --action actions/checkout --from v5 --version v6

			# Use inline target refs for mixed upgrades
			$ gh actions-pin upgrade --action actions/checkout@v6 --action actions/setup-go@v6

			# Preview without writing
			$ gh actions-pin upgrade --action actions/checkout --write=false
		`),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) > 0 {
				opts.WorkflowPaths = args
			}
			p := doctor.NewHuhPrompter()
			opts.Prompter = p
			if len(opts.Actions) == 0 {
				// Interactive mode — must be a TTY.
				if !p.IsInteractive() {
					return fmt.Errorf("--action is required in non-interactive mode\n\n  gh actions-pin upgrade --action actions/checkout")
				}
				return runUpgradeInteractive(opts)
			}
			// Non-interactive: write by default unless --write=false was explicit.
			if !cmd.Flags().Changed("write") {
				opts.Write = true
			}
			return runUpgrade(opts)
		},
	}

	cmd.Flags().StringArrayVar(&opts.Actions, "action", nil, "Upgrade the specified `action` (owner/repo or owner/repo/path). Append @ref to target a specific version.")
	cmd.Flags().StringVar(&opts.FromRef, "from", "", "Only upgrade actions currently on this `ref`")
	cmd.Flags().StringVar(&opts.Version, "version", "", "Target `version`/ref for all selected actions; defaults to the latest stable tag")
	cmd.Flags().BoolVar(&opts.Diff, "diff", true, "Show the full dependency diff vs existing pins")
	cmd.Flags().StringVar(&opts.Hostname, "hostname", "", "GitHub hostname to query (defaults to GH_HOST, current repo host, or github.com)")
	cmd.Flags().BoolVar(&opts.Write, "write", false, "Write the upgraded refs and dependencies back to the workflow file")
	cmd.Flags().StringVar(&opts.JSONFields, "json", "", "Output JSON with the specified `fields` (updated)")
	cmd.Flags().Lookup("json").NoOptDefVal = "updated"

	return cmd
}

func runUpgrade(opts *upgradeOptions) error {
	paths, err := discoverWorkflowPaths(opts.WorkflowPaths)
	if err != nil {
		return err
	}
	opts.WorkflowPaths = paths

	targets, err := parseUpgradeTargets(opts.Actions, opts.FromRef, opts.Version)
	if err != nil {
		return err
	}

	r, err := newResolver(resolveHostname(opts.Hostname))
	if err != nil {
		return err
	}

	var hadError bool
	var allChanges []jsonUpgradeChange
	for _, workflowPath := range opts.WorkflowPaths {
		changes, err := upgradeOneFile(opts, workflowPath, r, targets)
		if err != nil {
			output.Error("%s: %s", workflowPath, err)
			hadError = true
		}
		allChanges = append(allChanges, changes...)
	}

	if opts.JSONFields != "" {
		return writeUpgradeJSON(allChanges)
	}

	if hadError {
		return errSilent
	}
	return nil
}

func writeUpgradeJSON(changes []jsonUpgradeChange) error {
	result := jsonUpgradeResult{Updated: changes}
	if result.Updated == nil {
		result.Updated = []jsonUpgradeChange{}
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(result)
}

// upgradeCandidate represents an action that can be upgraded.
type upgradeCandidate struct {
	NWO         string   // e.g. "actions/checkout"
	CurrentRefs []string // deduplicated current refs across workflows (e.g. ["v5", "v4"])
	LatestRef   string   // latest available tag (or default branch for re-resolve)
	Files       []string // workflow files containing this action
	ReResolve   bool     // true if this is a default-branch re-resolve (no ref change, just repin SHA)
}

func runUpgradeInteractive(opts *upgradeOptions) error {
	paths, err := discoverWorkflowPaths(opts.WorkflowPaths)
	if err != nil {
		return err
	}

	// Detect current repo owner for same-owner (internal) action filtering.
	var repoOwner string
	if currentRepo, err := repository.Current(); err == nil {
		repoOwner = currentRepo.Owner
	}

	// TagLister for cooldown-aware, paginated tag resolution.
	hostname := resolveHostname(opts.Hostname)
	restClient, err := api.NewRESTClient(api.ClientOptions{Host: hostname})
	if err != nil {
		return err
	}
	tagLister := doctor.NewTagLister(restClient)

	// Phase 1: Scan all workflows and collect unique direct action NWOs.
	output.StartProgress(fmt.Sprintf("Scanning %d %s", len(paths), ui.Pluralize(len(paths), "workflow", "workflows")))

	type actionOccurrence struct {
		refs     map[string]bool
		files    map[string]bool
		internal bool // same-owner action
	}
	occurrences := make(map[string]*actionOccurrence) // keyed by NWO
	var nwoOrder []string
	// Collect current pinned SHAs from lockfile deps for re-resolve staleness check.
	// Key: "owner/repo@ref" → SHA
	pinnedSHAs := make(map[string]string)

	for _, workflowPath := range paths {
		wf, err := lockfile.Load(workflowPath)
		if err != nil {
			continue
		}
		refs, _, _ := wf.ExtractActionRefs()
		for _, ref := range refs {
			nwo := ref.NWO()
			isInternal := repoOwner != "" && strings.EqualFold(ref.Owner, repoOwner)
			occ, ok := occurrences[nwo]
			if !ok {
				occ = &actionOccurrence{
					refs:     make(map[string]bool),
					files:    make(map[string]bool),
					internal: isInternal,
				}
				occurrences[nwo] = occ
				nwoOrder = append(nwoOrder, nwo)
			}
			occ.refs[ref.Ref] = true
			occ.files[workflowPath] = true
		}

		// Read lockfile dependencies for pinned SHAs.
		deps, _ := wf.ReadDependencies()
		for _, dep := range deps {
			// Normalize dep NWO to owner/repo (strip sub-path like /save).
			depNWO := dep.NWO
			if parts := strings.SplitN(depNWO, "/", 3); len(parts) == 3 {
				depNWO = parts[0] + "/" + parts[1]
			}
			pinnedSHAs[depNWO+"@"+dep.Ref] = dep.SHA
		}
	}

	output.StopProgress()

	if len(occurrences) == 0 {
		output.Success("No action references found")
		return nil
	}

	// Phase 2: Resolve latest tags (cooldown-aware) and find upgradable actions.
	output.StartProgress(fmt.Sprintf("Checking latest versions for %d %s",
		len(occurrences), ui.Pluralize(len(occurrences), "action", "actions")))

	var candidates []upgradeCandidate
	for _, nwo := range nwoOrder {
		occ := occurrences[nwo]
		parts := strings.SplitN(nwo, "/", 2)
		if len(parts) != 2 {
			continue
		}

		currentRefs := make([]string, 0, len(occ.refs))
		for ref := range occ.refs {
			currentRefs = append(currentRefs, ref)
		}
		sort.Strings(currentRefs)

		files := make([]string, 0, len(occ.files))
		for f := range occ.files {
			files = append(files, f)
		}
		sort.Strings(files)

		if occ.internal {
			// Internal (same-owner) actions: offer to re-resolve default-branch
			// refs. Version-tagged internal actions get normal upgrade treatment.
			info, err := tagLister.GetRepoInfo(parts[0], parts[1])
			if err != nil {
				continue
			}

			hasDefaultBranch := false
			hasVersionRef := false
			for ref := range occ.refs {
				if ref == info.DefaultBranch {
					hasDefaultBranch = true
				}
				if doctor.LooksLikeVersion(ref) {
					hasVersionRef = true
				}
			}

			if hasDefaultBranch {
				// Only offer re-resolve if the branch HEAD has moved since last pin.
				currentSHA := pinnedSHAs[nwo+"@"+info.DefaultBranch]
				headSHA, err := tagLister.BranchHeadSHA(parts[0], parts[1], info.DefaultBranch)
				if err == nil && (currentSHA == "" || !strings.EqualFold(currentSHA, headSHA)) {
					candidates = append(candidates, upgradeCandidate{
						NWO:         nwo,
						CurrentRefs: currentRefs,
						LatestRef:   info.DefaultBranch,
						Files:       files,
						ReResolve:   true,
					})
				}
			}
			if hasVersionRef {
				// Version-tagged internal action — check for tag upgrade like external.
				latest, err := tagLister.LatestStableTag(parts[0], parts[1])
				if err != nil || latest == "" {
					continue
				}
				hasUpgrade := false
				for ref := range occ.refs {
					if doctor.LooksLikeVersion(ref) && doctor.IsUpgrade(ref, latest) {
						hasUpgrade = true
						break
					}
				}
				if hasUpgrade {
					var versionRefs []string
					for _, ref := range currentRefs {
						if doctor.LooksLikeVersion(ref) {
							versionRefs = append(versionRefs, ref)
						}
					}
					candidates = append(candidates, upgradeCandidate{
						NWO:         nwo,
						CurrentRefs: versionRefs,
						LatestRef:   latest,
						Files:       files,
					})
				}
			}
			continue
		}

		// External actions: resolve latest stable tag.
		latest, err := tagLister.LatestStableTag(parts[0], parts[1])
		if err != nil {
			output.StopProgress()
			output.Warning("%s: %s", nwo, err)
			output.StartProgress("Checking latest versions")
			continue
		}
		if latest == "" {
			continue // no suitable tags
		}

		// Filter noops via semver-aware comparison.
		hasUpgrade := false
		for ref := range occ.refs {
			if doctor.IsUpgrade(ref, latest) {
				hasUpgrade = true
				break
			}
		}
		if !hasUpgrade {
			continue
		}

		candidates = append(candidates, upgradeCandidate{
			NWO:         nwo,
			CurrentRefs: currentRefs,
			LatestRef:   latest,
			Files:       files,
		})
	}

	output.StopProgress()

	if len(candidates) == 0 {
		output.Success("All actions are already at their latest versions")
		return nil
	}

	// Phase 3: Present multi-select.
	menuOptions := make([]string, len(candidates))
	for i, c := range candidates {
		if c.ReResolve {
			menuOptions[i] = fmt.Sprintf("%s: re-resolve @%s (repin to latest commit)  (%d %s)",
				c.NWO, c.LatestRef,
				len(c.Files), ui.Pluralize(len(c.Files), "file", "files"))
		} else {
			current := strings.Join(c.CurrentRefs, ", ")
			menuOptions[i] = fmt.Sprintf("%s: %s → %s  (%d %s)",
				c.NWO, current, c.LatestRef,
				len(c.Files), ui.Pluralize(len(c.Files), "file", "files"))
		}
	}

	selected, err := opts.Prompter.MultiSelect("Select actions to upgrade", menuOptions)
	if err != nil {
		if errors.Is(err, doctor.ErrAborted) {
			return nil
		}
		return err
	}
	if len(selected) == 0 {
		output.Info("Nothing selected")
		return nil
	}

	// Phase 4: Show the plan with release links.
	fmt.Fprintln(os.Stderr)
	output.Header("Upgrade plan")
	for _, idx := range selected {
		c := candidates[idx]
		if c.ReResolve {
			output.Info("%s: re-resolve @%s to latest commit", c.NWO, c.LatestRef)
		} else {
			current := strings.Join(c.CurrentRefs, ", ")
			output.Info("%s: %s → %s", c.NWO, current, c.LatestRef)
			output.Detail("Release notes: https://github.com/%s/releases/tag/%s", c.NWO, c.LatestRef)
		}
		for _, f := range c.Files {
			output.Detail("in %s", f)
		}
	}
	fmt.Fprintln(os.Stderr)

	// Phase 5: Confirm.
	apply, err := opts.Prompter.Confirm("Apply these upgrades?", false)
	if err != nil {
		if errors.Is(err, doctor.ErrAborted) {
			return nil
		}
		return err
	}
	if !apply {
		output.Info("Upgrade cancelled")
		return nil
	}

	// Phase 6: Apply. Build --action list and delegate to the existing upgrade path.
	var actions []string
	for _, idx := range selected {
		c := candidates[idx]
		actions = append(actions, c.NWO+"@"+c.LatestRef)
	}

	applyOpts := &upgradeOptions{
		WorkflowPaths: paths,
		Actions:       actions,
		Write:         true,
		Diff:          false,
		Hostname:      opts.Hostname,
	}
	return runUpgrade(applyOpts)
}

func upgradeOneFile(opts *upgradeOptions, workflowPath string, r *resolver.Resolver, targets []upgradeTarget) ([]jsonUpgradeChange, error) {
	wf, err := lockfile.Load(workflowPath)
	if err != nil {
		return nil, err
	}

	existingDeps, err := wf.ReadDependencies()
	if err != nil {
		return nil, err
	}

	refs, _, warnings := wf.ExtractActionRefs()
	for _, warning := range warnings {
		output.Warning("%s", warning)
	}

	if len(refs) == 0 {
		return nil, nil
	}

	replacements := make(map[string]string)
	var matched []lockfile.ActionRef
	seenPlans := make(map[string]struct{})

	var planLines []string
	for _, ref := range refs {
		target, ok := matchingUpgradeTarget(ref, targets)
		if !ok {
			continue
		}

		targetRef := target.TargetRef
		if targetRef == "" {
			targetRef = opts.Version
		}
		if targetRef == "" {
			targetRef, err = r.LatestRef(ref.Owner, ref.Repo)
			if err != nil {
				return nil, err
			}
		}

		newUses := fmt.Sprintf("%s@%s", strings.Split(ref.Raw, "@")[0], targetRef)
		replacements[ref.Raw] = newUses
		matched = append(matched, ref)

		planKey := ref.FullName() + "@" + targetRef
		if _, seen := seenPlans[planKey]; seen {
			continue
		}
		seenPlans[planKey] = struct{}{}
		if ref.Ref == targetRef {
			planLines = append(planLines, fmt.Sprintf("%s already at %s", ref.FullName(), targetRef))
		} else {
			planLines = append(planLines, fmt.Sprintf("%s: %s -> %s", ref.FullName(), ref.Ref, targetRef))
		}
	}

	if len(matched) == 0 {
		return nil, nil
	}

	output.Header("%s", workflowPath)
	for _, line := range planLines {
		output.Detail("%s", line)
	}

	updatedContent, _, err := wf.RewriteActionRefs(replacements)
	if err != nil {
		return nil, err
	}
	upgradedWF, err := lockfile.Parse(workflowPath, updatedContent)
	if err != nil {
		return nil, err
	}

	upgradedRefs, _, upgradedWarnings := upgradedWF.ExtractActionRefs()
	for _, warning := range upgradedWarnings {
		output.Warning("%s", warning)
	}

	output.Info("Resolving %d action reference(s)...", len(upgradedRefs))
	for _, ref := range upgradedRefs {
		output.Detail("%s@%s", ref.FullName(), ref.Ref)
	}

	deps, err := r.ResolveAllRecursive(upgradedRefs)
	if err != nil {
		return nil, fmt.Errorf("resolving actions: %w", err)
	}

	if mismatches := lockfile.CheckSHARefMismatches(deps); len(mismatches) > 0 {
		output.Error("action ref(s) look like commit SHAs but resolved to different OIDs:")
		for _, mismatch := range mismatches {
			output.Detail("%s: ref %s resolved to %s", mismatch.Dep.NWO, mismatch.Dep.Ref, mismatch.ResolvedAs)
			output.Hint("This ref may be a deceptive branch or tag name masquerading as a commit hash.")
		}
		return nil, fmt.Errorf("%d action ref(s) have SHA-like names that point to different commits", len(mismatches))
	}

	deps = lockfile.PreserveRefs(existingDeps, deps)

	diff := lockfile.DiffDeps(existingDeps, deps)
	var changes []jsonUpgradeChange
	for _, c := range diff.Changed {
		changes = append(changes, jsonUpgradeChange{
			NWO: c.New.NWO, OldRef: c.Old.Ref, NewRef: c.New.Ref,
			OldSHA: c.Old.SHA, NewSHA: c.New.SHA, Files: []string{workflowPath},
		})
	}
	for _, c := range diff.Rekeyed {
		changes = append(changes, jsonUpgradeChange{
			NWO: c.New.NWO, OldRef: c.Old.Ref, NewRef: c.New.Ref,
			OldSHA: c.Old.SHA, NewSHA: c.New.SHA, Files: []string{workflowPath},
		})
	}

	if opts.Diff {
		showDiff(r.Hostname(), existingDeps, deps)
	}

	if !opts.Write {
		reviewHint := ""
		if !opts.Diff {
			reviewHint = buildCommandHint("gh actions-pin upgrade", workflowPath, opts.Actions, opts.FromRef, opts.Version, false)
		}
		applyHint := buildCommandHint("gh actions-pin upgrade", workflowPath, opts.Actions, opts.FromRef, opts.Version, true)
		output.Info("%s", previewMessage(workflowPath, upgradedRefs, existingDeps, deps, reviewHint, applyHint))
		return changes, nil
	}

	written, err := upgradedWF.WriteDependencies(deps)
	if err != nil {
		return nil, fmt.Errorf("writing dependencies: %w", err)
	}
	if err := os.WriteFile(workflowPath, written, 0o644); err != nil {
		return nil, fmt.Errorf("writing file: %w", err)
	}

	output.Success("Upgraded %d action(s) in %s", len(changes), workflowPath)
	for _, c := range changes {
		output.Detail("%s: %s → %s", c.NWO, c.OldRef, c.NewRef)
	}
	return changes, nil
}

func actionMatchesFilter(nwo, filter string) bool {
	if nwo == filter {
		return true
	}
	return strings.HasPrefix(nwo, filter+"/")
}

func showDiff(hostname string, old, new []lockfile.Dependency) {
	diff := lockfile.DiffDeps(old, new)

	for _, c := range diff.Changed {
		output.Infof("  %s %s\n", output.Yellow("~"), c.New.Key())
		output.Infof("    %s sha1-%s\n", output.Red("-"), c.Old.SHA)
		output.Infof("    %s sha1-%s\n", output.Green("+"), c.New.SHA)
		output.Infof("    compare: https://%s/%s/compare/%s...%s\n", hostname, c.New.NWO, c.Old.SHA, c.New.SHA)
	}

	for _, c := range diff.Rekeyed {
		output.Infof("  %s %s\n", output.Yellow("~"), c.New.NWO)
		output.Infof("    %s %s\n", output.Red("-"), c.Old.String())
		output.Infof("    %s %s\n", output.Green("+"), c.New.String())
		if !strings.EqualFold(c.Old.SHA, c.New.SHA) {
			output.Infof("    compare: https://%s/%s/compare/%s...%s\n", hostname, c.New.NWO, c.Old.SHA, c.New.SHA)
		} else {
			output.Infof("    permalink: https://%s/%s/commit/%s\n", hostname, c.New.NWO, c.New.SHA)
		}
	}

	for _, dep := range diff.Added {
		output.Infof("  %s %s\n", output.Green("+"), dep.String())
		output.Infof("    permalink: https://%s/%s/commit/%s\n", hostname, dep.NWO, dep.SHA)
	}

	for _, dep := range diff.Removed {
		output.Infof("  %s %s\n", output.Red("-"), dep.String())
	}

	if n := len(diff.Unchanged); n > 0 {
		output.Infof("  %s\n", output.Dim(fmt.Sprintf("%d unchanged", n)))
	}
}

type previewStats struct {
	directAdded       int
	directChanged     int
	directRemoved     int
	directUnchanged   int
	transitiveAdded   int
	transitiveChanged int
	transitiveRemoved int
	transitiveKept    int
}

func (s previewStats) hasChanges() bool {
	return s.directAdded+s.directChanged+s.directRemoved+s.transitiveAdded+s.transitiveChanged+s.transitiveRemoved > 0
}

func (s previewStats) unchanged() int {
	return s.directUnchanged + s.transitiveKept
}

func (s previewStats) hasTransitive() bool {
	return s.transitiveAdded+s.transitiveChanged+s.transitiveRemoved+s.transitiveKept > 0
}

func previewMessage(workflowPath string, refs []lockfile.ActionRef, old, new []lockfile.Dependency, reviewHint, applyHint string) string {
	stats := summarizePreview(refs, old, new)
	var lines []string

	if !stats.hasChanges() {
		lines = append(lines, fmt.Sprintf("Preview: no dependency changes for %s", workflowPath))
		if unchanged := stats.unchanged(); unchanged > 0 {
			lines = append(lines, fmt.Sprintf("  unchanged: %d", unchanged))
		}
		return strings.Join(lines, "\n")
	}

	lines = append(lines, fmt.Sprintf("Preview summary for %s", workflowPath))
	lines = append(lines, "  "+formatChangeSummary("direct", stats.directAdded, stats.directChanged, stats.directRemoved))
	if stats.hasTransitive() {
		lines = append(lines, "  "+formatChangeSummary("transitive", stats.transitiveAdded, stats.transitiveChanged, stats.transitiveRemoved))
	}
	if unchanged := stats.unchanged(); unchanged > 0 {
		lines = append(lines, fmt.Sprintf("  unchanged: %d", unchanged))
	}
	if len(old) > 0 && stats.directAdded+stats.directChanged+stats.directRemoved > 0 {
		lines = append(lines, "  Direct ref changes need `gh actions-pin upgrade --action <action>`.")
	}
	if reviewHint != "" {
		lines = append(lines, "  Review with: "+reviewHint)
	}
	if applyHint != "" {
		lines = append(lines, "  Apply with:  "+applyHint)
	}

	return strings.Join(lines, "\n")
}

func summarizePreview(refs []lockfile.ActionRef, old, new []lockfile.Dependency) previewStats {
	var stats previewStats

	directNames := make(map[string]struct{}, len(refs))
	for _, ref := range refs {
		directNames[ref.FullName()] = struct{}{}
	}

	isDirect := func(nwo string) bool {
		_, ok := directNames[nwo]
		return ok
	}

	diff := lockfile.DiffDeps(old, new)

	for _, c := range diff.Changed {
		if isDirect(c.New.NWO) {
			stats.directChanged++
		} else {
			stats.transitiveChanged++
		}
	}
	for _, c := range diff.Rekeyed {
		if isDirect(c.New.NWO) {
			stats.directChanged++
		} else {
			stats.transitiveChanged++
		}
	}
	for _, dep := range diff.Added {
		if isDirect(dep.NWO) {
			stats.directAdded++
		} else {
			stats.transitiveAdded++
		}
	}
	for _, dep := range diff.Removed {
		if isDirect(dep.NWO) {
			stats.directRemoved++
		} else {
			stats.transitiveRemoved++
		}
	}
	for _, dep := range diff.Unchanged {
		if isDirect(dep.NWO) {
			stats.directUnchanged++
		} else {
			stats.transitiveKept++
		}
	}

	return stats
}

func formatChangeSummary(label string, added, changed, removed int) string {
	parts := []string{}
	if added > 0 {
		parts = append(parts, fmt.Sprintf("%d added", added))
	}
	if changed > 0 {
		parts = append(parts, fmt.Sprintf("%d changed", changed))
	}
	if removed > 0 {
		parts = append(parts, fmt.Sprintf("%d removed", removed))
	}
	if len(parts) == 0 {
		return fmt.Sprintf("%s: no changes", label)
	}
	return fmt.Sprintf("%s: %s", label, strings.Join(parts, ", "))
}

func shellQuote(arg string) string {
	if strings.ContainsAny(arg, " \t\n\"'\\") {
		return fmt.Sprintf("%q", arg)
	}
	return arg
}

func buildCommandHint(base, workflowPath string, actions []string, fromRef, version string, write bool) string {
	parts := []string{base}
	for _, action := range actions {
		parts = append(parts, "--action", shellQuote(action))
	}
	if fromRef != "" {
		parts = append(parts, "--from", shellQuote(fromRef))
	}
	if version != "" {
		parts = append(parts, "--version", shellQuote(version))
	}
	if write {
		parts = append(parts, "--write")
	}
	parts = append(parts, shellQuote(workflowPath))
	return strings.Join(parts, " ")
}

func parseUpgradeTargets(actions []string, fromRef, version string) ([]upgradeTarget, error) {
	if len(actions) == 0 {
		return nil, fmt.Errorf("at least one --action is required for upgrade")
	}

	seen := make(map[string]upgradeTarget, len(actions))
	targets := make([]upgradeTarget, 0, len(actions))
	for _, action := range actions {
		action = strings.TrimSpace(action)
		action = strings.TrimPrefix(action, "github.com/")
		if action == "" {
			continue
		}

		match, targetRef, hasRef := strings.Cut(action, "@")
		if hasRef && targetRef == "" {
			return nil, fmt.Errorf("invalid --action %q: empty target ref", action)
		}
		if hasRef && version != "" {
			return nil, fmt.Errorf("can't combine --version with inline @ref in --action %q", action)
		}
		if match == "" {
			return nil, fmt.Errorf("invalid --action %q", action)
		}
		if targetRef == "" && version != "" {
			targetRef = version
		}

		target := upgradeTarget{
			Match:      match,
			CurrentRef: fromRef,
			TargetRef:  targetRef,
		}

		if prior, ok := seen[match]; ok {
			if prior.CurrentRef != target.CurrentRef || prior.TargetRef != target.TargetRef {
				return nil, fmt.Errorf("conflicting upgrade targets for %s", match)
			}
			continue
		}

		seen[match] = target
		targets = append(targets, target)
	}

	if len(targets) == 0 {
		return nil, fmt.Errorf("at least one --action is required for upgrade")
	}

	return targets, nil
}

func matchingUpgradeTarget(ref lockfile.ActionRef, targets []upgradeTarget) (upgradeTarget, bool) {
	var best upgradeTarget
	bestLen := -1
	for _, target := range targets {
		if !actionMatchesFilter(ref.FullName(), target.Match) {
			continue
		}
		if target.CurrentRef != "" && ref.Ref != target.CurrentRef {
			continue
		}
		if len(target.Match) > bestLen {
			best = target
			bestLen = len(target.Match)
		}
	}
	if bestLen >= 0 {
		return best, true
	}
	return upgradeTarget{}, false
}
