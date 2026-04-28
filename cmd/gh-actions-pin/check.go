package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/MakeNowJust/heredoc"
	"github.com/cli/go-gh/v2/pkg/api"
	"github.com/cli/go-gh/v2/pkg/repository"
	"github.com/github/gh-actions-pin/internal/doctor"
	"github.com/github/gh-actions-pin/internal/ui"
	"github.com/spf13/cobra"
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
			return runCheck(f, opts)
		},
	}

	cmd.Flags().StringVar(&opts.JSONFields, "json", "", "Output JSON with the specified `fields` (valid,findings,workflows,dependencies)")
	cmd.Flags().Lookup("json").NoOptDefVal = "valid,findings,workflows,dependencies"
	cmd.Flags().StringVar(&opts.Hostname, "hostname", "", "GitHub hostname to query (defaults to GH_HOST, current repo host, or github.com)")
	cmd.Flags().BoolVar(&opts.NoInteractive, "no-interactive", false, "Report-only mode (no prompts, no changes)")
	cmd.Flags().BoolVar(&opts.Write, "accept-all", false, "Auto-apply all safe fixes without prompting")
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

	// Single pass: doctor.Diagnose handles all validation.
	total := len(opts.WorkflowPaths)
	if opts.JSONFields == "" && total > 1 {
		f.UI.StartProgress(fmt.Sprintf("Checking %d %s", total, ui.Pluralize(total, "workflow", "workflows")))
	}

	report := doctor.Diagnose(opts.WorkflowPaths, r)

	f.UI.StopProgress()

	// Compute validity from findings.
	valid := report.IsValid()

	// JSON output — always before any human-readable output.
	if opts.JSONFields != "" {
		return writeCheckJSON(f.Out, report, valid, opts.JSONFields)
	}

	// Human-readable output.
	presentCheckResults(f.UI, report, valid)

	// Remediation.
	interactive := !opts.NoInteractive && os.Getenv("CI") != "true" && f.IsTerminal()
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
			prompter = doctor.NewHuhPrompterWithWriter(f.ErrOut, f.IsTerminal)
		}

		var repoOwner string
		if currentRepo, err := repository.Current(); err == nil {
			repoOwner = currentRepo.Owner
		}

		rem := doctor.NewRemediator(prompter, r, restClient, f.UI, doctor.RemediateOptions{
			Write:       opts.Write,
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

		f.UI.Blank()
		if rem.Fixed > 0 {
			f.UI.Success("%d %s fixed", rem.Fixed, ui.Pluralize(rem.Fixed, "issue", "issues"))
		}
		if rem.Skipped > 0 {
			f.UI.Skip("%d %s skipped", rem.Skipped, ui.Pluralize(rem.Skipped, "issue", "issues"))
		}
		if rem.Alerted > 0 {
			f.UI.Warning("%d %s need manual attention", rem.Alerted, ui.Pluralize(rem.Alerted, "issue", "issues"))
		}
	}

	if !valid {
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
	}
	if f.Dependency != nil {
		jf.Dependency = f.Dependency.Key()
	} else if f.ActionRef != nil {
		jf.Dependency = f.ActionRef.FullName() + "@" + f.ActionRef.Ref
	}
	return jf
}

// writeCheckJSON writes the unified JSON output.
func writeCheckJSON(w io.Writer, report *doctor.Report, valid bool, fieldsCSV string) error {
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

	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(payload)
}

// presentCheckResults renders human-readable output from a doctor report.
func presentCheckResults(out *ui.UI, report *doctor.Report, valid bool) {
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
			// Deduplicate findings by category within each dep group.
			// For repeated identical categories (e.g. NOT_PINNED across many workflows),
			// show once with a count of affected files.
			type catGroup struct {
				category doctor.Category
				files    []string
				sample   doctor.Finding
			}
			var catOrder []doctor.Category
			catMap := map[doctor.Category]*catGroup{}
			for _, f := range dg.findings {
				if cg, ok := catMap[f.Category]; ok {
					if f.WorkflowPath != "" {
						cg.files = append(cg.files, f.WorkflowPath)
					}
				} else {
					catOrder = append(catOrder, f.Category)
					files := []string{}
					if f.WorkflowPath != "" {
						files = append(files, f.WorkflowPath)
					}
					catMap[f.Category] = &catGroup{category: f.Category, files: files, sample: f}
				}
			}
			for _, cat := range catOrder {
				cg := catMap[cat]
				f := cg.sample
				label := strings.ToUpper(string(cat))
				if len(cg.files) > 1 {
					out.Detail("! %s %s (%d %s)",
						out.Dim(label), dep,
						len(cg.files), ui.Pluralize(len(cg.files), "file", "files"))
				} else {
					out.Detail("! %s %s", out.Dim(label), dep)
				}
				out.Detail("  %s", f.Detail)
				if hasTampered && unreachableDetail != "" && cat == doctor.CategoryTampered {
					out.Detail("  %s", unreachableDetail)
				}
				if f.Dependency != nil && cat == doctor.CategoryTampered {
					parts := strings.SplitN(f.Dependency.NWO, "/", 3)
					if len(parts) >= 2 {
						out.Detail("  → %s", out.Dim(fmt.Sprintf("https://github.com/%s/%s/compare/%s...", parts[0], parts[1], f.Dependency.SHA)))
						out.Detail("  → %s", out.Dim(fmt.Sprintf("https://github.com/%s/%s/releases", parts[0], parts[1])))
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
		out.Error("%d of %d %s failed: %s",
			failedCount, checked,
			ui.Pluralize(checked, "workflow", "workflows"),
			strings.Join(parts, ", "))
		out.Blank()
	}

	// Warnings — deduplicate by dep key.
	type warningGroup struct {
		finding doctor.Finding
		count   int
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
				} else {
					warnOrder = append(warnOrder, key)
					warnMap[key] = &warningGroup{finding: f, count: 1}
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
		out.Warning("%d %s not yet pinned (run `gh actions-pin` to fix)",
			len(unpinnedWorkflows),
			ui.Pluralize(len(unpinnedWorkflows), "workflow", "workflows"))
	}
	for _, key := range otherWarnings {
		wg := warnMap[key]
		f := wg.finding
		depKey := f.DepKey()
		switch {
		case f.Category == doctor.CategorySHAAsRef:
			isTransitive := f.Dependency != nil && f.ActionRef == nil
			repoNWO := extractRepoNWO(depKey)
			if isTransitive {
				out.Warning("%s: transitive dependency pinned to a bare SHA — reachability cannot be verified", depKey)
				out.Detail("  ↳ this comes from a composite action's internal dependency")
				out.Detail("  ↳ ask the maintainer of %s to onboard to dependency pinning", out.Bold(repoNWO))
			} else {
				out.Warning("%s: %s", depKey, f.Detail)
				out.Detail("  ↳ releases: https://github.com/%s/releases", repoNWO)
			}
		case f.Category == doctor.CategoryValid && f.Severity == doctor.SeverityWarning:
			out.Warning("%s: %s", depKey, f.Detail)
		}
	}
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
