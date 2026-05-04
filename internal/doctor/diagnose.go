package doctor

import (
	"fmt"

	"github.com/github/gh-actions-pin/internal/lockfile"
	"github.com/github/gh-actions-pin/internal/resolver"
	"github.com/github/gh-actions-pin/internal/ui"
)

// Diagnose scans a set of workflows and produces findings for each.
// It performs no output — purely analytical.
func Diagnose(paths []string, r *resolver.Resolver) *Report {
	report := &Report{}
	for _, path := range paths {
		wr := diagnoseOneWorkflow(path, r)
		report.Workflows = append(report.Workflows, wr)
	}
	return report
}

func diagnoseOneWorkflow(path string, r *resolver.Resolver) WorkflowReport {
	wr := WorkflowReport{Path: path}

	wf, err := lockfile.Load(path)
	if err != nil {
		wr.Findings = append(wr.Findings, Finding{
			WorkflowPath: path,
			Category:     CategoryNotPinned,
			Severity:     SeverityError,
			Detail:       fmt.Sprintf("failed to load workflow: %s", err),
		})
		return wr
	}

	refs, _, parseWarnings := wf.ExtractActionRefs()
	wr.ActionRefs = refs
	wr.ParseWarnings = parseWarnings

	// No action refs → run-only workflow, nothing to do.
	if len(refs) == 0 {
		wr.Findings = append(wr.Findings, Finding{
			WorkflowPath: path,
			Category:     CategoryRunOnly,
			Severity:     SeverityOK,
			Detail:       "no action references found",
		})
		return wr
	}

	existingDeps, depsErr := wf.ReadDependencies()
	if depsErr != nil {
		// Malformed dependencies: section — report as error, don't fold into "not pinned".
		wr.Findings = append(wr.Findings, Finding{
			WorkflowPath: path,
			Category:     CategoryNotPinned,
			Severity:     SeverityError,
			Detail:       fmt.Sprintf("failed to read dependencies: %s", depsErr),
			Remediation:  "fix or regenerate the dependencies: section with `gh actions-pin`",
		})
		return wr
	}

	if len(existingDeps) == 0 {
		// No lockfile. Check if any action refs are already SHA-pinned —
		// those should be SHAAsRef, not NotPinned.
		var shaRefs, tagRefs []lockfile.ActionRef
		for _, ref := range refs {
			if lockfile.IsFullSHA(ref.Ref) {
				shaRefs = append(shaRefs, ref)
			} else {
				tagRefs = append(tagRefs, ref)
			}
		}

		for _, ref := range shaRefs {
			nwo := ref.NWO()
			wr.Findings = append(wr.Findings, Finding{
				WorkflowPath: path,
				Category:     CategorySHAAsRef,
				Severity:     SeverityWarning,
				ActionRef:    &ref,
				Dependency: &lockfile.Dependency{
					NWO: nwo,
					Ref: ref.Ref,
					SHA: ref.Ref,
				},
				Detail: "pinned to a bare SHA without a tag ref — weakens supply-chain security",
				Remediation: fmt.Sprintf(
					"pin to a tag instead: https://github.com/%s/releases", nwo),
			})
		}

		if len(tagRefs) > 0 {
			wr.Findings = append(wr.Findings, Finding{
				WorkflowPath: path,
				Category:     CategoryNotPinned,
				Severity:     SeverityError,
				Detail:       fmt.Sprintf("%d %s not pinned", len(tagRefs), ui.Pluralize(len(tagRefs), "action", "actions")),
				Remediation:  "pin with `gh actions-pin`",
			})
		}

		if len(wr.Findings) == 0 {
			return wr
		}
	}
	wr.Deps = existingDeps

	// Build direct NWO set from workflow uses: lines.
	directNWOs := make(map[string]bool)
	for _, ref := range refs {
		directNWOs[ref.NWO()] = true
	}

	// Build dependency inventory with direct/transitive classification.
	for _, dep := range existingDeps {
		wr.Inventory = append(wr.Inventory, InventoryEntry{
			Dep:    dep,
			File:   path,
			Direct: directNWOs[dep.NWO],
		})
	}

	// Check for SHA-as-ref anti-pattern in existing deps (direct only).
	for i := range existingDeps {
		dep := &existingDeps[i]
		if !directNWOs[dep.NWO] {
			continue
		}
		if lockfile.IsFullSHA(dep.Ref) {
			owner, repo := dep.OwnerRepo()
			wr.Findings = append(wr.Findings, Finding{
				WorkflowPath: path,
				Category:     CategorySHAAsRef,
				Severity:     SeverityWarning,
				Dependency:   dep,
				Detail:       "pinned to a bare SHA without a tag ref — weakens supply-chain security",
				Remediation: fmt.Sprintf(
					"pin to a tag instead: https://github.com/%s/%s/releases", owner, repo),
			})
		}
	}

	// Re-resolve to check for staleness and tampering.
	liveDeps, err := r.ResolveAllRecursive(refs)
	if err != nil {
		wr.Findings = append(wr.Findings, Finding{
			WorkflowPath: path,
			Category:     CategoryValid,
			Severity:     SeverityWarning,
			Detail:       fmt.Sprintf("could not re-resolve actions: %s", err),
		})
		return wr
	}

	// MISLEADING_SHA: detect refs that look like SHAs but resolve to different commits.
	for _, mismatch := range lockfile.CheckSHARefMismatches(liveDeps) {
		wr.Findings = append(wr.Findings, Finding{
			WorkflowPath: path,
			Category:     CategoryMisleadingSHA,
			Severity:     SeverityError,
			Dependency:   &mismatch.Dep,
			Detail:       fmt.Sprintf("ref %s resolved to %s", mismatch.Dep.Ref, mismatch.ResolvedAs),
		})
	}

	// Compare pinned deps against live resolution.
	wr.Findings = append(wr.Findings, compareSnapshots(path, existingDeps, liveDeps, directNWOs)...)

	// Build set of NWOs with ref-changed findings to avoid duplicate "not pinned" findings.
	refChangedNWOs := make(map[string]bool)
	for _, f := range wr.Findings {
		if f.Category == CategoryRefChanged && f.Dependency != nil {
			refChangedNWOs[f.Dependency.NWO] = true
		}
	}

	// Check for missing deps (action in workflow but not pinned).
	depsByKey := make(map[string]lockfile.Dependency, len(existingDeps))
	for _, dep := range existingDeps {
		depsByKey[dep.Key()] = dep
	}
	for _, ref := range refs {
		key := ref.FullName() + "@" + ref.Ref
		if _, ok := depsByKey[key]; !ok {
			if refChangedNWOs[ref.NWO()] {
				continue
			}
			refCopy := ref
			wr.Findings = append(wr.Findings, Finding{
				WorkflowPath: path,
				Category:     CategoryNotPinned,
				Severity:     SeverityError,
				ActionRef:    &refCopy,
				Detail:       "used in workflow but not pinned",
				Remediation:  "pin with `gh actions-pin`",
			})
		}
	}

	// Reachability checks.
	reachResults := r.CheckReachabilityAll(existingDeps)
	// Build dep lookup by key for attaching to reachability findings.
	depByKey := make(map[string]lockfile.Dependency, len(existingDeps))
	for _, d := range existingDeps {
		depByKey[d.Key()] = d
	}
	for _, rr := range reachResults {
		depID := rr.DepKey
		if depID == "" {
			depID = fmt.Sprintf("%s/%s@%s", rr.Owner, rr.Repo, rr.Ref)
		}
		var depPtr *lockfile.Dependency
		if d, ok := depByKey[rr.DepKey]; ok {
			depPtr = &d
		}
		switch rr.Status {
		case resolver.Unreachable:
			wr.Findings = append(wr.Findings, Finding{
				WorkflowPath: path,
				Category:     CategoryImposterCommit,
				Severity:     SeverityError,
				Dependency:   depPtr,
				Detail:       rr.Detail,
			})
		case resolver.ReachabilityUnknown:
			isTransitive := !directNWOs[rr.Owner+"/"+rr.Repo]
			parentNWO := ""
			if isTransitive {
				if parents := r.ParentMap()[rr.DepKey]; len(parents) > 0 {
					parentNWO = parents[0]
				}
			}
			wr.Findings = append(wr.Findings, Finding{
				WorkflowPath: path,
				Category:     CategoryValid,
				Severity:     SeverityWarning,
				Detail:       rr.Detail,
				Dependency: &lockfile.Dependency{
					NWO: rr.Owner + "/" + rr.Repo,
					Ref: rr.Ref,
					SHA: rr.SHA,
				},
				ParentNWO: parentNWO,
				Remediation: func() string {
					if isTransitive {
						return "transitive dependency pinned to a bare SHA — reachability cannot be verified"
					}
					return "reachability check inconclusive"
				}(),
			})
		}
	}

	// If no issues found, mark as valid.
	hasIssues := false
	for _, f := range wr.Findings {
		if f.Severity == SeverityError || (f.Category != CategoryValid && f.Category != CategoryRunOnly && f.Severity == SeverityWarning) {
			hasIssues = true
			break
		}
	}
	if !hasIssues {
		wr.Findings = append(wr.Findings, Finding{
			WorkflowPath: path,
			Category:     CategoryValid,
			Severity:     SeverityOK,
			Detail:       "all dependencies pinned and verified",
		})
	}

	return wr
}
