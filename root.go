package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/MakeNowJust/heredoc"
	"github.com/cli/go-gh/v2/pkg/repository"
	"github.com/github/gh-actions-pin/internal/lockfile"
	"github.com/github/gh-actions-pin/internal/resolver"
	"github.com/github/gh-actions-pin/internal/ui"
	"github.com/spf13/cobra"
)

var errSilent = errors.New("silent error")
var errNoDeps = errors.New("no dependencies: section found")
var errNoActions = errors.New("no action references found")
var newResolver = resolver.New
var output = ui.New()

type pinOptions struct {
	WorkflowPaths   []string
	Actions         []string
	AllowRefChanges bool
	Write           bool
	Diff            bool
	Hostname        string
	CommandPath     string
}

type checkOptions struct {
	WorkflowPaths []string
	JSONFields    string
	Hostname      string
}

type upgradeOptions struct {
	WorkflowPaths []string
	Actions       []string
	FromRef       string
	Version       string
	Write         bool
	Diff          bool
	Hostname      string
}

type upgradeTarget struct {
	Match      string
	CurrentRef string
	TargetRef  string
}

type validationError struct {
	Type       string `json:"type"`
	Dependency string `json:"dependency"`
	Details    string `json:"details"`
}

type validationResult struct {
	Valid    bool              `json:"valid"`
	Errors   []validationError `json:"errors"`
	Warnings []string          `json:"warnings"`
}

func newRootCmd() *cobra.Command {
	opts := &pinOptions{CommandPath: "gh actions-pin"}

	cmd := &cobra.Command{
		Use:           "actions-pin [<workflow-path>...]",
		Args:          cobra.ArbitraryArgs,
		Short:         "Preview and manage GitHub Actions workflow dependencies",
		SilenceErrors: true,
		SilenceUsage:  true,
		Long: heredoc.Doc(`
			Resolve workflow action references to their current commit SHAs and preview
			the resulting lockfile changes.

			With no arguments, the extension discovers all workflows under
			.github/workflows/ and processes them in one pass. Pass --write to apply
			the changes and write a deterministic dependencies: section back into each
			workflow file.

			If the workflow's direct action refs have changed since the last lock,
			the write path refuses to bless those edits by default. Use
			gh actions-pin upgrade for intentional version bumps, or pass
			--allow-ref-changes to explicitly acknowledge the drift.

			Local path actions (uses: ./path) are currently skipped.

			Use the subcommands to manage your workflow's dependencies:

			  gh actions-pin           Preview lockfile changes without writing
			  gh actions-pin --write   Apply the lockfile changes to disk
			  gh actions-pin check     Verify the lock section against live resolution
			  gh actions-pin update    Refresh selected pinned dependencies
			  gh actions-pin upgrade   Bump workflow refs and repin them
		`),
		Example: heredoc.Doc(`
			# Preview changes for all workflows in .github/workflows/
			$ gh actions-pin

			# Write the resolved lockfile changes to disk
			$ gh actions-pin --write

			# Intentionally bless direct workflow ref edits
			$ gh actions-pin --write --allow-ref-changes

			# Preview a specific workflow without the dependency diff
			$ gh actions-pin .github/workflows/ci.yml --diff=false

			# Check the current lock state
			$ gh actions-pin check
		`),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) > 0 {
				opts.WorkflowPaths = args
			}
			return runPin(opts)
		},
	}

	addPinFlags(cmd, opts, true)
	cmd.Flags().BoolVar(&opts.AllowRefChanges, "allow-ref-changes", false, "Allow writing after direct workflow refs have changed since the last lock")
	cmd.AddCommand(newCheckCmd())
	cmd.AddCommand(newUpdateCmd())
	cmd.AddCommand(newUpgradeCmd())

	return cmd
}

func newUpdateCmd() *cobra.Command {
	opts := &pinOptions{AllowRefChanges: true, CommandPath: "gh actions-pin update"}

	cmd := &cobra.Command{
		Use:   "update [<workflow-path>...]",
		Args:  cobra.ArbitraryArgs,
		Short: "Refresh pinned workflow dependencies",
		Long: heredoc.Doc(`
			Refresh the pinned dependency SHAs for workflow files.

			Use --action one or more times to update selected actions without
			re-resolving everything else. Existing pinned dependencies are preserved;
			only the targeted actions are refreshed.

			By default, update previews changes with a diff. Pass --write to
			apply the updated lockfile entries to disk.
		`),
		Example: heredoc.Doc(`
			# Preview refreshed pinned dependencies
			$ gh actions-pin update

			# Apply the refreshed pins to disk
			$ gh actions-pin update --write

			# Refresh a single action across all workflows
			$ gh actions-pin update --action actions/checkout --write

			# Refresh multiple actions in one batch
			$ gh actions-pin update --action actions/checkout --action actions/setup-go --write
		`),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) > 0 {
				opts.WorkflowPaths = args
			}
			return runPin(opts)
		},
	}

	addPinFlags(cmd, opts, true)
	return cmd
}

func newUpgradeCmd() *cobra.Command {
	opts := &upgradeOptions{}

	cmd := &cobra.Command{
		Use:   "upgrade [<workflow-path>...]",
		Args:  cobra.ArbitraryArgs,
		Short: "Upgrade workflow action refs and repin dependencies",
		Long: heredoc.Doc(`
			Upgrade selected workflow actions to a newer ref and then recompute the
			inline dependencies: lock section.

			Pass --action to target one or more actions. By default, each selected
			action is upgraded to its latest stable tag. Use --version to force a
			specific target ref for all selected actions, or specify it inline as
			owner/repo@ref. Use --from to limit upgrades to actions currently on a
			specific ref.

			This command previews the resulting change by default. Pass --write to
			apply the workflow ref edits and the updated lockfile entries.
		`),
		Example: heredoc.Doc(`
			# Preview upgrading checkout to the latest stable tag
			$ gh actions-pin upgrade --action actions/checkout

			# Apply that upgrade to disk
			$ gh actions-pin upgrade --action actions/checkout --write

			# Upgrade to a specific version
			$ gh actions-pin upgrade --action actions/checkout --version v5 --write

			# Upgrade only checkout refs currently on v5 to v6
			$ gh actions-pin upgrade --action actions/checkout --from v5 --version v6 --write

			# Use inline target refs for mixed upgrades
			$ gh actions-pin upgrade --action actions/checkout@v6 --action actions/setup-go@v6 --write
		`),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) > 0 {
				opts.WorkflowPaths = args
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

	return cmd
}

func newCheckCmd() *cobra.Command {
	opts := &checkOptions{}

	cmd := &cobra.Command{
		Use:     "check [<workflow-path>...]",
		Args:    cobra.ArbitraryArgs,
		Aliases: []string{"verify"},
		Short:   "Verify pinned workflow dependencies",
		Long: heredoc.Doc(`
			Re-resolve all action dependencies in workflow files and compare them
			against the pinned SHAs in the dependencies: section.

			With no arguments, the extension discovers and validates all workflows
			under .github/workflows/.
			Local path actions (uses: ./path) are currently skipped.

			Detects:
			  TAMPERED      - SHA does not match live resolution
			  MISSING       - uses: ref has no dependencies: entry
			  STALE         - dependencies: entry is no longer discoverable
			  SHA_MISMATCH  - uses: ref looks like a SHA but resolves elsewhere
			  UNREACHABLE   - SHA is not on the ref's lineage (possible fork-network injection)
		`),
		Example: heredoc.Doc(`
			# Verify all workflows
			$ gh actions-pin check

			# Verify a specific workflow
			$ gh actions-pin check .github/workflows/ci.yml

			# Output JSON for CI
			$ gh actions-pin check --json valid,errors
		`),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) > 0 {
				opts.WorkflowPaths = args
			}
			return runCheck(opts)
		},
	}

	cmd.Flags().StringVar(&opts.JSONFields, "json", "", "Output JSON with the specified `fields` (valid,errors,warnings)")
	cmd.Flags().StringVar(&opts.Hostname, "hostname", "", "GitHub hostname to query (defaults to GH_HOST, current repo host, or github.com)")
	return cmd
}

func addPinFlags(cmd *cobra.Command, opts *pinOptions, includeWrite bool) {
	cmd.Flags().StringArrayVar(&opts.Actions, "action", nil, "Re-resolve only the specified `action` (owner/repo or owner/repo/path). Repeat to batch updates")
	cmd.Flags().BoolVar(&opts.Diff, "diff", true, "Show the full dependency diff vs existing pins")
	cmd.Flags().StringVar(&opts.Hostname, "hostname", "", "GitHub hostname to query (defaults to GH_HOST, current repo host, or github.com)")
	if includeWrite {
		cmd.Flags().BoolVar(&opts.Write, "write", false, "Write the resolved dependencies back to the workflow file")
	}
}

func runPin(opts *pinOptions) error {
	paths, err := discoverWorkflowPaths(opts.WorkflowPaths)
	if err != nil {
		return err
	}
	opts.WorkflowPaths = paths
	opts.Actions = normalizeActionFilters(opts.Actions)

	r, err := newResolver(resolveHostname(opts.Hostname))
	if err != nil {
		return err
	}

	var hadError bool
	for _, workflowPath := range opts.WorkflowPaths {
		if len(opts.WorkflowPaths) > 1 {
			output.Header("%s", workflowPath)
		}
		if err := pinOneFile(opts, workflowPath, r); err != nil {
			output.Error("%s: %s", workflowPath, err)
			hadError = true
		}
	}

	if hadError {
		return errSilent
	}
	return nil
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
	for _, workflowPath := range opts.WorkflowPaths {
		if len(opts.WorkflowPaths) > 1 {
			output.Header("%s", workflowPath)
		}
		if err := upgradeOneFile(opts, workflowPath, r, targets); err != nil {
			output.Error("%s: %s", workflowPath, err)
			hadError = true
		}
	}

	if hadError {
		return errSilent
	}
	return nil
}

func runCheck(opts *checkOptions) error {
	paths, err := discoverWorkflowPaths(opts.WorkflowPaths)
	if err != nil {
		return err
	}
	opts.WorkflowPaths = paths

	r, err := newResolver(resolveHostname(opts.Hostname))
	if err != nil {
		return err
	}

	aggregate := &validationResult{Valid: true}
	for _, workflowPath := range opts.WorkflowPaths {
		if len(opts.WorkflowPaths) > 1 && opts.JSONFields == "" {
			output.Header("%s", workflowPath)
		}
		result, err := validateOneFile(workflowPath, r)
		if err != nil {
			if errors.Is(err, errNoActions) {
				// Workflow has only run: steps, no actions to pin — skip silently.
				continue
			}
			if errors.Is(err, errNoDeps) {
				if opts.JSONFields != "" {
					aggregate.Warnings = append(aggregate.Warnings,
						fmt.Sprintf("%s: not yet pinned (run `gh actions-pin --write` first)", workflowPath))
				} else {
					output.Skip("%s: not yet pinned (run `gh actions-pin --write` first)", workflowPath)
				}
				continue
			}
			aggregate.Valid = false
			aggregate.Errors = append(aggregate.Errors, validationError{
				Type:       "ERROR",
				Dependency: workflowPath,
				Details:    err.Error(),
			})
			if opts.JSONFields == "" {
				output.Error("%s: %s", workflowPath, err)
			}
			continue
		}
		if !result.Valid {
			aggregate.Valid = false
		}
		aggregate.Errors = append(aggregate.Errors, result.Errors...)
		aggregate.Warnings = append(aggregate.Warnings, result.Warnings...)
	}

	if opts.JSONFields != "" {
		return writeValidationJSON(aggregate, opts.JSONFields)
	}

	if aggregate.Valid {
		if len(opts.WorkflowPaths) > 1 {
			output.Success("All %d workflow(s) valid", len(opts.WorkflowPaths))
		}
		return nil
	}

	// Print errors and warnings in human-readable mode.
	// Skip ERROR entries — those were already printed inline when validateOneFile returned an error.
	for _, e := range aggregate.Errors {
		if e.Type == "ERROR" {
			continue
		}
		output.Error("[%s] %s: %s", e.Type, e.Dependency, e.Details)
	}
	for _, w := range aggregate.Warnings {
		output.Warning("%s", w)
	}

	return errSilent
}

func discoverWorkflowPaths(existing []string) ([]string, error) {
	if len(existing) > 0 {
		return existing, nil
	}

	paths, err := lockfile.DiscoverWorkflows()
	if err != nil {
		return nil, err
	}
	if len(paths) == 0 {
		return nil, fmt.Errorf("no workflow files found in .github/workflows/")
	}
	return paths, nil
}

func normalizeActionFilters(filters []string) []string {
	if len(filters) == 0 {
		return nil
	}

	out := make([]string, 0, len(filters))
	seen := map[string]struct{}{}
	for _, filter := range filters {
		filter = strings.TrimSpace(filter)
		filter = strings.TrimPrefix(filter, "github.com/")
		filter = strings.TrimSuffix(filter, "@")
		if filter == "" {
			continue
		}
		if _, ok := seen[filter]; ok {
			continue
		}
		seen[filter] = struct{}{}
		out = append(out, filter)
	}
	return out
}

func resolveHostname(override string) string {
	if override != "" {
		return override
	}
	if host := os.Getenv("GH_HOST"); host != "" {
		return host
	}
	repo, err := repository.Current()
	if err == nil && repo.Host != "" {
		return repo.Host
	}
	return "github.com"
}

func pinOneFile(opts *pinOptions, workflowPath string, r *resolver.Resolver) error {
	wf, err := lockfile.Load(workflowPath)
	if err != nil {
		return err
	}

	existingDeps, err := wf.ReadDependencies()
	if err != nil {
		return err
	}

	refs, _, warnings := wf.ExtractActionRefs()
	for _, warning := range warnings {
		output.Warning("%s", warning)
	}

	if len(refs) == 0 {
		output.Skip("No repository action references found in %s", workflowPath)
		return nil
	}

	if len(opts.Actions) > 0 {
		var filtered []lockfile.ActionRef
		for _, ref := range refs {
			if actionMatchesAnyFilter(ref.FullName(), opts.Actions) {
				filtered = append(filtered, ref)
			}
		}
		if len(filtered) == 0 {
			output.Skip("No references to %s found in %s", strings.Join(opts.Actions, ", "), workflowPath)
			return nil
		}
		refs = filtered
	}

	output.Info("Resolving %d action reference(s)...", len(refs))
	for _, ref := range refs {
		output.Detail("%s@%s", ref.FullName(), ref.Ref)
	}

	deps, err := r.ResolveAllRecursive(refs)
	if err != nil {
		return fmt.Errorf("resolving actions: %w", err)
	}

	if mismatches := lockfile.CheckSHARefMismatches(deps); len(mismatches) > 0 {
		output.Error("action ref(s) look like commit SHAs but resolved to different OIDs:")
		for _, mismatch := range mismatches {
			output.Detail("%s: ref %s resolved to %s", mismatch.Dep.NWO, mismatch.Dep.Ref, mismatch.ResolvedAs)
			output.Hint("This ref may be a deceptive branch or tag name masquerading as a commit hash.")
		}
		return fmt.Errorf("%d action ref(s) have SHA-like names that point to different commits", len(mismatches))
	}

	// Reachability check on freshly resolved deps (warns, does not block)
	reachResults := r.CheckReachabilityAll(deps)
	for _, rr := range reachResults {
		depID := rr.DepKey
		if depID == "" {
			depID = fmt.Sprintf("%s/%s@%s", rr.Owner, rr.Repo, rr.Ref)
		}
		switch rr.Status {
		case resolver.Unreachable:
			output.Warning("%s: SHA %s is NOT reachable from ref (%s)", depID, rr.SHA[:12], rr.Detail)
			output.Hint("This may indicate a fork-network injection attack.")
		case resolver.ReachabilityUnknown:
			output.Warning("%s: reachability check inconclusive (%s)", depID, rr.Detail)
		}
	}

	if len(opts.Actions) > 0 && len(existingDeps) > 0 {
		deps = mergeTargetedDeps(existingDeps, deps, opts.Actions)
	} else if len(existingDeps) > 0 && opts.Write {
		if err := checkConsistency(existingDeps, deps, r.Hostname()); err != nil {
			return err
		}
	}

	if len(existingDeps) > 0 && opts.Write {
		if err := checkDirectRefChanges(refs, existingDeps, opts.AllowRefChanges); err != nil {
			return err
		}
	}

	if opts.Diff {
		showDiff(r.Hostname(), existingDeps, deps)
	}

	if !opts.Write {
		reviewHint := ""
		if !opts.Diff {
			reviewHint = buildCommandHint(opts.CommandPath, workflowPath, opts.Actions, "", "", false)
		}
		applyHint := buildCommandHint(opts.CommandPath, workflowPath, opts.Actions, "", "", true)
		output.Info("%s", previewMessage(workflowPath, refs, existingDeps, deps, reviewHint, applyHint))
		return nil
	}

	written, err := wf.WriteDependencies(deps)
	if err != nil {
		return fmt.Errorf("writing dependencies: %w", err)
	}
	if err := os.WriteFile(workflowPath, written, 0o644); err != nil {
		return fmt.Errorf("writing file: %w", err)
	}

	output.Success("Pinned %d dependencies in %s", len(deps), workflowPath)
	for _, dep := range deps {
		output.Detail("%s", dep.String())
	}
	return nil
}

func upgradeOneFile(opts *upgradeOptions, workflowPath string, r *resolver.Resolver, targets []upgradeTarget) error {
	wf, err := lockfile.Load(workflowPath)
	if err != nil {
		return err
	}

	existingDeps, err := wf.ReadDependencies()
	if err != nil {
		return err
	}

	refs, _, warnings := wf.ExtractActionRefs()
	for _, warning := range warnings {
		output.Warning("%s", warning)
	}

	if len(refs) == 0 {
		output.Skip("No repository action references found in %s", workflowPath)
		return nil
	}

	replacements := make(map[string]string)
	var matched []lockfile.ActionRef
	seenPlans := make(map[string]struct{})

	output.Info("Planning upgrade(s)...")
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
				return err
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
			output.Detail("%s already at %s", ref.FullName(), targetRef)
		} else {
			output.Detail("%s: %s -> %s", ref.FullName(), ref.Ref, targetRef)
		}
	}

	if len(matched) == 0 {
		var names []string
		for _, target := range targets {
			names = append(names, target.Match)
		}
		output.Skip("No references to %s found in %s", strings.Join(names, ", "), workflowPath)
		return nil
	}

	updatedContent, _, err := wf.RewriteActionRefs(replacements)
	if err != nil {
		return err
	}
	upgradedWF, err := lockfile.Parse(workflowPath, updatedContent)
	if err != nil {
		return err
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
		return fmt.Errorf("resolving actions: %w", err)
	}

	if mismatches := lockfile.CheckSHARefMismatches(deps); len(mismatches) > 0 {
		output.Error("action ref(s) look like commit SHAs but resolved to different OIDs:")
		for _, mismatch := range mismatches {
			output.Detail("%s: ref %s resolved to %s", mismatch.Dep.NWO, mismatch.Dep.Ref, mismatch.ResolvedAs)
			output.Hint("This ref may be a deceptive branch or tag name masquerading as a commit hash.")
		}
		return fmt.Errorf("%d action ref(s) have SHA-like names that point to different commits", len(mismatches))
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
		return nil
	}

	written, err := upgradedWF.WriteDependencies(deps)
	if err != nil {
		return fmt.Errorf("writing dependencies: %w", err)
	}
	if err := os.WriteFile(workflowPath, written, 0o644); err != nil {
		return fmt.Errorf("writing file: %w", err)
	}

	output.Success("Upgraded and pinned %d dependencies in %s", len(deps), workflowPath)
	for _, dep := range deps {
		output.Detail("%s", dep.String())
	}
	return nil
}

func validateOneFile(workflowPath string, r *resolver.Resolver) (*validationResult, error) {
	wf, err := lockfile.Load(workflowPath)
	if err != nil {
		return nil, err
	}

	existingDeps, err := wf.ReadDependencies()
	if err != nil {
		return nil, err
	}
	if len(existingDeps) == 0 {
		// Check if the workflow even has action references.
		// Run-only workflows (no uses: directives) need no pinning.
		refs, _, _ := wf.ExtractActionRefs()
		if len(refs) == 0 {
			return nil, errNoActions
		}
		return nil, errNoDeps
	}

	refs, _, parseWarnings := wf.ExtractActionRefs()

	result := &validationResult{
		Valid:    true,
		Warnings: parseWarnings,
	}

	depsByKey := make(map[string]lockfile.Dependency)
	for _, dep := range existingDeps {
		depsByKey[dep.Key()] = dep
	}

	for _, ref := range refs {
		key := ref.FullName() + "@" + ref.Ref
		if _, ok := depsByKey[key]; !ok {
			result.Valid = false
			result.Errors = append(result.Errors, validationError{
				Type:       "MISSING",
				Dependency: key,
				Details:    "used in workflow but not in dependencies: section",
			})
		}
	}

	output.Info("Re-resolving %d action reference(s)...", len(refs))
	liveDeps, err := r.ResolveAllRecursive(refs)
	if err != nil {
		return nil, fmt.Errorf("resolving actions: %w", err)
	}

	liveByKey := make(map[string]lockfile.Dependency)
	for _, dep := range liveDeps {
		liveByKey[dep.Key()] = dep
	}

	for _, existing := range existingDeps {
		live, ok := liveByKey[existing.Key()]
		if !ok {
			result.Valid = false
			result.Errors = append(result.Errors, validationError{
				Type:       "STALE",
				Dependency: existing.Key(),
				Details:    "in dependencies: but not discoverable from workflow uses: refs",
			})
			continue
		}
		if !strings.EqualFold(existing.SHA, live.SHA) {
			result.Valid = false
			result.Errors = append(result.Errors, validationError{
				Type:       "TAMPERED",
				Dependency: existing.Key(),
				Details:    fmt.Sprintf("expected %s but live resolution is %s", existing.SHA, live.SHA),
			})
		}
	}

	for _, mismatch := range lockfile.CheckSHARefMismatches(liveDeps) {
		result.Valid = false
		result.Errors = append(result.Errors, validationError{
			Type:       "SHA_MISMATCH",
			Dependency: mismatch.Dep.Key(),
			Details:    fmt.Sprintf("ref %s resolved to %s", mismatch.Dep.Ref, mismatch.ResolvedAs),
		})
	}

	// Reachability: verify pinned SHAs are on the ref's lineage in the
	// canonical repository, not injected from a fork network.
	output.Info("Checking commit reachability for %d dependency(ies)...", len(existingDeps))
	reachResults := r.CheckReachabilityAll(existingDeps)
	for _, rr := range reachResults {
		depID := rr.DepKey
		if depID == "" {
			depID = fmt.Sprintf("%s/%s@%s", rr.Owner, rr.Repo, rr.Ref)
		}
		switch rr.Status {
		case resolver.Unreachable:
			result.Valid = false
			result.Errors = append(result.Errors, validationError{
				Type:       "UNREACHABLE",
				Dependency: depID,
				Details:    fmt.Sprintf("SHA %s is not reachable from ref %s (%s)", rr.SHA[:12], rr.Ref, rr.Detail),
			})
		case resolver.ReachabilityUnknown:
			result.Warnings = append(result.Warnings,
				fmt.Sprintf("%s: reachability check inconclusive (%s)", depID, rr.Detail))
		}
	}

	if result.Valid {
		output.Success("%s valid", workflowPath)
	}
	return result, nil
}

func actionMatchesFilter(nwo, filter string) bool {
	if nwo == filter {
		return true
	}
	return strings.HasPrefix(nwo, filter+"/")
}

func actionMatchesAnyFilter(nwo string, filters []string) bool {
	for _, filter := range filters {
		if actionMatchesFilter(nwo, filter) {
			return true
		}
	}
	return false
}

func mergeTargetedDeps(existing, fresh []lockfile.Dependency, actions []string) []lockfile.Dependency {
	freshByKey := make(map[string]lockfile.Dependency)
	for _, dep := range fresh {
		freshByKey[dep.Key()] = dep
	}

	seen := make(map[string]bool)
	var merged []lockfile.Dependency

	for _, dep := range existing {
		if actionMatchesAnyFilter(dep.NWO, actions) {
			if freshDep, ok := freshByKey[dep.Key()]; ok {
				merged = append(merged, freshDep)
				seen[dep.Key()] = true
			}
		} else {
			merged = append(merged, dep)
			seen[dep.Key()] = true
		}
	}

	for _, dep := range fresh {
		if !seen[dep.Key()] {
			merged = append(merged, dep)
		}
	}

	return merged
}

func checkConsistency(existingDeps, newDeps []lockfile.Dependency, hostname string) error {
	oldByKey := make(map[string]lockfile.Dependency)
	for _, dep := range existingDeps {
		oldByKey[dep.Key()] = dep
	}

	type shaChange struct {
		dep    lockfile.Dependency
		oldSHA string
	}
	var shaChanges []shaChange
	for _, dep := range newDeps {
		if old, ok := oldByKey[dep.Key()]; ok && !strings.EqualFold(old.SHA, dep.SHA) {
			shaChanges = append(shaChanges, shaChange{dep: dep, oldSHA: old.SHA})
		}
	}
	if len(shaChanges) > 0 {
		output.Error("SHA changed for pinned dependencies (tag may have been force-pushed):")
		for _, change := range shaChanges {
			output.Detail("%s: %s -> %s", change.dep.Key(), change.oldSHA, change.dep.SHA)
			output.Detail("  compare: https://%s/%s/compare/%s...%s", hostname, change.dep.NWO, change.oldSHA, change.dep.SHA)
		}
		return fmt.Errorf("%d dependency SHA(s) changed since last pin", len(shaChanges))
	}

	return nil
}

type directRefChange struct {
	Name   string
	OldRef string
	NewRef string
}

func checkDirectRefChanges(refs []lockfile.ActionRef, existingDeps []lockfile.Dependency, allow bool) error {
	changes := detectDirectRefChanges(refs, existingDeps)
	if allow || len(changes) == 0 {
		return nil
	}

	output.Error("direct workflow action refs changed; refusing to bless them with --write:")
	for _, change := range changes {
		switch {
		case change.OldRef != "" && change.NewRef != "":
			output.Detail("%s: %s -> %s", change.Name, change.OldRef, change.NewRef)
		case change.NewRef != "":
			output.Detail("%s: new direct ref %s", change.Name, change.NewRef)
		default:
			output.Detail("%s: direct ref changed", change.Name)
		}
	}

	first := changes[0]
	if first.OldRef != "" && first.NewRef != "" {
		output.Hint("use `gh actions-pin upgrade --action %s --from %s --version %s --write`", first.Name, first.OldRef, first.NewRef)
	} else if first.NewRef != "" {
		output.Hint("rerun with `--allow-ref-changes` if adding %s@%s is intentional", first.Name, first.NewRef)
	}

	return fmt.Errorf("direct workflow refs changed since the last lock")
}

func detectDirectRefChanges(refs []lockfile.ActionRef, existingDeps []lockfile.Dependency) []directRefChange {
	oldByNWO := make(map[string][]lockfile.Dependency)
	currentRefsByNWO := make(map[string][]string)
	for _, dep := range existingDeps {
		oldByNWO[dep.NWO] = append(oldByNWO[dep.NWO], dep)
	}
	for _, ref := range refs {
		currentRefsByNWO[ref.FullName()] = append(currentRefsByNWO[ref.FullName()], ref.Ref)
	}

	matchedCurrent := make([]bool, len(refs))
	usedOld := make(map[string]map[int]bool)

	for i, ref := range refs {
		nwo := ref.FullName()
		for j, dep := range oldByNWO[nwo] {
			if dep.Ref != ref.Ref {
				continue
			}
			if usedOld[nwo] == nil {
				usedOld[nwo] = make(map[int]bool)
			}
			if usedOld[nwo][j] {
				continue
			}
			usedOld[nwo][j] = true
			matchedCurrent[i] = true
			break
		}
	}

	var changes []directRefChange
	for i, ref := range refs {
		if matchedCurrent[i] {
			continue
		}

		nwo := ref.FullName()
		foundOld := false
		for j, dep := range oldByNWO[nwo] {
			if usedOld[nwo] != nil && usedOld[nwo][j] {
				continue
			}
			if usedOld[nwo] == nil {
				usedOld[nwo] = make(map[int]bool)
			}
			usedOld[nwo][j] = true
			changes = append(changes, directRefChange{
				Name:   nwo,
				OldRef: dep.Ref,
				NewRef: ref.Ref,
			})
			foundOld = true
			break
		}
		if !foundOld {
			changes = append(changes, directRefChange{
				Name:   nwo,
				NewRef: ref.Ref,
			})
		}
	}

	for nwo, deps := range oldByNWO {
		currentRefs := currentRefsByNWO[nwo]
		if len(currentRefs) == 0 {
			continue
		}
		for j, dep := range deps {
			if usedOld[nwo] != nil && usedOld[nwo][j] {
				continue
			}
			changes = append(changes, directRefChange{
				Name:   nwo,
				OldRef: dep.Ref,
				NewRef: currentRefs[0],
			})
		}
	}

	return changes
}

func showDiff(hostname string, old, new []lockfile.Dependency) {
	oldMap := make(map[string]lockfile.Dependency)
	oldByNWO := make(map[string][]lockfile.Dependency)
	for _, dep := range old {
		oldMap[dep.Key()] = dep
		oldByNWO[dep.NWO] = append(oldByNWO[dep.NWO], dep)
	}
	newMap := make(map[string]lockfile.Dependency)
	for _, dep := range new {
		newMap[dep.Key()] = dep
	}

	handledOld := make(map[string]bool)
	handledNew := make(map[string]bool)

	for _, dep := range new {
		if oldDep, ok := oldMap[dep.Key()]; ok && !strings.EqualFold(oldDep.SHA, dep.SHA) {
			output.Infof("  %s %s\n", output.Yellow("~"), dep.Key())
			output.Infof("    %s sha1-%s\n", output.Red("-"), oldDep.SHA)
			output.Infof("    %s sha1-%s\n", output.Green("+"), dep.SHA)
			output.Infof("    compare: https://%s/%s/compare/%s...%s\n", hostname, dep.NWO, oldDep.SHA, dep.SHA)
			handledOld[oldDep.Key()] = true
			handledNew[dep.Key()] = true
		} else if ok {
			handledOld[oldDep.Key()] = true
			handledNew[dep.Key()] = true
		}
	}

	for _, dep := range new {
		if handledNew[dep.Key()] {
			continue
		}

		var replacement *lockfile.Dependency
		for _, oldDep := range oldByNWO[dep.NWO] {
			if handledOld[oldDep.Key()] {
				continue
			}
			replacement = &oldDep
			break
		}

		if replacement != nil {
			output.Infof("  %s %s\n", output.Yellow("~"), dep.NWO)
			output.Infof("    %s %s\n", output.Red("-"), replacement.String())
			output.Infof("    %s %s\n", output.Green("+"), dep.String())
			if !strings.EqualFold(replacement.SHA, dep.SHA) {
				output.Infof("    compare: https://%s/%s/compare/%s...%s\n", hostname, dep.NWO, replacement.SHA, dep.SHA)
			} else {
				output.Infof("    permalink: https://%s/%s/commit/%s\n", hostname, dep.NWO, dep.SHA)
			}
			handledOld[replacement.Key()] = true
			handledNew[dep.Key()] = true
		}
	}

	for _, dep := range new {
		if handledNew[dep.Key()] {
			continue
		}
		output.Infof("  %s %s\n", output.Green("+"), dep.String())
		output.Infof("    permalink: https://%s/%s/commit/%s\n", hostname, dep.NWO, dep.SHA)
	}

	for _, dep := range old {
		if handledOld[dep.Key()] {
			continue
		}
		if _, ok := newMap[dep.Key()]; !ok {
			output.Infof("  %s %s\n", output.Red("-"), dep.String())
		}
	}

	unchanged := 0
	for _, dep := range new {
		if oldDep, ok := oldMap[dep.Key()]; ok && strings.EqualFold(oldDep.SHA, dep.SHA) {
			unchanged++
		}
	}
	if unchanged > 0 {
		output.Infof("  %s\n", output.Dim(fmt.Sprintf("%d unchanged", unchanged)))
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
		lines = append(lines, "  Direct ref changes need `upgrade` or `--allow-ref-changes` when writing.")
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

	oldMap := make(map[string]lockfile.Dependency, len(old))
	oldByNWO := make(map[string][]lockfile.Dependency)
	for _, dep := range old {
		oldMap[dep.Key()] = dep
		oldByNWO[dep.NWO] = append(oldByNWO[dep.NWO], dep)
	}

	newMap := make(map[string]lockfile.Dependency, len(new))
	handledOld := make(map[string]bool)
	handledNew := make(map[string]bool)

	for _, dep := range new {
		newMap[dep.Key()] = dep

		_, direct := directNames[dep.NWO]
		if oldDep, ok := oldMap[dep.Key()]; ok {
			handledOld[oldDep.Key()] = true
			handledNew[dep.Key()] = true
			if !strings.EqualFold(oldDep.SHA, dep.SHA) {
				if direct {
					stats.directChanged++
				} else {
					stats.transitiveChanged++
				}
			} else if direct {
				stats.directUnchanged++
			} else {
				stats.transitiveKept++
			}
			continue
		}
	}

	for _, dep := range new {
		if handledNew[dep.Key()] {
			continue
		}
		_, direct := directNames[dep.NWO]
		var replacement *lockfile.Dependency
		for _, oldDep := range oldByNWO[dep.NWO] {
			if handledOld[oldDep.Key()] {
				continue
			}
			replacement = &oldDep
			break
		}
		if replacement != nil {
			handledOld[replacement.Key()] = true
			handledNew[dep.Key()] = true
			if direct {
				stats.directChanged++
			} else {
				stats.transitiveChanged++
			}
			continue
		}
		if direct {
			stats.directAdded++
		} else {
			stats.transitiveAdded++
		}
	}

	for _, dep := range old {
		if handledOld[dep.Key()] {
			continue
		}
		if _, direct := directNames[dep.NWO]; direct {
			stats.directRemoved++
		} else {
			stats.transitiveRemoved++
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

func writeValidationJSON(result *validationResult, fieldsCSV string) error {
	fields := []string{"valid", "errors", "warnings"}
	if fieldsCSV != "" {
		fields = strings.Split(fieldsCSV, ",")
	}

	payload := map[string]interface{}{}
	for _, field := range fields {
		field = strings.TrimSpace(field)
		switch field {
		case "valid":
			payload[field] = result.Valid
		case "errors":
			payload[field] = result.Errors
		case "warnings":
			payload[field] = result.Warnings
		default:
			return fmt.Errorf("unknown JSON field %q (expected valid, errors, warnings)", field)
		}
	}

	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(payload)
}
