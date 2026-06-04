package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/MakeNowJust/heredoc"
	"github.com/github/gh-actions-pin/internal/lockfile"
	"github.com/github/gh-actions-pin/internal/resolver"
	"github.com/github/gh-actions-pin/internal/runlog"
	"github.com/github/gh-actions-pin/internal/ui"
	"github.com/spf13/cobra"
)

// upgradeWorkers bounds concurrent per-file upgrade workers. Matches the pin
// worker pool size in internal/doctor so concurrency stays predictable across
// commands.
const upgradeWorkers = 8

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

	store, err := lockfile.OpenStore(".", r)
	if err != nil {
		return fmt.Errorf("opening lockfile: %w", err)
	}
	// Seed branch hints from the existing lockfile so repeat scans short-circuit
	// the per-branch Compare walk when the recorded branch still contains the SHA.
	r.SeedBranchHints(store.AllDeps())

	var hadError bool
	var allChanges []jsonUpgradeChange
	total := len(opts.WorkflowPaths)

	// Attach a run log so detailed narration goes to a file, leaving the
	// terminal to the spinner and a final summary. Skipped in --json mode.
	var logger *runlog.Logger
	parallel := opts.JSONFields == ""
	if parallel {
		if lg, lerr := runlog.Open(); lerr == nil {
			logger = lg
			f.UI.SetLog(logger)
			defer logger.Close()
		}
		label := fmt.Sprintf("Upgrading across %d %s", total, ui.Pluralize(total, "workflow", "workflows"))
		f.UI.StartProgress(label)
		// Workers own the detail area via SetWorkerStatus per slot; the
		// resolver's per-step progress would race with that, so silence it.
		r.ProgressFn = nil
		f.UI.ClearWorkerStatuses()
		defer f.UI.ClearWorkerStatuses()
	}

	workers := upgradeWorkers
	if workers > total {
		workers = total
	}
	if workers < 1 {
		workers = 1
	}

	type job struct {
		slot int
		path string
	}
	jobs := make(chan job, total)
	for _, p := range opts.WorkflowPaths {
		jobs <- job{path: p}
	}
	close(jobs)

	var (
		mu   sync.Mutex
		done atomic.Int64
		wg   sync.WaitGroup
	)
	updateLabel := func() {
		if !parallel {
			return
		}
		f.UI.UpdateLabel(fmt.Sprintf("[%d/%d] Upgrading workflows", done.Load(), total))
	}
	updateLabel()

	for slot := 0; slot < workers; slot++ {
		wg.Add(1)
		go func(slot int) {
			defer wg.Done()
			for j := range jobs {
				if parallel {
					f.UI.SetWorkerStatus(slot, "→ "+j.path)
				}
				changes, err := upgradeOneFile(f, opts, j.path, r, store, targets)
				mu.Lock()
				if err != nil {
					f.UI.Error("%s: %s", j.path, err)
					hadError = true
				}
				allChanges = append(allChanges, changes...)
				mu.Unlock()
				done.Add(1)
				updateLabel()
				if parallel {
					f.UI.SetWorkerStatus(slot, "")
				}
			}
		}(slot)
	}
	wg.Wait()

	if opts.Write {
		if err := store.Save(); err != nil {
			f.UI.StopProgress()
			return fmt.Errorf("saving lockfile: %w", err)
		}
	}

	if opts.JSONFields != "" {
		return writeUpgradeJSON(f.Out, allChanges)
	}

	// Work is done — stop the spinner before printing the terminal summary.
	f.UI.StopProgress()
	if logger != nil {
		defer func() { f.UI.TermDetail("Full log: %s", logger.Path()) }()
	}

	if hadError {
		f.UI.TermError("Upgrade failed — see the log for details")
		return errSilent
	}

	if len(allChanges) == 0 && len(opts.Actions) > 0 {
		f.UI.TermWarn("No matching actions found for: %s", strings.Join(opts.Actions, ", "))
		f.UI.TermDetail("Check the action name — use owner/repo format (e.g. docker/login-action, not docker/docker-login)")
		return errSilent
	}

	verb := "Upgraded"
	if !opts.Write {
		verb = "Previewed"
	}
	f.UI.TermSuccess("%s %d action(s)", verb, len(allChanges))
	for _, c := range allChanges {
		f.UI.TermDetail("%s: %s -> %s", c.NWO, c.OldRef, c.NewRef)
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

func upgradeOneFile(f *pinFactory, opts *upgradeOptions, workflowPath string, r *resolver.Resolver, store *lockfile.Store, targets []upgradeTarget) ([]jsonUpgradeChange, error) {
	wf, err := lockfile.Load(workflowPath)
	if err != nil {
		return nil, err
	}

	wfKey := lockfile.WorkflowKeyFromPath(workflowPath)
	existingDeps, err := store.Get(wfKey)
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

	deps, parentMap, err := r.ResolveAllRecursive(upgradedRefs)
	if err != nil {
		return nil, fmt.Errorf("resolving actions: %w", err)
	}
	directTracker := lockfile.NewDirectTracker(upgradedRefs, deps)

	if mismatches := lockfile.CheckSHARefMismatches(deps); len(mismatches) > 0 {
		f.UI.Error("action ref(s) look like commit SHAs but resolved to different OIDs:")
		for _, mismatch := range mismatches {
			f.UI.Detail("%s: ref %s resolved to %s", mismatch.Dep.NWO, mismatch.Dep.Ref, mismatch.ResolvedAs)
			f.UI.Hint("This ref may be a deceptive branch or tag name masquerading as a commit hash.")
		}
		return nil, fmt.Errorf("%d action ref(s) have SHA-like names that point to different commits", len(mismatches))
	}

	// Snapshot dep keys before PreserveRefs so we can rekey the parent map.
	preKeys := make([]string, len(deps))
	for i, d := range deps {
		preKeys[i] = d.Key()
	}

	deps = lockfile.PreserveRefs(existingDeps, deps)

	// Rekey parent map for any refs that PreserveRefs restored.
	parentRewrites := make(map[string]string)
	for i, d := range deps {
		if newKey := d.Key(); newKey != preKeys[i] {
			parentRewrites[preKeys[i]] = newKey
		}
	}
	parentMap = resolver.RekeyParentMap(parentMap, parentRewrites)

	// Discover containing tag/branch for every resolved commit and merge
	// any further rewrites (typically @sha → @tag) into updatedContent.
	// Fails closed when a commit has no containing branch.
	preNormKeys := make([]string, len(deps))
	for i, d := range deps {
		preNormKeys[i] = d.Key()
	}
	normRewrites, err := r.NormalizeContaining(deps)
	if err != nil {
		return nil, fmt.Errorf("normalizing containing refs: %w", err)
	}
	if len(normRewrites) > 0 {
		normedContent, _, err := upgradedWF.RewriteActionRefs(normRewrites)
		if err != nil {
			return nil, fmt.Errorf("rewriting refs to canonical tag/branch: %w", err)
		}
		updatedContent = normedContent
	}
	normParentRewrites := make(map[string]string)
	for i := range deps {
		if newKey := deps[i].Key(); newKey != preNormKeys[i] {
			normParentRewrites[preNormKeys[i]] = newKey
		}
	}
	parentMap = resolver.RekeyParentMap(parentMap, normParentRewrites)

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

	if err := os.WriteFile(workflowPath, updatedContent, 0o644); err != nil {
		return nil, fmt.Errorf("writing file: %w", err)
	}

	if err := store.Set(wfKey, deps, parentMap, directTracker.Keys(deps)); err != nil {
		return nil, fmt.Errorf("recording dependencies in lockfile: %w", err)
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
