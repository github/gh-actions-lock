package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/github/gh-actions-lock/cmd/gh-actions-lock/format"
	"github.com/github/gh-actions-lock/internal/lockfile"
	"github.com/github/gh-actions-lock/internal/pipeline"
	"github.com/github/gh-actions-lock/internal/ui"
)

// localVerifier is the offline static coverage check (--verify-local).
type localVerifier struct {
	opts *checkOptions
}

// Verify parses all workflows and checks that every action ref has a
// lockfile entry: no network, no auth, never writes to disk.
func (v localVerifier) Verify(_ context.Context, out io.Writer, console *ui.UI) error {
	opts := v.opts
	workflowsDir := os.Getenv("GH_ACTIONS_LOCK_WORKFLOWS_DIR")
	paths, err := discoverWorkflowPaths(opts.workflowPaths, workflowsDir, false)
	if err != nil {
		return err
	}

	var store *lockfile.State
	if workflowsDir != "" {
		store, err = lockfile.LoadStateAt(filepath.Join(workflowsDir, "actions.lock"), nil)
	} else {
		store, err = lockfile.LoadState(".", nil)
	}
	if err != nil {
		return fmt.Errorf("opening lockfile: %w", err)
	}
	hostname := resolveHostname(opts.hostname)
	if err := store.SetHostname(hostname); err != nil {
		return err
	}

	parsed := pipeline.ParseAll(paths, store)
	report := pipeline.VerifyLocalCoverage(parsed, store)
	valid := report.IsValid()

	if opts.jsonFields != "" {
		if err := format.WriteJSON(out, report, nil, valid, opts.jsonFields, cliVersion(), store.File().Version, hostname); err != nil {
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
		console.TermSuccess("All %d %s have valid lockfile coverage for inspected actions.",
			total, ui.Pluralize(total, "workflow", "workflows"))
	}
	return nil
}
