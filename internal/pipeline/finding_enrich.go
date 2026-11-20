package pipeline

import (
	parserlock "github.com/github/actions-lockfile/go/pkg/lockfile"
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
// composite expansion (i.e. has parents in the parent map) that the lock
// records only under the workflow, as a lock without `uses:` edges does.
// When a locked entry's `uses:` already lists the dep, that edge keeps it
// and the workflow's own entry for it is stale (#296). Any entry counts, not
// just a live parent's, since a live parent may be bumped in the same run.
func isTransitivePin(f checks.Finding, depByKey map[string]dep.Dependency, parentMap map[string][]string, lockedChildren map[string]bool) bool {
	if f.Dependency == nil {
		return false
	}
	if _, ok := depByKey[f.Dependency.Key()]; !ok {
		return false
	}
	if lockedChildren[canonicalPin(f.Dependency.Key())] {
		return false
	}
	return len(parentMap[f.Dependency.Key()]) > 0
}

// lockedChildren returns every pin key a locked entry lists in `uses:`.
func lockedChildren(deps map[string]parserlock.Action) map[string]bool {
	out := make(map[string]bool)
	for _, a := range deps {
		for _, u := range a.Uses {
			out[u] = true
		}
	}
	return out
}

func canonicalPin(key string) string {
	if p, ok := parserlock.ParsePin(key); ok {
		return p.String()
	}
	return key
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
