package pipeline

import (
	"fmt"
	"github.com/github/gh-actions-lock/internal/dep"
	"github.com/github/gh-actions-lock/internal/ghapi"
	"github.com/github/gh-actions-lock/internal/pipeline/checks"
	"github.com/github/gh-actions-lock/internal/resolve"
)

// reachabilityComplementFindings covers the cases the engine doesn't:
//   - Impostor for transitive (composite-expanded) deps the engine never
//     visits because they aren't in workflow uses.
//   - Reachability-Unknown warnings for all deps (engine fails open on
//     Unknown). Direct + transitive both get a warning so the user knows
//     the check was inconclusive.
func reachabilityComplementFindings(
	path string,
	reach []resolve.ReachabilityResult,
	deps []dep.Dependency,
	directNWOs map[ghapi.Repo]bool,
	parentMap map[string][]string,
	existing []checks.Finding,
) []checks.Finding {
	if len(reach) == 0 {
		return nil
	}

	forgeryKeys := map[string]bool{}
	for _, f := range existing {
		if f.Category == checks.LockfileForgery && f.Dependency != nil {
			forgeryKeys[f.Dependency.Key()] = true
		}
	}

	depByKey := make(map[string]dep.Dependency, len(deps))
	for _, d := range deps {
		depByKey[d.Key()] = d
	}

	var out []checks.Finding
	for _, rr := range reach {
		dep, ok := depByKey[rr.DepKey]
		if !ok {
			continue
		}
		depCopy := dep
		owner, repo := dep.OwnerRepo()
		direct := directNWOs[ghapi.ForRepo(owner, repo)]
		parent := ""
		if parents := parentMap[rr.DepKey]; len(parents) > 0 {
			parent = parents[0]
		}
		switch rr.Status {
		case resolve.Unreachable:
			if direct {
				continue // engine emits impostor for direct uses
			}
			if forgeryKeys[rr.DepKey] {
				continue
			}
			// High: branch_commits returned an authoritative
			// "unreachable" for this transitive pin.
			out = append(out, checks.Finding{
				WorkflowPath: path,
				Category:     checks.ImpostorCommit,
				Severity:     checks.SeverityError,
				Confidence:   checks.ConfidenceHigh,
				Dependency:   &depCopy,
				ParentNWO:    parent,
				Detail:       rr.Detail,
				Remediation:  "investigate immediately — the lockfile entry may have been injected",
				DocURL:       DocURLFor(checks.ImpostorCommit),
			})
		case resolve.ReachabilityUnknown:
			remediation := "transitive dependency pinned to a bare SHA — reachability cannot be verified"
			if direct {
				remediation = "reachability check inconclusive — retry when network/API is available"
			}
			// Low: we couldn't get a reachability answer at all.
			out = append(out, checks.Finding{
				WorkflowPath: path,
				Category:     checks.ReachabilityUnknown,
				Severity:     checks.SeverityWarning,
				Confidence:   checks.ConfidenceLow,
				Dependency:   &depCopy,
				ParentNWO:    parent,
				Detail:       rr.Detail,
				Remediation:  remediation,
			})
		}
	}
	return out
}

// liveReachImpostorFindings emits checks.ImpostorCommit for live-resolved
// SHAs that come back Unreachable from the live-direct sweep. Operates on
// synthetic live deps (not pw.ExistingDeps), so it fires for unpinned and
// transitive-not-in-lockfile cases that reachabilityComplementFindings
// (keyed on existing deps) can't see.
//
// Suppresses duplicates against any prior impostor/forgery finding for the
// same dep key — the engine's checkImpostorCommit may have already emitted
// for a direct ref via the live-ref-vs-locked compare in check_misleading.
func liveReachImpostorFindings(
	path string,
	reach []resolve.ReachabilityResult,
	live []dep.Dependency,
	directNWOs map[ghapi.Repo]bool,
	parentMap map[string][]string,
	existing []checks.Finding,
) []checks.Finding {
	if len(reach) == 0 {
		return nil
	}
	covered := map[string]bool{}
	for _, f := range existing {
		if f.Dependency == nil {
			continue
		}
		switch f.Category {
		case checks.ImpostorCommit, checks.LockfileForgery:
			covered[f.Dependency.Key()] = true
		}
	}
	liveByReachKey := make(map[ghapi.Reach]dep.Dependency, len(live))
	for _, d := range live {
		owner, repo := d.OwnerRepo()
		liveByReachKey[ghapi.ForReach(owner, repo, d.SHA, d.Ref)] = d
	}
	var out []checks.Finding
	for _, rr := range reach {
		if rr.Status != resolve.Unreachable {
			continue
		}
		dep, ok := liveByReachKey[ghapi.ForReach(rr.Owner, rr.Repo, rr.SHA, rr.Ref)]
		if !ok {
			continue
		}
		if covered[dep.Key()] {
			continue
		}
		depCopy := dep
		owner, repo := dep.OwnerRepo()
		direct := directNWOs[ghapi.ForRepo(owner, repo)]
		parent := ""
		if !direct {
			if parents := parentMap[dep.Key()]; len(parents) > 0 {
				parent = parents[0]
			}
		}
		detail := rr.Detail
		if detail == "" {
			detail = fmt.Sprintf("live resolve of %s/%s@%s → %s is not reachable from any branch", owner, repo, dep.Ref, dep.SHA)
		}
		out = append(out, checks.Finding{
			WorkflowPath: path,
			Category:     checks.ImpostorCommit,
			Severity:     checks.SeverityError,
			Confidence:   checks.ConfidenceHigh,
			Dependency:   &depCopy,
			ParentNWO:    parent,
			Detail:       detail,
			Remediation:  "investigate immediately — the live ref resolves to a commit that is not reachable from any branch",
			DocURL:       DocURLFor(checks.ImpostorCommit),
		})
		// Mark covered so a second reach result for the same dep doesn't double-emit.
		covered[dep.Key()] = true
	}
	return out
}
