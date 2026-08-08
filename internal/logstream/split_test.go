package logstream

import (
	"errors"
	"strings"
	"testing"
)

func collect(t *testing.T, in string, max int) ([]string, []int) {
	t.Helper()
	var lines []string
	var drops []int
	err := SplitLines(strings.NewReader(in), max, func(line string, dropped int) error {
		lines = append(lines, line)
		drops = append(drops, dropped)
		return nil
	})
	if err != nil {
		t.Fatalf("SplitLines: %v", err)
	}
	return lines, drops
}

func TestSplitLines(t *testing.T) {
	tests := map[string]struct {
		in   string
		want []string
	}{
		"empty":                  {"", nil},
		"single line with LF":    {"one\n", []string{"one"}},
		"single line without LF": {"one", []string{"one"}},
		"multiple":               {"one\ntwo\nthree\n", []string{"one", "two", "three"}},
		"crlf":                   {"one\r\ntwo\r\n", []string{"one", "two"}},
		"mixed endings":          {"one\r\ntwo\n", []string{"one", "two"}},
		"blank lines preserved":  {"one\n\ntwo\n", []string{"one", "", "two"}},
		"trailing blank":         {"one\n\n", []string{"one", ""}},
		"only newline":           {"\n", []string{""}},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			got, _ := collect(t, tc.in, MaxLineBytes)
			if len(got) != len(tc.want) {
				t.Fatalf("got %d lines %q, want %d %q", len(got), got, len(tc.want), tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("[%d] = %q, want %q", i, got[i], tc.want[i])
				}
			}
		})
	}
}

// A step that dies mid-write still gets its last words into the log, which is
// frequently the useful part.
func TestSplitLines_UnterminatedFinalLine(t *testing.T) {
	got, _ := collect(t, "complete\npartial", MaxLineBytes)
	if len(got) != 2 || got[1] != "partial" {
		t.Errorf("got %q", got)
	}
}

func TestSplitLines_Truncation(t *testing.T) {
	long := strings.Repeat("x", 100)
	lines, drops := collect(t, long+"\nshort\n", 40)

	if len(lines) != 2 {
		t.Fatalf("got %d lines", len(lines))
	}
	if len(lines[0]) != 40 {
		t.Errorf("kept %d bytes, want 40", len(lines[0]))
	}
	if drops[0] != 60 {
		t.Errorf("dropped %d, want 60", drops[0])
	}
	// Truncation must not derail the next line.
	if lines[1] != "short" || drops[1] != 0 {
		t.Errorf("line after truncation = %q (dropped %d)", lines[1], drops[1])
	}
}

// Truncating mid-character would emit half a rune and produce mojibake in
// whatever reads the log.
func TestSplitLines_TruncationRespectsRuneBoundaries(t *testing.T) {
	// "é" is two bytes; a 41-byte cut lands inside the 21st one.
	lines, _ := collect(t, strings.Repeat("é", 50)+"\n", 41)
	if len(lines) != 1 {
		t.Fatalf("got %d lines", len(lines))
	}
	for i, r := range lines[0] {
		if r == '�' {
			t.Fatalf("invalid rune at byte %d: %q", i, lines[0])
		}
	}
	if len(lines[0])%2 != 0 {
		t.Errorf("kept %d bytes, which splits a two-byte rune", len(lines[0]))
	}
}

// A line longer than the internal read buffer must still assemble correctly.
func TestSplitLines_LineExceedsReadBuffer(t *testing.T) {
	long := strings.Repeat("abcd", 40*1024) // 160 KB, well past the 32 KB buffer
	lines, drops := collect(t, long+"\ntail\n", MaxLineBytes)

	if len(lines) != 2 {
		t.Fatalf("got %d lines", len(lines))
	}
	if len(lines[0]) != MaxLineBytes {
		t.Errorf("kept %d bytes, want %d", len(lines[0]), MaxLineBytes)
	}
	if drops[0] != len(long)-MaxLineBytes {
		t.Errorf("dropped %d, want %d", drops[0], len(long)-MaxLineBytes)
	}
	if lines[1] != "tail" {
		t.Errorf("line after a huge line = %q", lines[1])
	}
}

func TestSplitLines_PropagatesEmitError(t *testing.T) {
	err := SplitLines(strings.NewReader("a\nb\n"), MaxLineBytes, func(string, int) error {
		return errSentinel
	})
	if !errors.Is(err, errSentinel) {
		t.Errorf("got %v, want %v", err, errSentinel)
	}
}

var errSentinel = errors.New("sentinel")
