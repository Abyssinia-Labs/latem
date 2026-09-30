package actions

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"time"
)

const (
	// DirMode is tight because the cache holds code that is about to run.
	DirMode = 0o700
	// fetchTimeout bounds one resolve or one fetch. A marketplace repository
	// that will not answer must fail the run rather than hold the job's
	// budget open until the whole build times out with nothing in the log.
	fetchTimeout = 2 * time.Minute
	gitWaitDelay = 5 * time.Second
)

// Runner executes git. It is an interface so the tests that matter here — the
// argv ones — can assert what would have been run without a network.
type Runner interface {
	Run(ctx context.Context, dir string, args ...string) (string, error)
}

// Cache fetches actions into a content-addressed directory tree.
//
// Keyed on the resolved commit, never on the ref: `actions/checkout@v5` today
// and `@v5` next month are different code and get different directories, so a
// tag that moves can never be served from a cache warmed by what it used to
// point at.
type Cache struct {
	Root   string
	Policy Policy
	Git    Runner
}

// Resolved is one action, on disk.
type Resolved struct {
	Ref Ref
	// SHA is the commit the ref resolved to, and what the run log records.
	SHA string
	// Dir holds action.yml — the repository root, or Ref.Path inside it.
	Dir string
	// Rel is Dir relative to the cache root, in slash form. It is what a
	// compiled job carries instead of Dir: the cache is reachable at a
	// different absolute path inside a container than on the host, so only the
	// part below the root means the same thing in both.
	Rel string
	// Cached reports that nothing was fetched, so a log can say so.
	Cached bool
}

// Fetch resolves a reference to a commit, fetches it if it is not already on
// disk, and returns where it landed.
//
// The policy is checked first and the network is touched second, which is the
// ordering gate 1 uses for the same reason: a refusal should cost a string
// comparison, not a round trip to somebody else's server.
func (c *Cache) Fetch(ctx context.Context, r Ref) (Resolved, error) {
	if err := c.Policy.Permits(r); err != nil {
		return Resolved{}, err
	}

	sha := r.Ref
	if !r.Pinned() {
		var err error
		if sha, err = c.resolve(ctx, r); err != nil {
			return Resolved{}, err
		}
	}
	if !shaPattern.MatchString(sha) {
		// Unreachable for a pinned ref, and for a resolved one it means the
		// remote answered with something that is not a commit id. Refusing is
		// the only safe reading: the value is about to become a path.
		return Resolved{}, fmt.Errorf("%s resolved to %q, which is not a commit id", r, sha)
	}

	repoDir := c.repoDir(r, sha)
	actionDir := repoDir
	if r.Path != "" {
		actionDir = filepath.Join(repoDir, filepath.FromSlash(r.Path))
	}

	rel := path.Join(r.Host, r.Owner, r.Repo, sha, r.Path)

	if ok, err := isDir(repoDir); err != nil {
		return Resolved{}, err
	} else if ok {
		return Resolved{Ref: r, SHA: sha, Dir: actionDir, Rel: rel, Cached: true}, nil
	}

	if err := c.fetch(ctx, r, sha, repoDir); err != nil {
		return Resolved{}, err
	}
	if ok, err := isDir(actionDir); err != nil {
		return Resolved{}, err
	} else if !ok {
		return Resolved{}, fmt.Errorf("%s has no directory %q at %s", r.Slug(), r.Path, shortSHA(sha))
	}
	return Resolved{Ref: r, SHA: sha, Dir: actionDir, Rel: rel}, nil
}

// resolve turns a tag or branch into the commit it names right now.
//
// `git ls-remote` rather than an API call: it works against any host a git
// remote works against, needs no token for a public repository, and uses the
// credential the machine already has for one that is private — the same
// credential `latem doctor` checks. A tag is dereferenced with ^{} so an
// annotated tag resolves to the commit rather than to the tag object.
func (c *Cache) resolve(ctx context.Context, r Ref) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, fetchTimeout)
	defer cancel()

	out, err := c.Git.Run(ctx, "", "ls-remote", "--end-of-options", r.CloneURL(), r.Ref, r.Ref+"^{}")
	if err != nil {
		return "", fmt.Errorf("resolving %s: %w", r, err)
	}

	// Last match wins: ls-remote prints the tag object first and its
	// dereferenced commit second, and the commit is what a checkout needs.
	var sha string
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) == 2 && shaPattern.MatchString(f[0]) {
			sha = f[0]
		}
	}
	if sha == "" {
		return "", fmt.Errorf("%s has no ref %q", r.Slug(), r.Ref)
	}
	return sha, nil
}

// fetch places one commit on disk.
//
// Into a temporary sibling and then renamed, so that a directory under the
// cache root is either a complete checkout or absent. Two runs fetching the
// same action race harmlessly: both do the work, one rename wins, and the
// loser's tree is removed.
func (c *Cache) fetch(ctx context.Context, r Ref, sha, dest string) error {
	ctx, cancel := context.WithTimeout(ctx, fetchTimeout)
	defer cancel()

	if err := os.MkdirAll(filepath.Dir(dest), DirMode); err != nil {
		return fmt.Errorf("action cache: %w", err)
	}
	tmp, err := os.MkdirTemp(filepath.Dir(dest), ".fetch-*")
	if err != nil {
		return fmt.Errorf("action cache: %w", err)
	}
	defer func() { _ = os.RemoveAll(tmp) }()

	steps := [][]string{
		{"init", "--quiet"},
		{"remote", "add", "origin", "--end-of-options", r.CloneURL()},
		// One commit, no tags, no history. An action is a working tree to
		// this package, never a repository to walk.
		{"fetch", "--quiet", "--depth=1", "--no-tags", "origin", "--end-of-options", sha},
		// FETCH_HEAD is a constant, so this needs no terminator — which is
		// just as well, because amendment A40 found that `checkout --detach`
		// refuses one on git 2.43.
		{"checkout", "--quiet", "--detach", "FETCH_HEAD"},
	}
	for _, args := range steps {
		if _, err := c.Git.Run(ctx, tmp, args...); err != nil {
			return fmt.Errorf("fetching %s at %s: %w", r.Slug(), shortSHA(sha), err)
		}
	}

	// The action's own .git is dropped. Nothing downstream reads it, it is
	// the bulk of what was just fetched, and leaving a second repository
	// inside a tree the agent will later hand to a build is a way for a
	// `git` command in a step to find the wrong one.
	if err := os.RemoveAll(filepath.Join(tmp, ".git")); err != nil {
		return fmt.Errorf("action cache: %w", err)
	}

	if err := os.Rename(tmp, dest); err != nil {
		if ok, statErr := isDir(dest); statErr == nil && ok {
			// Lost the race. The winner's tree is at the same commit.
			return nil
		}
		return fmt.Errorf("action cache: %w", err)
	}
	return nil
}

// repoDir is where one commit of one repository lives.
//
// Every element has been validated to a shape by [ParseRef], and the SHA by
// [shaPattern], so no part of this path is attacker-chosen text — which is
// what makes joining it safe to read as well as to write.
func (c *Cache) repoDir(r Ref, sha string) string {
	return filepath.Join(c.Root, r.Host, r.Owner, r.Repo, sha)
}

func isDir(path string) (bool, error) {
	info, err := os.Stat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("action cache: %w", err)
	}
	return info.IsDir(), nil
}

func shortSHA(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

// GitRunner is the production [Runner].
//
// It is the discipline internal/source/git.go describes, in the one other
// place the agent shells out to git: argv is a slice, there is no shell, and
// every element is either a literal written here or a value this package has
// validated to a shape. The environment is emptied of anything that could
// turn a fetch into a prompt or a credential leak.
type GitRunner struct{ Path string }

// Run executes git.
func (g GitRunner) Run(ctx context.Context, dir string, args ...string) (string, error) {
	argv := args
	if dir != "" {
		argv = append([]string{"-C", dir}, args...)
	}
	git := g.Path
	if git == "" {
		git = "git"
	}
	// #nosec G204 -- the INV-3 surface, safe by construction: git is resolved
	// from PATH or from config and is never workflow-derived, argv is a slice
	// passed to execve with no shell in the path, and every element is a
	// literal or a value ParseRef/shaPattern has already constrained.
	cmd := exec.CommandContext(ctx, git, argv...)
	cmd.Dir = ""
	// Nothing here reads input: a git that cannot ask for a password is a git
	// that fails instead of hanging a build for two minutes.
	cmd.Stdin = nil
	cmd.Env = append(os.Environ(),
		"GIT_TERMINAL_PROMPT=0",
		"GIT_ASKPASS=",
		"GCM_INTERACTIVE=never",
		// An action's repository is not a place to be running hooks or
		// reading a developer's global config from.
		"GIT_CONFIG_NOSYSTEM=1",
	)
	cmd.WaitDelay = gitWaitDelay

	out, err := cmd.Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) && len(ee.Stderr) > 0 {
			return "", fmt.Errorf("git %s: %s", args[0], strings.TrimSpace(firstLines(string(ee.Stderr))))
		}
		return "", fmt.Errorf("git %s: %w", args[0], err)
	}
	return strings.TrimSpace(string(out)), nil
}

// firstLines keeps a git error to something a log line can hold.
func firstLines(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) > 3 {
		lines = lines[:3]
	}
	return strings.Join(lines, "; ")
}
