package remote

import (
	"testing"
	"time"

	"github.com/Abyssinia-Labs/latem/internal/store"
	"github.com/Abyssinia-Labs/latem/pkg/protocol"
)

// This file is the M6 acceptance criterion, demonstrated rather than asserted:
//
//	"the agent survives a 10-minute network partition mid-run, reports the
//	 correct terminal state on reconnect, no duplicated and no missing log
//	 frames, and continues serving the local broker with a revoked token."
//
// # How the ten minutes are simulated, and what that does not reproduce
//
// The reconnect loop has exactly one wait in it — the backoff — and that wait is
// the only thing in this package that consults a clock. [fakeClock] records what
// it was asked to wait for and returns immediately, so the loop makes its real
// dials, against a real server that is really refusing them, on the real
// schedule, in about two milliseconds. Every test below asserts the *simulated*
// elapsed time reached ten minutes, so "a ten-minute partition" is a measured
// claim and not a figure of speech.
//
// What this faithfully reproduces: the schedule and its cap, the number of
// attempts, the resume frame's contents at the moment the link returns, the
// state of the log buffer after however many writes happened while the link was
// down, and the acknowledgement bookkeeping across the reconnect.
//
// What it does not: real TCP and TLS timeouts (the dial fails at the HTTP layer
// here, in microseconds, where a real partition often fails at connect and takes
// the full handshake timeout); sockets lingering in FIN_WAIT on the far side;
// and the sheer volume of ten real minutes of a Gradle build's output. The last
// of those is the one that matters, and it is covered on its own terms by
// TestSink_BufferBoundDropsAndSaysSo rather than by wall clock.

// TestPartition_TenMinutesMidRunLosesNoLogFrames is finding F6 made concrete.
//
// It is the test this workstream exists for. A run streams output, the control
// plane vanishes for ten simulated minutes mid-stream, the run keeps producing
// output throughout, and when the link returns the control plane must hold
// exactly sequences 1..N — no gap, no repeat, in order.
//
// The three cases differ only in where the partition lands relative to the
// acknowledgement, and that difference is the whole design:
//
//   - Between batches, everything sent was acknowledged, so the reconnect has
//     nothing to re-offer and the control plane sees no duplicate at all.
//   - With a batch in flight that the server did receive, the agent cannot know
//     that, so it offers it again from last_acked_seq+1 and the server's §7.2
//     dedupe settles it. That is a duplicate on the wire and not a duplicate in
//     the log, which is exactly the trade F6 describes.
//   - With a batch in flight that the server never received, re-offering is the
//     only thing standing between the operator and a hole in the log exactly
//     where the network dropped.
//
// The three fail in different directions, which is why all three are here. A
// broker that re-sent its whole buffer fails the first. One that trusted "sent"
// for "acknowledged" fails the third, with a hole where the outage was. One
// that never re-offered anything passes the first two and fails the third.
func TestPartition_TenMinutesMidRunLosesNoLogFrames(t *testing.T) {
	tests := map[string]struct {
		// withholdAcks: the server receives the in-flight batch and cannot
		// answer. swallow: it never receives it at all. Those are the two halves
		// of a partition landing on a write, and they fail in opposite
		// directions — one risks duplicates, the other risks a hole.
		withholdAcks bool
		swallow      bool
		// wantDuplicates is what §7.2's dedupe rule should have to discard.
		wantDuplicates func(int) bool
		why            string
	}{
		"partition between batches": {
			wantDuplicates: func(n int) bool { return n == 0 },
			why:            "every frame sent before the partition was acknowledged, so nothing needed re-offering",
		},
		"partition with a batch in flight that arrived": {
			withholdAcks: true,
			// Bounded, not zero: the agent re-offers only from last_acked_seq+1,
			// so the repeat is the withheld window and never the whole run.
			wantDuplicates: func(n int) bool { return n > 0 && n <= 60 },
			why:            "the acknowledgement never arrived, so the agent had to assume the batch did not either",
		},
		"partition with a batch in flight that never arrived": {
			swallow:        true,
			wantDuplicates: func(n int) bool { return n == 0 },
			why:            "the control plane never saw those frames, so re-offering them cannot duplicate anything",
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			const runID = "r_partition"
			const total = uint64(150)

			h := newHarness(t, nil)
			seedRun(t, h.DB, runID, protocol.StateRunning)
			h.AwaitConnected(t)
			h.AwaitResumes(t, 1)

			sink, err := h.Logs(t.Context(), runID)
			if err != nil {
				t.Fatalf("Logs: %v", err)
			}

			// Before the partition: 1..40, acknowledged.
			writeFrames(t, sink, runID, 1, 40, "before")
			h.CP.Await(t, "the first 40 frames to be acknowledged", func() bool { return sink.Acked() >= 40 })

			// 41..60 are written into an in-flight state, so the partition lands
			// on an unanswered batch.
			h.CP.WithholdLogAcks(tc.withholdAcks)
			h.CP.SwallowLogs(tc.swallow)
			writeFrames(t, sink, runID, 41, 60, "in-flight")
			switch {
			case tc.withholdAcks:
				h.CP.Await(t, "the in-flight batch to reach the control plane", func() bool {
					return len(h.CP.Frames(runID)) >= 60
				})
			case tc.swallow:
				// There is nothing to observe on the server side by design: the
				// batch is being thrown away. Waiting for the client to have
				// written it is the closest honest signal.
				h.CP.Await(t, "the in-flight batch to be written", func() bool {
					s := h.sinkFor(runID)
					if s == nil {
						return false
					}
					s.mu.Lock()
					defer s.mu.Unlock()
					return s.sent >= 60
				})
			default:
				h.CP.Await(t, "60 frames acknowledged", func() bool { return sink.Acked() >= 60 })
			}

			// The partition. The build keeps building.
			connectsBefore := h.CP.Handshakes()
			h.CP.Partition()
			h.CP.Await(t, "the broker to notice the partition", func() bool { return !h.Connected() })

			writeFrames(t, sink, runID, 61, 120, "during the outage")

			// Ten simulated minutes of backoff, on the real §7.4 schedule.
			h.CP.Await(t, "ten simulated minutes of backoff", func() bool {
				return h.Clock.Total() >= 10*time.Minute
			})
			if got := h.Clock.Total(); got < 10*time.Minute {
				t.Fatalf("simulated partition was %s, want at least 10m", got)
			}
			if h.CP.Refusals() < 5 {
				t.Errorf("the control plane refused only %d dials during the partition; the loop should have kept trying",
					h.CP.Refusals())
			}

			// The link returns.
			h.CP.WithholdLogAcks(false)
			h.CP.SwallowLogs(false)
			h.CP.Heal()
			h.CP.Await(t, "the broker to reconnect", func() bool {
				return h.Connected() && h.CP.Handshakes() > connectsBefore
			})

			writeFrames(t, sink, runID, 121, total, "after")
			h.CP.Await(t, "every frame to be acknowledged", func() bool { return sink.Acked() >= total })

			got := h.CP.Frames(runID)
			assertContiguous(t, got, seqRange(1, total), runID)

			if dupes := h.CP.Duplicates(runID); !tc.wantDuplicates(dupes) {
				t.Errorf("the control plane discarded %d duplicate frames; %s", dupes, tc.why)
			}

			// The resume frame is what made the reconnect non-duplicating. It
			// must have named the run and the point the far side had reached.
			resumes := h.CP.Resumes()
			if len(resumes) < 2 {
				t.Fatalf("the agent sent %d resume frames; a reconnect must send one", len(resumes))
			}
			last := resumes[len(resumes)-1]
			found := false
			for _, r := range last.Runs {
				if r.RunID != runID {
					continue
				}
				found = true
				if r.State != protocol.StateRunning {
					t.Errorf("resume says run %s is %s, want %s", runID, r.State, protocol.StateRunning)
				}
				if r.LastAckedSeq < 40 {
					t.Errorf("resume claims last_acked_seq %d; the control plane had already taken 40 frames, so ingestion would restart before the frames it holds",
						r.LastAckedSeq)
				}
				if r.LastAckedSeq > 60 {
					t.Errorf("resume claims last_acked_seq %d, which is past anything the control plane acknowledged; the frames between would be lost silently",
						r.LastAckedSeq)
				}
			}
			if !found {
				t.Errorf("the resume frame did not mention run %s; the server has nothing to replay from (RUNNER_SPEC §7.4)", runID)
			}
		})
	}
}

// TestPartition_TerminalStateIsReportedOnReconnect is the second M6 clause.
//
// A run that finishes while the control plane is unreachable has a terminal
// state that nothing can deliver at the time. §22's reported_state cannot be
// relied on to replay it — one column, shared with the local broker, which
// marks it the moment the transition is persisted — so this transport keeps its
// own memory of what it could not send. This test is the reason that memory
// exists.
func TestPartition_TerminalStateIsReportedOnReconnect(t *testing.T) {
	const runID = "r_finishes_alone"

	h := newHarness(t, nil)
	seedRun(t, h.DB, runID, protocol.StateRunning)
	h.AwaitConnected(t)
	h.AwaitResumes(t, 1)

	connectsBefore := h.CP.Handshakes()
	h.CP.Partition()
	h.CP.Await(t, "the broker to notice the partition", func() bool { return !h.Connected() })

	// The build finishes during the outage. Report fails — it must, there is
	// nowhere to send it — and the transition is remembered.
	finished := time.Now().UTC()
	exit := 0
	err := h.Report(t.Context(), runID, protocol.Update{
		State:      protocol.StateSucceeded,
		ExitCode:   &exit,
		FinishedAt: &finished,
	})
	if err == nil {
		t.Fatal("Report during a partition returned nil; the caller would believe the control plane had been told")
	}
	if h.pendingCount() != 1 {
		t.Fatalf("the transport remembers %d undelivered transitions, want 1", h.pendingCount())
	}

	h.CP.Await(t, "ten simulated minutes of backoff", func() bool { return h.Clock.Total() >= 10*time.Minute })

	h.CP.Heal()
	h.CP.Await(t, "the reconnect to replay the terminal state", func() bool {
		if h.CP.Handshakes() <= connectsBefore {
			return false
		}
		for _, u := range h.CP.Updates() {
			if u.RunID == runID && u.Update.State == protocol.StateSucceeded {
				return true
			}
		}
		return false
	})

	if h.pendingCount() != 0 {
		t.Errorf("the transport still remembers %d transitions after a successful replay; they would be sent again on every reconnect for the life of the process",
			h.pendingCount())
	}

	// The durable half. In-memory pending covers a reconnect; only broker_state
	// covers a restart, and the replay path has to record its own deliveries
	// there or the backstop never empties and every future reconnect replays the
	// machine's whole history (amendment A28).
	h.CP.Await(t, "the run to leave the remote transport's replay set", func() bool {
		rows, err := h.DB.Unreported(t.Context(), store.BrokerRemote)
		return err == nil && len(rows) == 0
	})
}

// TestPartition_BackoffFollowsTheSpecSchedule pins §7.4's numbers.
//
// The schedule is 1s → 2s → 4s … capped at 60s. Getting the cap wrong is the
// expensive mistake: an agent that kept doubling would be silent for hours after
// a five-minute outage, and the machine would look dead to everyone including
// its owner.
func TestPartition_BackoffFollowsTheSpecSchedule(t *testing.T) {
	h := newHarness(t, nil)
	h.AwaitConnected(t)

	h.CP.Partition()
	h.CP.Await(t, "ten simulated minutes of backoff", func() bool { return h.Clock.Total() >= 10*time.Minute })

	got := h.Clock.Schedule()
	// The first entry is the delay after the session that was cut off, which
	// starts the schedule again from the base.
	want := []time.Duration{
		time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second,
		16 * time.Second, 32 * time.Second, 60 * time.Second, 60 * time.Second,
	}
	if len(got) < len(want) {
		t.Fatalf("the loop backed off %d times, want at least %d", len(got), len(want))
	}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("backoff %d was %s, want %s (schedule so far: %v)", i, got[i], w, got[:i+1])
		}
	}
	for i, d := range got {
		if d > 60*time.Second {
			t.Fatalf("backoff %d was %s, past the 60s cap RUNNER_SPEC §7.4 sets", i, d)
		}
	}
}

// TestPartition_ResumePointIgnoresTheOtherTransport.
//
// This test was written when §22 gave the whole machine one `last_acked_seq`
// column. internal/broker/local advances its resume point to the highest
// sequence it has *persisted* the moment a batch is written — the honest local
// meaning of "durably accepted" — and this transport cannot say that: the
// control plane may be several batches behind, or unreachable. While the two
// meanings shared a column, reading it here told the server "you already have
// everything up to N" for an N it had never seen, and ingestion resumed past
// the frames covering the outage. The lost region is exactly the interesting
// part of the build.
//
// Amendment A28 replaced the column with `broker_state`, keyed by transport, and
// the assertion changes with it. The old version proved the defect was worked
// around — the sink was preferred over a column that was wrong. This one proves
// the defect is gone: the local transport races as far ahead as it likes, under
// its own key, and it is not merely ignored here, it is invisible. Both the live
// sink and the store answer 10.
//
// It is deliberately still a partition test rather than a store unit test. The
// store's own key separation is covered by TestLastAckedSeq_IsPerTransport; what
// this pins is that the number leaving the machine on a reconnect — the one the
// server resumes ingestion from — is this transport's.
func TestPartition_ResumePointIgnoresTheOtherTransport(t *testing.T) {
	const runID = "r_two_transports"

	h := newHarness(t, nil)
	seedRun(t, h.DB, runID, protocol.StateRunning)
	h.AwaitConnected(t)
	h.AwaitResumes(t, 1)

	sink, err := h.Logs(t.Context(), runID)
	if err != nil {
		t.Fatalf("Logs: %v", err)
	}
	writeFrames(t, sink, runID, 1, 10, "acknowledged")
	h.CP.Await(t, "ten frames acknowledged", func() bool { return sink.Acked() >= 10 })

	// The local broker on the same machine races ahead: it has written 5000
	// frames to disk and that is genuinely durable, for it. The write is
	// monotonic, so if the two transports shared a row this could not be undone.
	if err := h.DB.SetLastAckedSeq(t.Context(), store.BrokerLocal, runID, 5000); err != nil {
		t.Fatalf("SetLastAckedSeq(local): %v", err)
	}

	h.CP.Partition()
	h.CP.Await(t, "the partition", func() bool { return !h.Connected() })
	h.CP.Heal()
	h.AwaitResumes(t, 2)

	assertResumeSeq(t, h, runID, 10)

	// The persisted half, which is what a restart would read. The live sink is
	// preferred over it, so a resume frame can be right while the row is wrong —
	// which is precisely the state this used to be in.
	stored, err := h.DB.LastAckedSeq(t.Context(), store.BrokerRemote, runID)
	if err != nil {
		t.Fatalf("LastAckedSeq(remote): %v", err)
	}
	if stored != 10 {
		t.Errorf("broker_state holds last_acked_seq %d for the remote transport, want 10; an agent restarting here would resume past %d frames the control plane never received",
			stored, stored-10)
	}
}

// TestPartition_RestartResumesFromTheStoreNotFromZero is the other half of
// finding F6, and the half only the store can answer.
//
// A live sink covers a reconnect. Nothing in this process covers a restart: the
// buffer is gone, the sink is gone, and the only thing left is the row. If that
// row were read under the wrong key — or not read at all — the agent would tell
// the control plane to resume from 1 and re-send a build's entire log, or from
// the local transport's position and skip most of it.
func TestPartition_RestartResumesFromTheStoreNotFromZero(t *testing.T) {
	const runID = "r_restarted"

	first := newHarness(t, nil)
	seedRun(t, first.DB, runID, protocol.StateRunning)
	first.AwaitConnected(t)
	first.AwaitResumes(t, 1)

	sink, err := first.Logs(t.Context(), runID)
	if err != nil {
		t.Fatalf("Logs: %v", err)
	}
	writeFrames(t, sink, runID, 1, 25, "before the restart")
	first.CP.Await(t, "the frames to be acknowledged", func() bool { return sink.Acked() >= 25 })
	first.CP.Await(t, "the resume point to be persisted", func() bool {
		seq, serr := first.DB.LastAckedSeq(t.Context(), store.BrokerRemote, runID)
		return serr == nil && seq >= 25
	})

	// The local transport is streaming the same run to the same disk and is
	// arbitrarily far ahead. A restart must not pick that number up.
	if err := first.DB.SetLastAckedSeq(t.Context(), store.BrokerLocal, runID, 9000); err != nil {
		t.Fatalf("SetLastAckedSeq(local): %v", err)
	}

	// The agent restarts: a new broker, the same database, no memory at all.
	second := newHarnessOn(t, first.DB, nil)
	second.AwaitConnected(t)
	second.AwaitResumes(t, 1)

	assertResumeSeq(t, second, runID, 25)

	// And a sink opened after the restart starts there too, rather than
	// re-offering the whole run.
	resumed, err := second.Logs(t.Context(), runID)
	if err != nil {
		t.Fatalf("Logs after restart: %v", err)
	}
	if got := resumed.Acked(); got != 25 {
		t.Errorf("a sink opened after a restart reports Acked %d, want 25", got)
	}
}

// assertResumeSeq checks the last resume frame's position for one run.
func assertResumeSeq(t *testing.T, h *harness, runID string, want uint64) {
	t.Helper()

	resumes := h.CP.Resumes()
	if len(resumes) == 0 {
		t.Fatal("the agent sent no resume frame")
	}
	last := resumes[len(resumes)-1]
	for _, r := range last.Runs {
		if r.RunID != runID {
			continue
		}
		if r.LastAckedSeq != want {
			t.Errorf("resume reported last_acked_seq %d for %s, want %d — what this transport's far side actually took",
				r.LastAckedSeq, runID, want)
		}
		return
	}
	t.Fatalf("the resume frame did not mention run %s", runID)
}
