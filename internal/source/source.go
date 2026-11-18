// Package source turns a repo and a ref into a workspace a run can build in.
//
// The model is a persistent bare mirror per repo plus a disposable hard-linked
// clone per run (RUNNER_SPEC §12). Fetches are incremental, checkouts are
// clean, parallel runs share no working directory, and — the point of the
// whole design — the workspace is a real git repository from any vantage
// point.
//
// # Why not worktrees
//
// Draft 01 specified `git worktree add`, which is the obvious choice and is
// wrong here for one specific reason (finding F2). A worktree's .git is a
// *file* holding an absolute `gitdir:` path pointing back into the mirror. The
// executors bind-mount or share only the workspace, so that path does not
// exist inside a container or a Tart VM and every git command a build script
// runs fails with "not a git repository". Build scripts call `git rev-parse
// HEAD` and `git describe --tags` constantly — version stamping, build
// numbers, dirty-tree checks — so this breaks on the first real repo.
//
// A local clone is self-contained: `git clone --local` hardlinks the object
// store rather than copying it, so the workspace costs a few hundred kilobytes
// of refs and index metadata plus the working tree, which a worktree checkout
// writes anyway. Deleting one is a plain rm -rf with nothing left behind in
// the mirror, which is most of what makes startup reconciliation (§6) simple.
//
// # Where this sits in admission
//
// The package covers gates 6, 7, 9 and 10 of RUNNER_SPEC §10:
//
//  6. mirror fetch          Manager.Fetch      per-repo mutex
//  7. ref normalization     Mirror.HasRef      via registry.NormalizeRef
//  9. resolve to SHA        Mirror.Resolve     ${REF}^{commit}
//  10. workspace clone       Mirror.Checkout    hard-linked local clone
//
// Gate 8 — the authoritative ref policy check — sits between 7 and 9 and
// belongs to internal/registry, which is why those are four calls and not one.
// [Mirror.Acquire] fuses 9 and 10 for callers that have already passed 8.
//
// # INV-3
//
// Every git invocation in this package is built as an argv slice and passed to
// exec.Command, with "--" or "--end-of-options" before any argument a broker
// could have influenced. No shell is invoked anywhere on this path and no
// command string is ever concatenated. The validators in internal/registry and
// [protocol.ValidateID] are defence in depth on top of that; this package is
// the thing they defend, so it re-applies them at the call site rather than
// trusting that someone upstream already did.
package source

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"regexp"
	"strings"
	"sync"

	"github.com/Abyssinia-Labs/latem/internal/mirrorlock"
	"github.com/Abyssinia-Labs/latem/pkg/protocol"
)

// mirrorSuffix is appended to a repo key to form the bare mirror's directory
// name, matching the layout in RUNNER_SPEC §12 (mirrors/<repo-key>.git).
const mirrorSuffix = ".git"

// incomingPrefix names a mirror that is still being cloned. Mirrors are built
// under this prefix and renamed into place, so a crash halfway through a
// 2 GB clone cannot leave a truncated object store sitting at the final path
// where the next run would mistake it for a usable mirror.
const incomingPrefix = ".incoming-"

// Config describes where source lives on this machine.
type Config struct {
	// MirrorDir holds one bare mirror per repo. Persistent.
	MirrorDir string

	// WorkDir holds one workspace per run. Disposable; RUNNER_SPEC §6 deletes
	// everything here whose run id is not in a non-terminal state.
	WorkDir string

	// WarmDir holds the workspaces jobs keep between runs (A90), one per
	// repository, runner and job. Empty means this manager keeps none, and a
	// job that asks for one builds in a fresh clone.
	WarmDir string

	// GitBinary is the git executable. Empty means "git", resolved on PATH at
	// [New] so a missing git is a startup failure (§6 preflight) rather than a
	// surprise forty minutes into the first build.
	GitBinary string

	// Env is appended to the environment of every git invocation.
	//
	// Production leaves this empty: RUNNER_SPEC §12 says credentials are the
	// machine's own — SSH agent or a credential helper — apart from a token
	// the work brought ([Repo.Token]), so git must see the ambient environment
	// and the machine's own gitconfig. It exists so tests
	// can pin identity and disable ambient config without a global mutation
	// that would race every other test in the package.
	Env []string

	// Logger says which credential a fetch used, and is how anyone finds out
	// that a token stopped working on a machine whose own login hides it.
	// Nil logs nothing.
	Logger *slog.Logger
}

// Repo is one entry from the config allowlist: the map key from `[repos.<key>]`
// and the URL beside it.
//
// This is not the broker-supplied `repo` string. INV-3 requires that value to
// be matched exactly against the config map and never used to build a path;
// what reaches this package is the key that match produced.
type Repo struct {
	Key string
	URL string
	// Token is a credential the work came with, for this repository (A72): a
	// read-only GitHub token the control plane minted for the job or the plan.
	// It is not config, and it reaches git through one process's environment,
	// so no file or argv ever holds it. Empty means the machine's own.
	Token string
}

// repoKeyPattern bounds what may become a directory name under MirrorDir.
//
// The key comes from local config rather than from a broker, so this is not
// guarding against an attacker so much as against a config typo becoming a
// path. Dots are permitted because repo keys like "example.com" are ordinary,
// and ".." is rejected separately below — a class that allows dots needs that
// check, which is precisely why [protocol.ValidateID] excludes them entirely
// for the values that do come from outside.
var repoKeyPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// shaPattern matches a resolved object name, SHA-1 or SHA-256.
var shaPattern = regexp.MustCompile(`^[0-9a-f]{40}$|^[0-9a-f]{64}$`)

// validate checks a Repo before any of it becomes a path or an argv element.
func (r Repo) validate() error {
	switch {
	case r.Key == "":
		return codeErr(protocol.ErrUnknownRepo, "repo key is empty")
	case !repoKeyPattern.MatchString(r.Key):
		return codeErr(protocol.ErrUnknownRepo,
			"repo key %q is not [A-Za-z0-9][A-Za-z0-9._-]{0,63}", r.Key)
	case strings.Contains(r.Key, ".."):
		return codeErr(protocol.ErrUnknownRepo, "repo key %q contains \"..\"", r.Key)
	}
	return validateURL(r.URL)
}

// validateURL rejects remote URLs that would turn a git invocation into
// something other than a fetch.
//
// Two of these matter even though the URL comes from trusted local config.
// A leading dash is read as an option by any git subcommand that has not been
// given "--" — every call site here has, so this is the belt to that braces.
// The "ext::" transport is worse: it is documented git functionality that runs
// the rest of the URL as an arbitrary command, so a config file containing one
// is a config file that executes, and there is no legitimate reason for one to
// appear in a repo allowlist.
func validateURL(u string) error {
	switch {
	case u == "":
		return codeErr(protocol.ErrUnknownRepo, "repo url is empty")
	case len(u) > 2048:
		return codeErr(protocol.ErrUnknownRepo, "repo url is %d bytes, limit 2048", len(u))
	case strings.HasPrefix(u, "-"):
		return codeErr(protocol.ErrUnknownRepo,
			"repo url %q begins with a dash and would be read as a git option", u)
	case strings.HasPrefix(strings.ToLower(u), "ext::"):
		return codeErr(protocol.ErrUnknownRepo,
			"repo url %q uses the ext:: transport, which executes a command", u)
	}
	if i := strings.IndexFunc(u, func(r rune) bool { return r < 0x20 || r == 0x7f }); i >= 0 {
		return codeErr(protocol.ErrUnknownRepo,
			"repo url contains a control character at byte %d", i)
	}
	return nil
}

// Manager owns the mirror and work directories for one agent.
//
// One Manager serves every repo and every concurrent run on the machine. It is
// safe for concurrent use; see [Manager.Fetch] for what is and is not
// serialised, which is the difference between two runs on one repo overlapping
// and queueing.
type Manager struct {
	cfg Config
	git string

	// mu guards repoMu only. It is held for the map lookup and never across a
	// git invocation. repoMu is each repo's in-process half of
	// [Manager.lockMirror].
	mu     sync.Mutex
	repoMu map[string]*sync.RWMutex
	// warmGates is one turn per warm workspace, by its directory (A90).
	warmGates map[string]chan struct{}

	// readOnly marks a manager from [NewReader]: it reads mirrors the agent
	// made and writes nothing (A57).
	readOnly bool

	// beforeClone is a test seam, nil in production. It runs immediately
	// before `git clone --local`, which is where a test can observe whether
	// clones out of one mirror overlap or queue.
	beforeClone func(runID string)

	// onGit is a test seam, nil in production, receiving the argv of every
	// invocation. INV-3 is a claim about argv — one slice, no shell, "--"
	// before anything a broker influenced — and this is what lets a test check
	// that claim against the argv itself rather than against its effects.
	// Not synchronised; set it only from a single-goroutine test.
	onGit func(argv []string)
}

// New prepares the directories and verifies the machine can support the model.
//
// The same-filesystem check is the one that earns its place. `git clone
// --local` hardlinks the object store; if MirrorDir and WorkDir sit on
// different filesystems git silently falls back to copying the whole thing,
// and every run on the machine gets slower in a way nobody attributes to the
// right cause — there is no error, no warning, just a 2 GB copy per run. It is
// a hard error here, and `latem doctor` reports it (§12).
func New(cfg Config) (*Manager, error) {
	if !filepath.IsAbs(cfg.MirrorDir) {
		return nil, codeErr(protocol.ErrSource, "mirror dir %q is not absolute", cfg.MirrorDir)
	}
	if !filepath.IsAbs(cfg.WorkDir) {
		return nil, codeErr(protocol.ErrSource, "work dir %q is not absolute", cfg.WorkDir)
	}

	resolved, err := lookGit(cfg.GitBinary)
	if err != nil {
		return nil, err
	}

	cfg.MirrorDir = filepath.Clean(cfg.MirrorDir)
	cfg.WorkDir = filepath.Clean(cfg.WorkDir)

	if err := mkdirPrivate(cfg.MirrorDir); err != nil {
		return nil, err
	}
	if err := mkdirPrivate(cfg.WorkDir); err != nil {
		return nil, err
	}
	if err := CheckSameFilesystem(cfg.MirrorDir, cfg.WorkDir); err != nil {
		return nil, err
	}
	if cfg.WarmDir != "" {
		if !filepath.IsAbs(cfg.WarmDir) {
			return nil, codeErr(protocol.ErrSource, "warm dir %q is not absolute", cfg.WarmDir)
		}
		cfg.WarmDir = filepath.Clean(cfg.WarmDir)
		if err := mkdirPrivate(cfg.WarmDir); err != nil {
			return nil, err
		}
		// The same reason as WorkDir: each run's new .git is a hard-linked
		// clone, and across filesystems it is a copy of every object.
		if err := CheckSameFilesystem(cfg.MirrorDir, cfg.WarmDir); err != nil {
			return nil, err
		}
	}

	return &Manager{
		cfg:    cfg,
		git:    resolved,
		repoMu: make(map[string]*sync.RWMutex),
	}, nil
}

// MirrorDir returns the configured mirror directory.
func (m *Manager) MirrorDir() string { return m.cfg.MirrorDir }

// WorkDir returns the configured work directory.
func (m *Manager) WorkDir() string { return m.cfg.WorkDir }

// mirrorPath maps a repo key to its bare mirror, refusing anything that would
// not land directly inside MirrorDir.
func (m *Manager) mirrorPath(key string) (string, error) {
	path := filepath.Join(m.cfg.MirrorDir, key+mirrorSuffix)
	if filepath.Dir(path) != m.cfg.MirrorDir {
		return "", codeErr(protocol.ErrUnknownRepo, "repo key %q escapes the mirror directory", key)
	}
	return path, nil
}

// workspacePath maps a run id to its workspace.
//
// The id is re-validated here even though [protocol.ValidateID] already ran at
// decode. A run id of "../../../../tmp/x" was a real finding (amendment A6) and
// the fix is not one validation point everyone trusts — it is this function
// refusing to build the path, and then asserting the built path stayed put.
func (m *Manager) workspacePath(runID string) (string, error) {
	if err := protocol.ValidateID(runID); err != nil {
		return "", wrapErr(protocol.ErrSource, err, "refusing to build a workspace path")
	}
	path := filepath.Join(m.cfg.WorkDir, runID)
	if filepath.Dir(path) != m.cfg.WorkDir {
		return "", codeErr(protocol.ErrSource, "run id %q escapes the work directory", runID)
	}
	return path, nil
}

// repoLock returns the in-process half of one repo's mirror lock.
func (m *Manager) repoLock(key string) *sync.RWMutex {
	m.mu.Lock()
	defer m.mu.Unlock()
	l, ok := m.repoMu[key]
	if !ok {
		l = new(sync.RWMutex)
		m.repoMu[key] = l
	}
	return l
}

// lockMirror takes one repo's mirror lock and returns its release: exclusive
// for anything that writes into the mirror, shared for a `git clone --local`
// out of it (A112).
//
// A clone is not a read in the sense git's own commands read. `git
// rev-parse` or `git cat-file` that misses a pack a repack just removed looks
// again and finds the pack that replaced it; `clone --local` lists objects/
// once and hard-links what it listed, so a pack deleted between the two
// leaves the workspace without it. That is what happened on 2026-11-18: one
// run of take-one fetched, and the `git maintenance run --auto` that fetch
// left behind repacked the mirror while another run's clone was linking it.
// Its checkout said "unable to read tree".
//
// Two halves. This process's RWMutex keeps its own goroutines from polling a
// file, and a waiting writer from being starved by a stream of clones. The
// flock beside the mirror is what `latem cache prune`'s gc and `latem try`,
// in other processes, meet ([mirrorlock]). The mutex is taken first, so the
// file lock is only ever contended across processes.
//
// A wait that ctx ends is E_SOURCE, which the broker retries.
func (m *Manager) lockMirror(ctx context.Context, key string, exclusive bool) (func(), error) {
	path, err := m.mirrorPath(key)
	if err != nil {
		return nil, err
	}
	l := m.repoLock(key)
	if exclusive {
		l.Lock()
	} else {
		l.RLock()
	}
	unlockMu := l.Unlock
	if !exclusive {
		unlockMu = l.RUnlock
	}
	unlockFile, err := mirrorlock.Lock(ctx, path, exclusive)
	if err != nil {
		unlockMu()
		return nil, wrapErr(protocol.ErrSource, err, "locking the mirror of %s", key)
	}
	return func() {
		unlockFile()
		unlockMu()
	}, nil
}

func validateSHA(sha string) error {
	if !shaPattern.MatchString(sha) {
		return codeErr(protocol.ErrSource, "%q is not a resolved object name", sha)
	}
	return nil
}

// errUnexpected wraps a git failure that has no more specific explanation.
func gitErr(err error, what string, args ...any) error {
	return wrapErr(protocol.ErrSource, err, "%s", fmt.Sprintf(what, args...))
}
