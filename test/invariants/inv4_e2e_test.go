package invariants

// INV-4, the half invariants_test.go said was outstanding.
//
// That test covers the log pipeline end to end — a real emitter, a real file
// sink, a sink standing in for a broker — and its own comment records what it
// could not reach: "Two further surfaces named by the invariant — inputs_json
// in the store and Update.ErrorMsg — do not exist yet; WS-05 and WS-10 extend
// this test when they land."
//
// They have landed, and this is that extension. It runs a whole job through a
// real agent with real secrets resolved by the real §15 chain interface, and
// then sweeps every byte the agent wrote to disk.
//
// # Why the assertion is a filesystem sweep
//
// The invariant names four surfaces: the broker transcript, the log file,
// inputs_json, and the run's error field. Three of the four are in sqlite, and
// checking them by reading the columns back means trusting the same query layer
// that would have written the leak — and missing anything written to a column
// nobody thought to check, or sitting in the write-ahead log. So the columns
// are checked *and* every file under the agent's home is grepped, which is one
// assertion that covers the database, the WAL, the log file, the artifacts, and
// any file a future surface adds without anybody updating this test.

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Abyssinia-Labs/latem/internal/agent"
	"github.com/Abyssinia-Labs/latem/internal/secrets"
	"github.com/Abyssinia-Labs/latem/pkg/protocol"
)

// leakyPEM is RUNNER_SPEC §16's own example: an App Store Connect key.
//
// Multi-line, because that is the whole point. Registering only the whole value
// of a multi-line secret redacts nothing but the first line, and a test using
// only single-line secrets passes while the leak is live — which is how draft
// 01 shipped.
const leakyPEM = `-----BEGIN PRIVATE KEY-----
MIIEvQIBADANBgkqhkiG9w0BAQEFAASCBKcwggSjAgEAAoIBAQC7VJTUt9Us8cKj
MzEfYyjiWA4R4/M2bS1GB4t7NXp98C3SC6dVMvDuictGeurT8jNbvJZHtCSuYEvu
NMoSfm76oqFvAp8Gy0iz5sxjZmSnXyCdPEovGhLa0VzMaQ8s+CLOyS56YyCFGeJZ
qgtzJ6GR3eqoYSW9b9UMvkBpZODSctWSNGj3P7jRFDO5VoTwCQAWbFnOjDfH5Ulg
-----END PRIVATE KEY-----`

// leakyJSON is a multi-line secret that is NOT a PEM, and it is the fixture
// that makes this test able to fail.
//
// internal/logstream's emitter carries a second, independent defence beyond
// §16's registration rules: a guard that suppresses every line between
// "-----BEGIN … PRIVATE KEY-----" and its END. It is a good guard and it exists
// for undeclared keys a build prints by accident. It also means a PEM fixture
// is protected whether or not the redactor registered the value's individual
// lines — so a test whose only multi-line secret is a PEM stays green with
// finding F3 reverted, which is the precise defect §5 says this test must
// catch. Verified by reverting F3: with only the PEM, both INV-4 tests passed.
//
// A Google service-account JSON is the obvious real multi-line secret with no
// guard behind it. §16's rule is about multi-line values, not about PEMs; the
// PEM is only the example that motivated it.
const leakyJSON = `{
  "type": "service_account",
  "project_id": "latem-releases",
  "private_key_id": "8f2c1a9e4b7d0c3a5e6f8091a2b3c4d5e6f70819",
  "client_email": "releases@latem-releases.iam.gserviceaccount.com",
  "token_uri": "https://oauth2.googleapis.com/token"
}`

var inv4Secrets = map[string]string{
	"APP_STORE_CONNECT_KEY_P8":    leakyPEM,
	"GOOGLE_SERVICE_ACCOUNT_JSON": leakyJSON,
	"MATCH_PASSWORD":              "correct-horse-battery-staple",
	"ANDROID_KEYSTORE_PASSWORD":   "hunter2-but-longer",
}

// secretJobs declares them, so §15 resolves them and the executor is handed
// them in Spec.Env with Spec.Secrets naming the keys (amendment A10).
const secretJobs = `version: 1

jobs:
  leaky:
    runs_on: [test]
    timeout: 5m
    secrets:
      - APP_STORE_CONNECT_KEY_P8
      - GOOGLE_SERVICE_ACCOUNT_JSON
      - MATCH_PASSWORD
      - ANDROID_KEYSTORE_PASSWORD
    inputs:
      note:
        type: string
        max_length: 8192
        default: ""
    steps:
      - name: dump everything
        run: ./ci/build.sh
        env:
          RELEASE_NOTE: ${input.note}
`

// staticSecrets is the §15 chain, reduced to a map.
type staticSecrets struct{}

func (staticSecrets) Name() string { return "test" }

func (staticSecrets) Resolve(_ context.Context, name string) (secrets.Value, error) {
	if v, ok := inv4Secrets[name]; ok {
		return secrets.Value(v), nil
	}
	return nil, secrets.ErrNotFound
}

// TestINV4_SecretsNeverLeaveTheMachineEndToEnd.
//
// The job's step behaves like a build with `set -x` left on and a driver that
// puts its environment in an error message: it writes every injected secret to
// stdout and to stderr, and then fails with the PEM in the failure text. Both
// are ordinary. `eas build` prints its environment on some failures, and a
// non-zero exit carrying the last few hundred bytes of output is how most
// tooling reports a problem.
//
// Everything downstream of that is the system under test.
func TestINV4_SecretsNeverLeaveTheMachineEndToEnd(t *testing.T) {
	m := newMachine(t, machineOptions{
		jobs: secretJobs,
		mutate: func(o *agent.Options) {
			o.Secrets = staticSecrets{}
		},
	})
	m.exec.setLeaky()

	// A benign input, so inputs_json has something in it and a check against
	// that column is checking a populated column rather than a NULL one.
	inputs := map[string]string{"note": "shipping the thing"}
	if got := m.submit(t, runFrame("r_inv4", "app", "leaky", "main", inputs)); !got.OK {
		t.Fatalf("the run was refused at the transport: %s", got.Error)
	}
	r := m.waitForState(t, "r_inv4", 90*time.Second,
		protocol.StateSucceeded, protocol.StateFailed, protocol.StateRejected)

	// --- the control: the secrets actually reached the run ------------------
	//
	// Without this the test passes against a run that was rejected at gate 15,
	// never resolved a secret, and therefore had nothing to leak — the exact
	// shape of the vacuous INV-4 that shipped over a live leak.
	if r.State != protocol.StateFailed || r.ErrorCode != protocol.ErrExecutor {
		t.Fatalf("the run finished %s (%s): %s — it is supposed to reach the executor and fail "+
			"there with the secret in the error text", r.State, r.ErrorCode, r.ErrorMsg)
	}
	injected := m.exec.injectedSecrets()
	for name, want := range inv4Secrets {
		if injected[name] != want {
			t.Fatalf("%s did not reach the executor's environment (got %q); nothing below is "+
				"testing redaction", name, injected[name])
		}
	}
	if len(m.exec.emitted()) == 0 {
		t.Fatal("the step wrote nothing; the log pipeline was never given a secret to redact")
	}

	// --- the ordinary output survives ---------------------------------------
	//
	// A redactor that destroyed the build's own output would satisfy every
	// assertion below and be useless.
	logBody := readLog(t, m, "r_inv4")
	if !strings.Contains(logBody, "BUILD OUTPUT MARKER") {
		t.Errorf("the run's ordinary output did not reach the log file:\n%s", logBody)
	}

	// --- the four surfaces INV-4 names --------------------------------------
	for name, value := range inv4Secrets {
		for i, line := range strings.Split(value, "\n") {
			line = strings.TrimSpace(line)
			if len(line) < 6 {
				continue
			}
			if strings.Contains(logBody, line) {
				t.Errorf("%s line %d reached the persisted log file: %s", name, i, line)
			}
			if strings.Contains(r.ErrorMsg, line) {
				t.Errorf("%s line %d reached the run's error field: %s", name, i, line)
			}
			for k, v := range r.Inputs {
				if strings.Contains(v, line) {
					t.Errorf("%s line %d reached inputs_json under key %q", name, i, k)
				}
			}
		}
	}

	// --- and every other byte the agent wrote -------------------------------
	assertNothingUnderHomeContainsASecret(t, m.home)
}

// assertNothingUnderHomeContainsASecret sweeps the agent's whole state
// directory: the sqlite database, its write-ahead log, the run log, the
// artifacts, and anything else that appeared.
//
// The sweep carries its own control. It first checks that a value which is
// *not* a secret and *is* known to be on disk can be found — otherwise a walk
// that silently read nothing would report the invariant clean having examined
// nothing, which is amendment A25's failure in a different costume.
func assertNothingUnderHomeContainsASecret(t *testing.T, home string) {
	t.Helper()

	var files []string
	err := filepath.WalkDir(home, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil //nolint:nilerr // an unreadable subtree is not evidence either way
		}
		files = append(files, path)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("the sweep found no files under the agent's home; it is looking in the wrong place")
	}

	var sawControl bool
	for _, path := range files {
		body, rerr := os.ReadFile(path)
		if rerr != nil {
			continue
		}
		text := string(body)
		if strings.Contains(text, "BUILD OUTPUT MARKER") {
			sawControl = true
		}
		for name, value := range inv4Secrets {
			for i, line := range strings.Split(value, "\n") {
				line = strings.TrimSpace(line)
				if len(line) < 6 {
					continue
				}
				if strings.Contains(text, line) {
					t.Errorf("%s line %d is on disk in %s:\n  %s",
						name, i, strings.TrimPrefix(path, home), line)
				}
			}
		}
	}
	if !sawControl {
		t.Errorf("the sweep read %d files under %s and did not find the build's own output in "+
			"any of them; it cannot distinguish \"no secrets on disk\" from \"nothing was read\"",
			len(files), home)
	}
}

func readLog(t *testing.T, m *machine, runID string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(m.cfg.Paths.Logs, runID+".log"))
	if err != nil {
		t.Fatalf("reading the run's log file: %v", err)
	}
	return string(b)
}
