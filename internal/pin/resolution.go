package pin

// Resolution describes the outcome for a single action reference.
type Resolution string

// Resolution values reported for an action reference.
const (
	Pinned      Resolution = "pinned"
	Verified    Resolution = "verified"
	Investigate Resolution = "needs-investigation"
	Skipped     Resolution = "skipped"
	Unresolved  Resolution = "unresolved"
)

// String returns the resolution as a string.
func (r Resolution) String() string { return string(r) }
