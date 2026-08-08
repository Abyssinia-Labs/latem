package secrets

import (
	"context"
	"errors"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeRunner stands in for every credential store in the chain. It is why the
// keychain, pass, and command providers have real tests on a machine with no
// keychain, no password store, and no 1Password session.
type fakeRunner struct {
	mu    sync.Mutex
	calls [][]string
	fn    func(argv []string) ([]byte, error)
}

func (f *fakeRunner) Run(ctx context.Context, argv []string) ([]byte, error) {
	f.mu.Lock()
	f.calls = append(f.calls, slices.Clone(argv))
	fn := f.fn
	f.mu.Unlock()

	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if fn == nil {
		return nil, errors.New("fakeRunner: no behaviour configured")
	}
	return fn(argv)
}

func (f *fakeRunner) lastCall() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.calls) == 0 {
		return nil
	}
	return f.calls[len(f.calls)-1]
}

func answering(out string) *fakeRunner {
	return &fakeRunner{fn: func([]string) ([]byte, error) { return []byte(out), nil }}
}

func failing(err error) *fakeRunner {
	return &fakeRunner{fn: func([]string) ([]byte, error) { return nil, err }}
}

func exited(program string, code int) error {
	return &CommandError{Program: program, ExitCode: code, Err: errors.New("exit status")}
}

func missingBinary(program string) error {
	return &CommandError{Program: program, ExitCode: -1, Err: exec.ErrNotFound}
}

func timedOut(program string) error {
	return &CommandError{Program: program, ExitCode: -1, Err: context.DeadlineExceeded}
}

func requireBinary(t *testing.T, name string) {
	t.Helper()
	if _, err := exec.LookPath(name); err != nil {
		t.Skipf("%s is not on PATH", name)
	}
}

func TestExecRunner_CapturesStdout(t *testing.T) {
	requireBinary(t, "echo")

	out, err := ExecRunner{}.Run(t.Context(), []string{"echo", "hunter2"})
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != "hunter2\n" {
		t.Errorf("stdout = %q", out)
	}
}

// The chain must be able to tell "no such item" from "the tool broke", and the
// exit status is the only thing carrying that.
func TestExecRunner_ReportsExitCode(t *testing.T) {
	requireBinary(t, "false")

	_, err := ExecRunner{}.Run(t.Context(), []string{"false"})
	if err == nil {
		t.Fatal("a non-zero exit was reported as success")
	}
	var ce *CommandError
	if !errors.As(err, &ce) {
		t.Fatalf("err = %T, want *CommandError", err)
	}
	if ce.ExitCode != 1 {
		t.Errorf("ExitCode = %d, want 1", ce.ExitCode)
	}
	if ce.NotInstalled() || ce.TimedOut() {
		t.Error("a plain non-zero exit was classified as missing or timed out")
	}
}

// `security` does not exist on Linux and `pass` does not exist on most Macs.
// A provider whose binary is absent is not configured, not broken.
func TestExecRunner_ReportsAMissingBinary(t *testing.T) {
	_, err := ExecRunner{}.Run(t.Context(), []string{"latem-no-such-program-9f3a"})
	var ce *CommandError
	if !errors.As(err, &ce) {
		t.Fatalf("err = %v (%T), want *CommandError", err, err)
	}
	if !ce.NotInstalled() {
		t.Errorf("NotInstalled() = false for %v", err)
	}
}

// A tool waiting on a keychain prompt with no TTY would otherwise hang the run
// forever.
func TestExecRunner_TimesOut(t *testing.T) {
	requireBinary(t, "sleep")

	start := time.Now()
	_, err := ExecRunner{Timeout: 50 * time.Millisecond}.Run(t.Context(), []string{"sleep", "30"})
	elapsed := time.Since(start)

	var ce *CommandError
	if !errors.As(err, &ce) {
		t.Fatalf("err = %v (%T), want *CommandError", err, err)
	}
	if !ce.TimedOut() {
		t.Errorf("TimedOut() = false for %v", err)
	}
	if elapsed > 10*time.Second {
		t.Errorf("the timeout was not enforced: waited %v", elapsed)
	}
	if !strings.Contains(ce.Error(), "did not answer") {
		t.Errorf("Error() = %q, which does not say what happened", ce.Error())
	}
}

// A command configured to fetch MATCH_PASSWORD has no business being handed
// APP_STORE_CONNECT_KEY_P8.
func TestExecRunner_StripsOtherSecretsFromTheChildEnvironment(t *testing.T) {
	requireBinary(t, "env")

	r := ExecRunner{Environ: []string{
		"LATEM_SECRET_APP_STORE_CONNECT_KEY_P8=-----BEGIN PRIVATE KEY-----",
		"LATEM_SECRET_MATCH_PASSWORD=hunter2",
		"OP_SERVICE_ACCOUNT_TOKEN=needed-by-op",
	}}
	out, err := r.Run(t.Context(), []string{"env"})
	if err != nil {
		t.Fatal(err)
	}

	if strings.Contains(string(out), "LATEM_SECRET_") {
		t.Error("a LATEM_SECRET_* variable reached the child process")
	}
	if !strings.Contains(string(out), "OP_SERVICE_ACCOUNT_TOKEN=needed-by-op") {
		t.Error("the child lost the environment the tool needs to work")
	}
}

// Stderr is useful to an operator and unvouched-for by this package, so it is
// reachable only through Diagnostic and never through Error — Error is what
// reaches Update.ErrorMsg and therefore a broker (INV-4).
func TestExecRunner_KeepsStderrOutOfTheErrorString(t *testing.T) {
	requireBinary(t, "ls")

	_, err := ExecRunner{}.Run(t.Context(), []string{"ls", "/latem-no-such-path-9f3a"})
	var ce *CommandError
	if !errors.As(err, &ce) {
		t.Fatalf("err = %v (%T), want *CommandError", err, err)
	}
	if len(ce.Stderr) == 0 {
		t.Fatal("stderr was not captured, so `latem doctor` has nothing to print")
	}
	if strings.Contains(ce.Error(), string(ce.Stderr)) {
		t.Errorf("Error() = %q leaks the program's stderr", ce.Error())
	}
	if ce.Diagnostic() != string(ce.Stderr) {
		t.Error("Diagnostic() does not expose what was captured")
	}
}

func TestExecRunner_RefusesAnEmptyArgv(t *testing.T) {
	if _, err := (ExecRunner{}).Run(t.Context(), nil); err == nil {
		t.Error("an empty argv was accepted")
	}
}

func TestExecRunner_HonoursACancelledContext(t *testing.T) {
	requireBinary(t, "sleep")

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	_, err := ExecRunner{}.Run(ctx, []string{"sleep", "30"})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
}

func TestCommandError_ErrorStrings(t *testing.T) {
	tests := []struct {
		name string
		err  *CommandError
		want string
	}{
		{"timeout", &CommandError{Program: "op", ExitCode: -1, Err: context.DeadlineExceeded}, "op did not answer within the timeout"},
		{"missing", &CommandError{Program: "pass", ExitCode: -1, Err: exec.ErrNotFound}, "pass is not installed"},
		{"exit", &CommandError{Program: "security", ExitCode: 44, Err: errors.New("x")}, "security exited 44"},
		{"other", &CommandError{Program: "op", ExitCode: -1, Err: errors.New("broken pipe")}, "op: broken pipe"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.err.Error(); got != tc.want {
				t.Errorf("Error() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestClampStderr_IsBounded(t *testing.T) {
	got := clampStderr([]byte(strings.Repeat("x", maxStderr*2)))
	if len(got) != maxStderr {
		t.Errorf("len = %d, want %d", len(got), maxStderr)
	}
}
