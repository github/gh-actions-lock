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
	parserlock "github.com/github/actions-lockfile/go/pkg/lockfile"
	"github.com/github/gh-actions-lock/internal/lockfile"
	"github.com/github/gh-actions-lock/internal/pinpool"
	"github.com/github/gh-actions-lock/internal/resolve"
	"github.com/github/gh-actions-lock/internal/ui"
	"github.com/github/gh-actions-lock/internal/workflowfile"
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

// newRootCmd returns the cobra command for the root `actions-lock` invocation.
// newResolver supplies the resolver builder; pass nil for production wiring.
func newRootCmd(newResolver resolverFunc) *cobra.Command {
	opts := &checkOptions{}

	cmd := &cobra.Command{
		Use:           "actions-lock [<workflow-path>...]",
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

  gh actions-lock --no-fix --json 2>/dev/null | jq .valid

Commands:

  gh actions-lock             Verify and fix the dependency lock
`),
		Example: heredoc.Doc(`
# Verify all workflows and fix what's fixable
$ gh actions-lock

# Verify a specific workflow
$ gh actions-lock .github/workflows/ci.yml

# Read-only check for CI integration (writes nothing)
$ gh actions-lock --no-fix --json=valid,findings
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
	// --no-onboard and --no-interactive are persistent so they apply to the
	// root check invocation. --no-onboard refuses to onboard new workflows or
	// actions (re-pinning already-tracked entries still happens); used by
	// Dependabot so a relock never silently adds an entry it didn't ask for.
	cmd.PersistentFlags().Bool("no-onboard", false, "Refuse to onboard new workflows or actions; only re-pin already-tracked entries")
	cmd.PersistentFlags().Bool("no-interactive", false, "Run without interactive prompts")
	cmd.AddCommand(newCheckCmd(newResolver))

	return cmd
}

// newRun performs the per-invocation wiring shared by every command: expand the
// requested workflow paths (or discover them), build a resolver for the
// resolved hostname, open the lockfile store against it, and seed branch hints
// from the existing lockfile so repeat scans short-circuit the per-branch
// Compare walk. newResolver is the DI seam; pass nil for production wiring.
func newRun(workflowPaths []string, hostname string, pool *pinpool.Pool, newResolver resolverFunc, onCorrupt lockRecovery) ([]string, *resolve.Resolver, *lockfile.State, error) {
	workflowsDir := os.Getenv("GH_ACTIONS_LOCK_WORKFLOWS_DIR")
	paths, err := discoverWorkflowPaths(workflowPaths, workflowsDir)
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

	var store *lockfile.State
	if workflowsDir != "" {
		store, err = lockfile.LoadStateAt(filepath.Join(workflowsDir, "actions.lock"), r)
	} else {
		store, err = lockfile.LoadState(".", r)
	}
	if err != nil {
		// An unreadable (non-future-version) lockfile is never silently
		// discarded. Recovery policy may delete-and-recreate (interactive
		// fix mode) or fail (CI, read-only, relock); either way the choice
		// is explicit and surfaces to the user.
		if errors.Is(err, lockfile.ErrCorruptLockfile) && onCorrupt != nil {
			lockPath := filepath.Join(".", parserlock.Path)
			if workflowsDir != "" {
				lockPath = filepath.Join(workflowsDir, "actions.lock")
			}
			recovered, rerr := onCorrupt(lockPath, err)
			if rerr != nil {
				return nil, nil, nil, rerr
			}
			if recovered {
				if workflowsDir != "" {
					store, err = lockfile.LoadStateAt(filepath.Join(workflowsDir, "actions.lock"), r)
				} else {
					store, err = lockfile.LoadState(".", r)
				}
			}
		}
		if err != nil {
			return nil, nil, nil, fmt.Errorf("opening lockfile: %w", err)
		}
	}
	r.SeedBranchHints(store.AllDeps())

	return paths, r, store, nil
}

func discoverWorkflowPaths(existing []string, workflowsDir string) ([]string, error) {
	if len(existing) > 0 {
		return expandWorkflowPaths(existing)
	}

	if workflowsDir != "" {
		paths, err := workflowfile.DiscoverWorkflowsIn(workflowsDir)
		if err != nil {
			return nil, err
		}
		if len(paths) == 0 {
			return nil, fmt.Errorf("no workflow files found in %s", workflowsDir)
		}
		return paths, nil
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
