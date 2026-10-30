package secrets

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// DefaultTimeout bounds one external secret command.
//
// The wait is bounded because every tool in this chain can block on a human:
// `security` prompts to unlock a keychain, `op` waits for a Touch ID
// confirmation, `pass` waits for a GPG passphrase. An agent running as a
// service has no TTY to answer with, so the prompt never gets an answer and
// the process sits there. A build that hangs forever at secret resolution is
// harder to diagnose than one that fails in thirty seconds naming the program
// it was waiting for.
const DefaultTimeout = 30 * time.Second

// maxStderr caps the diagnostic captured from an external program. It exists
// so a tool that decides to dump a megabyte of debug output does not make the
// failure path allocate proportionally to how badly it is behaving.
const maxStderr = 8 << 10

// Runner executes an external program and returns its output.
//
// It is an interface for one reason: the chain has to be testable without a
// real Keychain, a real `pass` store, or a real 1Password session. Every test
// of the keychain, pass, and command providers substitutes a fake here and
// exercises the found, not-found, not-installed, non-zero-exit, and timeout
// paths without installing anything.
//
// Stderr is returned on success as well as on failure, which only the keychain
// provider uses: `security -g` prints the item's attributes to stdout and the
// password itself to stderr, and it is the only invocation that distinguishes a
// hex-encoded value from a literal one. See [KeychainProvider.Resolve].
//
// The argument is an argv slice, and there is no overload taking a command
// string. That is where INV-3 lives: a signature that cannot express
// `sh -c "…"` cannot be talked into it later.
type Runner interface {
	Run(ctx context.Context, argv []string) (stdout, stderr []byte, err error)
}

// ExecRunner is the production [Runner].
type ExecRunner struct {
	// Timeout bounds one execution. Zero uses [DefaultTimeout].
	Timeout time.Duration
	// Environ is the environment handed to the child, as os.Environ returns.
	// Nil uses the agent's own.
	Environ []string
}

// Run executes argv and returns its standard output and standard error.
//
// The child inherits the agent's environment because the tools here need it —
// `pass` reads GNUPGHOME and HOME, `op` reads OP_SERVICE_ACCOUNT_TOKEN — with
// one subtraction: every LATEM_SECRET_* variable is removed first. A command
// configured to fetch MATCH_PASSWORD has no business being handed
// APP_STORE_CONNECT_KEY_P8, and the cheapest way to guarantee it never leaks
// one secret into the process that fetches another is to not pass it.
func (r ExecRunner) Run(ctx context.Context, argv []string) ([]byte, []byte, error) {
	return r.run(ctx, argv, nil)
}

// RunInput is [ExecRunner.Run] with stdin. It exists for writing a secret:
// `security -i` reads the command that carries the value from stdin, which is
// the one way to hand it a value that never appears in a process listing.
func (r ExecRunner) RunInput(ctx context.Context, argv []string, stdin []byte) ([]byte, []byte, error) {
	return r.run(ctx, argv, stdin)
}

func (r ExecRunner) run(ctx context.Context, argv []string, stdin []byte) ([]byte, []byte, error) {
	if len(argv) == 0 {
		return nil, nil, errors.New("secrets: cannot run an empty argv")
	}

	timeout := r.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var stdout, stderr bytes.Buffer

	// #nosec G204 -- argv never contains a broker-supplied value. Element 0 and
	// the flag elements are constants in this package; the only variable parts
	// are a secret NAME (validated against namePattern, so it cannot begin with
	// `-` and cannot contain a separator) and words parsed by ParseCommand from
	// the operator's own ~/.latem/config.toml. Nothing is concatenated and no
	// shell is involved, which is the whole of INV-3: the alternative this
	// replaces is exec.Command("sh", "-c", configuredString).
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	// A nil Stdin gives the child the null device. A tool that decides to
	// prompt therefore reads EOF and fails, rather than blocking until the
	// timeout with no indication of what it wanted.
	cmd.Stdin = nil
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	cmd.Env = withoutAgentSecrets(r.environ())

	runErr := cmd.Run()

	// Checked before the exit status because a killed process also reports a
	// non-zero exit, and "op exited 1" would hide "op never answered".
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, nil, &CommandError{
			Program:  argv[0],
			ExitCode: -1,
			Stderr:   clampStderr(stderr.Bytes()),
			Err:      ctxErr,
		}
	}
	if runErr != nil {
		code := -1
		var exitErr *exec.ExitError
		if errors.As(runErr, &exitErr) {
			code = exitErr.ExitCode()
		}
		return nil, nil, &CommandError{
			Program:  argv[0],
			ExitCode: code,
			Stderr:   clampStderr(stderr.Bytes()),
			Err:      runErr,
		}
	}

	// Unclamped, unlike the failure paths above. There the stderr is a
	// diagnostic from a tool that misbehaved and [maxStderr] bounds how badly
	// it can make us allocate. Here it is a value the caller asked for:
	// `security -g` prints a secret there, and truncating it at 8 KiB would
	// hand back a silently corrupted key rather than an error. A 1.6 KiB PEM
	// already renders to 5.7 KiB, because -g prints the bytes as hex *and*
	// again as an escaped string.
	return stdout.Bytes(), stderr.Bytes(), nil
}

func (r ExecRunner) environ() []string {
	if r.Environ != nil {
		return r.Environ
	}
	return os.Environ()
}

func withoutAgentSecrets(environ []string) []string {
	out := make([]string, 0, len(environ))
	for _, kv := range environ {
		if strings.HasPrefix(kv, EnvPrefix) {
			continue
		}
		out = append(out, kv)
	}
	return out
}

func clampStderr(b []byte) []byte {
	if len(b) > maxStderr {
		b = b[:maxStderr]
	}
	return bytes.TrimSpace(b)
}

// CommandError is an external secret program that did not produce a value.
type CommandError struct {
	// Program is argv[0] — "security", "pass", "op". Never the full argv,
	// because a configured command's arguments name vault paths.
	Program string
	// ExitCode is -1 when the program never ran or was killed.
	ExitCode int
	// Stderr is the program's own diagnostics, captured for LOCAL display
	// only and deliberately absent from [CommandError.Error].
	//
	// It is genuinely useful — "op: you are not currently signed in" is the
	// whole answer to a support question — and it is also arbitrary output
	// from a third-party binary that this package cannot vouch for. Error()
	// is what ends up in Update.ErrorMsg and therefore on a broker (INV-4),
	// so the useful-but-unvouched half is reachable only through
	// [CommandError.Diagnostic], which `latem doctor` prints to a terminal
	// and nothing else calls.
	Stderr []byte
	// Err is the underlying os/exec or context error.
	Err error
}

func (e *CommandError) Error() string {
	switch {
	case e.TimedOut():
		return fmt.Sprintf("%s did not answer within the timeout", e.Program)
	case e.NotInstalled():
		return fmt.Sprintf("%s is not installed", e.Program)
	case e.ExitCode >= 0:
		return fmt.Sprintf("%s exited %d", e.Program, e.ExitCode)
	}
	return fmt.Sprintf("%s: %v", e.Program, e.Err)
}

func (e *CommandError) Unwrap() error { return e.Err }

// Diagnostic returns the program's own stderr, for printing to a local
// terminal. It is never part of an error string; see [CommandError.Stderr].
func (e *CommandError) Diagnostic() string { return string(e.Stderr) }

// NotInstalled reports whether the program is simply absent from PATH, which
// the chain treats as "this provider is not configured on this machine" rather
// than as a failure — `security` does not exist on Linux and `pass` does not
// exist on most Macs.
func (e *CommandError) NotInstalled() bool { return errors.Is(e.Err, exec.ErrNotFound) }

// TimedOut reports whether the program was killed for exceeding its deadline.
func (e *CommandError) TimedOut() bool { return errors.Is(e.Err, context.DeadlineExceeded) }

// exitCodeOf returns the process exit status of err, or -1.
func exitCodeOf(err error) int {
	var ce *CommandError
	if errors.As(err, &ce) {
		return ce.ExitCode
	}
	return -1
}

// notInstalled reports whether err is a missing binary.
func notInstalled(err error) bool {
	var ce *CommandError
	if errors.As(err, &ce) {
		return ce.NotInstalled()
	}
	return false
}
