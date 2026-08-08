//go:build integration

// Package docker's integration tests. They need a running Docker daemon and
// are excluded from the default `go test` gate, which is what lets CI and a
// laptop without Docker still run everything else in this package:
//
//	go test -tags=integration ./internal/executor/docker/...
//
// Every test cleans up after itself. A leaked container on a build machine is
// untidy; a leaked one holding a bind mount open is a workspace that cannot be
// pruned.
package docker

import (
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Abyssinia-Labs/latem/internal/executor"
	"github.com/Abyssinia-Labs/latem/pkg/protocol"
)

// testImage is deliberately tiny. These tests are about this package's
// behaviour, not about any particular base image, and a 5 MB image keeps them
// runnable on a laptop.
const testImage = "alpine:latest"

// testShell is sh rather than bash because alpine has no bash. The shell is an
// Options field precisely so the argv contract can be exercised against
// whatever the image actually ships; StepArgv's own unit test pins the default.
const testShell = "sh"

func newIntegrationExecutor(t *testing.T, mutate func(*Options)) *Executor {
	t.Helper()
	opts := Options{
		DefaultImage: testImage,
		Shell:        testShell,
		// Alpine's only uid-1000-adjacent user is absent, so forcing the host
		// uid would leave $HOME unwritable. The image's own user is right for
		// a test image; §13.1's Linux uid mapping is an operator concern.
		User: UserImageDefault,
	}
	if mutate != nil {
		mutate(&opts)
	}
	e, err := New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = e.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if err := e.Preflight(ctx); err != nil {
		t.Skipf("no usable Docker daemon: %v", err)
	}
	return e
}

func prepareRun(t *testing.T, e *Executor, runID string, mutate func(*executor.Spec)) (executor.Env, string) {
	t.Helper()
	ws := t.TempDir()
	spec := executor.Spec{
		RunID:     runID,
		Workspace: ws,
		Env:       map[string]string{"CI": "true"},
	}
	if mutate != nil {
		mutate(&spec)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	env, err := e.Prepare(ctx, spec)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	t.Cleanup(func() {
		closeCtx, closeCancel := context.WithTimeout(context.Background(), time.Minute)
		defer closeCancel()
		_ = env.Close(closeCtx)
	})
	return env, spec.Workspace
}

func run(t *testing.T, env executor.Env, name, script string) (int, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	var log bytes.Buffer
	code, err := env.Exec(ctx, executor.Step{Name: name, Run: script}, &log, &log)
	if err != nil {
		t.Fatalf("Exec(%s): %v\n%s", name, err, log.String())
	}
	return code, log.String()
}

func TestIntegration_RunsSteps(t *testing.T) {
	e := newIntegrationExecutor(t, nil)
	env, _ := prepareRun(t, e, "int-basic", nil)

	code, out := run(t, env, "echo", "echo hello from the container")
	if code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}
	if !strings.Contains(out, "hello from the container") {
		t.Errorf("output = %q", out)
	}

	code, _ = run(t, env, "fail", "exit 3")
	if code != 3 {
		t.Errorf("exit code = %d, want 3", code)
	}

	_, out = run(t, env, "streams", "echo to-stdout; echo to-stderr >&2")
	for _, want := range []string{"to-stdout", "to-stderr"} {
		if !strings.Contains(out, want) {
			t.Errorf("output %q is missing %q", out, want)
		}
	}

	_, out = run(t, env, "pwd", "pwd")
	if strings.TrimSpace(out) != WorkspacePath {
		t.Errorf("a step started in %q, want %q", strings.TrimSpace(out), WorkspacePath)
	}

	_, out = run(t, env, "env", `echo "$CI"`)
	if strings.TrimSpace(out) != "true" {
		t.Errorf("the run environment did not reach the step: %q", out)
	}
}

// This is finding F1, against a real container.
//
// Draft 01 of RUNNER_SPEC said `cd` and `export` persist across steps because
// the steps share a container. They do not: each step is a separate exec and
// therefore a separate process. Only the filesystem survives. Both halves are
// asserted here, because a change that broke either would be invisible until a
// job author hit it.
func TestIntegration_ShellStateDoesNotLeakButFilesDo(t *testing.T) {
	e := newIntegrationExecutor(t, nil)
	env, ws := prepareRun(t, e, "int-f1", nil)

	// Step one changes everything a shell can change about itself, and writes
	// one file.
	code, out := run(t, env, "mutate", strings.Join([]string{
		"cd /etc",
		"export LEAKED_EXPORT=yes",
		"NOT_EXPORTED=also-yes",
		"alias ll='ls -l'",
		"umask 077",
		"echo persisted > /workspace/from-step-one.txt",
		"mkdir -p /workspace/dir-from-step-one",
	}, "\n"))
	if code != 0 {
		t.Fatalf("step one exited %d: %s", code, out)
	}

	// Step two asks what survived.
	_, out = run(t, env, "observe", strings.Join([]string{
		`echo "pwd=$(pwd)"`,
		`echo "export=${LEAKED_EXPORT:-<unset>}"`,
		`echo "shellvar=${NOT_EXPORTED:-<unset>}"`,
		`echo "file=$(cat /workspace/from-step-one.txt 2>/dev/null || echo '<missing>')"`,
		`echo "dir=$([ -d /workspace/dir-from-step-one ] && echo present || echo '<missing>')"`,
	}, "\n"))

	facts := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		if k, v, ok := strings.Cut(strings.TrimSpace(line), "="); ok {
			facts[k] = v
		}
	}

	// Process state does NOT survive.
	if facts["pwd"] != WorkspacePath {
		t.Errorf("pwd = %q; step one's `cd` leaked into step two, want %q", facts["pwd"], WorkspacePath)
	}
	if facts["export"] != "<unset>" {
		t.Errorf("LEAKED_EXPORT = %q; step one's `export` leaked into step two", facts["export"])
	}
	if facts["shellvar"] != "<unset>" {
		t.Errorf("NOT_EXPORTED = %q; step one's shell variable leaked into step two", facts["shellvar"])
	}

	// The filesystem DOES survive.
	if facts["file"] != "persisted" {
		t.Errorf("file = %q; the filesystem did not persist across steps", facts["file"])
	}
	if facts["dir"] != "present" {
		t.Errorf("dir = %q; the filesystem did not persist across steps", facts["dir"])
	}

	// And it is the same filesystem the host sees, which is what makes
	// host-side artifact collection correct.
	body, err := os.ReadFile(filepath.Join(ws, "from-step-one.txt"))
	if err != nil {
		t.Fatalf("the workspace bind mount did not surface the file on the host: %v", err)
	}
	if strings.TrimSpace(string(body)) != "persisted" {
		t.Errorf("host sees %q", body)
	}
}

// $LATEM_ENV is the one sanctioned channel between steps (§9), and the whole
// reason it exists is that the test above passes.
func TestIntegration_LatemEnv(t *testing.T) {
	e := newIntegrationExecutor(t, nil)
	env, _ := prepareRun(t, e, "int-latem-env", func(s *executor.Spec) {
		s.Env = map[string]string{"CI": "true", "MATCH_PASSWORD": "real-secret"}
	})

	code, out := run(t, env, "write", strings.Join([]string{
		`echo "BUILD_NUMBER=42" >> "$LATEM_ENV"`,
		`echo "ARTIFACT=app.aab" >> "$LATEM_ENV"`,
		// Every one of these must be refused.
		`echo "MATCH_PASSWORD=stolen" >> "$LATEM_ENV"`,
		`echo "LATEM_ENV=/tmp/attacker" >> "$LATEM_ENV"`,
		`echo "1BAD=x" >> "$LATEM_ENV"`,
		`echo "not an assignment at all" >> "$LATEM_ENV"`,
	}, "\n"))
	if code != 0 {
		t.Fatalf("step one exited %d: %s", code, out)
	}
	if !strings.Contains(out, "[latem] $LATEM_ENV") {
		t.Errorf("nothing was reported about the dropped lines: %q", out)
	}
	if strings.Contains(out, "stolen") {
		t.Errorf("a rejected value was echoed into the log: %q", out)
	}

	_, out = run(t, env, "read", strings.Join([]string{
		`echo "build=${BUILD_NUMBER:-<unset>}"`,
		`echo "artifact=${ARTIFACT:-<unset>}"`,
		`echo "secret=${MATCH_PASSWORD:-<unset>}"`,
		`echo "envfile=${LATEM_ENV}"`,
		`echo "bad=${1BAD:-<unset>}" 2>/dev/null || echo "bad=<unset>"`,
	}, "\n"))

	if !strings.Contains(out, "build=42") {
		t.Errorf("BUILD_NUMBER did not carry forward: %q", out)
	}
	if !strings.Contains(out, "artifact=app.aab") {
		t.Errorf("ARTIFACT did not carry forward: %q", out)
	}
	if !strings.Contains(out, "secret=real-secret") {
		t.Errorf("a job overwrote an injected credential: %q", out)
	}
	if !strings.Contains(out, "envfile="+EnvFilePath) {
		t.Errorf("a job spoofed $LATEM_ENV: %q", out)
	}

	// Truncation: what step one wrote must not be re-merged forever.
	_, out = run(t, env, "size", `wc -c < "$LATEM_ENV"`)
	if strings.TrimSpace(out) != "0" {
		t.Errorf("$LATEM_ENV was not truncated between steps; it holds %q bytes", strings.TrimSpace(out))
	}
}

// §13.1 forgoes `--rm` so that a failed step's output is still collectable.
func TestIntegration_ArtifactsSurviveAFailingStep(t *testing.T) {
	e := newIntegrationExecutor(t, nil)
	env, _ := prepareRun(t, e, "int-artifacts", nil)

	code, out := run(t, env, "build-then-fail", strings.Join([]string{
		"mkdir -p /workspace/dist/nested",
		"echo apk > /workspace/dist/app.apk",
		"echo deep > /workspace/dist/nested/other.apk",
		"echo 'the build failed here'",
		"exit 1",
	}, "\n"))
	if code != 1 {
		t.Fatalf("step exited %d, want 1: %s", code, out)
	}

	files, err := env.Fetch(context.Background(), "dist/**/*.apk")
	if err != nil {
		t.Fatalf("Fetch after a failed step: %v", err)
	}
	got := map[string]string{}
	for _, f := range files {
		body, readErr := io.ReadAll(f.Content)
		_ = f.Content.Close()
		if readErr != nil {
			t.Fatalf("read %s: %v", f.Path, readErr)
		}
		got[f.Path] = strings.TrimSpace(string(body))
	}
	if got["dist/app.apk"] != "apk" || got["dist/nested/other.apk"] != "deep" {
		t.Errorf("Fetch = %v, want both apks", got)
	}
}

func TestIntegration_FetchRejectsASymlinkEscape(t *testing.T) {
	e := newIntegrationExecutor(t, nil)
	env, _ := prepareRun(t, e, "int-escape", nil)

	code, out := run(t, env, "leak", strings.Join([]string{
		"mkdir -p /workspace/dist",
		"ln -s /etc/passwd /workspace/dist/leak.apk",
	}, "\n"))
	if code != 0 {
		t.Fatalf("setup exited %d: %s", code, out)
	}

	if _, err := env.Fetch(context.Background(), "dist/*.apk"); err == nil {
		t.Fatal("a symlink to /etc/passwd was collected as an artifact")
	} else if got := CodeOf(err); got != protocol.ErrArtifact {
		t.Errorf("error code = %q, want %q (%v)", got, protocol.ErrArtifact, err)
	}
}

// Startup reconciliation depends entirely on the labels: an agent that died
// mid-run has no other record of what it started.
func TestIntegration_LabelledContainerIsDiscoverableAfterTheProcessDies(t *testing.T) {
	e := newIntegrationExecutor(t, nil)
	const runID = "int-reconcile"
	env, _ := prepareRun(t, e, runID, nil)

	if _, out := run(t, env, "work", "echo working"); !strings.Contains(out, "working") {
		t.Fatalf("step output = %q", out)
	}

	// Simulate the agent dying: throw away every in-process handle. A fresh
	// Executor, as a restarted agent would build, must still find the
	// container by its labels alone.
	restarted := newIntegrationExecutor(t, nil)
	found, err := restarted.Discover(context.Background())
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}

	var match *Container
	for i := range found {
		if found[i].RunID == runID {
			match = &found[i]
			break
		}
	}
	if match == nil {
		t.Fatalf("Discover did not find run %s among %v", runID, found)
	}
	if match.Name != ContainerNamePrefix+runID {
		t.Errorf("container name = %q, want %q", match.Name, ContainerNamePrefix+runID)
	}

	// And reconciliation can clean it up.
	if err := restarted.Remove(context.Background(), match.ID); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if err := restarted.Remove(context.Background(), match.ID); err != nil {
		t.Errorf("Remove is not idempotent: %v", err)
	}
}

// The container really does outlive a kill -9 of the agent, which is the
// premise the label check above is standing in for. Verified with the docker
// CLI so the assertion does not depend on this package's own view.
func TestIntegration_ContainerOutlivesTheAgentProcess(t *testing.T) {
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker CLI not on PATH")
	}
	e := newIntegrationExecutor(t, nil)
	const runID = "int-outlives"
	env, _ := prepareRun(t, e, runID, nil)
	if _, out := run(t, env, "work", "echo working"); !strings.Contains(out, "working") {
		t.Fatalf("step output = %q", out)
	}

	out, err := exec.Command("docker", "ps", "--all",
		"--filter", "label="+LabelManaged+"=true",
		"--filter", "label="+LabelRunID+"="+runID,
		"--format", "{{.Names}}").Output()
	if err != nil {
		t.Fatalf("docker ps: %v", err)
	}
	if !strings.Contains(string(out), ContainerNamePrefix+runID) {
		t.Errorf("docker ps by label found %q, want %s", out, ContainerNamePrefix+runID)
	}
}

func TestIntegration_CloseRemovesTheContainer(t *testing.T) {
	e := newIntegrationExecutor(t, nil)
	env, _ := prepareRun(t, e, "int-close", nil)
	if _, out := run(t, env, "work", "echo working"); !strings.Contains(out, "working") {
		t.Fatalf("step output = %q", out)
	}

	ctx := context.Background()
	if err := env.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := env.Close(ctx); err != nil {
		t.Fatalf("second Close: %v", err)
	}

	found, err := e.Discover(ctx)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	for _, c := range found {
		if c.RunID == "int-close" {
			t.Errorf("container %s survived Close", c.Name)
		}
	}
}

// A step that hangs must not hang the run. §21 calls this E_STEP_TIMEOUT and
// distinguishes it from the job budget, so an operator reading the error knows
// which limit to raise.
func TestIntegration_StepTimeout(t *testing.T) {
	e := newIntegrationExecutor(t, nil)
	env, _ := prepareRun(t, e, "int-timeout", nil)

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	start := time.Now()
	_, err := env.Exec(ctx, executor.Step{
		Name:    "hangs",
		Run:     "sleep 300",
		Timeout: 2 * time.Second,
	}, io.Discard, io.Discard)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("a hung step returned no error")
	}
	if got := CodeOf(err); got != protocol.ErrStepTimeout {
		t.Errorf("error code = %q, want %q (%v)", got, protocol.ErrStepTimeout, err)
	}
	if elapsed > 30*time.Second {
		t.Errorf("the step took %s to time out at 2s", elapsed)
	}
}

// A cache volume is created on first use and is still there on the next run,
// because §14 mounts caches rather than restoring them.
func TestIntegration_CacheVolumePersistsBetweenRuns(t *testing.T) {
	e := newIntegrationExecutor(t, nil)
	const volName = "latem-test-cache-ws07"
	cache := []executor.CacheMount{{Name: "test", Source: volName, Target: "/cache"}}

	t.Cleanup(func() {
		if _, err := exec.LookPath("docker"); err == nil {
			_ = exec.Command("docker", "volume", "rm", "-f", volName).Run()
		}
	})

	first, _ := prepareRun(t, e, "int-cache-1", func(s *executor.Spec) { s.Caches = cache })
	if code, out := run(t, first, "warm", "echo warmed > /cache/marker"); code != 0 {
		t.Fatalf("warming the cache exited %d: %s", code, out)
	}
	if err := first.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}

	second, _ := prepareRun(t, e, "int-cache-2", func(s *executor.Spec) { s.Caches = cache })
	code, out := run(t, second, "read", "cat /cache/marker")
	if code != 0 || !strings.Contains(out, "warmed") {
		t.Errorf("the cache did not survive the run: exit %d, %q", code, out)
	}
}

// The base image contract check, against an image that deliberately does not
// satisfy it. Alpine has no uid 1000, no bash and no git, so `latem doctor`
// must say so by name rather than failing vaguely.
func TestIntegration_CheckImageNamesWhatIsMissing(t *testing.T) {
	e := newIntegrationExecutor(t, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	report, err := e.CheckImage(ctx, testImage)
	if err != nil {
		t.Fatalf("CheckImage: %v", err)
	}
	if report.OK() {
		t.Fatal("alpine passed the base image contract; it has no uid 1000, bash or git")
	}

	failed := map[string]bool{}
	for _, c := range report.Failures() {
		failed[c.Name] = true
	}
	for _, want := range []string{"user-1000", "tool-bash", "tool-git"} {
		if !failed[want] {
			t.Errorf("check %q passed on alpine, which does not satisfy it", want)
		}
	}
	if report.Err() == nil {
		t.Error("Err returned nil for a failing report")
	}
}

// Preflight is called every five minutes (§20), so it must be cheap and it
// must not leave anything behind.
func TestIntegration_PreflightIsRepeatable(t *testing.T) {
	e := newIntegrationExecutor(t, nil)
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		if err := e.Preflight(ctx); err != nil {
			t.Fatalf("Preflight %d: %v", i, err)
		}
	}
}

// A GPU runner on a machine with no NVIDIA toolkit must fail at startup, not
// mid-build. On this hardware there is no toolkit, so the failure is the
// expected outcome.
func TestIntegration_GPUPreflightFailsWithoutTheToolkit(t *testing.T) {
	if os.Getenv("LATEM_TEST_GPU") == "" {
		t.Skip("set LATEM_TEST_GPU=1 to run the GPU preflight test (it pulls a CUDA base image)")
	}
	e, err := New(Options{DefaultImage: testImage, Shell: testShell, GPU: true})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = e.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	if err := e.Preflight(ctx); err == nil {
		t.Log("GPU preflight passed; this machine has a working NVIDIA Container Toolkit")
	} else if !strings.Contains(err.Error(), "gpu preflight") {
		t.Errorf("Preflight failed for an unrelated reason: %v", err)
	}
}

// Prepare must refuse a run id that would become a path or a container name it
// should not, before it creates anything (amendment A6).
func TestIntegration_PrepareRefusesAHostileRunID(t *testing.T) {
	e := newIntegrationExecutor(t, nil)
	_, err := e.Prepare(context.Background(), executor.Spec{
		RunID:     "../../../../tmp/x",
		Workspace: t.TempDir(),
	})
	if err == nil {
		t.Fatal("Prepare accepted a traversal run id")
	}
	if CodeOf(err) != protocol.ErrExecutor {
		t.Errorf("error code = %q, want %q (%v)", CodeOf(err), protocol.ErrExecutor, err)
	}
}
