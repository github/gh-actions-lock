package ui

// Color and style helpers. The plain (non-Term*) wrappers suppress styling
// while a narration log sink is attached so the JSONL transcript stays plain
// text; the Term* variants ignore the log sink because they always target the
// terminal.

// paint applies an ANSI foreground color regardless of the log sink. Used by
// the Term* summary methods, which always target the terminal.
func (u *UI) paint(colorCode, s string) string {
	if u.noColor {
		return s
	}
	return u.output.String(s).Foreground(u.output.Color(colorCode)).String()
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
