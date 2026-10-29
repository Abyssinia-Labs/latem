package workflow

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Abyssinia-Labs/latem/internal/registry"
	"github.com/Abyssinia-Labs/latem/pkg/protocol"
)

// workspace builds a throwaway checkout. Keys are repository-relative paths.
func workspace(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range files {
		full := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

const nativeJobs = `
version: 1
jobs:
  test:
    runs_on: [macos]
    steps:
      - name: native
        run: make native
`

const wfCI = `
name: CI
on: [push]
jobs:
  test:
    runs-on: [self-hosted, macos]
    steps:
      - run: make from-workflow
  ios-release:
    runs-on: [self-hosted, macos]
    steps:
      - run: make ios
`

// codeOf reads the stable code off a refusal. There is one error type in play
// — [registry.Error] — which is the whole point of not having two.
func codeOf(_ *testing.T, err error) protocol.ErrorCode { return registry.CodeOf(err) }

// The settled precedence: a name in both places is the native one.
func TestNativeRegistryWins(t *testing.T) {
	ws := workspace(t, map[string]string{
		registry.Path:              nativeJobs,
		".github/workflows/ci.yml": wfCI,
	})
	reg, src, err := Resolve(t.Context(), ws, "test", testScope())
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if !src.Native || src.Path != registry.Path {
		t.Errorf("source = %+v, want the native registry", src)
	}
	if got := reg.Jobs["test"].Steps[0].Run; got != "make native" {
		t.Errorf("ran %q, want the native job", got)
	}
}

// And a name only the workflow has is still reachable, which is what makes
// adoption incremental rather than all-or-nothing.
func TestWorkflowJobResolvesAlongsideNative(t *testing.T) {
	ws := workspace(t, map[string]string{
		registry.Path:              nativeJobs,
		".github/workflows/ci.yml": wfCI,
	})
	reg, src, err := Resolve(t.Context(), ws, "ios-release", testScope())
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if src.Native {
		t.Errorf("source = %+v, want the workflow", src)
	}
	if src.Path != ".github/workflows/ci.yml" {
		t.Errorf("source path = %q", src.Path)
	}
	if got := reg.Jobs["ios-release"].Steps[0].Run; got != "make ios" {
		t.Errorf("ran %q", got)
	}
}

func TestWorkflowOnlyRepository(t *testing.T) {
	ws := workspace(t, map[string]string{".github/workflows/ci.yml": wfCI})
	reg, src, err := Resolve(t.Context(), ws, "test", testScope())
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if src.Native || reg.Jobs["test"] == nil {
		t.Errorf("source = %+v, reg = %+v", src, reg)
	}
}

// The name is the only selector, so it has to be unambiguous.
func TestAmbiguousJobNameNamesBothFiles(t *testing.T) {
	ws := workspace(t, map[string]string{
		".github/workflows/a.yml": "jobs:\n  build:\n    runs-on: [macos]\n    steps:\n      - run: make a\n",
		".github/workflows/b.yml": "jobs:\n  build:\n    runs-on: [macos]\n    steps:\n      - run: make b\n",
	})
	_, _, err := Resolve(t.Context(), ws, "build", testScope())
	if err == nil {
		t.Fatal("two workflows declaring one name must not resolve")
	}
	for _, want := range []string{"a.yml", "b.yml", registry.Path} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error should mention %s: %v", want, err)
		}
	}
	if got := codeOf(t, err); got != protocol.ErrUnknownJob {
		t.Errorf("code = %s, want %s", got, protocol.ErrUnknownJob)
	}
}

// Declaring it natively is the documented fix, so it had better work.
func TestNativeResolvesAnAmbiguousWorkflowName(t *testing.T) {
	ws := workspace(t, map[string]string{
		registry.Path:             "version: 1\njobs:\n  build:\n    runs_on: [macos]\n    steps:\n      - run: make native\n",
		".github/workflows/a.yml": "jobs:\n  build:\n    runs-on: [macos]\n    steps:\n      - run: make a\n",
		".github/workflows/b.yml": "jobs:\n  build:\n    runs-on: [macos]\n    steps:\n      - run: make b\n",
	})
	reg, src, err := Resolve(t.Context(), ws, "build", testScope())
	if err != nil {
		t.Fatalf("the documented fix should resolve: %v", err)
	}
	if !src.Native || reg.Jobs["build"].Steps[0].Run != "make native" {
		t.Errorf("src = %+v", src)
	}
}

func TestUnknownJobListsWhatExists(t *testing.T) {
	ws := workspace(t, map[string]string{
		registry.Path:              nativeJobs,
		".github/workflows/ci.yml": wfCI,
	})
	_, _, err := Resolve(t.Context(), ws, "nope", testScope())
	if err == nil {
		t.Fatal("expected a refusal")
	}
	for _, want := range []string{"test", "ios-release"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error should list %q: %v", want, err)
		}
	}
}

func TestNoRegistryAtAll(t *testing.T) {
	ws := workspace(t, map[string]string{"README.md": "hi"})
	_, _, err := Resolve(t.Context(), ws, "test", testScope())
	if got := codeOf(t, err); got != protocol.ErrNoRegistry {
		t.Errorf("code = %s, want %s (%v)", got, protocol.ErrNoRegistry, err)
	}
}

// A broken jobs.yaml is a broken jobs.yaml. Falling through to a workflow would
// turn a syntax error into a silent change of which file defines the build.
func TestMalformedNativeRegistryDoesNotFallThrough(t *testing.T) {
	ws := workspace(t, map[string]string{
		registry.Path:              "version: 1\njobs:\n  test:\n    runz_on: oops\n",
		".github/workflows/ci.yml": wfCI,
	})
	_, _, err := Resolve(t.Context(), ws, "test", testScope())
	if err == nil {
		t.Fatal("a malformed native registry must be reported, not bypassed")
	}
	if strings.Contains(err.Error(), "from-workflow") {
		t.Error("it fell through to the workflow")
	}
}

// INV-5's reasoning applied to the new reader: a workspace holds whatever the
// repository put there, and .github/workflows being a link out of the tree is
// a plain thing to commit.
func TestWorkflowsDirectoryCannotEscapeTheWorkspace(t *testing.T) {
	outside := t.TempDir()
	if err := os.MkdirAll(filepath.Join(outside, "workflows"), 0o755); err != nil {
		t.Fatal(err)
	}
	evil := "jobs:\n  build:\n    runs-on: [macos]\n    steps:\n      - run: curl evil.example\n"
	if err := os.WriteFile(filepath.Join(outside, "workflows", "x.yml"), []byte(evil), 0o600); err != nil {
		t.Fatal(err)
	}

	ws := workspace(t, map[string]string{"README.md": "hi"})
	if err := os.MkdirAll(filepath.Join(ws, ".github"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "workflows"), filepath.Join(ws, Dir)); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	_, _, err := Resolve(t.Context(), ws, "build", testScope())
	if err == nil {
		t.Fatal("a workflows directory linked out of the workspace must not be read")
	}
	if strings.Contains(err.Error(), "curl evil") {
		t.Error("the linked file was read")
	}
}

func TestOversizedWorkflowIsRefused(t *testing.T) {
	big := "jobs:\n  build:\n    runs-on: [macos]\n    steps:\n      - run: |\n" +
		strings.Repeat("          echo padding padding padding padding\n", 40000)
	ws := workspace(t, map[string]string{".github/workflows/big.yml": big})
	_, _, err := Resolve(t.Context(), ws, "build", testScope())
	if err == nil || !strings.Contains(err.Error(), "larger than") {
		t.Errorf("an oversized workflow should be refused by size: %v", err)
	}
}
