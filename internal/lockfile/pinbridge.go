package lockfile

import (
	"fmt"

	"github.com/github/gh-actions-pin/internal/lockfile/parserlock"
)

// dependencyToPin converts a Dependency into a parserlock.Pin without any
// case-normalization — callers should rely on Pin.String / Pin.Canonical for
// the canonical form. Sub-action path is intentionally dropped: the lockfile
// pin grammar identifies a downloaded tarball at repo+sha granularity, and
// distinct subpaths in the same repo+ref collapse to one entry.
func dependencyToPin(d Dependency) (parserlock.Pin, error) {
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
		Algo:  d.HashAlgoOrDetect(),
		Hex:   d.SHA,
	}, nil
}

// pinToDependency converts a parser Pin back into the gh-actions-pin
// internal Dependency type. The lockfile-serialized Pin carries no Path,
// so the resulting Dependency has Path="" — sub-action paths are
// reconstructed at resolve time from workflow `uses:` strings.
func pinToDependency(p parserlock.Pin) Dependency {
	return Dependency{
		NWO:      p.NWO,
		Ref:      p.Ref,
		SHA:      p.Hex,
		HashAlgo: p.Algo,
	}
}
