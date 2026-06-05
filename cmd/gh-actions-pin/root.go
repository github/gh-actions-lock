package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"syscall"

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

// NewRootCmd returns the cobra command for the root `actions-pin` invocation.
// f supplies the runtime factory (HTTP transport, terminal detection, TTY hints)
// so tests can swap in fakes.
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
commit SHA in a per-repo lockfile at .github/workflows/actions.lock,
so changes are visible in pull request diffs and tampered or hijacked
actions are caught before they run.

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
$ gh actions-pin --json=valid,findings

# Upgrade a specific action
$ gh actions-pin upgrade --action actions/checkout
`),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) > 0 {
				opts.WorkflowPaths = args
			}
			return runCheck(cmd.Context(), f, opts)
		},
	}

	cmd.Flags().StringVar(&opts.JSONFields, "json", "", "Output JSON with the specified `fields` (valid,findings,workflows,dependencies)")
	cmd.Flags().Lookup("json").NoOptDefVal = "valid,findings,workflows"
	cmd.Flags().StringVar(&opts.Hostname, "hostname", "", "GitHub hostname to query (defaults to GH_HOST, current repo host, or github.com)")
	cmd.Flags().BoolVar(&opts.NoInteractive, "no-interactive", false, "Auto-fix deterministic issues; fail on issues requiring human input")
	cmd.Flags().BoolVar(&opts.Rescan, "rescan", false, "Re-verify reachability for every recorded pin (bypasses the lockfile fast path)")
	cmd.AddCommand(newCheckCmd(f))
	cmd.AddCommand(newUpgradeCmd(f))

	return cmd
}

// Execute runs the root command and returns an exit code.
//
// Exit code contract (see docs/dependabot-cli-contract.md and INTEGRATION.md):
//
//   - 0: clean run, no blocking findings.
//   - 1: blocking findings present (errSilent sentinel); stdout JSON is
//     well-formed when --json was requested.
//   - 2: tool failure (bad flag, IO error, network failure, malformed
//     lockfile, future-version refusal, panic, etc.). stdout may be empty
//     or partial; consumers should rely on stderr for diagnosis.
func Execute() int {
	f := NewDefaultFactory()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	err := NewRootCmd(f).ExecuteContext(ctx)
	if err != nil && !errors.Is(err, errSilent) {
		// Detach any narration log sink first: it may have been pointed at
		// io.Discard during the run (the JSON-less "the terminal owns the
		// spinners" mode), in which case routing the error through
		// f.UI.Error would silently swallow it. We want the error visible.
		f.UI.SetLog(nil)
		f.UI.Error("%s", err)
	}
	return exitCodeFor(err)
}

// exitCodeFor maps an error returned by the root command to a process exit
// code. The classification rule is intentionally narrow: only the errSilent
// sentinel (returned when blocking findings are reported via well-formed
// JSON on stdout) maps to 1. Every other non-nil error — including
// pkg/lockfile.ErrFutureVersion — is a tool failure and maps to 2.
func exitCodeFor(err error) int {
	switch {
	case err == nil:
		return 0
	case errors.Is(err, errSilent):
		return 1
	default:
		return 2
	}
}

func discoverWorkflowPaths(existing []string) ([]string, error) {
	if len(existing) > 0 {
		return expandWorkflowPaths(existing)
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

func expandWorkflowPaths(paths []string) ([]string, error) {
	var expanded []string
	for _, path := range paths {
		info, err := os.Stat(path)
		if err != nil {
			expanded = append(expanded, path)
			continue
		}
		if !info.IsDir() {
			expanded = append(expanded, path)
			continue
		}

		entries, err := os.ReadDir(path)
		if err != nil {
			return nil, fmt.Errorf("reading %s: %w", path, err)
		}
		for _, entry := range entries {
			if entry.IsDir() {
				continue
			}
			ext := filepath.Ext(entry.Name())
			if ext == ".yml" || ext == ".yaml" {
				expanded = append(expanded, filepath.Join(path, entry.Name()))
			}
		}
	}
	sort.Strings(expanded)
	return expanded, nil
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
