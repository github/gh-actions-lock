package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime/debug"
	"strings"

	"github.com/MakeNowJust/heredoc"
	"github.com/cli/go-gh/v2/pkg/api"
	"github.com/cli/go-gh/v2/pkg/repository"
	"github.com/github/gh-actions-pin/internal/doctor"
	"github.com/github/gh-actions-pin/internal/lockfile"
	"github.com/github/gh-actions-pin/internal/runlog"
	"github.com/github/gh-actions-pin/internal/ui"
	"github.com/spf13/cobra"
)

type checkOptions struct {
	WorkflowPaths []string
	JSONFields    string
	Hostname      string
	NoInteractive bool
	// Rescan forces a full reachability re-verification of every recorded
	// pin, bypassing the fast path that trusts the lockfile. Useful for
	// audits or when a CI policy requires re-attestation on every run.
	Rescan bool
}

// JSON output types — thin wrappers around doctor.Report.

type checkFinding struct {
	Workflow    string `json:"workflow"`
	Category    string `json:"category"`
	Severity    string `json:"severity"`
	Dependency  string `json:"dependency,omitempty"`
	RequiredBy  string `json:"required_by,omitempty"`
	Detail      string `json:"detail"`
	Remediation string `json:"remediation,omitempty"`
	DocURL      string `json:"doc_url,omitempty"`
}

type checkDependency struct {
	NWO        string   `json:"nwo"`
	Ref        string   `json:"ref"`
	SHA        string   `json:"sha"`
	HashAlgo   string   `json:"hash_algo,omitempty"`
	Direct     bool     `json:"direct"`
	RequiredBy []string `json:"required_by,omitempty"`
}

type checkWorkflow struct {
	Path         string            `json:"path"`
	Valid        bool              `json:"valid"`
	Findings     []checkFinding    `json:"findings"`
	Dependencies []checkDependency `json:"dependencies,omitempty"`
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
			  REF_MOVED      - locked SHA no longer matches upstream (expected for mutable tags like v4)
			  MISSING       - action in workflow has no lock entry
			  STALE         - lock entry references an action no longer in the workflow
			  REF_CHANGED   - workflow ref was edited; lock needs updating
			  MISLEADING_SHA - ref looks like a SHA but resolves to a different commit
			  IMPOSTER_COMMIT   - locked SHA is not in the ref's history
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
	cmd.Flags().StringVar(&opts.Hostname, "hostname", "", "GitHub hostname to query (defaults to GH_HOST, current repo host, or github.com)")
	cmd.Flags().BoolVar(&opts.NoInteractive, "no-interactive", false, "Auto-fix deterministic issues; fail on issues requiring human input")
	cmd.Flags().BoolVar(&opts.Rescan, "rescan", false, "Re-verify reachability for every recorded pin (bypasses the lockfile fast path)")
	return cmd
}

func runCheck(f *pinFactory, opts *checkOptions) error {
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
	if opts.JSONFields == "" {
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
	showSpinner := opts.JSONFields == "" && !f.UI.Headless()
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
		// We deliberately leave r.WorkerProgressFn unset so the resolver
		// stays in batched-GraphQL mode.
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
	if opts.Rescan {
		reachDeps = deps
	} else {
		// Re-resolve to populate liveDeps for the union. The cache makes
		// this O(1) per ref; we use the result purely to drive the
		// partition.
		live, _, _ := r.ResolveAllRecursive(refs)
		reachDeps = doctor.CollectReachDeps(networked, live)
	}
	if len(reachDeps) > 0 {
		if showHeadlessProgress {
			f.UI.UpdateLabel("Verifying reachability")
		}
		_ = r.CheckReachabilityAll(reachDeps)
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
	// The TagLister is still reused by EnrichImposterFindings and the
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

	// Enrich imposter-commit findings with a suggested re-pin target — the
	// most recent stable release whose commit is still reachable from a
	// branch in the action repo. Bounded network walk per affected action;
	// skipped entirely when no imposter findings exist.
	if hasImposterFindings(report) {
		if tagger != nil {
			doctor.EnrichImposterFindings(report, tagger, r)
		} else if rc, err := api.NewRESTClient(api.ClientOptions{Host: hostname}); err == nil {
			tl := doctor.NewTagLister(rc)
			doctor.EnrichImposterFindings(report, tl, r)
		}
	}

	// JSON output — always before any human-readable output.
	if opts.JSONFields != "" {
		f.UI.StopProgress()
		return writeCheckJSON(f.Out, report, valid, opts.JSONFields, store.File().Version)
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
	presentCheckResults(f.UI, report, valid, willRemediate)

	// Remediation.
	actionable := report.WorkflowsNeedingAttention()
	var fixedCount, skippedCount, alertedCount, unresolvedCount int
	var skippedDeps, alertedDeps, unresolvedDeps []string
	var alertedWorkflows map[string][]string
	var alertedReasons map[string]string
	var alertedSuggestions map[string]string
	var alertedSearched map[string]bool
	var fullScanDeps []string
	var autoFixedImposters []doctor.AutoFixedImposter
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
		// alerted-suggestions map populated by alertImposter.
		rem.AutoFixAlertedImposters()

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
		autoFixedImposters = rem.AutoFixedImposters
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
	// must eyeball each one — flag it loudly with the publisher-docs link.
	if len(autoFixedImposters) > 0 {
		if printed {
			f.UI.TermBlank()
		}
		printed = true
		f.UI.TermWarn("%d %s auto-pinned to a safer release — review for sanity:",
			len(autoFixedImposters), ui.Pluralize(len(autoFixedImposters), "action", "actions"))
		for _, fix := range autoFixedImposters {
			short := fix.NewSHA
			if len(short) > 7 {
				short = short[:7]
			}
			f.UI.TermDetail("  %s: %s → %s (%s)", fix.NWO, fix.OldRef, fix.NewTag, short)
			f.UI.TermDetail("    in %s", fix.Workflow)
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
					f.UI.TermDetail("%s%s", indent, f.UI.TermLink(f.UI.TermYellow(dep), depReleaseURL(dep, r)))
					paths := alertedWorkflows[dep]
					if len(paths) == 0 {
						paths = workflowsForDep(report, dep)
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
				f.UI.TermDetail("  %s", f.UI.TermLink(f.UI.TermYellow(dep), depReleaseURL(dep, r)))
			}
		}
		if unresolvedCount > 0 {
			if printed {
				f.UI.TermBlank()
			}
			f.UI.TermError("%d %s could not be resolved — verify the ref exists (tags are often prefixed with `v`):",
				unresolvedCount, ui.Pluralize(unresolvedCount, "action", "actions"))
			for _, dep := range unresolvedDeps {
				f.UI.TermDetail("  %s", f.UI.TermLink(f.UI.TermYellow(dep), depReleaseURL(dep, r)))
			}
		}
		return errSilent
	}
	return nil
}

// findingToJSON converts a doctor.Finding to a JSON-safe struct.
func findingToJSON(f doctor.Finding) checkFinding {
	jf := checkFinding{
		Workflow:    f.WorkflowPath,
		Category:    string(f.Category),
		Severity:    string(f.Severity),
		Detail:      f.Detail,
		Remediation: f.Remediation,
		DocURL:      f.DocURL,
	}
	if f.Dependency != nil {
		jf.Dependency = f.Dependency.Key()
	} else if f.ActionRef != nil {
		jf.Dependency = f.ActionRef.FullName() + "@" + f.ActionRef.Ref
	}
	if f.ParentNWO != "" {
		jf.RequiredBy = f.ParentNWO
	}
	return jf
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

// writeCheckJSON writes the unified JSON output.
func writeCheckJSON(w io.Writer, report *doctor.Report, valid bool, fieldsCSV, lockfileVersion string) error {
	fields := strings.Split(fieldsCSV, ",")

	// Build all data lazily.
	var allFindings []checkFinding
	var allDeps []checkDependency
	var allWorkflows []checkWorkflow

	buildFindings := func() []checkFinding {
		if allFindings != nil {
			return allFindings
		}
		allFindings = []checkFinding{}
		for _, f := range report.RepoFindings {
			allFindings = append(allFindings, findingToJSON(f))
		}
		for _, wr := range report.Workflows {
			for _, f := range wr.Findings {
				if f.Category == doctor.CategoryRunOnly || (f.Category == doctor.CategoryValid && f.Severity == doctor.SeverityOK) {
					continue
				}
				allFindings = append(allFindings, findingToJSON(f))
			}
		}
		return allFindings
	}

	buildDeps := func() []checkDependency {
		if allDeps != nil {
			return allDeps
		}
		allDeps = []checkDependency{}
		// Deduplicate across workflows, merging required_by lists.
		seen := make(map[string]*checkDependency)
		var order []string
		for _, wr := range report.Workflows {
			for _, inv := range wr.Inventory {
				key := inv.Dep.Key()
				if existing, ok := seen[key]; ok {
					// Merge required_by lists.
					for _, p := range inv.Parents {
						found := false
						for _, ep := range existing.RequiredBy {
							if ep == p {
								found = true
								break
							}
						}
						if !found {
							existing.RequiredBy = append(existing.RequiredBy, p)
						}
					}
					// If direct in any workflow, mark as direct.
					if inv.Direct {
						existing.Direct = true
					}
					continue
				}
				d := checkDependency{
					NWO:        inv.Dep.NWO,
					Ref:        inv.Dep.Ref,
					SHA:        inv.Dep.SHA,
					HashAlgo:   inv.Dep.HashAlgo,
					Direct:     inv.Direct,
					RequiredBy: inv.Parents,
				}
				seen[key] = &d
				order = append(order, key)
			}
		}
		for _, key := range order {
			allDeps = append(allDeps, *seen[key])
		}
		return allDeps
	}

	buildWorkflows := func() []checkWorkflow {
		if allWorkflows != nil {
			return allWorkflows
		}
		allWorkflows = []checkWorkflow{}
		for _, wr := range report.Workflows {
			wf := checkWorkflow{
				Path:     wr.Path,
				Valid:    wr.IsValid(),
				Findings: []checkFinding{},
			}
			for _, f := range wr.Findings {
				if f.Category == doctor.CategoryRunOnly || (f.Category == doctor.CategoryValid && f.Severity == doctor.SeverityOK) {
					continue
				}
				wf.Findings = append(wf.Findings, findingToJSON(f))
			}
			for _, inv := range wr.Inventory {
				wf.Dependencies = append(wf.Dependencies, checkDependency{
					NWO:        inv.Dep.NWO,
					Ref:        inv.Dep.Ref,
					SHA:        inv.Dep.SHA,
					HashAlgo:   inv.Dep.HashAlgo,
					Direct:     inv.Direct,
					RequiredBy: inv.Parents,
				})
			}
			allWorkflows = append(allWorkflows, wf)
		}
		return allWorkflows
	}

	payload := map[string]interface{}{
		"cli_version":      cliVersion(),
		"lockfile_version": lockfileVersion,
	}
	for _, field := range fields {
		field = strings.TrimSpace(field)
		switch field {
		case "valid":
			payload[field] = valid
		case "findings":
			payload[field] = buildFindings()
		case "dependencies":
			payload[field] = buildDeps()
		case "workflows":
			payload[field] = buildWorkflows()
		default:
			return fmt.Errorf("unknown JSON field %q (expected valid, findings, workflows, dependencies)", field)
		}
	}

	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(payload); err != nil {
		return err
	}
	if !valid {
		return errSilent
	}
	return nil
}

// presentCheckResults renders human-readable output from a doctor report.
func presentCheckResults(out *ui.UI, report *doctor.Report, valid bool, willRemediate bool) {
	for _, f := range report.RepoFindings {
		out.Warning("%s", f.Detail)
		if f.DocURL != "" {
			out.Detail("  see: %s", out.DocLink(f.DocURL))
		}
	}

	var validCount, failedCount int
	for _, wr := range report.Workflows {
		if wr.IsValid() {
			validCount++
		} else {
			failedCount++
		}
	}
	checked := validCount + failedCount

	if valid && checked > 0 {
		out.Success("All %d %s valid", checked, ui.Pluralize(checked, "workflow", "workflows"))
	} else if checked > 0 {
		// Collect error findings grouped by dependency.
		type depGroup struct {
			dep      string
			findings []doctor.Finding
		}
		var depOrder []string
		depMap := map[string]*depGroup{}

		for _, wr := range report.Workflows {
			for _, f := range wr.Findings {
				if f.IsValid() {
					continue
				}
				depKey := f.DepKey()
				if dg, ok := depMap[depKey]; ok {
					dg.findings = append(dg.findings, f)
				} else {
					depOrder = append(depOrder, depKey)
					depMap[depKey] = &depGroup{dep: depKey, findings: []doctor.Finding{f}}
				}
			}
		}

		// Count categories; render per-dep detail only for non-NOT_PINNED.
		catCounts := map[doctor.Category]int{}
		for _, dep := range depOrder {
			dg := depMap[dep]

			// Tally and check if this dep is NOT_PINNED only.
			allNotPinned := true
			for _, f := range dg.findings {
				catCounts[f.Category]++
				if f.Category != doctor.CategoryNotPinned {
					allNotPinned = false
				}
			}
			if allNotPinned {
				continue // pure aggregation — no per-dep output
			}

			// Render per-dep detail for actionable categories.
			for _, f := range dg.findings {
				if f.Category == doctor.CategoryNotPinned {
					continue // skip NOT_PINNED lines in mixed groups too
				}
				label := strings.ToUpper(string(f.Category))
				icon := "!"
				if isAlertedCategory(f.Category) {
					icon = "✗"
				}
				out.Detail("%s %s %s", icon, out.Dim(label), dep)
				out.Detail("  %s", f.Detail)
				if f.Category == doctor.CategoryLockfileForgery && f.Dependency != nil {
					owner, repo := f.Dependency.OwnerRepo()
					if owner != "" {
						out.Detail("  → %s", out.Dim(fmt.Sprintf("https://github.com/%s/%s/releases", owner, repo)))
					}
				}
				if isAlertedCategory(f.Category) && f.Remediation != "" {
					out.Detail("  %s %s", out.Bold("⚠"), f.Remediation)
				}
				if f.SaneSuggestionTag != "" {
					nwo := ""
					if f.Dependency != nil {
						nwo = f.Dependency.NWO
					}
					sha := f.SaneSuggestionSHA
					if len(sha) > 7 {
						sha = sha[:7]
					}
					out.Detail("  %s suggested re-pin: %s@%s (%s) — latest release reachable from a branch",
						out.Bold("→"), nwo, f.SaneSuggestionTag, sha)
				} else if f.SaneSuggestionSearched {
					out.Detail("  %s no recent release was reachable from a branch — escalate to the action publisher",
						out.Bold("→"))
				}
				if f.SaneSuggestionSearched {
					out.Detail("  publishers: %s", out.DocLink(doctor.PublisherTagReleasesDocURL))
				}
				if f.DocURL != "" {
					out.Detail("  see: %s", out.DocLink(f.DocURL))
				}
			}
		}

		parts := []string{}
		for _, cat := range []doctor.Category{
			doctor.CategoryLockfileForgery,
			doctor.CategoryRefChanged, doctor.CategoryNotPinned,
			doctor.CategoryStale, doctor.CategoryMisleadingSHA, doctor.CategoryImposterCommit,
		} {
			if n, ok := catCounts[cat]; ok {
				parts = append(parts, fmt.Sprintf("%d %s", n, string(cat)))
			}
		}
		out.Error("%d of %d %s failed: %s",
			failedCount, checked,
			ui.Pluralize(checked, "workflow", "workflows"),
			strings.Join(parts, ", "))
		out.Blank()
	}

	// Warnings — deduplicate by dep key.
	type warningGroup struct {
		finding   doctor.Finding
		count     int
		workflows []string
	}
	var warnOrder []string
	warnMap := map[string]*warningGroup{}
	for _, wr := range report.Workflows {
		for _, f := range wr.Findings {
			if f.IsWarning() {
				key := f.DepKey()
				if key == "" {
					key = f.WorkflowPath // workflow-level warnings
				}
				if wg, ok := warnMap[key]; ok {
					wg.count++
					wg.workflows = append(wg.workflows, f.WorkflowPath)
				} else {
					warnOrder = append(warnOrder, key)
					warnMap[key] = &warningGroup{finding: f, count: 1, workflows: []string{f.WorkflowPath}}
				}
			}
		}
		for _, pw := range wr.ParseWarnings {
			out.Warning("%s: %s", wr.Path, pw)
		}
	}

	// Collect workflow-level NOT_PINNED warnings separately for collapsing.
	var unpinnedWorkflows []string
	var otherWarnings []string
	for _, key := range warnOrder {
		wg := warnMap[key]
		f := wg.finding
		if f.Category == doctor.CategoryNotPinned && f.ActionRef == nil {
			unpinnedWorkflows = append(unpinnedWorkflows, f.WorkflowPath)
		} else {
			otherWarnings = append(otherWarnings, key)
		}
	}
	if len(unpinnedWorkflows) > 0 {
		if willRemediate {
			out.Warning("%d %s not yet pinned — resolving below",
				len(unpinnedWorkflows),
				ui.Pluralize(len(unpinnedWorkflows), "workflow", "workflows"))
		} else {
			out.Warning("%d %s not yet pinned (run `gh actions-pin` to fix)",
				len(unpinnedWorkflows),
				ui.Pluralize(len(unpinnedWorkflows), "workflow", "workflows"))
		}
	}
	// Separate SHA_AS_REF warnings into direct (aggregate) and transitive (suppressed).
	// TODO: Transitive deps pinned to bare SHAs are silently swallowed for now.
	// We need to figure out how to coexist better with composite actions that
	// don't use dependency pinning — warning on every transitive dep is noisy
	// and not actionable by the consumer. Revisit when we have a story for
	// composite action authors to adopt pinning.
	// Collect REF_MOVED warnings for compact display.
	var bareSHADeps []string
	var refMovedWarnings []string
	var otherDetailWarnings []string
	for _, key := range otherWarnings {
		wg := warnMap[key]
		f := wg.finding
		if f.Category == doctor.CategorySHAAsRef {
			isTransitive := f.Dependency != nil && f.ActionRef == nil
			if !isTransitive {
				bareSHADeps = append(bareSHADeps, key)
			}
			// transitive SHA_AS_REF: silently swallowed (see TODO above)
		} else if f.Category == doctor.CategoryRefMoved {
			refMovedWarnings = append(refMovedWarnings, key)
		} else if f.Category == doctor.CategoryValid && f.Severity == doctor.SeverityWarning &&
			strings.Contains(f.Remediation, "transitive dependency") {
			// transitive reachability unknown: silently swallowed (see TODO above)
		} else {
			otherDetailWarnings = append(otherDetailWarnings, key)
		}
	}
	if len(bareSHADeps) > 0 {
		out.Warning("%d %s pinned to a bare SHA without a tag ref",
			len(bareSHADeps),
			ui.Pluralize(len(bareSHADeps), "action is", "actions are"))
		if willRemediate {
			out.Detail("  ↳ resolving below")
		} else {
			out.Detail("  ↳ run `gh actions-pin upgrade` to pin to tagged releases")
		}
	}
	if len(refMovedWarnings) > 0 {
		out.Warning("%d %s moved upstream — run `gh actions-pin upgrade` to update",
			len(refMovedWarnings),
			ui.Pluralize(len(refMovedWarnings), "ref has", "refs have"))
		for _, key := range refMovedWarnings {
			wg := warnMap[key]
			f := wg.finding
			out.Detail("  ↳ %s: %s", key, f.Detail)
			if f.Dependency != nil && f.ObservedSHA != "" {
				owner, repo := f.Dependency.OwnerRepo()
				if owner != "" {
					out.Detail("    %s", out.Dim(fmt.Sprintf("https://github.com/%s/%s/compare/%s...%s", owner, repo, f.Dependency.SHA[:12], f.ObservedSHA[:12])))
				}
			}
		}
	}
	for _, key := range otherDetailWarnings {
		wg := warnMap[key]
		f := wg.finding
		depKey := f.DepKey()
		if f.Category == doctor.CategoryValid && f.Severity == doctor.SeverityWarning {
			label := depKey
			if label == "" {
				label = f.WorkflowPath
			}
			out.Warning("%s: %s", label, f.Detail)
		}
	}
}

// isAlertedCategory reports whether a finding category has no auto-fix and
// requires human investigation (already surfaced in presentCheckResults — the
// remediator should not re-print it in non-interactive mode).
func isAlertedCategory(c doctor.Category) bool {
	switch c {
	case doctor.CategoryImposterCommit, doctor.CategoryLockfileForgery, doctor.CategoryMisleadingSHA:
		return true
	}
	return false
}

// hasImposterFindings reports whether any workflow in the report carries a
// CategoryImposterCommit finding. Used to gate the bounded tag-walk that
// enriches those findings with a sane-release suggestion.
func hasImposterFindings(r *doctor.Report) bool {
	if r == nil {
		return false
	}
	for _, wr := range r.Workflows {
		for _, f := range wr.Findings {
			if f.Category == doctor.CategoryImposterCommit {
				return true
			}
		}
	}
	return false
}

// depReleaseURL derives a GitHub URL from a dep key of the form
// "owner/repo[/path]@ref". Commit-SHA pins link to /commit/<sha> (the
// diff view). Annotated-tag-object SHAs link to /tree/<sha> instead —
// /commit/<tagobject-sha> returns 404 because the tag object is not a
// commit. Non-SHA refs link to /releases/tag/<ref>. A nil checker
// (or one that has not peeled this SHA) falls back to the plain
// /commit/<sha> path.
func depReleaseURL(dep string, checker tagObjectChecker) string {
	nwo := dep
	ref := ""
	if i := strings.IndexByte(dep, '@'); i >= 0 {
		nwo = dep[:i]
		ref = dep[i+1:]
	}
	parts := strings.SplitN(nwo, "/", 3)
	if len(parts) < 2 {
		return ""
	}
	base := "https://github.com/" + parts[0] + "/" + parts[1]
	if ref != "" && isHexSHA(ref) {
		if checker != nil && checker.IsKnownTagObject(parts[0], parts[1], ref) {
			return base + "/tree/" + ref
		}
		return base + "/commit/" + ref
	}
	if ref != "" {
		return base + "/releases/tag/" + ref
	}
	return base + "/releases"
}

// tagObjectChecker is the cache-only "is this SHA an annotated tag object?"
// query used by depReleaseURL. *resolver.Resolver satisfies this; tests
// pass a stub. Kept as a small local interface so the URL builder stays
// I/O-free and trivially mockable.
type tagObjectChecker interface {
	IsKnownTagObject(owner, repo, sha string) bool
}

// workflowsForDep returns workflow paths whose findings reference the given
// dependency key (ordered as they appear in the report, deduplicated).
func workflowsForDep(report *doctor.Report, depKey string) []string {
	seen := map[string]bool{}
	var out []string
	for _, wr := range report.Workflows {
		for _, f := range wr.Findings {
			if f.DepKey() == depKey && !seen[wr.Path] {
				seen[wr.Path] = true
				out = append(out, wr.Path)
			}
		}
	}
	return out
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
