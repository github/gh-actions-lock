package ui

import (
	"io"
	"os"
	"strings"
	"unicode/utf8"

	"golang.org/x/term"
)

// labelStem returns the label trimmed of whitespace, used as a phase
// identifier for headless dedup so repeated UpdateLabel calls with the
// same text don't spam the log.
func labelStem(label string) string {
	return strings.TrimSpace(label)
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
