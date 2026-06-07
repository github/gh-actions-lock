package pipeline

import (
	"github.com/github/gh-actions-pin/internal/dep"
	"github.com/github/gh-actions-pin/internal/ghapi"
	"github.com/github/gh-actions-pin/internal/pipeline/checks"
	"github.com/github/gh-actions-pin/internal/resolve"
	"strings"
)

// CollectReachDeps returns the deduplicated union of existing deps across the
// given parsed workflows that will need a fresh reachability network check
// once diagnostics runs. It mirrors the per-workflow partition diagnose
// performs internally (see partitionReachByLive) but operates over the union,
// so callers can pre-warm CheckReachabilityAll once across every unresolved
// workflow instead of paying the per-workflow repo-warmup + per-dep
// concurrency cost serially. Pass live as the result of a single
// ResolveAllRecursive over the union of refs (the resolver cache makes the
// per-workflow re-lookups inside diagnose free).
func CollectReachDeps(parsed []checks.ParsedWorkflow, live []dep.Dependency) []dep.Dependency {
	if len(parsed) == 0 {
		return nil
	}
	liveSHA := make(map[string]string, len(live))
	for _, d := range live {
		liveSHA[d.Key()] = d.SHA
	}
	seen := make(map[string]bool)
	var out []dep.Dependency
	for _, pw := range parsed {
		if !pw.SkipReachWhenUnchanged {
			// When unchanged-skip isn't active (e.g. --rescan), the per-
			// workflow path will check every existing dep. Mirror that so
			// pre-warm sees the full set.
			for _, d := range pw.ExistingDeps {
				if seen[d.Key()] {
					continue
				}
				seen[d.Key()] = true
				out = append(out, d)
			}
			continue
		}
		for _, d := range pw.ExistingDeps {
			sha, ok := liveSHA[d.Key()]
			if ok && strings.EqualFold(sha, d.SHA) {
				continue
			}
			if seen[d.Key()] {
				continue
			}
			seen[d.Key()] = true
			out = append(out, d)
		}
	}
	return out
}

// CollectLiveMovedReachDeps returns the deduplicated set of synthetic
// dependencies (NWO, Ref + LIVE SHA) for which a reachability check
// should be pre-warmed. Each entry pairs an existing lockfile dep with
// the LIVE SHA it currently resolves to, when they differ — the input
// that lets the engine emit checks.ImpostorCommit for the
// tag-hijacked-to-fork-network shape. Pass live as the result of a
// single ResolveAllRecursive over the union of refs.

// CollectLiveMovedReachDeps returns the deduplicated set of synthetic
// dependencies (NWO, Ref + LIVE SHA) for which a reachability check
// should be pre-warmed. Each entry pairs an existing lockfile dep with
// the LIVE SHA it currently resolves to, when they differ — the input
// that lets the engine emit checks.ImpostorCommit for the
// tag-hijacked-to-fork-network shape. Pass live as the result of a
// single ResolveAllRecursive over the union of refs.
func CollectLiveMovedReachDeps(parsed []checks.ParsedWorkflow, live []dep.Dependency) []dep.Dependency {
	if len(parsed) == 0 || len(live) == 0 {
		return nil
	}
	liveSHA := make(map[string]string, len(live))
	liveDep := make(map[string]dep.Dependency, len(live))
	for _, d := range live {
		liveSHA[d.Key()] = d.SHA
		liveDep[d.Key()] = d
	}
	seen := make(map[ghapi.Reach]bool)
	var out []dep.Dependency
	for _, pw := range parsed {
		for _, d := range pw.ExistingDeps {
			ls, ok := liveSHA[d.Key()]
			if !ok || strings.EqualFold(ls, d.SHA) {
				continue
			}
			synthetic := d
			synthetic.SHA = ls
			// Prefer the live dep's NWO casing if the live resolve has
			// one — it's the canonical one returned by the API.
			if ld, ok := liveDep[d.Key()]; ok && ld.NWO != "" {
				synthetic.NWO = ld.NWO
			}
			owner, repo := synthetic.OwnerRepo()
			k := ghapi.ForReach(owner, repo, synthetic.SHA, synthetic.Ref)
			if seen[k] {
				continue
			}
			seen[k] = true
			out = append(out, synthetic)
		}
	}
	return out
}

// liveDirectReachDeps returns live-resolved deps whose (NWO, Ref, SHA)
// isn't already covered by the locked-SHA sweep (partitionReachByLive) or
// the tag-moved sweep (liveMovedDeps), so the engine can give them a
// fresh reachability check before pinning. Covers two pin-time impostor
// shapes that the existing diagnose paths miss:
//
//   - NotPinned workflow: no ExistingDep at all, so the locked-SHA sweep
//     never runs. Without this, applyPin's reach loop is the only thing
//     catching these — diagnose now fires the checks.ImpostorCommit
//     finding pre-pin so the auto-fix runs via tryAutoFixImpostors.
//   - Transitive composite dep that ResolveAllRecursive discovered but
//     isn't yet in the lockfile. The locked-SHA sweep can't see it; the
//     live-moved sweep only fires when an ExistingDep exists for the same
//     dep key with a different SHA.
//
// Dedup by ghapi.Reach across direct + transitive entries.

// liveDirectReachDeps returns live-resolved deps whose (NWO, Ref, SHA)
// isn't already covered by the locked-SHA sweep (partitionReachByLive) or
// the tag-moved sweep (liveMovedDeps), so the engine can give them a
// fresh reachability check before pinning. Covers two pin-time impostor
// shapes that the existing diagnose paths miss:
//
//   - NotPinned workflow: no ExistingDep at all, so the locked-SHA sweep
//     never runs. Without this, applyPin's reach loop is the only thing
//     catching these — diagnose now fires the checks.ImpostorCommit
//     finding pre-pin so the auto-fix runs via tryAutoFixImpostors.
//   - Transitive composite dep that ResolveAllRecursive discovered but
//     isn't yet in the lockfile. The locked-SHA sweep can't see it; the
//     live-moved sweep only fires when an ExistingDep exists for the same
//     dep key with a different SHA.
//
// Dedup by ghapi.Reach across direct + transitive entries.
func liveDirectReachDeps(pw checks.ParsedWorkflow, live []dep.Dependency) []dep.Dependency {
	if len(live) == 0 {
		return nil
	}
	covered := make(map[ghapi.Reach]bool, len(pw.ExistingDeps)+len(live))
	existingByDepKey := make(map[string]dep.Dependency, len(pw.ExistingDeps))
	for _, d := range pw.ExistingDeps {
		owner, repo := d.OwnerRepo()
		covered[ghapi.ForReach(owner, repo, d.SHA, d.Ref)] = true
		existingByDepKey[d.Key()] = d
	}
	for _, d := range live {
		ed, ok := existingByDepKey[d.Key()]
		if !ok || strings.EqualFold(ed.SHA, d.SHA) {
			continue
		}
		owner, repo := d.OwnerRepo()
		covered[ghapi.ForReach(owner, repo, d.SHA, d.Ref)] = true
	}
	seen := make(map[ghapi.Reach]bool, len(live))
	var out []dep.Dependency
	for _, d := range live {
		owner, repo := d.OwnerRepo()
		k := ghapi.ForReach(owner, repo, d.SHA, d.Ref)
		if covered[k] || seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, d)
	}
	return out
}

// CollectLiveDirectReachDeps is the cmd-level pre-warm analogue of
// liveDirectReachDeps. Returns the deduplicated set of synthetic live
// deps across all parsed workflows that need a fresh reachability check
// because they're outside both the locked-SHA and live-moved sweeps. On
// a fully steady-state lockfile this is empty; on a brand-new repo (no
// lockfile yet) it's the full live set.

// CollectLiveDirectReachDeps is the cmd-level pre-warm analogue of
// liveDirectReachDeps. Returns the deduplicated set of synthetic live
// deps across all parsed workflows that need a fresh reachability check
// because they're outside both the locked-SHA and live-moved sweeps. On
// a fully steady-state lockfile this is empty; on a brand-new repo (no
// lockfile yet) it's the full live set.
func CollectLiveDirectReachDeps(parsed []checks.ParsedWorkflow, live []dep.Dependency) []dep.Dependency {
	if len(parsed) == 0 || len(live) == 0 {
		return nil
	}
	covered := make(map[ghapi.Reach]bool)
	existingByDepKey := make(map[string]dep.Dependency)
	for _, pw := range parsed {
		for _, d := range pw.ExistingDeps {
			owner, repo := d.OwnerRepo()
			covered[ghapi.ForReach(owner, repo, d.SHA, d.Ref)] = true
			existingByDepKey[d.Key()] = d
		}
	}
	for _, d := range live {
		ed, ok := existingByDepKey[d.Key()]
		if !ok || strings.EqualFold(ed.SHA, d.SHA) {
			continue
		}
		owner, repo := d.OwnerRepo()
		covered[ghapi.ForReach(owner, repo, d.SHA, d.Ref)] = true
	}
	seen := make(map[ghapi.Reach]bool, len(live))
	var out []dep.Dependency
	for _, d := range live {
		owner, repo := d.OwnerRepo()
		k := ghapi.ForReach(owner, repo, d.SHA, d.Ref)
		if covered[k] || seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, d)
	}
	return out
}

// liveMovedDeps is the per-workflow analogue of CollectLiveMovedReachDeps.
// Returns synthetic (NWO, Ref, LIVE SHA) deps for any existing dep whose
// live resolve differs from the recorded SHA.

// liveMovedDeps is the per-workflow analogue of CollectLiveMovedReachDeps.
// Returns synthetic (NWO, Ref, LIVE SHA) deps for any existing dep whose
// live resolve differs from the recorded SHA.
func liveMovedDeps(existing, live []dep.Dependency) []dep.Dependency {
	if len(existing) == 0 || len(live) == 0 {
		return nil
	}
	liveSHA := make(map[string]string, len(live))
	liveDep := make(map[string]dep.Dependency, len(live))
	for _, d := range live {
		liveSHA[d.Key()] = d.SHA
		liveDep[d.Key()] = d
	}
	seen := make(map[ghapi.Reach]bool)
	var out []dep.Dependency
	for _, d := range existing {
		ls, ok := liveSHA[d.Key()]
		if !ok || strings.EqualFold(ls, d.SHA) {
			continue
		}
		synthetic := d
		synthetic.SHA = ls
		if ld, ok := liveDep[d.Key()]; ok && ld.NWO != "" {
			synthetic.NWO = ld.NWO
		}
		owner, repo := synthetic.OwnerRepo()
		k := ghapi.ForReach(owner, repo, synthetic.SHA, synthetic.Ref)
		if seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, synthetic)
	}
	return out
}

// partitionReachByLive splits existing deps into the set that needs a fresh
// reachability network check and the set that can be synthesized as
// Reachable because the freshly-resolved live deps confirm the recorded
// (NWO, Ref, SHA) is still what the ref resolves to right now.
//
// When skipUnchanged is false, every existing dep goes to toCheck. This
// is the --rescan path: re-verify every recorded pin against current
// upstream branches.

// partitionReachByLive splits existing deps into the set that needs a fresh
// reachability network check and the set that can be synthesized as
// Reachable because the freshly-resolved live deps confirm the recorded
// (NWO, Ref, SHA) is still what the ref resolves to right now.
//
// When skipUnchanged is false, every existing dep goes to toCheck. This
// is the --rescan path: re-verify every recorded pin against current
// upstream branches.
func partitionReachByLive(existing, live []dep.Dependency, skipUnchanged bool) (toCheck []dep.Dependency, trusted []resolve.ReachabilityResult) {
	if !skipUnchanged || len(live) == 0 {
		return existing, nil
	}
	liveSHA := make(map[string]string, len(live))
	for _, d := range live {
		liveSHA[d.Key()] = d.SHA
	}
	for _, d := range existing {
		sha, ok := liveSHA[d.Key()]
		if !ok || !strings.EqualFold(sha, d.SHA) {
			toCheck = append(toCheck, d)
			continue
		}
		owner, repo := d.OwnerRepo()
		trusted = append(trusted, resolve.ReachabilityResult{
			Owner:  owner,
			Repo:   repo,
			Ref:    d.Ref,
			SHA:    d.SHA,
			DepKey: d.Key(),
			Status: resolve.Reachable,
			Detail: "lockfile entry unchanged and live resolve confirms SHA — prior reachability verification retained",
		})
	}
	return toCheck, trusted
}

// reachabilityComplementFindings covers the cases the engine doesn't:
//   - Impostor for transitive (composite-expanded) deps the engine never
//     visits because they aren't in workflow uses.
//   - Reachability-Unknown warnings for all deps (engine fails open on
//     Unknown). Direct + transitive both get a warning so the user knows
//     the check was inconclusive.
