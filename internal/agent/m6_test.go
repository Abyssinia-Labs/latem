package agent

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Abyssinia-Labs/latem/internal/config"
	"github.com/Abyssinia-Labs/latem/internal/store"
	"github.com/Abyssinia-Labs/latem/pkg/protocol"
)

// This file is the M6 acceptance criterion with both transports live, which is
// the combination that could not be tested before amendment A28.
//
// internal/broker/remote already proves the partition behaviour of that
// transport in isolation, on a simulated ten-minute schedule. What it cannot
// prove on its own is the thing that actually shipped broken: that a local
// broker running beside it — writing its own reported_state on every transition
// and its own last_acked_seq on every batch — does not corrupt the remote
// transport's idea of what the control plane holds. While those were one column
// each, a fan-out would have advanced the remote resume point to everything the
// machine had emitted and lost precisely the frames covering the outage.
//
// The partition here is real rather than simulated: a real reconnect loop, a
// real HTTP refusal, and the real §7.4 backoff, so the test costs a second or
// two of wall clock. That is the price of asserting it end to end, and this is
// the one place worth paying it.

// newBothTransportsConfig is a runnable repo plus a live control plane.
func newBothTransportsConfig(t *testing.T, cp *controlPlane) *config.Config {
	t.Helper()
	if !hasGit(t) {
		t.Skip("git is not installed; the registry is read from a real checkout (INV-5)")
	}
	repo := shortTempDir(t)
	newRepo(t, repo, testJobs)
	return newConfigDir(t, configOptions{repoURL: repo, remoteURL: cp.URL()})
}

// emitLines writes n numbered lines, flushing through the pipeline as it goes.
func emitLines(w io.Writer, tag string, n int) {
	for i := 1; i <= n; i++ {
		_, _ = io.WriteString(w, fmt.Sprintf("%s line %d\n", tag, i))
	}
}

// TestM6_PartitionMidRunWithBothTransportsLive is the criterion, whole.
//
//	"a run executes, the remote link partitions mid-run, and on reconnect the
//	 control plane holds exactly log sequences 1..N — no gaps, no repeats —
//	 while the local transport is entirely unaffected."
//
// The three claims are asserted separately because they fail separately:
//
//   - Contiguity from 1 to N. A broker that trusted "sent" for "acknowledged"
//     leaves a hole exactly where the network dropped, which is the region
//     anybody debugging the outage wants.
//   - The local transport keeps working throughout, and its own resume point
//     runs ahead of the remote one *during* the outage. That divergence is the
//     state §22's single column could not represent, and asserting it is what
//     makes this a regression test for the defect rather than for the symptom.
//   - The run reaches its terminal state locally and the control plane is told
//     about it, on the reconnect, from this transport's own replay.
func TestM6_PartitionMidRunWithBothTransportsLive(t *testing.T) {
	cp := newControlPlane(t)
	cfg := newBothTransportsConfig(t, cp)
	ta := newTestAgent(t, cfg, nil)

	const runID = "r_m6"
	var (
		started = newGate()
		midway  = newGate()
		gate1   = newGate()
		gate2   = newGate()
	)
	ta.exec.emit = func(w io.Writer) {
		emitLines(w, "before the outage", 40)
		started.open()
		gate1.wait()
		emitLines(w, "during the outage", 60)
		midway.open()
		gate2.wait()
		emitLines(w, "after the outage", 30)
	}

	ta.run(t)
	// Registered after the agent's own cleanup so it runs first (LIFO): a
	// failing assertion must not leave the step blocked while shutdown waits out
	// its grace on a run nothing is going to release.
	t.Cleanup(func() {
		gate1.open()
		gate2.open()
	})
	db := openStore(t, cfg)

	waitFor(t, "the agent to connect to the control plane", 10*time.Second, func() bool {
		return ta.remoteBrk != nil && ta.remoteBrk.Connected()
	})

	enqueueDirectly(t, cfg, runID, "main")
	waitFor(t, "the build to start producing output", 60*time.Second, started.reached)

	// The control plane is genuinely receiving frames before the outage. Without
	// this the test could pass with a transport that never sent anything until
	// the very end, which would prove nothing about resumption.
	waitFor(t, "the control plane to be taking log frames", 30*time.Second, func() bool {
		return len(cp.Seqs(runID)) >= 20
	})

	// The partition.
	handshakesBefore := cp.Handshakes()
	cp.Partition()
	waitFor(t, "the broker to notice the partition", 30*time.Second, func() bool {
		return !ta.remoteBrk.Connected()
	})
	remoteAtPartition := mustAcked(t, db, store.BrokerRemote, runID)

	// The build keeps building, and the local transport keeps taking its output.
	gate1.open()
	waitFor(t, "the build to produce output during the outage", 60*time.Second, midway.reached)
	waitFor(t, "the local transport to keep acknowledging output during the outage", 30*time.Second, func() bool {
		return mustAcked(t, db, store.BrokerLocal, runID) > remoteAtPartition+20
	})

	// This is the divergence one shared column could not represent. The local
	// transport has taken tens of frames the control plane cannot possibly have
	// seen, and the remote transport's resume point has not moved — because
	// there is nothing on the far side to move it.
	localDuring := mustAcked(t, db, store.BrokerLocal, runID)
	remoteDuring := mustAcked(t, db, store.BrokerRemote, runID)
	if remoteDuring != remoteAtPartition {
		t.Fatalf("the remote transport's resume point moved from %d to %d while the control plane was unreachable; on reconnect ingestion would restart past frames it never received",
			remoteAtPartition, remoteDuring)
	}
	if remoteDuring >= localDuring {
		t.Fatalf("during the outage the remote transport claims %d acknowledged and the local one %d; the two are sharing a resume point and %d frames would be skipped on reconnect",
			remoteDuring, localDuring, localDuring-remoteDuring)
	}

	// The outage lasts long enough for the reconnect loop to have tried and been
	// refused. Without this the test could heal inside the first backoff step and
	// assert resumption across a partition the agent never noticed twice.
	waitFor(t, "the reconnect loop to be refused at least twice", 60*time.Second, func() bool {
		return cp.Refusals() >= 2
	})

	// The link returns.
	cp.Heal()
	waitFor(t, "the broker to reconnect", 60*time.Second, func() bool {
		return ta.remoteBrk.Connected() && cp.Handshakes() > handshakesBefore
	})
	gate2.open()

	run := waitState(t, cfg, runID, 120*time.Second, protocol.StateSucceeded, protocol.StateFailed)
	if run.State != protocol.StateSucceeded {
		t.Fatalf("run state = %s (%s: %s); a control plane outage must not fail a build",
			run.State, run.ErrorCode, run.ErrorMsg)
	}

	// N is what the machine actually emitted, which the local transport
	// acknowledged frame by frame as it was written to this disk.
	//
	// Waited for rather than sampled. A run reaching succeeded does not mean
	// every frame has been acknowledged: the sink batches at 100 frames or
	// 250 ms (§16), so the tail of a build is still in flight for a moment
	// after its last step exits. Reading the count once made this test fail
	// under -race and repetition — where the machine is slow enough for that
	// moment to matter — for a reason that had nothing to do with the
	// partition it exists to test.
	const emitted = 130
	waitFor(t, "the local transport to acknowledge the whole build", 60*time.Second, func() bool {
		return mustAcked(t, db, store.BrokerLocal, runID) >= emitted
	})
	total := mustAcked(t, db, store.BrokerLocal, runID)

	waitFor(t, "the control plane to catch up after the reconnect", 60*time.Second, func() bool {
		return uint64(len(cp.Seqs(runID))) >= total
	})

	assertContiguousFrom1(t, cp.Seqs(runID), total, runID)

	if dupes := cp.Duplicates(runID); uint64(dupes) > total/2 {
		t.Errorf("the control plane discarded %d duplicates against %d frames; the reconnect re-sent the buffer rather than resuming from last_acked_seq+1",
			dupes, total)
	}

	// The terminal state reached the control plane, and the two transports agree
	// on the run's outcome without having shared a row to say so.
	waitFor(t, "the terminal state to reach the control plane", 60*time.Second, func() bool {
		return cp.HasState(runID, protocol.StateSucceeded)
	})
	waitFor(t, "the remote transport's resume point to catch up", 60*time.Second, func() bool {
		return mustAcked(t, db, store.BrokerRemote, runID) >= total
	})

	// And the local path is whole: the file holds every line the build printed,
	// which is the copy an operator has when the hosted layer is gone for good.
	if lines := logLineCount(t, cfg, runID); uint64(lines) < total {
		t.Errorf("the local log file holds %d lines against %d frames emitted; the local transport lost output to a network outage",
			lines, total)
	}
}

// TestM6_AnUnreachableControlPlaneDoesNotFailARun is the other half of §7.4's
// promise, at the level a user experiences it.
//
// The control plane is a port nothing is listening on, so the transport fails at
// connect and keeps retrying for the whole test. A build has to run, succeed,
// and produce a complete local log anyway — and the local transport's state has
// to be untouched by the presence of a transport that never worked.
func TestM6_AnUnreachableControlPlaneDoesNotFailARun(t *testing.T) {
	if !hasGit(t) {
		t.Skip("git is not installed; the registry is read from a real checkout (INV-5)")
	}
	repo := shortTempDir(t)
	newRepo(t, repo, testJobs)
	cfg := newConfigDir(t, configOptions{repoURL: repo, remoteURL: "ws://127.0.0.1:1/agent"})

	ta := newTestAgent(t, cfg, nil)
	ta.exec.emit = func(w io.Writer) { emitLines(w, "unheard", 25) }
	ta.run(t)

	const runID = "r_unreachable"
	enqueueDirectly(t, cfg, runID, "main")

	run := waitState(t, cfg, runID, 120*time.Second, protocol.StateSucceeded, protocol.StateFailed, protocol.StateRejected)
	if run.State != protocol.StateSucceeded {
		t.Fatalf("run state = %s (%s: %s); an unreachable control plane must cost a build nothing",
			run.State, run.ErrorCode, run.ErrorMsg)
	}

	db := openStore(t, cfg)
	local := mustAcked(t, db, store.BrokerLocal, runID)
	if local < 25 {
		t.Errorf("the local transport acknowledged %d frames, want at least the 25 the build printed", local)
	}
	if remote := mustAcked(t, db, store.BrokerRemote, runID); remote != 0 {
		t.Errorf("the remote transport's resume point is %d for a control plane it never reached; on the next connection %d frames would be skipped",
			remote, remote)
	}

	// The run is still owed to the control plane, and the record of that is this
	// transport's own. A machine that came back online must send it.
	rows, err := db.Unreported(context.Background(), store.BrokerRemote)
	if err != nil {
		t.Fatal(err)
	}
	if !containsRun(rows, runID) {
		t.Errorf("the run is not in the remote transport's replay set after finishing during a total outage; the control plane would show it as queued forever")
	}
	if lines := logLineCount(t, cfg, runID); uint64(lines) < local {
		t.Errorf("the local log holds %d lines against %d frames", lines, local)
	}
}

// TestM6_ACancelFromTheControlPlaneStopsARun is the sharpest evidence that the
// remote command stream is read at all.
//
// A run command proves less than it looks like it does: [handleCommand] makes
// the run durable in sqlite *before* delivering it, so the runner pool's
// store.ClaimNext would execute it even if nothing ever read the channel — the
// doorbell is not the queue, and that is true of both transports. A cancel is
// durable nowhere. No table remembers it, nothing replays it, and the only thing
// that can act on it is the goroutine holding the run's context. If the agent is
// not consuming this transport's commands, this run builds to completion and the
// Stop button on somebody's phone does nothing at all.
func TestM6_ACancelFromTheControlPlaneStopsARun(t *testing.T) {
	cp := newControlPlane(t)
	cfg := newBothTransportsConfig(t, cp)
	ta := newTestAgent(t, cfg, nil)

	building := newGate()
	release := newGate()
	ta.exec.emit = func(w io.Writer) {
		emitLines(w, "building", 5)
		building.open()
		release.wait()
	}
	ta.run(t)
	t.Cleanup(release.open)

	waitFor(t, "the agent to connect to the control plane", 10*time.Second, func() bool {
		return ta.remoteBrk != nil && ta.remoteBrk.Connected()
	})

	const runID = "r_stop_button"
	enqueueDirectly(t, cfg, runID, "main")
	waitFor(t, "the build to be running", 120*time.Second, building.reached)

	cp.push(t, protocol.Command{
		Kind:   protocol.CommandCancel,
		Cancel: &protocol.CancelRequest{ID: runID, Actor: "web:someone", Reason: "wrong branch"},
	})

	run := waitState(t, cfg, runID, 120*time.Second,
		protocol.StateCancelled, protocol.StateSucceeded, protocol.StateFailed)
	if run.State != protocol.StateCancelled {
		t.Fatalf("run state = %s after a cancel from the control plane; the remote command stream is not being read, so a Stop button on a phone does nothing",
			run.State)
	}
	waitFor(t, "the cancellation to be reported back", 60*time.Second, func() bool {
		return cp.HasState(runID, protocol.StateCancelled)
	})
}

// TestM6_CommandsAreConsumedFromTheRemoteTransport.
//
// Reading the remote command stream is the half of this change that commits the
// agent to reporting back, and the two shipped together for that reason. This
// asserts the second half for a run the control plane started: it is executed
// and it is reported, and the transport's own replay set is empty afterwards —
// which is what stops the machine re-sending that run's whole history on every
// reconnect for the rest of its life.
func TestM6_CommandsAreConsumedFromTheRemoteTransport(t *testing.T) {
	cp := newControlPlane(t)
	cfg := newBothTransportsConfig(t, cp)
	ta := newTestAgent(t, cfg, nil)
	ta.run(t)

	waitFor(t, "the agent to connect to the control plane", 10*time.Second, func() bool {
		return ta.remoteBrk != nil && ta.remoteBrk.Connected()
	})

	const runID = "r_from_cp"
	cp.push(t, protocol.Command{
		Kind: protocol.CommandRun,
		Run: &protocol.RunRequest{
			ID:     runID,
			Repo:   "app",
			Job:    "test",
			Ref:    "main",
			Origin: protocol.Origin{Kind: "webhook", Actor: "github:octocat"},
		},
	})

	run := waitState(t, cfg, runID, 120*time.Second, protocol.StateSucceeded, protocol.StateFailed, protocol.StateRejected)
	if run.State != protocol.StateSucceeded {
		t.Fatalf("run state = %s (%s: %s); a run accepted from the control plane must actually build",
			run.State, run.ErrorCode, run.ErrorMsg)
	}

	waitFor(t, "the outcome to be reported to the control plane", 60*time.Second, func() bool {
		return cp.HasState(runID, protocol.StateSucceeded)
	})
	if !cp.HasState(runID, protocol.StateRunning) {
		t.Error("the control plane never saw the run start; it would show as queued until it finished")
	}
	// Waited for, not sampled. The terminal state arriving does not mean the
	// log frames have: the sink batches at 100 frames or 250 ms (§16) and the
	// tail of a build flushes after the transition that ended it. Reading the
	// count here made this fail roughly one run in four under -race, for a
	// reason that had nothing to do with the remote command stream it tests.
	waitFor(t, "the control plane to receive the run's log frames", 60*time.Second, func() bool {
		return len(cp.Seqs(runID)) > 0
	})

	// A delivered transition has to be recorded under this transport's key, or
	// the run never leaves store.Unreported(remote) and every reconnect replays
	// it again — for the life of the machine, growing with every build.
	db := openStore(t, cfg)
	waitFor(t, "the run to leave the remote transport's replay set", 60*time.Second, func() bool {
		rows, err := db.Unreported(context.Background(), store.BrokerRemote)
		return err == nil && !containsRun(rows, runID)
	})
}

// gate is a one-shot signal a test and a running build share.
//
// It is a type rather than a bare channel because both sides of every gate here
// are reachable from a failing assertion, and a double close or a step left
// blocked on a channel nobody will close turns one failed assertion into a
// two-minute shutdown timeout that hides it.
type gate struct {
	ch   chan struct{}
	once sync.Once
}

func newGate() *gate { return &gate{ch: make(chan struct{})} }

func (g *gate) open() { g.once.Do(func() { close(g.ch) }) }

func (g *gate) wait() { <-g.ch }

func (g *gate) reached() bool {
	select {
	case <-g.ch:
		return true
	default:
		return false
	}
}

// mustAcked reads one transport's resume point, treating an unknown run as 0.
func mustAcked(t *testing.T, db *store.DB, brk, runID string) uint64 {
	t.Helper()
	seq, err := db.LastAckedSeq(context.Background(), brk, runID)
	if err != nil {
		t.Fatalf("LastAckedSeq(%s, %s): %v", brk, runID, err)
	}
	return seq
}

func containsRun(rows []store.Run, id string) bool {
	for _, r := range rows {
		if r.ID == id {
			return true
		}
	}
	return false
}

// assertContiguousFrom1 is the no-gaps, no-repeats claim. It is spelled as the
// set 1..total rather than as a length check because a length check passes for a
// stream that lost frame 57 and delivered 1000 twice.
func assertContiguousFrom1(t *testing.T, seqs []uint64, total uint64, runID string) {
	t.Helper()

	have := make(map[uint64]bool, len(seqs))
	for _, s := range seqs {
		if have[s] {
			t.Fatalf("the control plane holds sequence %d twice for run %s; the dedupe key is (run_id, seq) and this is the same log line stored under it twice",
				s, runID)
		}
		have[s] = true
	}

	var missing []uint64
	for s := uint64(1); s <= total; s++ {
		if !have[s] {
			missing = append(missing, s)
		}
	}
	if len(missing) > 0 {
		shown := missing
		if len(shown) > 12 {
			shown = shown[:12]
		}
		t.Fatalf("the control plane is missing %d of sequences 1..%d for run %s (first missing: %v); those are the lines covering the outage, and they are gone from the hosted view of the build",
			len(missing), total, runID, shown)
	}
}

// logLineCount reads ~/.latem/logs/<run-id>.log, which is the copy that exists
// whether or not anything else does (§16).
func logLineCount(t *testing.T, cfg *config.Config, runID string) int {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(cfg.Paths.Logs, runID+".log"))
	if err != nil {
		t.Fatalf("reading the run's log file: %v", err)
	}
	return len(strings.Split(strings.TrimRight(string(data), "\n"), "\n"))
}
