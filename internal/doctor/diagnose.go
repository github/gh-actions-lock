package doctor

import (
	"context"
	"fmt"

	"github.com/github/actions-workflow-parser/go/lockfile/diagnostics"
	"github.com/github/gh-actions-pin/internal/lockfile"
	"github.com/github/gh-actions-pin/internal/resolver"
)

// Diagnose scans a set of workflows and produces findings for each.
// It performs no output — purely analytical.
func Diagnose(paths []string, r *resolver.Resolver, store *lockfile.Store) *Report {
	report := &Report{}
	for _, path := range paths {
		wr := diagnoseOneWorkflow(path, r, store)
		report.Workflows = append(report.Workflows, wr)
	}
	return report
}

func diagnoseOneWorkflow(path string, r *resolver.Resolver, store *lockfile.Store) WorkflowReport {
	wr := WorkflowReport{Path: path}

	wf, err := lockfile.Load(path)
	if err != nil {
		wr.Findings = append(wr.Findings, Finding{
			WorkflowPath: path,
			Category:     CategoryNotPinned,
			Severity:     SeverityError,
			Detail:       fmt.Sprintf("failed to load workflow: %s", err),
			DocURL:       DocURLFor(CategoryNotPinned),
		})
		return wr
	}

	refs, _, parseWarnings := wf.ExtractActionRefs()
	wr.ActionRefs = refs
	wr.ParseWarnings = parseWarnings

	if len(refs) == 0 {
		wr.Findings = append(wr.Findings, Finding{
			WorkflowPath: path,
			Category:     CategoryRunOnly,
			Severity:     SeverityOK,
			Detail:       "no action references found",
		})
		return wr
	}

	wfKey := lockfile.WorkflowKeyFromPath(path)
	existingDeps, depsErr := store.Get(wfKey)
	if depsErr != nil {
		wr.Findings = append(wr.Findings, Finding{
			WorkflowPath: path,
			Category:     CategoryNotPinned,
			Severity:     SeverityError,
			Detail:       fmt.Sprintf("failed to read dependencies: %s", depsErr),
			Remediation:  "fix or regenerate the dependencies: section with `gh actions-pin`",
			DocURL:       DocURLFor(CategoryNotPinned),
		})
		return wr
	}
	wr.Deps = existingDeps

	directNWOs := make(map[string]bool, len(refs))
	for _, ref := range refs {
		directNWOs[ref.NWO()] = true
	}

	// Resolve live state: used to populate the inventory parent map and to
	// prime the engine adapter. Failure degrades to structural-only checks
	// for any refs that couldn't be resolved — partial results are kept.
	var liveDeps []lockfile.Dependency
	if r != nil {
		var resolveErr error
		liveDeps, resolveErr = r.ResolveAllRecursive(refs)
		if resolveErr != nil {
			wr.Findings = append(wr.Findings, Finding{
				WorkflowPath: path,
				Category:     CategoryValid,
				Severity:     SeverityWarning,
				Detail:       fmt.Sprintf("could not re-resolve actions: %s", resolveErr),
			})
		}
	}

	for _, dep := range existingDeps {
		owner, repo := dep.OwnerRepo()
		wr.Inventory = append(wr.Inventory, InventoryEntry{
			Dep:    dep,
			File:   path,
			Direct: directNWOs[owner+"/"+repo],
		})
	}
	parentMap := map[string][]string{}
	if r != nil {
		parentMap = r.ParentMap()
		populateInventoryParents(wr.Inventory, parentMap)
	}

	// Run the portable diagnostics engine.
	wfInput := buildEngineWorkflowInput(wfKey, refs)
	var reach []resolver.ReachabilityResult
	if r != nil && !r.DisableReachability && len(existingDeps) > 0 {
		reach = r.CheckReachabilityAll(existingDeps)
	}
	var engineRes diagnostics.Resolver
	if r != nil && liveDeps != nil {
		engineRes = newEngineResolver(r, liveDeps, reach)
	}
	engineFindings := diagnostics.Run(
		context.Background(),
		store.File(),
		[]diagnostics.WorkflowInput{wfInput},
		diagnostics.Options{Resolver: engineRes},
	)

	refByKey := indexRefs(refs)
	depByKey := indexDeps(existingDeps)
	for _, ef := range engineFindings {
		// Engine's stale check doesn't know about composite-expanded
		// transitive deps — suppress those false positives here.
		if ef.Code == diagnostics.CodeStale && isTransitivePin(ef, depByKey, parentMap) {
			continue
		}
		wr.Findings = append(wr.Findings, translateFinding(path, ef, refByKey, depByKey, directNWOs, parentMap))
	}

	// Reachability complement: engine emits imposter for direct uses only,
	// and stays silent on Unknown. Cover transitive deps + Unknown for all.
	if len(reach) > 0 {
		wr.Findings = append(wr.Findings, reachabilityComplementFindings(path, reach, existingDeps, directNWOs, parentMap, wr.Findings)...)
	}

	if !hasIssues(wr.Findings) {
		wr.Findings = append(wr.Findings, Finding{
			WorkflowPath: path,
			Category:     CategoryValid,
			Severity:     SeverityOK,
			Detail:       "all dependencies pinned and verified",
		})
	}

	return wr
}

func buildEngineWorkflowInput(wfKey string, refs []lockfile.ActionRef) diagnostics.WorkflowInput {
	uses := make([]diagnostics.UsesRef, 0, len(refs))
	for _, ref := range refs {
		uses = append(uses, diagnostics.UsesRef{
			Owner: ref.Owner,
			Repo:  ref.Repo,
			Path:  ref.Path,
			Ref:   ref.Ref,
		})
	}
	return diagnostics.WorkflowInput{Path: wfKey, Uses: uses}
}

func indexRefs(refs []lockfile.ActionRef) map[string]lockfile.ActionRef {
	out := make(map[string]lockfile.ActionRef, len(refs))
	for _, ref := range refs {
		out[ref.FullName()+"@"+ref.Ref] = ref
	}
	return out
}

func indexDeps(deps []lockfile.Dependency) map[string]lockfile.Dependency {
	out := make(map[string]lockfile.Dependency, len(deps))
	for _, dep := range deps {
		out[dep.Key()] = dep
	}
	return out
}

// translateFinding converts an engine diagnostic into a doctor Finding,
// re-attaching the source ActionRef/Dependency pointers the CLI needs for
// rendering.
func translateFinding(
	path string,
	ef diagnostics.Finding,
	refByKey map[string]lockfile.ActionRef,
	depByKey map[string]lockfile.Dependency,
	directNWOs map[string]bool,
	parentMap map[string][]string,
) Finding {
	df := Finding{
		WorkflowPath: path,
		Category:     Category(ef.Code),
		Severity:     mapEngineSeverity(ef.Severity),
		Detail:       ef.Message,
		Remediation:  ef.Remediation,
		LiveSHA:      ef.LiveSha,
		DocURL:       DocURLFor(Category(ef.Code)),
	}

	fullName := ef.Owner + "/" + ef.Repo
	if ef.Path != "" {
		fullName += "/" + ef.Path
	}
	key := fullName + "@" + ef.Ref

	if ref, ok := refByKey[key]; ok {
		refCopy := ref
		df.ActionRef = &refCopy
	}
	if dep, ok := depByKey[key]; ok {
		depCopy := dep
		df.Dependency = &depCopy
	} else if ef.LockedSha != "" {
		df.Dependency = &lockfile.Dependency{
			NWO: fullName,
			Ref: ef.Ref,
			SHA: ef.LockedSha,
		}
	}

	if df.Dependency != nil && !directNWOs[ef.Owner+"/"+ef.Repo] {
		if parents := parentMap[df.Dependency.Key()]; len(parents) > 0 {
			df.ParentNWO = parents[0]
		}
	}

	return df
}

func mapEngineSeverity(s diagnostics.Severity) Severity {
	switch s {
	case diagnostics.SeverityError:
		return SeverityError
	case diagnostics.SeverityWarning:
		return SeverityWarning
	case diagnostics.SeverityInfo:
		return SeverityInfo
	default:
		return SeverityInfo
	}
}

// isTransitivePin reports whether the engine finding refers to a dep
// reached via composite expansion (i.e. has parents in the parent map).
func isTransitivePin(ef diagnostics.Finding, depByKey map[string]lockfile.Dependency, parentMap map[string][]string) bool {
	fullName := ef.Owner + "/" + ef.Repo
	if ef.Path != "" {
		fullName += "/" + ef.Path
	}
	key := fullName + "@" + ef.Ref
	dep, ok := depByKey[key]
	if !ok {
		return false
	}
	return len(parentMap[dep.Key()]) > 0
}

// reachabilityComplementFindings covers the cases the engine doesn't:
//   - Imposter for transitive (composite-expanded) deps the engine never
//     visits because they aren't in workflow uses.
//   - Reachability-Unknown warnings for all deps (engine fails open on
//     Unknown). Direct + transitive both get a warning so the user knows
//     the check was inconclusive.
func reachabilityComplementFindings(
	path string,
	reach []resolver.ReachabilityResult,
	deps []lockfile.Dependency,
	directNWOs map[string]bool,
	parentMap map[string][]string,
	existing []Finding,
) []Finding {
	if len(reach) == 0 {
		return nil
	}

	forgeryKeys := map[string]bool{}
	for _, f := range existing {
		if f.Category == CategoryLockfileForgery && f.Dependency != nil {
			forgeryKeys[f.Dependency.Key()] = true
		}
	}

	depByKey := make(map[string]lockfile.Dependency, len(deps))
	for _, d := range deps {
		depByKey[d.Key()] = d
	}

	var out []Finding
	for _, rr := range reach {
		dep, ok := depByKey[rr.DepKey]
		if !ok {
			continue
		}
		depCopy := dep
		owner, repo := dep.OwnerRepo()
		direct := directNWOs[owner+"/"+repo]
		parent := ""
		if parents := parentMap[rr.DepKey]; len(parents) > 0 {
			parent = parents[0]
		}
		switch rr.Status {
		case resolver.Unreachable:
			if direct {
				continue // engine emits imposter for direct uses
			}
			if forgeryKeys[rr.DepKey] {
				continue
			}
			out = append(out, Finding{
				WorkflowPath: path,
				Category:     CategoryImposterCommit,
				Severity:     SeverityError,
				Dependency:   &depCopy,
				ParentNWO:    parent,
				Detail:       rr.Detail,
				Remediation:  "investigate immediately — the lockfile entry may have been injected",
				DocURL:       DocURLFor(CategoryImposterCommit),
			})
		case resolver.ReachabilityUnknown:
			remediation := "transitive dependency pinned to a bare SHA — reachability cannot be verified"
			if direct {
				remediation = "reachability check inconclusive — retry when network/API is available"
			}
			out = append(out, Finding{
				WorkflowPath: path,
				Category:     CategoryValid,
				Severity:     SeverityWarning,
				Dependency:   &depCopy,
				ParentNWO:    parent,
				Detail:       rr.Detail,
				Remediation:  remediation,
			})
		}
	}
	return out
}

func hasIssues(findings []Finding) bool {
	for _, f := range findings {
		if f.Severity == SeverityError {
			return true
		}
		if f.Category != CategoryValid && f.Category != CategoryRunOnly && f.Severity == SeverityWarning {
			return true
		}
	}
	return false
}

func populateInventoryParents(inventory []InventoryEntry, parentMap map[string][]string) {
	for i := range inventory {
		if inventory[i].Direct || len(inventory[i].Parents) > 0 {
			continue
		}
		parents := parentMap[inventory[i].Dep.Key()]
		if len(parents) > 0 {
			inventory[i].Parents = append([]string(nil), parents...)
		}
	}
}
