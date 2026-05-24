package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime/debug"
	"strings"

	"github.com/MakeNowJust/heredoc"
	"github.com/cli/go-gh/v2/pkg/api"
	"github.com/cli/go-gh/v2/pkg/repository"
	"github.com/github/gh-actions-pin/internal/doctor"
	"github.com/github/gh-actions-pin/internal/lockfile"
	"github.com/github/gh-actions-pin/internal/ui"
	"github.com/spf13/cobra"
)

type checkOptions struct {
	WorkflowPaths []string
	JSONFields    string
	Hostname      string
	NoInteractive bool
}

// JSON output types — thin wrappers around doctor.Report.

type checkFinding struct {
	Workflow    string `json:"workflow"`
	Category    string `json:"category"`
	Severity    string `json:"severity"`
	Dependency  string `json:"dependency,omitempty"`
	RequiredBy  string `json:"required_by,omitempty"`
	Detail      string `json:"detail"`
	Remediation string `json:"remediation,omitempty"`
	DocURL      string `json:"doc_url,omitempty"`
}

type checkDependency struct {
	NWO        string   `json:"nwo"`
	Ref        string   `json:"ref"`
	SHA        string   `json:"sha"`
	HashAlgo   string   `json:"hash_algo,omitempty"`
	Direct     bool     `json:"direct"`
	RequiredBy []string `json:"required_by,omitempty"`
}

type checkWorkflow struct {
	Path         string            `json:"path"`
	Valid        bool              `json:"valid"`
	Findings     []checkFinding    `json:"findings"`
	Dependencies []checkDependency `json:"dependencies,omitempty"`
}

func newCheckCmd(f *pinFactory) *cobra.Command {
	opts := &checkOptions{}

	cmd := &cobra.Command{
		Use:   "check [<workflow-path>...]",
		Args:  cobra.ArbitraryArgs,
		Short: "Verify the dependency lock and fix issues",
		Long: heredoc.Doc(`
			Verify that every action dependency in your workflows is locked to
			an immutable commit SHA and that the lock is still valid.

			Scans all workflows under .github/workflows/ by default, or pass
			specific paths. Checks both direct and transitive dependencies
			(composite actions that reference other actions).

			When run interactively (TTY), offers to fix issues inline.
			Non-interactive mode (--no-interactive or CI) auto-fixes
			deterministic issues and exits non-zero if anything remains
			that requires human judgment.

			With --json, structured results go to stdout and progress to stderr:

			  gh actions-pin check --json 2>/dev/null | jq .valid

			Issue types:
			  REF_MOVED      - locked SHA no longer matches upstream (expected for mutable tags like v4)
			  MISSING       - action in workflow has no lock entry
			  STALE         - lock entry references an action no longer in the workflow
			  REF_CHANGED   - workflow ref was edited; lock needs updating
			  MISLEADING_SHA - ref looks like a SHA but resolves to a different commit
			  IMPOSTER_COMMIT   - locked SHA is not in the ref's history
		`),
		Example: heredoc.Doc(`
			# Verify all workflows
			$ gh actions-pin check

			# Verify a specific workflow
			$ gh actions-pin check .github/workflows/ci.yml

			# Report-only mode (no prompts, no changes)
			$ gh actions-pin check --no-interactive

			# JSON output for CI
			$ gh actions-pin check --json=valid,findings

			# All fields as JSON
			$ gh actions-pin check --json
		`),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) > 0 {
				opts.WorkflowPaths = args
			}
			return runCheck(f, opts)
		},
	}

	cmd.Flags().StringVar(&opts.JSONFields, "json", "", "Output JSON with the specified `fields` (valid,findings,workflows,dependencies)")
	cmd.Flags().Lookup("json").NoOptDefVal = "valid,findings,workflows"
	cmd.Flags().StringVar(&opts.Hostname, "hostname", "", "GitHub hostname to query (defaults to GH_HOST, current repo host, or github.com)")
	cmd.Flags().BoolVar(&opts.NoInteractive, "no-interactive", false, "Auto-fix deterministic issues; fail on issues requiring human input")
	return cmd
}

func runCheck(f *pinFactory, opts *checkOptions) error {
	paths, err := discoverWorkflowPaths(opts.WorkflowPaths)
	if err != nil {
		return err
	}
	opts.WorkflowPaths = paths

	r, err := f.NewResolver(resolveHostname(opts.Hostname))
	if err != nil {
		return err
	}
	// Respect test overrides — only apply config when no test func is injected.
	if !r.HasReachabilityFunc() {
		r.DisableReachability = !doctor.ReachabilityEnabled()
	}
	store, err := lockfile.OpenStore(".", r)
	if err != nil {
		return fmt.Errorf("opening lockfile: %w", err)
	}
	// Single pass: doctor.Diagnose handles all validation.
	total := len(opts.WorkflowPaths)
	if opts.JSONFields == "" && total > 1 {
		f.UI.StartProgress(fmt.Sprintf("Checking %d %s", total, ui.Pluralize(total, "workflow", "workflows")))
	}

	report := doctor.Diagnose(opts.WorkflowPaths, r, store)

	// Repo-level checks: warn if this repo defines actions but doesn't
	// publish them via immutable releases. Best-effort — failures here
	// (no remote, API errors, etc.) silently produce no findings.
	if currentRepo, repoErr := repository.Current(); repoErr == nil {
		hostname := resolveHostname(opts.Hostname)
		if restClient, clientErr := api.NewRESTClient(api.ClientOptions{Host: hostname}); clientErr == nil {
			report.RepoFindings = append(
				report.RepoFindings,
				doctor.CheckRepoImmutableReleases(restClient, ".", currentRepo.Owner, currentRepo.Name)...,
			)
		}
	}

	f.UI.StopProgress()

	// Compute validity from findings.
	valid := report.IsValid()

	// JSON output — always before any human-readable output.
	if opts.JSONFields != "" {
		return writeCheckJSON(f.Out, report, valid, opts.JSONFields, store.File().Version)
	}

	// Determine if interactive remediation will follow.
	interactive := !opts.NoInteractive && os.Getenv("CI") != "true" && f.IsTerminal()

	// Always remediate — non-interactive mode auto-fixes what it can.
	willRemediate := true

	// Human-readable output.
	presentCheckResults(f.UI, report, valid, willRemediate)

	// Remediation.
	actionable := report.WorkflowsNeedingAttention()
	var fixedCount, skippedCount, alertedCount int
	var skippedDeps, alertedDeps []string

	if willRemediate && len(actionable) > 0 {
		hostname := resolveHostname(opts.Hostname)
		restClient, err := api.NewRESTClient(api.ClientOptions{Host: hostname})
		if err != nil {
			return fmt.Errorf("creating REST client: %w", err)
		}

		var prompter doctor.Prompter
		if !interactive {
			prompter = &doctor.NoopPrompter{}
		} else {
			prompter = doctor.NewHuhPrompterWithWriter(f.ErrOut, f.IsTerminal)
		}

		var repoOwner string
		if currentRepo, err := repository.Current(); err == nil {
			repoOwner = currentRepo.Owner
		}

		rem := doctor.NewRemediator(prompter, r, restClient, store, f.UI, doctor.RemediateOptions{
			Interactive: interactive,
			RepoOwner:   repoOwner,
		})

		if err := rem.Remediate(report); err != nil {
			if errors.Is(err, doctor.ErrAborted) {
				f.UI.Blank()
				f.UI.Info("Interrupted — no further changes applied")
				return nil
			}
			return err
		}

		if err := store.Save(); err != nil {
			return fmt.Errorf("saving lockfile: %w", err)
		}

		f.UI.Blank()
		if rem.Fixed > 0 {
			f.UI.Success("%d %s fixed", rem.Fixed, ui.Pluralize(rem.Fixed, "issue", "issues"))
		}
		uniqueSkipped := len(rem.SkippedDeps)
		if uniqueSkipped > 0 {
			f.UI.Skip("%d %s skipped", uniqueSkipped, ui.Pluralize(uniqueSkipped, "action", "actions"))
		}
		fixedCount = rem.Fixed
		skippedCount = uniqueSkipped
		alertedCount = rem.Alerted
		skippedDeps = rem.SkippedDeps
		alertedDeps = rem.AlertedDeps
	}

	if !valid || skippedCount > 0 || alertedCount > 0 {
		// Exit 0 only if everything was resolved — nothing skipped or alerted.
		if fixedCount > 0 && skippedCount == 0 && alertedCount == 0 {
			return nil
		}
		if alertedCount > 0 {
			f.UI.Blank()
			f.UI.Error("%d %s %s investigation — do not auto-fix:",
				alertedCount, ui.Pluralize(alertedCount, "action", "actions"), ui.Pluralize(alertedCount, "requires", "require"))
			for _, dep := range alertedDeps {
				f.UI.Detail("  %s", dep)
				for _, path := range workflowsForDep(report, dep) {
					f.UI.Detail("    %s %s", f.UI.Dim("└─"), f.UI.Dim(path))
				}
			}
		}
		if skippedCount > 0 {
			f.UI.Blank()
			f.UI.Error("%d %s %s interactive resolution — run `gh actions-pin` locally:",
				skippedCount, ui.Pluralize(skippedCount, "action", "actions"), ui.Pluralize(skippedCount, "requires", "require"))
			for _, dep := range skippedDeps {
				f.UI.Detail("  %s", dep)
			}
		}
		return errSilent
	}
	return nil
}

// findingToJSON converts a doctor.Finding to a JSON-safe struct.
func findingToJSON(f doctor.Finding) checkFinding {
	jf := checkFinding{
		Workflow:    f.WorkflowPath,
		Category:    string(f.Category),
		Severity:    string(f.Severity),
		Detail:      f.Detail,
		Remediation: f.Remediation,
		DocURL:      f.DocURL,
	}
	if f.Dependency != nil {
		jf.Dependency = f.Dependency.Key()
	} else if f.ActionRef != nil {
		jf.Dependency = f.ActionRef.FullName() + "@" + f.ActionRef.Ref
	}
	if f.ParentNWO != "" {
		jf.RequiredBy = f.ParentNWO
	}
	return jf
}

// writeCheckJSON writes the unified JSON output.
func writeCheckJSON(w io.Writer, report *doctor.Report, valid bool, fieldsCSV, lockfileVersion string) error {
	fields := strings.Split(fieldsCSV, ",")

	// Build all data lazily.
	var allFindings []checkFinding
	var allDeps []checkDependency
	var allWorkflows []checkWorkflow

	buildFindings := func() []checkFinding {
		if allFindings != nil {
			return allFindings
		}
		allFindings = []checkFinding{}
		for _, f := range report.RepoFindings {
			allFindings = append(allFindings, findingToJSON(f))
		}
		for _, wr := range report.Workflows {
			for _, f := range wr.Findings {
				if f.Category == doctor.CategoryRunOnly || (f.Category == doctor.CategoryValid && f.Severity == doctor.SeverityOK) {
					continue
				}
				allFindings = append(allFindings, findingToJSON(f))
			}
		}
		return allFindings
	}

	buildDeps := func() []checkDependency {
		if allDeps != nil {
			return allDeps
		}
		allDeps = []checkDependency{}
		// Deduplicate across workflows, merging required_by lists.
		seen := make(map[string]*checkDependency)
		var order []string
		for _, wr := range report.Workflows {
			for _, inv := range wr.Inventory {
				key := inv.Dep.Key()
				if existing, ok := seen[key]; ok {
					// Merge required_by lists.
					for _, p := range inv.Parents {
						found := false
						for _, ep := range existing.RequiredBy {
							if ep == p {
								found = true
								break
							}
						}
						if !found {
							existing.RequiredBy = append(existing.RequiredBy, p)
						}
					}
					// If direct in any workflow, mark as direct.
					if inv.Direct {
						existing.Direct = true
					}
					continue
				}
				d := checkDependency{
					NWO:        inv.Dep.NWO,
					Ref:        inv.Dep.Ref,
					SHA:        inv.Dep.SHA,
					HashAlgo:   inv.Dep.HashAlgo,
					Direct:     inv.Direct,
					RequiredBy: inv.Parents,
				}
				seen[key] = &d
				order = append(order, key)
			}
		}
		for _, key := range order {
			allDeps = append(allDeps, *seen[key])
		}
		return allDeps
	}

	buildWorkflows := func() []checkWorkflow {
		if allWorkflows != nil {
			return allWorkflows
		}
		allWorkflows = []checkWorkflow{}
		for _, wr := range report.Workflows {
			wf := checkWorkflow{
				Path:     wr.Path,
				Valid:    wr.IsValid(),
				Findings: []checkFinding{},
			}
			for _, f := range wr.Findings {
				if f.Category == doctor.CategoryRunOnly || (f.Category == doctor.CategoryValid && f.Severity == doctor.SeverityOK) {
					continue
				}
				wf.Findings = append(wf.Findings, findingToJSON(f))
			}
			for _, inv := range wr.Inventory {
				wf.Dependencies = append(wf.Dependencies, checkDependency{
					NWO:        inv.Dep.NWO,
					Ref:        inv.Dep.Ref,
					SHA:        inv.Dep.SHA,
					HashAlgo:   inv.Dep.HashAlgo,
					Direct:     inv.Direct,
					RequiredBy: inv.Parents,
				})
			}
			allWorkflows = append(allWorkflows, wf)
		}
		return allWorkflows
	}

	payload := map[string]interface{}{
		"cli_version":      cliVersion(),
		"lockfile_version": lockfileVersion,
	}
	for _, field := range fields {
		field = strings.TrimSpace(field)
		switch field {
		case "valid":
			payload[field] = valid
		case "findings":
			payload[field] = buildFindings()
		case "dependencies":
			payload[field] = buildDeps()
		case "workflows":
			payload[field] = buildWorkflows()
		default:
			return fmt.Errorf("unknown JSON field %q (expected valid, findings, workflows, dependencies)", field)
		}
	}

	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(payload); err != nil {
		return err
	}
	if !valid {
		return errSilent
	}
	return nil
}

// presentCheckResults renders human-readable output from a doctor report.
func presentCheckResults(out *ui.UI, report *doctor.Report, valid bool, willRemediate bool) {
	for _, f := range report.RepoFindings {
		out.Warning("%s", f.Detail)
		if f.DocURL != "" {
			out.Detail("  see: %s", out.Dim(out.Hyperlink("docs", f.DocURL)))
		}
	}

	var validCount, failedCount int
	for _, wr := range report.Workflows {
		if wr.IsValid() {
			validCount++
		} else {
			failedCount++
		}
	}
	checked := validCount + failedCount

	if valid && checked > 0 {
		out.Success("All %d %s valid", checked, ui.Pluralize(checked, "workflow", "workflows"))
	} else if checked > 0 {
		// Collect error findings grouped by dependency.
		type depGroup struct {
			dep      string
			findings []doctor.Finding
		}
		var depOrder []string
		depMap := map[string]*depGroup{}

		for _, wr := range report.Workflows {
			for _, f := range wr.Findings {
				if f.IsValid() {
					continue
				}
				depKey := f.DepKey()
				if dg, ok := depMap[depKey]; ok {
					dg.findings = append(dg.findings, f)
				} else {
					depOrder = append(depOrder, depKey)
					depMap[depKey] = &depGroup{dep: depKey, findings: []doctor.Finding{f}}
				}
			}
		}

		// Count categories; render per-dep detail only for non-NOT_PINNED.
		catCounts := map[doctor.Category]int{}
		for _, dep := range depOrder {
			dg := depMap[dep]

			// Tally and check if this dep is NOT_PINNED only.
			allNotPinned := true
			for _, f := range dg.findings {
				catCounts[f.Category]++
				if f.Category != doctor.CategoryNotPinned {
					allNotPinned = false
				}
			}
			if allNotPinned {
				continue // pure aggregation — no per-dep output
			}

			// Render per-dep detail for actionable categories.
			for _, f := range dg.findings {
				if f.Category == doctor.CategoryNotPinned {
					continue // skip NOT_PINNED lines in mixed groups too
				}
				label := strings.ToUpper(string(f.Category))
				icon := "!"
				if isAlertedCategory(f.Category) {
					icon = "✗"
				}
				out.Detail("%s %s %s", icon, out.Dim(label), dep)
				out.Detail("  %s", f.Detail)
				if f.Category == doctor.CategoryLockfileForgery && f.Dependency != nil {
					owner, repo := f.Dependency.OwnerRepo()
					if owner != "" {
						out.Detail("  → %s", out.Dim(fmt.Sprintf("https://github.com/%s/%s/releases", owner, repo)))
					}
				}
				if isAlertedCategory(f.Category) && f.Remediation != "" {
					out.Detail("  %s %s", out.Bold("⚠"), f.Remediation)
				}
				if f.DocURL != "" {
					out.Detail("  see: %s", out.Dim(out.Hyperlink("docs", f.DocURL)))
				}
			}
		}

		parts := []string{}
		for _, cat := range []doctor.Category{
			doctor.CategoryLockfileForgery,
			doctor.CategoryRefChanged, doctor.CategoryNotPinned,
			doctor.CategoryStale, doctor.CategoryMisleadingSHA, doctor.CategoryImposterCommit,
		} {
			if n, ok := catCounts[cat]; ok {
				parts = append(parts, fmt.Sprintf("%d %s", n, string(cat)))
			}
		}
		out.Error("%d of %d %s failed: %s",
			failedCount, checked,
			ui.Pluralize(checked, "workflow", "workflows"),
			strings.Join(parts, ", "))
		out.Blank()
	}

	// Warnings — deduplicate by dep key.
	type warningGroup struct {
		finding   doctor.Finding
		count     int
		workflows []string
	}
	var warnOrder []string
	warnMap := map[string]*warningGroup{}
	for _, wr := range report.Workflows {
		for _, f := range wr.Findings {
			if f.IsWarning() {
				key := f.DepKey()
				if key == "" {
					key = f.WorkflowPath // workflow-level warnings
				}
				if wg, ok := warnMap[key]; ok {
					wg.count++
					wg.workflows = append(wg.workflows, f.WorkflowPath)
				} else {
					warnOrder = append(warnOrder, key)
					warnMap[key] = &warningGroup{finding: f, count: 1, workflows: []string{f.WorkflowPath}}
				}
			}
		}
		for _, pw := range wr.ParseWarnings {
			out.Warning("%s: %s", wr.Path, pw)
		}
	}

	// Collect workflow-level NOT_PINNED warnings separately for collapsing.
	var unpinnedWorkflows []string
	var otherWarnings []string
	for _, key := range warnOrder {
		wg := warnMap[key]
		f := wg.finding
		if f.Category == doctor.CategoryNotPinned && f.ActionRef == nil {
			unpinnedWorkflows = append(unpinnedWorkflows, f.WorkflowPath)
		} else {
			otherWarnings = append(otherWarnings, key)
		}
	}
	if len(unpinnedWorkflows) > 0 {
		if willRemediate {
			out.Warning("%d %s not yet pinned — resolving below",
				len(unpinnedWorkflows),
				ui.Pluralize(len(unpinnedWorkflows), "workflow", "workflows"))
		} else {
			out.Warning("%d %s not yet pinned (run `gh actions-pin` to fix)",
				len(unpinnedWorkflows),
				ui.Pluralize(len(unpinnedWorkflows), "workflow", "workflows"))
		}
	}
	// Separate SHA_AS_REF warnings into direct (aggregate) and transitive (suppressed).
	// TODO: Transitive deps pinned to bare SHAs are silently swallowed for now.
	// We need to figure out how to coexist better with composite actions that
	// don't use dependency pinning — warning on every transitive dep is noisy
	// and not actionable by the consumer. Revisit when we have a story for
	// composite action authors to adopt pinning.
	// Collect REF_MOVED warnings for compact display.
	var bareSHADeps []string
	var refMovedWarnings []string
	var otherDetailWarnings []string
	for _, key := range otherWarnings {
		wg := warnMap[key]
		f := wg.finding
		if f.Category == doctor.CategorySHAAsRef {
			isTransitive := f.Dependency != nil && f.ActionRef == nil
			if !isTransitive {
				bareSHADeps = append(bareSHADeps, key)
			}
			// transitive SHA_AS_REF: silently swallowed (see TODO above)
		} else if f.Category == doctor.CategoryRefMoved {
			refMovedWarnings = append(refMovedWarnings, key)
		} else if f.Category == doctor.CategoryValid && f.Severity == doctor.SeverityWarning &&
			strings.Contains(f.Remediation, "transitive dependency") {
			// transitive reachability unknown: silently swallowed (see TODO above)
		} else {
			otherDetailWarnings = append(otherDetailWarnings, key)
		}
	}
	if len(bareSHADeps) > 0 {
		out.Warning("%d %s pinned to a bare SHA without a tag ref",
			len(bareSHADeps),
			ui.Pluralize(len(bareSHADeps), "action is", "actions are"))
		if willRemediate {
			out.Detail("  ↳ resolving below")
		} else {
			out.Detail("  ↳ run `gh actions-pin upgrade` to pin to tagged releases")
		}
	}
	if len(refMovedWarnings) > 0 {
		out.Warning("%d %s moved upstream — run `gh actions-pin upgrade` to update",
			len(refMovedWarnings),
			ui.Pluralize(len(refMovedWarnings), "ref has", "refs have"))
		for _, key := range refMovedWarnings {
			wg := warnMap[key]
			f := wg.finding
			out.Detail("  ↳ %s: %s", key, f.Detail)
			if f.Dependency != nil && f.LiveSHA != "" {
				owner, repo := f.Dependency.OwnerRepo()
				if owner != "" {
					out.Detail("    %s", out.Dim(fmt.Sprintf("https://github.com/%s/%s/compare/%s...%s", owner, repo, f.Dependency.SHA[:12], f.LiveSHA[:12])))
				}
			}
		}
	}
	for _, key := range otherDetailWarnings {
		wg := warnMap[key]
		f := wg.finding
		depKey := f.DepKey()
		if f.Category == doctor.CategoryValid && f.Severity == doctor.SeverityWarning {
			label := depKey
			if label == "" {
				label = f.WorkflowPath
			}
			out.Warning("%s: %s", label, f.Detail)
		}
	}
}

// isAlertedCategory reports whether a finding category has no auto-fix and
// requires human investigation (already surfaced in presentCheckResults — the
// remediator should not re-print it in non-interactive mode).
func isAlertedCategory(c doctor.Category) bool {
	switch c {
	case doctor.CategoryImposterCommit, doctor.CategoryLockfileForgery, doctor.CategoryMisleadingSHA:
		return true
	}
	return false
}

// workflowsForDep returns workflow paths whose findings reference the given
// dependency key (ordered as they appear in the report, deduplicated).
func workflowsForDep(report *doctor.Report, depKey string) []string {
	seen := map[string]bool{}
	var out []string
	for _, wr := range report.Workflows {
		for _, f := range wr.Findings {
			if f.DepKey() == depKey && !seen[wr.Path] {
				seen[wr.Path] = true
				out = append(out, wr.Path)
			}
		}
	}
	return out
}

// cliVersion returns the gh-actions-pin extension version embedded by the Go
// build system. Returns "(devel)" for local `go build` and a real version
// like "v0.1.2" when installed via `gh extension install`.
func cliVersion() string {
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" {
		return info.Main.Version
	}
	return "unknown"
}
