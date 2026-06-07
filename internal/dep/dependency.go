// Package dep defines the working dependency types shared between the
// resolver and the lockfile.
package dep

import (
	"fmt"
	"strings"

	parserlock "github.com/github/actions-lockfile/go/pkg/lockfile"
)

// Dependency is the resolver's in-memory view of a single pinned action:
// the lockfile-grammar pin (NWO@Ref:Algo-SHA) plus the discovered Tag /
// Branch / sub-action Path that the lockfile-on-disk format does not
// carry. It is the working shape between `uses:` parsing, resolver
// traversal, and lockfile serialization — never persisted on disk and
// not part of any public API.
type Dependency struct {
	NWO string // owner/repo (no path)
	// Path is the optional sub-action subpath as written in `uses:`
	// (e.g. "save" for actions/cache/save). It is preserved on the
	// in-memory dep so resolver-time graph traversal can fetch the
	// correct sub-action.yml, but it is NOT part of the lockfile pin
	// identity (the runner downloads at repo+sha granularity) and is
	// dropped at serialization time. Distinct subpaths in the same
	// repo+ref collapse to one lockfile entry.
	Path     string
	Ref      string // resolved ref as given in uses:
	SHA      string // full commit hash
	HashAlgo string // "sha1" or "sha256"
	// Tag is the discovered release/tag pointing at SHA, if any. Optional.
	// Populated by the pin-time discovery pass; not read from `uses:`.
	Tag string
	// Branch is the discovered branch containing SHA. Required at write
	// time — a commit not on any branch is an impostor / fork-network
	// signal. Populated by the pin-time discovery pass.
	Branch string
}

// FullName returns owner/repo or owner/repo/path.
func (d Dependency) FullName() string {
	if d.Path != "" {
		return d.NWO + "/" + d.Path
	}
	return d.NWO
}

// Key returns the dependency key for deduplication: NWO@Ref.
func (d Dependency) Key() string {
	return d.NWO + "@" + d.Ref
}

// OwnerRepo splits NWO into owner and repo components.
func (d Dependency) OwnerRepo() (string, string) {
	owner, repo, _ := parserlock.SplitNWO(d.NWO)
	return owner, repo
}

// HashAlgoOrDetect returns the hash algorithm, falling back to detection from SHA length.
func (d Dependency) HashAlgoOrDetect() string {
	if d.HashAlgo != "" {
		return strings.ToLower(d.HashAlgo)
	}
	return detectHashAlgo(d.SHA)
}

// String formats the dependency using the canonical pin form.
func (d Dependency) String() string {
	return fmt.Sprintf("%s@%s:%s-%s", d.NWO, d.Ref, d.HashAlgoOrDetect(), d.SHA)
}

func detectHashAlgo(hash string) string {
	if len(hash) == 64 {
		return "sha256"
	}
	return "sha1"
}

// Dedup returns a copy of deps with duplicates (by Key) removed,
// preserving first-seen order.
func Dedup(deps []Dependency) []Dependency {
	seen := make(map[string]bool, len(deps))
	out := make([]Dependency, 0, len(deps))
	for _, d := range deps {
		if k := d.Key(); !seen[k] {
			seen[k] = true
			out = append(out, d)
		}
	}
	return out
}

// ParentMap is a child dep key → parent dep keys mapping returned alongside
// resolved dependencies by ResolveAllRecursive. It is value-typed so callers
// can hold their own copy across concurrent calls without racing on resolver
// state.
type ParentMap map[string][]string

// RekeyParentMap returns a new ParentMap with both child keys and parent
// values rewritten according to rewrites (e.g. tag narrowing v4 → v4.3.1,
// or ReverseLookup replacing a SHA with a discovered tag). The input
// is not mutated.
func RekeyParentMap(pm ParentMap, rewrites map[string]string) ParentMap {
	if len(pm) == 0 {
		return ParentMap{}
	}
	if len(rewrites) == 0 {
		out := make(ParentMap, len(pm))
		for k, v := range pm {
			out[k] = append([]string(nil), v...)
		}
		return out
	}
	updated := make(ParentMap, len(pm))
	for childKey, parents := range pm {
		newChild := childKey
		if rk, ok := rewrites[childKey]; ok {
			newChild = rk
		}
		newParents := make([]string, len(parents))
		for i, p := range parents {
			if rk, ok := rewrites[p]; ok {
				newParents[i] = rk
			} else {
				newParents[i] = p
			}
		}
		updated[newChild] = newParents
	}
	return updated
}
