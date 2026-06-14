// Command gh-actions-lock scans workflows and pins GitHub Actions to
// immutable commit SHAs.
package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime/debug"
	"sync"

	"github.com/MakeNowJust/heredoc"
	"github.com/cli/go-gh/v2/pkg/repository"
	parserlock "github.com/github/actions-lockfile/go/pkg/lockfile"
	"github.com/github/gh-actions-lock/cmd/gh-actions-lock/format"
	"github.com/github/gh-actions-lock/internal/config"
	"github.com/github/gh-actions-lock/internal/pin"
	"github.com/github/gh-actions-lock/internal/pinpool"
	"github.com/github/gh-actions-lock/internal/pipeline"
	"github.com/github/gh-actions-lock/internal/pipeline/checks"
	"github.com/github/gh-actions-lock/internal/profile"
	"github.com/github/gh-actions-lock/internal/resolve"
	"github.com/github/gh-actions-lock/internal/tag"
	"github.com/github/gh-actions-lock/internal/ui"
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
	// noNarrow disables tag narrowing: mutable version refs like "v4"
	// are kept as-is in the lock comment instead of being resolved to
	// the full patch tag (e.g. "v4.2.1").
	noNarrow bool
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

			  gh actions-lock check --no-fix --json 2>/dev/null | jq .valid

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
			$ gh actions-lock check

			# Verify a specific workflow
			$ gh actions-lock check .github/workflows/ci.yml

			# Read-only check for CI (writes nothing, exits 1 if invalid)
			$ gh actions-lock check --no-fix --json=valid,findings

			# All fields as JSON
			$ gh actions-lock check --json
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
	cmd.Flags().BoolVar(&opts.noNarrow, "no-narrow", false, "Keep mutable version refs (e.g. v4) instead of narrowing to full patch tags (e.g. v4.2.1)")
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
	// check fix mode can rebuild a deleted lockfile, so interactive sessions
	// may delete-and-recreate an unreadable one. --no-fix is read-only and
	// must not delete; it fails instead.
	recoverLock := newLockRecovery(noInteractiveFlag(cmd), console, confirmFactoryHook, !opts.noFix)
	paths, r, store, err := newRun(opts.workflowPaths, opts.hostname, pool, newResolver, recoverLock)
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

	if showHeadlessProgress {
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
	// Defer spinner start until actual network work begins. The fast path
	// (everything trusted from the lockfile) returns before resolve fires,
	// so the spinner never appears and there's no flicker.
	if showSpinner {
		var once sync.Once
		runOpts.OnResolveProgress = func(done, total int) {
			once.Do(func() {
				console.StartProgress("Resolving actions")
			})
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

	// --no-onboard: refuse to onboard new workflows or actions. Rewrite the
	// relevant not-pinned findings to onboarding-required and drop their refs
	// so Plan/Commit never pins them; already-tracked refs that were bumped
	// (ref-changed) are left to re-pin as usual.
	onboardingRefused := 0
	var refusedLabels []string
	if noOnboardFlag(cmd) {
		refusedLabels = gateNoOnboard(report)
		onboardingRefused = len(refusedLabels)
		if onboardingRefused > 0 {
			valid = report.IsValid()
		}
	}

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
		// Surface SSO URL even in read-only mode — it's the actionable fix
		// for SAML-gated repos and shouldn't require a --fix run to see.
		if gc := r.GHClient(); gc != nil {
			if ssoURL := gc.SSOURL(); ssoURL != "" {
				console.TermBlank()
				console.TermDetail("Authorize in your web browser:  %s", ssoURL)
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
	if showSpinner {
		console.ClearWorkerStatuses()
	}
	record, planErr := pin.Plan(ctx, report, pin.PlanOptions{
		Resolver:  r,
		Tagger:    tagger,
		Store:     store,
		Pool:      pool,
		RepoOwner: repoOwner,
		RepoName:  repoName,
		Version:   cliVersion(),
		NoNarrow:  opts.noNarrow,
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

	// Inject info-severity findings for non-semver refs so they appear
	// in --json output. Suppressed when --no-narrow is set (user chose
	// this deliberately).
	if !opts.noNarrow {
		injectVersionRefFindings(report, record)
	}

	// Write the run log.
	record.Repo = &pin.RepoInfo{Owner: repoOwner, Name: repoName, Host: resolveHostname(opts.hostname)}
	if path, werr := record.WriteJSON(); werr == nil {
		defer func() {
			console.TermBlank()
			console.TermDetail("Resolution record: %s", path)
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
	hasInconclusive := opts.rescan && report.HasInconclusive()
	summaryErr := renderPinSummary(console, record, report, r, skippedRescan, hasInconclusive, refusedLabels, opts.noNarrow)

	// Surface the SAML SSO authorization URL if one was captured during
	// the run, matching cli/cli's "Authorize in your web browser:" line.
	// This runs even when renderPinSummary returns errSilent (unresolved
	// entries exist) because the SSO hint is the fix for those entries.
	if gc := r.GHClient(); gc != nil {
		if ssoURL := gc.SSOURL(); ssoURL != "" {
			console.TermBlank()
			console.TermDetail("Authorize in your web browser:  %s", ssoURL)
		}
	}

	if summaryErr != nil {
		return summaryErr
	}

	// --rescan strict gate: inconclusive reachability is a hard failure when
	// the user explicitly requested a full re-verification. Without this,
	// inconclusive findings (e.g. SAML-blocked branch listing) silently pass
	// and the "✓ All N workflows valid" message is misleading.
	if hasInconclusive {
		return errSilent
	}
	return nil
}

// injectVersionRefFindings appends info-severity findings for entries pinned
// with a non-full-semver ref (v4, v3.1, main, etc.). These surface in --json
// output so machine consumers can detect imprecise refs.
func injectVersionRefFindings(report *checks.Report, record *pin.Record) {
	// Index which workflows each non-semver dep appears in.
	type depInfo struct {
		nwo string
		ref string
		wfs map[string]bool
	}
	seen := map[string]*depInfo{} // NWO@Ref → info
	for _, e := range record.Entries {
		if e.Resolution != pin.Pinned && e.Resolution != pin.Verified {
			continue
		}
		sv, ok := parserlock.ParseSemVer(e.Ref)
		if ok && sv.IsFull() {
			continue
		}
		key := e.NWO + "@" + e.Ref
		di, exists := seen[key]
		if !exists {
			di = &depInfo{nwo: e.NWO, ref: e.Ref, wfs: map[string]bool{}}
			seen[key] = di
		}
		for _, wf := range e.Workflows {
			di.wfs[wf] = true
		}
	}
	if len(seen) == 0 {
		return
	}

	// Append a finding to each affected workflow report.
	for i := range report.Workflows {
		wr := &report.Workflows[i]
		for _, di := range seen {
			if !di.wfs[wr.Path] {
				continue
			}
			wr.Findings = append(wr.Findings, checks.Finding{
				WorkflowPath: wr.Path,
				Category:     checks.VersionRef,
				Severity:     checks.SeverityInfo,
				Confidence:   checks.ConfidenceHigh,
				Detail: fmt.Sprintf(
					"%s@%s: prefer a full semver ref (e.g. v4.2.1) — each patch tag resolves to exactly one commit",
					di.nwo, di.ref,
				),
			})
		}
	}
}

// cliVersion returns the gh-actions-lock extension version embedded by the Go
// build system. Returns "(devel)" for local `go build` and a real version
// like "v0.1.2" when installed via `gh extension install`.
func cliVersion() string {
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" {
		return info.Main.Version
	}
	return "unknown"
}
