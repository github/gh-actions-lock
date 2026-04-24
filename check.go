package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/MakeNowJust/heredoc"
	"github.com/github/gh-actions-pin/internal/doctor"
	"github.com/github/gh-actions-pin/internal/lockfile"
	"github.com/github/gh-actions-pin/internal/resolver"
	"github.com/github/gh-actions-pin/internal/ui"
	"github.com/spf13/cobra"
)

type checkOptions struct {
	WorkflowPaths []string
	JSONFields    string
	Hostname      string
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

type validationResult struct {
	Valid    bool                `json:"valid"`
	Errors  []validationError   `json:"errors"`
	Warnings []validationWarning `json:"warnings"`
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
		// Group errors by dependency so we can merge related findings
		// (e.g. TAMPERED + UNREACHABLE for the same dep).
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

		// Merge TAMPERED+UNREACHABLE: when both exist for a dep, fold
		// the unreachable detail into the tampered entry.
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
				// Absorb UNREACHABLE into TAMPERED — keep unreachable
				// detail as supplementary info but don't double-count.
				var merged []validationError
				var unreachableDetail string
				for _, e := range df.errors {
					if e.Type == "UNREACHABLE" && hasTampered {
						unreachableDetail = e.Details
						continue
					}
					if e.Type == "TAMPERED" && unreachableDetail == "" {
						// Haven't seen UNREACHABLE yet; scan ahead.
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
		for _, t := range []string{"TAMPERED", "MISSING", "STALE", "SHA_MISMATCH", "UNREACHABLE", "ERROR"} {
			if n, ok := typeCounts[t]; ok {
				parts = append(parts, fmt.Sprintf("%d %s", n, strings.ToLower(t)))
			}
		}
		output.Error("%d of %d %s failed: %s",
			failed, checked,
			ui.Pluralize(checked, "workflow", "workflows"),
			strings.Join(parts, ", "))
		fmt.Fprintln(os.Stderr)

		// Show grouped errors with doctor-style formatting.
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
			w := g.warning
			if w.Transitive {
				// Extract owner/repo from dep key for a cleaner message.
				nwo := w.Dependency
				if idx := strings.Index(nwo, "@"); idx > 0 {
					nwo = nwo[:idx]
				}
				// Strip sub-path from NWO (e.g. owner/repo/subpath → owner/repo).
				nwoParts := strings.SplitN(nwo, "/", 3)
				repoNWO := nwo
				if len(nwoParts) >= 2 {
					repoNWO = nwoParts[0] + "/" + nwoParts[1]
				}
				output.Warning("%s: transitive dependency pinned to a bare SHA — reachability cannot be verified", w.Dependency)
				output.Detail("  ↳ this comes from a composite action's internal dependency")
				output.Detail("  ↳ ask the maintainer of %s to onboard to dependency pinning", output.Bold(repoNWO))
			} else {
				output.Warning("%s", w.String())
			}
			for _, f := range g.files {
				output.Detail("  in %s", f)
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
