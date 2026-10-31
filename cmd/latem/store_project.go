package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"regexp"
	"strings"
)

// storeProject is what a project says about its stores (A65): the identifiers
// a store check asks Apple and Google about. Flags override it for a project
// whose files cannot be read this way.
type storeProject struct {
	dir      string // the app's directory in the tree, "." at its root
	name     string
	bundleID string
	pkg      string
	team     string
	// dynamic is app.config.ts or its like, which is code. latem does not run
	// a project's code to read an identifier out of it.
	dynamic string
	match   matchStore
}

// matchStore is the private repository fastlane match keeps signing in.
type matchStore struct {
	url    string
	branch string
}

// expoConfigs are the files Expo reads a project's config from, after app.json.
var expoConfigs = []string{"app.config.ts", "app.config.js", "app.config.mjs", "app.config.cjs"}

// findExpoApps lists the directories in fsys that hold an Expo app, shallowest
// first. A monorepo's app is rarely at its root, and asking for it would be
// asking a person for something the tree says.
//
// The walk stops at four levels and skips what a build generates: an app.json
// under node_modules is a dependency's, and a template's.
func findExpoApps(fsys fs.FS) ([]string, error) {
	const maxDepth = 4
	var found []string
	err := fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if p == "." {
				return err
			}
			return fs.SkipDir
		}
		if !d.IsDir() {
			return nil
		}
		name := d.Name()
		if p != "." && (strings.HasPrefix(name, ".") || name == "node_modules" || name == "vendor" ||
			name == "ios" || name == "android" || name == "dist" || name == "build") {
			return fs.SkipDir
		}
		if strings.Count(p, "/") >= maxDepth {
			return fs.SkipDir
		}
		if isExpoApp(fsys, p) {
			found = append(found, p)
		}
		return nil
	})
	return found, err
}

// isExpoApp reports whether dir holds an Expo config. An app.json without an
// expo key is React Native's own, or anything else's.
func isExpoApp(fsys fs.FS, dir string) bool {
	if data, err := fs.ReadFile(fsys, path.Join(dir, "app.json")); err == nil {
		var doc struct {
			Expo *json.RawMessage `json:"expo"`
		}
		if json.Unmarshal(data, &doc) == nil && doc.Expo != nil {
			return true
		}
	}
	for _, name := range expoConfigs {
		if _, err := fs.Stat(fsys, path.Join(dir, name)); err == nil {
			return true
		}
	}
	return false
}

// readStoreProject reads the project at dir, or the one Expo app in fsys when
// dir is "".
func readStoreProject(fsys fs.FS, dir string) (storeProject, error) {
	if dir == "" {
		apps, err := findExpoApps(fsys)
		if err != nil {
			return storeProject{}, err
		}
		switch len(apps) {
		case 0:
			return storeProject{}, errors.New("no Expo app here: no app.json with an expo key, and no app.config file. " +
				"Give the app's directory with --app, or its identifiers with --bundle-id and --package")
		case 1:
			dir = apps[0]
		default:
			return storeProject{}, usageErrorf("this tree has %d Expo apps (%s); say which with --app",
				len(apps), strings.Join(apps, ", "))
		}
	}
	dir = path.Clean(dir)

	p := storeProject{dir: dir}
	if err := p.readAppJSON(fsys); err != nil {
		return p, err
	}
	for _, name := range expoConfigs {
		if _, err := fs.Stat(fsys, path.Join(dir, name)); err == nil {
			p.dynamic = name
			break
		}
	}
	p.readFastlane(fsys)
	return p, nil
}

// readAppJSON reads the identifiers. No app.json is not an error: the app may
// be all app.config.ts, and the caller reports what is still unknown.
func (p *storeProject) readAppJSON(fsys fs.FS) error {
	data, err := fs.ReadFile(fsys, path.Join(p.dir, "app.json"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var doc struct {
		Expo struct {
			Name string `json:"name"`
			IOS  struct {
				BundleIdentifier string `json:"bundleIdentifier"`
			} `json:"ios"`
			Android struct {
				Package string `json:"package"`
			} `json:"android"`
		} `json:"expo"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return fmt.Errorf("%s: %w", path.Join(p.dir, "app.json"), err)
	}
	p.name = doc.Expo.Name
	p.bundleID = doc.Expo.IOS.BundleIdentifier
	p.pkg = doc.Expo.Android.Package
	return nil
}

// readFastlane reads the team and the signing store, from the app's directory
// or the tree's root, wherever the project keeps fastlane.
func (p *storeProject) readFastlane(fsys fs.FS) {
	read := func(name string) string {
		for _, dir := range []string{p.dir, "."} {
			if data, err := fs.ReadFile(fsys, path.Join(dir, "fastlane", name)); err == nil {
				return string(data)
			}
		}
		return ""
	}
	appfile, matchfile := read("Appfile"), read("Matchfile")
	p.team = fastlaneValue(appfile, "team_id")
	if t := fastlaneValue(matchfile, "team_id"); t != "" {
		p.team = t
	}
	p.match = matchStore{
		url:    fastlaneValue(matchfile, "git_url"),
		branch: fastlaneValue(matchfile, "git_branch"),
	}
}

var quotedString = regexp.MustCompile(`"([^"]*)"|'([^']*)'`)

// fastlaneValue reads key's value out of a Matchfile or an Appfile.
//
// These are Ruby, and this is not a Ruby parser. It takes the last string on
// the line that calls key, which is the value in `team_id("X")`,
// `app_identifier(["X"])` and `git_url(ENV["URL"] || "X")`, the three shapes
// fastlane's own templates write. Anything cleverer reads as nothing, and the
// report says what it could not find.
func fastlaneValue(text, key string) string {
	for line := range strings.SplitSeq(text, "\n") {
		rest, ok := strings.CutPrefix(strings.TrimSpace(line), key)
		if !ok || rest == "" || (rest[0] != '(' && rest[0] != ' ') {
			continue
		}
		quoted := quotedString.FindAllStringSubmatch(rest, -1)
		if len(quoted) == 0 {
			continue
		}
		last := quoted[len(quoted)-1]
		return last[1] + last[2]
	}
	return ""
}

var (
	// Reverse-DNS, as Apple and Google both require. They end up in a URL.
	storeIdentifier = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]*(\.[A-Za-z0-9][A-Za-z0-9_-]*)+$`)
	// git over https or ssh. A Matchfile is a file in a repository, and its URL
	// goes to git: anything else, a local path or a transport with a helper,
	// is not read.
	storeURL = regexp.MustCompile(`^(https://|ssh://|git@)[A-Za-z0-9]`)
)
