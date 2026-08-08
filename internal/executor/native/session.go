package native

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/Abyssinia-Labs/latem/pkg/protocol"
)

// ProbeTimeout bounds one host probe.
//
// Every program these probes call can block on a human. `security` raises a
// password dialog for a locked keychain and waits for it; `launchctl` is fast
// but talks to a daemon that can be wedged. An agent running as a service has
// no one to answer with, so a probe that can hang is a preflight that can hang,
// and a preflight that hangs takes the runner offline without ever saying why.
const ProbeTimeout = 5 * time.Second

// Runner executes an external program and returns its standard output.
//
// It is an interface for one reason: the two conditions §13.3 and §23 call the
// most common install failures are, by their nature, conditions this machine is
// not in. A locked keychain and a LaunchDaemon context cannot be produced in a
// test, so every branch that matters would otherwise be reachable only by hand
// on somebody's misconfigured Mac — which is to say, never.
//
// The argument is an argv slice and there is no overload taking a command
// string, for the same reason secrets.Runner does not have one: a signature
// that cannot express `sh -c "…"` cannot be talked into it later (INV-3).
type Runner interface {
	Run(ctx context.Context, argv []string) (stdout []byte, err error)
}

// ExecRunner is the production [Runner]. The zero value is usable.
type ExecRunner struct {
	// Timeout bounds one execution. Zero uses [ProbeTimeout].
	Timeout time.Duration
}

var _ Runner = ExecRunner{}

// Run executes argv and returns its standard output.
//
// A timeout is reported as an error wrapping [context.DeadlineExceeded], which
// is load-bearing: for the keychain probe, "did not answer" is not an
// inconclusive result, it is the diagnosis. See [CheckKeychain].
func (r ExecRunner) Run(ctx context.Context, argv []string) ([]byte, error) {
	if len(argv) == 0 {
		return nil, errors.New("native: cannot run an empty argv")
	}
	timeout := r.Timeout
	if timeout <= 0 {
		timeout = ProbeTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var stdout, stderr bytes.Buffer
	// #nosec G204 -- argv is a slice built entirely in this package: element 0
	// is the literal "launchctl" or "security", the flags are literals, and the
	// only variable element is a keychain path from the agent's own config.
	// Nothing is broker-supplied and no shell is involved (INV-3).
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	// A nil Stdin gives the child the null device, so a tool that decides to
	// prompt on the terminal reads EOF and fails instead of blocking.
	cmd.Stdin = nil

	runErr := cmd.Run()
	// Checked before the exit status, because a killed process also reports a
	// non-zero exit and "security exited 1" would hide "security never
	// answered" — the two mean different things here.
	if ctxErr := ctx.Err(); ctxErr != nil {
		return stdout.Bytes(), fmt.Errorf("%s: %w", argv[0], ctxErr)
	}
	if runErr != nil {
		return stdout.Bytes(), &probeError{program: argv[0], stderr: firstLine(stderr.String()), err: runErr}
	}
	return stdout.Bytes(), nil
}

// probeError is an external probe that did not answer cleanly. Its message
// carries the tool's own first line of stderr, which is the whole diagnosis for
// something like "User interaction is not allowed."
type probeError struct {
	program string
	stderr  string
	err     error
}

func (e *probeError) Error() string {
	if e.stderr != "" {
		return fmt.Sprintf("%s: %v: %s", e.program, e.err, e.stderr)
	}
	return fmt.Sprintf("%s: %v", e.program, e.err)
}

func (e *probeError) Unwrap() error { return e.err }

// SessionKind is the launchd domain the agent process is running in, as
// `launchctl managername` reports it.
type SessionKind string

// The four managers launchd reports, plus the answer for a machine that could
// not be asked.
const (
	// SessionAqua is a GUI login session. This is the only context §13.3
	// supports: a LaunchAgent under an auto-logged-in user, with a real
	// security session and therefore a usable login keychain.
	SessionAqua SessionKind = "Aqua"
	// SessionBackground is a per-user launchd domain with no GUI session — the
	// state of a user who is enabled for lingering but not logged in at the
	// window server.
	SessionBackground SessionKind = "Background"
	// SessionStandardIO is an interactive login without a window server: an
	// ssh session, most often an operator running `latem agent` by hand.
	SessionStandardIO SessionKind = "StandardIO"
	// SessionSystem is the LaunchDaemon domain. This is the failure §13.3 and
	// §23 name.
	SessionSystem SessionKind = "System"
	// SessionUnknown means launchctl could not be asked or said something this
	// package does not recognise. It is not treated as a failure: refusing to
	// run because a diagnostic did not work would be worse than the thing the
	// diagnostic was looking for.
	SessionUnknown SessionKind = "unknown"
)

// Session is the launchd context the agent is running in.
type Session struct {
	// Kind is the classified manager.
	Kind SessionKind
	// Manager is `launchctl managername` verbatim, empty if it could not be
	// read. Kept so an unrecognised value reaches a human unaltered instead of
	// collapsing into "unknown".
	Manager string
	// UID is the effective uid the agent runs as. Zero is root, which on a
	// Mac mini runner means a LaunchDaemon whatever launchctl says.
	UID int
	// Err is why the probe failed, if it did.
	Err error
}

// CanSignCode reports whether this context has the user session `codesign`
// needs (§23).
//
// Only an Aqua session qualifies. A daemon has no security session at all; a
// Background or StandardIO context has one that cannot present the unlock
// dialog the Security framework wants, so signing fails with
// errSecInteractionNotAllowed — an error whose text mentions neither launchd,
// nor sessions, nor the keychain the operator would need to unlock.
func (s Session) CanSignCode() bool { return s.Kind == SessionAqua }

// Explain returns a one-line diagnosis suitable for `latem doctor` or a log.
//
// It names the fix rather than the symptom. An operator reading "signing will
// fail" learns nothing they did not already know from the failing build; an
// operator reading "this is a LaunchDaemon, move the plist to
// ~/Library/LaunchAgents" is done in five minutes.
func (s Session) Explain() string {
	switch {
	case s.UID == 0:
		return "the agent is running as root, which means a LaunchDaemon; codesign needs a LaunchAgent " +
			"in a logged-in user's session (§23), so install the plist to ~/Library/LaunchAgents and " +
			"remove it from /Library/LaunchDaemons"
	case s.Kind == SessionSystem:
		return "the agent is running in launchd's System domain, which is a LaunchDaemon; codesign needs " +
			"a LaunchAgent in a logged-in user's session (§23), so install the plist to " +
			"~/Library/LaunchAgents and remove it from /Library/LaunchDaemons"
	case s.Kind == SessionAqua:
		return "the agent is running in a GUI login session, which is what codesign needs (§23)"
	case s.Kind == SessionBackground:
		return "the agent is running in launchd's Background domain: the user is not logged in at the " +
			"window server, so there is no security session and codesign will fail. Enable auto-login " +
			"for this user (§23) and check that FileVault is not holding the machine at the pre-boot screen"
	case s.Kind == SessionStandardIO:
		return "the agent was started from a terminal or an ssh session rather than by launchd. Steps will " +
			"run, but codesign needs a GUI login session (§23); install the LaunchAgent before " +
			"expecting a signed build"
	case s.Err != nil:
		return fmt.Sprintf("launchd session context could not be determined (%v); "+
			"if signing fails, check that the agent runs as a LaunchAgent under an auto-login user (§23)", s.Err)
	}
	return fmt.Sprintf("launchd reported an unrecognised manager %q; "+
		"codesign needs a LaunchAgent in a logged-in user's session (§23)", s.Manager)
}

// launchctlManagerName is the argv that answers the question. `managername`
// prints one word and exits, needs no privilege, and reads no user data.
var launchctlManagerName = []string{"launchctl", "managername"}

// DetectSession reports which launchd domain the agent is running in.
//
// This is the single most common macOS install failure (§13.3), so it is a
// plain function over an injectable [Runner] rather than a method: `latem
// doctor` reports exactly what [Executor.Preflight] acts on, and the four
// branches are testable on a machine that is in none of them.
//
// The uid check comes first and overrides launchctl. A process running as root
// on a runner is a LaunchDaemon regardless of what domain it claims, and that
// answer needs no subprocess — which matters because the daemon case is
// precisely the one where the machine is least likely to behave normally.
func DetectSession(ctx context.Context, r Runner, uid int) Session {
	s := Session{UID: uid}
	if uid == 0 {
		s.Kind = SessionSystem
		return s
	}
	if r == nil {
		r = ExecRunner{}
	}

	out, err := r.Run(ctx, launchctlManagerName)
	if err != nil {
		s.Kind, s.Err = SessionUnknown, err
		return s
	}
	s.Manager = strings.TrimSpace(string(out))
	switch s.Manager {
	case string(SessionAqua):
		s.Kind = SessionAqua
	case string(SessionBackground):
		s.Kind = SessionBackground
	case string(SessionStandardIO):
		s.Kind = SessionStandardIO
	case string(SessionSystem):
		s.Kind = SessionSystem
	default:
		s.Kind = SessionUnknown
	}
	return s
}

// CheckSession turns [DetectSession] into a preflight verdict.
//
// A System domain always fails: it cannot be fixed by waiting, retrying, or
// unlocking anything, and a runner that keeps accepting iOS work in a daemon
// context produces one identical, unexplained signing failure per job until
// somebody reads §23.
//
// requireGUI decides the rest. An operator running `latem agent` from a
// terminal to build an Android app is in a StandardIO session and is perfectly
// fine; the same session is a broken install for a signing runner. Since only
// the operator knows which they are, the strict answer is opt-in and the
// permissive one still reports what it saw through [Session.Explain].
func CheckSession(ctx context.Context, r Runner, uid int, requireGUI bool) (Session, error) {
	s := DetectSession(ctx, r, uid)
	if s.Kind == SessionSystem {
		return s, codeErr(protocol.ErrExecutor, "native executor: %s", s.Explain())
	}
	if requireGUI && !s.CanSignCode() {
		return s, codeErr(protocol.ErrExecutor, "native executor: %s", s.Explain())
	}
	return s, nil
}

// securityShowKeychainInfo is the probe argv. `show-keychain-info` reads the
// keychain's settings, which requires it to be unlocked, so its exit status is
// the lock state. An empty keychain path means the default keychain.
func securityShowKeychainInfo(keychain string) []string {
	argv := []string{"security", "show-keychain-info"}
	if keychain != "" {
		argv = append(argv, keychain)
	}
	return argv
}

// CheckKeychain reports whether the keychain is unlocked (§13.3).
//
// A locked keychain is the normal state of a Mac that rebooted, and it is the
// second of the two conditions that turn `codesign` into an error pointing
// nowhere. The remedy is a step or an operator running `security
// unlock-keychain` before the signing step; this function is what tells them
// which of the two problems they have.
//
// # Why this is opt-in
//
// There is no way to ask macOS whether a keychain is locked without also asking
// for it to be unlocked. `security show-keychain-info` on a locked keychain
// raises a SecurityAgent password dialog and blocks on it — measured, not
// assumed — and the dialog outlives the process that asked for it. §20 calls
// Preflight every five minutes, so a machine whose keychain is locked would
// accumulate a password dialog per five minutes forever, on the screen of a
// machine nobody is sitting at. That is a worse failure than the one being
// diagnosed, which is why [Options.CheckKeychain] defaults to off and `latem
// doctor` — one shot, with a human present — is the natural caller.
//
// A timeout is therefore a result, not an inconclusive probe: it means the
// dialog appeared and nothing answered it, which is exactly "locked, and this
// machine cannot unlock it unattended".
func CheckKeychain(ctx context.Context, r Runner, keychain string) error {
	if r == nil {
		r = ExecRunner{}
	}
	_, err := r.Run(ctx, securityShowKeychainInfo(keychain))
	if err == nil {
		return nil
	}

	name := keychain
	if name == "" {
		name = "the default keychain"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return wrapErr(protocol.ErrExecutor, err,
			"native executor: %s did not answer within %s — a password dialog was almost certainly raised "+
				"and there is nobody logged in to answer it. Unlock it with `security unlock-keychain` and "+
				"give it no timeout (§13.3)", name, ProbeTimeout)
	}
	return wrapErr(protocol.ErrExecutor, err,
		"native executor: %s is locked or unreadable, so every codesign in a run will fail with an error "+
			"that does not mention the keychain. Unlock it with `security unlock-keychain` (§13.3)", name)
}

// currentUID is os.Getuid behind a variable so a test can pretend to be root
// without being root.
var currentUID = os.Getuid

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
