package remote

import (
	"strings"
	"testing"
	"time"

	"github.com/Abyssinia-Labs/latem/internal/store"
	"github.com/Abyssinia-Labs/latem/pkg/protocol"
)

// TestEnqueueRun_MakesAnInboundRunDurableOrRefusesIt.
//
// The table is the list of ways a control plane can send something this machine
// must not simply accept. Two of them matter more than the rest: an id that is a
// path (INV-3, amendment A6, and this is the field every path is built from),
// and a replay of a run that already finished — which the resume handshake makes
// a routine event rather than an exotic one.
func TestEnqueueRun_MakesAnInboundRunDurableOrRefusesIt(t *testing.T) {
	tests := map[string]struct {
		req     protocol.RunRequest
		seed    protocol.RunState // zero means the run does not exist yet
		wantErr string
	}{
		"a well-formed run": {
			req: protocol.RunRequest{ID: "r_ok", Repo: "myapp", Job: "build", Ref: "main"},
		},
		"an id that traverses": {
			req:     protocol.RunRequest{ID: "../../../../tmp/x", Repo: "myapp", Job: "build", Ref: "main"},
			wantErr: "must match",
		},
		"no repo": {
			req:     protocol.RunRequest{ID: "r_norepo", Job: "build", Ref: "main"},
			wantErr: "no repo",
		},
		"no job": {
			req:     protocol.RunRequest{ID: "r_nojob", Repo: "myapp", Ref: "main"},
			wantErr: "no job",
		},
		"no ref": {
			req:     protocol.RunRequest{ID: "r_noref", Repo: "myapp", Job: "build"},
			wantErr: "no ref",
		},
		"a replay of a run still in flight is re-queued": {
			req:  protocol.RunRequest{ID: "r_live", Repo: "myapp", Job: "build", Ref: "main"},
			seed: protocol.StateQueued,
		},
		"a replay of a finished run is refused": {
			req:     protocol.RunRequest{ID: "r_done", Repo: "myapp", Job: "build", Ref: "main"},
			seed:    protocol.StateSucceeded,
			wantErr: "already finished",
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			db := newStore(t)
			if tc.seed != "" {
				seedRun(t, db, tc.req.ID, tc.seed)
			}

			err := enqueueRun(t.Context(), db, tc.req, 0, time.Now().Add(time.Hour))
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("enqueueRun accepted %s", name)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Errorf("error %q does not mention %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("enqueueRun: %v", err)
			}

			run, gerr := db.Get(t.Context(), tc.req.ID)
			if gerr != nil {
				t.Fatalf("the run was accepted but not persisted: %v", gerr)
			}
			if run.State != protocol.StateQueued {
				t.Errorf("state = %s, want %s", run.State, protocol.StateQueued)
			}
			if run.ExpiresAt == nil {
				t.Error("no expires_at was set; §11's sweep has nothing to act on")
			}
		})
	}
}

// TestEnqueueRun_DefaultsTheOriginKind.
//
// origin_kind is NOT NULL in the §22 schema, and Origin is audit-only and
// untrusted. Refusing a build because a display field was blank would be the
// wrong trade; "manual" is the honest description of a run a control plane sent
// without saying why. It differs from the local broker's default of "cli", and
// that difference is the whole difference between the two copies.
func TestEnqueueRun_DefaultsTheOriginKind(t *testing.T) {
	db := newStore(t)
	req := protocol.RunRequest{ID: "r_origin", Repo: "myapp", Job: "build", Ref: "main"}
	if err := enqueueRun(t.Context(), db, req, 0, time.Time{}); err != nil {
		t.Fatalf("enqueueRun: %v", err)
	}
	run, err := db.Get(t.Context(), "r_origin")
	if err != nil {
		t.Fatal(err)
	}
	if run.OriginKind != defaultOriginKind {
		t.Errorf("origin_kind = %q, want %q", run.OriginKind, defaultOriginKind)
	}
	if run.ExpiresAt != nil {
		t.Error("a zero expiry produced a deadline; a machine with no queue_ttl must not sweep its own queue")
	}
}

// TestPendingReports_KeepOrderAndCollapseRepeats.
//
// The memory exists because §22's reported_state is one column shared with the
// local broker, which marks it the moment a transition is persisted — before
// this transport has had any chance to send it. Order matters because a control
// plane that sees `collecting` after `succeeded` shows a finished build as still
// working.
func TestPendingReports_KeepOrderAndCollapseRepeats(t *testing.T) {
	b := &Broker{pending: map[string][]protocol.Update{}}

	b.rememberReport("r_1", protocol.Update{State: protocol.StatePreparing})
	b.rememberReport("r_1", protocol.Update{State: protocol.StateRunning})
	b.rememberReport("r_2", protocol.Update{State: protocol.StateQueued})
	if got := b.pendingCount(); got != 3 {
		t.Fatalf("pendingCount = %d, want 3", got)
	}

	// The same state twice is the same transition (§7.2 makes it idempotent),
	// so the second replaces the first rather than being replayed twice.
	b.rememberReport("r_1", protocol.Update{State: protocol.StateRunning, Runner: "linux-x64"})
	if got := b.pendingCount(); got != 3 {
		t.Errorf("pendingCount = %d after re-reporting one state, want 3", got)
	}

	// The cap holds, and drops the oldest: the newest state is what a control
	// plane most needs, and the history is whole in sqlite either way.
	for i := range maxPendingPerRun + 10 {
		b.rememberReport("r_3", protocol.Update{State: protocol.RunState("s" + itoa(uint64(i)))})
	}
	b.mu.Lock()
	got := len(b.pending["r_3"])
	last := b.pending["r_3"][got-1].State
	b.mu.Unlock()
	if got != maxPendingPerRun {
		t.Errorf("one run holds %d pending transitions, want the cap of %d", got, maxPendingPerRun)
	}
	if want := protocol.RunState("s" + itoa(uint64(maxPendingPerRun+9))); last != want {
		t.Errorf("the newest transition is %s, want %s — the cap dropped the wrong end", last, want)
	}

	// Acknowledging one removes exactly it, and the last one empties the run.
	b.forgetReport("r_1", protocol.StatePreparing)
	b.mu.Lock()
	remaining := len(b.pending["r_1"])
	b.mu.Unlock()
	if remaining != 1 {
		t.Errorf("r_1 has %d pending transitions after one was acknowledged, want 1", remaining)
	}
	b.forgetReport("r_1", protocol.StateRunning)
	b.mu.Lock()
	_, present := b.pending["r_1"]
	b.mu.Unlock()
	if present {
		t.Error("a run with nothing pending is still in the map; it would grow for the life of the process")
	}

	// takePending empties, so a replay that fails halfway can put back only what
	// it did not reach.
	taken := b.takePending()
	if len(taken) == 0 {
		t.Error("takePending returned nothing")
	}
	if b.pendingCount() != 0 {
		t.Errorf("pendingCount = %d after takePending", b.pendingCount())
	}

	// And a closed broker forgets rather than accumulating for a replay that
	// will never happen.
	b.closed = true
	b.rememberReport("r_4", protocol.Update{State: protocol.StateFailed})
	if b.pendingCount() != 0 {
		t.Error("a closed broker queued a transition for replay")
	}
}

// TestAckReason_NeverInventsWordsTheFarSideDidNotSay.
//
// The reason reaches an operator through a `latem` error and possibly a log
// line. Making one up when the control plane declined to give one would send
// somebody looking for a cause that does not exist.
func TestAckReason_NeverInventsWordsTheFarSideDidNotSay(t *testing.T) {
	tests := map[string]struct {
		ack  protocol.Ack
		want string
	}{
		"a message and a code": {
			protocol.Ack{Error: "run already claimed", Code: protocol.ErrNoRunner},
			"run already claimed (E_NO_RUNNER)",
		},
		"a message alone": {protocol.Ack{Error: "no"}, "no"},
		"a code alone":    {protocol.Ack{Code: protocol.ErrBadInput}, "E_BAD_INPUT"},
		"neither":         {protocol.Ack{}, "the control plane refused without saying why"},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			if got := ackReason(tc.ack); got != tc.want {
				t.Errorf("ackReason = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestDeliver_NeverBlocksTheReader.
//
// The command buffer is the bound. A full one means the agent is not draining
// commands, and waiting here would stall the reader — and with it every log
// acknowledgement for every other run on the machine.
func TestDeliver_NeverBlocksTheReader(t *testing.T) {
	b := &Broker{cmds: make(chan protocol.Command, 2)}
	cmd := protocol.NewCancelCommand(protocol.CancelRequest{ID: "r_1"})

	for i := range 2 {
		if !b.deliver(cmd) {
			t.Fatalf("deliver refused command %d with room in the buffer", i+1)
		}
	}

	done := make(chan bool, 1)
	go func() { done <- b.deliver(cmd) }()
	select {
	case ok := <-done:
		if ok {
			t.Error("deliver reported success with a full buffer")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("deliver blocked on a full buffer; the reader goroutine would be stuck")
	}
}

// TestRandomJitter_StaysInRange. It is one line, and it is the one line that
// decides whether every agent in a fleet reconnects at the same instant.
func TestRandomJitter_StaysInRange(t *testing.T) {
	for range 1000 {
		if v := randomJitter(); v < 0 || v >= 1 {
			t.Fatalf("randomJitter returned %v, outside [0,1)", v)
		}
	}
}

// TestStoreRun_FieldsTheReplayNeeds pins the shape [Broker.replayReports] reads
// out of store.Unreported. A field silently dropped there would present as a
// control plane showing a finished run with no exit code.
func TestStoreRun_FieldsTheReplayNeeds(t *testing.T) {
	db := newStore(t)
	exit := 2
	started := time.Now().Add(-time.Minute).UTC().Truncate(time.Second)
	finished := time.Now().UTC().Truncate(time.Second)

	err := db.Create(t.Context(), store.Run{
		ID: "r_replay", Repo: "myapp", Job: "build", Ref: "main",
		RefNormalized: "refs/heads/main", SHA: "3bf1261",
		Runner: "linux-x64", State: protocol.StateFailed,
		ExitCode: &exit, ErrorCode: protocol.ErrStep, ErrorMsg: "step 2 exited 2",
		OriginKind: "webhook", StartedAt: &started, FinishedAt: &finished,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	rows, err := db.Unreported(t.Context(), store.BrokerRemote)
	if err != nil {
		t.Fatalf("Unreported: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("Unreported returned %d rows, want 1", len(rows))
	}
	r := rows[0]
	switch {
	case r.ExitCode == nil || *r.ExitCode != 2:
		t.Error("exit code did not survive the round trip")
	case r.ErrorCode != protocol.ErrStep:
		t.Errorf("error code = %q", r.ErrorCode)
	case r.Runner != "linux-x64":
		t.Errorf("runner = %q", r.Runner)
	case r.SHA != "3bf1261" || r.RefNormalized != "refs/heads/main":
		t.Errorf("the INV-5 audit pair did not survive: %q %q", r.RefNormalized, r.SHA)
	case r.FinishedAt == nil:
		t.Error("finished_at did not survive")
	}
}

// A run sent again for its tokens after a runner has claimed it takes them and
// stays claimed: queued again, it would be a claimed run in the queue (A75).
func TestEnqueueRun_AReplayOfAClaimedRunOnlyBringsTokens(t *testing.T) {
	db := newStore(t)
	seedRun(t, db, "r_claimed", protocol.StateClaimed)
	req := protocol.RunRequest{ID: "r_claimed", Repo: "myapp", Job: "build", Ref: "main",
		Token: "ghs_jobJOBjob0123456789", FetchToken: "ghs_fetchFETCH0123456789"}
	if err := enqueueRun(t.Context(), db, req, 0, time.Time{}); err != nil {
		t.Fatalf("enqueueRun: %v", err)
	}
	if db.Token("r_claimed") != req.Token || db.FetchToken("r_claimed") != req.FetchToken {
		t.Error("the tokens a replay brought were not held")
	}
	r, err := db.Get(t.Context(), "r_claimed")
	if err != nil || r.State != protocol.StateClaimed {
		t.Errorf("state = %s, %v; want it left claimed", r.State, err)
	}
}

// A workflow job that came through a restart says so in the resume, and the
// control plane sends it again with fresh tokens (A75). One that still holds
// its tokens, one a person ran with `latem run`, and one already running do
// not.
func TestResume_SaysWhichJobsLostTheirTokens(t *testing.T) {
	db := newStore(t)
	workflowRun := func(id string, state protocol.RunState, origin string) {
		t.Helper()
		err := db.Create(t.Context(), store.Run{ID: id, Repo: "myapp", Job: "build", Ref: "main", State: state,
			OriginKind: origin, Workflow: &store.WorkflowRun{Workflow: ".github/workflows/ci.yml", SHA: strings.Repeat("a", 40),
				WorkflowRunID: "wr_1"}})
		if err != nil {
			t.Fatal(err)
		}
	}
	workflowRun("r_lost", protocol.StateQueued, "webhook")
	workflowRun("r_claimed", protocol.StateClaimed, "webhook")
	workflowRun("r_held", protocol.StateQueued, "webhook")
	db.HoldFetchToken("r_held", "ghs_fetchFETCH0123456789")
	workflowRun("r_cli", protocol.StateQueued, "cli")
	workflowRun("r_running", protocol.StateRunning, "webhook")
	seedRun(t, db, "r_registry", protocol.StateQueued)

	h := newHarnessOn(t, db, nil)
	h.AwaitConnected(t)
	h.AwaitResumes(t, 1)
	resumes := h.CP.Resumes()
	got := map[string]bool{}
	for _, r := range resumes[len(resumes)-1].Runs {
		got[r.RunID] = r.NeedsTokens
	}
	want := map[string]bool{"r_lost": true, "r_claimed": true, "r_held": false, "r_cli": false,
		"r_running": false, "r_registry": false}
	for id, w := range want {
		if got[id] != w {
			t.Errorf("%s: needs_tokens = %v, want %v", id, got[id], w)
		}
	}
}

// A run latem try made never reaches a control plane (A76), which has never
// heard of it and would answer every frame "unknown run". Its updates are
// recorded as delivered without being sent, so the replay set does not hand
// it back at every reconnect; its log goes nowhere; and the resume frame
// leaves it out. A control plane's run beside it is reported as ever.
func TestLocalRuns_NeverReachTheControlPlane(t *testing.T) {
	db := newStore(t)
	sha := strings.Repeat("c", 40)
	if err := db.Create(t.Context(), store.Run{ID: "r_try", Repo: "myapp", Job: "build", Ref: "refs/heads/x",
		State: protocol.StateRunning, OriginKind: "cli", ApprovedSHA: sha,
		Workflow: &store.WorkflowRun{Workflow: ".github/workflows/ci.yml", SHA: sha, WorkflowRunID: "r_try", Local: true}}); err != nil {
		t.Fatal(err)
	}
	seedRun(t, db, "r_cloud", protocol.StateRunning)

	h := newHarnessOn(t, db, nil)
	h.AwaitConnected(t)
	h.AwaitResumes(t, 1)
	for _, r := range h.CP.Resumes()[0].Runs {
		if r.RunID == "r_try" {
			t.Error("the resume frame lists a local run")
		}
	}

	sink, err := h.Logs(t.Context(), "r_try")
	if err != nil {
		t.Fatal(err)
	}
	if err := sink.Write(t.Context(), []protocol.LogFrame{{RunID: "r_try", Seq: 1, Stream: "stdout", At: time.Now(), Line: "hi"}}); err != nil {
		t.Fatal(err)
	}
	if err := sink.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	// Each reported in the state its row is in, as the agent reports a
	// transition only once it is persisted.
	for _, id := range []string{"r_try", "r_cloud"} {
		if err := h.Report(t.Context(), id, protocol.Update{State: protocol.StateRunning}); err != nil {
			t.Fatalf("Report %s: %v", id, err)
		}
	}
	h.CP.Await(t, "the control plane's run to be reported", func() bool {
		for _, u := range h.CP.Updates() {
			if u.RunID == "r_cloud" {
				return true
			}
		}
		return false
	})
	for _, u := range h.CP.Updates() {
		if u.RunID == "r_try" {
			t.Errorf("a local run's update reached the control plane: %+v", u)
		}
	}
	if frames := h.CP.Frames("r_try"); len(frames) != 0 {
		t.Errorf("%d log frame(s) of a local run reached the control plane", len(frames))
	}
	rows, err := db.Unreported(t.Context(), store.BrokerRemote)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if r.ID == "r_try" {
			t.Error("a local run is still in the remote transport's replay set")
		}
	}
}
