package system

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"sync"
)

// fakeCmd is one canned answer from [fakeRunner].
type fakeCmd struct {
	out  string
	exit int
	// missing makes the program look absent from PATH, which several probes
	// treat differently from a program that ran and said no.
	missing bool
}

// fakeRunner answers commands from a table.
//
// It exists so every probe in this package is exercised on a machine that has
// none of the tools involved, which is the whole reason [Runner] is injectable:
// the CI runner for this repository has no Xcode, no Tart, and no FileVault,
// and those are precisely the checks most worth having tests for.
type fakeRunner struct {
	mu    sync.Mutex
	cmds  map[string]fakeCmd
	calls []string
}

func newFakeRunner(cmds map[string]fakeCmd) *fakeRunner {
	if cmds == nil {
		cmds = map[string]fakeCmd{}
	}
	return &fakeRunner{cmds: cmds}
}

func (f *fakeRunner) run(_ context.Context, name string, args ...string) ([]byte, error) {
	key := strings.Join(append([]string{name}, args...), " ")
	f.mu.Lock()
	f.calls = append(f.calls, key)
	cmd, ok := f.cmds[key]
	f.mu.Unlock()

	if !ok {
		return nil, &CommandError{Name: name, ExitCode: -1, Err: exec.ErrNotFound}
	}
	if cmd.missing {
		return nil, &CommandError{Name: name, ExitCode: -1, Err: exec.ErrNotFound}
	}
	if cmd.exit != 0 {
		return []byte(cmd.out), &CommandError{Name: name, ExitCode: cmd.exit, Output: []byte(cmd.out)}
	}
	return []byte(cmd.out), nil
}

func (f *fakeRunner) called(key string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.calls {
		if c == key {
			return true
		}
	}
	return false
}

// fakePath resolves only the named programs.
func fakePath(names ...string) func(string) (string, error) {
	set := make(map[string]bool, len(names))
	for _, n := range names {
		set[n] = true
	}
	return func(name string) (string, error) {
		if set[name] {
			return "/usr/bin/" + name, nil
		}
		return "", &exec.Error{Name: name, Err: exec.ErrNotFound}
	}
}

// testSystem builds a System with everything injected and nothing real.
func testSystem(goos string, r *fakeRunner, path func(string) (string, error), home string) *System {
	if path == nil {
		path = fakePath()
	}
	if home == "" {
		home = "/home/tester"
	}
	return New(Options{
		GOOS:     goos,
		GOARCH:   "arm64",
		Home:     home,
		User:     "tester",
		Run:      r.run,
		LookPath: path,
		Getenv:   func(string) string { return "" },
		Getuid:   func() int { return 501 },
	})
}

func mustWrite(path, content string, mode os.FileMode) error {
	return os.WriteFile(path, []byte(content), mode)
}
