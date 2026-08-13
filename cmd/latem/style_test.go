package main

import (
	"bytes"
	"os"
	"strings"
	"testing"

	"github.com/Abyssinia-Labs/latem/pkg/protocol"
)

// Padding must measure the printed text, not the bytes.
//
// This is the defect the column helper exists to prevent: %-*s counts bytes, so
// a styled cell padded that way comes out short by the length of its escape
// sequences. The column then drifts by exactly as much colour as it contains,
// which looks like a rendering bug and is one — and it only appears on a
// terminal, so it survives every piped test.
func TestColumnPadsVisibleWidth(t *testing.T) {
	styled := styler{color: true}
	plain := styler{color: false}

	cases := []struct{ name, text string }{
		{"plain", "failed"},
		{"coloured", styled.bad("failed")},
		{"dim", styled.dim("RUN ID")},
	}
	const width = 12
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := plain.column(tc.text, width)
			if n := visibleLen(got); n != width {
				t.Errorf("visible width = %d, want %d (%q)", n, width, got)
			}
		})
	}

	// Two rows, one coloured and one not, must line up.
	a := plain.column(styled.bad("failed"), width) + "x"
	b := plain.column("failed", width) + "x"
	if visibleLen(a) != visibleLen(b) {
		t.Errorf("a coloured cell and a plain one do not align: %d vs %d", visibleLen(a), visibleLen(b))
	}
}

func TestVisibleLenIgnoresEscapes(t *testing.T) {
	s := styler{color: true}
	for _, tc := range []struct {
		in   string
		want int
	}{
		{"", 0},
		{"plain", 5},
		{s.ok("ok"), 2},
		{s.bold(s.bad("failed")), 6},
		{"\x1b[31mred\x1b[0m and \x1b[32mgreen\x1b[0m", 13},
	} {
		if got := visibleLen(tc.in); got != tc.want {
			t.Errorf("visibleLen(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

// Colour is decoration, never information: every state keeps its word.
func TestPaintStateAlwaysKeepsTheWord(t *testing.T) {
	s := styler{color: true}
	for _, st := range []protocol.RunState{
		protocol.StateSucceeded, protocol.StateFailed, protocol.StateRunning,
		protocol.StateQueued, protocol.StateAwaitingApproval, protocol.StateCancelled,
	} {
		got := s.paintState(st)
		if !strings.Contains(got, string(st)) {
			t.Errorf("paintState(%s) = %q, which no longer contains the state name", st, got)
		}
		// And with colour off it must be exactly the word, with no escapes.
		if plain := (styler{}).paintState(st); plain != string(st) {
			t.Errorf("uncoloured paintState(%s) = %q, want %q", st, plain, st)
		}
	}
}

// A writer that is not a terminal must never receive escape codes: a log file,
// a pipe into grep, and a CI transcript are all this case.
func TestUseColorRefusesNonTerminals(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	t.Setenv("TERM", "xterm-256color")
	global.noColor = false
	t.Cleanup(func() { global.noColor = false })

	if useColor(&bytes.Buffer{}) {
		t.Error("a bytes.Buffer is not a terminal")
	}
	// A real file on disk is the log-redirect case.
	f, err := os.CreateTemp(t.TempDir(), "out")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if useColor(f) {
		t.Error("a regular file is not a terminal")
	}

	t.Setenv("NO_COLOR", "1")
	if useColor(os.Stdout) {
		t.Error("NO_COLOR must win even on a terminal")
	}
	t.Setenv("NO_COLOR", "")
	t.Setenv("TERM", "dumb")
	if useColor(os.Stdout) {
		t.Error("TERM=dumb must win even on a terminal")
	}
}
