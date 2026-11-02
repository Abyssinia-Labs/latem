package source

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strings"
	"time"

	"github.com/Abyssinia-Labs/latem/pkg/protocol"
)

// gitStateEnv are the variables that redirect git at a repository other than
// the one named on the command line.
//
// They are stripped from every invocation. If the agent is ever started from
// inside a git hook — `git push` triggering a deploy, someone running `latem
// agent` from a repo with GIT_DIR exported, a CI harness that sets it — then
// an inherited GIT_DIR silently overrides the "-C <path>" on every command in
// this package, and a run fetches and checks out somewhere entirely unrelated.
// Clearing them costs nothing and removes the class.
var gitStateEnv = []string{
	"GIT_DIR",
	"GIT_WORK_TREE",
	"GIT_COMMON_DIR",
	"GIT_INDEX_FILE",
	"GIT_OBJECT_DIRECTORY",
	"GIT_ALTERNATE_OBJECT_DIRECTORIES",
	"GIT_NAMESPACE",
	"GIT_CEILING_DIRECTORIES",
}

// gitWaitDelay bounds how long a cancelled git may keep the output pipes open.
//
// exec.CommandContext kills the process on cancellation, but a grandchild —
// git-upload-pack over ssh, a credential helper — inherits the pipes and can
// hold Wait open indefinitely. A run cancelled from a phone should not leave a
// goroutine parked on a dead fetch.
const gitWaitDelay = 10 * time.Second

// lookGit resolves the git executable once, at startup.
//
// Resolving here rather than at each invocation means a machine without git
// fails at agent start (the §6 preflight) instead of forty minutes into the
// first build, and it means every later exec.Command is handed an absolute
// path that cannot be re-resolved against a PATH that changed underneath.
func lookGit(bin string) (string, error) {
	if bin == "" {
		bin = "git"
	}
	resolved, err := exec.LookPath(bin)
	if err != nil {
		return "", wrapErr(protocol.ErrSource, err, "locating the git executable")
	}
	return resolved, nil
}

// env builds the environment for a git invocation.
//
// Overriding by appending would work — os/exec deduplicates and keeps the last
// value — but only because of an implementation detail of the standard
// library. Removing the shadowed entries makes the result correct by
// construction and readable in a process listing.
func (m *Manager) env(extra ...string) []string {
	drop := make(map[string]struct{}, len(gitStateEnv)+len(m.cfg.Env)+len(extra)+1)
	for _, name := range gitStateEnv {
		drop[name] = struct{}{}
	}
	drop["GIT_TERMINAL_PROMPT"] = struct{}{}
	for _, kv := range slices.Concat(m.cfg.Env, extra) {
		if name, _, ok := strings.Cut(kv, "="); ok {
			drop[name] = struct{}{}
		}
	}

	base := os.Environ()
	out := make([]string, 0, len(base)+len(m.cfg.Env)+len(extra)+1)
	for _, kv := range base {
		name, _, ok := strings.Cut(kv, "=")
		if ok {
			if _, shadowed := drop[name]; shadowed {
				continue
			}
		}
		out = append(out, kv)
	}

	// A private repo with no usable credential must fail, not block. Without
	// this git opens /dev/tty and asks for a username, and a run sits in
	// `preparing` until someone notices.
	out = append(out, "GIT_TERMINAL_PROMPT=0")

	return slices.Concat(out, m.cfg.Env, extra)
}

// tokenEnv is repo's token as git config in the environment, scoped to its
// remote's host, in the form [CredentialConfig] writes to a file. The
// environment rather than `-c`, because argv is readable by every process on
// the machine, and rather than a file, because nothing here needs one to
// outlive this git. It reports false for a remote that takes no such header:
// an ssh URL is the machine's own credential however the work arrived.
//
// GIT_CONFIG_COUNT is git 2.31's. An older git ignores it and falls back to the
// machine's credential, which is what it did before there were tokens.
func tokenEnv(repo Repo) ([]string, bool) {
	if repo.Token == "" {
		return nil, false
	}
	base, err := remoteBase(repo.URL)
	if err != nil {
		return nil, false
	}
	return []string{
		"GIT_CONFIG_COUNT=1",
		"GIT_CONFIG_KEY_0=http." + base + ".extraheader",
		"GIT_CONFIG_VALUE_0=AUTHORIZATION: basic " + basicAuth(repo.Token),
	}, true
}

// command builds one git invocation.
//
// This is the only place in the package that constructs an exec.Cmd, which is
// deliberate: INV-3 is a property of every git call the agent makes, and a
// property enforced in one function is auditable in a way that the same
// property enforced in eleven call sites is not. argv is a slice. There is no
// shell. Callers place "--" or "--end-of-options" before user-influenced
// arguments; see [Mirror.Resolve] for why it is sometimes the latter.
func (m *Manager) command(ctx context.Context, dir string, args []string) *exec.Cmd {
	argv := args
	if dir != "" {
		argv = append([]string{"-C", dir}, args...)
	}
	if m.onGit != nil {
		m.onGit(argv)
	}

	// #nosec G204 -- This is the INV-3 surface and it is safe by construction,
	// not by luck. m.git is resolved from PATH at New and is never
	// broker-derived. argv is a slice passed straight to execve with no shell
	// anywhere in the path, so no element can be word-split, globbed, or read
	// as a command regardless of its contents. Every element is either a
	// literal written in this package, a path this package built from a
	// validated repo key or run id, or a ref/SHA that has passed
	// registry.ValidateRef or validateSHA and sits after "--" or
	// "--end-of-options".
	cmd := exec.CommandContext(ctx, m.git, argv...)
	// Stdin stays nil: nothing here reads input, and a git that cannot read a
	// password from a pipe is a git that fails instead of hanging.
	cmd.Stdin = nil
	cmd.Env = m.env()
	cmd.WaitDelay = gitWaitDelay
	return cmd
}

// run executes git and returns its trimmed stdout.
func (m *Manager) run(ctx context.Context, dir string, args ...string) (string, error) {
	return capture(m.command(ctx, dir, args), args)
}

// runRemote is [Manager.run] for a git that talks to repo's remote, which
// authenticates with repo's token when it has one.
//
// A token that does not work is not the end of it: one can expire under a long
// job, and the machine's own login may reach what it cannot. So the machine's
// credential gets a turn, and git's first complaint is the one reported, since
// the second is usually only that there is no login here to try.
func (m *Manager) runRemote(ctx context.Context, repo Repo, dir string, args ...string) (string, error) {
	env, ok := tokenEnv(repo)
	if !ok {
		return m.run(ctx, dir, args...)
	}
	cmd := m.command(ctx, dir, args)
	cmd.Env = m.env(env...)
	out, err := capture(cmd, args)
	if err == nil || ctx.Err() != nil {
		return out, err
	}
	if out, ownErr := m.run(ctx, dir, args...); ownErr == nil {
		return out, nil
	}
	return "", err
}

// capture runs cmd, which args built, and returns its trimmed stdout.
func capture(cmd *exec.Cmd, args []string) (string, error) {
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return "", &gitError{
			args:   args,
			stderr: strings.TrimSpace(stderr.String()),
			err:    err,
		}
	}
	return strings.TrimSpace(stdout.String()), nil
}

// runQuery executes git for a command whose answer *is* its exit status, and
// reports whether it exited 0.
//
// `git show-ref --verify --quiet` is the motivating case: absent is exit 1 and
// broken is exit 128, and collapsing the two into "error" would turn a missing
// ref into E_SOURCE instead of E_INVALID_REF.
func (m *Manager) runQuery(ctx context.Context, dir string, args ...string) (bool, error) {
	cmd := m.command(ctx, dir, args)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	err := cmd.Run()
	if err == nil {
		return true, nil
	}

	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
		return false, nil
	}
	return false, &gitError{
		args:   args,
		stderr: strings.TrimSpace(stderr.String()),
		err:    err,
	}
}

// gitError reports a failed git invocation with enough context to diagnose it.
//
// It records argv rather than a rendered command string, because there is no
// command string — writing one would invite someone to build the next call
// site from it.
type gitError struct {
	args   []string
	stderr string
	err    error
}

func (e *gitError) Error() string {
	if e.stderr != "" {
		return fmt.Sprintf("git %s: %v: %s", strings.Join(e.args, " "), e.err, e.stderr)
	}
	return fmt.Sprintf("git %s: %v", strings.Join(e.args, " "), e.err)
}

func (e *gitError) Unwrap() error { return e.err }
