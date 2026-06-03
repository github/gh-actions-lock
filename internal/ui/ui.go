// Package ui provides terminal-aware output formatting for gh-actions-pin.
// It respects NO_COLOR, CLICOLOR, and TTY detection on stderr (not stdout)
// so that color works correctly even when stdout is piped (e.g. --json mode).
package ui

import (
	"fmt"
	"io"
	"os"
	"time"

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
type UI struct {
	w       io.Writer
	logw    io.Writer
	output  *termenv.Output
	noColor bool
	isTTY   bool
	spinner *spinner.Spinner

	// progLabel and progDetail hold the two halves of the active spinner line
	// (the per-workflow label and the resolver's current-action detail). They
	// are recombined and truncated to one terminal row on every update so the
	// spinner never wraps — a wrapped spinner breaks the library's
	// backspace-based erase and causes line jumping/leftover fragments.
	progLabel  string
	progDetail string
}

// SetLog attaches a narration sink. Once set, narration methods write plain
// text to w instead of the terminal. Pass nil to detach.
func (u *UI) SetLog(w io.Writer) {
	u.logw = w
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

// logTagged writes one structured line to the log sink: a fixed-width status
// column ("ok", "warn", "err", …) followed by the message. Continuation lines
// pass an empty tag so they align as indented detail under the column. The
// message keeps any leading whitespace it carries, preserving sub-hierarchy.
func (u *UI) logTagged(tag, text string) {
	fmt.Fprintf(u.logw, "%-6s%s\n", tag, text)
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
	colorEnabled := isTTY && !noColor

	profile := termenv.Ascii
	if colorEnabled {
		profile = termenv.ColorProfile()
	}

	return &UI{
		w:       os.Stderr,
		output:  termenv.NewOutput(os.Stderr, termenv.WithProfile(profile)),
		noColor: !colorEnabled,
		isTTY:   isTTY,
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
	colorEnabled := isTTY && !noColor

	profile := termenv.Ascii
	if colorEnabled {
		profile = termenv.ColorProfile()
	}

	return &UI{
		w:       w,
		output:  termenv.NewOutput(w, termenv.WithProfile(profile)),
		noColor: !colorEnabled,
		isTTY:   isTTY,
	}
}

// NewPlain creates a UI with no color and writes to the given writer.
// Useful for tests.
func NewPlain(w io.Writer) *UI {
	return &UI{
		w:       w,
		output:  termenv.NewOutput(w, termenv.WithProfile(termenv.Ascii)),
		noColor: true,
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

// printLine writes one line of output, transparently pausing an active
// spinner so its animation frame doesn't interleave with (and corrupt) the
// text. The spinner resumes after the write. When no spinner is active it
// just runs write.
func (u *UI) printLine(write func()) {
	if u.spinner != nil && u.spinner.Active() {
		u.spinner.Stop()
		// Clear any spinner remnant before writing the real line.
		fmt.Fprint(u.w, "\r\033[2K")
		write()
		u.spinner.Start()
		return
	}
	write()
}

// Success prints a green "✓" prefixed message.
func (u *UI) Success(msg string, args ...any) {
	text := fmt.Sprintf(msg, args...)
	if u.logging() {
		u.logTagged("ok", text)
		return
	}
	u.emit(fmt.Sprintf("%s %s\n", u.Green(IconSuccess), text))
}

// Error prints a red "✗" prefixed message.
func (u *UI) Error(msg string, args ...any) {
	text := fmt.Sprintf(msg, args...)
	if u.logging() {
		u.logTagged("err", text)
		return
	}
	u.emit(fmt.Sprintf("%s %s\n", u.Red(IconError), text))
}

// Warning prints a yellow "!" prefixed message.
func (u *UI) Warning(msg string, args ...any) {
	text := fmt.Sprintf(msg, args...)
	if u.logging() {
		u.logTagged("warn", text)
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
	u.emit(fmt.Sprintf("%s %s\n", u.Dim(IconSkip), u.Dim(text)))
}

// Info prints a message with no prefix.
func (u *UI) Info(msg string, args ...any) {
	if u.logging() {
		u.logTagged("info", fmt.Sprintf(msg, args...))
		return
	}
	u.emit(fmt.Sprintf(msg+"\n", args...))
}

// Infof prints a message with no prefix and no trailing newline.
func (u *UI) Infof(msg string, args ...any) {
	u.emit(fmt.Sprintf(msg, args...))
}

// Header prints a bold message, used for file/section headers.
func (u *UI) Header(msg string, args ...any) {
	text := fmt.Sprintf(msg, args...)
	if u.logging() {
		fmt.Fprintf(u.logw, "\n=== %s ===\n", text)
		return
	}
	u.emit(fmt.Sprintf("\n%s\n", u.Bold(text)))
}

// Hint prints a dim, indented message — typically a suggested command.
func (u *UI) Hint(msg string, args ...any) {
	text := fmt.Sprintf(msg, args...)
	if u.logging() {
		u.logTagged("", text)
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
	u.emit(fmt.Sprintf("  "+msg+"\n", args...))
}

// Blank prints an empty line.
func (u *UI) Blank() {
	u.emit("\n")
}

// TermSuccess prints a green "✓" summary line directly to the terminal,
// bypassing the narration log. Use for the final run summary.
func (u *UI) TermSuccess(msg string, args ...any) {
	fmt.Fprintf(u.w, "%s %s\n", u.paint("2", IconSuccess), fmt.Sprintf(msg, args...))
}

// TermError prints a red "✗" summary line directly to the terminal.
func (u *UI) TermError(msg string, args ...any) {
	fmt.Fprintf(u.w, "%s %s\n", u.paint("1", IconError), fmt.Sprintf(msg, args...))
}

// TermWarn prints a yellow "!" summary line directly to the terminal.
func (u *UI) TermWarn(msg string, args ...any) {
	fmt.Fprintf(u.w, "%s %s\n", u.paint("3", IconWarning), fmt.Sprintf(msg, args...))
}

// TermDetail prints an indented summary detail line directly to the terminal.
func (u *UI) TermDetail(msg string, args ...any) {
	fmt.Fprintf(u.w, "  "+msg+"\n", args...)
}

// TermBlank prints an empty line directly to the terminal.
func (u *UI) TermBlank() {
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

// StartProgress starts an animated spinner with the given label on stderr.
// On non-TTY outputs, prints a static label instead. Matches gh CLI's Primer
// progress indicator: braille dots, 120ms, cyan.
func (u *UI) StartProgress(label string) {
	if !u.isTTY {
		if label != "" {
			fmt.Fprintf(u.w, "%s...\n", label)
		}
		return
	}
	opts := []spinner.Option{spinner.WithWriter(u.w)}
	if !u.noColor {
		opts = append(opts, spinner.WithColor("fgCyan"))
	}
	sp := spinner.New(spinner.CharSets[11], 120*time.Millisecond, opts...)
	u.spinner = sp
	u.progLabel = label
	u.progDetail = ""
	u.renderProgress()
	sp.Start()
}

// StopProgress stops the spinner. Safe to call if no spinner is active.
func (u *UI) StopProgress() {
	if u.spinner != nil {
		u.spinner.Stop()
		u.spinner = nil
		u.progLabel = ""
		u.progDetail = ""
		// Clear the spinner line.
		fmt.Fprintf(u.w, "\r\033[2K")
	}
}

// UpdateProgress sets a detail string appended after the spinner label
// (e.g. the current action being scanned). No-op when no spinner is
// active or when running without color (non-TTY).
func (u *UI) UpdateProgress(detail string) {
	if u.spinner == nil {
		return
	}
	u.progDetail = detail
	u.renderProgress()
}

// UpdateLabel changes the spinner prefix label (e.g. to show per-workflow
// "[i/N] path" progress). No-op when no spinner is active.
func (u *UI) UpdateLabel(label string) {
	if u.spinner == nil {
		return
	}
	u.progLabel = label
	u.renderProgress()
}

// renderProgress recombines the label and detail into a single line that is
// truncated to fit the terminal width (leaving room for the spinner glyph and
// a space). The whole string is assigned to the spinner Prefix with an empty
// Suffix so the library always erases and redraws exactly one row — wrapping
// would defeat its backspace-based erase and cause the line jumping the user
// sees. The trailing space keeps the spinner glyph off the last column.
func (u *UI) renderProgress() {
	if u.spinner == nil {
		return
	}
	line := u.progLabel
	if u.progDetail != "" {
		if line != "" {
			line += " — "
		}
		line += u.progDetail
	}
	// Reserve 2 columns: the spinner glyph and a trailing space.
	if width := u.termWidth(); width > 3 {
		line = truncateRunes(line, width-2)
	}
	if line != "" {
		line += " "
	}
	u.spinner.Prefix = line
	u.spinner.Suffix = ""
}

// termWidth returns the terminal column count for the spinner writer, or 0 if
// it cannot be determined (in which case callers skip truncation).
func (u *UI) termWidth() int {
	f, ok := u.w.(*os.File)
	if !ok {
		return 0
	}
	w, _, err := term.GetSize(int(f.Fd()))
	if err != nil || w <= 0 {
		return 0
	}
	return w
}

// truncateRunes shortens s to at most max runes, replacing the tail with a
// single-character ellipsis when it overflows. Operates on runes so multibyte
// paths aren't cut mid-character.
func truncateRunes(s string, max int) string {
	if max <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	if max == 1 {
		return "…"
	}
	return string(r[:max-1]) + "…"
}

// Pluralize returns singular when n==1, plural otherwise.
func Pluralize(n int, singular, plural string) string {
	if n == 1 {
		return singular
	}
	return plural
}
