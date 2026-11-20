package main

// Verification modes for gh actions-lock:
//
//   --verify       Frozen check: the default checks, including runner parity
//                  for every locked pin, without writing. Requires auth.
//
//   --verify-local Offline static coverage check. Every action ref must have
//                  a lockfile entry. No network, no auth, no writes — ideal for
//                  pre-commit. Local `./…` actions are reported as warnings.

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	parserlock "github.com/github/actions-lockfile/go/pkg/lockfile"
	"github.com/github/gh-actions-lock/cmd/gh-actions-lock/format"
	"github.com/github/gh-actions-lock/internal/dep"
	"github.com/github/gh-actions-lock/internal/lockfile"
	"github.com/github/gh-actions-lock/internal/pipeline"
	"github.com/github/gh-actions-lock/internal/pipeline/checks"
	"github.com/github/gh-actions-lock/internal/ui"
)

// applyVerifyFlags expands --verify into its constituent flags. Called at the
// top of runCheck before any work begins.
func applyVerifyFlags(opts *checkOptions) {
	if opts.verify {
		opts.noFix = true
	}
}

// runVerifyLocal performs the offline lockfile coverage check. It parses all
// workflows and verifies that every action ref has a corresponding lockfile
// entry — no network, no auth, never writes to disk.
func runVerifyLocal(opts *checkOptions, out io.Writer, console *ui.UI) error {
	workflowsDir := os.Getenv("GH_ACTIONS_LOCK_WORKFLOWS_DIR")
	paths, err := discoverWorkflowPaths(opts.workflowPaths, workflowsDir, false)
	if err != nil {
		return err
	}

	// Load the lockfile without a resolver (no network).
	lockPath := filepath.Join(".", parserlock.Path)
	if workflowsDir != "" {
		lockPath = filepath.Join(workflowsDir, "actions.lock")
	}
	store, err := lockfile.LoadStateAt(lockPath, nil)
	if errors.Is(err, lockfile.ErrCorruptLockfile) {
		// Word it as the other modes do.
		_, err = newLockRecovery()(lockPath, err)
		return err
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
	appendOwnSHAEntryFindings(report, store)
	blockLocalActions(report, opts.ignoreLocalActions)
	valid := report.IsValid()

	if opts.jsonFields != "" {
		if err := format.WriteJSON(out, report, nil, valid, opts.jsonFields, cliVersion(), store.File().Version, hostname); err != nil {
			return err
		}
	} else {
		format.PresentResults(console, report, valid, false)
	}

	if !valid {
		if opts.jsonFields == "" && hasAutoFixable(report) {
			console.TermDetail("Run without --verify-local to resolve and pin missing actions.")
		}
		return errSilent
	}
	if err := failOnWarnings(opts, report, console); err != nil {
		return err
	}

	total := len(paths)
	if opts.jsonFields == "" {
		console.TermSuccess("All %d %s have complete lockfile coverage.",
			total, ui.Pluralize(total, "workflow", "workflows"))
	}
	return nil
}

// appendOwnSHAEntryFindings reports, for a read-only run, each entry that
// records its own SHA as its ref and that no workflow finding already
// blocks on: one listed only by workflows the run didn't scan (not
// selected, or deleted), one reached only through another entry's `uses:`,
// or one nothing reaches. The Actions backend rejects the
// pin, so the run fails rather than passing; only a full-scan fix run may
// re-record or drop it.
func appendOwnSHAEntryFindings(report *checks.Report, store *lockfile.State) {
	// An entry is reported once: here only if no workflow finding already
	// blocks on it.
	covered := map[string]bool{}
	for _, wr := range report.Workflows {
		for _, f := range wr.Findings {
			if f.Severity != checks.SeverityError || f.Dependency == nil {
				continue
			}
			owner, repo := f.Dependency.OwnerRepo()
			covered[lockfile.EntryKey(parserlock.Pin{Owner: owner, Repo: repo, Ref: f.Dependency.Ref})] = true
		}
	}
	for _, e := range store.OwnSHAEntries() {
		if covered[e.Key] {
			continue
		}
		f := ownSHAEntryFinding(e)
		f.Severity = checks.SeverityError
		f.Remediation = "run `gh actions-lock` without workflow paths to record the tag or branch this commit came from"
		switch {
		case len(e.Workflows) == 0 && len(e.Parents) == 0:
			f.Remediation = "run `gh actions-lock` to drop it"
		case len(e.Workflows) == 0:
			// Only the composite can change a pin it holds.
			f.Remediation += "; if none contains it, upgrade or replace " + e.Parents[0]
		}
		report.RepoFindings = append(report.RepoFindings, f)
	}
}

// appendUnselectedOwnSHAWarnings warns, after a fix run, about each entry
// that still records its own SHA as its ref. A run repairs or blocks on
// every such entry its workflows reach, so one left belongs to workflows
// the run wasn't given and must not rewrite.
func appendUnselectedOwnSHAWarnings(report *checks.Report, store *lockfile.State) {
	for _, e := range store.OwnSHAEntries() {
		f := ownSHAEntryFinding(e)
		f.Severity = checks.SeverityWarning
		f.Remediation = "run `gh actions-lock` without a path to repair it"
		report.RepoFindings = append(report.RepoFindings, f)
	}
}

// ownSHAEntryFinding describes e as a not-pinned finding, naming where it
// sits; callers set the severity and remediation.
func ownSHAEntryFinding(e lockfile.OwnSHAEntry) checks.Finding {
	pin, _ := parserlock.ParsePin(e.Key)
	f := checks.Finding{
		Category:   checks.NotPinned,
		Confidence: checks.ConfidenceHigh,
		Dependency: &dep.Dependency{NWO: pin.NWO, Ref: pin.Ref, SHA: pin.Ref},
	}
	where := "used by no workflow"
	switch {
	case len(e.Workflows) > 0:
		f.WorkflowPath = e.Workflows[0]
		where = "in " + e.Workflows[0]
	case len(e.Parents) > 0:
		f.ParentNWO = e.Parents[0]
		where = "used by " + e.Parents[0]
	}
	f.Detail = fmt.Sprintf("lock entry for %s, %s, records ref %s, a commit SHA rather than the tag or branch it was taken from; the Actions backend rejects this pin", e.Key, where, parserlock.ShortSHA(pin.Ref))
	return f
}

// hasAutoFixable reports whether a plain run can fix any blocking finding.
func hasAutoFixable(report *checks.Report) bool {
	for _, f := range report.RepoFindings {
		if !f.IsValid() {
			return true
		}
	}
	for _, wr := range report.Workflows {
		for _, f := range wr.Findings {
			if !f.IsValid() && (format.IsAutoFixable(f.Category) || f.Category == checks.LocalAction) {
				return true
			}
		}
	}
	return false
}
