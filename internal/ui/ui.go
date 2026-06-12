// Package ui provides terminal-aware output formatting for gh-actions-pin.
// It respects NO_COLOR, CLICOLOR, and TTY detection on stderr (not stdout)
// so that color works correctly even when stdout is piped (e.g. --json mode).
package ui

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/briandowns/spinner"
	"github.com/muesli/termenv"
	"golang.org/x/term"
)

// Icons used in status-prefixed output. These always appear regardless of
// color setting — PRimer says don't rely solely on color.
const (
	IconSuccess = "✓"
	IconError   = "✗"
	IconWarning = "!"
	IconSkip    = "-"
)

// UI writes human-readable output to an io.Writer with optional ANSI styling.
//
// When a log sink is attached via SetLog, all narration (Success, Detail,
// Warning, …) is redirected as plain text to the log and the terminal is left
// to spinners, prompts, and the Term* summary methods. This keeps the
// interactive output clean and avoids spinner/scrollback interleaving.
//
// Headless mode (set when stderr isn't a TTY or CI=true) takes precedence
// over the log sink: every narration call writes one plain `text\n` line to
// the writer with no icons, no indentation, and no color. Progress methods
// emit a single line per phase boundary (label stem change) and otherwise
// no-op — no spinner, no [N/M] churn.
type UI struct {
	w        io.Writer
	logw     io.Writer
	output   *termenv.Output
	noColor  bool
	isTTY    bool
	headless bool
	spinner  *spinner.Spinner

	// headlessLabelStem deduplicates repeated UpdateLabel calls in headless
	// mode so the same phase label isn't printed more than once.
	headlessLabelStem string

	// progLabel and progDetail hold the two halves of the active spinner line
	// (the per-workflow label and the resolver's current-action detail). They
	// are recombined and truncated to one terminal row on every update so the
	// spinner never wraps — a wrapped spinner breaks the library's
	// backspace-based erase and causes line jumping/leftover fragments.
	progLabel  string
	progDetail string

	// progLast holds the most recent non-empty rendered line. If an update
	// transiently leaves both label and detail empty (e.g. detail cleared
	// between phases before the next label is set), we keep showing progLast
	// so the spinner never flashes a bare, label-less glyph.
	progLast string

	// progPaused is set while the spinner is temporarily halted (e.g. to let an
	// interactive prompt own the terminal). The spinner object is retained so
	// ResumeProgress can restart it with the same label/detail.
	progPaused bool

	// progHasDetail tracks whether the last renderProgress call rendered a
	// second detail line. clearSpinnerLines uses this to know whether to also
	// erase line 2 after stopping the spinner.
	progHasDetail bool

	// spinWriter is a thin io.Writer wrapper set while a spinner is active.
	// It intercepts each spinner tick write (which starts with '\r') and
	// appends the detail line below the spinner WITHOUT putting the detail text
	// in the spinner Suffix — keeping the suffix short so the library's
	// byte-count wrap detection never triggers on the second line's content.
	spinWriter *spinnerWriter

	// progGrace delays spinner visibility so fast runs never flicker. When
	// non-nil, the spinner object exists but hasn't been Start()ed yet;
	// a background goroutine will start it after the grace period unless
	// StopProgress cancels first.
	progGrace     *time.Timer
	progGraceDone chan struct{} // closed when the grace goroutine exits
}

// SetLog attaches a narration sink. Once set, narration methods write plain
// text to w instead of the terminal. Pass nil to detach.
func (u *UI) SetLog(w io.Writer) {
	u.logw = w
}

// MarkHeadless forces the UI into plain-text streaming mode after
// construction. Used when a flag like --no-interactive signals headless
// intent that the auto-detection (TTY + CI env) at construction time
// couldn't see. Idempotent and one-way: once headless, the UI stays
// headless for the rest of its lifetime. Also disables color so any
// already-cached output profile is consistent with the new mode.
func (u *UI) MarkHeadless() {
	if u.headless {
		return
	}
	u.headless = true
	u.noColor = true
	u.output = termenv.NewOutput(u.w, termenv.WithProfile(termenv.Ascii))
}

// progressTrace caches the result of GH_ACTIONS_PIN_DEBUG_PROGRESS once. When
// set, every UpdateLabel and SetWorkerStatus call writes a timestamped JSONL
// line to a dedicated trace file so we can audit phase transitions and verify
// the worker pool is actually fanning out without depending on visual
// inspection of the spinner. The path is resolved from the env var: "1" or
// "true" maps to $TMPDIR/gh-actions-pin-progress.log, anything else is treated
// as a literal path.
var (
	progressTraceMu   sync.Mutex
	progressTraceFile *os.File
	progressTracePath = resolveProgressTracePath()
)

func resolveProgressTracePath() string {
	v := os.Getenv("GH_ACTIONS_PIN_DEBUG_PROGRESS")
	switch v {
	case "":
		return ""
	case "1", "true", "yes":
		dir := os.TempDir()
		return dir + "/gh-actions-pin-progress.log"
	default:
		return v
	}
}

// CloseProgressTrace closes the progress trace file if one was opened. It is
// safe to call unconditionally; it no-ops when tracing is off or already closed.
func CloseProgressTrace() {
	progressTraceMu.Lock()
	defer progressTraceMu.Unlock()
	if progressTraceFile != nil {
		progressTraceFile.Close()
		progressTraceFile = nil
	}
}

// traceProgress emits a structured trace event to the progress trace file when
// progress tracing is enabled. kind is "label" or "slot[N]"; payload is the new
// value. No-op when tracing is off.
func (u *UI) traceProgress(kind, payload string) {
	if progressTracePath == "" {
		return
	}
	progressTraceMu.Lock()
	defer progressTraceMu.Unlock()
	if progressTraceFile == nil {
		f, err := os.OpenFile(progressTracePath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			return
		}
		progressTraceFile = f
		fmt.Fprintf(f, "# gh-actions-pin progress trace %s\n", time.Now().Format(time.RFC3339))
	}
	rec := struct {
		Time    string `json:"time"`
		Kind    string `json:"kind"`
		Payload string `json:"payload"`
	}{
		Time:    time.Now().Format(time.RFC3339Nano),
		Kind:    kind,
		Payload: payload,
	}
	b, err := json.Marshal(rec)
	if err != nil {
		return
	}
	fmt.Fprintln(progressTraceFile, string(b))
}

// logging reports whether a narration log sink is attached.
func (u *UI) logging() bool {
	return u.logw != nil
}

// emit writes a fully-formatted narration line. With a log sink attached the
// line goes there (already plain, since color helpers no-op while logging);
// otherwise it goes to the terminal, pausing any active spinner.
func (u *UI) emit(line string) {
	if u.logw != nil {
		fmt.Fprint(u.logw, line)
		return
	}
	u.printLine(func() { fmt.Fprint(u.w, line) })
}

// logTagged writes one structured JSON record to the log sink (JSONL: one
// object per line). level classifies the line (success, error, warning, skip,
// info, detail, hint, header); an empty level falls back to "detail" so
// continuation lines stay machine-readable. The message is plain text — color
// and hyperlink helpers no-op while a log sink is attached.
func (u *UI) logTagged(level, text string) {
	if u.logw == nil {
		return
	}
	if level == "" {
		level = "detail"
	}
	rec := struct {
		Time  string `json:"time"`
		Level string `json:"level"`
		Msg   string `json:"msg"`
	}{
		Time:  time.Now().Format(time.RFC3339Nano),
		Level: level,
		Msg:   text,
	}
	b, err := json.Marshal(rec)
	if err != nil {
		return
	}
	fmt.Fprintf(u.logw, "%s\n", b)
}

// paint applies an ANSI foreground color regardless of the log sink. Used by
// the Term* summary methods, which always target the terminal.
func (u *UI) paint(colorCode, s string) string {
	if u.noColor {
		return s
	}
	return u.output.String(s).Foreground(u.output.Color(colorCode)).String()
}

// New creates a UI that writes to stderr with color auto-detected from the
// stderr file descriptor. Respects NO_COLOR and CLICOLOR environment variables.
func New() *UI {
	isTTY := term.IsTerminal(int(os.Stderr.Fd()))
	noColor := isColorDisabled()
	headless := isHeadless(isTTY)
	colorEnabled := isTTY && !noColor && !headless

	profile := termenv.Ascii
	if colorEnabled {
		profile = termenv.ColorProfile()
	}

	return &UI{
		w:        os.Stderr,
		output:   termenv.NewOutput(os.Stderr, termenv.WithProfile(profile)),
		noColor:  !colorEnabled,
		isTTY:    isTTY,
		headless: headless,
	}
}

// NewWithWriter creates a UI that writes to the given writer. If the writer
// is an *os.File whose fd is a terminal, color is auto-detected. Otherwise
// color is disabled. This is the injection-friendly constructor for DI.
func NewWithWriter(w io.Writer) *UI {
	noColor := isColorDisabled()
	isTTY := false
	if f, ok := w.(*os.File); ok {
		isTTY = term.IsTerminal(int(f.Fd()))
	}
	headless := isHeadless(isTTY)
	colorEnabled := isTTY && !noColor && !headless

	profile := termenv.Ascii
	if colorEnabled {
		profile = termenv.ColorProfile()
	}

	return &UI{
		w:        w,
		output:   termenv.NewOutput(w, termenv.WithProfile(profile)),
		noColor:  !colorEnabled,
		isTTY:    isTTY,
		headless: headless,
	}
}

// NewPlain creates a UI with no color and writes to the given writer.
// Useful for tests. The result is in headless mode: narration writes plain
// `text\n` lines to w with no icons or color, progress methods don't spawn
// a spinner, and Blank/TermBlank are no-ops.
func NewPlain(w io.Writer) *UI {
	return &UI{
		w:        w,
		output:   termenv.NewOutput(w, termenv.WithProfile(termenv.Ascii)),
		noColor:  true,
		headless: true,
	}
}

func isColorDisabled() bool {
	if _, ok := os.LookupEnv("NO_COLOR"); ok {
		return true
	}
	if v := os.Getenv("CLICOLOR"); v == "0" {
		return true
	}
	return false
}

// isHeadless reports whether the UI should run in plain-text streaming mode:
// any non-TTY writer, or any environment where CI is set (most providers set
// CI=true; GitHub Actions and others honor this convention). Headless mode
// suppresses spinners and ANSI styling so CI logs stay greppable.
func isHeadless(isTTY bool) bool {
	if !isTTY {
		return true
	}
	if v := os.Getenv("CI"); v != "" && v != "0" && v != "false" {
		return true
	}
	return false
}

// headlessEmit writes one plain-text line (no icons, no color, no log
// routing) to the UI writer. Used by every narration and Term* method when
// headless mode is on so CI logs are flat, greppable, and stdout-safe. Any
// trailing newlines in text are normalized so callers can pass either pre-
// terminated strings (e.g. from Detail's "  msg\n" pattern) or bare text.
func (u *UI) headlessEmit(text string) {
	text = strings.TrimRight(text, "\n")
	fmt.Fprintln(u.w, text)
}

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
	_, err = sw.w.Write([]byte(buf.String()))
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
			body = frame + " " + w[len("→ "):]
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
	sw.w.Write([]byte(buf.String()))
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

	// Hide the cursor for the duration of the animation so per-tick
	// cursor repositioning doesn't create visual noise. Restored in
	// stopAnimator unconditionally so a crash or early-stop can't leave
	// the cursor permanently invisible.
	fmt.Fprint(sw.w, "\033[?25l")

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
	// Restore the cursor that startAnimator hid.
	fmt.Fprint(sw.w, "\033[?25h")
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

// Success prints a green "✓" prefixed message.
func (u *UI) Success(msg string, args ...any) {
	text := fmt.Sprintf(msg, args...)
	if u.logging() {
		u.logTagged("success", text)
		return
	}
	if u.headless {
		u.headlessEmit(text)
		return
	}
	u.emit(fmt.Sprintf("%s %s\n", u.Green(IconSuccess), text))
}

// Error prints a red "✗" prefixed message.
func (u *UI) Error(msg string, args ...any) {
	text := fmt.Sprintf(msg, args...)
	if u.logging() {
		u.logTagged("error", text)
		return
	}
	if u.headless {
		u.headlessEmit(text)
		return
	}
	u.emit(fmt.Sprintf("%s %s\n", u.Red(IconError), text))
}

// Warning prints a yellow "!" prefixed message.
func (u *UI) Warning(msg string, args ...any) {
	text := fmt.Sprintf(msg, args...)
	if u.logging() {
		u.logTagged("warning", text)
		return
	}
	if u.headless {
		u.headlessEmit(text)
		return
	}
	u.emit(fmt.Sprintf("%s %s\n", u.Yellow(IconWarning), text))
}

// Skip prints a gray "-" prefixed message.
func (u *UI) Skip(msg string, args ...any) {
	text := fmt.Sprintf(msg, args...)
	if u.logging() {
		u.logTagged("skip", text)
		return
	}
	if u.headless {
		u.headlessEmit(text)
		return
	}
	u.emit(fmt.Sprintf("%s %s\n", u.Dim(IconSkip), u.Dim(text)))
}

// Info prints a message with no prefix.
func (u *UI) Info(msg string, args ...any) {
	if u.logging() {
		u.logTagged("info", fmt.Sprintf(msg, args...))
		return
	}
	if u.headless {
		u.headlessEmit(fmt.Sprintf(msg, args...))
		return
	}
	u.emit(fmt.Sprintf(msg+"\n", args...))
}

// Infof prints a message with no prefix and no trailing newline.
func (u *UI) Infof(msg string, args ...any) {
	if u.logging() {
		u.logTagged("info", fmt.Sprintf(msg, args...))
		return
	}
	if u.headless {
		u.headlessEmit(fmt.Sprintf(msg, args...))
		return
	}
	u.emit(fmt.Sprintf(msg, args...))
}

// Header prints a bold message, used for file/section headers.
func (u *UI) Header(msg string, args ...any) {
	text := fmt.Sprintf(msg, args...)
	if u.logging() {
		u.logTagged("header", text)
		return
	}
	if u.headless {
		u.headlessEmit(text)
		return
	}
	u.emit(fmt.Sprintf("\n%s\n", u.Bold(text)))
}

// Hint prints a dim, indented message — typically a suggested command.
func (u *UI) Hint(msg string, args ...any) {
	text := fmt.Sprintf(msg, args...)
	if u.logging() {
		u.logTagged("hint", text)
		return
	}
	if u.headless {
		u.headlessEmit(text)
		return
	}
	u.emit(fmt.Sprintf("  %s\n", u.Dim(text)))
}

// Detail prints an indented detail line (2-space indent).
func (u *UI) Detail(msg string, args ...any) {
	if u.logging() {
		u.logTagged("", fmt.Sprintf(msg, args...))
		return
	}
	if u.headless {
		u.headlessEmit(fmt.Sprintf(msg, args...))
		return
	}
	u.emit(fmt.Sprintf("  "+msg+"\n", args...))
}

// Blank prints an empty line. In log mode it is a no-op so the JSONL transcript
// stays one valid object per line. In headless mode it is also a no-op so CI
// logs stay flat — phase boundaries do the visual separating instead.
func (u *UI) Blank() {
	if u.headless || u.logging() {
		return
	}
	u.emit("\n")
}

// TermSuccess prints a green "✓" summary line directly to the terminal,
// bypassing the narration log. Use for the final run summary.
func (u *UI) TermSuccess(msg string, args ...any) {
	if u.headless {
		u.headlessEmit(fmt.Sprintf(msg, args...))
		return
	}
	fmt.Fprintf(u.w, "%s %s\n", u.paint("2", IconSuccess), fmt.Sprintf(msg, args...))
}

// TermError prints a red "✗" summary line directly to the terminal.
func (u *UI) TermError(msg string, args ...any) {
	if u.headless {
		u.headlessEmit(fmt.Sprintf(msg, args...))
		return
	}
	fmt.Fprintf(u.w, "%s %s\n", u.paint("1", IconError), fmt.Sprintf(msg, args...))
}

// TermWarn prints a yellow "!" summary line directly to the terminal.
func (u *UI) TermWarn(msg string, args ...any) {
	if u.headless {
		u.headlessEmit(fmt.Sprintf(msg, args...))
		return
	}
	fmt.Fprintf(u.w, "%s %s\n", u.paint("3", IconWarning), fmt.Sprintf(msg, args...))
}

// TermCaution prints a red "!" summary line directly to the terminal. Use for
// non-fatal but attention-worthy signals (e.g. a commit pinned only after a
// full-branch-scan fallback) that warrant red without the "✗ failure" framing.
func (u *UI) TermCaution(msg string, args ...any) {
	if u.headless {
		u.headlessEmit(fmt.Sprintf(msg, args...))
		return
	}
	fmt.Fprintf(u.w, "%s %s\n", u.paint("1", IconWarning), fmt.Sprintf(msg, args...))
}

// TermDetail prints an indented summary detail line directly to the terminal.
func (u *UI) TermDetail(msg string, args ...any) {
	if u.headless {
		u.headlessEmit(fmt.Sprintf(msg, args...))
		return
	}
	fmt.Fprintf(u.w, "  "+msg+"\n", args...)
}

// TermNeutral prints a dimmed, neutral "-" summary line directly to the
// terminal. Per cli/cli iconography, "-" denotes neutral/informational
// status (not success, alert, or failure). Used for footer pointers such
// as the resolution-record path.
func (u *UI) TermNeutral(msg string, args ...any) {
	text := fmt.Sprintf(msg, args...)
	if u.headless {
		u.headlessEmit(text)
		return
	}
	fmt.Fprintf(u.w, "%s %s\n", u.TermDim(IconSkip), u.TermDim(text))
}

// TermYellow returns s in yellow for use in Term* output. Unlike Yellow, this
// does not suppress color when a narration log sink is attached, since Term*
// methods write directly to the terminal rather than the log.
func (u *UI) TermYellow(s string) string {
	return u.paint("3", s)
}

// TermDim returns s in dim/faint for use in Term* output.
func (u *UI) TermDim(s string) string {
	if u.noColor {
		return s
	}
	return u.output.String(s).Faint().String()
}

// TermBold returns s in bold for use in Term* output.
func (u *UI) TermBold(s string) string {
	if u.noColor {
		return s
	}
	return u.output.String(s).Bold().String()
}

// TermLink wraps text in an OSC 8 hyperlink for use in Term* output. Falls
// back to plain text when color is disabled or url is empty.
func (u *UI) TermLink(text, url string) string {
	if u.noColor || url == "" {
		return text
	}
	return u.output.Hyperlink(url, text)
}

// TermBlank prints an empty line directly to the terminal.
func (u *UI) TermBlank() {
	if u.headless {
		return
	}
	fmt.Fprintln(u.w)
}

// Bold returns s in bold if color is enabled.
func (u *UI) Bold(s string) string {
	if u.noColor || u.logging() {
		return s
	}
	return u.output.String(s).Bold().String()
}

// Dim returns s in dim/faint if color is enabled.
func (u *UI) Dim(s string) string {
	if u.noColor || u.logging() {
		return s
	}
	return u.output.String(s).Faint().String()
}

// Red returns s in red if color is enabled.
func (u *UI) Red(s string) string {
	if u.noColor || u.logging() {
		return s
	}
	return u.output.String(s).Foreground(u.output.Color("1")).String()
}

// Green returns s in green if color is enabled.
func (u *UI) Green(s string) string {
	if u.noColor || u.logging() {
		return s
	}
	return u.output.String(s).Foreground(u.output.Color("2")).String()
}

// Yellow returns s in yellow if color is enabled.
func (u *UI) Yellow(s string) string {
	if u.noColor || u.logging() {
		return s
	}
	return u.output.String(s).Foreground(u.output.Color("3")).String()
}

// Cyan returns s in cyan if color is enabled.
func (u *UI) Cyan(s string) string {
	if u.noColor || u.logging() {
		return s
	}
	return u.output.String(s).Foreground(u.output.Color("6")).String()
}

// Hyperlink returns text as a clickable OSC 8 hyperlink when the terminal
// supports it, otherwise returns text as-is. Most modern terminals (iTerm2,
// WezTerm, kitty, GNOME Terminal, Windows Terminal) support this.
func (u *UI) Hyperlink(text, url string) string {
	if u.noColor || u.logging() {
		return text
	}
	return u.output.Hyperlink(url, text)
}

// DocLink renders a documentation reference: the bare URL when writing to the
// log (so the transcript stays actionable), otherwise a dim "docs" hyperlink
// for the terminal.
func (u *UI) DocLink(url string) string {
	if u.logging() {
		return url
	}
	return u.Dim(u.Hyperlink("docs", url))
}

// IsTTY returns true if the output is a terminal.
func (u *UI) IsTTY() bool {
	return u.isTTY
}

// Headless reports whether the UI is running in plain-text streaming mode
// (non-TTY writer, or CI environment). Callers can use this to gate behavior
// that should differ between interactive and machine-consumable output — for
// example, leaving the narration log attached so per-action lines stream to
// stderr rather than being discarded.
func (u *UI) Headless() bool {
	return u.headless
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
	u.spinner = sp
	u.progLabel = label
	u.progDetail = ""
	u.progLast = ""
	u.progPaused = false
	u.renderProgress()

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
		u.progLabel = ""
		u.progDetail = ""
		u.progLast = ""
		u.progPaused = false
	}
}

// UpdateProgress sets a detail string in worker slot 0 (backward-compat
// single-detail shim). No-op when no spinner is active.
func (u *UI) UpdateProgress(detail string) {
	if u.spinner == nil {
		return
	}
	u.progDetail = detail
	u.renderProgress()
}

// SetWorkerStatus sets or clears one worker slot's status line, shown as a
// subdued line below the spinner. slot indexes from 0. No-op when no spinner
// is active.
func (u *UI) SetWorkerStatus(slot int, status string) {
	u.traceProgress(fmt.Sprintf("slot[%d]", slot), status)
	if u.spinWriter == nil {
		return
	}
	if !u.noColor {
		width := u.termWidth()
		if width > 4 {
			status = truncateBytes(status, width-4)
		}
	}
	u.spinWriter.setWorkerStatus(slot, status)
}

// SetWorkerHint sets or clears a dim suffix appended after the worker slot's
// status text (e.g. "→ workflow.yml (still working…)"). Used by pinpool's
// stall watcher to surface that a worker has been on the same job for longer
// than the stall threshold without clobbering the slot's main status. No-op
// when no spinner is active.
func (u *UI) SetWorkerHint(slot int, hint string) {
	u.traceProgress(fmt.Sprintf("hint[%d]", slot), hint)
	if u.spinWriter == nil {
		return
	}
	u.spinWriter.setWorkerHint(slot, hint)
}

// ClearWorkerStatuses wipes every worker slot so stale "✓ NWO" rows from a
// completed phase don't carry into the next one. No-op when no spinner is
// active.
func (u *UI) ClearWorkerStatuses() {
	if u.spinWriter == nil {
		return
	}
	u.spinWriter.mu.Lock()
	for i := range u.spinWriter.workers {
		u.spinWriter.workers[i] = ""
	}
	for i := range u.spinWriter.hints {
		u.spinWriter.hints[i] = ""
	}
	u.spinWriter.mu.Unlock()
}

// UpdateLabel changes the spinner prefix label (e.g. to show per-workflow
// "[i/N] path" progress). No-op when no spinner is active.
func (u *UI) UpdateLabel(label string) {
	u.traceProgress("label", label)
	if u.headless {
		stem := labelStem(label)
		if stem != "" && stem != u.headlessLabelStem {
			u.headlessEmit(stem)
			u.headlessLabelStem = stem
		}
		return
	}
	if u.spinner == nil {
		return
	}
	u.progLabel = label
	u.renderProgress()
}

// labelStem returns the label trimmed of whitespace, used as a phase
// identifier for headless dedup so repeated UpdateLabel calls with the
// same text don't spam the log.
func labelStem(label string) string {
	return strings.TrimSpace(label)
}

// renderProgress recombines the label and detail into a single line that is
// truncated to fit the terminal width (leaving room for the spinner glyph and
// a space). The spinner glyph is anchored at the left edge (column 0): the
// combined line is assigned to the spinner Suffix with an empty Prefix, so the
// library always renders "\r{glyph} {label} — {detail}". Keeping the glyph
// fixed on the left stops it from drifting as the detail text changes width.
// The whole string is truncated to one terminal row — wrapping would defeat
// the library's backspace-based erase and cause the line jumping the user
// sees.
func (u *UI) renderProgress() {
	if u.spinner == nil {
		return
	}

	label := u.progLabel
	detail := u.progDetail

	if label == "" {
		if u.progLast == "" {
			return
		}
		label = u.progLast
	} else {
		u.progLast = label
	}

	width := u.termWidth()
	if width > 8 {
		budget := width - 6
		if !u.noColor {
			budget -= 7 // bold escape open+close
		}
		label = truncateBytes(label, budget)
	}

	if detail != "" {
		// Worker rows render a pulsing glyph only when the text starts
		// with "→ "; UpdateProgress callers (resolver progress hooks)
		// pass plain strings like "resolving foo@bar". Prepend the
		// arrow so the slot animates instead of looking frozen.
		if !strings.HasPrefix(detail, "→ ") {
			detail = "→ " + detail
		}
		if width > 4 {
			budget := width - 4 // "  " indent + faint open+close
			if !u.noColor {
				budget -= 7
			}
			detail = truncateBytes(detail, budget)
		}
	}

	// Pass detail (slot 0) to the writer; it appends worker lines on every
	// spinner tick without inflating the Suffix byte count.
	// Only write when detail is non-empty: calling setDetail("") would
	// overwrite slot 0 that the pool's worker status may have set.
	if u.spinWriter != nil && detail != "" {
		u.spinWriter.setDetail(detail)
	}
	u.progHasDetail = detail != ""

	var suffix string
	if !u.noColor {
		suffix = u.output.String(label).Bold().String()
	} else {
		suffix = label
	}

	u.spinner.Prefix = ""
	if suffix != "" {
		u.spinner.Suffix = " " + suffix
	} else {
		u.spinner.Suffix = ""
	}
}

// termWidth returns the terminal column count for the spinner writer, or 0 if
// it cannot be determined (in which case callers skip truncation).
func (u *UI) termWidth() int {
	return termWidthOf(u.w)
}

// termWidthOf returns the terminal width of w, or 0 when it isn't a TTY.
func termWidthOf(w io.Writer) int {
	f, ok := w.(*os.File)
	if !ok {
		return 0
	}
	cols, _, err := term.GetSize(int(f.Fd()))
	if err != nil || cols <= 0 {
		return 0
	}
	return cols
}

// truncateBytes shortens s so its UTF-8 byte length is at most max, never
// splitting a multibyte rune. When truncation occurs the tail is replaced with
// a single ellipsis ("…", 3 bytes). The budget is byte-based because the
// spinner library measures wrap width in bytes; a rune/column budget lets
// multibyte characters (the "—" separator, non-ASCII paths) push the real byte
// width past the terminal edge and trigger its two-line erase.
func truncateBytes(s string, max int) string {
	if max <= 0 {
		return ""
	}
	if len(s) <= max {
		return s
	}
	const ellipsis = "…" // 3 bytes
	if max < len(ellipsis) {
		return trimToRuneBoundary(s, max)
	}
	return trimToRuneBoundary(s, max-len(ellipsis)) + ellipsis
}

// trimToRuneBoundary returns the longest prefix of s whose byte length is at
// most max, cut on a rune boundary so multibyte characters aren't split.
func trimToRuneBoundary(s string, max int) string {
	if max <= 0 {
		return ""
	}
	if len(s) <= max {
		return s
	}
	end := max
	for end > 0 && !utf8.RuneStart(s[end]) {
		end--
	}
	return s[:end]
}

// Pluralize returns singular when n==1, plural otherwise.
func Pluralize(n int, singular, plural string) string {
	if n == 1 {
		return singular
	}
	return plural
}
