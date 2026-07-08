package dep

import "strings"

// Change pairs an old and new dependency that represent the same logical
// action across a lockfile update. Both fields are always populated.
type Change struct {
	Old Dependency
	New Dependency
}

// Diff is the result of comparing two dependency lists.
//
// Matching strategy:
//  1. Exact key match (NWO@Ref) — produces Changed (SHA differs) or Unchanged.
//  2. Fuzzy NWO match — unmatched deps with the same NWO are paired
//     preferring same-SHA first, then stable order. These appear in Rekeyed
//     (the ref changed but it's the same logical dependency).
//  3. Remaining unmatched new deps go to Added.
//  4. Remaining unmatched old deps go to Removed.
//
// Ordering: Changed, Rekeyed, and Unchanged preserve new-list order.
// Added preserves new-list order. Removed preserves old-list order.
type Diff struct {
	Changed   []Change     // Same Key, SHA differs
	Rekeyed   []Change     // Same NWO, different ref (tag moved)
	Added     []Dependency // In new but not old (no NWO match)
	Removed   []Dependency // In old but not new
	Unchanged []Dependency // Same Key, same SHA
}

// DiffDeps computes a structured diff between old and new dependency lists.
// If duplicate keys exist within a list, last-wins for exact matching.
func DiffDeps(old, new []Dependency) Diff {
	var diff Diff

	// Build old indices.
	oldByKey := make(map[string]Dependency, len(old))
	oldByNWO := make(map[string][]Dependency, len(old))
	for _, dep := range old {
		oldByKey[dep.Key()] = dep
		oldByNWO[dep.NWO] = append(oldByNWO[dep.NWO], dep)
	}

	handledOld := make(map[string]bool, len(old))
	handledNew := make(map[string]bool, len(new))

	// Pass 1: exact key matches.
	for _, dep := range new {
		oldDep, ok := oldByKey[dep.Key()]
		if !ok {
			continue
		}
		handledOld[oldDep.Key()] = true
		handledNew[dep.Key()] = true
		if strings.EqualFold(oldDep.SHA, dep.SHA) {
			diff.Unchanged = append(diff.Unchanged, dep)
		} else {
			diff.Changed = append(diff.Changed, Change{Old: oldDep, New: dep})
		}
	}

	// Pass 2: fuzzy NWO match for remaining deps.
	for _, dep := range new {
		if handledNew[dep.Key()] {
			continue
		}
		candidates := oldByNWO[dep.NWO]
		match := fuzzyMatch(dep, candidates, handledOld)
		if match == nil {
			continue
		}
		handledOld[match.Key()] = true
		handledNew[dep.Key()] = true
		diff.Rekeyed = append(diff.Rekeyed, Change{Old: *match, New: dep})
	}

	// Pass 3: remaining new deps are Added.
	for _, dep := range new {
		if handledNew[dep.Key()] {
			continue
		}
		diff.Added = append(diff.Added, dep)
	}

	// Pass 4: remaining old deps are Removed.
	for _, dep := range old {
		if handledOld[dep.Key()] {
			continue
		}
		diff.Removed = append(diff.Removed, dep)
	}

	return diff
}

// fuzzyMatch finds the best unhandled old dep with the same NWO.
// Prefers same-SHA first (ref-only change), then first unhandled.
func fuzzyMatch(target Dependency, candidates []Dependency, handled map[string]bool) *Dependency {
	var fallback *Dependency
	for i := range candidates {
		if handled[candidates[i].Key()] {
			continue
		}
		if strings.EqualFold(candidates[i].SHA, target.SHA) {
			return &candidates[i]
		}
		if fallback == nil {
			fallback = &candidates[i]
		}
	}
	return fallback
}

// PreserveRefs keeps existing human-readable refs for deps whose SHA hasn't
// changed. When re-resolving transitive deps, the parent action.yml often
// provides a bare SHA as the ref, losing the tag we already have in the
// lockfile. This restores it.
func PreserveRefs(old, new []Dependency) []Dependency {
	oldByNWO := make(map[string][]Dependency, len(old))
	for _, dep := range old {
		oldByNWO[dep.NWO] = append(oldByNWO[dep.NWO], dep)
	}

	result := make([]Dependency, len(new))
	copy(result, new)

	for i, dep := range result {
		for _, oldDep := range oldByNWO[dep.NWO] {
			if strings.EqualFold(oldDep.SHA, dep.SHA) && oldDep.Ref != dep.Ref {
				result[i].Ref = oldDep.Ref
				break
			}
		}
	}
	return result
}
