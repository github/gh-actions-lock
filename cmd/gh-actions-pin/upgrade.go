package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/MakeNowJust/heredoc"
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

func newUpgradeCmd(f *pinFactory) *cobra.Command {
	opts := &upgradeOptions{}

	cmd := &cobra.Command{
		Use:   "upgrade [<workflow-path>...]",
		Args:  cobra.ArbitraryArgs,
		Short: "Upgrade action versions and re-lock dependencies",
		Long: heredoc.Doc(`
			Upgrade action refs to newer versions and re-lock all direct and
			transitive dependencies with their new commit SHAs.

			Pass --action to target specific actions. Each action is upgraded to
			its latest stable tag by default. Use --version to target a specific
			version, or specify it inline as owner/repo@ref. Use --from to limit
			upgrades to actions currently on a specific ref.

			Changes are applied by default. Pass --write=false to preview only.
		`),
		Example: heredoc.Doc(`
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
			if len(opts.Actions) == 0 {
				return fmt.Errorf("--action is required\n\n  gh actions-pin upgrade --action actions/checkout")
			}
			// Write by default unless --write=false was explicit.
			if !cmd.Flags().Changed("write") {
				opts.Write = true
			}
			return runUpgrade(f, opts)
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

func runUpgrade(f *pinFactory, opts *upgradeOptions) error {
	paths, err := discoverWorkflowPaths(opts.WorkflowPaths)
	if err != nil {
		return err
	}
	opts.WorkflowPaths = paths

	targets, err := parseUpgradeTargets(opts.Actions, opts.FromRef, opts.Version)
	if err != nil {
		return err
	}

	r, err := f.NewResolver(resolveHostname(opts.Hostname))
	if err != nil {
		return err
	}

	var hadError bool
	var allChanges []jsonUpgradeChange
	for _, workflowPath := range opts.WorkflowPaths {
		changes, err := upgradeOneFile(f, opts, workflowPath, r, targets)
		if err != nil {
			f.UI.Error("%s: %s", workflowPath, err)
			hadError = true
		}
		allChanges = append(allChanges, changes...)
	}

	if opts.JSONFields != "" {
		return writeUpgradeJSON(f.Out, allChanges)
	}

	if hadError {
		return errSilent
	}
	return nil
}

func writeUpgradeJSON(w io.Writer, changes []jsonUpgradeChange) error {
	result := jsonUpgradeResult{Updated: changes}
	if result.Updated == nil {
		result.Updated = []jsonUpgradeChange{}
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(result)
}

func upgradeOneFile(f *pinFactory, opts *upgradeOptions, workflowPath string, r *resolver.Resolver, targets []upgradeTarget) ([]jsonUpgradeChange, error) {
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
		f.UI.Warning("%s", warning)
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

	f.UI.Header("%s", workflowPath)
	for _, line := range planLines {
		f.UI.Detail("%s", line)
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
		f.UI.Warning("%s", warning)
	}

	f.UI.Info("Resolving %d action reference(s)...", len(upgradedRefs))
	for _, ref := range upgradedRefs {
		f.UI.Detail("%s@%s", ref.FullName(), ref.Ref)
	}

	deps, err := r.ResolveAllRecursive(upgradedRefs)
	if err != nil {
		return nil, fmt.Errorf("resolving actions: %w", err)
	}

	if mismatches := lockfile.CheckSHARefMismatches(deps); len(mismatches) > 0 {
		f.UI.Error("action ref(s) look like commit SHAs but resolved to different OIDs:")
		for _, mismatch := range mismatches {
			f.UI.Detail("%s: ref %s resolved to %s", mismatch.Dep.NWO, mismatch.Dep.Ref, mismatch.ResolvedAs)
			f.UI.Hint("This ref may be a deceptive branch or tag name masquerading as a commit hash.")
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
		showDiff(f.UI, r.Hostname(), existingDeps, deps)
	}

	if !opts.Write {
		f.UI.Info("Preview: %d change(s) for %s (pass --write to apply)", len(changes), workflowPath)
		return changes, nil
	}

	written, err := upgradedWF.WriteDependencies(deps)
	if err != nil {
		return nil, fmt.Errorf("writing dependencies: %w", err)
	}
	if err := os.WriteFile(workflowPath, written, 0o644); err != nil {
		return nil, fmt.Errorf("writing file: %w", err)
	}

	f.UI.Success("Upgraded %d action(s) in %s", len(changes), workflowPath)
	for _, c := range changes {
		f.UI.Detail("%s: %s → %s", c.NWO, c.OldRef, c.NewRef)
	}
	return changes, nil
}

func actionMatchesFilter(nwo, filter string) bool {
	if nwo == filter {
		return true
	}
	return strings.HasPrefix(nwo, filter+"/")
}

func showDiff(out *ui.UI, hostname string, old, new []lockfile.Dependency) {
	diff := lockfile.DiffDeps(old, new)

	for _, c := range diff.Changed {
		out.Infof("  %s %s\n", out.Yellow("~"), c.New.Key())
		out.Infof("    %s %s-%s\n", out.Red("-"), c.Old.HashAlgoOrDetect(), c.Old.SHA)
		out.Infof("    %s %s-%s\n", out.Green("+"), c.New.HashAlgoOrDetect(), c.New.SHA)
		out.Infof("    compare: https://%s/%s/compare/%s...%s\n", hostname, c.New.NWO, c.Old.SHA, c.New.SHA)
	}

	for _, c := range diff.Rekeyed {
		out.Infof("  %s %s\n", out.Yellow("~"), c.New.NWO)
		out.Infof("    %s %s\n", out.Red("-"), c.Old.String())
		out.Infof("    %s %s\n", out.Green("+"), c.New.String())
		if !strings.EqualFold(c.Old.SHA, c.New.SHA) {
			out.Infof("    compare: https://%s/%s/compare/%s...%s\n", hostname, c.New.NWO, c.Old.SHA, c.New.SHA)
		} else {
			out.Infof("    permalink: https://%s/%s/commit/%s\n", hostname, c.New.NWO, c.New.SHA)
		}
	}

	for _, dep := range diff.Added {
		out.Infof("  %s %s\n", out.Green("+"), dep.String())
		out.Infof("    permalink: https://%s/%s/commit/%s\n", hostname, dep.NWO, dep.SHA)
	}

	for _, dep := range diff.Removed {
		out.Infof("  %s %s\n", out.Red("-"), dep.String())
	}

	if n := len(diff.Unchanged); n > 0 {
		out.Infof("  %s\n", out.Dim(fmt.Sprintf("%d unchanged", n)))
	}
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
