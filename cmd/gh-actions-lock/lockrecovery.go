package main

import (
	"fmt"
	"os"

	"github.com/cli/go-gh/v2/pkg/prompter"
	"github.com/github/gh-actions-lock/internal/ui"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

// noInteractiveFlag reports the value of the persistent --no-interactive flag,
// defaulting to false when it is not registered.
func noInteractiveFlag(cmd *cobra.Command) bool {
	v, _ := cmd.Flags().GetBool("no-interactive")
	return v
}

// confirmer asks a yes/no question. Satisfied by *prompter.Prompter; an
// interface so tests inject a fake without a TTY.
type confirmer interface {
	Confirm(prompt string, defaultValue bool) (bool, error)
}

// confirmFactory returns a confirmer and whether the session can prompt.
// canPrompt is false in any non-interactive context (no TTY, CI), so the
// recovery policy fails closed instead of blocking on input that never comes.
type confirmFactory func() (confirmer, bool)

// confirmFactoryHook is the confirm factory commands use to build the
// corrupt-lockfile recovery policy. Production points at defaultConfirmFactory
// (real terminal). Tests override it to drive the interactive delete-and-
// recreate path without a TTY; the command tests run serially (t.Chdir) so a
// package-level override with cleanup is safe.
var confirmFactoryHook confirmFactory = defaultConfirmFactory

// defaultConfirmFactory binds to the real terminal and renders to stderr so
// `--json` stdout stays clean. It reports canPrompt only when both stdin and
// stderr are TTYs and CI is unset — a CI runner with a stray TTY must never
// be prompted.
func defaultConfirmFactory() (confirmer, bool) {
	if !term.IsTerminal(int(os.Stdin.Fd())) || !term.IsTerminal(int(os.Stderr.Fd())) || ciEnabled() {
		return nil, false
	}
	return prompter.New(os.Stdin, os.Stderr, os.Stderr), true
}

// ciEnabled mirrors the CI convention used by internal/ui: most providers set
// CI=true. A truthy CI value means no interactive prompts.
func ciEnabled() bool {
	v := os.Getenv("CI")
	return v != "" && v != "0" && v != "false"
}

// lockRecovery decides what to do when the on-disk lockfile can't be parsed.
// It returns (true, nil) when the lockfile was removed and loading should be
// retried (the empty-lockfile path then recreates it), or a non-nil error to
// abort the run (exit 2). It never silently accepts an unreadable lockfile.
type lockRecovery func(lockPath string, parseErr error) (recovered bool, err error)

// newLockRecovery builds the recovery policy. allowDelete is false for
// read-only or relock commands that cannot rebuild a deleted lockfile
// (`check --no-fix`, `update`); those always fail with a clear pointer. When
// allowDelete is true (`check` fix mode), an interactive session is offered a
// delete-and-recreate; non-interactive sessions (CI, --no-interactive) fail.
func newLockRecovery(noInteractive bool, console *ui.UI, newConfirm confirmFactory, allowDelete bool) lockRecovery {
	return func(lockPath string, parseErr error) (bool, error) {
		if !allowDelete {
			return false, fmt.Errorf("%w; run `gh actions-lock check` to rebuild it, or delete it by hand", parseErr)
		}
		var (
			confirm   confirmer
			canPrompt bool
		)
		if newConfirm != nil {
			confirm, canPrompt = newConfirm()
		}
		if noInteractive || !canPrompt {
			return false, fmt.Errorf("%w; delete it and re-run to recreate it, or fix it by hand", parseErr)
		}
		// Release the terminal so the prompt renders cleanly over any spinner.
		console.StopProgress()
		ok, err := confirm.Confirm(fmt.Sprintf("Lockfile %s is unreadable (%v). Delete and recreate it?", lockPath, parseErr), false)
		if err != nil {
			return false, err
		}
		if !ok {
			return false, fmt.Errorf("%w; left in place", parseErr)
		}
		if err := os.Remove(lockPath); err != nil {
			return false, fmt.Errorf("deleting unreadable lockfile %s: %w", lockPath, err)
		}
		console.TermNeutral("Deleted unreadable lockfile %s; it will be recreated.", lockPath)
		return true, nil
	}
}
