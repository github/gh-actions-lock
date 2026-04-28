package doctor

import (
	"fmt"
	"strings"

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

	// SHA_MISMATCH: detect refs that look like SHAs but resolve to different commits.
	for _, mismatch := range lockfile.CheckSHARefMismatches(liveDeps) {
		wr.Findings = append(wr.Findings, Finding{
			WorkflowPath: path,
			Category:     CategorySHAMismatch,
			Severity:     SeverityError,
			Dependency:   &mismatch.Dep,
			Detail:       fmt.Sprintf("ref %s resolved to %s", mismatch.Dep.Ref, mismatch.ResolvedAs),
		})
	}

	depsByKey := make(map[string]lockfile.Dependency)
	for _, dep := range existingDeps {
		depsByKey[dep.Key()] = dep
	}
	liveByKey := make(map[string]lockfile.Dependency)
	// Multi-value NWO index for transitive fuzzy matching.
	liveByNWO := make(map[string][]lockfile.Dependency)
	for _, dep := range liveDeps {
		liveByKey[dep.Key()] = dep
		liveByNWO[dep.NWO] = append(liveByNWO[dep.NWO], dep)
	}

	// Check each existing dep against live resolution.
	for _, existing := range existingDeps {
		if lockfile.IsFullSHA(existing.Ref) {
			continue
		}

		live, ok := liveByKey[existing.Key()]
		if !ok {
			// Fuzzy match by NWO: same repo, same SHA or narrowed version.
			if candidates, has := liveByNWO[existing.NWO]; has {
				for _, cand := range candidates {
					if strings.EqualFold(cand.SHA, existing.SHA) {
						live = cand
						ok = true
						break
					}
					if IsNarrowedVersion(cand.Ref, existing.Ref) {
						live = cand
						ok = true
						break
					}
				}
			}
		}
		if !ok {
			// Direct ref change?
			if directNWOs[existing.NWO] {
				if candidates, has := liveByNWO[existing.NWO]; has && len(candidates) > 0 {
					newDep := candidates[0]
					if newDep.Ref != existing.Ref {
						refOwner, refRepo := existing.OwnerRepo()
						wr.Findings = append(wr.Findings, Finding{
							WorkflowPath: path,
							Category:     CategoryRefChanged,
							Severity:     SeverityWarning,
							Dependency:   &existing,
							ActionRef: &lockfile.ActionRef{
								Owner: refOwner,
								Repo:  refRepo,
								Ref:   newDep.Ref,
							},
							Detail:      fmt.Sprintf("ref changed from %s to %s in workflow — re-pin to update", existing.Ref, newDep.Ref),
							Remediation: "re-pin to match the new ref",
						})
						continue
					}
				}
			}
			// Transitive dep no longer discovered, or stale direct dep.
			if !directNWOs[existing.NWO] {
				wr.Findings = append(wr.Findings, Finding{
					WorkflowPath: path,
					Category:     CategoryStale,
					Severity:     SeverityInfo,
					Dependency:   &existing,
					Detail:       "transitive dependency no longer discovered from upstream composite action",
					Remediation:  "re-resolve to clean up",
				})
			} else {
				wr.Findings = append(wr.Findings, Finding{
					WorkflowPath: path,
					Category:     CategoryStale,
					Severity:     SeverityInfo,
					Dependency:   &existing,
					Detail:       "no longer in workflow — will be cleaned up",
					Remediation:  "re-resolve to remove orphaned dependency",
				})
			}
			continue
		}
		if !strings.EqualFold(existing.SHA, live.SHA) {
			liveCopy := live
			wr.Findings = append(wr.Findings, Finding{
				WorkflowPath: path,
				Category:     CategoryTampered,
				Severity:     SeverityError,
				Dependency:   &existing,
				Detail:       fmt.Sprintf("pinned %s but ref now resolves to %s", existing.SHA[:12], live.SHA[:12]),
				Remediation:  fmt.Sprintf("update to %s with `gh actions-pin upgrade`", liveCopy.SHA[:12]),
			})
		}
	}

	// Build set of NWOs with ref-changed findings to avoid duplicate "not pinned" findings.
	refChangedNWOs := make(map[string]bool)
	for _, f := range wr.Findings {
		if f.Category == CategoryRefChanged && f.Dependency != nil {
			refChangedNWOs[f.Dependency.NWO] = true
		}
	}

	// Check for missing deps (action in workflow but not pinned).
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
				Category:     CategoryUnreachable,
				Severity:     SeverityError,
				Dependency:   depPtr,
				Detail:       fmt.Sprintf("SHA %s is NOT reachable from ref — possible fork injection", rr.SHA[:12]),
			})
		case resolver.ReachabilityUnknown:
			isTransitive := !directNWOs[rr.Owner+"/"+rr.Repo]
			parentNWO := ""
			if isTransitive {
				parentNWO = r.ParentMap()[rr.Owner+"/"+rr.Repo]
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
