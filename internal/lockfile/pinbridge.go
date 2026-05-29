package lockfile

import (
	"fmt"

	parserlock "github.com/github/actions-workflow-parser/go/lockfile"
)

// dependencyToPin converts a Dependency into a parserlock.Pin without any
// case-normalization — callers should rely on Pin.String / Pin.Canonical for
// the canonical form. Path is not included in the pin — the resolution unit
// is the repository.
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
// internal Dependency type. Pin.NWO is always owner/repo (no path).
func pinToDependency(p parserlock.Pin) Dependency {
	return Dependency{
		NWO:      p.NWO,
		Ref:      p.Ref,
		SHA:      p.Hex,
		HashAlgo: p.Algo,
	}
}
