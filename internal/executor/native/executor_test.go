package native

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Abyssinia-Labs/latem/internal/executor"
	"github.com/Abyssinia-Labs/latem/pkg/protocol"
)

// newExecutor builds an Executor whose $LATEM_ENV directories live under a
// temporary root, so a test never litters the operator's $TMPDIR.
func newExecutor(t *testing.T, opts Options) *Executor {
	t.Helper()
	if opts.EnvFileDir == "" {
		opts.EnvFileDir = t.TempDir()
	}
	e, err := New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return e
}

func TestName(t *testing.T) {
	if got := newExecutor(t, Options{}).Name(); got != "native" {
		t.Errorf("Name = %q, want native", got)
	}
}

// Amendment A6. The run id becomes a directory name here, so it is validated
// before any path is built from it — a control plane sending "../../tmp/x"
// would otherwise have the agent creating and removing directories outside its
// own tree.
func TestPrepare_RejectsAnInvalidRunID(t *testing.T) {
	e := newExecutor(t, Options{})
	for _, id := range []string{"", "../escape", "has space", strings.Repeat("x", 300)} {
		t.Run(id, func(t *testing.T) {
			_, err := e.Prepare(t.Context(), executor.Spec{RunID: id, Workspace: t.TempDir()})
			if err == nil {
				t.Fatalf("Prepare accepted run id %q", id)
			}
			if got := CodeOf(err); got != protocol.ErrExecutor {
				t.Errorf("code = %q, want %q", got, protocol.ErrExecutor)
			}
		})
	}
}

func TestPrepare_RejectsABadWorkspace(t *testing.T) {
	e := newExecutor(t, Options{})
	file := filepath.Join(t.TempDir(), "a-file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	tests := map[string]string{
		"relative":     "work/run-1",
		"missing":      filepath.Join(t.TempDir(), "nope"),
		"not a folder": file,
	}
	for name, ws := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := e.Prepare(t.Context(), executor.Spec{RunID: "run-1", Workspace: ws})
			if err == nil {
				t.Fatalf("Prepare accepted workspace %q", ws)
			}
			if got := CodeOf(err); got != protocol.ErrExecutor {
				t.Errorf("code = %q, want %q", got, protocol.ErrExecutor)
			}
		})
	}
}

// §9's between-step channel has to exist before the first step, or `>>
// "$LATEM_ENV"` fails on a job's very first use of it.
func TestPrepare_CreatesTheEnvFileOutsideTheWorkspace(t *testing.T) {
	root := t.TempDir()
	e := newExecutor(t, Options{EnvFileDir: root})
	ws := t.TempDir()

	env, err := e.Prepare(t.Context(), executor.Spec{RunID: "run-abc", Workspace: ws})
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	t.Cleanup(func() { _ = env.Close(t.Context()) })

	path := env.EnvFile()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat $LATEM_ENV: %v", err)
	}
	if info.Size() != 0 {
		t.Errorf("$LATEM_ENV is %d bytes, want empty", info.Size())
	}
	if perm := info.Mode().Perm(); perm != envFileMode {
		t.Errorf("mode = %o, want %o", perm, envFileMode)
	}
	if !strings.HasPrefix(path, root) {
		t.Errorf("$LATEM_ENV is %q, want it under %q", path, root)
	}
	// The workspace is a git clone (§12). A file appearing in it would show up
	// in `git status` and eventually in somebody's commit.
	if within(ws, path) {
		t.Errorf("$LATEM_ENV %q is inside the workspace %q", path, ws)
	}
}

func TestClose_IsIdempotentAndRemovesTheEnvDirectory(t *testing.T) {
	root := t.TempDir()
	e := newExecutor(t, Options{EnvFileDir: root})
	env, err := e.Prepare(t.Context(), executor.Spec{RunID: "run-1", Workspace: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}

	for i := range 3 {
		if err := env.Close(t.Context()); err != nil {
			t.Fatalf("Close #%d: %v", i+1, err)
		}
	}
	if _, err := os.Stat(env.EnvFile()); !os.IsNotExist(err) {
		t.Errorf("$LATEM_ENV survived Close: %v", err)
	}
}

// Close tears down the environment, not the run's clone: §12 owns the
// workspace, Fetch still needs it, and an executor that deleted it would take
// the artifacts with it.
func TestClose_LeavesTheWorkspaceAlone(t *testing.T) {
	ws := workspace(t, map[string]string{"dist/app.apk": "apk"})
	e := newExecutor(t, Options{})
	env, err := e.Prepare(t.Context(), executor.Spec{RunID: "run-1", Workspace: ws})
	if err != nil {
		t.Fatal(err)
	}
	if err := env.Close(t.Context()); err != nil {
		t.Fatal(err)
	}

	files, err := env.Fetch(t.Context(), "dist/*.apk")
	if err != nil {
		t.Fatalf("Fetch after Close: %v", err)
	}
	if len(files) != 1 {
		t.Fatalf("got %d artifacts after Close, want 1 — artifacts matter most when a step failed", len(files))
	}
	_ = files[0].Content.Close()
}

func TestExec_AfterCloseIsRefused(t *testing.T) {
	e := newExecutor(t, Options{})
	env, err := e.Prepare(t.Context(), executor.Spec{RunID: "run-1", Workspace: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if err := env.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := env.Exec(t.Context(), executor.Step{Run: "true"}, nil, nil); err == nil {
		t.Fatal("Exec succeeded on a closed environment")
	}
}

func TestValidateCacheMounts(t *testing.T) {
	ws := t.TempDir()
	cacheRoot := t.TempDir()

	tests := map[string]struct {
		caches  []executor.CacheMount
		wantErr string
	}{
		"ok": {
			caches: []executor.CacheMount{
				{Name: "gradle", Source: filepath.Join(cacheRoot, "gradle"), Target: "/Users/runner/.gradle"},
				{Name: "pods", Source: filepath.Join(cacheRoot, "pods"), Target: "/Users/runner/.cocoapods"},
			},
		},
		"no source": {
			caches:  []executor.CacheMount{{Name: "gradle", Target: "/Users/runner/.gradle"}},
			wantErr: "no source",
		},
		// A relative source is a Docker volume name (§14.1) reaching an
		// executor that has no volumes.
		"volume name as source": {
			caches:  []executor.CacheMount{{Name: "gradle", Source: "latem-cache-mac-gradle", Target: "/x"}},
			wantErr: "absolute",
		},
		"relative target": {
			caches:  []executor.CacheMount{{Name: "gradle", Source: cacheRoot, Target: ".gradle"}},
			wantErr: "absolute",
		},
		"target is root": {
			caches:  []executor.CacheMount{{Name: "gradle", Source: cacheRoot, Target: "/"}},
			wantErr: "take over /",
		},
		// A cache over the workspace shadows the clone §12 produced, and the
		// resulting "my repo is empty" points nowhere near the declaration.
		"target is the workspace": {
			caches:  []executor.CacheMount{{Name: "gradle", Source: cacheRoot, Target: ws}},
			wantErr: "shadow",
		},
		"target inside the workspace": {
			caches:  []executor.CacheMount{{Name: "gradle", Source: cacheRoot, Target: filepath.Join(ws, "build")}},
			wantErr: "shadow",
		},
		"two caches at one target": {
			caches: []executor.CacheMount{
				{Name: "a", Source: filepath.Join(cacheRoot, "a"), Target: "/Users/runner/.cache"},
				{Name: "b", Source: filepath.Join(cacheRoot, "b"), Target: "/Users/runner/.cache"},
			},
			wantErr: "both resolve to",
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			err := validateCacheMounts(tc.caches, ws)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("validateCacheMounts: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("validateCacheMounts accepted %v", tc.caches)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error = %q, want it to mention %q", err, tc.wantErr)
			}
			if got := CodeOf(err); got != protocol.ErrCache {
				t.Errorf("code = %q, want %q", got, protocol.ErrCache)
			}
		})
	}
}

// §14.2: "used in place for native". A symlink is the only mechanism a host
// offers, and this executor will create one where nothing exists and never
// replace anything that does.
func TestLinkCache(t *testing.T) {
	t.Run("creates the link and the parent", func(t *testing.T) {
		root := t.TempDir()
		source := filepath.Join(root, "caches", "mac", "gradle")
		target := filepath.Join(root, "home", "nested", ".gradle")

		if err := linkCaches([]executor.CacheMount{{Name: "gradle", Source: source, Target: target}}); err != nil {
			t.Fatalf("linkCaches: %v", err)
		}
		if !sameDir(target, source) {
			t.Fatalf("%s does not resolve to %s", target, source)
		}
		if info, err := os.Lstat(target); err != nil || info.Mode()&os.ModeSymlink == 0 {
			t.Errorf("target is not a symlink: %v %v", info, err)
		}
	})

	t.Run("an existing link to the same place is fine", func(t *testing.T) {
		root := t.TempDir()
		mount := executor.CacheMount{
			Name:   "gradle",
			Source: filepath.Join(root, "cache"),
			Target: filepath.Join(root, ".gradle"),
		}
		if err := linkCaches([]executor.CacheMount{mount}); err != nil {
			t.Fatal(err)
		}
		if err := linkCaches([]executor.CacheMount{mount}); err != nil {
			t.Fatalf("second Prepare with the same cache: %v", err)
		}
	})

	t.Run("target equal to source needs no link", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "gradle")
		if err := linkCaches([]executor.CacheMount{{Name: "g", Source: dir, Target: dir}}); err != nil {
			t.Fatalf("linkCaches: %v", err)
		}
	})

	// Replacing a real ~/.gradle with a symlink would strand however many
	// gigabytes are already in it. On a host that is not ephemeral, the
	// operator's own directory already satisfies the declaration.
	t.Run("an existing real directory is left alone", func(t *testing.T) {
		root := t.TempDir()
		target := filepath.Join(root, ".gradle")
		if err := os.MkdirAll(target, 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(target, "existing"), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}

		if err := linkCaches([]executor.CacheMount{
			{Name: "gradle", Source: filepath.Join(root, "cache"), Target: target},
		}); err != nil {
			t.Fatalf("linkCaches: %v", err)
		}
		if _, err := os.Stat(filepath.Join(target, "existing")); err != nil {
			t.Errorf("the operator's own cache contents were disturbed: %v", err)
		}
	})

	// Most likely two runners sharing one target, which §14.3 exists to
	// prevent. Failing beats two runs corrupting one cache.
	t.Run("a link somewhere else is E_CACHE", func(t *testing.T) {
		root := t.TempDir()
		other := filepath.Join(root, "other")
		if err := os.MkdirAll(other, 0o750); err != nil {
			t.Fatal(err)
		}
		target := filepath.Join(root, ".gradle")
		if err := os.Symlink(other, target); err != nil {
			t.Fatal(err)
		}

		err := linkCaches([]executor.CacheMount{
			{Name: "gradle", Source: filepath.Join(root, "cache"), Target: target},
		})
		if err == nil {
			t.Fatal("a cache target pointing at another runner's directory was accepted")
		}
		if got := CodeOf(err); got != protocol.ErrCache {
			t.Errorf("code = %q, want %q", got, protocol.ErrCache)
		}
	})

	t.Run("a file at the target is E_CACHE", func(t *testing.T) {
		root := t.TempDir()
		target := filepath.Join(root, ".gradle")
		if err := os.WriteFile(target, []byte("not a directory"), 0o600); err != nil {
			t.Fatal(err)
		}
		err := linkCaches([]executor.CacheMount{
			{Name: "gradle", Source: filepath.Join(root, "cache"), Target: target},
		})
		if err == nil {
			t.Fatal("a regular file at the cache target was accepted")
		}
		if got := CodeOf(err); got != protocol.ErrCache {
			t.Errorf("code = %q, want %q", got, protocol.ErrCache)
		}
	})
}

func TestPrepare_LinksCaches(t *testing.T) {
	root := t.TempDir()
	e := newExecutor(t, Options{})
	source := filepath.Join(root, "caches", "mac-mini", "gradle")
	target := filepath.Join(root, "home", ".gradle")

	env, err := e.Prepare(t.Context(), executor.Spec{
		RunID:     "run-1",
		Workspace: t.TempDir(),
		Caches:    []executor.CacheMount{{Name: "gradle", Source: source, Target: target}},
	})
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	t.Cleanup(func() { _ = env.Close(t.Context()) })

	if !sameDir(target, source) {
		t.Errorf("%s does not resolve to the runner's cache directory %s", target, source)
	}
}

func TestPreflight(t *testing.T) {
	t.Run("darwin refuses a daemon context", func(t *testing.T) {
		e := newExecutor(t, Options{Runner: managerRunner("System")})
		e.goos = "darwin"
		err := e.Preflight(t.Context())
		if err == nil {
			t.Fatal("Preflight accepted a LaunchDaemon context")
		}
		if !strings.Contains(err.Error(), "LaunchAgents") {
			t.Errorf("error = %q, want it to name the fix (§23)", err)
		}
	})

	t.Run("darwin accepts a gui session", func(t *testing.T) {
		e := newExecutor(t, Options{Runner: managerRunner("Aqua")})
		e.goos = "darwin"
		if err := e.Preflight(t.Context()); err != nil {
			t.Fatalf("Preflight: %v", err)
		}
	})

	// launchctl and security do not exist on Linux, and refusing to run there
	// over a missing diagnostic would make a legitimate deployment impossible.
	t.Run("non darwin skips the macOS probes", func(t *testing.T) {
		r := &fakeRunner{}
		e := newExecutor(t, Options{Runner: r, RequireGUISession: true, CheckKeychain: true})
		e.goos = "linux"
		if err := e.Preflight(t.Context()); err != nil {
			t.Fatalf("Preflight on linux: %v", err)
		}
		if len(r.seen) != 0 {
			t.Errorf("macOS probes ran on linux: %v", r.seen)
		}
	})

	t.Run("keychain check is opt in", func(t *testing.T) {
		locked := &fakeRunner{
			out: map[string][]byte{"managername": []byte("Aqua")},
			err: map[string]error{"show-keychain-info": &probeError{program: "security"}},
		}
		off := newExecutor(t, Options{Runner: locked})
		off.goos = "darwin"
		if err := off.Preflight(t.Context()); err != nil {
			t.Fatalf("a locked keychain failed Preflight with the check off: %v", err)
		}

		on := newExecutor(t, Options{Runner: locked, CheckKeychain: true})
		on.goos = "darwin"
		if err := on.Preflight(t.Context()); err == nil {
			t.Fatal("Preflight passed with CheckKeychain and a locked keychain")
		}
	})

	// A native runner whose shell is missing cannot run any step, and §20 wants
	// that to mark the runner unavailable rather than fail the eleventh minute
	// of the first build.
	t.Run("a missing shell fails preflight and prepare", func(t *testing.T) {
		e := newExecutor(t, Options{Shell: "no-such-shell-anywhere"})
		if err := e.Preflight(t.Context()); err == nil {
			t.Fatal("Preflight passed with no shell on PATH")
		}
		if _, err := e.Prepare(t.Context(), executor.Spec{RunID: "run-1", Workspace: t.TempDir()}); err == nil {
			t.Fatal("Prepare succeeded with no shell on PATH")
		}
	})
}

func TestSession_ReportsWithoutJudging(t *testing.T) {
	e := newExecutor(t, Options{Runner: managerRunner("StandardIO")})
	got := e.Session(t.Context())
	if got.Kind != SessionStandardIO {
		t.Errorf("Kind = %q, want %q", got.Kind, SessionStandardIO)
	}
}

// The native counterpart of the docker executor's label-based reconciliation:
// an agent killed mid-run leaves an empty directory per interrupted run, and
// nothing else ever collects it.
func TestReap(t *testing.T) {
	root := t.TempDir()
	e := newExecutor(t, Options{EnvFileDir: root})

	for _, name := range []string{EnvDirPrefix + "dead", EnvDirPrefix + "alive", "someone-elses-tempdir"} {
		if err := os.MkdirAll(filepath.Join(root, name), 0o700); err != nil {
			t.Fatal(err)
		}
	}

	if err := mustReap(t, e, func(runID string) bool { return runID == "alive" }); err != nil {
		t.Fatalf("Reap: %v", err)
	}
	for name, wantGone := range map[string]bool{
		EnvDirPrefix + "dead":   true,
		EnvDirPrefix + "alive":  false,
		"someone-elses-tempdir": false,
	} {
		_, err := os.Stat(filepath.Join(root, name))
		if gone := os.IsNotExist(err); gone != wantGone {
			t.Errorf("%s: gone = %v, want %v", name, gone, wantGone)
		}
	}
}

func TestReap_MissingDirectoryIsNotAnError(t *testing.T) {
	e := newExecutor(t, Options{EnvFileDir: filepath.Join(t.TempDir(), "never-created")})
	if err := mustReap(t, e, nil); err != nil {
		t.Errorf("Reap on a directory that was never created: %v", err)
	}
}

// mustReap adapts the A23 signature for tests that only care about the error.
func mustReap(t *testing.T, e *Executor, live func(runID string) bool) error {
	t.Helper()
	_, err := e.Reap(t.Context(), live)
	return err
}
