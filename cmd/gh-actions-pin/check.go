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

type validationError struct {
	Type              string `json:"type"`
	Dependency        string `json:"dependency"`
	Details           string `json:"details"`
	CompareURL        string `json:"compare_url,omitempty"`
	ReleasesURL       string `json:"releases_url,omitempty"`
	UnreachableDetail string `json:"unreachable_detail,omitempty"`
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

type jsonDependency struct {
	NWO      string `json:"nwo"`
	Ref      string `json:"ref"`
	SHA      string `json:"sha"`
	HashAlgo string `json:"hash_algo,omitempty"`
	File     string `json:"file"`
	Direct   bool   `json:"direct"`
}

type validationResult struct {
	Valid        bool                `json:"valid"`
	Errors       []validationError   `json:"errors"`
	Warnings     []validationWarning `json:"warnings"`
	Dependencies []jsonDependency    `json:"dependencies,omitempty"`
	Workflows    []jsonWorkflow      `json:"workflows,omitempty"`
}

type jsonWorkflow struct {
	Path         string              `json:"path"`
	Valid        bool                `json:"valid"`
	Dependencies []jsonDependency    `json:"dependencies,omitempty"`
	Errors       []validationError   `json:"errors"`
	Warnings     []validationWarning `json:"warnings"`
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
			$ gh actions-pin check --json=valid,errors

			# Diagnostic findings as JSON
			$ gh actions-pin check --json=findings
		`),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) > 0 {
				opts.WorkflowPaths = args
			}
			return runCheck(opts)
		},
	}

	cmd.Flags().StringVar(&opts.JSONFields, "json", "", "Output JSON with the specified `fields` (valid,errors,warnings,dependencies,workflows,findings)")
	cmd.Flags().Lookup("json").NoOptDefVal = "valid,errors,warnings,dependencies,workflows"
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

	// JSON output — always before any human-readable output.
	if opts.JSONFields != "" {
		if opts.JSONFields == "findings" {
			return writeDoctorJSON(report)
		}
		aggregate := reportToValidationResult(report)
		return writeValidationJSON(aggregate, opts.JSONFields)
	}

	// Human-readable output.
	aggregate := reportToValidationResult(report)
	presentCheckResults(aggregate)

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
				fmt.Fprintln(os.Stderr)
				output.Info("Interrupted — no further changes applied")
				return nil
			}
			return err
		}

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
	}

	if !aggregate.Valid {
		return errSilent
	}
	return nil
}

// reportToValidationResult maps a doctor.Report into the JSON-contract types.
func reportToValidationResult(report *doctor.Report) *validationResult {
	aggregate := &validationResult{Valid: true}

	for _, wr := range report.Workflows {
		wfResult := jsonWorkflow{
			Path:  wr.Path,
			Valid: true,
		}

		// Dependency inventory from report.
		for _, inv := range wr.Inventory {
			dep := jsonDependency{
				NWO:      inv.Dep.NWO,
				Ref:      inv.Dep.Ref,
				SHA:      inv.Dep.SHA,
				HashAlgo: inv.Dep.HashAlgo,
				File:     inv.File,
				Direct:   inv.Direct,
			}
			wfResult.Dependencies = append(wfResult.Dependencies, dep)
			aggregate.Dependencies = append(aggregate.Dependencies, dep)
		}

		// Parse warnings.
		for _, pw := range wr.ParseWarnings {
			w := validationWarning{
				Details:      pw,
				WorkflowPath: wr.Path,
			}
			wfResult.Warnings = append(wfResult.Warnings, w)
			aggregate.Warnings = append(aggregate.Warnings, w)
		}

		for _, f := range wr.Findings {
			switch f.Category {
			case doctor.CategoryRunOnly:
				continue
			case doctor.CategoryValid:
				if f.Severity == doctor.SeverityWarning {
					// Reachability unknown or similar informational warning.
					w := validationWarning{
						Details:      f.Detail,
						WorkflowPath: wr.Path,
					}
					if f.Dependency != nil {
						w.Dependency = f.Dependency.Key()
					}
					wfResult.Warnings = append(wfResult.Warnings, w)
					aggregate.Warnings = append(aggregate.Warnings, w)
				}
				continue

			case doctor.CategoryNotPinned:
				if f.ActionRef != nil {
					// Individual missing dep.
					e := validationError{
						Type:       "MISSING",
						Dependency: f.ActionRef.FullName() + "@" + f.ActionRef.Ref,
						Details:    f.Detail,
					}
					wfResult.Valid = false
					wfResult.Errors = append(wfResult.Errors, e)
					aggregate.Errors = append(aggregate.Errors, e)
				} else {
					// Workflow-level "not yet pinned".
					w := validationWarning{
						Details:      "not yet pinned (run `gh actions-pin` interactively to fix)",
						WorkflowPath: wr.Path,
					}
					wfResult.Warnings = append(wfResult.Warnings, w)
					aggregate.Warnings = append(aggregate.Warnings, w)
				}

			case doctor.CategorySHAAsRef:
				depKey := ""
				if f.Dependency != nil {
					depKey = f.Dependency.Key()
				}
				isTransitive := f.Dependency != nil && f.ActionRef == nil
				w := validationWarning{
					Dependency:   depKey,
					Details:      f.Detail,
					WorkflowPath: wr.Path,
					Transitive:   isTransitive,
				}
				wfResult.Warnings = append(wfResult.Warnings, w)
				aggregate.Warnings = append(aggregate.Warnings, w)

			case doctor.CategoryTampered:
				depKey := ""
				var compareURL, releasesURL string
				if f.Dependency != nil {
					depKey = f.Dependency.Key()
					parts := strings.SplitN(f.Dependency.NWO, "/", 3)
					if len(parts) >= 2 {
						baseURL := fmt.Sprintf("https://github.com/%s/%s", parts[0], parts[1])
						// Extract SHAs from the finding detail for compare URL.
						compareURL = baseURL + "/compare/" + f.Dependency.SHA + "..."
						releasesURL = baseURL + "/releases"
					}
				}
				e := validationError{
					Type:        "TAMPERED",
					Dependency:  depKey,
					Details:     f.Detail,
					CompareURL:  compareURL,
					ReleasesURL: releasesURL,
				}
				wfResult.Valid = false
				wfResult.Errors = append(wfResult.Errors, e)
				aggregate.Errors = append(aggregate.Errors, e)

			case doctor.CategoryUnreachable:
				e := validationError{
					Type:    "UNREACHABLE",
					Details: f.Detail,
				}
				if f.Dependency != nil {
					e.Dependency = f.Dependency.Key()
				}
				wfResult.Valid = false
				wfResult.Errors = append(wfResult.Errors, e)
				aggregate.Errors = append(aggregate.Errors, e)

			case doctor.CategorySHAMismatch:
				depKey := ""
				if f.Dependency != nil {
					depKey = f.Dependency.Key()
				}
				e := validationError{
					Type:       "SHA_MISMATCH",
					Dependency: depKey,
					Details:    f.Detail,
				}
				wfResult.Valid = false
				wfResult.Errors = append(wfResult.Errors, e)
				aggregate.Errors = append(aggregate.Errors, e)

			case doctor.CategoryRefChanged:
				depKey := ""
				if f.Dependency != nil {
					depKey = f.Dependency.Key()
				}
				e := validationError{
					Type:       "REF_CHANGED",
					Dependency: depKey,
					Details:    f.Detail,
				}
				wfResult.Valid = false
				wfResult.Errors = append(wfResult.Errors, e)
				aggregate.Errors = append(aggregate.Errors, e)

			case doctor.CategoryStale:
				depKey := ""
				if f.Dependency != nil {
					depKey = f.Dependency.Key()
				}
				e := validationError{
					Type:       "STALE",
					Dependency: depKey,
					Details:    f.Detail,
				}
				wfResult.Valid = false
				wfResult.Errors = append(wfResult.Errors, e)
				aggregate.Errors = append(aggregate.Errors, e)
			}
		}

		if !wfResult.Valid {
			aggregate.Valid = false
		}
		// Ensure non-nil slices for JSON output.
		if wfResult.Errors == nil {
			wfResult.Errors = []validationError{}
		}
		if wfResult.Warnings == nil {
			wfResult.Warnings = []validationWarning{}
		}
		aggregate.Workflows = append(aggregate.Workflows, wfResult)
	}

	return aggregate
}

// presentCheckResults renders human-readable output from validation results.
func presentCheckResults(aggregate *validationResult) {
	// Count workflows with errors vs valid.
	var valid, failed int
	for _, wf := range aggregate.Workflows {
		if wf.Valid {
			valid++
		} else {
			failed++
		}
	}
	checked := valid + failed

	if aggregate.Valid && checked > 0 {
		output.Success("All %d %s valid", checked, ui.Pluralize(checked, "workflow", "workflows"))
	} else if checked > 0 {
		// Group errors by dependency to merge related findings.
		type depFindings struct {
			dep    string
			errors []validationError
		}
		var depOrder []string
		depMap := map[string]*depFindings{}
		for _, e := range aggregate.Errors {
			if df, ok := depMap[e.Dependency]; ok {
				df.errors = append(df.errors, e)
			} else {
				depOrder = append(depOrder, e.Dependency)
				depMap[e.Dependency] = &depFindings{dep: e.Dependency, errors: []validationError{e}}
			}
		}

		// Merge TAMPERED+UNREACHABLE: fold unreachable detail into tampered.
		typeCounts := map[string]int{}
		for _, dep := range depOrder {
			df := depMap[dep]
			hasTampered := false
			for _, e := range df.errors {
				if e.Type == "TAMPERED" {
					hasTampered = true
					break
				}
			}
			if hasTampered {
				var merged []validationError
				var unreachableDetail string
				for _, e := range df.errors {
					if e.Type == "UNREACHABLE" {
						unreachableDetail = e.Details
						continue
					}
					if e.Type == "TAMPERED" && unreachableDetail == "" {
						for _, e2 := range df.errors {
							if e2.Type == "UNREACHABLE" {
								unreachableDetail = e2.Details
								break
							}
						}
					}
					me := e
					if me.Type == "TAMPERED" && unreachableDetail != "" {
						me.UnreachableDetail = unreachableDetail
					}
					merged = append(merged, me)
				}
				df.errors = merged
			}
			for _, e := range df.errors {
				typeCounts[e.Type]++
			}
		}

		parts := []string{}
		for _, t := range []string{"TAMPERED", "REF_CHANGED", "MISSING", "STALE", "SHA_MISMATCH", "UNREACHABLE", "ERROR"} {
			if n, ok := typeCounts[t]; ok {
				parts = append(parts, fmt.Sprintf("%d %s", n, strings.ToLower(t)))
			}
		}
		output.Error("%d of %d %s failed: %s",
			failed, checked,
			ui.Pluralize(checked, "workflow", "workflows"),
			strings.Join(parts, ", "))
		fmt.Fprintln(os.Stderr)

		for i, dep := range depOrder {
			df := depMap[dep]
			for _, e := range df.errors {
				fmt.Fprintf(os.Stderr, "  ! %s %s\n", output.Dim(e.Type), e.Dependency)
				fmt.Fprintf(os.Stderr, "    %s\n", e.Details)
				if e.UnreachableDetail != "" {
					fmt.Fprintf(os.Stderr, "    %s\n", e.UnreachableDetail)
				}
				if e.CompareURL != "" {
					fmt.Fprintf(os.Stderr, "    → %s\n", output.Dim(e.CompareURL))
				}
				if e.ReleasesURL != "" {
					fmt.Fprintf(os.Stderr, "    → %s\n", output.Dim(e.ReleasesURL))
				}
			}
			if i < len(depOrder)-1 {
				fmt.Fprintln(os.Stderr)
			}
		}
		fmt.Fprintln(os.Stderr)
	}

	// Warnings.
	if len(aggregate.Warnings) > 0 {
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
			w := g.warning
			if strings.Contains(w.Details, "not yet pinned") {
				output.Warning("%s: %s", w.WorkflowPath, w.Details)
			} else if w.Transitive {
				nwo := w.Dependency
				if idx := strings.Index(nwo, "@"); idx > 0 {
					nwo = nwo[:idx]
				}
				nwoParts := strings.SplitN(nwo, "/", 3)
				repoNWO := nwo
				if len(nwoParts) >= 2 {
					repoNWO = nwoParts[0] + "/" + nwoParts[1]
				}
				output.Warning("%s: transitive dependency pinned to a bare SHA — reachability cannot be verified", w.Dependency)
				output.Detail("  ↳ this comes from a composite action's internal dependency")
				output.Detail("  ↳ ask the maintainer of %s to onboard to dependency pinning", output.Bold(repoNWO))
			} else if w.Dependency != "" {
				nwo := w.Dependency
				if idx := strings.Index(nwo, "@"); idx > 0 {
					nwo = nwo[:idx]
				}
				nwoParts := strings.SplitN(nwo, "/", 3)
				repoNWO := nwo
				if len(nwoParts) >= 2 {
					repoNWO = nwoParts[0] + "/" + nwoParts[1]
				}
				output.Warning("%s: %s", w.Dependency, w.Details)
				output.Detail("  ↳ releases: https://github.com/%s/releases", repoNWO)
			} else {
				output.Warning("%s", w.Details)
			}
			for _, f := range g.files {
				output.Detail("  in %s", f)
			}
		}
	}
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
		case "dependencies":
			payload[field] = result.Dependencies
		case "workflows":
			payload[field] = result.Workflows
		default:
			return fmt.Errorf("unknown JSON field %q (expected valid, errors, warnings, dependencies, workflows)", field)
		}
	}

	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(payload)
}

func isTerminal() bool {
	return term.IsTerminal(int(os.Stderr.Fd()))
}

func writeDoctorJSON(report *doctor.Report) error {
	type jsonFinding struct {
		Workflow    string `json:"workflow"`
		Category    string `json:"category"`
		Severity    string `json:"severity"`
		Dependency  string `json:"dependency,omitempty"`
		Detail      string `json:"detail"`
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
