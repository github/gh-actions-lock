// Package ui provides terminal-aware output formatting for gh-actions-pin.
// It respects NO_COLOR, CLICOLOR, and TTY detection on stderr (not stdout)
// so that color works correctly even when stdout is piped (e.g. --json mode).
package ui

import (
	"fmt"
	"io"
	"os"

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
type UI struct {
	w       io.Writer
	output  *termenv.Output
	noColor bool
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

// Success prints a green "✓" prefixed message.
func (u *UI) Success(msg string, args ...any) {
	text := fmt.Sprintf(msg, args...)
	fmt.Fprintf(u.w, "%s %s\n", u.Green(IconSuccess), text)
}

// Error prints a red "✗" prefixed message.
func (u *UI) Error(msg string, args ...any) {
	text := fmt.Sprintf(msg, args...)
	fmt.Fprintf(u.w, "%s %s\n", u.Red(IconError), text)
}

// Warning prints a yellow "!" prefixed message.
func (u *UI) Warning(msg string, args ...any) {
	text := fmt.Sprintf(msg, args...)
	fmt.Fprintf(u.w, "%s %s\n", u.Yellow(IconWarning), text)
}

// Skip prints a gray "-" prefixed message.
func (u *UI) Skip(msg string, args ...any) {
	text := fmt.Sprintf(msg, args...)
	fmt.Fprintf(u.w, "%s %s\n", u.Dim(IconSkip), u.Dim(text))
}

// Info prints a message with no prefix.
func (u *UI) Info(msg string, args ...any) {
	fmt.Fprintf(u.w, msg+"\n", args...)
}

// Infof prints a message with no prefix and no trailing newline.
func (u *UI) Infof(msg string, args ...any) {
	fmt.Fprintf(u.w, msg, args...)
}

// Header prints a bold message, used for file/section headers.
func (u *UI) Header(msg string, args ...any) {
	text := fmt.Sprintf(msg, args...)
	fmt.Fprintf(u.w, "\n%s\n", u.Bold(text))
}

// Hint prints a dim, indented message — typically a suggested command.
func (u *UI) Hint(msg string, args ...any) {
	text := fmt.Sprintf(msg, args...)
	fmt.Fprintf(u.w, "  %s\n", u.Dim(text))
}

// Detail prints an indented detail line (2-space indent).
func (u *UI) Detail(msg string, args ...any) {
	fmt.Fprintf(u.w, "  "+msg+"\n", args...)
}

// Bold returns s in bold if color is enabled.
func (u *UI) Bold(s string) string {
	if u.noColor {
		return s
	}
	return u.output.String(s).Bold().String()
}

// Dim returns s in dim/faint if color is enabled.
func (u *UI) Dim(s string) string {
	if u.noColor {
		return s
	}
	return u.output.String(s).Faint().String()
}

// Red returns s in red if color is enabled.
func (u *UI) Red(s string) string {
	if u.noColor {
		return s
	}
	return u.output.String(s).Foreground(u.output.Color("1")).String()
}

// Green returns s in green if color is enabled.
func (u *UI) Green(s string) string {
	if u.noColor {
		return s
	}
	return u.output.String(s).Foreground(u.output.Color("2")).String()
}

// Yellow returns s in yellow if color is enabled.
func (u *UI) Yellow(s string) string {
	if u.noColor {
		return s
	}
	return u.output.String(s).Foreground(u.output.Color("3")).String()
}

// Cyan returns s in cyan if color is enabled.
func (u *UI) Cyan(s string) string {
	if u.noColor {
		return s
	}
	return u.output.String(s).Foreground(u.output.Color("6")).String()
}
