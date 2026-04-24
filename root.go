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

var errSilent = errors.New("silent error")
var errNoDeps = errors.New("no dependencies: section found")
var errNoActions = errors.New("no action references found")
var newResolver = resolver.New
var output = ui.New()

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
	Prompter      doctor.Prompter
}

type upgradeTarget struct {
	Match      string
	CurrentRef string
	TargetRef  string
}

type validationError struct {
	Type        string `json:"type"`
	Dependency  string `json:"dependency"`
	Details     string `json:"details"`
	CompareURL  string `json:"compare_url,omitempty"`
	ReleasesURL string `json:"releases_url,omitempty"`
}

type validationWarning struct {
	Dependency   string `json:"dependency"`
	Details      string `json:"details"`
	WorkflowPath string `json:"workflow_path,omitempty"`
	Transitive   bool   `json:"transitive,omitempty"`
}

// warningKey returns a grouping key (same dependency+details = same warning).
func (w validationWarning) warningKey() string {
	return w.Dependency + "\x00" + w.Details
}

func (w validationWarning) String() string {
	s := fmt.Sprintf("%s: %s", w.Dependency, w.Details)
	if w.Transitive {
		s += " (transitive dependency)"
	}
	return s
}

type validationResult struct {
	Valid    bool                `json:"valid"`
	Errors  []validationError   `json:"errors"`
	Warnings []validationWarning `json:"warnings"`
}

func newRootCmd() *cobra.Command {
	opts := &checkOptions{}

	cmd := &cobra.Command{
		Use:           "actions-pin [<workflow-path>...]",
		Args:          cobra.ArbitraryArgs,
		Short:         "Verify pinned GitHub Actions workflow dependencies",
		SilenceErrors: true,
		SilenceUsage:  true,
		Long: heredoc.Doc(`
			Re-resolve all action dependencies in workflow files and compare them
			against the pinned SHAs in the dependencies: section.

			With no arguments, the extension discovers and validates all workflows
			under .github/workflows/.

			Use subcommands to manage your workflow's dependencies:

			  gh actions-pin             Verify the lock section against live resolution
			  gh actions-pin upgrade     Bump action refs and repin them
			  gh actions-pin doctor      Interactively diagnose and fix pinning issues
		`),
		Example: heredoc.Doc(`
			# Verify all workflows
			$ gh actions-pin

			# Verify a specific workflow
			$ gh actions-pin .github/workflows/ci.yml

			# Output JSON for CI
			$ gh actions-pin --json valid,errors

			# Interactively pin and fix all workflows
			$ gh actions-pin doctor

			# Upgrade a specific action
			$ gh actions-pin upgrade --action actions/checkout --write
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
	cmd.AddCommand(newCheckCmd())
	cmd.AddCommand(newUpgradeCmd())
	cmd.AddCommand(newDoctorCmd())

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

			With no flags, runs interactively: scans all workflows, shows which
			actions have newer versions available, and lets you pick which to
			upgrade. Release page links are shown so you can review changelogs
			before confirming.

			Pass --action to target specific actions non-interactively. By default,
			each selected action is upgraded to its latest stable tag. Use --version
			to force a specific target ref for all selected actions, or specify it
			inline as owner/repo@ref. Use --from to limit upgrades to actions
			currently on a specific ref.

			In non-interactive mode, pass --write to apply changes. In interactive
			mode, you'll be prompted to confirm.
		`),
		Example: heredoc.Doc(`
			# Interactive: pick which actions to upgrade
			$ gh actions-pin upgrade

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
			p := doctor.NewHuhPrompter()
			opts.Prompter = p
			if len(opts.Actions) == 0 {
				// Interactive mode — must be a TTY.
				if !p.IsInteractive() {
					return fmt.Errorf("--action is required in non-interactive mode\n\n  gh actions-pin upgrade --action actions/checkout --write")
				}
				return runUpgradeInteractive(opts)
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
			output.Detail("Release notes: https://github.com/%s/releases", c.NWO)
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

	// For a single workflow, show verbose output inline (no log file).
	// For multiple workflows, write verbose output to a temp log file.
	singleFile := len(opts.WorkflowPaths) == 1
	var logFile *os.File
	if opts.JSONFields == "" && !singleFile {
		logFile, err = os.CreateTemp("", "gh-actions-pin-check-*.log")
		if err != nil {
			return fmt.Errorf("creating log file: %w", err)
		}
		defer logFile.Close()
	}
	logOutput := ui.NewPlain(os.Stderr) // inline for single file or fallback
	if logFile != nil {
		logOutput = ui.NewPlain(logFile)
	}

	total := len(opts.WorkflowPaths)
	aggregate := &validationResult{Valid: true}
	var skipped, valid, failed int

	if opts.JSONFields == "" && !singleFile {
		output.StartProgress(fmt.Sprintf("Checking %d %s", total, ui.Pluralize(total, "workflow", "workflows")))
	}

	for _, workflowPath := range opts.WorkflowPaths {
		logOutput.Header("%s", workflowPath)

		result, err := validateOneFile(workflowPath, r, logOutput)
		if err != nil {
			if errors.Is(err, errNoActions) {
				continue
			}
			if errors.Is(err, errNoDeps) {
				skipped++
				w := validationWarning{
					Details:      "not yet pinned (run `gh actions-pin doctor` to fix)",
					WorkflowPath: workflowPath,
				}
				aggregate.Warnings = append(aggregate.Warnings, w)
				logOutput.Warning("%s", w.String())
				continue
			}
			failed++
			aggregate.Valid = false
			aggregate.Errors = append(aggregate.Errors, validationError{
				Type:       "ERROR",
				Dependency: workflowPath,
				Details:    err.Error(),
			})
			logOutput.Error("%s: %s", workflowPath, err)
			continue
		}
		if !result.Valid {
			failed++
			aggregate.Valid = false
		} else {
			valid++
		}
		aggregate.Errors = append(aggregate.Errors, result.Errors...)
		aggregate.Warnings = append(aggregate.Warnings, result.Warnings...)
	}

	output.StopProgress()

	if opts.JSONFields != "" {
		return writeValidationJSON(aggregate, opts.JSONFields)
	}

	// Summary.
	checked := valid + failed
	if aggregate.Valid && checked > 0 {
		output.Success("All %d %s valid", checked, ui.Pluralize(checked, "workflow", "workflows"))
	} else if checked > 0 {
		typeCounts := map[string]int{}
		for _, e := range aggregate.Errors {
			typeCounts[e.Type]++
		}
		parts := []string{}
		for _, t := range []string{"TAMPERED", "MISSING", "STALE", "SHA_MISMATCH", "UNREACHABLE", "ERROR"} {
			if n, ok := typeCounts[t]; ok {
				parts = append(parts, fmt.Sprintf("%d %s", n, strings.ToLower(t)))
			}
		}
		output.Error("%d of %d %s failed: %s",
			failed, checked,
			ui.Pluralize(checked, "workflow", "workflows"),
			strings.Join(parts, ", "))

		// Show each error inline so users don't have to dig through the log.
		for _, e := range aggregate.Errors {
			label := output.Dim("[" + e.Type + "]")
			output.Detail("  %s %s: %s", label, e.Dependency, e.Details)
			if e.CompareURL != "" {
				output.Detail("    → Compare: %s", e.CompareURL)
			}
			if e.ReleasesURL != "" {
				output.Detail("    → Releases: %s", e.ReleasesURL)
			}
		}
	}
	if len(aggregate.Warnings) > 0 {
		// Group warnings by key (same dependency+details) and collect workflow files.
		type groupedWarning struct {
			warning validationWarning
			files   []string
		}
		var order []string
		groups := map[string]*groupedWarning{}
		for _, w := range aggregate.Warnings {
			key := w.warningKey()
			if g, ok := groups[key]; ok {
				if w.WorkflowPath != "" {
					g.files = append(g.files, w.WorkflowPath)
				}
			} else {
				order = append(order, key)
				g := &groupedWarning{warning: w}
				if w.WorkflowPath != "" {
					g.files = []string{w.WorkflowPath}
				}
				groups[key] = g
			}
		}
		for _, key := range order {
			g := groups[key]
			output.Warning("%s", g.warning.String())
			for _, f := range g.files {
				output.Detail("in %s", f)
			}
		}
	}
	if logFile != nil {
		output.Hint("Full log: %s", logFile.Name())
	}

	if !aggregate.Valid {
		return errSilent
	}
	return nil
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

func validateOneFile(workflowPath string, r *resolver.Resolver, log *ui.UI) (*validationResult, error) {
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

	// Build set of direct action NWOs (from workflow uses: lines).
	directNWOs := make(map[string]bool)
	for _, ref := range refs {
		directNWOs[ref.FullName()] = true
	}

	result := &validationResult{
		Valid: true,
	}
	for _, pw := range parseWarnings {
		result.Warnings = append(result.Warnings, validationWarning{
			Details:      pw,
			WorkflowPath: workflowPath,
		})
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

	log.Info("Re-resolving %d action reference(s)...", len(refs))
	liveDeps, err := r.ResolveAllRecursive(refs)
	if err != nil {
		return nil, fmt.Errorf("resolving actions: %w", err)
	}

	liveByKey := make(map[string]lockfile.Dependency)
	for _, dep := range liveDeps {
		liveByKey[dep.Key()] = dep
	}

	// Build a secondary index by NWO so we can match transitive deps whose
	// lockfile ref differs from the live-resolved ref. Common cases:
	//   - Narrowed: lockfile has v4.1.0, live has v4
	//   - SHA-pinned composite: lockfile has v4.1.0, live has 59d894... (bare SHA)
	liveByNWO := make(map[string][]lockfile.Dependency)
	for _, dep := range liveDeps {
		liveByNWO[dep.NWO] = append(liveByNWO[dep.NWO], dep)
	}

	for _, existing := range existingDeps {
		live, ok := liveByKey[existing.Key()]
		if !ok {
			// Try to match by NWO: same repo, same SHA = same transitive dep
			// regardless of whether the ref is a tag, branch, or bare SHA.
			if candidates, has := liveByNWO[existing.NWO]; has {
				for _, cand := range candidates {
					if strings.EqualFold(cand.SHA, existing.SHA) {
						live = cand
						ok = true
						break
					}
					if doctor.IsNarrowedVersion(cand.Ref, existing.Ref) {
						live = cand
						ok = true
						break
					}
				}
			}
			if !ok {
				// Not a direct ref and not a recognizable transitive dep.
				// Check if this NWO is a direct workflow action — if not,
				// it's a transitive dep that the upstream composite changed.
				if !directNWOs[existing.NWO] {
					result.Warnings = append(result.Warnings, validationWarning{
						Dependency:   existing.Key(),
						Details:      "transitive dependency no longer discovered from upstream composite action",
						WorkflowPath: workflowPath,
						Transitive:   true,
					})
				} else {
					result.Valid = false
					result.Errors = append(result.Errors, validationError{
						Type:       "STALE",
						Dependency: existing.Key(),
						Details:    "in dependencies: but not discoverable from workflow uses: refs",
					})
				}
				continue
			}
		}
		if !strings.EqualFold(existing.SHA, live.SHA) {
			result.Valid = false
			// Build enrichment URLs for TAMPERED findings.
			parts := strings.SplitN(existing.NWO, "/", 3)
			var compareURL, releasesURL string
			if len(parts) >= 2 {
				baseURL := fmt.Sprintf("https://%s/%s/%s", r.Hostname(), parts[0], parts[1])
				compareURL = fmt.Sprintf("%s/compare/%s...%s", baseURL, existing.SHA, live.SHA)
				releasesURL = baseURL + "/releases"
			}
			result.Errors = append(result.Errors, validationError{
				Type:        "TAMPERED",
				Dependency:  existing.Key(),
				Details:     fmt.Sprintf("expected %s but live resolution is %s", existing.SHA, live.SHA),
				CompareURL:  compareURL,
				ReleasesURL: releasesURL,
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
	log.Info("Checking commit reachability for %d dependency(ies)...", len(existingDeps))
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
			isTransitive := !directNWOs[rr.Owner+"/"+rr.Repo]
			result.Warnings = append(result.Warnings, validationWarning{
				Dependency:   depID,
				Details:      rr.Detail,
				WorkflowPath: workflowPath,
				Transitive:   isTransitive,
			})
		}
	}

	if result.Valid {
		log.Success("%s valid", workflowPath)
	}

	// Log errors/warnings to the file.
	for _, e := range result.Errors {
		log.Error("[%s] %s: %s", e.Type, e.Dependency, e.Details)
		if e.CompareURL != "" {
			log.Detail("  → Compare: %s", e.CompareURL)
		}
		if e.ReleasesURL != "" {
			log.Detail("  → Releases: %s", e.ReleasesURL)
		}
		if e.Type == "TAMPERED" {
			log.Detail("  → If unexpected, reach out to the action maintainer")
		}
	}
	for _, w := range result.Warnings {
		log.Warning("%s", w.String())
	}

	return result, nil
}

func actionMatchesFilter(nwo, filter string) bool {
	if nwo == filter {
		return true
	}
	return strings.HasPrefix(nwo, filter+"/")
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

type doctorOptions struct {
	Hostname      string
	NoInteractive bool
	Write         bool
	JSONFields    string
}

func newDoctorCmd() *cobra.Command {
	opts := &doctorOptions{}

	cmd := &cobra.Command{
		Use:   "doctor [<workflow-path>...]",
		Args:  cobra.ArbitraryArgs,
		Short: "Diagnose and interactively fix pinning issues",
		Long: heredoc.Doc(`
			Scan workflow files for pinning issues and walk through fixes
			interactively. Doctor diagnoses:

			  • Unpinned workflows (action refs without a dependencies: section)
			  • SHA-as-ref anti-pattern (bare SHA pins that weaken security)
			  • Stale pins (SHA no longer matches live ref resolution)
			  • Unreachable commits (possible fork-network injection)

			By default, doctor auto-discovers workflows in .github/workflows/
			and presents interactive prompts for each issue. Use --no-interactive
			for report-only mode, or --write to auto-fix safe issues (unpinned
			workflows only).
		`),
		Example: heredoc.Doc(`
			# Interactive diagnosis and repair
			$ gh actions-pin doctor

			# Report-only mode (no prompts, no changes)
			$ gh actions-pin doctor --no-interactive

			# Auto-pin unpinned workflows without prompting
			$ gh actions-pin doctor --write

			# Diagnose a specific workflow
			$ gh actions-pin doctor .github/workflows/ci.yml

			# JSON output for automation
			$ gh actions-pin doctor --json findings
		`),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDoctor(opts, args)
		},
	}

	cmd.Flags().BoolVar(&opts.NoInteractive, "no-interactive", false, "Report-only mode (no prompts, no changes)")
	cmd.Flags().BoolVar(&opts.Write, "write", false, "Auto-apply safe fixes (unpinned workflows only)")
	cmd.Flags().StringVar(&opts.Hostname, "hostname", "", "GitHub hostname (default: auto-detect)")
	cmd.Flags().StringVar(&opts.JSONFields, "json", "", "Output as JSON (fields: findings)")

	return cmd
}

func runDoctor(opts *doctorOptions, paths []string) error {
	hostname := resolveHostname(opts.Hostname)
	r, err := newResolver(hostname)
	if err != nil {
		return err
	}

	// Discover or validate workflow paths.
	workflowPaths, err := doctor.ResolveWorkflowPaths(paths)
	if err != nil {
		return err
	}

	output.StartProgress(fmt.Sprintf("Scanning %d %s", len(workflowPaths), ui.Pluralize(len(workflowPaths), "workflow", "workflows")))

	// Phase 1: Diagnose.
	report := doctor.Diagnose(workflowPaths, r)

	output.StopProgress()

	// Print summary.
	actionable := report.WorkflowsNeedingAttention()
	runOnlyCount := 0
	for _, wr := range report.Workflows {
		if len(wr.Findings) == 1 && wr.Findings[0].Category == doctor.CategoryRunOnly {
			runOnlyCount++
		}
	}
	validWorkflows := len(report.Workflows) - len(actionable) - runOnlyCount
	if validWorkflows > 0 {
		output.Success("%d %s fully pinned and valid", validWorkflows, ui.Pluralize(validWorkflows, "workflow", "workflows"))
	}
	if len(actionable) > 0 {
		output.Warning("%d %s need attention", len(actionable), ui.Pluralize(len(actionable), "workflow", "workflows"))
	}
	if runOnlyCount > 0 {
		output.Skip("%d %s have no action dependencies", runOnlyCount, ui.Pluralize(runOnlyCount, "workflow", "workflows"))
	}
	fmt.Fprintln(os.Stderr)

	// JSON mode: output findings and exit.
	if opts.JSONFields != "" {
		return writeDoctorJSON(report)
	}

	// Phase 2: Remediate.
	if len(actionable) == 0 {
		return nil
	}

	// Create REST client for tag listing.
	restClient, err := api.NewRESTClient(api.ClientOptions{Host: hostname})
	if err != nil {
		return fmt.Errorf("creating REST client: %w", err)
	}

	var prompter doctor.Prompter
	if opts.NoInteractive {
		prompter = &doctor.NoopPrompter{}
	} else {
		prompter = doctor.NewHuhPrompter()
	}

	// Detect current repo owner for same-owner action detection.
	var repoOwner string
	if currentRepo, err := repository.Current(); err == nil {
		repoOwner = currentRepo.Owner
	}

	rem := doctor.NewRemediator(prompter, r, restClient, output, doctor.RemediateOptions{
		Write:       opts.Write,
		Interactive: !opts.NoInteractive && prompter.IsInteractive(),
		RepoOwner:   repoOwner,
	})

	if err := rem.Remediate(report); err != nil {
		if errors.Is(err, doctor.ErrAborted) {
			fmt.Fprintln(os.Stderr)
			output.Info("Interrupted — no further changes applied")
			return nil
		}
		return err
	}

	// Phase 3: Summary.
	fmt.Fprintln(os.Stderr)
	if rem.Fixed > 0 {
		output.Success("%d %s fixed", rem.Fixed, ui.Pluralize(rem.Fixed, "issue", "issues"))
	}
	if rem.Skipped > 0 {
		output.Skip("%d %s skipped", rem.Skipped, ui.Pluralize(rem.Skipped, "issue", "issues"))
	}
	if rem.Alerted > 0 {
		output.Warning("%d %s need manual attention", rem.Alerted, ui.Pluralize(rem.Alerted, "issue", "issues"))
	}

	if rem.Alerted > 0 {
		return errSilent
	}
	return nil
}

func writeDoctorJSON(report *doctor.Report) error {
	type jsonFinding struct {
		Workflow    string `json:"workflow"`
		Category   string `json:"category"`
		Severity   string `json:"severity"`
		Dependency string `json:"dependency,omitempty"`
		Detail     string `json:"detail"`
		Remediation string `json:"remediation,omitempty"`
	}

	var findings []jsonFinding
	for _, wr := range report.Workflows {
		for _, f := range wr.Findings {
			jf := jsonFinding{
				Workflow:    f.WorkflowPath,
				Category:    string(f.Category),
				Severity:    string(f.Severity),
				Detail:      f.Detail,
				Remediation: f.Remediation,
			}
			if f.Dependency != nil {
				jf.Dependency = f.Dependency.Key()
			} else if f.ActionRef != nil {
				jf.Dependency = f.ActionRef.FullName() + "@" + f.ActionRef.Ref
			}
			findings = append(findings, jf)
		}
	}

	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(map[string]interface{}{
		"findings": findings,
	})
}
