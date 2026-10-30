package source

import (
	"context"
	"os"
	"path/filepath"
	"strings"

	"github.com/Abyssinia-Labs/latem/internal/registry"
	"github.com/Abyssinia-Labs/latem/pkg/protocol"
)

// Mirror is one repo's persistent bare clone.
//
// A Mirror is a handle, not a lock. Holding one grants no exclusivity: reads
// out of a mirror (listing refs, resolving a SHA, cloning a workspace) are
// concurrent by design, because git objects are immutable and the whole point
// of the model is that two runs on one repo do not queue behind each other.
type Mirror struct {
	mgr  *Manager
	repo Repo
	path string
}

// Compile-time proof that a Mirror is what registry.NormalizeRef consumes.
// The interface exists so normalization stays pure and testable without a git
// repository; this assertion is what catches the signature drifting apart.
var _ registry.RefLister = (*Mirror)(nil)

// Path returns the mirror's directory.
func (mi *Mirror) Path() string { return mi.path }

// Repo returns the repo this mirror belongs to.
func (mi *Mirror) Repo() Repo { return mi.repo }

// Fetch is gate 6 of RUNNER_SPEC §10: make the mirror exist and make it fresh.
//
// The per-repo mutex is held for the whole of this call and released before it
// returns. Concurrent `git fetch` into one bare repository is not reliably
// safe — two fetches racing on packed-refs and on the same tmp pack names — so
// fetches serialise. Nothing else does. In particular the clone in
// [Mirror.Checkout] runs outside this lock, so a repo with four queued runs
// pays for one fetch and then clones four workspaces in parallel.
func (m *Manager) Fetch(ctx context.Context, repo Repo) (*Mirror, error) {
	if m.readOnly {
		return nil, errReader
	}
	if err := repo.validate(); err != nil {
		return nil, err
	}
	path, err := m.mirrorPath(repo.Key)
	if err != nil {
		return nil, err
	}

	lock := m.repoLock(repo.Key)
	lock.Lock()
	defer lock.Unlock()

	if _, statErr := os.Stat(path); statErr != nil {
		if !os.IsNotExist(statErr) {
			return nil, wrapErr(protocol.ErrSource, statErr, "inspecting mirror %s", path)
		}
		if err := m.createMirror(ctx, repo, path); err != nil {
			return nil, err
		}
		return &Mirror{mgr: m, repo: repo, path: path}, nil
	}

	// gc.auto before fetch, not after. `git fetch` is one of the commands that
	// triggers automatic gc, so a mirror created before this rule existed —
	// or by an older agent — must be repaired before the fetch that would
	// otherwise collect the commit an in-flight run is building.
	if err := m.disableAutoGC(ctx, path); err != nil {
		return nil, err
	}

	// Keep the mirror pointed at what config currently says. A repo whose URL
	// changed in config.toml otherwise keeps fetching from the old host
	// forever, and the symptom is a ref that exists upstream and cannot be
	// found here.
	if _, err := m.run(ctx, path, "remote", "set-url", "origin", "--", repo.URL); err != nil {
		return nil, gitErr(err, "pointing mirror %s at %s", path, repo.URL)
	}

	if _, err := m.run(ctx, path, "fetch", "--prune", "--tags", "--", "origin"); err != nil {
		return nil, gitErr(err, "fetching %s", repo.Key)
	}

	return &Mirror{mgr: m, repo: repo, path: path}, nil
}

// createMirror clones a repo into MirrorDir for the first time.
//
// The clone lands on a temporary name and is renamed into place. A crash or a
// killed agent partway through cloning a large repo would otherwise leave a
// truncated object store at the final path, where the next run's os.Stat sees
// a directory, skips creation, and fetches into a repository that is missing
// most of its objects. Renaming makes the mirror's existence atomic.
func (m *Manager) createMirror(ctx context.Context, repo Repo, path string) error {
	tmp, err := os.MkdirTemp(m.cfg.MirrorDir, incomingPrefix+repo.Key+"-*")
	if err != nil {
		return wrapErr(protocol.ErrSource, err, "creating a staging directory for %s", repo.Key)
	}
	// git clone refuses a destination that already exists and is non-empty;
	// MkdirTemp made it, so hand git a name inside it instead.
	staging := filepath.Join(tmp, "mirror"+mirrorSuffix)
	defer func() { _ = os.RemoveAll(tmp) }()

	if _, err := m.run(ctx, "", "clone", "--mirror", "--", repo.URL, staging); err != nil {
		return gitErr(err, "mirroring %s", repo.Key)
	}
	if err := m.disableAutoGC(ctx, staging); err != nil {
		return err
	}
	if err := os.Rename(staging, path); err != nil {
		return wrapErr(protocol.ErrSource, err, "installing mirror at %s", path)
	}
	return nil
}

// disableAutoGC turns off git's automatic garbage collection for a repository.
//
// RUNNER_SPEC §12 calls this not optional, and the reasoning is worth keeping
// next to the code. A force-push followed by `fetch --prune` can make the
// previously-fetched tip unreachable. Automatic gc is then free to collect
// it — mid-run, forty minutes into a build, on a commit a run already
// resolved and is building. The mirror is instead compacted by an explicit
// `git gc --prune=30.days.ago` from the daily ticker (§14.4), at a moment when
// no run holds the mirror.
func (m *Manager) disableAutoGC(ctx context.Context, dir string) error {
	if _, err := m.run(ctx, dir, "config", "gc.auto", "0"); err != nil {
		return gitErr(err, "disabling automatic gc in %s", dir)
	}
	return nil
}

// HasRef reports whether a fully-qualified ref exists in the mirror. It
// implements [registry.RefLister] for gate 7.
//
// A ref that does not exist is exit 1 and reports (false, nil); anything else
// is an error, so "no such branch" stays E_INVALID_REF instead of being
// promoted to a retryable E_SOURCE.
//
// The refs/ prefix is required rather than assumed. registry.NormalizeRef
// always supplies one, so a caller that does not has misunderstood the
// contract, and `show-ref --verify` would otherwise quietly report false for a
// short name that does exist — a normalization bug that presents as a missing
// branch.
func (mi *Mirror) HasRef(ctx context.Context, fullyQualified string) (bool, error) {
	if err := registry.ValidateRef(fullyQualified); err != nil {
		return false, wrapErr(protocol.ErrInvalidRef, err, "listing refs in %s", mi.repo.Key)
	}
	if !strings.HasPrefix(fullyQualified, "refs/") {
		return false, codeErr(protocol.ErrInvalidRef,
			"HasRef needs a fully-qualified ref, got %q", fullyQualified)
	}

	ok, err := mi.mgr.runQuery(ctx, mi.path, "show-ref", "--verify", "--quiet", "--", fullyQualified)
	if err != nil {
		return false, gitErr(err, "checking %s in %s", fullyQualified, mi.repo.Key)
	}
	return ok, nil
}

// Resolve is gate 9: turn a normalized ref into the immutable commit it names.
//
// The `^{commit}` peel is what makes annotated tags behave. `refs/tags/v1.0`
// on an annotated tag names a *tag object*, not a commit; checking that out
// produces something surprising, and recording it as the run's SHA records a
// value that `git log` will not accept. The peel resolves to the commit or
// fails loudly, which are the only two acceptable outcomes.
//
// The separator here is "--end-of-options", not "--". `git rev-parse --` means
// "everything after this is a pathspec", so the spec's own idiom does not
// apply to this one command; --end-of-options (git 2.24+) is the form that
// says "no more options" without saying "these are paths". The ref has already
// passed registry.ValidateRef and so cannot begin with a dash, but INV-3 is
// not satisfied by the argument being safe — it is satisfied by the call site
// making it impossible for the argument to be read as an option.
func (mi *Mirror) Resolve(ctx context.Context, normalizedRef string) (string, error) {
	if err := registry.ValidateRef(normalizedRef); err != nil {
		return "", wrapErr(protocol.ErrInvalidRef, err, "resolving a ref in %s", mi.repo.Key)
	}

	out, err := mi.mgr.run(ctx, mi.path,
		"rev-parse", "--verify", "--end-of-options", normalizedRef+"^{commit}")
	if err != nil {
		return "", gitErr(err, "resolving %s in %s", normalizedRef, mi.repo.Key)
	}
	if err := validateSHA(out); err != nil {
		return "", err
	}
	return out, nil
}

// FetchCommit fetches one commit by its name, for a job pinned to a commit no
// ref the mirror fetched still reaches: a re-run of a pull request that has
// since merged, whose refs/pull/N/* GitHub deleted, or of one another machine
// built while the merge ref pointed somewhere this one never saw. GitHub serves
// a commit by its name, which is how actions/checkout fetches one. It lands
// under no ref; gc.auto is off and the daily gc keeps an unreachable object for
// thirty days, longer than any run that wants it.
//
// It takes the per-repo lock that Fetch holds, because it is a fetch into the
// same bare repository.
func (mi *Mirror) FetchCommit(ctx context.Context, sha string) error {
	if err := validateSHA(sha); err != nil {
		return err
	}
	lock := mi.mgr.repoLock(mi.repo.Key)
	lock.Lock()
	defer lock.Unlock()
	if _, err := mi.mgr.run(ctx, mi.path, "fetch", "--no-tags", "--", "origin", sha); err != nil {
		return gitErr(err, "fetching commit %s of %s", sha, mi.repo.Key)
	}
	return nil
}

// HasCommit reports whether a commit is present in the mirror.
//
// It exists for the daily gc ticker and for the assertion that a force-pushed
// commit survived a prune, which is the property gc.auto 0 buys.
//
// `rev-parse --verify --quiet` rather than `cat-file -e`: cat-file exits 128
// on a name it cannot resolve, which is indistinguishable from a broken
// repository, whereas rev-parse's --quiet turns "no such object" into a clean
// exit 1.
func (mi *Mirror) HasCommit(ctx context.Context, sha string) (bool, error) {
	if err := validateSHA(sha); err != nil {
		return false, err
	}
	ok, err := mi.mgr.runQuery(ctx, mi.path,
		"rev-parse", "--verify", "--quiet", "--end-of-options", sha+"^{commit}")
	if err != nil {
		return false, gitErr(err, "checking for %s in %s", sha, mi.repo.Key)
	}
	return ok, nil
}
