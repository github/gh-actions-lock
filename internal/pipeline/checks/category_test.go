package checks

import "testing"

// TestCategoryStringsAreFrozen pins the Category string vocabulary.
// These string values are surfaced to consumers (SARIF rule IDs, JSON
// output, doc URL slugs). Renaming a constant's string is a breaking
// change; this test fails loudly so the breakage is intentional.
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
		{MisleadingSHA, "misleading-sha"},
		{LockfileForgery, "lockfile-forgery"},
		{Valid, "valid"},
		{RunOnly, "run-only"},
		{AncestryUnknown, "ancestry-unknown"},
		{ReachabilityUnknown, "reachability-unknown"},
		{OnboardingRequired, "onboarding-required"},
		{VersionRef, "version-ref"},
		{LocalAction, "local-action"},
		{SelfRepositoryAction, "self-reference-action"},
		{InvalidSelfRepositoryRef, "invalid-self-reference"},
	}
	for _, c := range cases {
		if string(c.got) != c.want {
			t.Errorf("Category renamed: got %q, want %q (this is a breaking change to the schema)", string(c.got), c.want)
		}
	}
}

// TestCategoryIsInconclusive guards the inconclusive partition so a
// new diagnostic category isn't silently treated as blocking by
// consumers that key off this predicate.
func TestCategoryIsInconclusive(t *testing.T) {
	inconclusive := []Category{AncestryUnknown, ReachabilityUnknown}
	for _, c := range inconclusive {
		if !c.IsInconclusive() {
			t.Errorf("%q must be inconclusive", string(c))
		}
	}
	blocking := []Category{
		NotPinned, ShaAsRef, RefChanged, RefMoved, Stale,
		MisleadingSHA, LockfileForgery,
		Valid, RunOnly, OnboardingRequired, VersionRef, LocalAction,
		SelfRepositoryAction, InvalidSelfRepositoryRef,
	}
	for _, c := range blocking {
		if c.IsInconclusive() {
			t.Errorf("%q must not be inconclusive", string(c))
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
			t.Errorf("Severity renamed: got %q, want %q (this is a breaking change to the schema)", string(c.got), c.want)
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
			t.Errorf("Confidence renamed: got %q, want %q (this is a breaking change to the schema)", string(c.got), c.want)
		}
	}
}
