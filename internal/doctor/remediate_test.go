package doctor

import "testing"

// TestDedupePinsByPath guards the parallel-pin-pool UX fix: submitPin runs
// once per finding, so a workflow with multiple findings (e.g. three
// SHA-as-ref entries that each take the applySHAToTag → submitPin path) can
// end up enqueued multiple times. Without this dedupe the pool would launch
// concurrent workers on the same file — wasted work, racy file writes, and
// stacked sub-spinner rows for one workflow.
func TestDedupePinsByPath(t *testing.T) {
	pins := []WorkflowReport{
		{Path: ".github/workflows/codeql.yml"},
		{Path: ".github/workflows/secret-rotation.yml"},
		{Path: ".github/workflows/codeql.yml"},
		{Path: ".github/workflows/codeql.yml"},
		{Path: ".github/workflows/release.yml"},
		{Path: ".github/workflows/secret-rotation.yml"},
	}

	got := dedupePinsByPath(pins)

	want := []string{
		".github/workflows/codeql.yml",
		".github/workflows/secret-rotation.yml",
		".github/workflows/release.yml",
	}
	if len(got) != len(want) {
		t.Fatalf("dedupePinsByPath returned %d pins, want %d (%v)", len(got), len(want), got)
	}
	for i, p := range got {
		if p.Path != want[i] {
			t.Fatalf("pin[%d].Path = %q, want %q (full list: %v)", i, p.Path, want[i], got)
		}
	}
}

func TestDedupePinsByPathEmpty(t *testing.T) {
	if got := dedupePinsByPath(nil); len(got) != 0 {
		t.Fatalf("dedupePinsByPath(nil) = %v, want empty", got)
	}
}
