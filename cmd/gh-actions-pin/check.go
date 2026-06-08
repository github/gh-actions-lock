// Command gh-actions-pin scans workflows and pins GitHub Actions to
// immutable commit SHAs.
package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime/debug"

	"github.com/MakeNowJust/heredoc"
	"github.com/cli/go-gh/v2/pkg/repository"
	"github.com/github/gh-actions-pin/cmd/gh-actions-pin/format"
	"github.com/github/gh-actions-pin/internal/config"
	"github.com/github/gh-actions-pin/internal/pin"
	"github.com/github/gh-actions-pin/internal/pinpool"
	"github.com/github/gh-actions-pin/internal/pipeline"
	"github.com/github/gh-actions-pin/internal/profile"
	"github.com/github/gh-actions-pin/internal/resolve"
	"github.com/github/gh-actions-pin/internal/tag"
	"github.com/github/gh-actions-pin/internal/ui"
	"github.com/spf13/cobra"
)

type checkOptions struct {
	workflowPaths []string
	jsonFields    string
	hostname      string
	// rescan forces a full reachability re-verification of every recorded
	// pin, bypassing the fast path that trusts the lockfile. Useful for
	// audits or when a CI policy requires re-attestation on every run.
	rescan bool
	// profileDir, when non-empty, enables profiling: execution trace,
	// CPU profile, and HTTP request log are written to files in this dir.
	profileDir string
	// noFix makes the run read-only: report findings and exit without
	// rewriting workflows or updating the lockfile. Orthogonal to the
	// renderer choice (--json).
	noFix bool
}

func newCheckCmd(newResolver resolverFunc) *cobra.Command {
	opts := &checkOptions{}

	cmd := &cobra.Command{
		Use:   "check [<workflow-path>...]",
		Args:  cobra.ArbitraryArgs,
		Short: "Verify the dependency lock and fix issues",
		Long: heredoc.Doc(`
			Verify that every action dependency in your workflows is locked to
			an immutable commit SHA and that the lock is still valid.

			Scans all workflows under .github/workflows/ by default, or pass
			specific paths. Checks both direct and transitive dependencies
			(composite actions that reference other actions).

			By default this writes: every resolvable action is pinned and the
			lockfile is updated in place. Pass --no-fix for a read-only check
			that reports findings and changes nothing on disk (the CI gate).

			--json selects the output format only — structured results on
			stdout, progress on stderr — and is independent of --no-fix:

			  gh actions-pin check --no-fix --json 2>/dev/null | jq .valid

			Issue types:
			  ref-moved        - locked SHA no longer matches upstream (expected for mutable tags like v4)
			  not-pinned       - action in workflow has no lock entry
			  stale            - lock entry references an action no longer in the workflow
			  ref-changed      - workflow ref was edited; lock needs updating
			  misleading-sha   - ref looks like a SHA but resolves to a different commit
			  impostor-commit  - locked SHA is not reachable from any branch in the upstream repo
			  lockfile-forgery - pinned SHA is not an ancestor of the upstream ref it claims

			Exit status:
			  0  read-only run that found everything valid, or a fix run where
			     every finding was resolved automatically.
			  1  blocking findings remain — under --no-fix, any invalid finding;
			     otherwise, findings that can't be auto-fixed (impostor commit
			     or lockfile forgery) and need manual review. Output is
			     well-formed when --json is set.
			  2  the tool itself failed (bad flag, IO error, network failure,
			     malformed lockfile, etc.).
			With --json, parse stdout and branch on .valid (the pre-fix
			diagnosis) regardless of exit code.
		`),
		Example: heredoc.Doc(`
			# Verify all workflows and fix what's fixable
			$ gh actions-pin check

			# Verify a specific workflow
			$ gh actions-pin check .github/workflows/ci.yml

			# Read-only check for CI (writes nothing, exits 1 if invalid)
			$ gh actions-pin check --no-fix --json=valid,findings

			# All fields as JSON
			$ gh actions-pin check --json
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
	return cmd
}

// bindCheckFlags registers the flags shared by the root command and the
// explicit `check` subcommand. Root is just the default check invocation, so
// both bind the identical surface from one place.
func bindCheckFlags(cmd *cobra.Command, opts *checkOptions) {
	cmd.Flags().StringVar(&opts.jsonFields, "json", "", "Output JSON with the specified `fields` (valid,findings,workflows,dependencies)")
	cmd.Flags().Lookup("json").NoOptDefVal = "valid,findings,workflows"
	cmd.Flags().StringVar(&opts.hostname, "hostname", "", "GitHub hostname to query (defaults to GH_HOST, current repo host, or github.com)")
	cmd.Flags().BoolVar(&opts.rescan, "rescan", false, "Re-verify reachability for every recorded pin (bypasses the lockfile fast path)")
	cmd.Flags().BoolVar(&opts.noFix, "no-fix", false, "Read-only: report findings without modifying workflows or the lockfile")
	cmd.Flags().StringVar(&opts.profileDir, "profile", "", "Enable profiling: write trace, CPU profile, and HTTP log to `dir`")
}

// validateOutputFlags rejects incoherent structured-output flag combinations.
// Wired as PreRunE so the error surfaces at the command layer before any work runs.
func (opts *checkOptions) validateOutputFlags() error {
	return format.ValidateJSONFields(opts.jsonFields)
}

func runCheck(cmd *cobra.Command, opts *checkOptions, newResolver resolverFunc) error {
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	out := cmd.OutOrStdout()
	errOut := cmd.ErrOrStderr()
	console := ui.NewWithWriter(errOut)
	defer console.StopProgress()

	// Profiling: when --profile is set, start trace + CPU profile + HTTP log.
	var prof *profile.Session
	if opts.profileDir != "" {
		if err := os.MkdirAll(opts.profileDir, 0o755); err != nil {
			return fmt.Errorf("creating profile dir: %w", err)
		}
		var err error
		prof, err = profile.Start(profile.Options{
			TracePath:      filepath.Join(opts.profileDir, "trace.out"),
			CPUProfilePath: filepath.Join(opts.profileDir, "cpu.prof"),
			HTTPLog:        true,
			Output:         errOut,
		})
		if err != nil {
			return fmt.Errorf("starting profiler: %w", err)
		}
		defer prof.Stop()
	}

	// Shared worker pool for all concurrent phases.
	pool := pinpool.New(0, console) // 0 → DefaultWorkers

	// If profiling, use a profiled resolver that logs HTTP calls.
	if prof != nil && newResolver == nil {
		newResolver = func(hostname string, pool *pinpool.Pool) (*resolve.Resolver, error) {
			return resolve.New(hostname, pool, resolve.WithProfile(prof))
		}
	}

	endSetup := prof.Phase("setup (discover + lockfile)")
	paths, r, store, err := newRun(opts.workflowPaths, opts.hostname, pool, newResolver)
	if err != nil {
		return err
	}
	// Pre-warm resolver caches from the lockfile so repeat runs skip
	// redundant GraphQL and REST calls. Skipped when --rescan is set:
	// a full re-verification must hit the network to detect ref movement
	// and re-check reachability.
	trustLockfileCaches := !opts.rescan
	if trustLockfileCaches {
		r.SeedFromLockfile(store.AllDeps())
	}
	endSetup()
	opts.workflowPaths = paths

	// Detailed narration is suppressed from the terminal during the run so the
	// output stays limited to phase labels, prompts, and the final summary.
	// A structured, action-centric provenance record (what was resolved and
	// how) is written at the end instead. JSON mode keeps the narration log
	// attached so machine-readable events can flow on stderr.
	if opts.jsonFields == "" {
		console.SetLog(io.Discard)
	}

	// Pipeline: scan → resolve → reachability → diagnose → enrich.
	total := len(opts.workflowPaths)
	showSpinner := opts.jsonFields == "" && !console.Headless()
	showHeadlessProgress := console.Headless()

	// Start spinner — the label stays empty until the first per-ref resolve
	// callback fires. The grace period suppresses flicker on fast runs.
	if showSpinner {
		console.StartProgress("")
	} else if showHeadlessProgress {
		console.StartProgress(fmt.Sprintf("Scanning %d %s", total, ui.Pluralize(total, "workflow", "workflows")))
	}

	// Build a Lister for impostor enrichment + pin narrowing,
	// reusing the resolver's unified API client.
	var tagger *tag.Lister
	if gc := r.GHClient(); gc != nil {
		tagger = tag.NewLister(gc, config.Load().Cooldown)
	}

	runOpts := pipeline.RunOptions{
		WorkflowPaths: opts.workflowPaths,
		Resolver:      r,
		Tagger:        tagger,
		Store:         store,
		Pool:          pool,
		Rescan:        opts.rescan,
		Profile:       prof,
	}
	// Interactive spinner mode: wire per-ref resolver progress counter.
	// Reachability runs under the same label — no separate phase.
	if showSpinner {
		runOpts.OnResolveProgress = func(done, total int) {
			console.UpdateLabel(fmt.Sprintf("Resolving actions [%d/%d]", done, total))
		}
	}

	endRun := prof.Phase("pipeline.Run (scan+resolve+reachability+diagnose)")
	result, err := pipeline.Run(ctx, runOpts)
	endRun()
	if err != nil {
		console.StopProgress()
		return err
	}
	if showSpinner {
		console.ClearWorkerStatuses()
		console.UpdateProgress("")
	}

	report := result.Report
	valid := result.Valid
	skippedRescan := result.SkippedRescan

	// Render the read-only diagnosis. --json selects the renderer; it does
	// not decide whether fixes are applied. Terminal output is shown up front
	// (the human narrative). JSON is emitted later, after any fixes land, so
	// stdout never carries a diagnosis for a run that then failed to commit.
	if opts.jsonFields == "" {
		// Pause the spinner so PresentResults lines don't collide with it.
		if showSpinner {
			console.PauseProgress()
		}
		format.PresentResults(console, report, valid, true)
		if showSpinner {
			console.ResumeProgress()
		}
	}

	// --no-fix: read-only. Stop after the diagnosis without touching disk.
	// Strict gate — any blocking finding is a non-zero exit.
	if opts.noFix {
		console.StopProgress()
		if opts.jsonFields != "" {
			if err := format.WriteJSON(out, report, valid, opts.jsonFields, cliVersion(), store.File().Version); err != nil {
				return err
			}
		}
		if !valid {
			if opts.jsonFields == "" {
				console.TermDetail("Re-run without --no-fix to apply fixes.")
			}
			return errSilent
		}
		return nil
	}

	// Plan: build the complete pin record (resolution, reachability,
	// narrowing, reverse lookup — all computation, no disk writes).
	var repoOwner, repoName string
	if currentRepo, err := repository.Current(); err == nil {
		repoOwner = currentRepo.Owner
		repoName = currentRepo.Name
	}

	endPlan := prof.Phase("pin.Plan (narrowing+reverse)")
	record, planErr := pin.Plan(ctx, report, pin.PlanOptions{
		Resolver:  r,
		Tagger:    tagger,
		Store:     store,
		Pool:      pool,
		RepoOwner: repoOwner,
		RepoName:  repoName,
		Version:   cliVersion(),
	})
	endPlan()
	if planErr != nil {
		console.StopProgress()
		return fmt.Errorf("planning pins: %w", planErr)
	}

	// Commit: write all changes to disk atomically (fast local I/O, no
	// spinner label — it finishes before the user could read one).
	endCommit := prof.Phase("pin.Commit (disk writes)")
	if err := pin.Commit(ctx, record, store, nil); err != nil {
		console.StopProgress()
		return fmt.Errorf("committing pins: %w", err)
	}
	endCommit()

	console.StopProgress()

	// Write the run log.
	record.Repo = &pin.RepoInfo{Owner: repoOwner, Name: repoName, Host: resolveHostname(opts.hostname)}
	if path, werr := record.WriteJSON(); werr == nil {
		defer func() {
			console.TermBlank()
			console.TermNeutral("Resolution record: %s", path)
		}()
	}

	// JSON mode emits the (pre-fix) diagnosis now — after the commit
	// succeeded — so machine consumers never see findings for a run that
	// then failed to write. Exit code mirrors the terminal autofix path: a
	// non-zero exit only when findings remain that can't be auto-fixed
	// (impostor commit / lockfile forgery).
	if opts.jsonFields != "" {
		if err := format.WriteJSON(out, report, valid, opts.jsonFields, cliVersion(), store.File().Version); err != nil {
			return err
		}
		if len(record.Investigated()) > 0 {
			return errSilent
		}
		return nil
	}

	// Terminal summary.
	return renderPinSummary(console, record, report, r, skippedRescan)
}

// cliVersion returns the gh-actions-pin extension version embedded by the Go
// build system. Returns "(devel)" for local `go build` and a real version
// like "v0.1.2" when installed via `gh extension install`.
func cliVersion() string {
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" {
		return info.Main.Version
	}
	return "unknown"
}
