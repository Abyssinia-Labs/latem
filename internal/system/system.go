// Package system answers questions about the machine the agent runs on.
//
// It exists because `latem doctor` (RUNNER_SPEC §19) and service installation
// (§23) both need facts that only the host can supply — which tools are on
// PATH and at what version, whether FileVault is on, whether this user will
// have a session after a reboot — and because both need those facts on a
// machine that is missing most of them. Every probe here therefore reports a
// structured answer rather than an error, and routes external commands through
// an injectable [Runner], so the whole of `doctor` is testable on a machine
// with neither Docker nor Xcode installed.
//
// The package deliberately knows nothing about checks, severities, or
// remediation text. It reports what is true; deciding whether that is a
// failure, and what command fixes it, belongs to the CLI, where the operator's
// configuration is in scope.
//
// Nothing here requires root. Service installation writes a systemd *user*
// unit or a LaunchAgent, both under $HOME, because §23's whole argument is
// that a LaunchDaemon has no user session and therefore no keychain, and
// `codesign` fails in a way that points nowhere useful.
package system

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// DefaultTimeout bounds one probe command.
//
// The probes are all local and instant except the ones that touch a network
// (`git ls-remote`) or a daemon (`docker`), and those are exactly the ones an
// operator runs `doctor` to diagnose. A probe that hangs turns a diagnostic
// into another thing that needs diagnosing.
const DefaultTimeout = 20 * time.Second

// Runner runs an external command and returns its combined output.
//
// Combined rather than separated because every consumer here parses a version
// banner or a status line, and the tools involved are inconsistent about which
// stream they use: `defaults read` writes its "does not exist" to stderr,
// `tart --version` writes to stdout, and `security` uses both. A caller that
// had to know which would get it wrong on some machine.
type Runner func(ctx context.Context, name string, args ...string) ([]byte, error)

// CommandError is a command that ran and did not succeed, or could not run.
//
// It exists so probes can distinguish "the tool is absent" from "the tool said
// no", which are different diagnoses with different fixes — and so a test can
// construct either without an os/exec process. [ExitCode] is the accessor;
// callers should not type-assert.
type CommandError struct {
	// Name is argv[0]. The arguments are deliberately absent: a configured
	// secret command's arguments name vault paths (RUNNER_SPEC §15).
	Name string
	// ExitCode is -1 when the command never started.
	ExitCode int
	Output   []byte
	Err      error
}

func (e *CommandError) Error() string {
	if e.ExitCode < 0 {
		return fmt.Sprintf("system: %s did not run: %v", e.Name, e.Err)
	}
	return fmt.Sprintf("system: %s exited %d", e.Name, e.ExitCode)
}

func (e *CommandError) Unwrap() error { return e.Err }

// NotInstalled reports whether the program is simply absent from PATH.
//
// On a mixed fleet this is the common case and not a fault: `security` does
// not exist on Linux, `loginctl` does not exist on macOS, and `tart` does not
// exist on a machine with no Tart runner configured.
func (e *CommandError) NotInstalled() bool {
	return errors.Is(e.Err, exec.ErrNotFound) || errors.Is(e.Err, os.ErrNotExist)
}

// ExitCode reports the exit status carried by err, or -1 when the command
// never ran, or 0 when err is nil.
func ExitCode(err error) int {
	if err == nil {
		return 0
	}
	var ce *CommandError
	if errors.As(err, &ce) {
		return ce.ExitCode
	}
	return -1
}

// Output returns the combined output captured alongside err, if any.
//
// Several probes have to read a failing command's own words — `xcodebuild`
// says "Agreeing to the Xcode/iOS license requires admin privileges" on the
// exact exit code that also means "no Xcode installed" — so the output is part
// of the answer rather than debris.
func Output(err error) []byte {
	var ce *CommandError
	if errors.As(err, &ce) {
		return ce.Output
	}
	return nil
}

// ExecRunner is the default [Runner]: run the program, capture both streams.
func ExecRunner(ctx context.Context, name string, args ...string) ([]byte, error) {
	return execWithEnv(ctx, nil, name, args...)
}

// EnvRunner is [ExecRunner] with extra KEY=VALUE entries layered over the
// agent's own environment.
//
// It exists for one probe and is worth the API surface for it. `git ls-remote`
// against a repository whose credentials are missing will sit waiting for a
// username on a terminal that nobody is watching, and a diagnostic that hangs
// is worse than no diagnostic. GIT_TERMINAL_PROMPT=0 turns that into an
// immediate, reportable failure.
func EnvRunner(env []string) Runner {
	return func(ctx context.Context, name string, args ...string) ([]byte, error) {
		return execWithEnv(ctx, env, name, args...)
	}
}

func execWithEnv(ctx context.Context, env []string, name string, args ...string) ([]byte, error) {
	// #nosec G204 -- every call site in this package passes a literal program
	// name and literal arguments, or a path derived from the agent's own
	// config. No value crossing the trust boundary of RUNNER_SPEC §5 reaches
	// here, and argv is passed as a slice so nothing is shell-parsed (INV-3).
	cmd := exec.CommandContext(ctx, name, args...)
	if len(env) > 0 {
		cmd.Env = append(os.Environ(), env...)
	}
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err := cmd.Run()
	if err == nil {
		return buf.Bytes(), nil
	}
	ce := &CommandError{Name: name, ExitCode: -1, Output: buf.Bytes(), Err: err}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		ce.ExitCode = ee.ExitCode()
	}
	return buf.Bytes(), ce
}

// Options configures a [System]. The zero value probes the real machine.
type Options struct {
	// GOOS and GOARCH default to the compiled-in values. They are injectable
	// so the macOS and Linux halves of `doctor` can both be tested from one
	// machine, which is the only way either half gets tested at all.
	GOOS, GOARCH string
	// Home defaults to the user's home directory. It is what "~" expands to
	// and where a LaunchAgent or systemd user unit is written.
	Home string
	// User defaults to the current username, for `loginctl enable-linger`.
	User string
	// Run defaults to [ExecRunner], or to [EnvRunner] when Env is set.
	Run Runner
	// Env layers KEY=VALUE entries over the agent's environment for the
	// default runner. It is ignored when Run is set, because a caller
	// supplying a Runner owns its behaviour — the same convention
	// internal/secrets uses for its own Runner and Timeout.
	Env []string
	// LookPath defaults to exec.LookPath.
	LookPath func(string) (string, error)
	// Getenv defaults to os.Getenv.
	Getenv func(string) string
	// Getuid defaults to os.Getuid. launchd addresses a LaunchAgent by
	// `gui/<uid>/<label>`, so the number is part of every service command and
	// has to be substitutable for a test to assert on one.
	Getuid func() int
	// HTTP is the client used for the clock-skew probe. Nil builds one from
	// Timeout. A test substitutes one whose transport answers locally, so the
	// probe is exercised without a network.
	HTTP *http.Client
	// Timeout bounds one probe. Zero uses [DefaultTimeout].
	Timeout time.Duration
	// Root is put in front of the absolute system paths a toolchain probe
	// reads — /Applications, /usr/lib/jvm — so a test reads a fixture
	// rather than the machine running it. Empty is the real filesystem.
	Root string
}

// System probes one machine.
//
// It holds no mutable state and is safe for concurrent use; `doctor` runs a
// dozen probes and several of them shell out, so serialising them would make a
// diagnostic slower than the thing it diagnoses.
type System struct {
	goos, goarch string
	home         string
	username     string
	run          Runner
	lookPath     func(string) (string, error)
	getenv       func(string) string
	getuid       func() int
	httpClient   *http.Client
	timeout      time.Duration
	root         string
}

// New builds a System from opts, filling in the real machine for anything the
// caller left unset.
func New(opts Options) *System {
	s := &System{
		goos:       opts.GOOS,
		goarch:     opts.GOARCH,
		home:       opts.Home,
		username:   opts.User,
		run:        opts.Run,
		lookPath:   opts.LookPath,
		getenv:     opts.Getenv,
		getuid:     opts.Getuid,
		httpClient: opts.HTTP,
		timeout:    opts.Timeout,
		root:       opts.Root,
	}
	if s.goos == "" {
		s.goos = runtime.GOOS
	}
	if s.goarch == "" {
		s.goarch = runtime.GOARCH
	}
	if s.home == "" {
		s.home, _ = os.UserHomeDir()
	}
	if s.username == "" {
		if u, err := user.Current(); err == nil {
			s.username = u.Username
		}
	}
	if s.run == nil {
		s.run = ExecRunner
		if len(opts.Env) > 0 {
			s.run = EnvRunner(opts.Env)
		}
	}
	if s.lookPath == nil {
		s.lookPath = exec.LookPath
	}
	if s.getenv == nil {
		s.getenv = os.Getenv
	}
	if s.getuid == nil {
		s.getuid = os.Getuid
	}
	if s.timeout <= 0 {
		s.timeout = DefaultTimeout
	}
	return s
}

// OS is the operating system, in Go's spelling: "darwin", "linux".
func (s *System) OS() string { return s.goos }

// Arch is the architecture, in Go's spelling: "arm64", "amd64".
func (s *System) Arch() string { return s.goarch }

// Home is the user's home directory.
func (s *System) Home() string { return s.home }

// User is the username the agent runs as.
func (s *System) User() string { return s.username }

// IsMacOS reports whether this machine runs macOS.
func (s *System) IsMacOS() bool { return s.goos == "darwin" }

// IsLinux reports whether this machine runs Linux.
func (s *System) IsLinux() bool { return s.goos == "linux" }

// Getenv reads an environment variable through the injected lookup.
func (s *System) Getenv(key string) string { return s.getenv(key) }

// UID is the numeric user id the agent runs as.
func (s *System) UID() int { return s.getuid() }

// Exec runs a command with the probe timeout applied, returning its combined
// output. It is exported because `doctor` runs a few probes that belong to
// other packages' domains — `git ls-remote` against a configured repo — and
// they should be bounded and fakeable the same way these are.
func (s *System) Exec(ctx context.Context, name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	return s.run(ctx, name, args...)
}

// LookPath resolves a program on PATH.
func (s *System) LookPath(name string) (string, error) { return s.lookPath(name) }

// Expand replaces a leading "~" with the home directory.
//
// It is here rather than in a helper file because every path this package
// writes — a LaunchAgent, a systemd unit, a log destination — is one an
// operator wrote with a tilde in a config file, and launchd in particular does
// not expand one.
func (s *System) Expand(path string) string {
	switch {
	case path == "~":
		return s.home
	case strings.HasPrefix(path, "~/"):
		return filepath.Join(s.home, path[2:])
	default:
		return path
	}
}
