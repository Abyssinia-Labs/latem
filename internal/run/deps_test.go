package run

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/Abyssinia-Labs/latem/internal/source"
	"github.com/Abyssinia-Labs/latem/pkg/protocol"
)

// The adapter is the production wiring: it narrows *source.Mirror to the
// [Mirror] the gate chain wants and adds nothing else. Testing it against a
// real git repository is the only way to know the narrowing compiles into
// something that works, rather than something that satisfies a fake.
func TestSourceManager_AdaptsARealManager(t *testing.T) {
	git, err := exec.LookPath("git")
	if err != nil {
		t.Skipf("git is not on PATH: %v", err)
	}

	root := t.TempDir()
	upstream := filepath.Join(root, "upstream")
	run := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command(git, args...) // #nosec G204 -- argv is literal; only the temp dir varies
		cmd.Dir = dir
		cmd.Env = append(cmd.Environ(),
			"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.invalid",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.invalid")
		if out, cmdErr := cmd.CombinedOutput(); cmdErr != nil {
			t.Fatalf("git %v: %v\n%s", args, cmdErr, out)
		}
	}

	if err := makeDir(upstream); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	run(upstream, "init", "-q", "-b", "main", ".")
	if err := writeFile(filepath.Join(upstream, "README"), "hello"); err != nil {
		t.Fatalf("write: %v", err)
	}
	run(upstream, "add", "README")
	run(upstream, "commit", "-qm", "first")

	mgr, err := source.New(source.Config{
		MirrorDir: filepath.Join(root, "mirrors"),
		WorkDir:   filepath.Join(root, "work"),
		Env: []string{
			"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.invalid",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.invalid",
		},
	})
	if err != nil {
		t.Fatalf("source.New: %v", err)
	}

	adapted := SourceManager(mgr)
	if adapted == nil {
		t.Fatal("SourceManager returned nil for a real manager")
	}

	mirror, err := adapted.Fetch(t.Context(), source.Repo{Key: "app", URL: upstream})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	ok, err := mirror.HasRef(t.Context(), "refs/heads/main")
	if err != nil || !ok {
		t.Fatalf("HasRef(refs/heads/main) = %v, %v", ok, err)
	}
	if ok, _ := mirror.HasRef(t.Context(), "refs/tags/main"); ok {
		t.Fatal("a tag that does not exist was reported present")
	}

	ws, err := mirror.Acquire(t.Context(), "run1", "refs/heads/main")
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	t.Cleanup(func() { _ = ws.Remove() })
	if ws.SHA == "" || ws.Ref != "refs/heads/main" {
		t.Fatalf("workspace = %+v", ws)
	}
	if _, err := readRegistry(ws.Path); codeOf(err) != protocol.ErrNoRegistry {
		t.Fatalf("a repo with no .latem/jobs.yaml gave %v", err)
	}

	// A repo the mirror cannot reach is an error, not a nil Mirror to
	// dereference later.
	if _, err := adapted.Fetch(t.Context(), source.Repo{
		Key: "gone", URL: filepath.Join(root, "no-such-repo"),
	}); err == nil {
		t.Fatal("fetching a nonexistent repo succeeded")
	}
}

func TestNote_WithoutAnEmitterIsSafe(t *testing.T) {
	x := &execution{}
	x.note("no emitter yet: %v", 1)
	if got := x.redact("plain"); got != "plain" {
		t.Fatalf("redact without a redactor = %q", got)
	}
	if got := x.redact(""); got != "" {
		t.Fatalf("redact(\"\") = %q", got)
	}
	if x.secretCount() != 0 || x.secretNames() != nil {
		t.Fatal("a run with no secrets reported some")
	}
}

func makeDir(p string) error      { return os.MkdirAll(p, 0o750) }
func writeFile(p, s string) error { return os.WriteFile(p, []byte(s), 0o600) }
