package run

import (
	"context"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"

	"github.com/Abyssinia-Labs/latem/internal/executor"
	"github.com/Abyssinia-Labs/latem/internal/logstream"
	"github.com/Abyssinia-Labs/latem/internal/registry"
	"github.com/Abyssinia-Labs/latem/internal/secrets"
	"github.com/Abyssinia-Labs/latem/internal/store"
	"github.com/Abyssinia-Labs/latem/pkg/protocol"
)

const secretValue = "correct horse battery staple"

func TestEnv_AssemblesSecretsWorkspaceAndJob(t *testing.T) {
	h := newHarness(t)
	h.source.registry = `
version: 1
jobs:
  build:
    runs_on: [linux]
    secrets: [MATCH_PASSWORD]
    steps:
      - name: sign
        run: ./ci/sign.sh
        env:
          RELEASE_CHANNEL: internal
`
	o := h.build()

	res := h.run(o, h.request("build"))
	if res.State != protocol.StateSucceeded {
		t.Fatalf("state = %s (%s)", res.State, res.ErrorMsg)
	}

	spec := h.env.runSpec()

	// §15: the []byte → string conversion happens here and nowhere else.
	if spec.Env["MATCH_PASSWORD"] != secretValue {
		t.Errorf("secret missing from the run environment: %q", spec.Env["MATCH_PASSWORD"])
	}
	// §12: the agent exports these regardless, so a job need not shell out.
	for k, want := range map[string]string{
		"LATEM_REPO":   "app",
		"LATEM_RUN_ID": "run1",
		"LATEM_REF":    "refs/heads/main",
		"LATEM_SHA":    h.source.sha,
		"LATEM_JOB":    "build",
	} {
		if spec.Env[k] != want {
			t.Errorf("%s = %q, want %q", k, spec.Env[k], want)
		}
	}

	// A10: the executor is told which keys are credentials so $LATEM_ENV can
	// refuse to overwrite exactly those and no others.
	if !slices.Equal(spec.Secrets, []string{"MATCH_PASSWORD"}) {
		t.Errorf("Spec.Secrets = %v, want [MATCH_PASSWORD]", spec.Secrets)
	}

	// steps[].env is per step, merged over the run environment by the executor.
	steps := h.env.executed()
	if len(steps) != 1 || steps[0].Env["RELEASE_CHANNEL"] != "internal" {
		t.Fatalf("step env = %+v", steps)
	}
	if _, leaked := spec.Env["RELEASE_CHANNEL"]; leaked {
		t.Error("a step's own env leaked into the run environment")
	}
}

// A9: the $LATEM_ENV path comes from Env.EnvFile and is never constructed by
// the orchestrator, because it is a container path for docker, a guest path
// for tart, and a host path for native.
func TestEnv_LatemEnvPathComesFromTheExecutor(t *testing.T) {
	h := newHarness(t)
	h.env.envFile = "/somewhere/only/the/executor/knows"
	o := h.build()

	h.run(o, h.request("test"))
	for _, s := range h.env.executed() {
		if s.Env[LatemEnvVar] != h.env.envFile {
			t.Fatalf("$LATEM_ENV = %q, want %q", s.Env[LatemEnvVar], h.env.envFile)
		}
	}
}

// A job cannot spoof the agent's own variables. The registry refuses the whole
// LATEM_* namespace at parse, so this never reaches the environment builder —
// and the environment builder applies $LATEM_ENV last regardless, which is the
// second of the two defences.
func TestEnv_JobCannotRedefineLatemEnv(t *testing.T) {
	h := newHarness(t)
	h.source.registry = `
version: 1
jobs:
  sneaky:
    runs_on: [linux]
    steps:
      - name: try
        run: ./try.sh
        env:
          LATEM_ENV: /tmp/mine
`
	o := h.build()

	res := h.run(o, h.request("sneaky"))
	if res.ErrorCode != protocol.ErrBadRegistry {
		t.Fatalf("code = %s, want E_BAD_REGISTRY", res.ErrorCode)
	}

	// And the layering: a step env the registry does allow cannot displace it.
	x := &execution{env: h.env}
	got := x.stepEnv(registryStep(map[string]string{"NODE_ENV": "test"}))
	if got[LatemEnvVar] != h.env.EnvFile() || got["NODE_ENV"] != "test" {
		t.Fatalf("step env = %+v", got)
	}
}

func TestEnv_InputsAreSubstitutedIntoStepEnvOnly(t *testing.T) {
	h := newHarness(t)
	h.source.registry = `
version: 1
jobs:
  release:
    runs_on: [linux]
    inputs:
      channel:
        type: enum
        values: [internal, external]
        default: internal
    steps:
      - name: ship
        run: ./ship.sh
        env:
          RELEASE_CHANNEL: ${input.channel}
`
	o := h.build()

	req := h.request("release")
	req.Inputs = map[string]string{"channel": "external"}
	res := h.run(o, req)
	if res.State != protocol.StateSucceeded {
		t.Fatalf("state = %s (%s)", res.State, res.ErrorMsg)
	}

	steps := h.env.executed()
	if steps[0].Env["RELEASE_CHANNEL"] != "external" {
		t.Errorf("RELEASE_CHANNEL = %q", steps[0].Env["RELEASE_CHANNEL"])
	}
	// The command itself is literal; nothing interpolates into `run`.
	if steps[0].Run != "./ship.sh" {
		t.Errorf("run = %q, want the literal from the registry", steps[0].Run)
	}
}

func TestEnv_UndeclaredInputIsEBadInput(t *testing.T) {
	h := newHarness(t)
	o := h.build()

	req := h.request("test")
	req.Inputs = map[string]string{"surprise": "1"}
	res := h.run(o, req)
	if res.State != protocol.StateRejected || res.ErrorCode != protocol.ErrBadInput {
		t.Fatalf("got %s/%s, want rejected/E_BAD_INPUT", res.State, res.ErrorCode)
	}
}

func TestEnv_SpecCarriesImageCachesLabelsAndResources(t *testing.T) {
	h := newHarness(t)
	h.runners[0].DefaultImage = "ghcr.io/me/base:20"
	h.runners[0].Resources = executor.Resources{CPUs: 4, Memory: "8g"}
	o := h.build()

	h.run(o, h.request("test"))
	spec := h.env.runSpec()

	if spec.Image != "ghcr.io/me/base:20" {
		t.Errorf("image = %q, want the runner default", spec.Image)
	}
	if spec.Workspace == "" || !strings.HasSuffix(spec.Workspace, "run1") {
		t.Errorf("workspace = %q", spec.Workspace)
	}
	if spec.Labels["latem.run-id"] != "run1" {
		t.Errorf("labels = %+v, want latem.run-id for crash reaping", spec.Labels)
	}
	if spec.Resources.CPUs != 4 || spec.Resources.Memory != "8g" {
		t.Errorf("resources = %+v", spec.Resources)
	}
	// A11: the mount target comes from the registry's caches[].path.
	if len(spec.Caches) != 1 || spec.Caches[0].Target != "/caches/pnpm" {
		t.Fatalf("caches = %+v, want a target from the registry", spec.Caches)
	}
	reqs := h.caches.requests()
	if len(reqs) != 1 || reqs[0].Name != "pnpm" || reqs[0].Target != "/caches/pnpm" {
		t.Fatalf("cache requests = %+v", reqs)
	}
}

// A job's own image: overrides the runner default.
func TestEnv_JobImageOverridesTheRunnerDefault(t *testing.T) {
	h := newHarness(t)
	h.runners[0].DefaultImage = "ghcr.io/me/base:20"
	h.source.registry = `
version: 1
jobs:
  pinned:
    runs_on: [linux]
    image: ghcr.io/me/special:1
    steps:
      - name: only
        run: ./only.sh
`
	o := h.build()

	h.run(o, h.request("pinned"))
	if got := h.env.runSpec().Image; got != "ghcr.io/me/special:1" {
		t.Fatalf("image = %q", got)
	}
}

func TestSecrets_MissingIsEMissingSecret(t *testing.T) {
	h := newHarness(t)
	h.secret.values = nil
	o := h.build()

	res := h.run(o, h.request("secretive"))
	if res.State != protocol.StateRejected || res.ErrorCode != protocol.ErrMissingSecret {
		t.Fatalf("got %s/%s, want rejected/E_MISSING_SECRET", res.State, res.ErrorCode)
	}
	if h.exec.prepared != 0 {
		t.Error("a job with an unresolvable secret reached the executor")
	}
	if !strings.Contains(res.ErrorMsg, "MATCH_PASSWORD") {
		t.Errorf("the message does not name the secret: %q", res.ErrorMsg)
	}
}

// A16: an unusable backend is a machine condition, not a missing secret, and
// it is retryable.
func TestSecrets_BackendFailureIsRetryable(t *testing.T) {
	h := newHarness(t)
	h.secret.err = &secrets.CommandError{Program: "security", ExitCode: 1, Err: errors.New("locked")}
	o := h.build()

	res := h.run(o, h.request("secretive"))
	if res.ErrorCode != protocol.ErrSecretBackend {
		t.Fatalf("code = %s, want E_SECRET_BACKEND", res.ErrorCode)
	}
	if !res.ErrorCode.Retryable() {
		t.Error("E_SECRET_BACKEND should be retryable")
	}
}

func TestSecrets_NoProviderConfigured(t *testing.T) {
	h := newHarness(t)
	o := h.build()
	o.secrets = nil

	res := h.run(o, h.request("secretive"))
	if res.ErrorCode != protocol.ErrSecretBackend {
		t.Fatalf("code = %s, want E_SECRET_BACKEND", res.ErrorCode)
	}
}

// INV-4: a secret a build prints reaches neither the log file, nor the broker,
// nor Update.ErrorMsg, nor the store.
func TestSecrets_NeverLeakIntoLogsOrState(t *testing.T) {
	h := newHarness(t)
	h.source.registry = `
version: 1
jobs:
  leaky:
    runs_on: [linux]
    secrets: [MATCH_PASSWORD]
    steps:
      - name: oops
        run: ./oops.sh
`
	h.env.exec = func(_ context.Context, _ executor.Step, stdout, stderr io.Writer) (int, error) {
		_, _ = io.WriteString(stdout, "echoing the password: "+secretValue+"\n")
		_, _ = io.WriteString(stderr, "and again: "+secretValue+"\n")
		// The failure message carries it too, which is the path INV-4 is
		// easiest to lose on: an error string nobody thought of as output.
		return -1, errors.New("connect failed with token " + secretValue)
	}
	o := h.build()

	res := h.run(o, h.request("leaky"))
	if res.State != protocol.StateFailed {
		t.Fatalf("state = %s", res.State)
	}

	if strings.Contains(res.ErrorMsg, secretValue) {
		t.Error("the secret reached Update.ErrorMsg")
	}
	if got := h.logContents("run1"); strings.Contains(got, secretValue) {
		t.Error("the secret reached the local log file")
	} else if !strings.Contains(got, logstream.Marker) {
		t.Error("nothing was redacted, so the log did not carry the value at all")
	}

	waitFor(t, "the failure to be reported", func() bool {
		for _, r := range h.broker.updates() {
			if r.update.State == protocol.StateFailed {
				return true
			}
		}
		return false
	})
	for _, r := range h.broker.updates() {
		if strings.Contains(r.update.ErrorMsg, secretValue) {
			t.Error("the secret crossed to the broker in an update")
		}
	}
	for _, f := range h.broker.lines() {
		if strings.Contains(f.Line, secretValue) {
			t.Error("the secret crossed to the broker in a log frame")
		}
	}

	row := mustGet(t, h.db, "run1")
	if strings.Contains(row.ErrorMsg, secretValue) {
		t.Error("the secret reached sqlite")
	}
}

// A9: stdout and stderr are reported separately, so LogFrame.Stream is
// fillable — the whole reason Exec takes two writers.
func TestLogs_StdoutAndStderrAreDistinguished(t *testing.T) {
	h := newHarness(t)
	h.env.exec = func(_ context.Context, _ executor.Step, stdout, stderr io.Writer) (int, error) {
		_, _ = io.WriteString(stdout, "to stdout\n")
		_, _ = io.WriteString(stderr, "to stderr\n")
		return 0, nil
	}
	o := h.build()
	h.run(o, h.request("test"))

	seen := map[protocol.Stream]string{}
	waitFor(t, "both streams", func() bool {
		seen = map[protocol.Stream]string{}
		for _, f := range h.broker.lines() {
			if strings.Contains(f.Line, "to std") {
				seen[f.Stream] = f.Line
			}
		}
		return len(seen) == 2
	})
	if !strings.Contains(seen[protocol.StreamStdout], "to stdout") {
		t.Errorf("stdout frame = %q", seen[protocol.StreamStdout])
	}
	if !strings.Contains(seen[protocol.StreamStderr], "to stderr") {
		t.Errorf("stderr frame = %q", seen[protocol.StreamStderr])
	}

	// Agent narration is its own stream.
	var agent int
	for _, f := range h.broker.lines() {
		if f.Stream == protocol.StreamAgent {
			agent++
		}
	}
	if agent == 0 {
		t.Error("no agent-stream narration at all")
	}
}

// TestLogs_SeqIsPersistedAsTheResumePoint is finding F6's durable half. Since
// amendment A28 the persisting is the transport's, under its own key, and the
// key is the assertion: a resume point written where nobody reads it is the same
// as none at all.
func TestLogs_SeqIsPersistedAsTheResumePoint(t *testing.T) {
	h := newHarness(t)
	o := h.build()
	h.run(o, h.request("test"))

	seq, err := h.db.LastAckedSeq(t.Context(), store.BrokerLocal, "run1")
	if err != nil {
		t.Fatalf("LastAckedSeq: %v", err)
	}
	if seq == 0 {
		t.Fatal("no log resume point was persisted (finding F6)")
	}
	if other, oerr := h.db.LastAckedSeq(t.Context(), store.BrokerRemote, "run1"); oerr != nil || other != 0 {
		t.Errorf("last_acked_seq(remote) = %d, %v; a transport that never saw a frame was credited with %d of them", other, oerr, other)
	}
}

// A broker that will not take logs is not a reason to refuse to build.
func TestLogs_BrokerSinkFailureIsNotFatal(t *testing.T) {
	h := newHarness(t)
	h.broker.logsErr = errors.New("no log channel")
	o := h.build()

	res := h.run(o, h.request("test"))
	if res.State != protocol.StateSucceeded {
		t.Fatalf("state = %s (%s)", res.State, res.ErrorMsg)
	}
	if !strings.Contains(h.logContents("run1"), "broker log sink unavailable") {
		t.Error("the degradation was not narrated")
	}
}

func registryStep(env map[string]string) registry.ResolvedStep {
	return registry.ResolvedStep{Name: "s", Run: "./s.sh", Env: env}
}
