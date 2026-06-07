package pin

import "encoding/json"

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

// MarshalJSON emits the resolution string.
func (r Resolution) MarshalJSON() ([]byte, error) {
	return json.Marshal(string(r))
}

// UnmarshalJSON parses a resolution string.
func (r *Resolution) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	*r = Resolution(s)
	return nil
}
