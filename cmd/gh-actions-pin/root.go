package main

import (
	"errors"
	"fmt"
	"os"

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

func NewRootCmd() *cobra.Command {
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
under .github/workflows/. When run interactively, it offers to fix
issues it finds.

Use subcommands to manage your workflow's dependencies:

  gh actions-pin             Verify the lock section, fix interactively
  gh actions-pin upgrade     Bump action refs and repin them
`),
		Example: heredoc.Doc(`
# Verify all workflows
$ gh actions-pin

# Verify a specific workflow
$ gh actions-pin .github/workflows/ci.yml

# Output JSON for CI
$ gh actions-pin --json valid,errors

# Auto-pin unpinned workflows
$ gh actions-pin --write

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

	cmd.Flags().StringVar(&opts.JSONFields, "json", "", "Output JSON with the specified `fields` (valid,errors,warnings,dependencies,workflows,findings)")
	cmd.Flags().StringVar(&opts.Hostname, "hostname", "", "GitHub hostname to query (defaults to GH_HOST, current repo host, or github.com)")
	cmd.Flags().BoolVar(&opts.NoInteractive, "no-interactive", false, "Report-only mode (no prompts, no changes)")
	cmd.Flags().BoolVar(&opts.Write, "write", false, "Auto-apply safe fixes (unpinned workflows only)")
	cmd.AddCommand(newCheckCmd())
	cmd.AddCommand(newUpgradeCmd())

	return cmd
}

// Execute runs the root command and returns an exit code.
func Execute() int {
	if err := NewRootCmd().Execute(); err != nil {
		if !errors.Is(err, errSilent) {
			output.Error("%s", err)
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
