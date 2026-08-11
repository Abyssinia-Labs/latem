// Package invariants holds one test per normative rule in RUNNER_SPEC §5.
//
// The names carry the rule number so a CI failure says which invariant broke
// before anyone opens a file. A pull request that weakens one of these is
// rejected regardless of its other merits.
//
// Every test here started skipped. That was deliberate: a skipped test is
// visible debt that shows up on every `go test ./...`, whereas a test that
// does not exist yet is invisible and gets written late, or not at all. Each
// was unskipped by the workstream that made it pass, in the same pull request
// that made it pass — never separately. As of WS-16 none of the five is
// skipped, and a skip reappearing here is a regression rather than a plan.
//
// These are not build-tagged. They must run in the default `go test ./...`.
// The three cross-cutting ones need a real agent, a real sqlite database and a
// real git repository, all of which this suite builds for itself; none of them
// needs a daemon, so the default gate stays runnable on a laptop.
package invariants

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/Abyssinia-Labs/latem/internal/logstream"
	"github.com/Abyssinia-Labs/latem/pkg/protocol"
)

// INV-1 — The agent only makes outbound connections.
//
// Unskipped by WS-11 and moved to inv1_unix_test.go, which carries a
// `//go:build unix` tag: enumerating a process's own listening sockets needs
// syscalls that do not exist on Windows, and keeping them out of this file is
// what lets the portable invariants below still compile there.

// INV-2 — The broker supplies names, never definitions.
//
// Unskipped by WS-16 and moved to inv2_test.go. The end-to-end half it was
// waiting for — that no execution occurs outside the registry-declared command
// set — needs a broker, an admission chain and an executor in one process, so
// it lives with the harness in harness_test.go rather than here.

// INV-3 — Broker-supplied values are never interpolated into a shell.
//
// Unskipped by WS-16 and split in two. inv3_test.go is portable and holds the
// structural half — no file in the tree may build a shell invocation outside
// the four sanctioned step-execution sites. inv3_unix_test.go carries a
// `//go:build unix` tag and holds the behavioural half, because its detector is
// a shell planted on PATH and the negative control that proves the detector
// works has to execute one.

// INV-4 — Secrets never leave the machine.
//
// Run a job that echoes every loaded secret; assert none appears in the broker
// transcript, the persisted log file, inputs_json, or the run's error field.
//
// The fixture set MUST include a multi-line PEM: header, at least four base64
// body lines, footer. A version of this test covering only single-line secrets
// passes while the leak described in RUNNER_SPEC §16 is live — that is exactly
// how draft 01 of the spec shipped an unredacted App Store Connect key with a
// green test suite. Single-line coverage alone does not satisfy INV-4.
//
// This covers the log path end to end: a real emitter, a real on-disk file
// sink, and a sink standing in for the broker. Two further surfaces named by
// the invariant — inputs_json in the store and Update.ErrorMsg — did not exist
// when it was written. They do now, and inv4_e2e_test.go is the extension:
// a whole job through a real agent, with every byte the agent wrote to disk
// swept afterwards. What is asserted here is the dominant leak path and the
// one §16 is written about.
func TestINV4_SecretsNeverLeave(t *testing.T) {
	const p8 = `-----BEGIN PRIVATE KEY-----
MIIEvQIBADANBgkqhkiG9w0BAQEFAASCBKcwggSjAgEAAoIBAQC7VJTUt9Us8cKj
MzEfYyjiWA4R4/M2bS1GB4t7NXp98C3SC6dVMvDuictGeurT8jNbvJZHtCSuYEvu
NMoSfm76oqFvAp8Gy0iz5sxjZmSnXyCdPEovGhLa0VzMaQ8s+CLOyS56YyCFGeJZ
qgtzJ6GR3eqoYSW9b9UMvkBpZODSctWSNGj3P7jRFDO5VoTwCQAWbFnOjDfH5Ulg
-----END PRIVATE KEY-----`

	// A multi-line secret that is not a PEM, added by WS-16, and the reason is
	// worth stating because it is the difference between this test working and
	// this test looking like it works.
	//
	// The emitter carries a second defence beyond §16's registration rules: a
	// guard that suppresses every line between "-----BEGIN … PRIVATE KEY-----"
	// and its END, for undeclared keys a build prints by accident. It is a good
	// guard. It also means the PEM above is protected whether or not the
	// redactor registered the value's individual lines — so with only a PEM
	// here, reverting finding F3 in internal/logstream leaves this test green.
	// Verified by doing it. That is precisely the regression §5 says this test
	// exists to catch, so the fixture set needs a multi-line secret with no
	// guard standing behind it.
	const serviceAccountJSON = `{
  "type": "service_account",
  "project_id": "latem-releases",
  "private_key_id": "8f2c1a9e4b7d0c3a5e6f8091a2b3c4d5e6f70819",
  "client_email": "releases@latem-releases.iam.gserviceaccount.com"
}`

	secrets := map[string]string{
		"APP_STORE_CONNECT_KEY_P8":    p8,
		"GOOGLE_SERVICE_ACCOUNT_JSON": serviceAccountJSON,
		"MATCH_PASSWORD":              "correct-horse-battery-staple",
		"ANDROID_KEYSTORE_PASSWORD":   "hunter2-but-longer",
	}

	dir := t.TempDir()
	red := logstream.NewRedactor()
	for _, v := range secrets {
		red.Register([]byte(v))
	}

	fileSink, err := logstream.NewFileSink(dir, "run_inv4")
	if err != nil {
		t.Fatal(err)
	}
	broker := &recordingSink{}
	emitter := logstream.NewEmitter("run_inv4", red, fileSink, broker)

	// The job echoes every secret, the way a debugging `set -x` or a careless
	// `env` would.
	var script strings.Builder
	for name, value := range secrets {
		script.WriteString("+ echo \"$" + name + "\"\n")
		script.WriteString(value + "\n")
	}
	script.WriteString("BUILD SUCCESSFUL\n")

	if err := emitter.Consume(context.Background(), protocol.StreamStdout, strings.NewReader(script.String())); err != nil {
		t.Fatal(err)
	}
	if err := emitter.Close(); err != nil {
		t.Fatal(err)
	}

	onDisk, err := os.ReadFile(filepath.Join(dir, "run_inv4.log"))
	if err != nil {
		t.Fatal(err)
	}
	transcript := strings.Join(broker.lines(), "\n")

	for name, value := range secrets {
		// Every line of a multi-line secret is checked individually, because
		// the whole value never appears in one line and a whole-value check
		// would therefore pass against a broken redactor.
		for i, line := range strings.Split(value, "\n") {
			line = strings.TrimSpace(line)
			if len(line) < 6 {
				continue
			}
			if strings.Contains(string(onDisk), line) {
				t.Errorf("%s line %d reached the log file:\n%s", name, i, line)
			}
			if strings.Contains(transcript, line) {
				t.Errorf("%s line %d reached the broker transcript:\n%s", name, i, line)
			}
		}
	}

	// The build's own output must survive, or the redactor is useless in a
	// different way.
	if !strings.Contains(string(onDisk), "BUILD SUCCESSFUL") {
		t.Error("ordinary output was destroyed along with the secrets")
	}
}

// recordingSink stands in for whatever a broker does with frames.
type recordingSink struct {
	mu     sync.Mutex
	frames []protocol.LogFrame
}

func (s *recordingSink) Write(frames []protocol.LogFrame) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.frames = append(s.frames, frames...)
	return nil
}

func (s *recordingSink) Close() error { return nil }

func (s *recordingSink) lines() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, len(s.frames))
	for i, f := range s.frames {
		out[i] = f.Line
	}
	return out
}

// INV-5 — The registry comes from disk, at the pinned commit.
//
// Unskipped by WS-16 and moved to inv5_test.go. Both outstanding halves —
// that jobs.yaml is read from the checked-out workspace at the resolved
// commit, and that approval is a local action bound to a commit (amendment
// A27) — are orchestration properties and need the harness in
// harness_test.go.
