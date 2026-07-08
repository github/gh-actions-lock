package main

// Verification modes for gh actions-lock:
//
//   --verify       Full re-verification of every pin against the network.
//                  Equivalent to --rescan --no-fix. Requires auth.
//
//   --verify-local Offline static coverage check. Every action ref must have
//                  a lockfile entry. No network, no auth — ideal for pre-commit.

import (
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/github/gh-actions-lock/cmd/gh-actions-lock/format"
	"github.com/github/gh-actions-lock/internal/lockfile"
	"github.com/github/gh-actions-lock/internal/pipeline"
	"github.com/github/gh-actions-lock/internal/ui"
)

// applyVerifyFlags expands --verify into its constituent flags. Called at the
// top of runCheck before any work begins.
func applyVerifyFlags(opts *checkOptions) {
	if opts.verify {
		opts.rescan = true
		opts.noFix = true
	}
}

// runVerifyLocal performs the offline lockfile coverage check. It parses all
// workflows and verifies that every action ref has a corresponding lockfile
// entry — no network, no auth required.
func runVerifyLocal(opts *checkOptions, out io.Writer, console *ui.UI) error {
	workflowsDir := os.Getenv("GH_ACTIONS_LOCK_WORKFLOWS_DIR")
	paths, err := discoverWorkflowPaths(opts.workflowPaths, workflowsDir)
	if err != nil {
		return err
	}

	// Load the lockfile without a resolver (no network).
	var store *lockfile.State
	if workflowsDir != "" {
		store, err = lockfile.LoadStateAt(filepath.Join(workflowsDir, "actions.lock"), nil)
	} else {
		store, err = lockfile.LoadState(".", nil)
	}
	if err != nil {
		return fmt.Errorf("opening lockfile: %w", err)
	}

	parsed := pipeline.ParseAll(paths, store)
	report := pipeline.VerifyLocalCoverage(parsed, store)
	valid := report.IsValid()

	if opts.jsonFields != "" {
		if err := format.WriteJSON(out, report, valid, opts.jsonFields, cliVersion(), store.File().Version); err != nil {
			return err
		}
	} else {
		format.PresentResults(console, report, valid, false)
	}

	if !valid {
		if opts.jsonFields == "" {
			console.TermDetail("Run without --verify-local to resolve and pin missing actions.")
		}
		return errSilent
	}

	total := len(paths)
	if opts.jsonFields == "" {
		console.TermSuccess("All %d %s have complete lockfile coverage.",
			total, ui.Pluralize(total, "workflow", "workflows"))
	}
	return nil
}
