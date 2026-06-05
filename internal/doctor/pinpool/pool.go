// Package pinpool runs the parallel pin pass for `gh actions pin doctor`.
//
// A pin can involve several network round-trips, so Run spreads workflow jobs
// across worker goroutines and returns the first error after all jobs finish.
package pinpool

import (
	"fmt"
	"sync"
	"sync/atomic"
)

// DefaultWorkers is a reasonable number of concurrent pins to run when
// run is called with workers <= 0.
const DefaultWorkers = 8

// Reporter is the small surface the pool calls back into so the caller
// can show progress. `*internal/ui.UI` satisfies it.
type Reporter interface {
	// SetWorkerStatus paints (or clears, with "") the status row for a
	// given worker slot. Slots are stable for the lifetime of a Run call.
	SetWorkerStatus(slot int, status string)
	// UpdateLabel replaces the spinner's top label.
	UpdateLabel(label string)
}

// Run dispatches jobs across up to `workers` goroutines.
//
// Worker slots are not cleared between jobs; the next job overwrites the
// previous status so the spinner never flickers down to a bare header. Slots
// are cleared only when their worker exits.
//
// The first non-nil job error is returned after every worker has finished.
// Callers with non-fatal sentinels should normalize them to nil inside run.
//
// If workers <= 0, DefaultWorkers is used. workers is clamped to len(jobs).
// Returns nil when len(jobs) == 0 without touching r. r, display, and run must
// be non-nil when len(jobs) > 0.
func Run[T any](
	workers int,
	r Reporter,
	label string,
	jobs []T,
	display func(T) string,
	run func(slot int, j T) error,
) error {
	if len(jobs) == 0 {
		return nil
	}
	if workers <= 0 {
		workers = DefaultWorkers
	}
	if workers > len(jobs) {
		workers = len(jobs)
	}

	total := int64(len(jobs))
	// N workers write done; the same N workers read it to render the
	// progress label. atomic.Int64 fits exactly that — a lone counter
	// shared across goroutines, with no other state to coordinate.
	var done atomic.Int64

	// rMu serializes the Reporter calls the pool makes from worker
	// goroutines. *ui.UI's UpdateLabel writes shared spinner state
	// without a lock of its own, so the pool defends its own callers
	// rather than racing the terminal.
	var rMu sync.Mutex
	setStatus := func(slot int, s string) {
		rMu.Lock()
		r.SetWorkerStatus(slot, s)
		rMu.Unlock()
	}
	updateLabel := func() {
		rMu.Lock()
		r.UpdateLabel(fmt.Sprintf("[%d/%d] %s", done.Load(), total, label))
		rMu.Unlock()
	}
	updateLabel()

	ch := make(chan T, len(jobs))
	for _, j := range jobs {
		ch <- j
	}
	close(ch)

	var (
		wg       sync.WaitGroup
		firstErr error
		errMu    sync.Mutex
	)
	for slot := 0; slot < workers; slot++ {
		wg.Add(1)
		go func(slot int) {
			defer wg.Done()
			// Clear the slot once when the goroutine exits (channel
			// drained). The inner loop does not clear between jobs so the
			// slot keeps showing the most recent "→ path" until the next
			// job overwrites it, which avoids the empty-window flicker
			// that made the spinner look frozen during the tail of the
			// run.
			defer setStatus(slot, "")
			for j := range ch {
				setStatus(slot, "→ "+display(j))
				err := run(slot, j)
				done.Add(1)
				updateLabel()
				if err != nil {
					errMu.Lock()
					if firstErr == nil {
						firstErr = err
					}
					errMu.Unlock()
				}
			}
		}(slot)
	}
	wg.Wait()

	return firstErr
}
