package cache

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Abyssinia-Labs/latem/pkg/protocol"
)

// E_CACHE is "this run cannot have the cache it asked for"; E_DISK is "this
// machine cannot hold the build at all". Collapsing them would tell an
// operator to clear caches when the answer is to buy a disk.
func TestError_Formatting(t *testing.T) {
	cause := errors.New("daemon is not running")

	tests := []struct {
		name string
		err  *Error
		want []string
	}{
		{"message and cause", cacheErr(cause, "cache %q", "pnpm"), []string{"E_CACHE", "pnpm", "daemon"}},
		{"message only", cacheErr(nil, "cache %q", "pnpm"), []string{"E_CACHE", "pnpm"}},
		{"cause only", &Error{Code: protocol.ErrCache, Err: cause}, []string{"E_CACHE", "daemon"}},
		{"code only", &Error{Code: protocol.ErrDisk}, []string{"E_DISK"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.err.Error()
			for _, want := range tc.want {
				if !strings.Contains(got, want) {
					t.Errorf("Error() = %q, want it to mention %q", got, want)
				}
			}
			if strings.Contains(got, "%!") {
				t.Errorf("Error() = %q", got)
			}
		})
	}
}

func TestCodeOf(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want protocol.ErrorCode
	}{
		{"nil", nil, ""},
		{"a stranger's error", errors.New("something else"), ""},
		{"a cache error", cacheErr(nil, "x"), protocol.ErrCache},
		{"a disk error", diskErr("x"), protocol.ErrDisk},
		// Callers may compare against ErrLockTimeout directly, so it maps even
		// when it arrives unwrapped.
		{"a bare lock timeout", ErrLockTimeout, protocol.ErrCache},
		{"a wrapped lock timeout", errors.Join(ErrLockTimeout), protocol.ErrCache},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := CodeOf(tc.err); got != tc.want {
				t.Errorf("CodeOf = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestExecCommand(t *testing.T) {
	out, err := execCommand(context.Background(), "/bin/echo", "latem")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(out)) != "latem" {
		t.Errorf("output = %q", out)
	}

	if _, err := execCommand(context.Background(), "/nonexistent/binary"); err == nil {
		t.Error("a missing binary was reported as success")
	}
}

// A mirror with no FETCH_HEAD has never been fetched into. It should still be
// datable, from the directory itself.
func TestStaleMirrors_FallsBackToTheDirectoryTimestamp(t *testing.T) {
	root := t.TempDir()
	now := time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)
	l := newLocal(t, root, time.Second, func(o *Options) { o.Now = func() time.Time { return now } })

	dir := filepath.Join(root, "mirrors", "never-fetched.git")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	old := now.Add(-200 * 24 * time.Hour)
	if err := os.Chtimes(dir, old, old); err != nil {
		t.Fatal(err)
	}

	stale, err := l.StaleMirrors(StaleMirrorAge)
	if err != nil {
		t.Fatal(err)
	}
	if len(stale) != 1 || stale[0] != "never-fetched.git" {
		t.Errorf("StaleMirrors = %v", stale)
	}
}

// git gc failing on one mirror must be reported and must not stop the others:
// a corrupt mirror is exactly the case where the rest of the sweep matters.
func TestPrune_ReportsGitFailures(t *testing.T) {
	root := t.TempDir()
	rec := &recorder{
		reply: func(_ string, args []string) ([]byte, error) {
			for _, a := range args {
				if strings.Contains(a, "broken.git") {
					return []byte("fatal: not a git repository"), errors.New("exit status 128")
				}
			}
			return nil, nil
		},
	}
	l := newLocal(t, root, time.Second, func(o *Options) { o.Exec = rec.run })
	for _, name := range []string{"broken.git", "healthy.git"} {
		writeSized(t, filepath.Join(root, "mirrors", name, "FETCH_HEAD"), 10, time.Now())
	}

	report, err := l.Prune(context.Background(), PruneOptions{})
	if err == nil {
		t.Fatal("a failing git gc was swallowed")
	}
	if CodeOf(err) != protocol.ErrCache {
		t.Errorf("CodeOf(err) = %q", CodeOf(err))
	}
	if len(report.MirrorsCollected) != 1 || report.MirrorsCollected[0] != "healthy.git" {
		t.Errorf("MirrorsCollected = %v, want the healthy mirror collected anyway", report.MirrorsCollected)
	}
}

func TestPrune_ReportsUnparseableTartOutput(t *testing.T) {
	rec := &recorder{
		reply: func(_ string, _ []string) ([]byte, error) { return []byte("not json"), nil },
	}
	l := newLocal(t, t.TempDir(), time.Second, func(o *Options) {
		o.Exec = rec.run
		o.Runners = map[string]RunnerConfig{"ios": {Executor: ExecutorTart}}
	})

	if _, err := l.Prune(context.Background(), PruneOptions{}); err == nil {
		t.Fatal("unparseable tart output was treated as an empty VM list")
	}
}
