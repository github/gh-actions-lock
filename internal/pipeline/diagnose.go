// Package pipeline orchestrates the scan, resolve, check, and report
// flow for a single run.
package pipeline

import (
	"context"
	"fmt"

	"github.com/github/gh-actions-pin/internal/dep"
	"github.com/github/gh-actions-pin/internal/ghapi"
	"github.com/github/gh-actions-pin/internal/lockfile"
	"github.com/github/gh-actions-pin/internal/pinpool"
	"github.com/github/gh-actions-pin/internal/pipeline/checks"
	"github.com/github/gh-actions-pin/internal/resolve"
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
		func(ipw indexedPW) string { return ipw.pw.Path },
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
		return wr
	}

	wr.ActionRefs = pw.Refs
	wr.ParseWarnings = pw.ParseWarnings

	if len(pw.Refs) == 0 {
		wr.Findings = append(wr.Findings, checks.Finding{
			WorkflowPath: pw.Path,
			Category:     checks.RunOnly,
			Severity:     checks.SeverityOK,
			Confidence:   checks.ConfidenceHigh,
			Detail:       "no action references found",
		})
		return wr
	}

	if pw.DepsErr != nil {
		wr.Findings = append(wr.Findings, checks.Finding{
			WorkflowPath: pw.Path,
			Category:     checks.NotPinned,
			Severity:     checks.SeverityError,
			Confidence:   checks.ConfidenceHigh,
			Detail:       fmt.Sprintf("failed to read dependencies: %s", pw.DepsErr),
			Remediation:  "fix or regenerate the dependencies: section with `gh actions-pin`",
			DocURL:       DocURLFor(checks.NotPinned),
		})
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

	var reach []resolve.ReachabilityResult
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
	var liveMovedReach []resolve.ReachabilityResult
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
	var liveDirectReach []resolve.ReachabilityResult
	if r != nil && len(liveDeps) > 0 {
		if extra := liveDirectReachDeps(pw, liveDeps); len(extra) > 0 {
			liveDirectReach = r.CheckReachabilityAll(ctx, extra)
		}
	}
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
		if f.Category != checks.Valid && f.Category != checks.RunOnly && f.Severity == checks.SeverityWarning {
			return true
		}
	}
	return false
}
