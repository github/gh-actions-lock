package lockfile

import (
	"fmt"
	"strings"

	parserlock "github.com/github/actions-workflow-parser/go/lockfile"
)

// dependencyToPin converts a Dependency into a parserlock.Pin without any
// case-normalization — callers should rely on Pin.String / Pin.Canonical for
// the canonical form.
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
	path := ""
	if rest := strings.TrimPrefix(d.NWO, owner+"/"+repo); rest != "" {
		path = strings.TrimPrefix(rest, "/")
	}
	return parserlock.Pin{
		Owner: owner,
		Repo:  repo,
		Path:  path,
		Ref:   d.Ref,
		Algo:  d.HashAlgoOrDetect(),
		Hex:   d.SHA,
	}, nil
}

// pinToDependency converts a parser Pin back into the gh-actions-pin
// internal Dependency type. Pin fields produced by parserlock.ParsePin are
// already canonical, so no extra normalization is needed here.
func pinToDependency(p parserlock.Pin) Dependency {
	return Dependency{
		NWO:      p.FullName(),
		Ref:      p.Ref,
		SHA:      p.Hex,
		HashAlgo: p.Algo,
	}
}
