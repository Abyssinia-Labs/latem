package agent

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Abyssinia-Labs/latem/internal/config"
	"github.com/Abyssinia-Labs/latem/internal/logstream"
	"github.com/Abyssinia-Labs/latem/pkg/protocol"
)

// RUNNER_SPEC §16, "Redactor contents", lists the machine token first. Nothing
// in the tree registered it: internal/secrets registers what a job *declares*,
// and the machine token is not a declared secret — it is the credential that
// authenticates this machine to the control plane, kept in its own file
// precisely so that it is never an environment variable anybody could declare.
//
// The gap was live rather than theoretical. A build that printed the file — a
// script that cats the config directory, a `set -x` over an installer, a support
// bundle — put the token in ~/.latem/logs/<run-id>.log and shipped it to the
// control plane over the very link it authenticates, with every INV-4 test
// passing.

// TestMachineToken_IsRedactedFromBuildOutput is the gap closed end to end: a
// real run prints the token, and neither the log file on disk nor the frames
// leaving the machine contain it.
func TestMachineToken_IsRedactedFromBuildOutput(t *testing.T) {
	cp := newControlPlane(t)
	cfg := newBothTransportsConfig(t, cp)

	token := readTokenFile(t, cfg.Paths.TokenFile)
	if len(token) < logstream.MinRedactableLength {
		t.Fatalf("the test fixture's token is %d characters, below the redaction floor; this test would pass vacuously", len(token))
	}

	ta := newTestAgent(t, cfg, nil)
	ta.exec.emit = func(w io.Writer) {
		_, _ = io.WriteString(w, "curl -H 'Authorization: Bearer "+token+"' https://api.example.invalid\n")
		_, _ = io.WriteString(w, "done\n")
	}
	ta.run(t)

	const runID = "r_token"
	enqueueDirectly(t, cfg, runID, "main")
	run := waitState(t, cfg, runID, 120*time.Second, protocol.StateSucceeded, protocol.StateFailed)
	if run.State != protocol.StateSucceeded {
		t.Fatalf("run state = %s (%s)", run.State, run.ErrorMsg)
	}

	body, err := os.ReadFile(filepath.Join(cfg.Paths.Logs, runID+".log"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), token) {
		t.Errorf("the machine token appears verbatim in %s; §16 lists it among the redactor's contents and a build that prints it has leaked the credential for this whole machine",
			filepath.Join(cfg.Paths.Logs, runID+".log"))
	}
	if !strings.Contains(string(body), logstream.Marker) {
		t.Errorf("the log holds no redaction marker at all, so the line was not redacted — it was lost: %q", string(body))
	}

	// And the copy that left the machine. The frames are redacted at emission,
	// before sequencing and fan-out (§16), so this cannot differ from the file —
	// which is exactly the property worth pinning, because the alternative
	// design, redacting per sink, would leak on whichever sink was added last.
	waitFor(t, "the control plane to receive the run's output", 60*time.Second, func() bool {
		return len(cp.Seqs(runID)) > 0
	})
	for _, u := range cp.Updates() {
		if strings.Contains(u.Update.ErrorMsg, token) {
			t.Error("the machine token reached the control plane in an error message")
		}
	}
	if strings.Contains(cpFramesText(cp, runID), token) {
		t.Error("the machine token reached the control plane in a log frame")
	}
}

// cpFramesText joins everything the control plane holds for one run.
func cpFramesText(cp *controlPlane, runID string) string {
	cp.mu.Lock()
	defer cp.mu.Unlock()
	var b strings.Builder
	for _, f := range cp.frames[runID] {
		b.WriteString(f.Line)
		b.WriteByte('\n')
	}
	return b.String()
}

// TestLoadMachineToken registers with whatever redactor it is handed, and does
// not turn a missing or unusable token file into a startup failure. A machine
// with no control plane is a fully supported state (§7.3), and one whose token
// file is broken has a problem the remote transport reports far better than this
// could.
func TestLoadMachineToken(t *testing.T) {
	tests := map[string]struct {
		token       string
		write       bool
		mode        os.FileMode
		remote      bool
		wantHeld    bool
		wantRedacts bool
	}{
		"a usable token is registered": {
			token: "tok-abc123", write: true, mode: 0o600, remote: true,
			wantHeld: true, wantRedacts: true,
		},
		"no token file at all": {
			remote: false,
		},
		"a token below the redaction floor is refused rather than registered": {
			// §16: values shorter than six characters are not redacted, because
			// matching them turns every occurrence of "dev" into a marker. The
			// operator is warned by name; the value is not registered, and
			// pretending otherwise would be worse than saying so.
			token: "ab", write: true, mode: 0o600, remote: true,
		},
		"a group-readable token is not read": {
			// remote.ReadToken refuses it, and it refuses it for a better reason
			// than redaction: a token readable by other users is a machine
			// somebody else can build on. The transport says so loudly.
			token: "tok-abc123", write: true, mode: 0o644, remote: true,
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "agent.key")
			if tc.write {
				if err := os.WriteFile(path, []byte(tc.token+"\n"), tc.mode); err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(path, tc.mode); err != nil {
					t.Fatal(err)
				}
			}

			a := &Agent{cfg: testTokenConfig(path, tc.remote), log: testLogger(t)}
			red := logstream.NewRedactor()
			a.loadMachineToken(red)

			if held := len(a.machineToken) > 0; held != tc.wantHeld {
				t.Errorf("machineToken held = %v, want %v", held, tc.wantHeld)
			}
			if got := len(a.alwaysRedact()); (got > 0) != tc.wantHeld {
				t.Errorf("alwaysRedact returned %d values, want held = %v", got, tc.wantHeld)
			}
			if tc.token == "" {
				return
			}
			line := "token=" + tc.token
			redacted := red.Redact(line) != line
			if redacted != tc.wantRedacts {
				t.Errorf("Redact(%q) changed it = %v, want %v", line, redacted, tc.wantRedacts)
			}
		})
	}
}

// testTokenConfig is the two fields [Agent.loadMachineToken] reads.
func testTokenConfig(tokenFile string, remote bool) *config.Config {
	cfg := &config.Config{}
	cfg.Paths.TokenFile = tokenFile
	cfg.Broker.Remote.Enabled = remote
	return cfg
}

func readTokenFile(t *testing.T, path string) string {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the fixture's machine token: %v", err)
	}
	return strings.TrimSpace(string(body))
}

// TestAlwaysRedactReachesEveryRun pins the wiring rather than the redactor: the
// orchestrator is built with the token, so a run's own redactor holds it before
// the first step (INV-4's ordering).
func TestAlwaysRedactReachesEveryRun(t *testing.T) {
	cp := newControlPlane(t)
	cfg := newBothTransportsConfig(t, cp)
	ta := newTestAgent(t, cfg, nil)
	ta.run(t)

	if len(ta.alwaysRedact()) != 1 {
		t.Fatalf("the agent is passing %d always-redacted values to the orchestrator, want the machine token",
			len(ta.alwaysRedact()))
	}
	// The store's redactor got it too: error_message, cancel_reason and
	// inputs_json are scrubbed on the way into sqlite (§22, INV-4), and a token
	// echoed into a failing step's error message would otherwise be persisted.
	token := readTokenFile(t, cfg.Paths.TokenFile)
	if err := ta.db.SetCancelReason(context.Background(), seedRun(t, ta.db, "r_red", protocol.StateQueued).ID,
		"stopped while using "+token); err != nil {
		t.Fatal(err)
	}
	row, err := ta.db.Get(context.Background(), "r_red")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(row.CancelReason, token) {
		t.Errorf("the machine token was persisted verbatim in cancel_reason: %q", row.CancelReason)
	}
}
