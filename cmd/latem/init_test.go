package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Abyssinia-Labs/latem/internal/config"
)

// `latem init` is the first command a machine runs, and its template is what
// every config on every machine starts from — so the property that matters is
// not what the file looks like but that the loader accepts it.
//
// The command already parses back what it wrote and calls a failure "a bug in
// `latem init`", which is the right runtime behaviour and the wrong place to
// find out: that check runs on the operator's machine, once, while they are
// trying to install something. These tests move the discovery to CI.
//
// A38 is the precedent. A `~` in `caches[].path` was in the spec's own example
// and in both reference registries, and no executor accepted it — a documented
// form that had never worked, found by the first automated run rather than by
// the eleven manual ones.

// emptyConfigDir is a short, *empty* config directory with the environment
// pointed at it.
//
// Short because the loader refuses a socket path over 103 bytes and macOS's
// `t.TempDir()` under /var/folders/… spends most of that budget before the
// agent.sock is appended — a real constraint the config package checks for,
// and one this test should respect rather than route around. Empty because
// `init` is the command that creates the config, so it is the one helper that
// must not arrive with one already written.
func emptyConfigDir(t *testing.T) string {
	t.Helper()
	home, err := os.MkdirTemp("/tmp", "latinit")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(home) })

	dir := filepath.Join(home, ".latem")
	t.Setenv("HOME", home)
	t.Setenv("LATEM_CONFIG_DIR", dir)
	t.Cleanup(func() { global.configDir = "" })
	global.configDir = ""
	return dir
}

// shortDir is the same, without touching the environment: for the template
// tests, which parse a rendered config against a directory rather than running
// a command.
func shortDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "lattpl")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return dir
}

// renderFor is the template as `init` would render it on a given platform.
func renderFor(goos, goarch string, repos []repoSpec, refs []string) string {
	return renderInitConfig(initParams{
		Machine: "test-machine",
		GOOS:    goos,
		GOARCH:  goarch,
		Repos:   repos,
		Refs:    refs,
	})
}

func TestInitTemplate_LoadsOnEveryPlatformItTargets(t *testing.T) {
	repos := []repoSpec{{Name: "myapp", URL: "git@github.com:me/myapp.git"}}

	for _, tc := range []struct {
		goos, goarch string
		wantExecutor config.Executor
		wantLabel    string
	}{
		// Native on macOS, not tart: amendment A31, and the reference config
		// ships the tart block commented out. If this flips, a stock install
		// starts pulling a 69 GB image.
		{"darwin", "arm64", config.ExecutorNative, "macos"},
		{"darwin", "amd64", config.ExecutorNative, "macos"},
		{"linux", "amd64", config.ExecutorDocker, "linux"},
		{"linux", "arm64", config.ExecutorDocker, "linux"},
	} {
		t.Run(tc.goos+"/"+tc.goarch, func(t *testing.T) {
			dir := shortDir(t)
			body := renderFor(tc.goos, tc.goarch, repos, []string{"main"})

			cfg, err := config.Parse([]byte(body), config.Options{Dir: dir})
			if err != nil {
				t.Fatalf("the template `latem init` writes does not load:\n%v\n\n%s", err, body)
			}

			if len(cfg.Runners) != 1 {
				t.Fatalf("%d runners, want 1", len(cfg.Runners))
			}
			runner := cfg.Runners[0]
			if runner.Executor != tc.wantExecutor {
				t.Errorf("executor = %q, want %q", runner.Executor, tc.wantExecutor)
			}
			if !contains(runner.Labels, tc.wantLabel) {
				t.Errorf("labels = %v, want one of them %q", runner.Labels, tc.wantLabel)
			}
			// The arch is a label because a job's `runs_on` is how a registry
			// picks a machine, and "some mac" is not specific enough on a
			// fleet with both.
			if !contains(runner.Labels, tc.goarch) {
				t.Errorf("labels = %v, want the architecture %q among them", runner.Labels, tc.goarch)
			}
			// §13.3: concurrency 1 on native, because parallel Xcode builds
			// contend on DerivedData and the keychain.
			if tc.wantExecutor == config.ExecutorNative && runner.Concurrency != 1 {
				t.Errorf("native concurrency = %d, want 1 (§13.3)", runner.Concurrency)
			}
		})
	}
}

func TestInitTemplate_TheReposBecomeAnAllowlist(t *testing.T) {
	dir := shortDir(t)
	body := renderFor("darwin", "arm64", []repoSpec{
		{Name: "myapp", URL: "git@github.com:me/myapp.git"},
		{Name: "other", URL: "https://example.invalid/other.git"},
	}, []string{"main", "release/*"})

	cfg, err := config.Parse([]byte(body), config.Options{Dir: dir})
	if err != nil {
		t.Fatalf("Parse: %v\n\n%s", err, body)
	}

	if len(cfg.Repos) != 2 {
		t.Fatalf("%d repos, want 2: %v", len(cfg.Repos), cfg.Repos)
	}
	// Keyed by the name the operator chose, not by anything derived from the
	// URL: two remotes of one project is a normal thing to have.
	app, ok := cfg.Repos["myapp"]
	if !ok {
		t.Fatalf("repos are keyed by %v, want a \"myapp\" key", keysOf(cfg.Repos))
	}
	if app.URL != "git@github.com:me/myapp.git" {
		t.Errorf("url = %q", app.URL)
	}
	if strings.Join(app.AllowedRefs, ",") != "main,release/*" {
		t.Errorf("allowed_refs = %v, want both refs in order", app.AllowedRefs)
	}
}

// The name is a value a person typed, and it lands in a TOML string. A machine
// called `my "mac"` must produce a config that still loads — or `init` has
// turned a naming choice into a broken installation.
func TestInitTemplate_AwkwardNamesStillLoad(t *testing.T) {
	for _, name := range []string{
		`mac-mini-01`,
		`mini.office`,
		`mac_mini`,
	} {
		t.Run(name, func(t *testing.T) {
			dir := shortDir(t)
			body := renderInitConfig(initParams{
				Machine: name,
				GOOS:    "darwin",
				GOARCH:  "arm64",
				Repos:   []repoSpec{{Name: "app", URL: "git@github.com:me/app.git"}},
				Refs:    []string{"main"},
			})
			cfg, err := config.Parse([]byte(body), config.Options{Dir: dir})
			if err != nil {
				t.Fatalf("machine name %q produced a config that does not load:\n%v", name, err)
			}
			if cfg.Machine.Name != name {
				t.Errorf("machine name = %q, want %q", cfg.Machine.Name, name)
			}
		})
	}
}

func TestParseRepoFlags(t *testing.T) {
	ok, err := parseRepoFlags([]string{"myapp=git@github.com:me/myapp.git", " other = https://x.invalid/o.git "})
	if err != nil {
		t.Fatalf("parseRepoFlags: %v", err)
	}
	if len(ok) != 2 {
		t.Fatalf("%d specs, want 2", len(ok))
	}
	// Trimmed, because `--repo name = url` is what a person types.
	if ok[1].Name != "other" || ok[1].URL != "https://x.invalid/o.git" {
		t.Errorf("spec = %+v, want the spaces trimmed", ok[1])
	}

	for _, bad := range []struct {
		name string
		in   []string
		want string
	}{
		{"no equals", []string{"myapp"}, "not <name>=<url>"},
		{"empty name", []string{"=git@github.com:me/app.git"}, "not <name>=<url>"},
		{"empty url", []string{"myapp="}, "not <name>=<url>"},
		{"only spaces", []string{"  =  "}, "not <name>=<url>"},
		// Two entries under one name would make `latem run myapp` ambiguous,
		// and the second would silently win in a map.
		{"repeated name", []string{"a=git@x/1.git", "a=git@x/2.git"}, "given twice"},
	} {
		t.Run(bad.name, func(t *testing.T) {
			if _, err := parseRepoFlags(bad.in); err == nil {
				t.Fatalf("parseRepoFlags(%q) was accepted", bad.in)
			} else if !strings.Contains(err.Error(), bad.want) {
				t.Errorf("error %q does not mention %q", err, bad.want)
			}
		})
	}
}

// ---------------------------------------------------------------- the command

func TestInitCommand_WritesAConfigThatLoads(t *testing.T) {
	dir := emptyConfigDir(t)

	stdout, _, err := execCommand(t, "init", "--repo", "myapp=git@github.com:me/myapp.git")
	if err != nil {
		t.Fatalf("init: %v\n%s", err, stdout)
	}

	path := filepath.Join(dir, config.FileName)
	info, statErr := os.Stat(path)
	if statErr != nil {
		t.Fatalf("no config written: %v", statErr)
	}
	// §8: the directory holds secrets.env and the machine token, and the file
	// itself is the allowlist.
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("config mode = %o, want 600", perm)
	}
	if dirInfo, err := os.Stat(dir); err != nil {
		t.Fatal(err)
	} else if perm := dirInfo.Mode().Perm(); perm != 0o700 {
		t.Errorf("config dir mode = %o, want 700 (§8)", perm)
	}

	if _, err := config.Load(config.Options{Dir: dir}); err != nil {
		t.Fatalf("the config `latem init` wrote does not load: %v", err)
	}
	// The output is the operator's next move, and `doctor` is deliberately
	// first: it is what turns a broken machine into a list of fixes.
	if !strings.Contains(stdout, "latem doctor") {
		t.Errorf("init did not point at doctor:\n%s", stdout)
	}
}

func TestInitCommand_RefusesWithoutARepo(t *testing.T) {
	emptyConfigDir(t)

	_, _, err := execCommand(t, "init")
	if err == nil {
		t.Fatal("init with no --repo was accepted; a config with no repo can build nothing")
	}
	if code := exitCode(err); code != ExitUsage {
		t.Errorf("exit code = %d, want %d (usage)", code, ExitUsage)
	}
}

func TestInitCommand_WillNotOverwriteWithoutForce(t *testing.T) {
	dir := emptyConfigDir(t)
	path := filepath.Join(dir, config.FileName)

	if _, _, err := execCommand(t, "init", "--repo", "a=git@x/a.git"); err != nil {
		t.Fatalf("first init: %v", err)
	}
	first, err := os.ReadFile(path) //nolint:gosec // a path this test just wrote
	if err != nil {
		t.Fatal(err)
	}

	if _, _, err := execCommand(t, "init", "--repo", "b=git@x/b.git"); err == nil {
		t.Fatal("a second init overwrote an existing config without --force")
	}
	after, err := os.ReadFile(path) //nolint:gosec // a path this test just wrote
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(first) {
		t.Error("the refused init changed the file anyway")
	}

	// And --force is the way through, because a machine being reconfigured is
	// a normal thing.
	if _, _, err := execCommand(t, "init", "--repo", "b=git@x/b.git", "--force"); err != nil {
		t.Fatalf("--force was refused: %v", err)
	}
	forced, err := os.ReadFile(path) //nolint:gosec // a path this test just wrote
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(forced), "git@x/b.git") {
		t.Error("--force did not write the new repo")
	}
}

// --print exists so an operator can read what they are about to be given. It
// must therefore not give it to them: A39 was a plist that could never start,
// caught by exactly this kind of flag.
func TestInitCommand_PrintWritesNothing(t *testing.T) {
	dir := emptyConfigDir(t)

	stdout, _, err := execCommand(t, "init", "--repo", "a=git@x/a.git", "--print")
	if err != nil {
		t.Fatalf("init --print: %v", err)
	}
	if !strings.Contains(stdout, "schema_version") {
		t.Errorf("--print did not print a config:\n%s", stdout)
	}
	if _, err := os.Stat(filepath.Join(dir, config.FileName)); !os.IsNotExist(err) {
		t.Error("--print wrote the file it was asked to only show")
	}
	// And what it printed is the thing that would have been written.
	if _, err := config.Parse([]byte(stdout), config.Options{Dir: dir}); err != nil {
		t.Errorf("--print's output does not load: %v", err)
	}
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

func keysOf[T any](m map[string]T) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
