package main

import (
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/Abyssinia-Labs/latem/pkg/protocol"
)

// Terminal styling, shared by every command.
//
// This began inside doctor.go, which was the only command that produced
// coloured output. Everything else printed plain text, so `latem run` and
// `latem list` looked like a different program from `latem doctor`. Moving it
// here is what lets one decision — is this a terminal? — be made once.
//
// Three rules hold throughout:
//
//  1. Colour is decoration, never information. Every state that has a colour
//     also has a word or a symbol, because a pipe, a CI transcript, a screen
//     reader and a colourblind reader all lose the colour and none of them
//     should lose the meaning.
//  2. Style is decided per writer, not globally. A command may print a table to
//     stdout and a diagnostic to stderr, and one of those can be a terminal
//     while the other is a file.
//  3. Nothing here needs a dependency. Spec §3 makes the dependency list a
//     reviewed part of the design, and ANSI escapes plus a stat are not a trade
//     worth making a module for.
const (
	ansiReset  = "\x1b[0m"
	ansiBold   = "\x1b[1m"
	ansiDim    = "\x1b[90m"
	ansiRed    = "\x1b[31m"
	ansiGreen  = "\x1b[32m"
	ansiYellow = "\x1b[33m"
	ansiCyan   = "\x1b[36m"
)

// styler renders text for one destination. The zero value is safe and plain,
// which matters: a command that forgets to build one still produces correct
// output rather than escape codes in a log file.
type styler struct {
	color bool
	width int
}

// newStyler decides once, for one writer.
func newStyler(w io.Writer) styler {
	return styler{color: useColor(w), width: terminalWidth()}
}

func (s styler) wrap(code, text string) string {
	if !s.color || text == "" {
		return text
	}
	return code + text + ansiReset
}

func (s styler) bold(t string) string   { return s.wrap(ansiBold, t) }
func (s styler) dim(t string) string    { return s.wrap(ansiDim, t) }
func (s styler) ok(t string) string     { return s.wrap(ansiGreen, t) }
func (s styler) warn(t string) string   { return s.wrap(ansiYellow, t) }
func (s styler) bad(t string) string    { return s.wrap(ansiRed, t) }
func (s styler) accent(t string) string { return s.wrap(ansiCyan, t) }

// heading is a section title: bold on a terminal, and on its own line either
// way so the structure survives losing the colour.
func (s styler) heading(t string) string { return s.bold(t) }

// useColor reports whether the destination is an interactive terminal.
//
// The check is a stat on the file rather than a terminal library, because
// adding a dependency to colour four words is not a trade worth making in a
// module whose dependency list is a reviewed part of the spec (§3).
//
// NO_COLOR is honoured because it is the convention every other tool an
// operator has follows (no-color.org), and --no-color because a flag is what
// somebody reaches for when they have already been surprised once.
func useColor(w io.Writer) bool {
	if global.noColor || os.Getenv("NO_COLOR") != "" {
		return false
	}
	// TERM=dumb is how emacs shells and some CI runners announce that they
	// render nothing. Believing them costs nothing.
	if os.Getenv("TERM") == "dumb" {
		return false
	}
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}

// defaultWidth is what to assume when nothing says otherwise. 80 is the
// conservative choice: too narrow only wastes space, while too wide wraps in
// the middle of a word on the machine that can least afford confusion.
const defaultWidth = 80

// terminalWidth reads COLUMNS, which a shell exports and a user can override.
//
// There is no ioctl here for the same reason there is no terminal library: the
// failure mode of guessing wrong is a line break in an awkward place, and that
// is not worth a dependency or a syscall wrapper per platform.
func terminalWidth() int {
	n, err := strconv.Atoi(os.Getenv("COLUMNS"))
	if err != nil || n < 40 {
		return defaultWidth
	}
	if n > 120 {
		// Long measures are hard to read. Cap rather than filling a 300-column
		// window with one sentence.
		return 120
	}
	return n
}

// column pads s to width, measuring the text rather than the escape codes.
//
// %-*s counts bytes, so a styled cell padded that way comes out short by the
// length of its escape sequences — the column drifts by exactly as much colour
// as it contains, which looks like a rendering bug and is one.
func (s styler) column(text string, width int) string {
	visible := visibleLen(text)
	if visible >= width {
		return text
	}
	return text + strings.Repeat(" ", width-visible)
}

// printAligned writes a table for a terminal: each column padded to its widest
// cell, the header dimmed, trailing space trimmed.
//
// paint styles a cell, and is told its column by header name rather than by
// position. `latem list` once coloured its state by index, and when # was
// inserted before it (amendment A36) the colour moved onto the build number,
// which paintState passes through plain — so the state lost its colour and
// nothing looked wrong.
func printAligned(out io.Writer, s styler, header []string, rows [][]string, paint func(column, cell string) string) {
	widths := make([]int, len(header))
	for i, h := range header {
		widths[i] = visibleLen(h)
	}
	for _, row := range rows {
		for i, cell := range row {
			if n := visibleLen(cell); n > widths[i] {
				widths[i] = n
			}
		}
	}

	var b strings.Builder
	for i, h := range header {
		b.WriteString(s.column(s.dim(h), widths[i]+2))
	}
	printf(out, "%s\n", strings.TrimRight(b.String(), " "))

	for _, row := range rows {
		b.Reset()
		for i, cell := range row {
			b.WriteString(s.column(paint(header[i], cell), widths[i]+2))
		}
		printf(out, "%s\n", strings.TrimRight(b.String(), " "))
	}
}

// visibleLen is the printed width of s, ignoring ANSI escape sequences.
func visibleLen(s string) int {
	n, inEscape := 0, false
	for _, r := range s {
		switch {
		case inEscape:
			if r == 'm' {
				inEscape = false
			}
		case r == '\x1b':
			inEscape = true
		default:
			n++
		}
	}
	return n
}

// wrapLines breaks text into lines no longer than width, on word boundaries.
func wrapLines(s string, width int) []string {
	words := strings.Fields(s)
	if len(words) == 0 {
		return nil
	}
	var (
		lines []string
		line  strings.Builder
	)
	for _, w := range words {
		if line.Len() > 0 && line.Len()+1+len(w) > width {
			lines = append(lines, line.String())
			line.Reset()
		}
		if line.Len() > 0 {
			line.WriteByte(' ')
		}
		line.WriteString(w)
	}
	if line.Len() > 0 {
		lines = append(lines, line.String())
	}
	return lines
}

// paintState colours a run state and returns the word unchanged when there is
// no colour to give it.
//
// The states an operator is scanning for are the two ends: failed, and running.
// Everything else is context, so it stays plain rather than competing for
// attention — a table where every row is coloured is a table with no signal.
func (s styler) paintState(st protocol.RunState) string {
	switch st {
	case protocol.StateSucceeded:
		return s.ok(string(st))
	case protocol.StateFailed, protocol.StateCancelled, protocol.StateRejected:
		return s.bad(string(st))
	case protocol.StateRunning, protocol.StatePreparing, protocol.StateCollecting:
		return s.accent(string(st))
	case protocol.StateAwaitingApproval:
		return s.warn(string(st))
	default:
		return string(st)
	}
}
