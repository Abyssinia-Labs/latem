package source

import (
	"context"
	"os"
	"path/filepath"
	"strings"

	"github.com/Abyssinia-Labs/latem/internal/registry"
	"github.com/Abyssinia-Labs/latem/pkg/protocol"
)

// gitattributesLimit caps how much of a .gitattributes file is read when
// deciding whether a repo uses LFS. The file is a few lines in every real
// repo; the cap is here so a pathological one cannot be read into memory.
const gitattributesLimit = 1 << 20

// Workspace is one run's checkout: a standalone git repository at a pinned
// commit.
//
// "Standalone" is the load-bearing word. .git is a real directory with a real
// object store, so `git rev-parse HEAD` and `git describe --tags` work inside
// a container that has only this path bind-mounted, and deleting the directory
// is a plain rm -rf that leaves nothing behind in the mirror.
type Workspace struct {
	// Path is the workspace root and the executor's Spec.Workspace.
	Path string

	// RunID, Repo, Ref and SHA are what the run is, recorded here so the
	// caller does not have to keep them alongside. Ref is the normalized,
	// fully-qualified name from gate 7, never the raw broker string.
	RunID string
	Repo  string
	Ref   string
	SHA   string

	// release is set for a warm workspace (A90): removing it gives its turn
	// to the next run and leaves its files where they are.
	release func() error
}

// Warm reports a workspace its job keeps between runs (A90).
func (w *Workspace) Warm() bool { return w.release != nil }

// Acquire runs gates 9 and 10 together: resolve the ref to a commit, then
// build the workspace at it.
//
// Gate 8 — the authoritative ref policy check — happens between gate 7 and
// this call, in internal/registry, which is why normalization is not folded in
// here as well.
func (mi *Mirror) Acquire(ctx context.Context, runID, normalizedRef string) (*Workspace, error) {
	sha, err := mi.Resolve(ctx, normalizedRef)
	if err != nil {
		return nil, err
	}
	return mi.Checkout(ctx, runID, normalizedRef, sha)
}

// Checkout is gate 10: a hard-linked local clone of the mirror, detached at sha.
//
// The sequence is RUNNER_SPEC §12's, and each step is there for a reason:
//
//	clone --local --no-checkout   hardlinks the object store; --no-checkout
//	                              because cloning without it checks out the
//	                              mirror's default branch and then the target,
//	                              doubling I/O on a large repo for nothing
//	checkout --detach <sha>       the one and only checkout
//	remote set-url origin <url>   so `git fetch` inside a build reaches the
//	                              real upstream rather than a host path that
//	                              does not exist in the container
//	config gc.auto 0              a gc inside a run would unpick the hardlinks
//	                              it was cloned with, turning a 300 KB
//	                              workspace into a full copy mid-build
//
// Submodules and LFS follow, in that order and only when the checked-out tree
// asks for them. Submodules come after `remote set-url` deliberately: a
// .gitmodules with a relative URL ("../shared.git") resolves against origin,
// so running it before the rewrite would resolve against the mirror path.
func (mi *Mirror) Checkout(ctx context.Context, runID, normalizedRef, sha string) (*Workspace, error) {
	if mi.mgr.readOnly {
		return nil, errReader
	}
	m := mi.mgr

	if err := registry.ValidateRef(normalizedRef); err != nil {
		return nil, wrapErr(protocol.ErrInvalidRef, err, "checking out in %s", mi.repo.Key)
	}
	if err := validateSHA(sha); err != nil {
		return nil, err
	}
	path, err := m.workspacePath(runID)
	if err != nil {
		return nil, err
	}

	// A workspace already at this path is a run id colliding with a crashed
	// run's leftovers. Reconciliation (§6) removes those at startup, so
	// reaching here means something is wrong; deleting someone else's
	// workspace on a guess is a worse answer than refusing.
	if _, err := os.Stat(path); err == nil {
		return nil, codeErr(protocol.ErrSource,
			"workspace %s already exists; startup reconciliation should have removed it", path)
	} else if !os.IsNotExist(err) {
		return nil, wrapErr(protocol.ErrSource, err, "inspecting %s", path)
	}

	if m.beforeClone != nil {
		m.beforeClone(runID)
	}

	// Anything that fails from here leaves a partial workspace, which would
	// then trip the "already exists" check above on the retry. Unwind it.
	ok := false
	defer func() {
		if !ok {
			_ = os.RemoveAll(path)
		}
	}()

	if _, err := m.run(ctx, "", "clone", "--local", "--no-checkout", "--", mi.path, path); err != nil {
		return nil, gitErr(err, "cloning %s into %s", mi.repo.Key, path)
	}
	// No options terminator here, deliberately, and this is the one argv in the
	// package without one (amendment A40).
	//
	// `git checkout --detach --end-of-options <sha>` works on git 2.50 and FAILS
	// on 2.43 with "--detach does not take a path argument '--end-of-options'":
	// checkout's own path/revision disambiguation did not learn the terminator
	// until somewhere between those releases. A7 recorded the terminator as
	// required, having verified it on 2.50.1 alone, and that made every runner
	// with an older git unable to check anything out.
	//
	// Dropping it is safe *here specifically* because sha is not a
	// broker-supplied string. validateSHA at the top of this function has
	// already required ^[0-9a-f]{40}$ or ^[0-9a-f]{64}$, so the value provably
	// cannot begin with a dash and cannot be read as an option. The terminator
	// stays on every argv carrying a ref, a URL or a path, where the value is
	// genuinely attacker-influenced — see git.go and mirror.go.
	if _, err := m.run(ctx, path, "checkout", "--detach", sha); err != nil {
		return nil, gitErr(err, "checking out %s in %s", sha, path)
	}
	if _, err := m.run(ctx, path, "remote", "set-url", "origin", "--", mi.repo.URL); err != nil {
		return nil, gitErr(err, "pointing %s at %s", path, mi.repo.URL)
	}
	if err := m.disableAutoGC(ctx, path); err != nil {
		return nil, err
	}
	if err := m.initSubmodules(ctx, path); err != nil {
		return nil, err
	}
	if err := m.fetchLFS(ctx, mi, path); err != nil {
		return nil, err
	}

	ok = true
	return &Workspace{
		Path:  path,
		RunID: runID,
		Repo:  mi.repo.Key,
		Ref:   normalizedRef,
		SHA:   sha,
	}, nil
}

// initSubmodules populates submodules, and only when there are any.
//
// The guard is not an optimisation. `git submodule update` on a repo without
// submodules is harmless but not free, and more importantly the conditional is
// what keeps the common case free of a network round trip per run. --depth 1
// because a submodule's history is never what is being built.
func (m *Manager) initSubmodules(ctx context.Context, path string) error {
	if _, err := os.Stat(filepath.Join(path, ".gitmodules")); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return wrapErr(protocol.ErrSource, err, "looking for submodules in %s", path)
	}
	if _, err := m.run(ctx, path, "submodule", "update", "--init", "--recursive", "--depth", "1"); err != nil {
		return gitErr(err, "initialising submodules in %s", path)
	}
	return nil
}

// fetchLFS materialises LFS content, and only when the tree routes something
// through LFS.
//
// The condition matters more than it looks. `git lfs fetch --all` on a repo
// that does not use LFS is a no-op that still costs a process; on a repo that
// does, it is potentially tens of gigabytes, and doing it unconditionally
// would make every run on every repo pay for a feature two of them use.
//
// A repo that needs LFS on a machine without git-lfs installed is a hard
// failure rather than a warning. The alternative is a workspace full of
// 130-byte pointer files, a build that fails somewhere much later with an
// unrecognisable error, and an operator reading the wrong log.
func (m *Manager) fetchLFS(ctx context.Context, mi *Mirror, path string) error {
	uses, err := usesLFS(filepath.Join(path, ".gitattributes"))
	if err != nil || !uses {
		return err
	}

	// This writes into the mirror, so it takes the per-repo lock that
	// [Manager.Fetch] uses. The workspace half below does not.
	lock := m.repoLock(mi.repo.Key)
	lock.Lock()
	fetchErr := func() error {
		defer lock.Unlock()
		if _, err := m.runRemote(ctx, mi.repo, mi.path, "lfs", "fetch", "--all"); err != nil {
			return gitErr(err, "fetching LFS objects for %s", mi.repo.Key)
		}
		return nil
	}()
	if fetchErr != nil {
		return fetchErr
	}

	if _, err := m.run(ctx, path, "lfs", "checkout"); err != nil {
		return gitErr(err, "checking out LFS content in %s", path)
	}
	return nil
}

// usesLFS reports whether a .gitattributes file routes any path through
// git-lfs.
//
// It looks for `filter=lfs` rather than for the string "lfs" anywhere. The
// filter attribute is what actually makes git invoke git-lfs; a comment
// mentioning LFS, or a directory named lfs/, should not commit every run on
// the repo to a `git lfs fetch --all`.
func usesLFS(gitattributes string) (bool, error) {
	info, err := os.Stat(gitattributes)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, wrapErr(protocol.ErrSource, err, "reading %s", gitattributes)
	}
	if info.Size() > gitattributesLimit {
		return false, codeErr(protocol.ErrSource,
			"%s is %d bytes, which is not a .gitattributes file", gitattributes, info.Size())
	}

	// #nosec G304 -- gitattributes is filepath.Join(<workspace>, ".gitattributes")
	// where the workspace path was built from a protocol.ValidateID'd run id and
	// asserted to sit directly inside WorkDir. The trailing element is a
	// literal.
	data, err := os.ReadFile(gitattributes)
	if err != nil {
		return false, wrapErr(protocol.ErrSource, err, "reading %s", gitattributes)
	}

	for line := range strings.Lines(string(data)) {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.Contains(strings.ToLower(line), "filter=lfs") {
			return true, nil
		}
	}
	return false, nil
}

// Env is the run environment this package contributes.
//
// RUNNER_SPEC §12 exports these regardless of whether a job asks, so a job
// that wants the commit does not have to shell out to git for it — which
// matters for the executors where shelling out is a round trip into a VM.
// LATEM_REF is the normalized fully-qualified name, so a job logging it
// records something reproducible rather than whatever short form the broker
// happened to send.
func (w *Workspace) Env() map[string]string {
	return map[string]string{
		"LATEM_SHA":    w.SHA,
		"LATEM_REF":    w.Ref,
		"LATEM_REPO":   w.Repo,
		"LATEM_RUN_ID": w.RunID,
	}
}

// Remove deletes the workspace.
//
// It is a plain rm -rf, and that is the whole point of §12's model: there is
// no worktree administrative state in the mirror to prune, so a workspace
// cannot be half-deleted and cannot leak into the next run. Safe to call
// twice.
//
// A warm workspace is released instead (A90): its files are what the job
// asked to keep.
func (w *Workspace) Remove() error {
	if w.release != nil {
		return w.release()
	}
	if err := os.RemoveAll(w.Path); err != nil {
		return wrapErr(protocol.ErrSource, err, "removing workspace %s", w.Path)
	}
	return nil
}

// ListWorkspaces returns the names of every directory under WorkDir.
//
// Startup reconciliation (§6) calls this, cross-references sqlite, and deletes
// anything whose run id is not in a non-terminal state. Names that are not
// valid run ids are returned too, because hiding them would hide a leak; they
// are simply not removable through [Manager.RemoveWorkspace], which is
// deliberate — this package will not rm -rf a path it cannot vouch for having
// created.
func (m *Manager) ListWorkspaces() ([]string, error) {
	entries, err := os.ReadDir(m.cfg.WorkDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, wrapErr(protocol.ErrSource, err, "listing %s", m.cfg.WorkDir)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			names = append(names, e.Name())
		}
	}
	return names, nil
}

// RemoveWorkspace deletes work/<runID>, for reconciliation paths that have a
// run id and no [Workspace].
//
// Removing a workspace that is not there succeeds: reconciliation should be
// able to run twice.
func (m *Manager) RemoveWorkspace(runID string) error {
	path, err := m.workspacePath(runID)
	if err != nil {
		return err
	}
	if err := os.RemoveAll(path); err != nil {
		return wrapErr(protocol.ErrSource, err, "removing workspace %s", path)
	}
	return nil
}
