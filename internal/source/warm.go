package source

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/Abyssinia-Labs/latem/internal/registry"
	"github.com/Abyssinia-Labs/latem/pkg/protocol"
)

// A warm workspace is one a job keeps between its runs (A90).
//
// §12's workspace is a new clone per run, which is what makes a run
// reproducible and is also why every run compiles from nothing: a build's
// incremental state lives beside its sources, in target/, node_modules/,
// .next/, and a new clone has none of it. A job that says `workspace: warm`
// gets the same directory back instead, at the same path, with those
// directories as its last run left them.
//
// What is kept is exactly what the repository ignores. Every tracked file is
// brought to the run's commit and checked by content, not by timestamp, so a
// file a run edited is put back and a file that did not change is not touched,
// and its mtime survives for the build tool that reads it. An untracked file
// the repository does not ignore is removed. So a warm workspace differs from
// a fresh one only in the paths .gitignore names.
//
// # The .git is never kept
//
// A step can write the workspace's .git: through the bind mount on docker, and
// as the same user on native. A host git reading a .git a step wrote would run
// its hooks, its fsmonitor and its filter drivers as the agent (checkout.go
// has the long form). So the agent never runs git in a .git a step has seen.
// Each run moves a new one into place, cloned from the mirror, with an index
// that holds the commit's tree and no memory of what is on disk, and git
// compares that with the files as it finds them.
//
// That is also what makes the content check unconditional: an index with no
// stat data cannot take a timestamp's word for anything.

// WarmKey names a warm workspace: one per repository, runner and job. The
// runner is in it because two runners are two toolchains, and what one built
// is not the other's to reuse.
type WarmKey struct {
	Runner string
	Job    string
}

// WarmReport says what bringing a warm workspace to a commit did, for the
// run's log.
type WarmReport struct {
	// Fresh is set when there was nothing to reuse: the first run, or one
	// after the workspace was removed.
	Fresh bool
	// Rebuilt is why a workspace that was there was thrown away and made
	// again, or empty.
	Rebuilt string
	// Written is how many tracked files differed from the commit and were
	// written; Removed, how many untracked files and directories went.
	Written int
	Removed int
	// PrevSHA and PrevRun are the commit and run it was last used for.
	PrevSHA string
	PrevRun string
	// PrevUsed is when that was.
	PrevUsed time.Time
}

// WarmInfo is one warm workspace on disk, for listing and pruning.
type WarmInfo struct {
	Repo   string    `json:"repo"`
	Runner string    `json:"runner"`
	Job    string    `json:"job"`
	SHA    string    `json:"sha"`
	RunID  string    `json:"run_id"`
	UsedAt time.Time `json:"used_at"`
	// Path is the directory that holds it. Not stored.
	Path string `json:"-"`
	// InUse is set while a run holds it. Not stored.
	InUse bool `json:"-"`
}

// KeptWorkspace is a warm [Workspace] at path whose Remove calls release and
// deletes nothing. [Mirror.WarmCheckout] makes its own; this is for a mirror
// implemented outside this package, which a test's is.
func KeptWorkspace(path, runID, repo, ref, sha string, release func() error) *Workspace {
	return &Workspace{Path: path, RunID: runID, Repo: repo, Ref: ref, SHA: sha, release: release}
}

// ErrWarmUnavailable is returned when this machine keeps no warm workspaces,
// or cannot keep this commit's: the caller builds in a fresh one and says why.
var ErrWarmUnavailable = errors.New("source: no warm workspace")

const (
	warmTreeName  = "tree"
	warmStateName = "state.json"
	warmLockName  = "lock"
	// warmStagePrefix names the clone a run's new .git is taken from.
	warmStagePrefix = "git-"
	// warmLockPoll is how often a run waiting for another process's lock
	// asks again. The only other process is a `latem cache prune`, which
	// holds one for as long as a removal takes.
	warmLockPoll = 50 * time.Millisecond
)

// warmPartPattern bounds what becomes a directory name under WarmDir. Runner
// and job names are already held to it where they are declared; this is the
// function that builds the path refusing to trust that.
func validWarmPart(s string) bool {
	return repoKeyPattern.MatchString(s) && !strings.Contains(s, "..")
}

// warmPath is where a key's warm workspace lives: <WarmDir>/<repo>/<runner>/<job>.
func (m *Manager) warmPath(repo string, key WarmKey) (string, error) {
	if m.cfg.WarmDir == "" {
		return "", ErrWarmUnavailable
	}
	for _, part := range []string{repo, key.Runner, key.Job} {
		if !validWarmPart(part) {
			return "", codeErr(protocol.ErrSource, "%q cannot name a warm workspace", part)
		}
	}
	return filepath.Join(m.cfg.WarmDir, repo, key.Runner, key.Job), nil
}

// warmGate is the turn-taking for one warm workspace among this process's
// runs. A channel, not a mutex, so a run waiting its turn can still be
// cancelled or run out of time. [lockWarm] is the same turn across processes.
func (m *Manager) warmGate(dir string) chan struct{} {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.warmGates == nil {
		m.warmGates = map[string]chan struct{}{}
	}
	g, ok := m.warmGates[dir]
	if !ok {
		g = make(chan struct{}, 1)
		m.warmGates[dir] = g
	}
	return g
}

// WarmUnsupported says why a commit cannot be built in a warm workspace, or ""
// when it can. Read from the mirror, before anything is written.
//
// Submodules keep their repositories under the superproject's .git, which a
// warm workspace replaces each run, and LFS keeps its objects there too. Both
// would be fetched again every run, which is the cost warm exists to avoid.
func (mi *Mirror) WarmUnsupported(ctx context.Context, sha string) (string, error) {
	if err := validateSHA(sha); err != nil {
		return "", err
	}
	out, err := mi.mgr.runRaw(ctx, mi.path, 1<<16, "ls-tree", "--name-only", "-z", "--end-of-options", sha,
		"--", ".gitmodules", ".gitattributes")
	if err != nil {
		return "", gitErr(err, "reading %s in %s", sha, mi.repo.Key)
	}
	names := splitNUL(out)
	for _, n := range names {
		if n == ".gitmodules" {
			return "the repository has submodules", nil
		}
	}
	for _, n := range names {
		if n != ".gitattributes" {
			continue
		}
		data, err := mi.mgr.runRaw(ctx, mi.path, gitattributesLimit, "cat-file", "blob", sha+":.gitattributes")
		if err != nil {
			return "", gitErr(err, "reading .gitattributes at %s in %s", sha, mi.repo.Key)
		}
		for line := range strings.Lines(string(data)) {
			line = strings.TrimSpace(line)
			if line != "" && !strings.HasPrefix(line, "#") && strings.Contains(strings.ToLower(line), "filter=lfs") {
				return "the repository uses git-lfs", nil
			}
		}
	}
	return "", nil
}

// WarmCheckout is gate 10 for a job that keeps its workspace (A90): the
// directory its last run on this runner left, brought to sha.
//
// Runs of one key take turns, waiting as long as ctx allows. The workspace is
// held until the returned [Workspace] is removed, which for a warm one means
// released: its files stay.
//
// A workspace that cannot be brought to the commit, whatever the reason, is
// deleted and made again from nothing, once. The run is then a cold one and
// the report says why.
func (mi *Mirror) WarmCheckout(
	ctx context.Context, key WarmKey, runID, normalizedRef, sha string,
) (*Workspace, WarmReport, error) {
	m := mi.mgr
	if m.readOnly {
		return nil, WarmReport{}, errReader
	}
	if err := registry.ValidateRef(normalizedRef); err != nil {
		return nil, WarmReport{}, wrapErr(protocol.ErrInvalidRef, err, "checking out in %s", mi.repo.Key)
	}
	if err := validateSHA(sha); err != nil {
		return nil, WarmReport{}, err
	}
	if err := protocol.ValidateID(runID); err != nil {
		return nil, WarmReport{}, wrapErr(protocol.ErrSource, err, "refusing to build a workspace path")
	}
	dir, err := m.warmPath(mi.repo.Key, key)
	if err != nil {
		return nil, WarmReport{}, err
	}

	unlock, err := m.lockWarm(ctx, dir)
	if err != nil {
		return nil, WarmReport{}, wrapErr(protocol.ErrSource, err,
			"waiting for the warm workspace of %s, which another run holds", key.Job)
	}
	held := true
	release := func() {
		if held {
			held = false
			unlock()
		}
	}
	ok := false
	defer func() {
		if !ok {
			release()
		}
	}()

	prev, _ := readWarmState(dir)
	report := WarmReport{PrevSHA: prev.SHA, PrevRun: prev.RunID, PrevUsed: prev.UsedAt}
	tree := filepath.Join(dir, warmTreeName)
	if _, statErr := os.Lstat(tree); statErr != nil {
		report.Fresh = true
	}

	written, removed, err := m.syncWarm(ctx, mi, dir, runID, sha)
	if err != nil && !report.Fresh && ctx.Err() == nil {
		// Whatever a run left that git could not get past — a directory it
		// may not enter, a file it may not replace — the answer is the same
		// as for a workspace that was never there.
		report.Rebuilt = firstLine(err.Error())
		if rmErr := removeAllWritable(tree); rmErr != nil {
			return nil, report, wrapErr(protocol.ErrSource, rmErr, "removing the warm workspace %s", tree)
		}
		written, removed, err = m.syncWarm(ctx, mi, dir, runID, sha)
	}
	if err != nil {
		return nil, report, err
	}
	report.Written, report.Removed = written, removed

	info := WarmInfo{Repo: mi.repo.Key, Runner: key.Runner, Job: key.Job, SHA: sha, RunID: runID, UsedAt: time.Now().UTC()}
	if err := writeWarmState(dir, info); err != nil {
		return nil, report, err
	}
	ok = true
	return &Workspace{
		Path:  tree,
		RunID: runID,
		Repo:  mi.repo.Key,
		Ref:   normalizedRef,
		SHA:   sha,
		release: func() error {
			defer release()
			// The time it was last used is when its run ended, so a long
			// build does not make its workspace look idle.
			info.UsedAt = time.Now().UTC()
			return writeWarmState(dir, info)
		},
	}, report, nil
}

// lockWarm takes dir's turn: this process's gate, then a flock on a file
// beside the tree, which is what a `latem cache prune` in another process
// meets. It waits as long as ctx allows and returns the release.
func (m *Manager) lockWarm(ctx context.Context, dir string) (func(), error) {
	gate := m.warmGate(dir)
	select {
	case gate <- struct{}{}:
	case <-ctx.Done():
		return nil, context.Cause(ctx)
	}
	for {
		f, got, err := tryLockWarm(dir, true)
		if err != nil {
			<-gate
			return nil, err
		}
		if got {
			return func() {
				_ = f.Close() // closing the description drops its flock
				<-gate
			}, nil
		}
		select {
		case <-ctx.Done():
			<-gate
			return nil, context.Cause(ctx)
		case <-time.After(warmLockPoll):
		}
	}
}

// tryLockWarm tries dir's flock once. With create unset, a directory with no
// lock file is one nothing holds, and the answer is an unlocked nil file.
func tryLockWarm(dir string, create bool) (*os.File, bool, error) {
	flags := os.O_RDWR
	if create {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, false, err
		}
		flags |= os.O_CREATE
	}
	// #nosec G304 -- dir is built by warmPath from validated parts, or read
	// from WarmDir's own listing; the name is a literal.
	f, err := os.OpenFile(filepath.Join(dir, warmLockName), flags, 0o600)
	if err != nil {
		if !create && os.IsNotExist(err) {
			return nil, true, nil
		}
		return nil, false, err
	}
	got, err := tryFlock(f)
	if err != nil || !got {
		_ = f.Close()
		return nil, false, err
	}
	return f, true, nil
}

// syncWarm gives dir's tree a new .git and brings its files to sha. It returns
// how many tracked files it wrote and how many untracked paths it removed.
func (m *Manager) syncWarm(ctx context.Context, mi *Mirror, dir, runID, sha string) (written, removed int, err error) {
	tree := filepath.Join(dir, warmTreeName)
	if err := os.MkdirAll(tree, 0o700); err != nil {
		return 0, 0, wrapErr(protocol.ErrSource, err, "creating %s", tree)
	}

	// The new .git is cloned beside the tree and moved in, so the tree never
	// holds half of one, and the old one goes whole, unread. A clone a crash
	// left beside it goes too: this run holds the directory.
	if entries, err := os.ReadDir(dir); err == nil {
		for _, e := range entries {
			if strings.HasPrefix(e.Name(), warmStagePrefix) {
				_ = removeAllWritable(filepath.Join(dir, e.Name()))
			}
		}
	}
	stage := filepath.Join(dir, warmStagePrefix+runID)
	defer func() { _ = os.RemoveAll(stage) }()
	if _, err := m.run(ctx, "", "clone", "--local", "--no-checkout", "--", mi.path, stage); err != nil {
		return 0, 0, gitErr(err, "cloning %s for a warm workspace", mi.repo.Key)
	}
	dotGit := filepath.Join(tree, ".git")
	if err := removeAllWritable(dotGit); err != nil {
		return 0, 0, wrapErr(protocol.ErrSource, err, "removing the last run's %s", dotGit)
	}
	if err := os.Rename(filepath.Join(stage, ".git"), dotGit); err != nil {
		return 0, 0, wrapErr(protocol.ErrSource, err, "placing a new .git in %s", tree)
	}

	// The index is the commit's tree with no stat data, so refreshing it
	// reads every tracked file and compares what it holds; a file that
	// matches is marked clean and left alone. sha is validated, so it cannot
	// be read as an option (A40).
	if _, err := m.run(ctx, tree, "read-tree", sha); err != nil {
		return 0, 0, gitErr(err, "reading %s into the warm workspace's index", sha)
	}
	// Exit 1 says some file differs, which the checkout is about to settle.
	if _, err := m.runQuery(ctx, tree, "update-index", "-q", "--refresh"); err != nil {
		return 0, 0, gitErr(err, "comparing the warm workspace with %s", sha)
	}
	differ, err := m.runRaw(ctx, tree, 64<<20, "diff-files", "--name-only", "-z")
	if err != nil {
		return 0, 0, gitErr(err, "listing what differs from %s in the warm workspace", sha)
	}
	written = len(splitNUL(differ))
	if _, err := m.run(ctx, tree, "checkout", "--force", "--detach", sha); err != nil {
		return 0, 0, gitErr(err, "checking out %s in the warm workspace", sha)
	}
	// Untracked and not ignored: a file the last commit tracked and this one
	// does not, or one a step left. -x is what is not here: ignored paths
	// are the point of keeping the workspace. Twice -f, for a directory that
	// is a repository of its own.
	cleaned, err := m.run(ctx, tree, "clean", "-ffd")
	if err != nil {
		return 0, 0, gitErr(err, "removing untracked files from the warm workspace")
	}
	for line := range strings.Lines(cleaned) {
		if strings.HasPrefix(line, "Removing ") {
			removed++
		}
	}
	if _, err := m.run(ctx, tree, "remote", "set-url", "origin", "--", mi.repo.URL); err != nil {
		return 0, 0, gitErr(err, "pointing %s at %s", tree, mi.repo.URL)
	}
	if err := m.disableAutoGC(ctx, tree); err != nil {
		return 0, 0, err
	}
	return written, removed, nil
}

// removeAllWritable is os.RemoveAll for a tree a build wrote, which may hold
// directories it made read-only: Go's module cache does, and so does any
// `chmod -R a-w`. A first failure makes every directory writable and tries
// once more.
func removeAllWritable(path string) error {
	err := os.RemoveAll(path)
	if err == nil {
		return nil
	}
	_ = filepath.WalkDir(path, func(p string, d os.DirEntry, walkErr error) error {
		if walkErr == nil && d.IsDir() {
			_ = os.Chmod(p, 0o700) // #nosec G302 -- a directory, about to be removed
		}
		return nil
	})
	return os.RemoveAll(path)
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(s), "\n")
	return line
}

func readWarmState(dir string) (WarmInfo, error) {
	// #nosec G304 -- dir is built by warmPath from validated parts, or read
	// from WarmDir's own listing; the name is a literal.
	data, err := os.ReadFile(filepath.Join(dir, warmStateName))
	if err != nil {
		return WarmInfo{}, err
	}
	var info WarmInfo
	if err := json.NewDecoder(bytes.NewReader(data)).Decode(&info); err != nil {
		return WarmInfo{}, err
	}
	return info, nil
}

// writeWarmState records what a warm workspace holds, beside its tree and
// outside what a step is given, by rename.
func writeWarmState(dir string, info WarmInfo) error {
	data, err := json.Marshal(info)
	if err != nil {
		return wrapErr(protocol.ErrSource, err, "encoding the warm workspace's state")
	}
	path := filepath.Join(dir, warmStateName)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o600); err != nil {
		return wrapErr(protocol.ErrSource, err, "writing %s", tmp)
	}
	if err := os.Rename(tmp, path); err != nil {
		return wrapErr(protocol.ErrSource, err, "writing %s", path)
	}
	return nil
}

// ListWarm returns every warm workspace on this machine, oldest use first.
//
// A directory with no state file is listed with what its path says and a zero
// time: it is one a crash left half made, and the oldest thing there.
func (m *Manager) ListWarm() ([]WarmInfo, error) {
	if m.cfg.WarmDir == "" {
		return nil, nil
	}
	var out []WarmInfo
	repos, err := readDirNames(m.cfg.WarmDir)
	if err != nil {
		return nil, err
	}
	for _, repo := range repos {
		runners, err := readDirNames(filepath.Join(m.cfg.WarmDir, repo))
		if err != nil {
			return nil, err
		}
		for _, runner := range runners {
			jobs, err := readDirNames(filepath.Join(m.cfg.WarmDir, repo, runner))
			if err != nil {
				return nil, err
			}
			for _, job := range jobs {
				dir := filepath.Join(m.cfg.WarmDir, repo, runner, job)
				info, _ := readWarmState(dir)
				info.Repo, info.Runner, info.Job, info.Path = repo, runner, job, dir
				info.InUse = m.warmBusy(dir)
				out = append(out, info)
			}
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].UsedAt.Before(out[j].UsedAt) })
	return out, nil
}

func readDirNames(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, wrapErr(protocol.ErrSource, err, "listing %s", dir)
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() {
			names = append(names, e.Name())
		}
	}
	return names, nil
}

// warmBusy reports a warm workspace a run holds, in this process or another.
func (m *Manager) warmBusy(dir string) bool {
	if len(m.warmGate(dir)) > 0 {
		return true
	}
	f, got, err := tryLockWarm(dir, false)
	if f != nil {
		_ = f.Close()
	}
	return err == nil && !got
}

// PruneWarm removes warm workspaces last used before cutoff, and returns the
// ones it removed. One a run holds is never removed, whatever its age.
//
// A warm workspace is a build's whole output and grows with it, so one nobody
// has used for a while is the largest thing on the machine that nothing
// needs. A zero cutoff removes every one not in use, for a disk below its
// floor.
func (m *Manager) PruneWarm(cutoff time.Time) ([]WarmInfo, error) {
	if m.readOnly {
		return nil, errReader
	}
	all, err := m.ListWarm()
	if err != nil {
		return nil, err
	}
	var gone []WarmInfo
	for _, info := range all {
		if !cutoff.IsZero() && !info.UsedAt.Before(cutoff) {
			continue
		}
		if !validWarmPart(info.Repo) || !validWarmPart(info.Runner) || !validWarmPart(info.Job) {
			continue // not a name this package made; not its to remove
		}
		gate := m.warmGate(info.Path)
		select {
		case gate <- struct{}{}:
		default:
			continue
		}
		lock, got, err := tryLockWarm(info.Path, false)
		if err != nil || !got {
			<-gate
			continue // a run in another process holds it
		}
		rmErr := removeAllWritable(info.Path)
		if lock != nil {
			_ = lock.Close()
		}
		<-gate
		if rmErr != nil {
			return gone, wrapErr(protocol.ErrSource, rmErr, "removing the warm workspace %s", info.Path)
		}
		gone = append(gone, info)
		// Leave no empty <repo>/<runner> behind; a directory that still
		// holds another job's workspace refuses, which is the right answer.
		_ = os.Remove(filepath.Dir(info.Path))
		_ = os.Remove(filepath.Dir(filepath.Dir(info.Path)))
	}
	return gone, nil
}
