package ghapi

import "fmt"

// AmbiguousRefError identifies a short name shared by different tag and branch commits.
type AmbiguousRefError struct {
	Owner, Repo, Ref string
}

func (e *AmbiguousRefError) Error() string {
	return fmt.Sprintf("ref %s is ambiguous in %s/%s: its tag and branch point at different commits", e.Ref, e.Owner, e.Repo)
}

// RefCollisionRemediation avoids suggesting a repin, which cannot disambiguate a name.
const RefCollisionRemediation = "choose a distinct tag or branch name and update the reference; relocking the same ambiguous name cannot fix it"

func fullCommitRef(ref string) bool {
	return (len(ref) == 40 || len(ref) == 64) && isHexString(ref)
}
