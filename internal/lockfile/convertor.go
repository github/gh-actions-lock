// Package lockfile manages CLI lockfile state: loading, saving, and
// converting the on-disk format.
package lockfile

import (
	"fmt"
	"strings"

	parserlock "github.com/github/actions-lockfile/go/pkg/lockfile"
	"github.com/github/gh-actions-lock/internal/dep"
)

// depToPin converts a dep.Dependency into a parserlock.Pin without any
// case-normalization — callers should rely on Pin.String / Pin.Canonical for
// the canonical form. Sub-action path is dropped: the lockfile pin grammar
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
		Ref:   d.Ref,
	}, nil
}

// ValidEntryRef checks that metadata records a non-empty symbolic ref,
// matching the key unless the key is a full SHA.
func ValidEntryRef(pin parserlock.Pin, ref string) bool {
	if ref == "" || isSHARef(ref) || strings.HasPrefix(ref, "refs/") {
		return false
	}
	return parserlock.IsFullSha(pin.Ref) || ref == pin.Ref
}

func entryRefError(pin parserlock.Pin, ref string) error {
	switch {
	case ref == "":
		return fmt.Errorf("missing ref")
	case isSHARef(ref):
		return fmt.Errorf("ref %q is a commit SHA, not the tag or branch it was taken from", ref)
	case strings.HasPrefix(ref, "refs/"):
		return fmt.Errorf("ref %q must be a short tag or branch name", ref)
	}
	return fmt.Errorf("ref %q does not match the key's ref %q", ref, pin.Ref)
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
