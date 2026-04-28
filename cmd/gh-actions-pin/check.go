package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/MakeNowJust/heredoc"
	"github.com/cli/go-gh/v2/pkg/api"
	"github.com/cli/go-gh/v2/pkg/repository"
	"github.com/github/gh-actions-pin/internal/doctor"
	"github.com/github/gh-actions-pin/internal/ui"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

type checkOptions struct {
	WorkflowPaths []string
	JSONFields    string
	Hostname      string
	NoInteractive bool
	Write         bool
}

// JSON output types — thin wrappers around doctor.Report.

type checkFinding struct {
	Workflow    string `json:"workflow"`
	Category    string `json:"category"`
	Severity    string `json:"severity"`
	Dependency  string `json:"dependency,omitempty"`
	Detail      string `json:"detail"`
	Remediation string `json:"remediation,omitempty"`
}

type checkDependency struct {
	NWO      string `json:"nwo"`
	Ref      string `json:"ref"`
	SHA      string `json:"sha"`
	HashAlgo string `json:"hash_algo,omitempty"`
	Direct   bool   `json:"direct"`
}

type checkWorkflow struct {
	Path         string            `json:"path"`
	Valid        bool              `json:"valid"`
	Findings     []checkFinding    `json:"findings"`
	Dependencies []checkDependency `json:"dependencies,omitempty"`
}

func newCheckCmd() *cobra.Command {
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

			When run interactively (TTY), offers to fix issues inline. Use
			--no-interactive for report-only mode, or --accept-all to auto-fix
			everything without prompting.

			With --json, structured results go to stdout and progress to stderr:

			  gh actions-pin check --json 2>/dev/null | jq .valid

			Issue types:
			  TAMPERED      - locked SHA no longer matches upstream
			  MISSING       - action in workflow has no lock entry
			  STALE         - lock entry references an action no longer in the workflow
			  REF_CHANGED   - workflow ref was edited; lock needs updating
			  SHA_MISMATCH  - ref looks like a SHA but resolves to a different commit
			  UNREACHABLE   - locked SHA is not in the ref's history
		`),
		Example: heredoc.Doc(`
			# Verify all workflows
			$ gh actions-pin check

			# Verify a specific workflow
			$ gh actions-pin check .github/workflows/ci.yml

			# Auto-fix all issues without prompting
			$ gh actions-pin check --accept-all

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
			return runCheck(opts)
		},
	}

	cmd.Flags().StringVar(&opts.JSONFields, "json", "", "Output JSON with the specified `fields` (valid,findings,workflows,dependencies)")
	cmd.Flags().Lookup("json").NoOptDefVal = "valid,findings,workflows,dependencies"
	cmd.Flags().StringVar(&opts.Hostname, "hostname", "", "GitHub hostname to query (defaults to GH_HOST, current repo host, or github.com)")
	cmd.Flags().BoolVar(&opts.NoInteractive, "no-interactive", false, "Report-only mode (no prompts, no changes)")
	cmd.Flags().BoolVar(&opts.Write, "accept-all", false, "Auto-apply all safe fixes without prompting")
	return cmd
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

	// Single pass: doctor.Diagnose handles all validation.
	total := len(opts.WorkflowPaths)
	if opts.JSONFields == "" && total > 1 {
		output.StartProgress(fmt.Sprintf("Checking %d %s", total, ui.Pluralize(total, "workflow", "workflows")))
	}

	report := doctor.Diagnose(opts.WorkflowPaths, r)

	output.StopProgress()

	// Compute validity from findings.
	valid := reportIsValid(report)

	// JSON output — always before any human-readable output.
	if opts.JSONFields != "" {
		return writeCheckJSON(report, valid, opts.JSONFields)
	}

	// Human-readable output.
	presentCheckResults(report, valid)

	// Remediation.
	interactive := !opts.NoInteractive && os.Getenv("CI") != "true" && isTerminal()
	willRemediate := interactive || opts.Write
	actionable := report.WorkflowsNeedingAttention()

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
			prompter = doctor.NewHuhPrompter()
		}

		var repoOwner string
		if currentRepo, err := repository.Current(); err == nil {
			repoOwner = currentRepo.Owner
		}

		rem := doctor.NewRemediator(prompter, r, restClient, output, doctor.RemediateOptions{
			Write:       opts.Write,
			Interactive: interactive,
			RepoOwner:   repoOwner,
		})

		if err := rem.Remediate(report); err != nil {
			if errors.Is(err, doctor.ErrAborted) {
				output.Blank()
				output.Info("Interrupted — no further changes applied")
				return nil
			}
			return err
		}

		output.Blank()
		if rem.Fixed > 0 {
			output.Success("%d %s fixed", rem.Fixed, ui.Pluralize(rem.Fixed, "issue", "issues"))
		}
		if rem.Skipped > 0 {
			output.Skip("%d %s skipped", rem.Skipped, ui.Pluralize(rem.Skipped, "issue", "issues"))
		}
		if rem.Alerted > 0 {
			output.Warning("%d %s need manual attention", rem.Alerted, ui.Pluralize(rem.Alerted, "issue", "issues"))
		}
	}

	if !valid {
		return errSilent
	}
	return nil
}

// reportIsValid returns true if the lockfile is in sync with all workflows.
// Findings that represent integrity violations make it false.
func reportIsValid(report *doctor.Report) bool {
	for _, wr := range report.Workflows {
		if !workflowIsValid(&wr) {
			return false
		}
	}
	return true
}

func workflowIsValid(wr *doctor.WorkflowReport) bool {
	for _, f := range wr.Findings {
		switch f.Category {
		case doctor.CategoryValid, doctor.CategoryRunOnly, doctor.CategorySHAAsRef:
			continue
		case doctor.CategoryNotPinned:
			// Workflow-level "not pinned" is a warning, not a failure.
			// Individual missing dep (has ActionRef) is a failure.
			if f.ActionRef != nil {
				return false
			}
		default:
			// tampered, unreachable, sha_mismatch, ref_changed, stale
			return false
		}
	}
	return true
}

// findingToJSON converts a doctor.Finding to a JSON-safe struct.
func findingToJSON(f doctor.Finding) checkFinding {
	jf := checkFinding{
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
	return jf
}

// writeCheckJSON writes the unified JSON output.
func writeCheckJSON(report *doctor.Report, valid bool, fieldsCSV string) error {
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
		for _, wr := range report.Workflows {
			directNWOs := make(map[string]bool)
			for _, ref := range wr.ActionRefs {
				directNWOs[ref.FullName()] = true
			}
			for _, inv := range wr.Inventory {
				allDeps = append(allDeps, checkDependency{
					NWO:      inv.Dep.NWO,
					Ref:      inv.Dep.Ref,
					SHA:      inv.Dep.SHA,
					HashAlgo: inv.Dep.HashAlgo,
					Direct:   inv.Direct,
				})
			}
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
				Valid:    workflowIsValid(&wr),
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
					NWO:      inv.Dep.NWO,
					Ref:      inv.Dep.Ref,
					SHA:      inv.Dep.SHA,
					HashAlgo: inv.Dep.HashAlgo,
					Direct:   inv.Direct,
				})
			}
			allWorkflows = append(allWorkflows, wf)
		}
		return allWorkflows
	}

	payload := map[string]interface{}{}
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

	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(payload)
}

// presentCheckResults renders human-readable output from a doctor report.
func presentCheckResults(report *doctor.Report, valid bool) {
	var validCount, failedCount int
	for _, wr := range report.Workflows {
		if workflowIsValid(&wr) {
			validCount++
		} else {
			failedCount++
		}
	}
	checked := validCount + failedCount

	if valid && checked > 0 {
		output.Success("All %d %s valid", checked, ui.Pluralize(checked, "workflow", "workflows"))
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
				if isValidFinding(f) {
					continue
				}
				depKey := findingDepKey(f)
				if dg, ok := depMap[depKey]; ok {
					dg.findings = append(dg.findings, f)
				} else {
					depOrder = append(depOrder, depKey)
					depMap[depKey] = &depGroup{dep: depKey, findings: []doctor.Finding{f}}
				}
			}
		}

		// Merge TAMPERED+UNREACHABLE for same dep.
		catCounts := map[doctor.Category]int{}
		for _, dep := range depOrder {
			dg := depMap[dep]
			hasTampered := false
			var unreachableDetail string
			for _, f := range dg.findings {
				if f.Category == doctor.CategoryTampered {
					hasTampered = true
				}
				if f.Category == doctor.CategoryUnreachable {
					unreachableDetail = f.Detail
				}
			}
			if hasTampered && unreachableDetail != "" {
				// Remove standalone unreachable, fold into tampered.
				var merged []doctor.Finding
				for _, f := range dg.findings {
					if f.Category == doctor.CategoryUnreachable {
						continue
					}
					merged = append(merged, f)
				}
				dg.findings = merged
			}
			for _, f := range dg.findings {
				catCounts[f.Category]++
			}
			// Print.
			for _, f := range dg.findings {
				cat := strings.ToUpper(string(f.Category))
				output.Detail("! %s %s", output.Dim(cat), dep)
				output.Detail("  %s", f.Detail)
				if hasTampered && unreachableDetail != "" && f.Category == doctor.CategoryTampered {
					output.Detail("  %s", unreachableDetail)
				}
				if f.Dependency != nil && f.Category == doctor.CategoryTampered {
					parts := strings.SplitN(f.Dependency.NWO, "/", 3)
					if len(parts) >= 2 {
						output.Detail("  → %s", output.Dim(fmt.Sprintf("https://github.com/%s/%s/compare/%s...", parts[0], parts[1], f.Dependency.SHA)))
						output.Detail("  → %s", output.Dim(fmt.Sprintf("https://github.com/%s/%s/releases", parts[0], parts[1])))
					}
				}
			}
		}

		parts := []string{}
		for _, cat := range []doctor.Category{
			doctor.CategoryTampered, doctor.CategoryRefChanged, doctor.CategoryNotPinned,
			doctor.CategoryStale, doctor.CategorySHAMismatch, doctor.CategoryUnreachable,
		} {
			if n, ok := catCounts[cat]; ok {
				parts = append(parts, fmt.Sprintf("%d %s", n, string(cat)))
			}
		}
		output.Error("%d of %d %s failed: %s",
			failedCount, checked,
			ui.Pluralize(checked, "workflow", "workflows"),
			strings.Join(parts, ", "))
		output.Blank()
	}

	// Warnings.
	var warnings []doctor.Finding
	for _, wr := range report.Workflows {
		for _, f := range wr.Findings {
			if isWarningFinding(f) {
				warnings = append(warnings, f)
			}
		}
		for _, pw := range wr.ParseWarnings {
			output.Warning("%s: %s", wr.Path, pw)
		}
	}

	for _, f := range warnings {
		depKey := findingDepKey(f)
		switch {
		case f.Category == doctor.CategoryNotPinned && f.ActionRef == nil:
			output.Warning("%s: not yet pinned (run `gh actions-pin` to fix)", f.WorkflowPath)
		case f.Category == doctor.CategorySHAAsRef:
			isTransitive := f.Dependency != nil && f.ActionRef == nil
			repoNWO := extractRepoNWO(depKey)
			if isTransitive {
				output.Warning("%s: transitive dependency pinned to a bare SHA — reachability cannot be verified", depKey)
				output.Detail("  ↳ this comes from a composite action's internal dependency")
				output.Detail("  ↳ ask the maintainer of %s to onboard to dependency pinning", output.Bold(repoNWO))
			} else {
				output.Warning("%s: %s", depKey, f.Detail)
				output.Detail("  ↳ releases: https://github.com/%s/releases", repoNWO)
			}
		case f.Category == doctor.CategoryValid && f.Severity == doctor.SeverityWarning:
			output.Warning("%s: %s", depKey, f.Detail)
		}
	}
}

// isValidFinding returns true for findings that don't represent integrity violations.
func isValidFinding(f doctor.Finding) bool {
	switch f.Category {
	case doctor.CategoryValid, doctor.CategoryRunOnly, doctor.CategorySHAAsRef:
		return true
	case doctor.CategoryNotPinned:
		return f.ActionRef == nil // workflow-level is a warning
	default:
		return false
	}
}

// isWarningFinding returns true for findings that should render as warnings.
func isWarningFinding(f doctor.Finding) bool {
	switch {
	case f.Category == doctor.CategorySHAAsRef:
		return true
	case f.Category == doctor.CategoryValid && f.Severity == doctor.SeverityWarning:
		return true
	case f.Category == doctor.CategoryNotPinned && f.ActionRef == nil:
		return true
	default:
		return false
	}
}

// findingDepKey returns a dependency identifier for display.
func findingDepKey(f doctor.Finding) string {
	if f.Dependency != nil {
		return f.Dependency.Key()
	}
	if f.ActionRef != nil {
		return f.ActionRef.FullName() + "@" + f.ActionRef.Ref
	}
	return ""
}

// extractRepoNWO strips sub-path and ref from a dep key like "owner/repo/sub@ref".
func extractRepoNWO(depKey string) string {
	nwo := depKey
	if idx := strings.Index(nwo, "@"); idx > 0 {
		nwo = nwo[:idx]
	}
	parts := strings.SplitN(nwo, "/", 3)
	if len(parts) >= 2 {
		return parts[0] + "/" + parts[1]
	}
	return nwo
}

func isTerminal() bool {
	return term.IsTerminal(int(os.Stderr.Fd()))
}
