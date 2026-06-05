package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"runtime/debug"
	"strings"

	"github.com/MakeNowJust/heredoc"
	"github.com/cli/go-gh/v2/pkg/api"
	"github.com/cli/go-gh/v2/pkg/repository"
	"github.com/github/gh-actions-pin/cmd/gh-actions-pin/format"
	"github.com/github/gh-actions-pin/internal/doctor"
	"github.com/github/gh-actions-pin/internal/lockfile"
	"github.com/github/gh-actions-pin/internal/runlog"
	"github.com/github/gh-actions-pin/internal/ui"
	"github.com/spf13/cobra"
)

type checkOptions struct {
	WorkflowPaths []string
	JSONFields    string
	// Format selects a structured output format. Currently only "sarif"
	// is supported; "" leaves the existing terminal/--json behavior
	// untouched. SARIF output is written to OutputPath (or stdout when
	// OutputPath is "-").
	Format string
	// OutputPath is where structured output (currently SARIF) is
	// written. "-" means stdout.
	OutputPath    string
	Hostname      string
	NoInteractive bool
	// Rescan forces a full reachability re-verification of every recorded
	// pin, bypassing the fast path that trusts the lockfile. Useful for
	// audits or when a CI policy requires re-attestation on every run.
	Rescan bool
}

func newCheckCmd(f *pinFactory) *cobra.Command {
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

			When run interactively (TTY), offers to fix issues inline.
			Non-interactive mode (--no-interactive or CI) auto-fixes
			deterministic issues and exits non-zero if anything remains
			that requires human judgment.

			With --json, structured results go to stdout and progress to stderr:

			  gh actions-pin check --json 2>/dev/null | jq .valid

			Issue types:
			  ref-moved        - locked SHA no longer matches upstream (expected for mutable tags like v4)
			  not-pinned       - action in workflow has no lock entry
			  stale            - lock entry references an action no longer in the workflow
			  ref-changed      - workflow ref was edited; lock needs updating
			  misleading-sha   - ref looks like a SHA but resolves to a different commit
			  impostor-commit  - locked SHA is not reachable from any branch in the upstream repo
			  lockfile-forgery - pinned SHA is not an ancestor of the upstream ref it claims

			Exit status: 0 if valid; 1 if blocking findings remain (stdout JSON
			is well-formed when --json is set); 2 if the tool itself failed
			(bad flag, IO error, network failure, malformed lockfile, etc.).
			With --json, parse stdout regardless of exit code and branch on
			.valid — see INTEGRATION.md for details.
		`),
		Example: heredoc.Doc(`
			# Verify all workflows
			$ gh actions-pin check

			# Verify a specific workflow
			$ gh actions-pin check .github/workflows/ci.yml

			# Report-only mode (no prompts, no changes)
			$ gh actions-pin check --no-interactive

			# JSON output for CI
			$ gh actions-pin check --json=valid,findings

			# All fields as JSON
			$ gh actions-pin check --json
		`),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) > 0 {
				opts.WorkflowPaths = args
			}
			return runCheck(f, opts)
		},
	}

	cmd.Flags().StringVar(&opts.JSONFields, "json", "", "Output JSON with the specified `fields` (valid,findings,workflows,dependencies)")
	cmd.Flags().Lookup("json").NoOptDefVal = "valid,findings,workflows"
	cmd.Flags().StringVar(&opts.Format, "format", "", "Structured output `format` (currently only `sarif`)")
	cmd.Flags().StringVar(&opts.OutputPath, "output", "", "Write structured output to `path` (use `-` for stdout). Required with --format=sarif.")
	cmd.Flags().StringVar(&opts.Hostname, "hostname", "", "GitHub hostname to query (defaults to GH_HOST, current repo host, or github.com)")
	cmd.Flags().BoolVar(&opts.NoInteractive, "no-interactive", false, "Auto-fix deterministic issues; fail on issues requiring human input")
	cmd.Flags().BoolVar(&opts.Rescan, "rescan", false, "Re-verify reachability for every recorded pin (bypasses the lockfile fast path)")
	return cmd
}

func runCheck(f *pinFactory, opts *checkOptions) error {
	// Validate structured-output flags before any work runs. --format is
	// orthogonal to --json: emitting both at once would produce two
	// stdout streams competing for the same writer, so we reject the
	// combination up front instead of letting the second one clobber
	// the first.
	switch opts.Format {
	case "", "sarif":
	default:
		return fmt.Errorf("unknown --format %q (supported: sarif)", opts.Format)
	}
	if opts.Format == "sarif" {
		if opts.OutputPath == "" {
			return fmt.Errorf("--output is required when --format=sarif (use `-` for stdout)")
		}
		if opts.JSONFields != "" {
			return fmt.Errorf("--format=sarif cannot be combined with --json")
		}
	} else if opts.OutputPath != "" {
		return fmt.Errorf("--output requires --format")
	}

	paths, err := discoverWorkflowPaths(opts.WorkflowPaths)
	if err != nil {
		return err
	}
	opts.WorkflowPaths = paths

	// --no-interactive is an explicit headless signal: skip the spinner,
	// icons, and per-tick churn even on a real TTY. This complements the
	// auto-detection in ui.New (which fires for non-TTY writers and CI=true)
	// so users running headlessly on a workstation without CI set still get
	// log-oriented output.
	if opts.NoInteractive {
		f.UI.MarkHeadless()
	}

	// Detailed narration is suppressed from the terminal during the run so the
	// output stays limited to phase labels, prompts, and the final summary.
	// A structured, action-centric provenance record (what was resolved and
	// how) is written at the end instead. JSON mode keeps the narration log
	// attached so machine-readable events can flow on stderr.
	if opts.JSONFields == "" && opts.Format == "" {
		f.UI.SetLog(io.Discard)
	}

	r, err := f.NewResolver(resolveHostname(opts.Hostname))
	if err != nil {
		return err
	}
	store, err := lockfile.OpenStore(".", r)
	if err != nil {
		return fmt.Errorf("opening lockfile: %w", err)
	}
	// Seed branch hints from the existing lockfile so repeat scans short-circuit
	// the per-branch Compare walk when the recorded branch still contains the SHA.
	r.SeedBranchHints(store.AllDeps())
	// Two-phase scan/resolve.
	//
	// Phase 1 (Scan): parse every workflow with a clean `Scanning [i/n] path`
	// label. No resolver progress or worker rows leak under the bar because
	// nothing in this phase calls the resolver.
	//
	// Phase 2 (Resolve): pre-warm the resolver caches once for the union of
	// refs and deps. The resolver's per-action worker UX takes over.
	//
	// Phase 3 (Diagnose): per-workflow engine diagnostics that hit the warmed
	// caches and stay silent.
	total := len(opts.WorkflowPaths)
	// showSpinner gates the interactive spinner + per-action worker rows +
	// resolver progress callbacks. We restrict it to non-JSON TTY runs:
	//
	//   * JSON mode keeps stderr quiet so consumers can pipe `2>/dev/null`.
	//   * Headless mode (CI, non-TTY) gets its phase markers directly from
	//     showHeadlessProgress below, which doesn't install
	//     r.WorkerProgressFn. That matters because a non-nil
	//     WorkerProgressFn flips the resolver from a single batched GraphQL
	//     call to per-ref parallel calls — a UX choice that should not leak
	//     into headless output (and that would, e.g., quadruple the request
	//     count for test stubs registered against the batched query shape).
	showSpinner := opts.JSONFields == "" && opts.Format == "" && !f.UI.Headless()
	showHeadlessProgress := f.UI.Headless()

	var onScan func(done, total int, path string)
	if showSpinner {
		label := fmt.Sprintf("Scanning %d %s", total, ui.Pluralize(total, "workflow", "workflows"))
		f.UI.StartProgress(label)
		onScan = func(done, total int, path string) {
			f.UI.UpdateLabel(fmt.Sprintf("Scanning [%d/%d] %s", done, total, path))
			f.UI.UpdateProgress("")
		}
	} else if showHeadlessProgress {
		// Headless: emit one line per phase directly, no callbacks. The UI
		// layer dedupes label stems so subsequent UpdateLabel calls with the
		// same phase name are no-ops.
		f.UI.StartProgress(fmt.Sprintf("Scanning %d %s", total, ui.Pluralize(total, "workflow", "workflows")))
	}

	parsed := doctor.ParseAll(opts.WorkflowPaths, store, onScan)

	// Fast path: unless the user asked for a full --rescan, validate
	// fully-recorded workflows purely from disk. Their refs all match
	// lockfile entries, so we trust the prior verification and skip every
	// network round-trip (no live resolve, no reachability check). Network
	// work is restricted to workflows that are actually changing or adding
	// pins — true for both default and explicit-scope runs. Pass `--rescan`
	// to re-verify everything end-to-end.
	//
	// For workflows that don't qualify for the full fast path (at least one
	// direct ref is new or changed), we still avoid per-dep reachability
	// network calls for the unchanged pins in that workflow:
	// SkipReachWhenUnchanged tells diagnose to synthesize Reachable for any
	// ExistingDep whose live-resolved SHA still matches the recorded SHA.
	// Without this, a single new dep in a workflow with 20 transitive pins
	// triggers ~20 list-branches + ~N compare calls per pin — most of which
	// re-prove what the lockfile already records.
	skippedRescan := 0
	if !opts.Rescan {
		for i := range parsed {
			if isFullyRecorded(parsed[i]) {
				parsed[i].TrustLockfile = true
				skippedRescan++
			} else {
				parsed[i].SkipReachWhenUnchanged = true
			}
		}
	}

	// Network-bound resolve+reach only sees the non-trusted workflows. When
	// every workflow is trusted (the steady-state happy path), refs/deps
	// are both empty and we make zero network calls.
	var networked []doctor.ParsedWorkflow
	for _, pw := range parsed {
		if !pw.TrustLockfile {
			networked = append(networked, pw)
		}
	}
	refs, deps := doctor.CollectResolvable(networked)
	if showSpinner {
		f.UI.UpdateProgress("")
		f.UI.ClearWorkerStatuses()
		// Wire structured counter callbacks so the top label expresses one
		// rolling total per phase. The resolver no longer rewrites the label
		// itself; we own the phrasing so the bar never jumps between
		// "resolving" and "transitive resolving" — transitive is just deeper
		// edges in the same Resolve phase.
		r.OnResolveProgress = func(done, total int) {
			f.UI.UpdateLabel(fmt.Sprintf("Resolving actions [%d/%d]", done, total))
		}
		r.OnVerifyProgress = func(done, total int) {
			f.UI.UpdateLabel(fmt.Sprintf("Verifying reachability [%d/%d]", done, total))
		}
		r.WorkerProgressFn = func(slot int, status string) { f.UI.SetWorkerStatus(slot, status) }
	} else if showHeadlessProgress && (len(refs) > 0 || (opts.Rescan && len(deps) > 0)) {
		// Headless: announce the phase once, without per-ref worker callbacks.
		// Leave r.WorkerProgressFn unset so the resolver stays in
		// batched-GraphQL mode.
		f.UI.UpdateLabel("Resolving actions")
	}
	if len(refs) > 0 {
		_, _, _ = r.ResolveAllRecursive(refs)
	}
	// Pre-warm reachability across all networked workflows in one shot. The
	// per-workflow diagnose pass also calls CheckReachabilityAll, but doing
	// it once here lets the resolver pool per-repo branch/default-branch
	// warmup and per-dep Compare concurrency across every workflow that
	// needs verification — instead of running each workflow's small batch
	// serially. The resolver's reach cache makes the per-workflow calls
	// downstream effectively free hits.
	//
	// On --rescan the pre-warm intentionally includes every recorded pin
	// (verifies everything end-to-end). On the default path it's the
	// deduplicated union of "needs fresh check" deps per CollectReachDeps
	// (new pins + pins whose live SHA differs from the lockfile). On a
	// fully steady-state lockfile this set is empty and we make zero reach
	// network calls here.
	var reachDeps []lockfile.Dependency
	var liveMoved []lockfile.Dependency
	if opts.Rescan {
		reachDeps = deps
		// Even on --rescan, the live-SHA reach sweep only matters
		// when the union has lockfile entries to compare against;
		// new-only scans have nothing to "move".
		if len(networked) > 0 {
			live, _, _ := r.ResolveAllRecursive(refs)
			liveMoved = doctor.CollectLiveMovedReachDeps(networked, live)
		}
	} else {
		// One resolve, two uses: drive the locked-SHA partition AND
		// the live-SHA moved-set. Cache makes the call O(1) per ref;
		// hoisting it avoids two resolver round-trips.
		live, _, _ := r.ResolveAllRecursive(refs)
		reachDeps = doctor.CollectReachDeps(networked, live)
		liveMoved = doctor.CollectLiveMovedReachDeps(networked, live)
	}
	if len(reachDeps) > 0 || len(liveMoved) > 0 {
		if showHeadlessProgress {
			f.UI.UpdateLabel("Verifying reachability")
		}
		if len(reachDeps) > 0 {
			_ = r.CheckReachabilityAll(reachDeps)
		}
		if len(liveMoved) > 0 {
			_ = r.CheckReachabilityAll(liveMoved)
		}
	}

	if showSpinner {
		// Quiet the resolver before per-workflow diagnostics — they hit cache
		// and shouldn't repaint label/workers.
		r.OnResolveProgress = nil
		r.OnVerifyProgress = nil
		r.WorkerProgressFn = nil
		f.UI.ClearWorkerStatuses()
		f.UI.UpdateLabel("Analyzing")
		f.UI.UpdateProgress("")
	} else if showHeadlessProgress {
		f.UI.UpdateLabel("Analyzing")
	}

	// Build a shared REST client + TagLister up-front. The diagnostics
	// engine now uses the resolver's PeelTagObject directly to recognize
	// annotated-tag-object SHA pins, so no tagger is needed for that path.
	// The TagLister is still reused by EnrichImpostorFindings and the
	// Remediator (best-patch-for-SHA lookups, release tag hints) so we
	// don't refetch tags downstream.
	hostname := resolveHostname(opts.Hostname)
	var tagger *doctor.TagLister
	var sharedRestClient *api.RESTClient
	if rc, err := api.NewRESTClient(api.ClientOptions{Host: hostname}); err == nil {
		sharedRestClient = rc
		tagger = doctor.NewTagLister(rc)
	}

	report := doctor.DiagnoseParsed(parsed, r, store)

	// Compute validity from findings.
	valid := report.IsValid()

	// Enrich impostor-commit findings with a suggested re-pin target — the
	// most recent stable release whose commit is still reachable from a
	// branch in the action repo. Bounded network walk per affected action;
	// skipped entirely when no impostor findings exist.
	if hasImpostorFindings(report) {
		if tagger != nil {
			doctor.EnrichImpostorFindings(report, tagger, r)
		} else if rc, err := api.NewRESTClient(api.ClientOptions{Host: hostname}); err == nil {
			tl := doctor.NewTagLister(rc)
			doctor.EnrichImpostorFindings(report, tl, r)
		}
	}

	// JSON output — always before any human-readable output.
	if opts.JSONFields != "" {
		f.UI.StopProgress()
		if err := format.WriteJSON(f.Out, report, valid, opts.JSONFields, cliVersion(), store.File().Version); err != nil {
			return err
		}
		if !valid {
			return errSilent
		}
		return nil
	}

	// SARIF output — write to file (or stdout when `-`). Like JSON mode,
	// nothing else prints; SARIF consumers want exactly one document on
	// the configured sink. Exit code mirrors JSON mode so CI gates on
	// the same signal regardless of format.
	if opts.Format == "sarif" {
		f.UI.StopProgress()
		out := f.Out
		var closeFn func() error
		if opts.OutputPath != "-" {
			fh, err := os.Create(opts.OutputPath)
			if err != nil {
				return fmt.Errorf("opening --output: %w", err)
			}
			out = fh
			closeFn = fh.Close
		}
		if err := format.WriteSARIF(out, report, cliVersion()); err != nil {
			if closeFn != nil {
				_ = closeFn()
			}
			return err
		}
		if closeFn != nil {
			if err := closeFn(); err != nil {
				return fmt.Errorf("closing --output: %w", err)
			}
		}
		if !valid {
			return errSilent
		}
		return nil
	}

	// Determine if interactive remediation will follow.
	interactive := !opts.NoInteractive && os.Getenv("CI") != "true" && f.IsTerminal()

	// In interactive mode the summary and prompts render on the terminal, so
	// stop the checking spinner now. In non-interactive mode keep it running
	// across the phase transition into remediation: all narration goes to the
	// log, so the terminal stays a single continuous spinner with no gap before
	// pinning begins. The remediator adopts the running spinner; check.go stops
	// it after remediation, just before the terminal summary.
	if interactive {
		f.UI.StopProgress()
	}

	// Always remediate — non-interactive mode auto-fixes what it can.
	willRemediate := true

	// Human-readable output.
	format.PresentResults(f.UI, report, valid, willRemediate)

	// Remediation.
	actionable := report.WorkflowsNeedingAttention()
	var fixedCount, skippedCount, alertedCount, unresolvedCount int
	var skippedDeps, alertedDeps, unresolvedDeps []string
	var alertedWorkflows map[string][]string
	var alertedReasons map[string]string
	var alertedSuggestions map[string]string
	var alertedSearched map[string]bool
	var fullScanDeps []string
	var autoFixedImpostors []doctor.AutoFixedImpostor
	printed := false

	var repoOwner, repoName string
	if currentRepo, err := repository.Current(); err == nil {
		repoOwner = currentRepo.Owner
		repoName = currentRepo.Name
	}

	if willRemediate && len(actionable) > 0 {
		restClient := sharedRestClient
		if restClient == nil {
			rc, err := api.NewRESTClient(api.ClientOptions{Host: hostname})
			if err != nil {
				return fmt.Errorf("creating REST client: %w", err)
			}
			restClient = rc
		}

		var prompter doctor.Prompter
		if !interactive {
			prompter = &doctor.NoopPrompter{}
		} else {
			hp := doctor.NewHuhPrompterWithWriter(f.ErrOut, f.IsTerminal)
			// Let prompts pause the continuous pinning spinner while they own
			// the terminal, then resume it — no blank gaps between workflows.
			hp.SetProgress(f.UI)
			prompter = hp
		}

		rem := doctor.NewRemediator(prompter, r, restClient, store, f.UI, doctor.RemediateOptions{
			Interactive: interactive,
			RepoOwner:   repoOwner,
			RepoName:    repoName,
		})

		if err := rem.Remediate(report); err != nil {
			f.UI.StopProgress()
			if errors.Is(err, doctor.ErrAborted) {
				f.UI.TermWarn("Interrupted — no further changes applied")
				return nil
			}
			return err
		}

		// Pass C: convert any alerted impostors that already carry a
		// sane-release suggestion into actual rewrites + re-pins. Runs
		// after Remediate so applyPin/applySHAToTag have had a chance to
		// surface the impostor signal first; the rewrites consume the
		// alerted-suggestions map populated by alertImpostor.
		rem.AutoFixAlertedImpostors()

		if err := store.Save(); err != nil {
			return fmt.Errorf("saving lockfile: %w", err)
		}

		uniqueSkipped := len(rem.SkippedDeps)
		if rem.Fixed > 0 {
			f.UI.TermSuccess("%d %s fixed", rem.Fixed, ui.Pluralize(rem.Fixed, "issue", "issues"))
			printed = true
		}
		if uniqueSkipped > 0 {
			f.UI.TermWarn("%d %s skipped", uniqueSkipped, ui.Pluralize(uniqueSkipped, "action", "actions"))
			printed = true
		}
		if rem.Unresolved > 0 {
			f.UI.TermWarn("%d %s could not be resolved", rem.Unresolved, ui.Pluralize(rem.Unresolved, "action", "actions"))
			printed = true
		}
		fixedCount = rem.Fixed
		skippedCount = uniqueSkipped
		alertedCount = rem.Alerted
		unresolvedCount = rem.Unresolved
		skippedDeps = rem.SkippedDeps
		alertedDeps = rem.AlertedDeps
		alertedWorkflows = rem.AlertedWorkflows
		alertedReasons = rem.AlertedReasons
		alertedSuggestions = rem.AlertedSuggestions
		alertedSearched = rem.AlertedSearched
		fullScanDeps = rem.FullScanDeps
		unresolvedDeps = rem.UnresolvedDeps
		autoFixedImpostors = rem.AutoFixedImpostors
	}

	// Terminal end-state: spinners and narration are done; print the summary.
	// Stop any spinner still running (non-interactive path keeps it alive
	// through remediation, or no workflows were actionable) before Term* output
	// writes directly to the terminal.
	f.UI.StopProgress()

	// Write a structured, action-centric provenance record — what was resolved
	// and how, deduplicated to one entry per action — and point the user at it.
	// Skipped in --json mode, which already emitted machine-readable output.
	if opts.JSONFields == "" {
		repoInfo := &runlog.RepoInfo{Owner: repoOwner, Name: repoName, Host: resolveHostname(opts.Hostname)}
		outcomes := newProvenanceOutcomes(alertedDeps, skippedDeps, unresolvedDeps, fullScanDeps, alertedReasons)
		prov := buildProvenanceReport(report, store, valid, repoInfo, outcomes)
		if path, werr := runlog.WriteReport(prov); werr == nil {
			defer func() { f.UI.TermDetail("Resolution record: %s", path) }()
		}
	}

	// Surface any commits that were pinned only after a full-branch-scan
	// fallback: they are valid but not on a canonical branch, which is worth
	// a red heads-up even when the run otherwise succeeds.
	if len(fullScanDeps) > 0 {
		if printed {
			f.UI.TermBlank()
		}
		printed = true
		f.UI.TermCaution("%d %s pinned but not on a canonical branch — verified via full branch scan:",
			len(fullScanDeps), ui.Pluralize(len(fullScanDeps), "action", "actions"))
		for _, dep := range fullScanDeps {
			f.UI.TermDetail("  %s", f.UI.TermYellow(dep))
		}
	}

	// Surface auto-fixed impostors: refs whose pinned commit was unreachable
	// from any branch but where a recent stable release tag was reachable, so
	// pin rewrote uses: to that tag instead of alerting. The substitution may
	// cross a major-version boundary (e.g. v1.25.0 → v3.0.3) so the user
	// must eyeball each one. Surface enough links to investigate without
	// leaving the terminal: the impostor commit (with GitHub's own
	// "doesn't belong to any branch" copy), the release the workflow now
	// points at, and a diff between old and new.
	if len(autoFixedImpostors) > 0 {
		if printed {
			f.UI.TermBlank()
		}
		printed = true
		f.UI.TermWarn("%d %s auto-pinned to a safer release — review for sanity:",
			len(autoFixedImpostors), ui.Pluralize(len(autoFixedImpostors), "action", "actions"))
		f.UI.TermDetail("  The original tag pointed at a commit that doesn't belong to any branch")
		f.UI.TermDetail("  on the upstream repository, and may belong to a fork outside of it.")
		f.UI.TermDetail("  Each was re-pinned to the latest release reachable from a branch.")
		for _, fix := range autoFixedImpostors {
			short := fix.NewSHA
			if len(short) > 7 {
				short = short[:7]
			}
			f.UI.TermDetail("  %s: %s → %s (%s)", fix.NWO, fix.OldRef, fix.NewTag, short)
			f.UI.TermDetail("    in %s", fix.Workflow)
			if fix.OldSHA != "" {
				commitURL := fmt.Sprintf("https://github.com/%s/commit/%s", fix.NWO, fix.OldSHA)
				f.UI.TermDetail("    impostor commit: %s", f.UI.TermLink(commitURL, commitURL))
				f.UI.TermDetail("      \"This commit does not belong to any branch on this repository, and may belong to a fork outside of the repository.\"")
				if fix.NewSHA != "" {
					compareURL := fmt.Sprintf("https://github.com/%s/compare/%s...%s", fix.NWO, fix.OldSHA, fix.NewSHA)
					f.UI.TermDetail("    compare:         %s", f.UI.TermLink(compareURL, compareURL))
				}
			}
			releaseURL := fmt.Sprintf("https://github.com/%s/releases/tag/%s", fix.NWO, fix.NewTag)
			f.UI.TermDetail("    new release:     %s", f.UI.TermLink(releaseURL, releaseURL))
		}
		f.UI.TermDetail("  Publishers: %s", doctor.PublisherTagReleasesDocURL)
	}

	if valid && fixedCount == 0 && skippedCount == 0 && alertedCount == 0 && unresolvedCount == 0 {
		f.UI.TermSuccess("All %d %s valid", total, ui.Pluralize(total, "workflow", "workflows"))
		if skippedRescan > 0 {
			f.UI.TermDetail("Trusted lockfile for %d already-pinned %s; run `gh actions-pin --rescan` to re-verify reachability.",
				skippedRescan, ui.Pluralize(skippedRescan, "workflow", "workflows"))
		}
		return nil
	}

	if !valid || skippedCount > 0 || alertedCount > 0 || unresolvedCount > 0 {
		// Exit 0 only if everything was resolved — nothing skipped, alerted, or unresolved.
		if fixedCount > 0 && skippedCount == 0 && alertedCount == 0 && unresolvedCount == 0 {
			return nil
		}
		if alertedCount > 0 {
			if printed {
				f.UI.TermBlank()
			}
			printed = true
			f.UI.TermError("%d %s %s investigation — do not auto-fix:",
				alertedCount, ui.Pluralize(alertedCount, "action", "actions"), ui.Pluralize(alertedCount, "requires", "require"))

			// Group deps by reason, preserving first-seen order.
			var reasonOrder []string
			byReason := map[string][]string{}
			for _, dep := range alertedDeps {
				reason := alertedReasons[dep]
				if _, seen := byReason[reason]; !seen {
					reasonOrder = append(reasonOrder, reason)
				}
				byReason[reason] = append(byReason[reason], dep)
			}
			for _, reason := range reasonOrder {
				if reason != "" {
					f.UI.TermDetail("  %s", f.UI.TermBold(reason))
				}
				indent := "  "
				if reason != "" {
					indent = "    "
				}
				for _, dep := range byReason[reason] {
					f.UI.TermDetail("%s%s", indent, f.UI.TermLink(f.UI.TermYellow(dep), format.DepReleaseURL(dep, r.IsKnownTagObject)))
					paths := alertedWorkflows[dep]
					if len(paths) == 0 {
						paths = format.WorkflowsForDep(report, dep)
					}
					for _, path := range paths {
						f.UI.TermDetail("%s  └─ %s", indent, f.UI.TermDim(path))
					}
					if sug := alertedSuggestions[dep]; sug != "" {
						nwo := dep
						if i := strings.IndexByte(dep, '@'); i >= 0 {
							nwo = dep[:i]
						}
						// sug is "tag short-sha"; split for clean display.
						tag, sha := sug, ""
						if sp := strings.IndexByte(sug, ' '); sp >= 0 {
							tag = sug[:sp]
							sha = sug[sp+1:]
						}
						display := nwo + "@" + tag
						if sha != "" {
							display += " (" + sha + ")"
						}
						f.UI.TermDetail("%s  %s suggested: re-pin to %s — latest release reachable from a branch",
							indent, f.UI.TermBold("→"), f.UI.TermYellow(display))
					} else if alertedSearched[dep] {
						f.UI.TermDetail("%s  %s no recent release was reachable from a branch — escalate to the action publisher",
							indent, f.UI.TermBold("→"))
					}
					if alertedSearched[dep] {
						f.UI.TermDetail("%s     publishers: %s", indent, f.UI.TermDim(doctor.PublisherTagReleasesDocURL))
					}
				}
			}
		}
		if skippedCount > 0 {
			if printed {
				f.UI.TermBlank()
			}
			printed = true
			f.UI.TermError("%d %s %s interactive resolution — run `gh actions-pin` locally:",
				skippedCount, ui.Pluralize(skippedCount, "action", "actions"), ui.Pluralize(skippedCount, "requires", "require"))
			for _, dep := range skippedDeps {
				f.UI.TermDetail("  %s", f.UI.TermLink(f.UI.TermYellow(dep), format.DepReleaseURL(dep, r.IsKnownTagObject)))
			}
		}
		if unresolvedCount > 0 {
			if printed {
				f.UI.TermBlank()
			}
			f.UI.TermError("%d %s could not be resolved — verify the ref exists (tags are often prefixed with `v`):",
				unresolvedCount, ui.Pluralize(unresolvedCount, "action", "actions"))
			for _, dep := range unresolvedDeps {
				f.UI.TermDetail("  %s", f.UI.TermLink(f.UI.TermYellow(dep), format.DepReleaseURL(dep, r.IsKnownTagObject)))
			}
		}
		return errSilent
	}
	return nil
}

// isFullyRecorded reports whether every direct action ref in the workflow is
// already present in the lockfile-recorded deps for that workflow. It's the
// gate for the fast path: when true, we trust the prior verification and
// skip the network round-trip to re-verify reachability. Parse/load errors
// or refs missing from the lockfile force a full re-verification.
func isFullyRecorded(pw doctor.ParsedWorkflow) bool {
	if pw.LoadErr != nil || pw.DepsErr != nil {
		return false
	}
	if len(pw.Refs) == 0 {
		return true
	}
	haveDep := make(map[string]bool, len(pw.ExistingDeps))
	for _, d := range pw.ExistingDeps {
		haveDep[d.NWO+"@"+d.Ref] = true
	}
	for _, r := range pw.Refs {
		if !haveDep[r.Owner+"/"+r.Repo+"@"+r.Ref] {
			return false
		}
	}
	return true
}

// hasImpostorFindings reports whether any workflow in the report carries a
// CategoryImpostorCommit finding. Used to gate the bounded tag-walk that
// enriches those findings with a sane-release suggestion.
func hasImpostorFindings(r *doctor.Report) bool {
	if r == nil {
		return false
	}
	for _, wr := range r.Workflows {
		for _, f := range wr.Findings {
			if f.Category == doctor.CategoryImpostorCommit {
				return true
			}
		}
	}
	return false
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
