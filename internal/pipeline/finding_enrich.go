package pipeline

import (
	"github.com/github/gh-actions-lock/internal/dep"
	"github.com/github/gh-actions-lock/internal/ghapi"
	"github.com/github/gh-actions-lock/internal/pipeline/checks"
)

// attachParent looks up the dep's composite-expansion parents (if any)
// and surfaces the first one as checks.Finding.ParentNWO. Direct (workflow-level)
// uses don't get a parent attached even if one exists in the graph.
//
// Findings emitted by RunChecks already carry an ActionRef for direct uses
// and a Dependency synthesized from the workflow ref / lockfile pin. This
// is purely about pointing the user at the composite that pulled in a
// transitively-pinned dep.
func attachParent(f *checks.Finding, depByKey map[string]dep.Dependency, directRefs map[ghapi.NWORef]bool, parentMap map[string][]string) {
	if f.Dependency == nil {
		return
	}
	owner, repo := f.Dependency.OwnerRepo()
	if directRefs[ghapi.ForNWORef(owner, repo, f.Dependency.Ref)] {
		return
	}
	// Prefer the dep snapshot from the workflow's RecordedDeps (it has the
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
func isTransitivePin(f checks.Finding, depByKey map[string]dep.Dependency, parentMap map[string][]string) bool {
	if f.Dependency == nil {
		return false
	}
	if _, ok := depByKey[f.Dependency.Key()]; !ok {
		return false
	}
	return len(parentMap[f.Dependency.Key()]) > 0
}

// populateInventoryParents fills in the Parents field for transitive inventory
// entries (those not marked Direct and without parents yet) by looking up each
// entry's dep key in parentMap.
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

func mergeParentMaps(maps ...dep.ParentMap) dep.ParentMap {
	merged := make(dep.ParentMap)
	for _, parentMap := range maps {
		for child, parents := range parentMap {
			seen := make(map[string]bool, len(merged[child]))
			for _, parent := range merged[child] {
				seen[parent] = true
			}
			for _, parent := range parents {
				if !seen[parent] {
					merged[child] = append(merged[child], parent)
					seen[parent] = true
				}
			}
		}
	}
	return merged
}
