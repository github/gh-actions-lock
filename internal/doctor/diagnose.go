package doctor

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/github/gh-actions-pin/internal/lockfile"
	"github.com/github/gh-actions-pin/internal/resolver"
	"github.com/github/gh-actions-pin/internal/ui"
)

var shaRefRE = regexp.MustCompile(`^[0-9a-fA-F]{40}$`)

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

	refs, _, _ := wf.ExtractActionRefs()
	wr.ActionRefs = refs

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

	existingDeps, err := wf.ReadDependencies()
	if err != nil || len(existingDeps) == 0 {
		// No lockfile. Check if any action refs are already SHA-pinned —
		// those should be SHAAsRef, not NotPinned.
		var shaRefs, tagRefs []lockfile.ActionRef
		for _, ref := range refs {
			if shaRefRE.MatchString(ref.Ref) {
				shaRefs = append(shaRefs, ref)
			} else {
				tagRefs = append(tagRefs, ref)
			}
		}

		// Actions pinned to bare SHAs without a lockfile.
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

		// Actions not pinned at all (tag/branch ref, no lockfile).
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
			// All refs are SHA-pinned, no tag refs — we already added SHAAsRef findings.
			return wr
		}
	}
	wr.Deps = existingDeps

	// Check for SHA-as-ref anti-pattern in existing deps (direct only).
	directNWOs := make(map[string]bool)
	for _, ref := range refs {
		directNWOs[ref.NWO()] = true
	}
	for i := range existingDeps {
		dep := &existingDeps[i]
		if !directNWOs[dep.NWO] {
			continue // transitive dep — user can't control it, skip
		}
		if shaRefRE.MatchString(dep.Ref) {
			parts := strings.SplitN(dep.NWO, "/", 3)
			owner, repo := "", ""
			if len(parts) >= 2 {
				owner, repo = parts[0], parts[1]
			}
			wr.Findings = append(wr.Findings, Finding{
				WorkflowPath: path,
				Category:     CategorySHAAsRef,
				Severity:     SeverityWarning,
				Dependency:   dep,
				Detail: fmt.Sprintf(
					"pinned to a bare SHA without a tag ref — weakens supply-chain security"),
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

	depsByKey := make(map[string]lockfile.Dependency)
	for _, dep := range existingDeps {
		depsByKey[dep.Key()] = dep
	}
	liveByKey := make(map[string]lockfile.Dependency)
	liveByNWO := make(map[string]lockfile.Dependency)
	for _, dep := range liveDeps {
		liveByKey[dep.Key()] = dep
		liveByNWO[dep.NWO] = dep
	}

	// Check each existing dep against live resolution.
	for _, existing := range existingDeps {
		// Skip deps already flagged as SHA-as-ref.
		if shaRefRE.MatchString(existing.Ref) {
			continue
		}

		live, ok := liveByKey[existing.Key()]
		if !ok {
			// Check if the same NWO exists with a different ref — that's a direct ref change.
			if directNWOs[existing.NWO] {
				if newDep, found := liveByNWO[existing.NWO]; found && newDep.Ref != existing.Ref {
					newCopy := newDep
					nwoParts := strings.SplitN(existing.NWO, "/", 3)
					var refOwner, refRepo string
					if len(nwoParts) >= 2 {
						refOwner, refRepo = nwoParts[0], nwoParts[1]
					}
					wr.Findings = append(wr.Findings, Finding{
						WorkflowPath: path,
						Category:     CategoryRefChanged,
						Severity:     SeverityWarning,
						Dependency:   &existing,
						ActionRef: &lockfile.ActionRef{
							Owner: refOwner,
							Repo:  refRepo,
							Ref:   newCopy.Ref,
						},
						Detail:      fmt.Sprintf("ref changed from %s to %s in workflow — re-pin to update", existing.Ref, newCopy.Ref),
						Remediation: "re-pin to match the new ref",
					})
					continue
				}
			}
			// Dep is no longer in the workflow (removed action or changed transitive).
			// Auto-clean via re-resolve.
			wr.Findings = append(wr.Findings, Finding{
				WorkflowPath: path,
				Category:     CategoryStale,
				Severity:     SeverityInfo,
				Dependency:   &existing,
				Detail:       "no longer in workflow — will be cleaned up",
				Remediation:  "re-resolve to remove orphaned dependency",
			})
			continue
		}
		if !strings.EqualFold(existing.SHA, live.SHA) {
			liveCopy := live
			parts := strings.SplitN(existing.NWO, "/", 3)
			var compareHint string
			if len(parts) >= 2 {
				compareHint = fmt.Sprintf(
					"\n  → Compare: https://github.com/%s/%s/compare/%s...%s\n  → Releases: https://github.com/%s/%s/releases\n  → If unexpected, reach out to the action maintainer",
					parts[0], parts[1], existing.SHA, live.SHA,
					parts[0], parts[1])
			}
			wr.Findings = append(wr.Findings, Finding{
				WorkflowPath: path,
				Category:     CategoryTampered,
				Severity:     SeverityError,
				Dependency:   &existing,
				Detail:       fmt.Sprintf("pinned %s but ref now resolves to %s%s", existing.SHA[:12], live.SHA[:12], compareHint),
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
			// Skip if this NWO already has a ref-changed finding.
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
	for _, rr := range reachResults {
		depID := rr.DepKey
		if depID == "" {
			depID = fmt.Sprintf("%s/%s@%s", rr.Owner, rr.Repo, rr.Ref)
		}
		switch rr.Status {
		case resolver.Unreachable:
			wr.Findings = append(wr.Findings, Finding{
				WorkflowPath: path,
				Category:     CategoryUnreachable,
				Severity:     SeverityError,
				Detail:       fmt.Sprintf("%s: SHA %s is NOT reachable from ref — possible fork injection", depID, rr.SHA[:12]),
			})
		}
		// ReachabilityUnknown for non-SHA-as-ref cases (e.g. API errors)
		// are informational — already covered by SHA-as-ref findings if applicable.
	}

	// If no issues found, mark as valid.
	hasIssues := false
	for _, f := range wr.Findings {
		if f.Category != CategoryValid && f.Category != CategoryRunOnly {
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


