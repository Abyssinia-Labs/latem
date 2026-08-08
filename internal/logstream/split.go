package logstream

import (
	"bufio"
	"errors"
	"io"
	"strings"
	"unicode/utf8"
)

// MaxLineBytes is the ceiling on one log line (RUNNER_SPEC §16). Beyond it the
// line is truncated and the overflow counted.
//
// The cap exists because a build can emit a single line of arbitrary length —
// a minified bundle, a base64 blob, a stack trace with an embedded payload —
// and an unbounded line is an unbounded allocation driven by untrusted output.
const MaxLineBytes = 64 * 1024

// SplitLines reads r and calls emit once per line, with the count of bytes
// dropped to the length cap.
//
// Handles LF and CRLF, and emits a final unterminated line at EOF — a step
// that dies mid-write still gets its last words into the log, which is
// frequently the useful part.
//
// Truncation has one honest limitation worth stating: a secret straddling the
// cut point is only partly present in the kept prefix, so it will not match a
// registered form and the fragment is emitted. This matters only for lines
// over 64 KB, and a fragment of a credential is not usually a usable
// credential, but it is not nothing.
func SplitLines(r io.Reader, max int, emit func(line string, dropped int) error) error {
	if max <= 0 {
		max = MaxLineBytes
	}
	br := bufio.NewReaderSize(r, 32*1024)

	var line []byte
	dropped := 0

	flush := func() error {
		if dropped > 0 {
			line = trimToRuneBoundary(line)
		}
		s := strings.TrimSuffix(string(line), "\r")
		line = line[:0]
		d := dropped
		dropped = 0
		return emit(s, d)
	}

	for {
		chunk, err := br.ReadSlice('\n')

		if len(chunk) > 0 {
			data := chunk
			if data[len(data)-1] == '\n' {
				data = data[:len(data)-1]
			}
			if room := max - len(line); room > 0 {
				if len(data) <= room {
					line = append(line, data...)
				} else {
					line = append(line, data[:room]...)
					dropped += len(data) - room
				}
			} else {
				dropped += len(data)
			}
		}

		switch {
		case errors.Is(err, bufio.ErrBufferFull):
			// The line is longer than the read buffer; keep accumulating.
			continue

		case err == nil:
			if e := flush(); e != nil {
				return e
			}

		case errors.Is(err, io.EOF):
			if len(line) > 0 || dropped > 0 {
				return flush()
			}
			return nil

		default:
			return err
		}
	}
}

// trimToRuneBoundary drops a trailing partial UTF-8 sequence, so truncating a
// line never emits half a character.
func trimToRuneBoundary(b []byte) []byte {
	for len(b) > 0 {
		if r, size := utf8.DecodeLastRune(b); r != utf8.RuneError || size > 1 {
			return b
		}
		b = b[:len(b)-1]
	}
	return b
}
