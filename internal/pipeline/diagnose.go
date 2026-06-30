// Package pipeline orchestrates the scan, resolve, check, and report
// flow for a single run.
package pipeline

import (
	"context"
	"errors"
	"fmt"

	"github.com/github/gh-actions-lock/internal/dep"
	"github.com/github/gh-actions-lock/internal/ghapi"
	"github.com/github/gh-actions-lock/internal/lockfile"
	"github.com/github/gh-actions-lock/internal/pinpool"
	"github.com/github/gh-actions-lock/internal/pipeline/checks"
	"github.com/github/gh-actions-lock/internal/resolve"
	"github.com/github/gh-actions-lock/internal/workflowfile"
)

// DiagnoseParsed runs engine diagnostics for each pre-parsed workflow, assuming
// the resolver caches are warm (calls hit cache and stay silent). Returns a
// checks.Report aggregating per-workflow findings in input order.
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
			if resolve.IsCompositeLocalPath(resolveErr) {
				wr.Findings = append(wr.Findings, checks.Finding{
					WorkflowPath: pw.Path,
					Category:     checks.LocalAction,
					Severity:     checks.SeverityError,
					Confidence:   checks.ConfidenceHigh,
					Detail:       fmt.Sprintf("a composite action uses local path actions whose transitive dependencies cannot be resolved: %s", resolveErr),
					Remediation:  "the composite action must reference dependencies by owner/repo/path@ref instead of ./path",
				})
				return wr
			}
			// A resolve failure means we could not verify these refs. Fail
			// closed: an unverifiable pin must never pass as valid. Classify
			// by whether the failure is definitive.
			cat := checks.ReachabilityUnverified
			conf := checks.ConfidenceLow
			if errors.Is(resolveErr, ghapi.ErrUnresolvableCommit) {
				// At least one full-SHA pin authoritatively resolves to no
				// object upstream — the commit is missing/unreachable.
				cat = checks.UnresolvableCommit
				conf = checks.ConfidenceHigh
			}
			wr.Findings = append(wr.Findings, checks.Finding{
				WorkflowPath: pw.Path,
				Category:     cat,
				Severity:     checks.SeverityError,
				Confidence:   conf,
				Detail:       fmt.Sprintf("could not re-resolve actions: %s", resolveErr),
				DocURL:       DocURLFor(cat),
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

	var checkR checks.CheckResolver
	if r != nil && liveDeps != nil {
		checkR = checks.NewPrewarmedResolver(r, liveDeps)
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

// precheckWorkflow handles terminal preconditions (load error, local-path
// actions, non-hosted runner, no refs, unreadable deps). It returns true when
// one fired; otherwise the report is seeded with ActionRefs/ParseWarnings.
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
		if f.Category != checks.Valid && f.Category != checks.RunOnly && f.Category != checks.LocalAction && f.Severity == checks.SeverityWarning {
			return true
		}
	}
	return false
}
