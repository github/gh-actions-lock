// Package lockfile manages CLI lockfile state: loading, saving, and
// converting the on-disk format.
package lockfile

import (
	"fmt"
	"strings"

	parserlock "github.com/github/actions-lockfile/go/pkg/lockfile"
	"github.com/github/gh-actions-lock/internal/dep"
	"github.com/github/gh-actions-lock/internal/ghapi"
)

// depToPin converts a dep.Dependency into a parserlock.Pin with its full-SHA
// ref spelled canonically; Pin.String folds owner/repo. Sub-action path is dropped: the lockfile pin grammar
// identifies a downloaded tarball at repo+sha granularity, and distinct
// subpaths in the same repo+ref collapse to one entry.
func depToPin(d dep.Dependency) (parserlock.Pin, error) {
	owner, repo := d.OwnerRepo()
	if owner == "" || repo == "" {
		return parserlock.Pin{}, fmt.Errorf("invalid NWO %q", d.NWO)
	}
	if d.Ref == "" {
		return parserlock.Pin{}, fmt.Errorf("missing ref for %s", d.NWO)
	}
	if d.SHA == "" {
		return parserlock.Pin{}, fmt.Errorf("missing SHA for %s@%s", d.NWO, d.Ref)
	}
	return parserlock.Pin{
		Owner: owner,
		Repo:  repo,
		Ref:   CanonicalRef(d.Ref),
	}, nil
}

// CanonicalRef spells a full commit SHA in lowercase and returns any other
// ref unchanged. The Actions backend matches a `uses:` ref to its lockfile
// key exactly, so a full SHA has one spelling in both.
func CanonicalRef(ref string) string {
	if parserlock.IsFullSha(ref) {
		return strings.ToLower(ref)
	}
	return ref
}

// ValidEntryRef reports whether ref is a lock entry ref the Actions backend
// resolver accepts for pin: a short tag or branch name it accepts (never a
// SHA, a refs/ name, revision syntax like main~1, or a `git describe` name),
// and the key's own ref unless the key is a full SHA. The
// resolver looks it up as refs/tags/<ref>, then refs/heads/<ref>, and
// requires the locked commit in its history.
func ValidEntryRef(pin parserlock.Pin, ref string) bool {
	if !ghapi.RecordableRefName(ref) {
		return false
	}
	return parserlock.IsFullSha(pin.Ref) || ref == pin.Ref
}

// RecordsOwnSHA reports whether a full-SHA key's entry records that same
// SHA as its ref. The Actions backend rejects it, but a run can re-record
// the tag or branch the commit came from, so such an entry loads instead
// of failing the lockfile. It stays recorded and tracked; only ref seeding
// (Seedable) and parity's ref lookup skip it.
func RecordsOwnSHA(pin parserlock.Pin, ref string) bool {
	return parserlock.IsFullSha(pin.Ref) && strings.EqualFold(ref, pin.Ref)
}

// Seedable reports whether a recorded dependency (from Get or AllDeps) may
// seed ref resolution or branch hints. One whose entry records its own SHA
// names no tag or branch, so it is resolved afresh; its host and repository
// identity still bind.
func Seedable(d dep.Dependency) bool {
	return !(parserlock.IsFullSha(d.Ref) && strings.EqualFold(parserlock.BestRef(d.Tag, d.Branch), d.Ref))
}

// UsedRoot returns the test for whether a workflow's recorded root is still
// used by refs, the workflow's current `uses:` (see RootUse).
func UsedRoot(refs []parserlock.ActionRef) func(pin parserlock.Pin, commit string) bool {
	use := RootUse(refs)
	return func(pin parserlock.Pin, commit string) bool {
		_, ok := use(pin, commit)
		return ok
	}
}

// RootUse returns the lookup of the use in refs, a workflow's current
// `uses:`, that still uses a recorded root: the use naming its key, else one
// naming its commit as a full SHA, since rewriting `uses:` to the commit
// keeps the tag-keyed entry. SHAs compare case-insensitively; other refs
// are case-sensitive. commit is the entry's algo-hex commit.
func RootUse(refs []parserlock.ActionRef) func(pin parserlock.Pin, commit string) (parserlock.ActionRef, bool) {
	byKey := make(map[string]parserlock.ActionRef, len(refs))
	bySHA := make(map[string]parserlock.ActionRef, len(refs))
	for _, ref := range refs {
		if k := parserlock.IndexKey(ref.Owner, ref.Repo, ref.Ref); !hasKey(byKey, k) {
			byKey[k] = ref
		}
		if k := strings.ToLower(ref.Owner + "/" + ref.Repo + "@" + ref.Ref); !hasKey(bySHA, k) {
			bySHA[k] = ref
		}
	}
	return func(pin parserlock.Pin, commit string) (parserlock.ActionRef, bool) {
		if ref, ok := byKey[pin.IndexKey()]; ok {
			return ref, true
		}
		sha := commit
		if _, hex, ok := strings.Cut(commit, "-"); ok {
			sha = hex
		}
		if sha == "" {
			return parserlock.ActionRef{}, false
		}
		ref, ok := bySHA[strings.ToLower(pin.Owner+"/"+pin.Repo+"@"+sha)]
		return ref, ok
	}
}

func hasKey(m map[string]parserlock.ActionRef, k string) bool {
	_, ok := m[k]
	return ok
}

// entryRefError says what is wrong with an entry's recorded ref. It reads
// as a predicate of "entry for <pin>".
func entryRefError(pin parserlock.Pin, ref string) error {
	switch {
	case ref == "":
		return fmt.Errorf("records no ref")
	case isSHARef(ref):
		return fmt.Errorf("records ref %q, a commit SHA rather than the tag or branch it was taken from", ref)
	case strings.HasPrefix(ref, "refs/"):
		return fmt.Errorf("records ref %q, which isn't a short tag or branch name", ref)
	case !ghapi.AcceptedRefName(ref):
		return fmt.Errorf("records ref %q, which isn't a tag or branch name the Actions backend accepts", ref)
	}
	return fmt.Errorf("records ref %q, which doesn't match its key's ref %q", ref, pin.Ref)
}

// pinToDep converts a parser Pin back into the internal dep.Dependency type.
// The lockfile-serialized Pin carries no Path, so the resulting Dependency
// has Path="" — sub-action paths are reconstructed at resolve time from
// workflow uses: strings.
func pinToDep(p parserlock.Pin) dep.Dependency {
	return dep.Dependency{
		NWO: p.NWO,
		Ref: p.Ref,
	}
}
