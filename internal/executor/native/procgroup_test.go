package native

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/Abyssinia-Labs/latem/pkg/protocol"
)

// writeRecord puts a process group record in dir, as a crashed agent would have
// left it, and returns its path.
func writeRecord(t *testing.T, dir, name, content string) string {
	t.Helper()
	if err := os.MkdirAll(dir, envDirMode); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, GroupRecordPrefix+name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// groupRecords lists the process group records in a run's $LATEM_ENV directory.
func groupRecords(t *testing.T, dir string) []string {
	t.Helper()
	found, err := filepath.Glob(filepath.Join(dir, GroupRecordPrefix+"*"))
	if err != nil {
		t.Fatal(err)
	}
	return found
}

// A record is a number a reaper will signal by, so every way of not being a
// number has to end somewhere other than kill(2).
func TestReadGroupRecord(t *testing.T) {
	dir := t.TempDir()

	tests := map[string]struct {
		content string
		want    int
		wantErr error
	}{
		// The window between fork and [stepGroup.note]. Nothing to reap, and
		// nothing wrong either.
		"empty":            {content: "", want: 0},
		"whitespace only":  {content: " \n", want: 0},
		"a group":          {content: "4242\n", want: 4242},
		"no trailing line": {content: "4242", want: 4242},
		"not a number":     {content: "not-a-pid\n", wantErr: strconv.ErrSyntax},
		// Truncated at maxGroupRecordBytes, so it cannot parse — which is the
		// point of the cap, since the file lives where a job could write to it.
		"absurdly long": {content: strings.Repeat("9", 4096), wantErr: strconv.ErrRange},
		// The two numbers that must never reach kill(2).
		"zero":     {content: "0\n", wantErr: errRefusedPgid},
		"one":      {content: "1\n", wantErr: errRefusedPgid},
		"negative": {content: "-1\n", wantErr: errRefusedPgid},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			path := writeRecord(t, dir, strings.ReplaceAll(name, " ", "-"), tc.content)
			got, err := readGroupRecord(path)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("readGroupRecord = (%d, %v), want an error wrapping %v", got, err, tc.wantErr)
				}
				if got != 0 {
					t.Errorf("readGroupRecord returned pgid %d alongside an error; it must return nothing to signal", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("readGroupRecord: %v", err)
			}
			if got != tc.want {
				t.Errorf("readGroupRecord = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestReadGroupRecord_MissingFile(t *testing.T) {
	if _, err := readGroupRecord(filepath.Join(t.TempDir(), "pgroup-nope")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("readGroupRecord on a missing record = %v, want a not-exist error", err)
	}
}

// Everything in a $LATEM_ENV directory that is not a record file is left to the
// directory removal. A directory named like a record is the interesting case:
// os.ReadDir would happily hand it over, and reading it would be an error that
// says nothing useful.
func TestReapGroups_IgnoresWhatIsNotARecord(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, GroupRecordPrefix+"a-directory"), envDirMode); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, EnvFileName), []byte("KEY=value\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	writeRecord(t, dir, "unwritten", "")

	killed, err := reapGroups(dir, 0)
	if err != nil {
		t.Fatalf("reapGroups: %v", err)
	}
	if len(killed) != 0 {
		t.Errorf("reapGroups signalled %v", killed)
	}
}

func TestReapGroups_MissingDirectoryIsNotAnError(t *testing.T) {
	killed, err := reapGroups(filepath.Join(t.TempDir(), "gone"), 0)
	if err != nil || killed != nil {
		t.Errorf("reapGroups on a missing directory = (%v, %v), want (nil, nil)", killed, err)
	}
}

// A corrupt record is reported rather than swallowed — an operator whose reap
// found something it could not read should hear about it — but it never stops
// the rest of the directory being handled.
func TestReapGroups_ReportsACorruptRecord(t *testing.T) {
	dir := t.TempDir()
	writeRecord(t, dir, "corrupt", "not-a-pid\n")

	if _, err := reapGroups(dir, 0); err == nil {
		t.Fatal("reapGroups accepted a record that is not a number")
	}
}

// The nil receiver is not reachable today; it is the contract that keeps
// [runEnv.run]'s single deferred release correct if it ever becomes so.
func TestStepGroup_NilIsSafe(t *testing.T) {
	var g *stepGroup
	if err := g.note(1234); err != nil {
		t.Errorf("note on a nil group: %v", err)
	}
	g.release()
}

// A pgid of 0 or 1 is never written, so a platform without process groups
// ([processGroupOf] returns 0 there) leaves a record that reaps nothing rather
// than one that names launchd.
func TestStepGroup_NoteRefusesTheNumbersThatAreNotGroups(t *testing.T) {
	dir := t.TempDir()
	f, err := os.CreateTemp(dir, GroupRecordPrefix)
	if err != nil {
		t.Fatal(err)
	}
	g := &stepGroup{file: f, path: f.Name()}
	t.Cleanup(g.release)

	for _, pgid := range []int{0, 1, -1} {
		if err := g.note(pgid); err != nil {
			t.Fatalf("note(%d): %v", pgid, err)
		}
	}
	got, err := readGroupRecord(g.path)
	if err != nil || got != 0 {
		t.Errorf("after note(0), note(1) and note(-1) the record reads (%d, %v), want (0, nil)", got, err)
	}
}

// A step whose group cannot be recorded is a step whose group a crash would
// leak, and the directory it could not be recorded in is the same one $LATEM_ENV
// lives in — so this fails the step rather than running one the agent cannot
// account for.
func TestNewStepGroup_ADirectoryItCannotWriteTo(t *testing.T) {
	_, err := newStepGroup(filepath.Join(t.TempDir(), "never-created"), &exec.Cmd{})
	if err == nil {
		t.Fatal("newStepGroup succeeded with nowhere to put the record")
	}
	if got := CodeOf(err); got != protocol.ErrExecutor {
		t.Errorf("code = %q, want %q", got, protocol.ErrExecutor)
	}
}

func TestStepGroup_NoteReportsAWriteFailure(t *testing.T) {
	dir := t.TempDir()
	f, err := os.CreateTemp(dir, GroupRecordPrefix)
	if err != nil {
		t.Fatal(err)
	}
	g := &stepGroup{file: f, path: f.Name()}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	if err := g.note(4242); err == nil {
		t.Fatal("note reported success writing to a closed file")
	} else if got := CodeOf(err); got != protocol.ErrExecutor {
		t.Errorf("code = %q, want %q", got, protocol.ErrExecutor)
	}
}

func TestStepGroup_ReleaseRemovesTheRecord(t *testing.T) {
	dir := t.TempDir()
	f, err := os.CreateTemp(dir, GroupRecordPrefix)
	if err != nil {
		t.Fatal(err)
	}
	g := &stepGroup{file: f, path: f.Name()}
	g.release()

	if recs := groupRecords(t, dir); len(recs) != 0 {
		t.Errorf("release left %v behind; a record that outlives its step is one a later reap would act on", recs)
	}
	// Idempotent, because [runEnv.run] defers it on paths that may already have
	// gone through a Close.
	g.release()
}
