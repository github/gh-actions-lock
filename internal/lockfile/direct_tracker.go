package lockfile

import (
	"github.com/github/gh-actions-pin/internal/dep"
	parserlock "github.com/github/actions-lockfile/go/pkg/lockfile"
)

// DirectTracker captures, by dep index, which resolved deps correspond to a
// workflow's direct uses: refs. Build it from the freshly resolved deps
// (before any narrowing/normalization mutates dep.Ref), then read the final
// NWO@Ref direct-key set back with Keys after mutation.
//
// Keys takes the (possibly reassigned) deps slice so callers that swap the
// slice for an index-aligned copy — e.g. PreserveRefs — still resolve to the
// post-mutation refs. The index alignment is the contract: every transform
// between NewDirectTracker and Keys must preserve dep order and length.
//
// Directness is determined by the workflow file's own uses: refs, not by the
// resolver parent map. A dep that is both a direct use and a composite
// transitive dep (e.g. actions/setup-go used directly and also pulled in by a
// composite) has a parent in the parent map but must still be recorded as a
// workflow-direct pin — otherwise it is perpetually re-flagged as not-pinned.
type DirectTracker struct {
	direct []bool
}

// NewDirectTracker records which entries of deps match one of the workflow's
// direct refs, by NWO@Ref, at the deps' current (pre-mutation) refs.
func NewDirectTracker(refs []parserlock.ActionRef, deps []dep.Dependency) DirectTracker {
	want := make(map[string]bool, len(refs))
	for _, ref := range refs {
		want[ref.NWO()+"@"+ref.Ref] = true
	}
	direct := make([]bool, len(deps))
	for i, d := range deps {
		direct[i] = want[d.Key()]
	}
	return DirectTracker{direct: direct}
}

// Keys returns the set of workflow-direct NWO@Ref keys, reading each dep's
// current (post-mutation) Key() from the supplied index-aligned slice.
func (t DirectTracker) Keys(deps []dep.Dependency) map[string]bool {
	out := make(map[string]bool, len(deps))
	for i, d := range deps {
		if i < len(t.direct) && t.direct[i] {
			out[d.Key()] = true
		}
	}
	return out
}
