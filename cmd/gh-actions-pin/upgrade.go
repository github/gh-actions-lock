package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/MakeNowJust/heredoc"
	"github.com/github/gh-actions-pin/internal/doctor"
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
	NoOnboard     bool
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

// jsonUpgradeFinding mirrors format.Finding for the upgrade command,
// which carries its own minimal finding shape rather than pulling in the
// doctor.Report apparatus — upgrade only emits structured findings for
// refusal/blocker cases, not dependency-state inspection.
type jsonUpgradeFinding struct {
	Workflow    string `json:"workflow"`
	Category    string `json:"category"`
	Severity    string `json:"severity"`
	Confidence  string `json:"confidence,omitempty"`
	Detail      string `json:"detail"`
	Remediation string `json:"remediation,omitempty"`
	DocURL      string `json:"doc_url,omitempty"`
}

// jsonUpgradeWorkflow records that an upgrade ran successfully against a
// workflow file. Refused workflows do NOT appear here — they appear only
// in findings[]. Consumers (Dependabot) read workflows[] to know which
// files to commit.
type jsonUpgradeWorkflow struct {
	Path string `json:"path"`
}

type jsonUpgradeResult struct {
	Updated   []jsonUpgradeChange   `json:"updated"`
	Findings  []jsonUpgradeFinding  `json:"findings,omitempty"`
	Workflows []jsonUpgradeWorkflow `json:"workflows,omitempty"`
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
		PreRunE: func(cmd *cobra.Command, args []string) error {
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
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			return runUpgrade(cmd.Context(), f, opts)
		},
	}

	cmd.Flags().StringArrayVar(&opts.Actions, "action", nil, "Upgrade the specified `action` (owner/repo or owner/repo/path). Append @ref to target a specific version.")
	cmd.Flags().StringVar(&opts.FromRef, "from", "", "Only upgrade actions currently on this `ref`")
	cmd.Flags().StringVar(&opts.Version, "version", "", "Target `version`/ref for all selected actions; defaults to the latest stable tag")
	cmd.Flags().BoolVar(&opts.Diff, "diff", true, "Show the full dependency diff vs existing pins")
	cmd.Flags().StringVar(&opts.Hostname, "hostname", "", "GitHub hostname to query (defaults to GH_HOST, current repo host, or github.com)")
	cmd.Flags().BoolVar(&opts.Write, "write", false, "Write the upgraded refs and dependencies back to the workflow file")
	cmd.Flags().StringVar(&opts.JSONFields, "json", "", "Output JSON with the specified diff `fields` (updated). findings[] and workflows[] are always emitted alongside.")
	cmd.Flags().Lookup("json").NoOptDefVal = "updated"
	cmd.Flags().BoolVar(&opts.NoOnboard, "no-onboard", false, "Refuse to add new workflow entries to the lockfile. Workflows not already tracked in lockfile.workflows{} are skipped with an onboarding-required finding. Required for dependency-update tools (e.g. Dependabot) that must not silently expand the tracked workflow set.")
	// Accepted for symmetry with `check`; upgrade is non-interactive already, so this is a no-op.
	var noInteractive bool
	cmd.Flags().BoolVar(&noInteractive, "no-interactive", false, "Auto-fix deterministic issues; fail on issues requiring human input")

	return cmd
}

func runUpgrade(ctx context.Context, f *pinFactory, opts *upgradeOptions) error {
	if ctx == nil {
		ctx = context.Background()
	}
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

	store, err := lockfile.OpenStore(".", ctxMetadataResolver{r: r, ctx: ctx})
	if err != nil {
		return fmt.Errorf("opening lockfile: %w", err)
	}
	// Seed branch hints from the existing lockfile so repeat scans short-circuit
	// the per-branch Compare walk when the recorded branch still contains the SHA.
	r.SeedBranchHints(store.AllDeps())

	var hadError bool
	var allChanges []jsonUpgradeChange
	var findings []jsonUpgradeFinding
	var savedWorkflows []jsonUpgradeWorkflow
	// anyMatched tracks whether ANY workflow contained a ref matching the
	// --action filter (independent of whether resolution produced a non-empty
	// diff). Onboarding a previously-untracked workflow produces a
	// purely-Added diff that contributes 0 entries to allChanges but is
	// nonetheless a successful upgrade — without this signal the "No
	// matching actions found" branch would fire and flip exit code to
	// non-zero. See cmd/gh-actions-pin/upgrade_no_onboard_test.go for the
	// regression guard.
	var anyMatched bool
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
				result, err := upgradeOneFile(ctx, f, opts, j.path, r, store, targets)
				mu.Lock()
				if err != nil {
					f.UI.Error("%s: %s", j.path, err)
					hadError = true
				}
				if result != nil {
					allChanges = append(allChanges, result.Changes...)
					if result.Matched {
						anyMatched = true
					}
					if result.Finding != nil {
						findings = append(findings, *result.Finding)
					}
					if result.Saved {
						savedWorkflows = append(savedWorkflows, jsonUpgradeWorkflow{Path: j.path})
					}
				}
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

	// store.Save() is atomic (tmp+rename), so a partial-success save still
	// leaves the on-disk lockfile consistent. When every workflow was
	// refused or errored, skip Save entirely so the bytes stay untouched.
	if opts.Write && len(savedWorkflows) > 0 {
		if err := store.Save(); err != nil {
			f.UI.StopProgress()
			return fmt.Errorf("saving lockfile: %w", err)
		}
	}

	// A blocking finding (onboarding-required, severity:error) must drive
	// the process exit code non-zero even when no other error occurred.
	hadBlockingFinding := false
	for _, fnd := range findings {
		if fnd.Severity == "error" {
			hadBlockingFinding = true
			break
		}
	}

	if opts.JSONFields != "" {
		if err := writeUpgradeJSON(f.Out, allChanges, findings, savedWorkflows); err != nil {
			return err
		}
		if hadBlockingFinding || hadError {
			return errSilent
		}
		return nil
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

	if len(allChanges) == 0 && len(opts.Actions) > 0 && len(findings) == 0 && !anyMatched {
		f.UI.TermWarn("No matching actions found for: %s", strings.Join(opts.Actions, ", "))
		f.UI.TermDetail("Check the action name — use owner/repo format (e.g. docker/login-action, not docker/docker-login)")
		return errSilent
	}

	// When the matched action(s) only triggered onboarding (purely-Added
	// diff entries that aren't surfaced in allChanges to preserve the
	// updated[] JSON shape), say so explicitly rather than printing the
	// misleading "Upgraded 0 action(s)".
	if len(allChanges) == 0 && anyMatched && len(savedWorkflows) > 0 {
		f.UI.TermSuccess("Onboarded %d workflow(s); no action refs needed upgrading", len(savedWorkflows))
		for _, w := range savedWorkflows {
			f.UI.TermDetail("%s", w.Path)
		}
		for _, fnd := range findings {
			f.UI.TermWarn("%s: %s", fnd.Workflow, fnd.Detail)
			if fnd.Remediation != "" {
				f.UI.TermDetail("%s", fnd.Remediation)
			}
		}
		if hadBlockingFinding {
			return errSilent
		}
		return nil
	}

	verb := "Upgraded"
	if !opts.Write {
		verb = "Previewed"
	}
	f.UI.TermSuccess("%s %d action(s)", verb, len(allChanges))
	for _, c := range allChanges {
		f.UI.TermDetail("%s: %s -> %s", c.NWO, c.OldRef, c.NewRef)
	}
	for _, fnd := range findings {
		f.UI.TermWarn("%s: %s", fnd.Workflow, fnd.Detail)
		if fnd.Remediation != "" {
			f.UI.TermDetail("%s", fnd.Remediation)
		}
	}
	if hadBlockingFinding {
		return errSilent
	}
	return nil
}

func writeUpgradeJSON(w io.Writer, changes []jsonUpgradeChange, findings []jsonUpgradeFinding, workflows []jsonUpgradeWorkflow) error {
	result := jsonUpgradeResult{
		Updated:   changes,
		Findings:  findings,
		Workflows: workflows,
	}
	if result.Updated == nil {
		result.Updated = []jsonUpgradeChange{}
	}
	if result.Findings == nil {
		result.Findings = []jsonUpgradeFinding{}
	}
	if result.Workflows == nil {
		result.Workflows = []jsonUpgradeWorkflow{}
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(result)
}

// upgradeFileResult is the per-workflow return shape from upgradeOneFile.
// Saved=true means the workflow was upgraded and written through to the
// in-memory lockfile store (caller decides whether to persist). Matched=true
// means at least one ref in the workflow matched the --action filter,
// regardless of whether the resulting diff produced any Changes entries
// (onboarding a previously-untracked workflow yields a purely-Added diff,
// which contributes 0 entries to Changes but is nonetheless a successful
// match — the caller uses Matched to gate the "No matching actions found"
// exit-code branch). Finding is populated when the workflow was refused
// (e.g. --no-onboard on an untracked workflow) or when any other
// per-workflow blocker fires; the caller propagates it into the run's
// findings[] payload.
type upgradeFileResult struct {
	Changes []jsonUpgradeChange
	Saved   bool
	Matched bool
	Finding *jsonUpgradeFinding
}

func upgradeOneFile(ctx context.Context, f *pinFactory, opts *upgradeOptions, workflowPath string, r *resolver.Resolver, store *lockfile.Store, targets []upgradeTarget) (*upgradeFileResult, error) {
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
			targetRef, err = r.LatestRef(ctx, ref.Owner, ref.Repo)
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

	// --no-onboard refuses workflows that are not already in
	// lockfile.workflows{}. The check fires after plan construction but
	// before resolver work or file writes, so store.Set is never reached
	// for an untracked workflow.
	if opts.NoOnboard && !store.HasWorkflow(wfKey) {
		f.UI.Warning("%s: refusing to onboard (--no-onboard); workflow has no entry in actions.lock", workflowPath)
		finding := &jsonUpgradeFinding{
			Workflow:    workflowPath,
			Category:    string(doctor.CategoryOnboardingRequired),
			Severity:    string(doctor.SeverityError),
			Confidence:  string(doctor.ConfidenceHigh),
			Detail:      fmt.Sprintf("workflow %s is not tracked in the lockfile; --no-onboard refuses to add new workflow entries during this run", workflowPath),
			Remediation: "Run `gh actions-pin` (without --no-onboard) on this repository to onboard the workflow into the lockfile, then re-run the upgrade.",
			DocURL:      doctor.DocURLFor(doctor.CategoryOnboardingRequired),
		}
		return &upgradeFileResult{Matched: true, Finding: finding}, nil
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

	deps, parentMap, err := r.ResolveAllRecursive(ctx, upgradedRefs)
	if err != nil {
		return nil, fmt.Errorf("resolving actions: %w", err)
	}
	directTracker := lockfile.NewDirectTracker(upgradedRefs, deps)

	if mismatches := lockfile.CheckSHARefMismatches(deps, ctxTagPeeler{r: r, ctx: ctx}); len(mismatches) > 0 {
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
	normRewrites, err := r.NormalizeContaining(ctx, deps)
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
		return &upgradeFileResult{Changes: changes, Matched: true}, nil
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
	return &upgradeFileResult{Changes: changes, Saved: true, Matched: true}, nil
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
