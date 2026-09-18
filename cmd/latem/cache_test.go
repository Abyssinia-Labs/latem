package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// `latem cache rm` deletes a directory, and until now nothing tested it.
//
// The interesting cases are not the happy path. They are the three ways this
// command could delete the wrong thing: a name that exists on two runners, a
// name that exists on none, and a name shaped like a path. §14.2's caches are
// host directories, so every one of those ends in an `os.RemoveAll` on a path
// this command assembled.

// cacheConfigDir writes a config with the given native runners and returns the
// config directory, with the environment pointed at it.
//
// Short path: the loader refuses a socket path over 103 bytes (and macOS's
// per-test temp directories spend most of that before agent.sock is appended).
func cacheConfigDir(t *testing.T, runners ...string) string {
	t.Helper()
	home, err := os.MkdirTemp("/tmp", "latcache")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(home) })

	dir := filepath.Join(home, ".latem")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}

	var b strings.Builder
	b.WriteString(`schema_version = 1

[machine]
name = "cache-test"

[agent]
metrics_addr = ""

[repos.app]
url          = "https://git.example.invalid/app.git"
allowed_refs = ["main"]
`)
	for _, r := range runners {
		b.WriteString("\n[[runner]]\nname        = \"" + r + "\"\n")
		b.WriteString("executor    = \"native\"\nlabels      = [\"" + r + "\"]\nconcurrency = 1\n")
		b.WriteString("\n  [runner.native]\n  min_free_gb = 0.001\n")
	}
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}

	t.Setenv("HOME", home)
	t.Setenv("LATEM_CONFIG_DIR", dir)
	t.Cleanup(func() { global.configDir = "" })
	global.configDir = ""
	return dir
}

// seedCache creates a directory-backed cache with a file in it, and returns its
// path.
func seedCache(t *testing.T, configDir, runner, name string) string {
	t.Helper()
	dir := filepath.Join(configDir, "caches", runner, name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "contents"), []byte("warm"), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestCacheLs_ListsDirectoryBackedCaches(t *testing.T) {
	dir := cacheConfigDir(t, "ios")
	seedCache(t, dir, "ios", "derived-data")
	seedCache(t, dir, "ios", "cocoapods")

	stdout, _, err := execCommand(t, "cache", "ls")
	if err != nil {
		t.Fatalf("cache ls: %v", err)
	}
	for _, want := range []string{"derived-data", "cocoapods", "ios"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("listing does not mention %q:\n%s", want, stdout)
		}
	}

	// F19: the summary went through the tabwriter, so a separator row of empty
	// cells was padded into a line of trailing spaces and the summary's own
	// empty cells were padded under columns it has no values for.
	for i, line := range strings.Split(strings.TrimRight(stdout, "\n"), "\n") {
		if line != "" && strings.TrimSpace(line) == "" {
			t.Errorf("line %d is whitespace only: %q", i+1, line)
		}
		if line != strings.TrimRight(line, " ") {
			t.Errorf("line %d has trailing padding: %q", i+1, line)
		}
	}
	if !strings.Contains(stdout, "2 caches") {
		t.Errorf("no summary line:\n%s", stdout)
	}
}

// The summary read "1 caches".
func TestCacheLs_SummaryAgreesWithTheCount(t *testing.T) {
	dir := cacheConfigDir(t, "ios")
	seedCache(t, dir, "ios", "derived-data")

	stdout, _, err := execCommand(t, "cache", "ls")
	if err != nil {
		t.Fatalf("cache ls: %v", err)
	}
	if strings.Contains(stdout, "1 caches") {
		t.Errorf("the summary does not agree with its own count:\n%s", stdout)
	}
	if !strings.Contains(stdout, "1 cache,") {
		t.Errorf("no singular summary:\n%s", stdout)
	}
}

func TestCacheRm_DeletesTheCacheAndNothingElse(t *testing.T) {
	dir := cacheConfigDir(t, "ios")
	target := seedCache(t, dir, "ios", "derived-data")
	keep := seedCache(t, dir, "ios", "cocoapods")

	stdout, _, err := execCommand(t, "cache", "rm", "derived-data")
	if err != nil {
		t.Fatalf("cache rm: %v", err)
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Errorf("the cache is still there: %v", err)
	}
	if _, err := os.Stat(keep); err != nil {
		t.Errorf("rm took the other cache with it: %v", err)
	}
	// It says what it removed, because "done" is not a receipt for a delete.
	if !strings.Contains(stdout, "derived-data") {
		t.Errorf("rm did not name what it removed:\n%s", stdout)
	}
}

// A name on two runners is the case that could delete the wrong machine's
// cache. It must refuse and name the candidates rather than pick one.
func TestCacheRm_RefusesAnAmbiguousName(t *testing.T) {
	dir := cacheConfigDir(t, "ios", "android")
	first := seedCache(t, dir, "ios", "gradle")
	second := seedCache(t, dir, "android", "gradle")

	_, stderr, err := execCommand(t, "cache", "rm", "gradle")
	if err == nil {
		t.Fatal("an ambiguous name was accepted; one of two caches would have been deleted")
	}
	if code := exitCode(err); code != ExitUsage {
		t.Errorf("exit code = %d, want %d (usage): the fix is an argument, not a retry", code, ExitUsage)
	}
	for _, want := range []string{"ios", "android", "--runner"} {
		if !strings.Contains(err.Error()+stderr, want) {
			t.Errorf("the refusal does not mention %q: %v", want, err)
		}
	}
	for _, path := range []string{first, second} {
		if _, err := os.Stat(path); err != nil {
			t.Errorf("the refused rm deleted %s anyway", path)
		}
	}

	// And --runner is the way through.
	if _, _, err := execCommand(t, "cache", "rm", "gradle", "--runner", "android"); err != nil {
		t.Fatalf("cache rm --runner android: %v", err)
	}
	if _, err := os.Stat(second); !os.IsNotExist(err) {
		t.Error("--runner android did not delete android's cache")
	}
	if _, err := os.Stat(first); err != nil {
		t.Error("--runner android deleted ios's cache")
	}
}

func TestCacheRm_UnknownNameSaysWhereToLook(t *testing.T) {
	dir := cacheConfigDir(t, "ios")
	seedCache(t, dir, "ios", "derived-data")

	_, _, err := execCommand(t, "cache", "rm", "nosuchcache")
	if err == nil {
		t.Fatal("an unknown cache name was accepted")
	}
	if !strings.Contains(err.Error(), "cache ls") {
		t.Errorf("the error does not point at `latem cache ls`: %v", err)
	}

	// Named for a runner that has nothing under that name: the message should
	// say which runner it looked at, or an operator will retry the same
	// command believing the cache is gone.
	_, _, err = execCommand(t, "cache", "rm", "derived-data", "--runner", "android")
	if err == nil {
		t.Fatal("a cache on another runner was accepted")
	}
	if !strings.Contains(err.Error(), "android") {
		t.Errorf("the error does not name the runner asked about: %v", err)
	}
}

// The argument is a name, never a path. It is matched against what the cache
// manager found on disk, so a name shaped like a traversal matches nothing —
// and the test says so, because the alternative is an `os.RemoveAll` on
// `<caches>/<runner>/../../..`.
func TestCacheRm_APathShapedNameMatchesNothing(t *testing.T) {
	dir := cacheConfigDir(t, "ios")
	seedCache(t, dir, "ios", "derived-data")
	sentinel := filepath.Join(dir, "config.toml")

	for _, name := range []string{"../../..", "../derived-data", "/etc", "..", "./derived-data"} {
		t.Run(name, func(t *testing.T) {
			_, _, err := execCommand(t, "cache", "rm", name)
			if err == nil {
				t.Fatalf("cache rm %q was accepted", name)
			}
			if _, statErr := os.Stat(sentinel); statErr != nil {
				t.Fatalf("cache rm %q removed something outside a cache: %v", name, statErr)
			}
		})
	}
}
