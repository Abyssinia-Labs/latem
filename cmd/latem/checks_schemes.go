package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/Abyssinia-Labs/latem/internal/config"
)

// schemeDir is where Xcode writes a scheme that is shared, and therefore the
// only place a fresh clone can find one.
const schemeDir = "/xcshareddata/xcschemes/"

// checkSchemes fails a repository whose Xcode project has no committed scheme
// (A34).
//
// `xcodebuild -scheme X` fails with "does not contain a scheme named X" on a
// fresh clone unless the `.xcscheme` is committed. Xcode auto-creates schemes
// into `xcuserdata/`, which nobody commits — so this works on every developer's
// machine and fails on every runner, and §12 guarantees a fresh clone for every
// run, which guarantees the failure.
//
// It reads the bare mirror rather than a checkout, because that is the only copy
// of a repository this machine has between runs. INV-5 is untouched: nothing
// here reads a job definition, and a mirror is what §12 already fetched.
func (d *doctor) checkSchemes(ctx context.Context) []check {
	if d.cfg == nil || len(d.cfg.Repos) == 0 {
		return nil
	}
	// A34 scopes this to macOS runners: an Xcode scheme is not a property a
	// docker runner can act on, and reporting it there is noise.
	if len(d.runnersFor(config.ExecutorNative))+len(d.runnersFor(config.ExecutorTart)) == 0 {
		return nil
	}

	var out []check
	for _, name := range sortedStrings(d.cfg.Repos) {
		id, title := "repo.scheme."+name, "xcode scheme / "+name

		mirror, err := mirrorPathFor(d.cfg.Paths.Mirrors, name)
		if err != nil {
			out = append(out, warn(id, title, err.Error()))
			continue
		}
		if _, statErr := os.Stat(mirror); statErr != nil {
			// Nothing has been built yet. Saying so is more useful than
			// silence, because it tells an operator this check exists and
			// will start answering after the first run.
			out = append(out, skip(id, title, "no mirror yet; checked once this repo has been fetched"))
			continue
		}

		// HEAD, not a configured ref: a --mirror clone's HEAD is the origin's
		// default branch, and it is a literal rather than a value from
		// anywhere, so INV-3 has nothing to defend here.
		tree, gitErr := d.sys.Exec(ctx, "git", "-C", mirror, "ls-tree", "-r", "--name-only", "HEAD")
		if gitErr != nil {
			detail := firstLineOf(strings.TrimSpace(string(tree)))
			if detail == "" {
				detail = gitErr.Error()
			}
			out = append(out, warn(id, title, "could not read the mirror: "+detail))
			continue
		}

		projects, schemes := scanForSchemes(string(tree))
		switch {
		case len(projects) == 0:
			out = append(out, skip(id, title, "no Xcode project in the default branch"))
		case len(schemes) == 0:
			out = append(out, fail(id, title,
				fmt.Sprintf("%s has no committed scheme", strings.Join(projects, ", ")),
				fmt.Sprintf("# in a checkout of %s, with the scheme shared in Xcode:", name),
				"git add -f '*.xcodeproj/xcshareddata/xcschemes/*.xcscheme' && git commit").
				withNotes("In Xcode: Product → Scheme → Manage Schemes, tick Shared. It writes the "+
					"scheme to "+strings.TrimPrefix(schemeDir, "/")+" instead of xcuserdata/.",
					"This is the cheapest check in the document and it fails a 90-minute job in the "+
						"first ten seconds. If this repository's jobs never call `xcodebuild -scheme`, "+
						"it costs nothing to commit the scheme anyway."))
		default:
			out = append(out, pass(id, title,
				fmt.Sprintf("%d shared: %s", len(schemes), strings.Join(schemes, ", "))))
		}
	}
	return out
}

// scanForSchemes reads `git ls-tree -r --name-only` output.
//
// Returns the Xcode projects and workspaces it found, and the names of the
// schemes committed under any of them.
func scanForSchemes(tree string) (projects, schemes []string) {
	seenProject := map[string]bool{}
	for _, path := range strings.Split(tree, "\n") {
		path = strings.TrimSpace(path)
		if path == "" {
			continue
		}
		if container := containerOf(path); container != "" && !seenProject[container] {
			seenProject[container] = true
			projects = append(projects, container)
		}
		if i := strings.Index(path, schemeDir); i >= 0 && strings.HasSuffix(path, ".xcscheme") {
			name := strings.TrimSuffix(path[i+len(schemeDir):], ".xcscheme")
			// A scheme in a subdirectory of xcschemes/ is not a scheme Xcode
			// will find, so only a direct child counts.
			if name != "" && !strings.Contains(name, "/") {
				schemes = append(schemes, name)
			}
		}
	}
	return projects, schemes
}

// containerOf returns the `.xcodeproj` or `.xcworkspace` a path sits inside.
//
// `ls-tree -r` lists blobs, so a project appears only as the files within it —
// `App.xcodeproj/project.pbxproj`. A path that *is* a project bundle with
// nothing in it cannot happen in git, which stores no empty directories.
func containerOf(path string) string {
	for _, suffix := range []string{".xcodeproj/", ".xcworkspace/"} {
		if i := strings.Index(path, suffix); i >= 0 {
			return filepath.Base(path[:i+len(suffix)-1])
		}
	}
	return ""
}

// mirrorPathFor maps a repo key to its bare mirror, refusing anything that
// would not land directly inside the mirror directory.
//
// The same refusal as internal/source's own, restated rather than exported:
// `doctor` runs on a machine whose config may be the thing that is wrong, and a
// key like "../../etc" must not become a path this process stats.
func mirrorPathFor(mirrorDir, key string) (string, error) {
	path := filepath.Join(mirrorDir, key+".git")
	if filepath.Dir(path) != filepath.Clean(mirrorDir) {
		return "", fmt.Errorf("repo key %q escapes the mirror directory", key)
	}
	return path, nil
}
