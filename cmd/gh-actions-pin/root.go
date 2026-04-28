package main

import (
"errors"
"fmt"
"io"
"os"

"github.com/MakeNowJust/heredoc"
"github.com/cli/go-gh/v2/pkg/repository"
"github.com/github/gh-actions-pin/internal/lockfile"
"github.com/github/gh-actions-pin/internal/resolver"
"github.com/github/gh-actions-pin/internal/ui"
"github.com/spf13/cobra"
"golang.org/x/term"
)

var errSilent = errors.New("silent error")

// pinFactory provides dependency injection for all commands. When integrating
// into cli/cli, swap this for cmdutil.Factory — the interface is analogous.
type pinFactory struct {
// Out is the writer for structured output (JSON). Typically os.Stdout.
Out io.Writer
// ErrOut is the writer for human-readable output (progress, errors). Typically os.Stderr.
ErrOut io.Writer
// UI provides formatted terminal output via ErrOut.
UI *ui.UI
// NewResolver creates a resolver for the given hostname.
NewResolver func(hostname string) (*resolver.Resolver, error)
// IsTerminal reports whether ErrOut is a TTY.
IsTerminal func() bool
}

// NewDefaultFactory creates a factory wired to real stdio.
func NewDefaultFactory() *pinFactory {
return &pinFactory{
Out:         os.Stdout,
ErrOut:      os.Stderr,
UI:          ui.New(),
NewResolver: resolver.New,
IsTerminal: func() bool {
return term.IsTerminal(int(os.Stderr.Fd()))
},
}
}

func NewRootCmd(f *pinFactory) *cobra.Command {
opts := &checkOptions{}

cmd := &cobra.Command{
Use:           "actions-pin [<workflow-path>...]",
Args:          cobra.ArbitraryArgs,
Short:         "Lock and verify GitHub Actions dependencies",
SilenceErrors: true,
SilenceUsage:  true,
Long: heredoc.Doc(`
Lock and verify GitHub Actions dependencies to protect your workflows
from supply chain attacks.

Actions are resolved by mutable tags and branches at runtime. This
extension pins every direct and transitive dependency to an immutable
commit SHA in an inline dependencies: section, so changes are visible
in pull request diffs and tampered or hijacked actions are caught
before they run.

Scans all workflows under .github/workflows/ by default. When run
interactively it offers to fix any issues it finds.

With --json, structured results go to stdout and progress to stderr:

  gh actions-pin --json 2>/dev/null | jq .valid

Commands:

  gh actions-pin             Verify and fix the dependency lock
  gh actions-pin upgrade     Bump action versions and re-lock
`),
Example: heredoc.Doc(`
# Verify all workflows
$ gh actions-pin

# Verify a specific workflow
$ gh actions-pin .github/workflows/ci.yml

# Output JSON for CI integration
$ gh actions-pin --json=valid,errors

# Auto-fix all issues without prompting
$ gh actions-pin --accept-all

# Upgrade a specific action
$ gh actions-pin upgrade --action actions/checkout
`),
RunE: func(cmd *cobra.Command, args []string) error {
if len(args) > 0 {
opts.WorkflowPaths = args
}
return runCheck(f, opts)
},
}

cmd.Flags().StringVar(&opts.JSONFields, "json", "", "Output JSON with the specified `fields` (valid,errors,warnings,dependencies,workflows,findings)")
cmd.Flags().Lookup("json").NoOptDefVal = "valid,findings,workflows,dependencies"
cmd.Flags().StringVar(&opts.Hostname, "hostname", "", "GitHub hostname to query (defaults to GH_HOST, current repo host, or github.com)")
cmd.Flags().BoolVar(&opts.NoInteractive, "no-interactive", false, "Report-only mode (no prompts, no changes)")
cmd.Flags().BoolVar(&opts.Write, "accept-all", false, "Auto-apply all safe fixes without prompting")
cmd.AddCommand(newCheckCmd(f))
cmd.AddCommand(newUpgradeCmd(f))

return cmd
}

// Execute runs the root command and returns an exit code.
func Execute() int {
f := NewDefaultFactory()
if err := NewRootCmd(f).Execute(); err != nil {
if !errors.Is(err, errSilent) {
f.UI.Error("%s", err)
}
return 1
}
return 0
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
