package pinpool

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
)

// fakeReporter captures pool UI calls so tests can assert on the labels and
// worker rows the pool produced. All methods are mutex-guarded because
// the pool calls them from worker goroutines.
type fakeReporter struct {
	mu       sync.Mutex
	labels   []string
	statuses [][2]any // [slot, status]
}

func (f *fakeReporter) SetWorkerStatus(slot int, status string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.statuses = append(f.statuses, [2]any{slot, status})
}

func (f *fakeReporter) UpdateLabel(label string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.labels = append(f.labels, label)
}

func TestRunSerializesReporterCalls(t *testing.T) {
	// Mirror *ui.UI's UpdateLabel: shared state without internal
	// locking. If the pool ever calls Reporter from two goroutines at
	// once, -race trips on this Reporter.
	type racyReporter struct {
		labels   []string
		latest   int
		statuses int
	}
	rr := &racyReporter{}
	setStatus := func(slot int, status string) { rr.statuses++ }
	updateLabel := func(label string) {
		rr.labels = append(rr.labels, label)
		rr.latest = len(rr.labels)
	}
	r := reporterFunc{set: setStatus, label: updateLabel}

	jobs := make([]int, 64)
	err := Run(8, r, "Pinning", jobs,
		func(j int) string { return "" },
		func(slot, j int) error { return nil },
	)
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if rr.latest != len(rr.labels) {
		t.Fatalf("racy reporter saw torn state: latest=%d len=%d", rr.latest, len(rr.labels))
	}
}

// reporterFunc adapts plain functions to the Reporter interface so the
// race test above can pass a lock-free Reporter without writing a
// struct just to hold two callbacks.
type reporterFunc struct {
	set   func(slot int, status string)
	label func(label string)
}

func (f reporterFunc) SetWorkerStatus(slot int, status string) { f.set(slot, status) }
func (f reporterFunc) UpdateLabel(label string)                { f.label(label) }

// TestRunDoesNotClearSlotBetweenJobs guards the spinner tail UX fix: each
// worker must keep its slot showing the previous "→ path" until it overwrites
// it with the next job's "→ path", so the spinner never flickers down to an
// empty slot in the millisecond between Run returning and the next iteration.
// A single trailing clear (empty status) is permitted — and required — once
// the channel drains and the worker exits, because the caller relies on it to
// wipe stale rows from the screen at the end of the run.
func TestRunDoesNotClearSlotBetweenJobs(t *testing.T) {
	ui := &fakeReporter{}
	// Force a single worker so all status events are on slot 0 and the
	// "between two jobs" predicate is unambiguous.
	jobs := []int{1, 2, 3, 4, 5}
	err := Run(1, ui, "Pinning", jobs,
		func(j int) string { return fmt.Sprintf("job-%d", j) },
		func(slot, j int) error { return nil },
	)
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}

	// Filter to slot 0 (the only one used) and scan for the bad pattern:
	// "" (clear) followed by another "→ ..." on the same slot — that's the
	// spinner blink we're guarding against.
	var slot0 []string
	for _, s := range ui.statuses {
		if s[0].(int) != 0 {
			continue
		}
		slot0 = append(slot0, s[1].(string))
	}
	for i := 0; i < len(slot0)-1; i++ {
		if slot0[i] == "" && slot0[i+1] != "" {
			t.Fatalf("slot 0 was cleared between jobs (index %d): %v", i, slot0)
		}
	}
	// The exit-time clear is non-negotiable: the caller (the Remediator)
	// relies on it to wipe the worker row after the pool finishes.
	if len(slot0) == 0 || slot0[len(slot0)-1] != "" {
		t.Fatalf("expected final status on slot 0 to be \"\" (exit-time clear), got %v", slot0)
	}
}

func TestRunEmptyJobsIsNoOp(t *testing.T) {
	ui := &fakeReporter{}
	err := Run[int](4, ui, "Pinning", nil,
		func(j int) string { return fmt.Sprintf("%d", j) },
		func(slot, j int) error { return nil },
	)
	if err != nil {
		t.Fatalf("Run with no jobs returned error: %v", err)
	}
	if len(ui.labels) != 0 || len(ui.statuses) != 0 {
		t.Fatalf("Run with no jobs touched UI: labels=%v statuses=%v", ui.labels, ui.statuses)
	}
}

func TestRunCallsRunForEveryJob(t *testing.T) {
	ui := &fakeReporter{}
	jobs := []int{1, 2, 3, 4, 5, 6, 7}
	var seen sync.Map
	var calls atomic.Int64
	err := Run(3, ui, "Pinning", jobs,
		func(j int) string { return fmt.Sprintf("job-%d", j) },
		func(slot, j int) error {
			calls.Add(1)
			seen.Store(j, true)
			return nil
		},
	)
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if got := calls.Load(); got != int64(len(jobs)) {
		t.Fatalf("run called %d times, want %d", got, len(jobs))
	}
	for _, j := range jobs {
		if _, ok := seen.Load(j); !ok {
			t.Fatalf("job %d never ran", j)
		}
	}
	// First label must be the [0/N] initial update.
	if len(ui.labels) == 0 {
		t.Fatalf("no labels emitted")
	}
	want := fmt.Sprintf("[0/%d] Pinning", len(jobs))
	if ui.labels[0] != want {
		t.Fatalf("first label = %q, want %q", ui.labels[0], want)
	}
	// Some label must report the terminal [N/N] state. Ordering across
	// goroutines is not guaranteed (each worker reads `done` after its
	// own Add and labels are appended under a separate mutex), so we
	// can't rely on the last slice entry being the highest count.
	wantTerminal := fmt.Sprintf("[%d/%d] Pinning", len(jobs), len(jobs))
	found := false
	for _, l := range ui.labels {
		if l == wantTerminal {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("no label matched terminal %q; got %v", wantTerminal, ui.labels)
	}
}

func TestRunReturnsFirstErrorButDrainsAllJobs(t *testing.T) {
	ui := &fakeReporter{}
	jobs := []int{1, 2, 3, 4, 5, 6}
	sentinel := errors.New("boom")
	var calls atomic.Int64
	err := Run(2, ui, "Pinning", jobs,
		func(j int) string { return fmt.Sprintf("%d", j) },
		func(slot, j int) error {
			calls.Add(1)
			if j == 3 || j == 5 {
				return sentinel
			}
			return nil
		},
	)
	if !errors.Is(err, sentinel) {
		t.Fatalf("Run err = %v, want sentinel", err)
	}
	if got := calls.Load(); got != int64(len(jobs)) {
		t.Fatalf("run called %d times, want %d (pool must drain on error)", got, len(jobs))
	}
}

func TestRunClampsWorkersToJobCount(t *testing.T) {
	ui := &fakeReporter{}
	jobs := []int{1, 2}
	maxSlot := atomic.Int64{}
	err := Run(99, ui, "Pinning", jobs,
		func(j int) string { return "" },
		func(slot, j int) error {
			for {
				cur := maxSlot.Load()
				if int64(slot) <= cur {
					break
				}
				if maxSlot.CompareAndSwap(cur, int64(slot)) {
					break
				}
			}
			return nil
		},
	)
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if maxSlot.Load() >= int64(len(jobs)) {
		t.Fatalf("saw slot %d with only %d jobs; workers not clamped", maxSlot.Load(), len(jobs))
	}
}

func TestRunDefaultWorkersWhenNonPositive(t *testing.T) {
	ui := &fakeReporter{}
	jobs := make([]int, DefaultWorkers+5)
	for i := range jobs {
		jobs[i] = i
	}
	var calls atomic.Int64
	err := Run(0, ui, "Pinning", jobs,
		func(j int) string { return "" },
		func(slot, j int) error {
			calls.Add(1)
			return nil
		},
	)
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if got := calls.Load(); got != int64(len(jobs)) {
		t.Fatalf("calls = %d, want %d", got, len(jobs))
	}
}
