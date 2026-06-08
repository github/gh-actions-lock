package resolve

import (
	"fmt"

	parserlock "github.com/github/actions-lockfile/go/pkg/lockfile"
)

// ImpostorError indicates a commit that is not reachable from any branch — a
// fork-network / impostor signal. It carries the offending action so callers
// can report which workflow is affected without abandoning the whole run.
type ImpostorError struct {
	NWO string // owner/repo
	Ref string // ref as written in the workflow
	SHA string // resolved commit SHA
}

func (e *ImpostorError) Error() string {
	return fmt.Sprintf("%s@%s (%s) is not on any branch — fork-network / impostor signal; refusing to pin", e.NWO, e.Ref, parserlock.ShortSHA(e.SHA))
}
