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
	"fmt"
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
		// User is left at auto, which is what an operator gets: the invoking
		// uid:gid on Linux, the image's own user on Docker Desktop. Forcing
		// UserImageDefault here made the suite pass on macOS and fail on Linux
		// — the container wrote root-owned files into a bind mount and the
		// non-root host could then neither read them back nor delete them at
		// cleanup. That is the option behaving as documented; the test was
		// asserting host visibility while opting out of the setting that
		// provides it (§13.1).
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
	res, err := env.Exec(ctx, executor.Step{Name: name, Run: script}, &log, &log)
	if err != nil {
		t.Fatalf("Exec(%s): %v\n%s", name, err, log.String())
	}
	return res.ExitCode, log.String()
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

// A JavaScript action reads a hyphenated input as INPUT_NO-CACHE. The step runs
// under the wrapper's /bin/sh, and dash — Debian's and Ubuntu's /bin/sh — drops
// such a name as it starts: setup-bun then read its `no-cache` as empty and
// threw. busybox's ash happens to keep it, so alpine alone would not have
// caught this; both are run. On debian the step itself runs under bash, as a
// workflow step does by default, so the only dash in the way is the wrapper's.
// Carried past it, the name arrives, and the carriers do not.
func TestIntegration_HyphenatedNamesReachTheStep(t *testing.T) {
	for _, c := range []struct{ image, shell string }{
		{"debian:stable-slim", "bash"},
		{testImage, testShell},
	} {
		t.Run(c.image, func(t *testing.T) {
			e := newIntegrationExecutor(t, func(o *Options) { o.DefaultImage, o.Shell = c.image, c.shell })
			env, _ := prepareRun(t, e, "int-hyphen", nil)

			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			var log bytes.Buffer
			res, err := env.Exec(ctx, executor.Step{
				Name: "inputs",
				Run:  `env | grep -E '^(INPUT_|LATEM_ENVX_|PLAIN=)' | sort`,
				Env: map[string]string{
					"INPUT_NO-CACHE":    "false",
					"INPUT_BUN-VERSION": "1.3.3 with spaces; and $(no) expansion",
					"PLAIN":             "kept",
				},
			}, &log, &log)
			if err != nil || res.ExitCode != 0 {
				t.Fatalf("Exec: %v, exit %d\n%s", err, res.ExitCode, log.String())
			}
			want := "INPUT_BUN-VERSION=1.3.3 with spaces; and $(no) expansion\nINPUT_NO-CACHE=false\nPLAIN=kept\n"
			if log.String() != want {
				t.Errorf("the step saw\n%s\nwant\n%s", log.String(), want)
			}
		})
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
	env, ws := prepareRun(t, e, "int-artifacts", nil)

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

	// Read the host side of the bind mount, which is where collection actually
	// happens: internal/artifact walks the workspace path directly, and does so
	// during `collecting` — after this environment has been torn down
	// (amendment A29 removed executor.Env.Fetch for exactly that reason).
	for rel, want := range map[string]string{
		"dist/app.apk":          "apk",
		"dist/nested/other.apk": "deep",
	} {
		body, err := os.ReadFile(filepath.Join(ws, filepath.FromSlash(rel)))
		if err != nil {
			t.Fatalf("a failing step lost %s, which is when artifacts matter most: %v", rel, err)
		}
		if strings.TrimSpace(string(body)) != want {
			t.Errorf("%s = %q, want %q", rel, strings.TrimSpace(string(body)), want)
		}
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

// A45, against a daemon. A timed-out step's process group is stopped in place:
// its TERM handler runs, its children are gone, and — the point — the container
// is still there for the step after it, which is what an `if: always()` cleanup
// needs. The first assertion also holds the fact the mechanism rests on: that
// the pid a step records for itself is its process group's id.
func TestIntegration_StepTimeoutLeavesTheContainerForLaterSteps(t *testing.T) {
	e := newIntegrationExecutor(t, func(o *Options) { o.TerminateGrace = 5 * time.Second })
	env, _ := prepareRun(t, e, "a45-stop-in-place", nil)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	_, err := env.Exec(ctx, executor.Step{
		Name: "hangs",
		Run: `trap 'echo graceful > trap.out; exit 0' TERM
sleep 300 & echo $! > child.pid
read -r stat < /proc/$$/stat; set -- $stat; echo "$1 $5" > group.out
wait`,
		Timeout: 3 * time.Second,
	}, io.Discard, io.Discard)
	if got := CodeOf(err); got != protocol.ErrStepTimeout {
		t.Fatalf("error code = %q, want %q (%v)", got, protocol.ErrStepTimeout, err)
	}

	var log bytes.Buffer
	res, err := env.Exec(ctx, executor.Step{Name: "after", Run: `
echo "group: $(cat group.out)"
echo "trap: $(cat trap.out 2>/dev/null || echo none)"
if kill -0 "$(cat child.pid)" 2>/dev/null; then echo "child: ALIVE"; else echo "child: gone"; fi`}, &log, &log)
	if err != nil {
		t.Fatalf("the step after a timeout could not run — the container did not survive: %v\n%s", err, log.String())
	}
	if res.ExitCode != 0 {
		t.Fatalf("exit %d\n%s", res.ExitCode, log.String())
	}
	out := log.String()
	var pid, pgid string
	if _, scanErr := fmt.Sscanf(out[strings.Index(out, "group: ")+7:], "%s %s", &pid, &pgid); scanErr != nil || pid == "" || pid != pgid {
		t.Errorf("a step must be the leader of its own process group, or stopping it signals the wrong processes: pid=%q pgid=%q\n%s", pid, pgid, out)
	}
	if !strings.Contains(out, "trap: graceful") {
		t.Errorf("the step's TERM handler should have run before it was killed:\n%s", out)
	}
	if !strings.Contains(out, "child: gone") {
		t.Errorf("the step's child outlived it:\n%s", out)
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

// LAT-3: a target written `~/…`, as actions/cache writes ~/.cargo/git, is
// mounted where the steps' own $HOME says, whichever user and image that is:
// root's /root on Docker Desktop, / for a uid alpine has no entry for on Linux.
func TestIntegration_TildeCacheIsMountedUnderTheStepsHome(t *testing.T) {
	e := newIntegrationExecutor(t, nil)
	const volName = "latem-test-cache-lat3"
	cache := []executor.CacheMount{{Name: "test", Source: volName, Target: "~/.latem-lat3"}}

	t.Cleanup(func() {
		if _, err := exec.LookPath("docker"); err == nil {
			_ = exec.Command("docker", "volume", "rm", "-f", volName).Run()
		}
	})

	first, _ := prepareRun(t, e, "int-lat3-1", func(s *executor.Spec) { s.Caches = cache })
	if code, out := run(t, first, "warm", `echo warmed > "$HOME/.latem-lat3/marker"`); code != 0 {
		t.Fatalf("warming the cache under $HOME exited %d: %s", code, out)
	}
	if err := first.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}

	second, _ := prepareRun(t, e, "int-lat3-2", func(s *executor.Spec) { s.Caches = cache })
	code, out := run(t, second, "read", `cat "$HOME/.latem-lat3/marker"`)
	if code != 0 || !strings.Contains(out, "warmed") {
		t.Errorf("the cache under $HOME did not survive the run: exit %d, %q", code, out)
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

// A44, against a real daemon — the part the fake cannot answer. The four files
// live on a per-run volume, which relies on two things only a daemon can show:
// that the archive API writes into a volume mounted at the target path, and
// that a second container mounting that volume sees the same files. A
// container step writes an output and an env value; the Result carries the
// first and the next ordinary step reads the second.
func TestIntegration_ContainerStepSharesTheRunFiles(t *testing.T) {
	e := newIntegrationExecutor(t, nil)
	env, _ := prepareRun(t, e, "a44-container-step", nil)
	ctx := context.Background()

	var log bytes.Buffer
	res, err := env.Exec(ctx, executor.Step{
		Name: "side",
		Container: &executor.Container{
			Image:      "alpine:3.19",
			Entrypoint: "/bin/sh",
			Args:       []string{"-c", `echo "k=from-the-container" >> "$GITHUB_OUTPUT"; echo "CARRIED=yes" >> "$GITHUB_ENV"; pwd`},
		},
	}, &log, &log)
	if err != nil {
		t.Fatalf("Exec: %v\n%s", err, log.String())
	}
	if res.ExitCode != 0 {
		t.Fatalf("exit %d\n%s", res.ExitCode, log.String())
	}
	if res.Outputs["k"] != "from-the-container" {
		t.Errorf("the container step's output did not come back: %+v\n%s", res.Outputs, log.String())
	}
	if !strings.Contains(log.String(), WorkspacePath) {
		t.Errorf("the step should start in the workspace: %s", log.String())
	}

	log.Reset()
	if _, err := env.Exec(ctx, executor.Step{Name: "after", Run: `echo "[$CARRIED]"`}, &log, &log); err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if !strings.Contains(log.String(), "[yes]") {
		t.Errorf("$GITHUB_ENV written by a container step did not carry: %s", log.String())
	}
}

// A84: an upload reads runner.temp out of a real container, and a download
// writes into it, through the archive API: a file and a directory come out at
// their paths under runner.temp, a symlink a step left there does not, and
// what goes in can be read, and removed, by the step's user.
func TestIntegration_RunnerTempCopiesOutAndIn(t *testing.T) {
	e := newIntegrationExecutor(t, nil)
	env, _ := prepareRun(t, e, "int-tempcopy", nil)
	tc, ok := env.(executor.TempCopier)
	if !ok {
		t.Fatal("docker's environment is no TempCopier")
	}
	if code, out := run(t, env, "pack", `set -e
mkdir -p "$RUNNER_TEMP/pack/sub"
printf tgz > "$RUNNER_TEMP/pack/sokie-1.0.0.tgz"
printf notes > "$RUNNER_TEMP/pack/sub/notes.txt"
ln -s /etc/passwd "$RUNNER_TEMP/pack/passwd"`); code != 0 {
		t.Fatalf("pack: exit %d\n%s", code, out)
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	dst := t.TempDir()
	n, err := tc.CopyFromTemp(ctx, "pack/sokie-1.0.0.tgz", dst, 1<<20)
	if err != nil || n != 3 {
		t.Fatalf("one file: %d, %v", n, err)
	}
	if b, _ := os.ReadFile(filepath.Join(dst, "pack", "sokie-1.0.0.tgz")); string(b) != "tgz" {
		t.Errorf("the tarball reads %q", b)
	}
	dst = t.TempDir()
	if _, err := tc.CopyFromTemp(ctx, "pack", dst, 1<<20); err != nil {
		t.Fatalf("a directory: %v", err)
	}
	if b, _ := os.ReadFile(filepath.Join(dst, "pack", "sub", "notes.txt")); string(b) != "notes" {
		t.Errorf("notes.txt reads %q", b)
	}
	if _, err := os.Lstat(filepath.Join(dst, "pack", "passwd")); err == nil {
		t.Error("a symlink came out of the container")
	}
	if n, err := tc.CopyFromTemp(ctx, "nothing-here", t.TempDir(), 1<<20); err != nil || n != 0 {
		t.Errorf("a path not there: %d, %v", n, err)
	}

	src := t.TempDir()
	if err := os.MkdirAll(filepath.Join(src, "dmg"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "dmg", "Wake-1.dmg"), []byte("dmg"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := tc.CopyToTemp(ctx, src); err != nil {
		t.Fatalf("CopyToTemp: %v", err)
	}
	code, out := run(t, env, "publish", `set -e
test "$(cat "$RUNNER_TEMP/dmg/Wake-1.dmg")" = dmg
rm -rf "$RUNNER_TEMP/dmg"
echo removed`)
	if code != 0 || !strings.Contains(out, "removed") {
		t.Errorf("the step's user could not read and remove what came in: exit %d\n%s", code, out)
	}
}
