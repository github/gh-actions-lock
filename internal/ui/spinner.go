package ui

import (
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/briandowns/spinner"
	"github.com/muesli/termenv"
)

// spinnerWriter wraps the terminal writer used by the spinner. It intercepts
// each write from the spinner goroutine (every write starts with '\r') and,
// when worker status lines are set, appends them as dim lines below the spinner
// after the spinner has written its own content. Worker text is kept entirely
// out of the spinner Suffix so the library's byte-count wrap detection never
// sees the extra lines — eliminating the runaway multi-line erase bug.
type spinnerWriter struct {
	mu        sync.Mutex
	w         io.Writer
	workers   []string // per-slot status; empty string = idle
	hints     []string // per-slot dim suffix appended after the status
	nRendered int      // number of worker lines written in the last tick
	noColor   bool
	output    *termenv.Output
	// prefix is the static label text written before the spinner glyph
	// (e.g. "Resolving actions "). Stored here so startAnimator can write
	// an immediate frame on resume, avoiding the one-tick blank gap that
	// occurs because briandowns never fires a tick immediately on Start().
	prefix string
	// stop closes to signal the independent worker-redraw ticker to exit.
	// done is closed once the ticker goroutine has returned. The ticker
	// keeps worker glyphs animating even when the spinner library coalesces,
	// throttles, or briefly stalls its own writes (e.g. under network
	// contention) — without the ticker, animation freezes whenever Write
	// isn't called.
	stop chan struct{}
	done chan struct{}

	// deferredWrites buffers setWorkerStatus calls that happen while
	// printLine has snapshotted-and-cleared the workers slice for an
	// inline message print. When non-nil, setWorkerStatus writes into
	// this map instead of workers; printLine merges the buffered writes
	// back onto its snapshot on restore so concurrent clears/updates
	// from the pin pool aren't clobbered by the restore. Nil during
	// normal operation.
	deferredWrites map[int]string
	// deferredHints mirrors deferredWrites for hint state so concurrent
	// stall-watcher updates aren't clobbered by printLine's restore.
	deferredHints map[int]string
}

// workerSpinFrames is the rotating glyph shown next to each ACTIVE worker row
// (rows starting with "→") so subtasks visibly pulse instead of looking
// frozen. Matches the main spinner's braille charset (CharSets[11]) so the
// motion stays cohesive.
var workerSpinFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

// workerFrameInterval is how often (wall-clock) each worker glyph advances one
// step. Matches the main spinner's 120ms tick so motion feels cohesive.
const workerFrameInterval = 120 * time.Millisecond

func (sw *spinnerWriter) Write(p []byte) (n int, err error) {
	sw.mu.Lock()
	defer sw.mu.Unlock()

	if len(p) == 0 || p[0] != '\r' {
		n, err = sw.w.Write(p)
		return
	}

	// briandowns calls Write twice per tick:
	//   1. erase:  \r\033[K          — wipe the previous frame
	//   2. frame:  \r{Prefix}{glyph}{Suffix} — paint the new frame
	//
	// Only render worker rows on the frame write. Rendering them on the
	// erase write too means two cursor-down/up sequences per 120ms tick,
	// which is the primary cause of residual flicker on embedded terminals.
	//
	// Match the specific erase sequence the library emits rather than any
	// generic CSI prefix — frame writes that start with a color SGR
	// (e.g. \r\033[36m…) must not be misclassified as erases.
	// briandowns.erase() always writes "\r\033[K" (4 bytes) for single-line
	// spinners, optionally followed by "\033[F\033[K" per additional line.
	isErase := len(p) >= 4 && p[0] == '\r' && p[1] == '\033' && p[2] == '[' && p[3] == 'K'
	if isErase {
		// Just pass the erase through; worker rows are still on screen
		// from the previous frame and will be refreshed momentarily.
		n, err = sw.w.Write(p)
		return
	}

	// Combine the spinner frame and worker rows into a single
	// synchronized write so the terminal never shows a partial frame.
	var buf strings.Builder
	buf.WriteString("\033[?2026h") // begin synchronized output
	// Replace the leading \r with \r\033[2K (go-to-col-0 + erase line)
	// so that when the label shrinks between ticks the old longer text
	// is fully cleared instead of leaving leftover characters visible.
	buf.WriteString("\r\033[2K")
	buf.Write(p[1:]) // p[0] is the \r we already emitted above
	sw.buildWorkerFrameLocked(&buf)
	buf.WriteString("\033[?2026l") // end synchronized output
	_, err = io.WriteString(sw.w, buf.String())
	n = len(p)
	return
}

// buildWorkerFrameLocked appends the escape sequences for worker rows into
// buf, leaving the cursor back on the spinner line. Caller must hold sw.mu
// and the cursor must currently be on the spinner line.
func (sw *spinnerWriter) buildWorkerFrameLocked(buf *strings.Builder) {
	step := int(time.Now().UnixNano() / int64(workerFrameInterval))
	// Per-slot phase offset so rows visibly cascade instead of all hitting
	// the same frame in lockstep — that lockstep was what made them look
	// frozen even when they weren't.
	width := termWidthOf(sw.w)
	var lines []string
	for slot, w := range sw.workers {
		if w == "" {
			continue
		}
		body := w
		if len(w) >= len("→ ") && w[:len("→ ")] == "→ " {
			frame := workerSpinFrames[(step+slot)%len(workerSpinFrames)]
			body = w[len("→ "):] + " " + frame
		}
		hint := ""
		if slot < len(sw.hints) {
			hint = sw.hints[slot]
		}
		// Combined width budget: "  " indent + body + " " + hint must
		// fit one terminal row. Hint loses first if there isn't room.
		if width > 4 {
			budget := width - 2 // "  " indent
			if len(body)+1+len(hint) > budget {
				if len(body) >= budget {
					body = truncateBytes(body, budget)
					hint = ""
				} else if hint != "" {
					hintBudget := budget - len(body) - 1
					if hintBudget < 4 {
						hint = ""
					} else {
						hint = truncateBytes(hint, hintBudget)
					}
				}
			}
		}
		var line string
		if !sw.noColor {
			line = sw.output.String("  " + body).Faint().String()
			if hint != "" {
				line += " " + sw.output.String(hint).Faint().String()
			}
		} else {
			line = "  " + body
			if hint != "" {
				line += " " + hint
			}
		}
		lines = append(lines, line)
	}
	nLines := len(lines)
	for _, line := range lines {
		// Use \n (newline) rather than \033[1B (cursor-down) so the
		// buffer scrolls when we're at the bottom of the viewport. ESC[1B
		// is a no-op at the last row and the subsequent ESC[NA cursor-up
		// would then overshoot, landing on (and clobbering) lines above
		// the spinner — including the user's typed command line.
		fmt.Fprintf(buf, "\n\r\033[2K%s", line)
	}
	// Erase stale lines left over from a previous render that had more
	// active workers. Without this, ghost "→ dep" rows from finished
	// workers persist below the current set.
	for i := nLines; i < sw.nRendered; i++ {
		buf.WriteString("\n\r\033[2K")
	}
	totalDown := nLines
	if sw.nRendered > nLines {
		totalDown = sw.nRendered
	}
	if totalDown > 0 {
		fmt.Fprintf(buf, "\033[%dA\r", totalDown)
	}
	sw.nRendered = nLines
}

// renderWorkersLocked redraws the worker rows below the spinner line as a
// single synchronized write. Caller must hold sw.mu.
func (sw *spinnerWriter) renderWorkersLocked() {
	var buf strings.Builder
	buf.WriteString("\033[?2026h") // begin synchronized output
	sw.buildWorkerFrameLocked(&buf)
	buf.WriteString("\033[?2026l") // end synchronized output
	io.WriteString(sw.w, buf.String())
}

// startAnimator launches a goroutine that periodically redraws the worker
// rows so their glyphs keep pulsing even when the spinner library's own
// writes stall or coalesce. Caller MUST NOT hold sw.mu.
func (sw *spinnerWriter) startAnimator() {
	sw.mu.Lock()
	if sw.stop != nil {
		sw.mu.Unlock()
		return
	}
	sw.stop = make(chan struct{})
	sw.done = make(chan struct{})
	stop := sw.stop
	done := sw.done
	sw.mu.Unlock()

	sw.mu.Lock()
	// Wrap cursor-hide in a synchronized block so it can't interleave
	// with a concurrent spinner frame write mid-output.
	io.WriteString(sw.w, "\033[?2026h\033[?25l\033[?2026l")
	sw.mu.Unlock()

	// Write a synthetic first frame immediately so the spinner line is never
	// blank during the ~120ms gap before the library's first ticker tick.
	// briandowns never writes a frame on Start() — it always waits for the
	// first tick — so without this, every Resume causes a visible blank line.
	sw.mu.Lock()
	var buf strings.Builder
	buf.WriteString("\033[?2026h")
	buf.WriteString("\r\033[2K")
	buf.WriteString(sw.prefix)
	buf.WriteString(workerSpinFrames[0]) // placeholder glyph from shared braille charset; real tick replaces it
	sw.buildWorkerFrameLocked(&buf)
	buf.WriteString("\033[?2026l")
	io.WriteString(sw.w, buf.String())
	sw.mu.Unlock()

	go func() {
		defer close(done)
		t := time.NewTicker(workerFrameInterval)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				sw.mu.Lock()
				hasActive := false
				for _, w := range sw.workers {
					if len(w) >= len("→ ") && w[:len("→ ")] == "→ " {
						hasActive = true
						break
					}
				}
				if hasActive {
					sw.renderWorkersLocked()
				}
				sw.mu.Unlock()
			}
		}
	}()
}

// stopAnimator signals the redraw ticker to exit and waits for it. Caller
// MUST NOT hold sw.mu.
func (sw *spinnerWriter) stopAnimator() {
	sw.mu.Lock()
	stop := sw.stop
	done := sw.done
	sw.stop = nil
	sw.done = nil
	sw.mu.Unlock()
	if stop == nil {
		return
	}
	close(stop)
	<-done
	// Restore the cursor that startAnimator hid, synchronized so it can't
	// interleave with any write still draining from the ticker goroutine.
	sw.mu.Lock()
	io.WriteString(sw.w, "\033[?2026h\033[?25h\033[?2026l")
	sw.mu.Unlock()
}

// setDetail is a backward-compat shim that sets a single worker slot (slot 0).
func (sw *spinnerWriter) setDetail(det string) {
	sw.setWorkerStatus(0, det)
}

// setWorkerStatus sets or clears one worker's status slot. While printLine
// has the workers slice snapshotted (deferredWrites != nil) the write is
// buffered into the deferred map so printLine's restore phase can merge it
// onto the snapshot — preventing the restore from clobbering clears and
// updates issued by the pin pool during the print window. Setting a slot to
// any value also clears that slot's hint so a stale "(still working…)"
// suffix from a previous job can't bleed into the next one.
func (sw *spinnerWriter) setWorkerStatus(slot int, status string) {
	sw.mu.Lock()
	if sw.deferredWrites != nil {
		sw.deferredWrites[slot] = status
		if sw.deferredHints != nil {
			sw.deferredHints[slot] = ""
		}
		sw.mu.Unlock()
		return
	}
	for len(sw.workers) <= slot {
		sw.workers = append(sw.workers, "")
	}
	sw.workers[slot] = status
	if slot < len(sw.hints) {
		sw.hints[slot] = ""
	}
	sw.mu.Unlock()
}

// setWorkerHint sets or clears one worker's dim suffix without touching the
// status text. Defers like setWorkerStatus while printLine has the slices
// snapshotted.
func (sw *spinnerWriter) setWorkerHint(slot int, hint string) {
	sw.mu.Lock()
	defer sw.mu.Unlock()
	if sw.deferredHints != nil {
		sw.deferredHints[slot] = hint
		return
	}
	for len(sw.hints) <= slot {
		sw.hints = append(sw.hints, "")
	}
	sw.hints[slot] = hint
}

// clearSpinnerLines erases the spinner line and any worker lines that were
// rendered below it on the last tick. Uses \033[J (erase to end of screen)
// rather than the per-line cursor-down/clear/cursor-up dance the previous
// implementation used: ESC[1B doesn't scroll the buffer at the bottom of the
// viewport, so the followup ESC[NA could overshoot into already-rendered
// rows above and clobber them on the next spinner tick.
func (u *UI) clearSpinnerLines() {
	var lines int
	if u.spinWriter != nil {
		u.spinWriter.mu.Lock()
		lines = u.spinWriter.nRendered
		u.spinWriter.nRendered = 0
		u.spinWriter.mu.Unlock()
	}
	// Buffer the entire clear into a single write so the terminal
	// never shows a partially erased frame.
	var buf strings.Builder
	buf.WriteString("\r\033[2K")
	for i := 0; i < lines; i++ {
		buf.WriteString("\n\033[2K")
	}
	if lines > 0 {
		fmt.Fprintf(&buf, "\033[%dA\r", lines)
	}
	fmt.Fprint(u.w, buf.String())
	u.progHasDetail = false
}

// printLine writes one line of output, transparently pausing an active
// spinner so its animation frame doesn't interleave with (and corrupt) the
// text. The spinner resumes after the write. When no spinner is active it
// just runs write.
func (u *UI) printLine(write func()) {
	if u.spinner != nil && u.spinner.Active() {
		// Snapshot and zero all worker slots so the stop-write doesn't render
		// extra lines that we'd then fail to clear. Buffer any concurrent
		// setWorkerStatus calls into deferredWrites so the restore merges
		// them instead of clobbering — without the buffer, pin-pool workers
		// that clear or repaint their slot during the write window lose
		// those updates, which is what leaves stale "→ path" rows visible
		// after their owner worker has exited.
		var savedWorkers []string
		var savedHints []string
		if u.spinWriter != nil {
			u.spinWriter.mu.Lock()
			savedWorkers = make([]string, len(u.spinWriter.workers))
			copy(savedWorkers, u.spinWriter.workers)
			savedHints = make([]string, len(u.spinWriter.hints))
			copy(savedHints, u.spinWriter.hints)
			u.spinWriter.workers = nil
			u.spinWriter.hints = nil
			u.spinWriter.deferredWrites = map[int]string{}
			u.spinWriter.deferredHints = map[int]string{}
			u.spinWriter.mu.Unlock()
		}
		u.spinner.Stop()
		u.clearSpinnerLines()
		write()
		if u.spinWriter != nil {
			u.spinWriter.mu.Lock()
			for slot, status := range u.spinWriter.deferredWrites {
				for len(savedWorkers) <= slot {
					savedWorkers = append(savedWorkers, "")
				}
				savedWorkers[slot] = status
				// A status update implicitly clears the hint, mirroring
				// setWorkerStatus's normal-path semantics.
				for len(savedHints) <= slot {
					savedHints = append(savedHints, "")
				}
				savedHints[slot] = ""
			}
			for slot, hint := range u.spinWriter.deferredHints {
				for len(savedHints) <= slot {
					savedHints = append(savedHints, "")
				}
				// Don't overwrite a hint clear caused by a same-window
				// status update: if deferredWrites also touched this slot,
				// the status reset already cleared the hint above.
				if _, statusUpdated := u.spinWriter.deferredWrites[slot]; statusUpdated {
					continue
				}
				savedHints[slot] = hint
			}
			u.spinWriter.workers = savedWorkers
			u.spinWriter.hints = savedHints
			u.spinWriter.deferredWrites = nil
			u.spinWriter.deferredHints = nil
			u.spinWriter.mu.Unlock()
		}
		u.spinner.Start()
		return
	}
	write()
}

// ProgressActive reports whether a spinner is currently running. Callers use
// this to adopt an already-running spinner (keeping it continuous across
// phases) instead of stopping and restarting one, which would leave a visible
// gap on the terminal.
func (u *UI) ProgressActive() bool {
	return u.spinner != nil
}

// progressGrace is how long StartProgress waits before showing the spinner.
// Runs that complete within this window never flicker a spinner at all.
const progressGrace = 500 * time.Millisecond

// StartProgress starts an animated spinner with the given label on stderr.
// On non-TTY outputs, prints a static label instead. Matches gh CLI's Primer
// progress indicator: braille dots, 120ms, cyan.
//
// The spinner is not rendered immediately: a short grace period suppresses
// flicker for fast runs. If StopProgress is called before the grace period
// expires, no spinner is ever shown.
func (u *UI) StartProgress(label string) {
	if u.headless {
		if label != "" {
			u.headlessEmit(label)
			u.headlessLabelStem = labelStem(label)
		}
		return
	}
	sw := &spinnerWriter{
		w:       u.w,
		noColor: u.noColor,
		output:  u.output,
	}
	u.spinWriter = sw
	opts := []spinner.Option{spinner.WithWriter(sw)}
	if !u.noColor {
		opts = append(opts, spinner.WithColor("fgCyan"))
	}
	sp := spinner.New(spinner.CharSets[11], 120*time.Millisecond, opts...)
	// The label is static for the lifetime of this spinner. Set Prefix
	// before sp.Start() — the goroutine isn't running yet so no lock needed.
	if label != "" {
		sp.Prefix = label + " "
		sw.prefix = label + " "
	}
	u.spinner = sp
	u.progDetail = ""
	u.progPaused = false

	// Defer the visible start so fast runs never flicker.
	done := make(chan struct{})
	u.progGraceDone = done
	u.progGrace = time.AfterFunc(progressGrace, func() {
		defer close(done)
		sp.Start()
		sw.startAnimator()
	})
}

// PauseProgress temporarily halts the spinner and clears its line so other
// output (typically an interactive prompt) can render cleanly. The label and
// detail are retained; ResumeProgress restarts the spinner where it left off.
// Safe to call when no spinner is active or one is already paused.
func (u *UI) PauseProgress() {
	if u.spinner == nil || u.progPaused {
		return
	}
	// Cancel grace timer — if the spinner hasn't appeared yet, keep it hidden.
	if u.progGrace != nil {
		if !u.progGrace.Stop() {
			<-u.progGraceDone
		}
		u.progGrace = nil
		u.progGraceDone = nil
	}
	if u.spinWriter != nil {
		u.spinWriter.stopAnimator()
	}
	// Clear worker lines before stopping the spinner (see StopProgress).
	if u.isTTY {
		u.clearSpinnerLines()
	}
	if u.spinWriter != nil {
		u.spinWriter.mu.Lock()
		u.spinWriter.workers = nil
		u.spinWriter.hints = nil
		u.spinWriter.mu.Unlock()
	}
	u.spinner.Stop()
	u.progPaused = true
}

// ResumeProgress restarts a spinner previously paused by PauseProgress,
// redrawing the retained label/detail. Safe to call when no spinner is active
// or one is not paused.
func (u *UI) ResumeProgress() {
	if u.spinner == nil || !u.progPaused {
		return
	}
	u.progPaused = false
	u.renderProgress()
	u.spinner.Start()
	if u.spinWriter != nil {
		u.spinWriter.startAnimator()
	}
}

// StopProgress stops the spinner. Safe to call if no spinner is active.
func (u *UI) StopProgress() {
	if u.spinner != nil {
		// Cancel the grace timer. If the timer already fired (Stop returns
		// false) the spinner is running — wait for the goroutine to finish
		// before tearing down so we don't race with Start().
		if u.progGrace != nil {
			if !u.progGrace.Stop() {
				// Timer already fired — spinner is starting or started.
				<-u.progGraceDone
			}
			u.progGrace = nil
			u.progGraceDone = nil
		}
		if u.spinWriter != nil {
			u.spinWriter.stopAnimator()
		}
		// Erase worker lines BEFORE stopping the spinner — at this point
		// the cursor is on the spinner line, so the down/up dance to
		// clear worker rows below is safe. Doing this after Stop() races
		// with the shell prompt redraw and clobbers it.
		u.clearSpinnerLines()
		if u.spinWriter != nil {
			u.spinWriter.mu.Lock()
			u.spinWriter.workers = nil
			u.spinWriter.hints = nil
			u.spinWriter.mu.Unlock()
		}
		u.spinner.Stop()
		u.spinner = nil
		u.spinWriter = nil
		u.progDetail = ""
		u.progPaused = false
	}
}
