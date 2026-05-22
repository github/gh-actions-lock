package lockfile

import (
	"fmt"
	"strings"

	parserlock "github.com/github/actions-workflow-parser/go/lockfile"
)

// dependencyPinKey returns the canonical lockfile pin key for d:
// "OWNER/REPO[/PATH]@REF:ALGO-HEX".
func dependencyPinKey(d Dependency) (string, error) {
	owner, repo := d.OwnerRepo()
	if owner == "" || repo == "" {
		return "", fmt.Errorf("invalid NWO %q", d.NWO)
	}
	if d.Ref == "" {
		return "", fmt.Errorf("missing ref for %s", d.NWO)
	}
	if d.SHA == "" {
		return "", fmt.Errorf("missing SHA for %s@%s", d.NWO, d.Ref)
	}
	path := ""
	if rest := strings.TrimPrefix(d.NWO, owner+"/"+repo); rest != "" {
		path = strings.TrimPrefix(rest, "/")
	}
	pin := parserlock.Pin{
		Owner: owner,
		Repo:  repo,
		Path:  path,
		Ref:   d.Ref,
		Algo:  d.HashAlgoOrDetect(),
		Hex:   strings.ToLower(d.SHA),
	}
	return pin.String(), nil
}

// pinToDependency converts a parser Pin back into the gh-actions-pin
// internal Dependency type.
func pinToDependency(p parserlock.Pin) Dependency {
	return Dependency{
		NWO:      p.FullName(),
		Ref:      p.Ref,
		SHA:      strings.ToLower(p.Hex),
		HashAlgo: p.Algo,
	}
}
