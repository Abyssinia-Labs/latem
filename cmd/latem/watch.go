package main

import (
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/Abyssinia-Labs/latem/internal/store"
	"github.com/Abyssinia-Labs/latem/pkg/protocol"
)

// pollInterval is how often a watching client re-reads a run's row and its
// log. It is short because the thing on the other end is a person waiting, and
// cheap because both reads are local: a sqlite point query and a read from the
// current offset of one file.
const pollInterval = 250 * time.Millisecond

// watchRun blocks until a run reaches a terminal state, optionally streaming
// its log, and returns the exit status the run earned.
//
// # Why the log comes off disk rather than off the socket
//
// The obvious design streams frames from the agent. It is not available, and
// the reason is INV-2 rather than effort: the inbound command set is closed at
// run and cancel, and a third kind — even a read-only one for tailing — is a
// change to §5 requiring a written threat model, not a feature. So `latem
// logs -f` reads the file the log pipeline is writing, which has already
// passed the redactor (INV-4) and is the same bytes a broker would have seen.
// A finished run and a running one are then read identically, which is worth
// more than the latency it costs.
func watchRun(cmd *cobra.Command, st *state, runID string, follow bool) error {
	ctx := cmd.Context()

	var tail *logTail
	if follow {
		tail = newLogTail(st.cfg.Paths.Logs, runID)
		defer tail.close()
	}

	for {
		r, err := st.db.Get(ctx, runID)
		if err != nil {
			return err
		}
		if tail != nil {
			tail.pump(cmd.OutOrStdout())
		}
		if r.State.IsTerminal() {
			if tail != nil {
				// One last read: the run finished between the previous pump
				// and this query, and the last few lines of a build are the
				// ones somebody is waiting for.
				tail.pump(cmd.OutOrStdout())
			}
			return terminalStatus(cmd, r)
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(pollInterval):
		}
	}
}

// terminalStatus prints the outcome and maps it to an exit code.
//
// Only a succeeded run exits 0. A cancelled or rejected run is not a crash,
// but it is not a build either, and a script that treats "somebody pressed
// Stop" as success will deploy something nobody built.
func terminalStatus(cmd *cobra.Command, r store.Run) error {
	out := cmd.ErrOrStderr()
	if r.State == protocol.StateSucceeded {
		printf(out, "%s succeeded in %s\n", r.ID, runDuration(r))
		return nil
	}

	msg := fmt.Sprintf("%s %s", r.ID, r.State)
	if r.ErrorCode != "" {
		msg += " (" + string(r.ErrorCode) + ")"
	}
	if r.ExitCode != nil {
		msg += fmt.Sprintf(" exit %d", *r.ExitCode)
	}
	if r.ErrorMsg != "" {
		msg += ": " + r.ErrorMsg
	}
	if r.ErrorCode.Retryable() {
		msg += "\n\nthis one is retryable — the machine, not the commit, is what failed"
	}
	return &ExitError{Code: ExitFailure, Err: errors.New(msg)}
}

// runDuration is how long a run took, or how long it has been going.
func runDuration(r store.Run) string {
	if r.StartedAt == nil {
		return "-"
	}
	end := time.Now()
	if r.FinishedAt != nil {
		end = *r.FinishedAt
	}
	return end.Sub(*r.StartedAt).Round(time.Second).String()
}

// logTail streams a run's log file from wherever it last stopped.
//
// It reopens on every pump until it succeeds, because the file does not exist
// until the run reaches preparing — a client that opened once and gave up
// would show nothing for exactly the runs that waited in the queue.
type logTail struct {
	path   string
	f      *os.File
	offset int64
	buf    []byte
}

func newLogTail(dir, runID string) *logTail {
	return &logTail{path: filepath.Join(dir, runID+".log"), buf: make([]byte, 32*1024)}
}

// pump copies whatever has been appended since the last call.
func (t *logTail) pump(w io.Writer) {
	if t.f == nil {
		// #nosec G304 -- path is <logs>/<run-id>.log, and the id was validated
		// by protocol.ValidateID before this command reached here.
		f, err := os.Open(t.path)
		if err != nil {
			return // not started yet, or retained away; try again next tick
		}
		t.f = f
		if _, err := t.f.Seek(t.offset, io.SeekStart); err != nil {
			return
		}
	}
	for {
		n, err := t.f.Read(t.buf)
		if n > 0 {
			t.offset += int64(n)
			_, _ = w.Write(t.buf[:n])
		}
		if err != nil || n == 0 {
			return
		}
	}
}

// seekEnd starts the tail at the end of what is already there, for `latem logs
// -f` after the existing content has been printed once.
func (t *logTail) seekEnd() {
	info, err := os.Stat(t.path)
	if err != nil {
		return
	}
	t.offset = info.Size()
}

func (t *logTail) close() {
	if t.f != nil {
		_ = t.f.Close()
		t.f = nil
	}
}

// readLog returns a finished run's log, transparently decompressing the gzip
// the daily sweep leaves behind after 24 hours (§16).
//
// The two names are one log. A client that only knew about `.log` would report
// "no log" for every build older than a day, which is the same answer it gives
// for a run that never started — and those need to look different.
func readLog(dir, runID string) (io.ReadCloser, error) {
	plain := filepath.Join(dir, runID+".log")
	// #nosec G304 -- <logs>/<validated run id>.log; see [logTail.pump].
	if f, err := os.Open(plain); err == nil {
		return f, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}

	// #nosec G304 -- same path with the sweep's .gz suffix.
	f, err := os.Open(plain + ".gz")
	if err != nil {
		return nil, err
	}
	zr, err := gzip.NewReader(f)
	if err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("reading %s.gz: %w", plain, err)
	}
	return &gzReadCloser{zr: zr, f: f}, nil
}

type gzReadCloser struct {
	zr *gzip.Reader
	f  *os.File
}

func (g *gzReadCloser) Read(p []byte) (int, error) { return g.zr.Read(p) }

func (g *gzReadCloser) Close() error {
	err := g.zr.Close()
	if cerr := g.f.Close(); err == nil {
		err = cerr
	}
	return err
}

// lastLines returns the final n lines of s, for `latem logs --tail`.
func lastLines(s string, n int) string {
	if n <= 0 {
		return s
	}
	lines := strings.Split(strings.TrimSuffix(s, "\n"), "\n")
	if len(lines) <= n {
		return s
	}
	return strings.Join(lines[len(lines)-n:], "\n") + "\n"
}

// waitForRun is `latem logs -f`'s loop: stream until the run stops.
func waitForRun(ctx context.Context, st *state, runID string, tail *logTail, w io.Writer) error {
	for {
		r, err := st.db.Get(ctx, runID)
		if err != nil {
			return err
		}
		tail.pump(w)
		if r.State.IsTerminal() {
			tail.pump(w)
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(pollInterval):
		}
	}
}
