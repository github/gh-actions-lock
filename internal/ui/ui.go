// Package ui provides terminal-aware output formatting for gh-actions-lock.
// It respects NO_COLOR, CLICOLOR, and TTY detection on stderr (not stdout)
// so that color works correctly even when stdout is piped (e.g. --json mode).
//
// The implementation is split across files within the package:
//
//   - ui.go: the UI struct, constructors, mode detection, and the low-level
//     emit/log plumbing.
//   - style.go: color and hyperlink helpers.
//   - status.go: status-prefixed narration (Success, Error, …) and the Term*
//     summary methods.
//   - spinner.go: the spinner writer and progress spinner lifecycle.
//   - progress.go: the worker-slot progress API and debug tracing.
//   - text.go: width-aware truncation and other string utilities.
package ui

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/briandowns/spinner"
	"github.com/muesli/termenv"
	"golang.org/x/term"
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

	// progDetail holds the current resolver detail shown in worker slot 0.
	progDetail string

	// progPaused is set while the spinner is temporarily halted (e.g. to let an
	// interactive prompt own the terminal). The spinner object is retained so
	// ResumeProgress can restart it with the same label/detail.
	progPaused bool

	// progHasDetail tracks whether the last renderProgress call put something
	// in worker slot 0. Used by clearSpinnerLines to know the row count.
	progHasDetail bool

	// spinWriter is a thin io.Writer wrapper set while a spinner is active.
	// It intercepts each spinner tick write and appends worker status rows
	// below the spinner line in a single synchronized write.
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
