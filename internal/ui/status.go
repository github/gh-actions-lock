package ui

import "fmt"

// Icons used in status-prefixed output. These always appear regardless of
// color setting — Primer says don't rely solely on color.
const (
	IconSuccess = "✓"
	IconError   = "✗"
	IconWarning = "!"
	IconSkip    = "-"
)

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

// TermCaution prints a yellow "!" summary line directly to the terminal. Use for
// non-fatal but attention-worthy signals (e.g. a commit pinned only after a
// full-branch-scan fallback) that warrant emphasis without the "✗ failure" framing.
func (u *UI) TermCaution(msg string, args ...any) {
	if u.headless {
		u.headlessEmit(fmt.Sprintf(msg, args...))
		return
	}
	fmt.Fprintf(u.w, "%s %s\n", u.paint("3", IconWarning), fmt.Sprintf(msg, args...))
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

// TermBlank prints an empty line directly to the terminal.
func (u *UI) TermBlank() {
	if u.headless {
		return
	}
	fmt.Fprintln(u.w)
}
