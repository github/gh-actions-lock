package pipeline

import (
	"github.com/github/gh-actions-pin/internal/dep"
	"github.com/github/gh-actions-pin/internal/ghapi"
	"github.com/github/gh-actions-pin/internal/pipeline/checks"
)

// attachParent looks up the dep's composite-expansion parents (if any)
// and surfaces the first one as checks.Finding.ParentNWO. Direct (workflow-level)
// uses don't get a parent attached even if one exists in the graph.
//
// Findings emitted by RunChecks already carry an ActionRef for direct uses
// and a Dependency synthesized from the workflow ref / lockfile pin. This
// is purely about pointing the user at the composite that pulled in a
// transitively-pinned dep.
func attachParent(f *checks.Finding, depByKey map[string]dep.Dependency, directNWOs map[ghapi.Repo]bool, parentMap map[string][]string) {
	if f.Dependency == nil {
		return
	}
	owner, repo := f.Dependency.OwnerRepo()
	if directNWOs[ghapi.ForRepo(owner, repo)] {
		return
	}
	// Prefer the dep snapshot from the workflow's ExistingDeps (it has the
	// canonical NWO casing the parent map keys with). Synthesised deps
	// already match — but the indexed lookup is cheap regardless.
	key := f.Dependency.Key()
	if dep, ok := depByKey[key]; ok {
		key = dep.Key()
	}
	if parents := parentMap[key]; len(parents) > 0 {
		f.ParentNWO = parents[0]
	}
}

// isTransitivePin reports whether the finding refers to a dep reached via
// composite expansion (i.e. has parents in the parent map).

// isTransitivePin reports whether the finding refers to a dep reached via
// composite expansion (i.e. has parents in the parent map).
func isTransitivePin(f checks.Finding, depByKey map[string]dep.Dependency, parentMap map[string][]string) bool {
	if f.Dependency == nil {
		return false
	}
	if _, ok := depByKey[f.Dependency.Key()]; !ok {
		return false
	}
	return len(parentMap[f.Dependency.Key()]) > 0
}

// CollectReachDeps returns the deduplicated union of existing deps across the
// given parsed workflows that will need a fresh reachability network check
// once diagnostics runs. It mirrors the per-workflow partition diagnose
// performs internally (see partitionReachByLive) but operates over the union,
// so callers can pre-warm CheckReachabilityAll once across every unresolved
// workflow instead of paying the per-workflow repo-warmup + per-dep
// concurrency cost serially. Pass live as the result of a single
// ResolveAllRecursive over the union of refs (the resolver cache makes the
// per-workflow re-lookups inside diagnose free).

func populateInventoryParents(inventory []checks.InventoryEntry, parentMap map[string][]string) {
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
