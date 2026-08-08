// Package invariants holds one test per normative rule in RUNNER_SPEC §5.
//
// The names carry the rule number so a CI failure says which invariant broke
// before anyone opens a file. A pull request that weakens one of these is
// rejected regardless of its other merits.
//
// Every test here starts skipped. That is deliberate: a skipped test is
// visible debt that shows up on every `go test ./...`, whereas a test that
// does not exist yet is invisible and gets written late, or not at all. Each
// is unskipped by the workstream that makes it pass, in the same pull request
// that makes it pass — never separately.
//
// These are not build-tagged. They must run in the default `go test ./...` so
// their skip lines stay in front of people.
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
// Fuzz both command kinds with hostile values in every field; assert no
// execution occurs outside the registry-declared command set, and that the
// decoder rejects any kind outside the closed set rather than defaulting.
//
// Two thirds are already covered at the unit level:
// pkg/protocol/command_test.go pins the closed command set, and
// internal/registry/admission_test.go pins that a hostile input reaches only
// an environment map and never a command string. What remains is the
// end-to-end claim — that no execution occurs outside the registry-declared
// command set — which needs a real executor to assert against.
func TestINV2_NamesNotDefinitions(t *testing.T) {
	t.Skip("registry half done in WS-01; execution half needs WS-10 (run orchestration)")
}

// INV-3 — Broker-supplied values are never interpolated into a shell.
//
// The validation table — --upload-pack=/bin/sh, "; rm -rf /", $(id),
// ../../etc/passwd, embedded newlines, 10 KB refs, unicode homoglyphs — lives
// in internal/registry/ref_test.go and passes.
//
// What is left is the claim validation exists to back up: that every git
// invocation passes argv as a slice with "--" before user-influenced
// arguments, and that nothing anywhere reaches `sh -c`. That is a property of
// the call sites, not of the validator, and the call sites are in
// internal/source.
func TestINV3_NoShellInterpolation(t *testing.T) {
	t.Skip("validation done in WS-01; git call sites need WS-06 (source acquisition)")
}

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
// the invariant — inputs_json in the store and Update.ErrorMsg — do not exist
// yet; WS-05 and WS-10 extend this test when they land. What is asserted here
// is the dominant leak path and the one §16 is written about.
func TestINV4_SecretsNeverLeave(t *testing.T) {
	const p8 = `-----BEGIN PRIVATE KEY-----
MIIEvQIBADANBgkqhkiG9w0BAQEFAASCBKcwggSjAgEAAoIBAQC7VJTUt9Us8cKj
MzEfYyjiWA4R4/M2bS1GB4t7NXp98C3SC6dVMvDuictGeurT8jNbvJZHtCSuYEvu
NMoSfm76oqFvAp8Gy0iz5sxjZmSnXyCdPEovGhLa0VzMaQ8s+CLOyS56YyCFGeJZ
qgtzJ6GR3eqoYSW9b9UMvkBpZODSctWSNGj3P7jRFDO5VoTwCQAWbFnOjDfH5Ulg
-----END PRIVATE KEY-----`

	secrets := map[string]string{
		"APP_STORE_CONNECT_KEY_P8":  p8,
		"MATCH_PASSWORD":            "correct-horse-battery-staple",
		"ANDROID_KEYSTORE_PASSWORD": "hunter2-but-longer",
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
// The policy half passes in internal/registry/ref_test.go: normalization
// before matching, refs/heads/main and main receiving the same answer, tags
// requiring an explicit entry, and refs/pull/* failing by construction.
//
// Still outstanding: that jobs.yaml is read from the checked-out workspace at
// the resolved commit and never from a broker, and that approval is a local
// action no inbound command can grant. Both are orchestration properties.
func TestINV5_RegistryFromDisk(t *testing.T) {
	t.Skip("policy half done in WS-01; disk read and approval need WS-10 (run orchestration)")
}
