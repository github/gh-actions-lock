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

// Pool holds shared configuration for dispatching work across goroutines.
// Create one in the pipeline entry point and reuse it across phases.
type Pool struct {
	Workers  int
	Reporter Reporter
}

// noopReporter is a silent Reporter used when the caller passes nil.
type noopReporter struct{}

func (noopReporter) SetWorkerStatus(int, string) {}
func (noopReporter) SetWorkerHint(int, string)   {}
func (noopReporter) UpdateLabel(string)          {}

// New returns a Pool with the given worker count and reporter. If workers
// is <= 0, DefaultWorkers is used. A nil reporter is replaced with a
// silent no-op.
func New(workers int, r Reporter) *Pool {
	if workers <= 0 {
		workers = DefaultWorkers
	}
	if r == nil {
		r = noopReporter{}
	}
	return &Pool{Workers: workers, Reporter: r}
}

// Run dispatches jobs across up to p.Workers goroutines.
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
// Workers are clamped to len(jobs). Returns nil when len(jobs) == 0 without
// touching the reporter. display and run must be non-nil when len(jobs) > 0.
//
// An empty label suppresses the pool's own "[done/total] label" writes,
// leaving the spinner label to the caller; per-worker status rows and stall
// hints are unaffected.
func (p *Pool) Run(
	ctx context.Context,
	label string,
	jobs []any,
	display func(any) string,
	run func(ctx context.Context, slot int, j any) error,
) error {
	if len(jobs) == 0 {
		return nil
	}
	workers := p.Workers
	if workers > len(jobs) {
		workers = len(jobs)
	}
	r := p.Reporter

	total := int64(len(jobs))
	var done atomic.Int64

	// rMu serializes Reporter calls from worker goroutines.
	var rMu sync.Mutex

	// stallMu guards per-slot activity tracking. Separate from rMu so
	// the watcher reads state without serializing against reporter calls.
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
			r.SetWorkerHint(slot, "")
		}
		rMu.Unlock()
	}
	updateLabel := func() {
		// An empty label hands label ownership to the caller: the pool drives
		// per-worker status rows and stall hints but writes no "[done/total]"
		// prefix. Used when an outer per-item progress counter (e.g. resolve's
		// ref-denominated bar) owns the spinner label and a chunk-denominated
		// pool counter would fight it.
		if label == "" {
			return
		}
		rMu.Lock()
		r.UpdateLabel(fmt.Sprintf("[%d/%d] %s", done.Load(), total, label))
		rMu.Unlock()
	}
	updateLabel()

	threshold := resolveStallThreshold()
	stop := make(chan struct{})
	stopped := make(chan struct{})
	if threshold > 0 {
		tick := threshold / 4
		if tick < 25*time.Millisecond {
			tick = 25 * time.Millisecond
		}
		go stallWatcher(stop, stopped, tick, threshold, workers, &stallMu, lastUpdate, active, hinted, &rMu, r)
	} else {
		close(stopped)
	}

	ch := make(chan any, len(jobs))
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
			defer setStatus(slot, "")
			var lastDisplay string
			for j := range ch {
				if ctx.Err() != nil {
					return
				}
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
			if lastDisplay != "" {
				setStatus(slot, "✓ "+lastDisplay)
			}
		}(slot)
	}
	wg.Wait()
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

// RunTyped dispatches typed jobs through the pool. This is a generic helper
// that wraps p.Run, boxing each job to any and unboxing in the display and
// run callbacks.
func RunTyped[T any](
	p *Pool,
	ctx context.Context,
	label string,
	jobs []T,
	display func(T) string,
	run func(ctx context.Context, slot int, j T) error,
) error {
	boxed := make([]any, len(jobs))
	for i, j := range jobs {
		boxed[i] = j
	}
	return p.Run(ctx, label, boxed,
		func(j any) string { return display(j.(T)) },
		func(ctx context.Context, slot int, j any) error { return run(ctx, slot, j.(T)) },
	)
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
