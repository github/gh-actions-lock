package findings

import "testing"

// TestCategoryStringsAreFrozen pins the Category string vocabulary.
// These string values are part of the public schema (SARIF rule IDs,
// JSON output, doc URL slugs). Renaming a constant's string is a
// breaking change for downstream consumers; this test fails loudly so
// the breakage is intentional.
func TestCategoryStringsAreFrozen(t *testing.T) {
	cases := []struct {
		got  Category
		want string
	}{
		{NotPinned, "not-pinned"},
		{ShaAsRef, "sha-as-ref"},
		{RefChanged, "ref-changed"},
		{RefMoved, "ref-moved"},
		{Stale, "stale"},
		{ImpostorCommit, "impostor-commit"},
		{MisleadingSHA, "misleading-sha"},
		{LockfileForgery, "lockfile-forgery"},
		{Valid, "valid"},
		{RunOnly, "run-only"},
	}
	for _, c := range cases {
		if string(c.got) != c.want {
			t.Errorf("Category renamed: got %q, want %q (this is a breaking change to the public schema)", string(c.got), c.want)
		}
	}
}

// TestSeverityStringsAreFrozen pins the Severity string vocabulary.
func TestSeverityStringsAreFrozen(t *testing.T) {
	cases := []struct {
		got  Severity
		want string
	}{
		{SeverityOK, "ok"},
		{SeverityInfo, "info"},
		{SeverityWarning, "warning"},
		{SeverityError, "error"},
	}
	for _, c := range cases {
		if string(c.got) != c.want {
			t.Errorf("Severity renamed: got %q, want %q (this is a breaking change to the public schema)", string(c.got), c.want)
		}
	}
}

// TestConfidenceStringsAreFrozen pins the Confidence string vocabulary.
func TestConfidenceStringsAreFrozen(t *testing.T) {
	cases := []struct {
		got  Confidence
		want string
	}{
		{ConfidenceLow, "low"},
		{ConfidenceMedium, "medium"},
		{ConfidenceHigh, "high"},
	}
	for _, c := range cases {
		if string(c.got) != c.want {
			t.Errorf("Confidence renamed: got %q, want %q (this is a breaking change to the public schema)", string(c.got), c.want)
		}
	}
}
