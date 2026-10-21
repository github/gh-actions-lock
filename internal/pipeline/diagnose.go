// Package pipeline orchestrates the scan, resolve, check, and report
// flow for a single run.
package pipeline

import (
	"context"
	"errors"
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
	wr.Findings = append(wr.Findings, localActionFindings(pw)...)
	if done {
		return wr
	}
	wr.Deps = pw.ExistingDeps

	if len(pw.SelfRepositoryRefs) > 0 {
		wr.Findings = append(wr.Findings, selfRepositoryFinding(pw))
	}

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
		var nestedLocal []*resolve.CompositeLocalPathError
		liveDeps, resolvedParents, resolveErr = r.ResolveAllRecursive(ctx, pw.Refs)
		nestedLocal, resolveErr = resolve.SplitLocalPaths(resolveErr)
		wr.Findings = append(wr.Findings, nestedLocalActionFindings(pw.Path, nestedLocal)...)
		for _, img := range r.ContainerImages(pw.Refs) {
			checks.AddContainerImage(&wr, img)
		}
		if resolveErr != nil {
			blockingResolverError := false
			if resolve.IsInvalidSelfRepositoryRef(resolveErr) {
				var invalidRef *resolve.InvalidSelfRepositoryRefError
				_ = errors.As(resolveErr, &invalidRef)
				detail := fmt.Sprintf("a composite action uses an invalid self repository reference: %s", resolveErr)
				if invalidRef != nil {
					detail = fmt.Sprintf("self repository actions must not carry an @ref: %s in %s", invalidRef.Ref, invalidRef.Parent)
				}
				wr.Findings = append(wr.Findings, checks.Finding{
					WorkflowPath: pw.Path,
					Category:     checks.InvalidSelfRepositoryRef,
					Severity:     checks.SeverityError,
					Confidence:   checks.ConfidenceHigh,
					Detail:       detail,
					Remediation:  "drop the `@ref` suffix — `$/…` always resolves to the running ref",
				})
				blockingResolverError = true
			}
			for _, malformed := range malformedNestedRefs(resolveErr) {
				wr.Findings = append(wr.Findings, checks.Finding{
					WorkflowPath: pw.Path,
					Category:     checks.NotPinned,
					Severity:     checks.SeverityError,
					Confidence:   checks.ConfidenceHigh,
					Detail:       malformed.Error(),
					Remediation:  fmt.Sprintf("fix the reference in %s, or stop using that version", malformed.Parent),
				})
				blockingResolverError = true
			}
			for _, missing := range missingRefs(resolveErr) {
				nwo := missing.Owner + "/" + missing.Repo
				wr.Findings = append(wr.Findings, checks.Finding{
					WorkflowPath: pw.Path,
					Category:     checks.MissingRef,
					Severity:     checks.SeverityError,
					Confidence:   checks.ConfidenceHigh,
					Dependency:   &dep.Dependency{NWO: nwo, Ref: missing.Ref},
					Detail:       missingRefDetail(missing.Ref, nwo),
					Remediation:  missingRefRemediation(missing.Ref, nwo),
					RefMissing:   true,
				})
				blockingResolverError = true
			}
			if blockingResolverError {
				wr.BlockingResolverError = true
				return wr
			}
			wr.Findings = append(wr.Findings, resolveErrFinding(pw.Path, resolveErr))
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
	liveByKey := indexDeps(liveDeps)
	for _, f := range rawFindings {
		if f.Category == checks.Stale && isTransitivePin(f, depByKey, parentMap) {
			continue
		}
		attachParent(&f, depByKey, directNWOs, parentMap)
		if f.Dependency != nil {
			d, ok := depByKey[f.Dependency.Key()]
			if !ok {
				d = liveByKey[f.Dependency.Key()]
			}
			f.Dependency.Hostname = d.Hostname
			if f.Category == checks.ShaAsRef && d.Hostname != "" {
				f.Remediation = fmt.Sprintf("pin to a tag instead: https://%s/%s/releases", d.Hostname, d.NWO)
			}
		}
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

// resolveErrFinding reports a resolver failure no definitive class above
// explains. The workflow's actions are unverified either way, so it blocks:
// as retryable when GitHub didn't answer.
func resolveErrFinding(path string, err error) checks.Finding {
	f := checks.Finding{
		WorkflowPath: path,
		Category:     checks.RepoUnavailable,
		Severity:     checks.SeverityError,
		Confidence:   checks.ConfidenceHigh,
		Detail:       fmt.Sprintf("could not re-resolve actions: %s", err),
		Remediation:  "check that each action's repository exists and this token can read it, then re-run",
	}
	if unanswered(err) {
		// Low: this is the resolver failure itself, not a verdict about
		// any specific dependency.
		f.Category = checks.ReachabilityUnknown
		f.Confidence = checks.ConfidenceLow
		f.MarkRetryable(err)
	}
	return f
}

// selfRepositoryFinding builds the informational finding for a workflow that
// references same-repo actions via `$/…`. These are inherently pinned.
func selfRepositoryFinding(pw checks.ParsedWorkflow) checks.Finding {
	return checks.Finding{
		WorkflowPath: pw.Path,
		Category:     checks.SelfRepositoryAction,
		Severity:     checks.SeverityInfo,
		Confidence:   checks.ConfidenceHigh,
		Detail:       fmt.Sprintf("references `$/…` self repository actions (inherently pinned): %s", strings.Join(pw.SelfRepositoryRefs, ", ")),
	}
}

// precheckWorkflow handles terminal preconditions (load error, invalid
// self repository refs, no refs, invalid deps). It returns true when
// one fired; otherwise the report is seeded with ActionRefs and images.
func precheckWorkflow(pw checks.ParsedWorkflow, store *lockfile.State) (checks.WorkflowReport, bool) {
	wr := checks.WorkflowReport{Path: pw.Path}

	if pw.LoadErr != nil {
		wr.SkipCommit = true
		wr.Findings = append(wr.Findings, checks.Finding{
			WorkflowPath: pw.Path,
			Category:     checks.InvalidWorkflow,
			Severity:     checks.SeverityError,
			// High: the YAML failed to load — concrete, file-level fact.
			Confidence: checks.ConfidenceHigh,
			Detail:     fmt.Sprintf("failed to load workflow: %s", pw.LoadErr),
		})
		preserveExistingInventory(&wr, pw)
		return wr, true
	}

	wr.ActionRefs = pw.Refs
	wr.RewriteRefs = pw.RewriteRefs
	if wr.RewriteRefs == nil {
		wr.RewriteRefs = pw.Refs
	}
	wr.SHASpellings = pw.SHASpellings
	wr.SelfActionFiles = pw.SelfActionFiles
	wr.SelfActionRefs = pw.SelfActionRefs
	for _, img := range pw.Images {
		checks.AddContainerImage(&wr, img)
	}

	hasTerminalFinding := false
	if len(pw.SelfRepositoryRefErrs) > 0 {
		wr.Findings = append(wr.Findings, checks.Finding{
			WorkflowPath: pw.Path,
			Category:     checks.InvalidSelfRepositoryRef,
			Severity:     checks.SeverityError,
			Confidence:   checks.ConfidenceHigh,
			Detail:       fmt.Sprintf("self repository actions must not carry an @ref: %s", strings.Join(pw.SelfRepositoryRefErrs, ", ")),
			Remediation:  "drop the `@ref` suffix — `$/…` always resolves to the running ref",
		})
		hasTerminalFinding = true
	}

	for _, resolutionErr := range pw.SelfRepositoryResolutionErrs {
		wr.Findings = append(wr.Findings, checks.Finding{
			WorkflowPath: pw.Path,
			Category:     checks.InvalidSelfRepositoryRef,
			Severity:     checks.SeverityError,
			Confidence:   checks.ConfidenceHigh,
			Detail:       resolutionErr,
			Remediation:  "fix the `$/…` action path so its dependencies can be inspected",
		})
		hasTerminalFinding = true
	}

	if hasTerminalFinding {
		preserveExistingInventory(&wr, pw)
		return wr, true
	}

	if len(pw.Refs) == 0 {
		// No refs to resolve, but the workflow entry itself must still be
		// present and carry no unused pins.
		if store != nil {
			for _, f := range checks.RunChecks(context.Background(), pw, store.File(), nil) {
				wr.Findings = append(wr.Findings, f)
			}
		}
		if len(pw.SelfRepositoryRefs) > 0 {
			wr.Findings = append(wr.Findings, selfRepositoryFinding(pw))
			return wr, true
		}
		if len(wr.Findings) > 0 {
			return wr, true
		}
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
			Remediable:   true,
			Detail:       fmt.Sprintf("failed to read dependencies: %s", pw.DepsErr),
			Remediation:  "fix or regenerate the dependencies: section with `gh actions-lock`",
		})
		return wr, true
	}

	return wr, false
}

// localActionFindings warns about each `./…` action a workflow (or one of its
// `$/…` actions) uses. They name the runner workspace, which checkouts and
// scripts can change, so they are never followed or locked. Shared by
// --verify and --verify-local so both modes agree.
func localActionFindings(pw checks.ParsedWorkflow) []checks.Finding {
	var out []checks.Finding
	for _, p := range pw.LocalPaths {
		out = append(out, checks.Finding{
			WorkflowPath:    pw.Path,
			Category:        checks.LocalAction,
			Severity:        checks.SeverityWarning,
			Confidence:      checks.ConfidenceHigh,
			LocalPath:       p,
			LocalRemoteUses: workflowfile.LocalActionRemoteUses(pw.Path, p),
			Detail:          "cannot follow workspace-local action " + p,
		})
	}
	for _, lu := range pw.NestedLocalUses {
		out = append(out, checks.Finding{
			WorkflowPath:    pw.Path,
			Category:        checks.LocalAction,
			Severity:        checks.SeverityWarning,
			Confidence:      checks.ConfidenceHigh,
			ParentNWO:       lu.Parent,
			LocalPath:       lu.Path,
			LocalRemoteUses: workflowfile.LocalActionRemoteUses(pw.Path, lu.Path),
			Detail:          fmt.Sprintf("cannot follow workspace-local action %s used by %s", lu.Path, lu.Parent),
		})
	}
	return out
}

// nestedLocalActionFindings warns about `./…` actions used inside remote
// composites, once per composite and path.
func nestedLocalActionFindings(path string, local []*resolve.CompositeLocalPathError) []checks.Finding {
	var out []checks.Finding
	seen := map[[2]string]bool{}
	for _, l := range local {
		k := [2]string{l.Parent, l.LocalPath}
		if seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, checks.Finding{
			WorkflowPath:    path,
			Category:        checks.LocalAction,
			Severity:        checks.SeverityWarning,
			Confidence:      checks.ConfidenceHigh,
			ParentNWO:       l.Parent,
			LocalPath:       l.LocalPath,
			LocalRemoteUses: workflowfile.LocalActionRemoteUses(path, l.LocalPath),
			Detail:          fmt.Sprintf("cannot follow workspace-local action %s used by %s", l.LocalPath, l.Parent),
		})
	}
	return out
}

func preserveExistingInventory(wr *checks.WorkflowReport, pw checks.ParsedWorkflow) {
	wr.Deps = pw.ExistingDeps
	for _, d := range pw.ExistingDeps {
		wr.Inventory = append(wr.Inventory, checks.InventoryEntry{
			Dep:    d,
			File:   pw.Path,
			Direct: true,
		})
	}
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
		if f.Category != checks.Valid && f.Category != checks.RunOnly && f.Category != checks.LocalAction && f.Category != checks.ContainerImage && f.Severity == checks.SeverityWarning {
			return true
		}
	}
	return false
}

// missingRefs collects every ref the resolver definitively failed to find.
func missingRefs(err error) []*ghapi.RefNotFoundError {
	switch e := err.(type) {
	case nil:
		return nil
	case *ghapi.RefNotFoundError:
		return []*ghapi.RefNotFoundError{e}
	case interface{ Unwrap() []error }:
		var out []*ghapi.RefNotFoundError
		for _, inner := range e.Unwrap() {
			out = append(out, missingRefs(inner)...)
		}
		return out
	}
	return missingRefs(errors.Unwrap(err))
}

// missingRefDetail names a full commit SHA by its short form so the message
// reads like the rest of the CLI's commit references.
func missingRefDetail(ref, nwo string) string {
	switch {
	case isFullSHA(ref):
		return fmt.Sprintf("commit %s does not exist or is not reachable in %s", ref[:12], nwo)
	case isHex(ref):
		return fmt.Sprintf("ref %s does not resolve in %s", ref, nwo)
	}
	return fmt.Sprintf("ref %s does not exist in %s", ref, nwo)
}

// missingRefRemediation keeps the short-SHA hint: a truncated commit is a
// different mistake from a tag that was never published.
func missingRefRemediation(ref, nwo string) string {
	switch {
	case isFullSHA(ref):
		return fmt.Sprintf("pin a commit that exists in %s, or point `uses:` at a tag or branch", nwo)
	case isHex(ref):
		return "if this is a commit, use the full 40-character SHA; otherwise point `uses:` at a tag or branch that exists"
	}
	return "point `uses:` at a tag or branch that exists"
}

func isFullSHA(ref string) bool {
	return (len(ref) == 40 || len(ref) == 64) && isHex(ref)
}

// malformedNestedRefs lists every malformed composite reference in err.
func malformedNestedRefs(err error) []*resolve.MalformedNestedRefError {
	var out []*resolve.MalformedNestedRefError
	var walk func(error)
	walk = func(err error) {
		if m, ok := err.(*resolve.MalformedNestedRefError); ok {
			out = append(out, m)
			return
		}
		if joined, ok := err.(interface{ Unwrap() []error }); ok {
			for _, e := range joined.Unwrap() {
				walk(e)
			}
			return
		}
		if e := errors.Unwrap(err); e != nil {
			walk(e)
		}
	}
	walk(err)
	return out
}
