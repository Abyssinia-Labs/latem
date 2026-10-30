package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"

	"github.com/Abyssinia-Labs/latem/internal/config"
	"github.com/Abyssinia-Labs/latem/internal/source"
)

// Reading a configured repository's files without a checkout (A57), for
// `latem workflows check --repo` and for doctor and setup asking what a
// machine's repositories need.

// treeSource says where readRepo reads a repository from.
type treeSource int

const (
	// fromMirror reads the agent's mirror and nothing else: offline, and
	// absent until the agent's first plan or run of the repository.
	fromMirror treeSource = iota
	// fromRemote reads the default branch from the repository itself,
	// through a temporary blobless clone.
	fromRemote
	// fromMirrorThenRemote reads the mirror when there is one.
	fromMirrorThenRemote
)

// repoFiles is a repository's default branch, read without a checkout.
type repoFiles struct {
	fsys  fs.FS
	sha   string
	where string // what it was read from, for a report
	close func()
}

// readRepo reads [repos.<key>]'s default branch. A reader never writes the
// agent's mirror: a fetch into it from here would race the agent's own. A
// seam: a test hands back an fstest.MapFS and touches no git.
var readRepo = func(ctx context.Context, cfg *config.Config, key string, from treeSource) (*repoFiles, error) {
	r, ok := cfg.Repos[key]
	if !ok {
		return nil, fmt.Errorf("there is no [repos.%s] in %s", key, cfg.Paths.File)
	}
	repo := source.Repo{Key: key, URL: r.URL}

	if from != fromRemote {
		reader, err := source.NewReader(source.Config{MirrorDir: cfg.Paths.Mirrors})
		if err != nil {
			return nil, err
		}
		m, err := reader.Open(repo)
		switch {
		case err == nil:
			fsys, sha, err := m.Tree(ctx, "HEAD", source.DefaultTreeLimits)
			if err != nil {
				return nil, err
			}
			return &repoFiles{fsys: fsys, sha: sha, where: "the agent's mirror of " + key, close: func() {}}, nil
		case !errors.Is(err, source.ErrNoMirror):
			return nil, err
		case from == fromMirror:
			return nil, fmt.Errorf("the agent has no mirror of %s yet; it makes one on its first plan or run. "+
				"Without --offline this reads %s itself", key, r.URL)
		}
	}

	dir, err := os.MkdirTemp("", "latem-read-")
	if err != nil {
		return nil, err
	}
	cleanup := func() { _ = os.RemoveAll(dir) }
	// The command's own environment: whatever credential this shell has —
	// an ssh agent, gh's helper — reads the repository, as `git clone`
	// typed here would.
	m, err := source.NewSnapshot(ctx, source.Config{}, repo, dir)
	if err != nil {
		cleanup()
		return nil, err
	}
	fsys, sha, err := m.Tree(ctx, "HEAD", source.DefaultTreeLimits)
	if err != nil {
		cleanup()
		return nil, err
	}
	return &repoFiles{fsys: fsys, sha: sha, where: r.URL, close: cleanup}, nil
}
