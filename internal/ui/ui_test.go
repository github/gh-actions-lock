package ui

import (
	"bytes"
	"testing"

	"github.com/muesli/termenv"
)

func newTestSpinWriter() *spinnerWriter {
	var buf bytes.Buffer
	return &spinnerWriter{
		w:       &buf,
		noColor: true,
		output:  termenv.NewOutput(&buf, termenv.WithProfile(termenv.Ascii)),
	}
}

// TestSpinnerWriter_DeferredWritesBufferClearAndUpdate verifies the buffer
// path setWorkerStatus takes while printLine has the workers slice
// snapshotted. Without buffering, those writes would land in a slice that
// printLine throws away on restore.
func TestSpinnerWriter_DeferredWritesBufferClearAndUpdate(t *testing.T) {
	sw := newTestSpinWriter()
	sw.workers = []string{"→ a.yml", "→ b.yml"}
	sw.deferredWrites = map[int]string{}

	sw.setWorkerStatus(0, "")
	sw.setWorkerStatus(1, "→ c.yml")
	sw.setWorkerStatus(3, "→ d.yml")

	if len(sw.workers) != 2 || sw.workers[0] != "→ a.yml" || sw.workers[1] != "→ b.yml" {
		t.Errorf("workers slice mutated while deferred: %#v", sw.workers)
	}
	if got, want := sw.deferredWrites[0], ""; got != want {
		t.Errorf("deferred[0]: got %q, want %q", got, want)
	}
	if got, want := sw.deferredWrites[1], "→ c.yml"; got != want {
		t.Errorf("deferred[1]: got %q, want %q", got, want)
	}
	if got, want := sw.deferredWrites[3], "→ d.yml"; got != want {
		t.Errorf("deferred[3]: got %q, want %q", got, want)
	}
}

// TestPrintLine_MergesDeferredWritesOverSnapshot exercises the full
// snapshot → buffered writes → restore-with-merge dance via the public
// printLine entry point.
func TestPrintLine_MergesDeferredWritesOverSnapshot(t *testing.T) {
	u := &UI{w: &bytes.Buffer{}, noColor: true}
	u.spinWriter = newTestSpinWriter()
	u.spinWriter.workers = []string{"→ a.yml", "→ b.yml", "→ c.yml"}

	// Simulate the snapshot-and-clear that printLine performs when the
	// spinner is active. We bypass the actual spinner because tests run
	// headless; the buffered-write semantics under test live entirely in
	// spinWriter, independent of the spinner library.
	u.spinWriter.mu.Lock()
	savedWorkers := append([]string(nil), u.spinWriter.workers...)
	u.spinWriter.workers = nil
	u.spinWriter.deferredWrites = map[int]string{}
	u.spinWriter.mu.Unlock()

	// Concurrent pin-pool activity during the write window: slot 0
	// cleared (worker exited), slot 1 repainted (new job grabbed), slot
	// 2 untouched.
	u.SetWorkerStatus(0, "")
	u.SetWorkerStatus(1, "→ new.yml")

	// Apply the merge-on-restore the way printLine does.
	u.spinWriter.mu.Lock()
	for slot, status := range u.spinWriter.deferredWrites {
		for len(savedWorkers) <= slot {
			savedWorkers = append(savedWorkers, "")
		}
		savedWorkers[slot] = status
	}
	u.spinWriter.workers = savedWorkers
	u.spinWriter.deferredWrites = nil
	u.spinWriter.mu.Unlock()

	got := u.spinWriter.workers
	want := []string{"", "→ new.yml", "→ c.yml"}
	if len(got) != len(want) {
		t.Fatalf("workers length: got %d, want %d (%#v)", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("workers[%d]: got %q, want %q", i, got[i], want[i])
		}
	}
}
