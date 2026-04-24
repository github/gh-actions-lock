package main

import (
"encoding/json"
"errors"
"fmt"
"os"

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
