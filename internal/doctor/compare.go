package doctor

import (
	"fmt"
	"strings"

	"github.com/github/gh-actions-pin/internal/lockfile"
)

// compareSnapshots compares pinned dependencies against a live resolution and
// returns findings for any discrepancies: ref moves, ref changes, and stale deps.
func compareSnapshots(path string, existing, live []lockfile.Dependency, directNWOs map[string]bool) []Finding {
	liveByKey := make(map[string]lockfile.Dependency, len(live))
	liveByNWO := make(map[string][]lockfile.Dependency, len(live))
	for _, dep := range live {
		liveByKey[dep.Key()] = dep
		liveByNWO[dep.NWO] = append(liveByNWO[dep.NWO], dep)
	}

	var findings []Finding
	for _, pinned := range existing {
		if lockfile.IsFullSHA(pinned.Ref) {
			continue
		}

		resolved, ok := matchLiveDep(pinned, liveByKey, liveByNWO)
		if !ok {
			findings = append(findings, unmatchedFindings(path, pinned, liveByNWO, directNWOs)...)
			continue
		}
		if !strings.EqualFold(pinned.SHA, resolved.SHA) {
			resolvedCopy := resolved
			findings = append(findings, Finding{
				WorkflowPath: path,
				Category:     CategoryRefMoved,
				Severity:     SeverityWarning,
				Dependency:   &pinned,
				Detail:       fmt.Sprintf("pinned %s but ref now resolves to %s", pinned.SHA[:12], resolvedCopy.SHA[:12]),
				Remediation:  fmt.Sprintf("update to %s with `gh actions-pin upgrade`", resolvedCopy.SHA[:12]),
				LiveSHA:      resolvedCopy.SHA,
			})
		}
	}
	return findings
}

// matchLiveDep tries to find a live dependency matching the pinned one.
// First by exact key, then fuzzy by NWO (same SHA or narrowed version).
func matchLiveDep(pinned lockfile.Dependency, byKey map[string]lockfile.Dependency, byNWO map[string][]lockfile.Dependency) (lockfile.Dependency, bool) {
	if dep, ok := byKey[pinned.Key()]; ok {
		return dep, true
	}
	if candidates, has := byNWO[pinned.NWO]; has {
		for _, cand := range candidates {
			if strings.EqualFold(cand.SHA, pinned.SHA) {
				return cand, true
			}
			if IsNarrowedVersion(cand.Ref, pinned.Ref) {
				return cand, true
			}
		}
	}
	return lockfile.Dependency{}, false
}

// unmatchedFindings produces findings for a pinned dep that has no live match:
// either a ref change (direct dep with different ref) or stale (orphaned).
func unmatchedFindings(path string, pinned lockfile.Dependency, liveByNWO map[string][]lockfile.Dependency, directNWOs map[string]bool) []Finding {
	if directNWOs[pinned.NWO] {
		if candidates, has := liveByNWO[pinned.NWO]; has && len(candidates) > 0 {
			newDep := candidates[0]
			if newDep.Ref != pinned.Ref {
				refOwner, refRepo := pinned.OwnerRepo()
				return []Finding{{
					WorkflowPath: path,
					Category:     CategoryRefChanged,
					Severity:     SeverityWarning,
					Dependency:   &pinned,
					ActionRef: &lockfile.ActionRef{
						Owner: refOwner,
						Repo:  refRepo,
						Ref:   newDep.Ref,
					},
					Detail:      fmt.Sprintf("ref changed from %s to %s in workflow — re-pin to update", pinned.Ref, newDep.Ref),
					Remediation: "re-pin to match the new ref",
				}}
			}
		}
	}

	detail := "no longer in workflow — will be cleaned up"
	remediation := "re-resolve to remove orphaned dependency"
	if !directNWOs[pinned.NWO] {
		detail = "transitive dependency no longer discovered from upstream composite action"
		remediation = "re-resolve to clean up"
	}
	return []Finding{{
		WorkflowPath: path,
		Category:     CategoryStale,
		Severity:     SeverityInfo,
		Dependency:   &pinned,
		Detail:       detail,
		Remediation:  remediation,
	}}
}
