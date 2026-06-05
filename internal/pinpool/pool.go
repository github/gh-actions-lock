// Package pinpool is a small generic worker pool with a Reporter hook for
// per-slot UI status. Run dispatches jobs across goroutines and returns the
// first non-nil error after all workers finish.
package pinpool

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// DefaultWorkers is a reasonable number of concurrent pins to run when
// run is called with workers <= 0.
const DefaultWorkers = 8

// DefaultStallThreshold is how long a slot may sit on the same status before
// the watcher tags it with a "(still working…)" hint. Tunable via the
// GH_ACTIONS_PIN_STALL_HINT_MS env var; set to 0 to disable the watcher.
const DefaultStallThreshold = 5 * time.Second

// stallHintText is the dim suffix the watcher appends to a stalled slot. Kept
// generic — without per-slot resolver instrumentation, we can't say which
// network call is hanging, but the suffix still defeats the "frozen spinner"
// perception when a single pin takes 30+ seconds.
const stallHintText = "(still working…)"

// Reporter is the small surface the pool calls back into so the caller
// can show progress. `*internal/ui.UI` satisfies it.
type Reporter interface {
	// SetWorkerStatus paints (or clears, with "") the status row for a
	// given worker slot. Slots are stable for the lifetime of a Run call.
	SetWorkerStatus(slot int, status string)
	// SetWorkerHint sets (or clears, with "") a dim suffix appended after
	// the slot's status text. Used by the stall watcher to flag workers
	// that have been on the same status longer than the stall threshold
	// without disturbing the main status text. A subsequent
	// SetWorkerStatus call is expected to clear any active hint as a side
	// effect so a stale hint can't bleed into the next job.
	SetWorkerHint(slot int, hint string)
	// UpdateLabel replaces the spinner's top label.
	UpdateLabel(label string)
}

// resolveStallThreshold reads GH_ACTIONS_PIN_STALL_HINT_MS. "0" disables; any
// other unparseable value falls back to the default. Parsed once per Run.
func resolveStallThreshold() time.Duration {
	v := os.Getenv("GH_ACTIONS_PIN_STALL_HINT_MS")
	if v == "" {
		return DefaultStallThreshold
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return DefaultStallThreshold
	}
	return time.Duration(n) * time.Millisecond
}

// Run dispatches jobs across up to `workers` goroutines.
//
// The pool observes ctx: if ctx is canceled before all jobs drain, workers
// stop pulling new jobs and Run returns ctx.Err() unless a job error
// already won the race. Jobs in flight are left to finish; cancellation is
// cooperative through the ctx the caller passes to run.
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
	ctx context.Context,
	workers int,
	r Reporter,
	label string,
	jobs []T,
	display func(T) string,
	run func(ctx context.Context, slot int, j T) error,
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

	// stallMu guards the per-slot activity tracking the stall watcher
	// reads. Kept separate from rMu so the watcher can read state
	// without serializing against in-flight reporter calls — the watcher
	// only acquires rMu when it actually fires a SetWorkerHint.
	var stallMu sync.Mutex
	lastUpdate := make([]time.Time, workers)
	active := make([]bool, workers)
	hinted := make([]bool, workers)
	now := time.Now()
	for i := range lastUpdate {
		lastUpdate[i] = now
	}

	setStatus := func(slot int, s string) {
		stallMu.Lock()
		lastUpdate[slot] = time.Now()
		active[slot] = s != ""
		wasHinted := hinted[slot]
		hinted[slot] = false
		stallMu.Unlock()

		rMu.Lock()
		r.SetWorkerStatus(slot, s)
		if wasHinted {
			// SetWorkerStatus implicitly clears the hint, but be explicit
			// for Reporter implementations that don't tie the two together.
			r.SetWorkerHint(slot, "")
		}
		rMu.Unlock()
	}
	updateLabel := func() {
		rMu.Lock()
		r.UpdateLabel(fmt.Sprintf("[%d/%d] %s", done.Load(), total, label))
		rMu.Unlock()
	}
	updateLabel()

	threshold := resolveStallThreshold()
	stop := make(chan struct{})
	stopped := make(chan struct{})
	if threshold > 0 {
		// Tick at threshold/4 (clamped) so a stalled slot is flagged
		// within ~1.25× the threshold worst case. The watcher checks
		// `stop` before every reporter call and exits cleanly when Run
		// closes the channel.
		tick := threshold / 4
		if tick < 25*time.Millisecond {
			tick = 25 * time.Millisecond
		}
		go stallWatcher(stop, stopped, tick, threshold, workers, &stallMu, lastUpdate, active, hinted, &rMu, r)
	} else {
		close(stopped)
	}

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
			var lastDisplay string
			for j := range ch {
				// Cooperative cancellation: stop pulling new jobs once
				// ctx is canceled. Jobs already in flight finish on
				// their own ctx-awareness inside run.
				if ctx.Err() != nil {
					return
				}
				// Flash a "✓ <prev>" between jobs so the slot visibly
				// transitions from in-flight → complete → next-job
				// instead of jumping straight to the next "→". Without
				// this, finished work is invisible: the user sees the
				// next workflow appear in-place and never sees the
				// previous one acknowledged.
				if lastDisplay != "" {
					setStatus(slot, "✓ "+lastDisplay)
				}
				lastDisplay = display(j)
				setStatus(slot, "→ "+lastDisplay)
				err := run(ctx, slot, j)
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
			// Final acknowledgement before clearing — visible briefly
			// during the wg.Wait() before the spinner area is erased.
			if lastDisplay != "" {
				setStatus(slot, "✓ "+lastDisplay)
			}
		}(slot)
	}
	wg.Wait()
	// Stop the watcher before returning so it can't race a caller that
	// resets the UI immediately after Run.
	close(stop)
	<-stopped

	if firstErr != nil {
		return firstErr
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return nil
}

// stallWatcher periodically scans per-slot activity and tags any slot that
// has been on the same status for at least `threshold` with the stall hint.
// It clears its own previously-set hint as soon as the slot updates again or
// goes idle — setStatus also clears the hint via the Reporter, so the
// watcher's view stays in sync without coordination.
func stallWatcher(
	stop <-chan struct{},
	stopped chan<- struct{},
	tick, threshold time.Duration,
	slots int,
	stallMu *sync.Mutex,
	lastUpdate []time.Time,
	active, hinted []bool,
	rMu *sync.Mutex,
	r Reporter,
) {
	defer close(stopped)
	t := time.NewTicker(tick)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
		}
		// Re-check stop after tick wakeup so we don't fire a hint after
		// Run has logically completed.
		select {
		case <-stop:
			return
		default:
		}
		now := time.Now()
		// Snapshot slot transitions that need a reporter call so we
		// don't hold stallMu across rMu acquisition.
		type op struct {
			slot int
			set  bool
		}
		var ops []op
		stallMu.Lock()
		for s := 0; s < slots; s++ {
			if active[s] && !hinted[s] && now.Sub(lastUpdate[s]) >= threshold {
				hinted[s] = true
				ops = append(ops, op{slot: s, set: true})
			}
		}
		stallMu.Unlock()
		if len(ops) == 0 {
			continue
		}
		// Check stop one more time before mutating UI state.
		select {
		case <-stop:
			return
		default:
		}
		rMu.Lock()
		for _, o := range ops {
			if o.set {
				r.SetWorkerHint(o.slot, stallHintText)
			} else {
				r.SetWorkerHint(o.slot, "")
			}
		}
		rMu.Unlock()
	}
}
