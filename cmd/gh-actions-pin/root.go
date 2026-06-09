package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"syscall"

	"github.com/MakeNowJust/heredoc"
	"github.com/cli/go-gh/v2/pkg/repository"
	"github.com/github/gh-actions-pin/internal/lockfile"
	"github.com/github/gh-actions-pin/internal/pinpool"
	"github.com/github/gh-actions-pin/internal/resolve"
	"github.com/github/gh-actions-pin/internal/ui"
	"github.com/github/gh-actions-pin/internal/workflowfile"
	"github.com/spf13/cobra"
)

// execute runs the root command and returns an exit code.
//
//   - 0: nothing to fix, or every finding was auto-fixed.
//   - 1: blocking findings remain (errSilent sentinel) — any invalid finding
//     under --no-fix, otherwise findings needing manual review; stdout JSON
//     is well-formed when --json was requested.
//   - 2: tool failure (bad flag, IO error, network failure, malformed
//     lockfile, future-version refusal, panic, etc.). stdout may be empty
//     or partial; consumers should rely on stderr for diagnosis.
func execute() int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	defer ui.CloseProgressTrace()
	err := newRootCmd(nil).ExecuteContext(ctx)
	switch {
	case err == nil:
		return 0
	case errors.Is(err, errSilent):
		// Blocking findings were already reported via well-formed output; exit
		// 1 quietly so a second error line doesn't clobber the JSON/summary.
		return 1
	case errors.Is(err, context.Canceled):
		// CTRL+C / SIGTERM: exit silently. The signal is intentional, not a
		// tool failure. Print a newline so the shell prompt starts clean.
		fmt.Fprintln(os.Stderr)
		return 2
	default:
		// Every other non-nil error — including lockfile.ErrFutureVersion —
		// is a tool failure and maps to 2. Print it on a fresh UI bound to
		// stderr so it's visible regardless of how the run configured its own.
		ui.New().Error("%s", err)
		return 2
	}
}

type resolverFunc func(hostname string, pool *pinpool.Pool) (*resolve.Resolver, error)

// newRootCmd returns the cobra command for the root `actions-pin` invocation.
// newResolver supplies the resolver builder; pass nil for production wiring.
func newRootCmd(newResolver resolverFunc) *cobra.Command {
	return newRootCmdWithPrompter(newResolver, nil)
}

// newRootCmdWithPrompter is newRootCmd with an injectable interactive prompter
// factory for `update`. newPrompt nil selects the terminal-bound default.
func newRootCmdWithPrompter(newResolver resolverFunc, newPrompt promptFactory) *cobra.Command {
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

Scans all workflows under .github/workflows/ by default and fixes
what it can — pinning every resolvable action and updating the
lockfile. Pass --no-fix for a read-only check that writes nothing.

--json selects the output format only (independent of --no-fix);
structured results go to stdout and progress to stderr:

  gh actions-pin --no-fix --json 2>/dev/null | jq .valid

Commands:

  gh actions-pin             Verify and fix the dependency lock
`),
		Example: heredoc.Doc(`
# Verify all workflows and fix what's fixable
$ gh actions-pin

# Verify a specific workflow
$ gh actions-pin .github/workflows/ci.yml

# Read-only check for CI integration (writes nothing)
$ gh actions-pin --no-fix --json=valid,findings
`),
		PreRunE: func(cmd *cobra.Command, args []string) error {
			if len(args) > 0 {
				opts.workflowPaths = args
			}
			return opts.validateOutputFlags()
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			return runCheck(cmd, opts, newResolver)
		},
	}

	bindCheckFlags(cmd, opts)
	// Accept --no-onboard and --no-interactive everywhere for symmetry and
	// Dependabot compatibility. `check` ignores both; `update` refuses
	// onboarding unconditionally (so --no-onboard reaffirms the default) and
	// treats --no-interactive as a no-op.
	cmd.PersistentFlags().Bool("no-onboard", false, "Refuse to add new workflow lockfile entries (update refuses onboarding regardless)")
	cmd.PersistentFlags().Bool("no-interactive", false, "Run without interactive prompts (accepted and ignored)")
	cmd.AddCommand(newCheckCmd(newResolver))
	cmd.AddCommand(newUpdateCmd(newResolver, newPrompt))

	return cmd
}

// newRun performs the per-invocation wiring shared by every command: expand the
// requested workflow paths (or discover them), build a resolver for the
// resolved hostname, open the lockfile store against it, and seed branch hints
// from the existing lockfile so repeat scans short-circuit the per-branch
// Compare walk. newResolver is the DI seam; pass nil for production wiring.
func newRun(workflowPaths []string, hostname string, pool *pinpool.Pool, newResolver resolverFunc) ([]string, *resolve.Resolver, *lockfile.State, error) {
	paths, err := discoverWorkflowPaths(workflowPaths)
	if err != nil {
		return nil, nil, nil, err
	}

	if newResolver == nil {
		newResolver = func(hostname string, pool *pinpool.Pool) (*resolve.Resolver, error) {
			return resolve.New(hostname, pool)
		}
	}
	r, err := newResolver(resolveHostname(hostname), pool)
	if err != nil {
		return nil, nil, nil, err
	}

	store, err := lockfile.LoadState(".", r)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("opening lockfile: %w", err)
	}
	r.SeedBranchHints(store.AllDeps())

	return paths, r, store, nil
}

func discoverWorkflowPaths(existing []string) ([]string, error) {
	if len(existing) > 0 {
		return expandWorkflowPaths(existing)
	}

	paths, err := workflowfile.DiscoverWorkflows()
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
