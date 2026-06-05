// Package pinpool runs the parallel pin pass for `gh actions pin doctor`.
//
// The doctor often has dozens of workflow files to pin in one run, and
// each pin involves several network round-trips. Doing them one at a
// time is slow, so pinpool spreads the work across a handful of
// goroutines (8 by default) that pull from a shared queue.
//
// Run takes the list of workflows, hands each one to the first free
// goroutine, and waits for them all to finish before returning. If a
// pin fails, the remaining workflows still get a chance to pin — Run
// only returns the first failure once every workflow has been tried.
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

// Run dispatches jobs across up to `workers` goroutines. For each job,
// the assigned worker:
//
//   - calls r.SetWorkerStatus(slot, "→ "+display(j)) before invoking run
//   - calls run(slot, j)
//   - increments a shared done counter and calls
//     r.UpdateLabel(fmt.Sprintf("[%d/%d] %s", done, total, label))
//
// The slot is NOT cleared between jobs — the next iteration's "→ next"
// status overwrites the previous one. This keeps a current target
// visible at all times during steady-state operation, so the spinner
// never flickers down to a bare header during the millisecond gap
// between completing one job and pulling the next. Slots are cleared
// once, with r.SetWorkerStatus(slot, ""), only when the worker exits
// because the job channel has drained.
//
// run reports an error per job. The first non-nil error is returned
// after every worker has finished, so a single failure does not strand
// siblings. Callers that have a "non-fatal" sentinel (e.g. a
// security-gate skip) should normalize it to nil inside run.
//
// If workers <= 0, DefaultWorkers is used. workers is clamped to
// len(jobs). Returns nil when len(jobs) == 0 without touching r.
//
// r, display, and run must be non-nil when len(jobs) > 0.
//
// This is a simplified approach that requires all jobs before execution.
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
