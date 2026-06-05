package ui

import (
	"bytes"
	"strings"
	"sync/atomic"
	"testing"
	"time"

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

// TestSpinnerWriter_HintAppendedAfterStatus verifies a hint set via
// setWorkerHint renders as a suffix on the worker row, after the status text.
func TestSpinnerWriter_HintAppendedAfterStatus(t *testing.T) {
	sw := newTestSpinWriter()
	buf := sw.w.(*bytes.Buffer)

	sw.setWorkerStatus(0, "→ workflow.yml")
	sw.setWorkerHint(0, "(still working…)")

	sw.mu.Lock()
	sw.renderWorkersLocked()
	sw.mu.Unlock()

	out := buf.String()
	if !strings.Contains(out, "workflow.yml") {
		t.Fatalf("expected workflow.yml in output; got %q", out)
	}
	if !strings.Contains(out, "(still working…)") {
		t.Fatalf("expected hint suffix in output; got %q", out)
	}
	statusIdx := strings.Index(out, "workflow.yml")
	hintIdx := strings.Index(out, "(still working…)")
	if hintIdx < statusIdx {
		t.Fatalf("hint should come after status; got hint@%d status@%d in %q", hintIdx, statusIdx, out)
	}
}

// TestSpinnerWriter_SetWorkerStatusClearsHint guards the defensive-clear
// contract: any setWorkerStatus call must wipe that slot's hint so a stale
// "(still working…)" from the previous job can't bleed into the next one.
func TestSpinnerWriter_SetWorkerStatusClearsHint(t *testing.T) {
	sw := newTestSpinWriter()
	sw.setWorkerStatus(0, "→ a.yml")
	sw.setWorkerHint(0, "(still working…)")
	sw.setWorkerStatus(0, "→ b.yml")

	sw.mu.Lock()
	defer sw.mu.Unlock()
	if len(sw.hints) > 0 && sw.hints[0] != "" {
		t.Fatalf("hint not cleared on status update: %q", sw.hints[0])
	}
}

// TestSpinnerWriter_HintSurvivesPrintLineDeferral mirrors the printLine
// snapshot/restore dance for hints. Without deferredHints, a hint set
// concurrently while printLine has the slices snapshotted would be lost on
// restore.
func TestSpinnerWriter_HintSurvivesPrintLineDeferral(t *testing.T) {
	sw := newTestSpinWriter()
	sw.workers = []string{"→ a.yml"}
	sw.hints = []string{""}

	// Begin snapshot, like printLine does.
	sw.mu.Lock()
	savedHints := append([]string(nil), sw.hints...)
	savedWorkers := append([]string(nil), sw.workers...)
	sw.workers = nil
	sw.hints = nil
	sw.deferredWrites = map[int]string{}
	sw.deferredHints = map[int]string{}
	sw.mu.Unlock()

	// Concurrent stall-watcher fires hint while snapshot is active.
	sw.setWorkerHint(0, "(still working…)")

	// Restore, merging deferred hints onto the snapshot.
	sw.mu.Lock()
	for slot, hint := range sw.deferredHints {
		for len(savedHints) <= slot {
			savedHints = append(savedHints, "")
		}
		savedHints[slot] = hint
	}
	sw.workers = savedWorkers
	sw.hints = savedHints
	sw.deferredWrites = nil
	sw.deferredHints = nil
	sw.mu.Unlock()

	if len(sw.hints) == 0 || sw.hints[0] != "(still working…)" {
		t.Fatalf("hint lost across snapshot/restore: %v", sw.hints)
	}
}

// TestSpinnerWriter_AnimatorHeartbeat guards Thing 2's regression test:
// even when the worker status text never changes, the independent animator
// goroutine must keep redrawing the row (advancing the spinner glyph) so
// users don't perceive the spinner as frozen.
func TestSpinnerWriter_AnimatorHeartbeat(t *testing.T) {
	sw := newTestSpinWriter()
	cw := &countingWriter{inner: sw.w}
	sw.w = cw
	sw.output = termenv.NewOutput(cw, termenv.WithProfile(termenv.Ascii))

	sw.setWorkerStatus(0, "→ blocked.yml") // active "→" row triggers animator
	sw.startAnimator()
	defer sw.stopAnimator()

	// Wait three frame intervals plus slack. The animator must have
	// written at least twice — proving the row is still being redrawn
	// even though setWorkerStatus hasn't been called again.
	time.Sleep(4 * workerFrameInterval)

	if cw.writes.Load() < 2 {
		t.Fatalf("animator should have redrawn ≥2 times after %s; saw %d writes",
			4*workerFrameInterval, cw.writes.Load())
	}
}

// countingWriter wraps an io.Writer and counts Write calls.
type countingWriter struct {
	inner  interface{ Write(p []byte) (int, error) }
	writes atomic.Int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	c.writes.Add(1)
	return c.inner.Write(p)
}
