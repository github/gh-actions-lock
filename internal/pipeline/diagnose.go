// Package pipeline orchestrates the scan, resolve, check, and report
// flow for a single run.
package pipeline

import (
	"context"
	"fmt"
	"strings"

	"github.com/github/gh-actions-lock/internal/dep"
	"github.com/github/gh-actions-lock/internal/ghapi"
	"github.com/github/gh-actions-lock/internal/lockfile"
	"github.com/github/gh-actions-lock/internal/pinpool"
	"github.com/github/gh-actions-lock/internal/pipeline/checks"
	"github.com/github/gh-actions-lock/internal/resolve"
	"github.com/github/gh-actions-lock/internal/workflowfile"
)

// DiagnoseParsed runs the engine diagnostics for each pre-parsed workflow.
// Assumes the resolver caches have already been warmed (calls into the
// resolver will hit cache and stay silent). Returns a checks.Report aggregating per-
// workflow findings in input order.
func DiagnoseParsed(ctx context.Context, parsed []checks.ParsedWorkflow, r *resolve.Resolver, store *lockfile.State, pool *pinpool.Pool) *checks.Report {
	type indexedPW struct {
		idx int
		pw  checks.ParsedWorkflow
	}
	items := make([]indexedPW, len(parsed))
	for i, pw := range parsed {
		items[i] = indexedPW{idx: i, pw: pw}
	}

	results := make([]checks.WorkflowReport, len(parsed))
	_ = pinpool.RunTyped(pool, ctx, "",
		items,
		func(ipw indexedPW) string { return "diagnosing " + ipw.pw.Path },
		func(ctx context.Context, _ int, ipw indexedPW) error {
			effR := r
			if ipw.pw.Resolved {
				effR = nil
			}
			results[ipw.idx] = diagnoseOneParsed(ctx, ipw.pw, effR, store, pool)
			return nil
		},
	)

	return &checks.Report{Workflows: results}
}

func diagnoseOneParsed(ctx context.Context, pw checks.ParsedWorkflow, r *resolve.Resolver, store *lockfile.State, pool *pinpool.Pool) checks.WorkflowReport {
	wr, done := precheckWorkflow(pw, store)
	if done {
		return wr
	}
	wr.Deps = pw.ExistingDeps

	directNWOs := make(map[ghapi.Repo]bool, len(pw.Refs))
	for _, ref := range pw.Refs {
		directNWOs[ghapi.ForRepo(ref.Owner, ref.Repo)] = true
	}

	// Resolve live state: hits cache when ParseAll's caller pre-warmed the
	// resolver. Failure degrades to structural-only checks for any refs that
	// couldn't be resolved — partial results are kept.
	var liveDeps []dep.Dependency
	var resolvedParents dep.ParentMap
	if r != nil {
		var resolveErr error
		liveDeps, resolvedParents, resolveErr = r.ResolveAllRecursive(ctx, pw.Refs)
		if resolveErr != nil {
			// Low: we're surfacing the resolver failure itself, not a
			// verdict about any specific dependency.
			wr.Findings = append(wr.Findings, checks.Finding{
				WorkflowPath: pw.Path,
				Category:     checks.ReachabilityUnknown,
				Severity:     checks.SeverityWarning,
				Confidence:   checks.ConfidenceLow,
				Detail:       fmt.Sprintf("could not re-resolve actions: %s", resolveErr),
			})
		}
	}

	for _, dep := range pw.ExistingDeps {
		owner, repo := dep.OwnerRepo()
		wr.Inventory = append(wr.Inventory, checks.InventoryEntry{
			Dep:    dep,
			File:   pw.Path,
			Direct: directNWOs[ghapi.ForRepo(owner, repo)],
		})
	}
	parentMap := map[string][]string{}
	if r != nil {
		parentMap = resolvedParents
		populateInventoryParents(wr.Inventory, parentMap)
	}

	reach, liveMovedReach, liveDirectReach := reachabilitySweeps(ctx, pw, r, liveDeps)
	var checkR checks.CheckResolver
	if r != nil && liveDeps != nil {
		checkR = checks.NewPrewarmedResolver(r, liveDeps, reach, liveMovedReach, liveDirectReach)
	}
	rawFindings := checks.RunChecks(ctx, pw, store.File(), checkR)

	depByKey := indexDeps(pw.ExistingDeps)
	for _, f := range rawFindings {
		if f.Category == checks.Stale && isTransitivePin(f, depByKey, parentMap) {
			continue
		}
		attachParent(&f, depByKey, directNWOs, parentMap)
		f.DocURL = DocURLFor(f.Category)
		wr.Findings = append(wr.Findings, f)
	}

	if len(reach) > 0 {
		wr.Findings = append(wr.Findings, reachabilityComplementFindings(pw.Path, reach, pw.ExistingDeps, directNWOs, parentMap, wr.Findings)...)
	}
	if len(liveDirectReach) > 0 {
		wr.Findings = append(wr.Findings, liveReachImpostorFindings(pw.Path, liveDirectReach, liveDeps, directNWOs, parentMap, wr.Findings)...)
	}

	if !hasIssues(wr.Findings) {
		wr.Findings = append(wr.Findings, checks.Finding{
			WorkflowPath: pw.Path,
			Category:     checks.Valid,
			Severity:     checks.SeverityOK,
			Confidence:   checks.ConfidenceHigh,
			Detail:       "all dependencies pinned and verified",
		})
	}

	return wr
}

// precheckWorkflow handles the terminal preconditions that stop a workflow
// from being diagnosed normally: a load error, local-path actions, a
// non-hosted runner, no action refs, or an unreadable dependencies block. It
// returns the report plus true when one fired; otherwise it returns a report
// seeded with ActionRefs/ParseWarnings and false.
func precheckWorkflow(pw checks.ParsedWorkflow, store *lockfile.State) (checks.WorkflowReport, bool) {
	wr := checks.WorkflowReport{Path: pw.Path}

	if pw.LoadErr != nil {
		wr.Findings = append(wr.Findings, checks.Finding{
			WorkflowPath: pw.Path,
			Category:     checks.NotPinned,
			Severity:     checks.SeverityError,
			// High: the YAML failed to load — concrete, file-level fact.
			Confidence: checks.ConfidenceHigh,
			Detail:     fmt.Sprintf("failed to load workflow: %s", pw.LoadErr),
			DocURL:     DocURLFor(checks.NotPinned),
		})
		return wr, true
	}

	wr.ActionRefs = pw.Refs
	wr.ParseWarnings = pw.ParseWarnings

	if len(pw.LocalPaths) > 0 {
		wfKey := workflowfile.KeyFromPath(pw.Path)
		if store != nil && store.HasWorkflow(wfKey) {
			wr.Findings = append(wr.Findings, checks.Finding{
				WorkflowPath: pw.Path,
				Category:     checks.LocalAction,
				Severity:     checks.SeverityError,
				Confidence:   checks.ConfidenceHigh,
				Detail:       "workflow uses local path actions which are not supported; remove local path actions to continue using the lockfile",
				Remediation:  "remove `uses: ./…` steps or move them to a separate workflow",
			})
		} else {
			wr.Findings = append(wr.Findings, checks.Finding{
				WorkflowPath: pw.Path,
				Category:     checks.LocalAction,
				Severity:     checks.SeverityWarning,
				Confidence:   checks.ConfidenceHigh,
				Detail:       "workflow uses local path actions; lockfile onboarding is not supported",
			})
		}
		return wr, true
	}

	if pw.NonHostedRunner {
		// Split labels into expressions vs literal non-hosted labels.
		var exprLabels, literalLabels []string
		for _, l := range pw.NonHostedLabels {
			if strings.Contains(l, "${") {
				exprLabels = append(exprLabels, l)
			} else {
				literalLabels = append(literalLabels, l)
			}
		}

		// If all non-hosted labels are expressions, use ExpressionRunner.
		if len(literalLabels) == 0 {
			wr.Findings = append(wr.Findings, checks.Finding{
				WorkflowPath: pw.Path,
				Category:     checks.ExpressionRunner,
				Severity:     checks.SeverityWarning,
				Confidence:   checks.ConfidenceHigh,
				Detail:       fmt.Sprintf("runs-on uses expressions [%s] that can't be resolved statically", strings.Join(exprLabels, ", ")),
			})
			return wr, true
		}

		// Otherwise report as self-hosted (include only literal labels in detail).
		labelList := strings.Join(literalLabels, ", ")
		wfKey := workflowfile.KeyFromPath(pw.Path)
		if store != nil && store.HasWorkflow(wfKey) {
			wr.Findings = append(wr.Findings, checks.Finding{
				WorkflowPath: pw.Path,
				Category:     checks.SelfHostedRunner,
				Severity:     checks.SeverityError,
				Confidence:   checks.ConfidenceHigh,
				Detail:       fmt.Sprintf("uses non-hosted runner labels [%s]; use GitHub-hosted runners to continue using the lockfile", labelList),
				Remediation:  "switch to GitHub-hosted runner labels or move self-hosted jobs to a separate workflow",
			})
		} else {
			wr.Findings = append(wr.Findings, checks.Finding{
				WorkflowPath: pw.Path,
				Category:     checks.SelfHostedRunner,
				Severity:     checks.SeverityWarning,
				Confidence:   checks.ConfidenceHigh,
				Detail:       fmt.Sprintf("uses non-hosted runner labels [%s]; lockfile onboarding is not supported", labelList),
			})
		}
		return wr, true
	}

	if len(pw.Refs) == 0 {
		wr.Findings = append(wr.Findings, checks.Finding{
			WorkflowPath: pw.Path,
			Category:     checks.RunOnly,
			Severity:     checks.SeverityOK,
			Confidence:   checks.ConfidenceHigh,
			Detail:       "no action references found",
		})
		return wr, true
	}

	if pw.DepsErr != nil {
		wr.Findings = append(wr.Findings, checks.Finding{
			WorkflowPath: pw.Path,
			Category:     checks.NotPinned,
			Severity:     checks.SeverityError,
			Confidence:   checks.ConfidenceHigh,
			Detail:       fmt.Sprintf("failed to read dependencies: %s", pw.DepsErr),
			Remediation:  "fix or regenerate the dependencies: section with `gh actions-lock`",
			DocURL:       DocURLFor(checks.NotPinned),
		})
		return wr, true
	}

	return wr, false
}

// reachabilitySweeps runs the three independent reachability passes over a
// workflow's deps: the locked-SHA sweep (split into trusted vs needs-check),
// the tag-moved live-SHA sweep, and the pin-time parity sweep for live SHAs
// the other two miss. Each pass returns its own result set so their
// (NWO, Ref, SHA) keys stay unmixed downstream.
func reachabilitySweeps(ctx context.Context, pw checks.ParsedWorkflow, r *resolve.Resolver, liveDeps []dep.Dependency) (reach, liveMovedReach, liveDirectReach []resolve.ReachabilityResult) {
	if r != nil && len(pw.ExistingDeps) > 0 {
		toCheck, trusted := partitionReachByLive(pw.ExistingDeps, liveDeps, pw.SkipReachWhenUnchanged)
		reach = trusted
		if len(toCheck) > 0 {
			reach = append(reach, r.CheckReachabilityAll(ctx, toCheck)...)
		}
	}
	// Independent sweep for LIVE SHAs whose tag has moved: the
	// tag-hijacked-to-fork-network shape is invisible to the locked-SHA
	// sweep above (the lockfile entry is still legitimate; the live
	// SHA is the impostor). Kept separate so the result map's
	// (NWO, Ref, SHA) keys don't shadow the lockfile sweep — they
	// share NWO@Ref dep keys, which would confuse
	// reachabilityComplementFindings if mixed into `reach`.
	if r != nil && len(liveDeps) > 0 && len(pw.ExistingDeps) > 0 {
		if moved := liveMovedDeps(pw.ExistingDeps, liveDeps); len(moved) > 0 {
			liveMovedReach = r.CheckReachabilityAll(ctx, moved)
		}
	}
	// Pin-time parity sweep: any (NWO, Ref, LIVE SHA) that neither the
	// locked-SHA sweep nor the tag-moved sweep covers gets a fresh reach
	// check here. Catches the NotPinned-direct impostor case and any
	// transitive composite live dep that isn't in the lockfile yet. With
	// this in place, applyPin's reach-loop Unreachable branch becomes a
	// fail-loud invariant rather than a primary detection path.
	if r != nil && len(liveDeps) > 0 {
		if extra := liveDirectReachDeps(pw, liveDeps); len(extra) > 0 {
			liveDirectReach = r.CheckReachabilityAll(ctx, extra)
		}
	}
	return reach, liveMovedReach, liveDirectReach
}

func indexDeps(deps []dep.Dependency) map[string]dep.Dependency {
	out := make(map[string]dep.Dependency, len(deps))
	for _, dep := range deps {
		out[dep.Key()] = dep
	}
	return out
}

func hasIssues(ff []checks.Finding) bool {
	for _, f := range ff {
		if f.Severity == checks.SeverityError {
			return true
		}
		if f.Category.IsInconclusive() {
			continue
		}
		if f.Category != checks.Valid && f.Category != checks.RunOnly && f.Category != checks.LocalAction && f.Category != checks.SelfHostedRunner && f.Severity == checks.SeverityWarning {
			return true
		}
	}
	return false
}
