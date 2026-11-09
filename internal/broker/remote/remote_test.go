package remote

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/Abyssinia-Labs/latem/internal/broker"
	"github.com/Abyssinia-Labs/latem/internal/store"
	"github.com/Abyssinia-Labs/latem/pkg/protocol"
)

// TestRevokedToken_StopsRetryingAndStopsNothingElse is the fourth M6 clause,
// and it is a product property rather than an error path.
//
// A hosted layer that can switch off somebody's build machine by revoking a
// token is a hosted layer nobody should install. So a 401 must: stop the
// reconnect loop (not spin against a server that has already said no), say so
// once and loudly enough to be actionable, refuse further work honestly, and —
// the part that matters — leave [Open] having succeeded and the agent running.
func TestRevokedToken_StopsRetryingAndStopsNothingElse(t *testing.T) {
	// The control plane wants "right-token"; this machine has the old one.
	cp := newControlPlane(t, "right-token")
	db := newStore(t)
	clock := newFakeClock()

	b, err := Open(Options{
		URL:       cp.URL(),
		TokenFile: newTokenFile(t, "revoked-token"),
		MachineID: "mch_test",
		Store:     db,
		Logger:    testLogger(t),
		sleep:     clock.sleep,
		jitter:    func() float64 { return 0.5 },
	})
	if err != nil {
		t.Fatalf("Open returned an error for a revoked token: %v\n"+
			"a machine whose token was revoked must still start and still build", err)
	}
	t.Cleanup(func() { _ = b.Close() })

	cp.Await(t, "the control plane to refuse the token", func() bool { return b.Revoked() })

	// It must stop. A loop that kept dialling would hammer the control plane
	// forever for every revoked machine it has, which is a self-inflicted
	// denial of service on the day somebody rotates a fleet's tokens.
	refusals := cp.Refusals()
	if refusals != 1 {
		t.Errorf("the control plane refused %d handshakes; a 401 should be asked exactly once", refusals)
	}
	if got := clock.Total(); got != 0 {
		t.Errorf("the loop backed off %s after a 401; it should have stopped rather than scheduled a retry", got)
	}

	// Give it a generous window to prove it really has stopped.
	time.Sleep(50 * time.Millisecond)
	if cp.Refusals() != refusals {
		t.Errorf("the reconnect loop is still dialling after a 401 (%d → %d)", refusals, cp.Refusals())
	}

	// Everything the agent still calls on it must answer honestly rather than
	// blocking or pretending.
	if b.Connected() {
		t.Error("Connected() is true for a revoked broker")
	}
	if err := b.Heartbeat(t.Context(), protocol.MachineHealth{MachineID: "mch_test"}); err != nil {
		t.Errorf("Heartbeat on a revoked broker returned %v; the agent beats every fifteen seconds and would log every one of them", err)
	}
	if err := b.Claim(t.Context(), "r_1"); !errors.Is(err, ErrRevoked) {
		t.Errorf("Claim on a revoked broker returned %v, want ErrRevoked", err)
	}
	if err := b.Report(t.Context(), "r_1", protocol.Update{State: protocol.StateQueued}); !errors.Is(err, ErrRevoked) {
		t.Errorf("Report on a revoked broker returned %v, want ErrRevoked", err)
	}
	// A refused Claim is emphatically not ErrAlreadyClaimed: reporting "somebody
	// else has it" for "I could not ask" would idle a machine that could have
	// taken the work.
	if err := b.Claim(t.Context(), "r_1"); errors.Is(err, broker.ErrAlreadyClaimed) {
		t.Error("a revoked token was reported as another runner winning the claim")
	}

	// And the command stream stays open. The interface says it closes only on
	// ctx cancellation; an agent whose command loop exited here would need a
	// restart to notice a new token.
	cmds, err := b.Commands(t.Context())
	if err != nil {
		t.Fatalf("Commands on a revoked broker: %v", err)
	}
	select {
	case _, open := <-cmds:
		if !open {
			t.Error("the command channel closed because the token was revoked; it closes only on ctx cancellation")
		}
	case <-time.After(50 * time.Millisecond):
	}
}

// TestOpen_RequiresWhatItCannotInvent. Every one of these is a startup failure
// naming the config key, because this runs at §6 step 7 where there is no log
// pipeline and no run to diagnose from.
func TestOpen_RequiresWhatItCannotInvent(t *testing.T) {
	db := newStore(t)
	token := newTokenFile(t, "tok")

	tests := map[string]struct {
		opts Options
		want string
	}{
		"no url":        {Options{TokenFile: token, Store: db}, "url"},
		"no token file": {Options{URL: "wss://x/agent", Store: db}, "token_file"},
		"no store":      {Options{URL: "wss://x/agent", TokenFile: token}, "store"},
		"unknown log mode": {
			Options{URL: "wss://x/agent", TokenFile: token, Store: db, LogMode: "sometimes"},
			"remote_logs",
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			b, err := Open(tc.opts)
			if err == nil {
				_ = b.Close()
				t.Fatalf("Open succeeded without %s", name)
			}
			errorContains(t, err, tc.want)
		})
	}
}

// TestOpen_DoesNotWaitForTheNetwork. §6 must finish whether or not a control
// plane is reachable: an agent that refused to start because api.example.com
// was down would turn somebody else's outage into an outage here.
func TestOpen_DoesNotWaitForTheNetwork(t *testing.T) {
	db := newStore(t)
	start := time.Now()
	b, err := Open(Options{
		// A port nothing is listening on, so the dial fails at connect.
		URL:       "ws://127.0.0.1:1/agent",
		TokenFile: newTokenFile(t, "tok"),
		Store:     db,
		Logger:    testLogger(t),
		sleep:     newFakeClock().sleep,
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = b.Close() }()

	if took := time.Since(start); took > 2*time.Second {
		t.Errorf("Open took %s against an unreachable control plane", took)
	}
	if b.Connected() {
		t.Error("Connected() is true before any handshake completed")
	}
}

// TestClose_IsIdempotent. Amendment A19 exists because both graceful shutdown
// and a deferred close run on the normal path, and a second Close reporting a
// failure would mask the first error that mattered.
func TestClose_IsIdempotent(t *testing.T) {
	h := newHarness(t, nil)
	h.AwaitConnected(t)

	for i := range 3 {
		if err := h.Close(); err != nil {
			t.Errorf("Close call %d returned %v", i+1, err)
		}
	}
	if _, err := h.Commands(t.Context()); err == nil {
		t.Error("Commands succeeded on a closed broker")
	}
}

// TestCommands_OneConsumer. Two consumers would split the stream between them
// and a cancel would reach whichever half was not executing the run — a bug
// that presents as an intermittently ignored Stop button.
func TestCommands_OneConsumer(t *testing.T) {
	h := newHarness(t, nil)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	if _, err := h.Commands(ctx); err != nil {
		t.Fatalf("first Commands: %v", err)
	}
	if _, err := h.Commands(ctx); err == nil {
		t.Error("a second consumer was accepted")
	}
}

// TestCommands_RunIsDurableBeforeItIsAcknowledged.
//
// A control plane told "accepted" for a run that evaporates when the agent
// restarts a second later has been lied to. This is the same property §7.3
// gives the local socket, and the same reason: sqlite is the truth and the
// transport is a doorbell.
func TestCommands_RunIsDurableBeforeItIsAcknowledged(t *testing.T) {
	h := newHarness(t, func(o *harnessOptions) { o.queueTTL = time.Hour })
	h.AwaitConnected(t)

	cmds, err := h.Commands(t.Context())
	if err != nil {
		t.Fatalf("Commands: %v", err)
	}

	h.CP.Command(protocol.NewRunCommand(protocol.RunRequest{
		ID: "r_inbound", Repo: "myapp", Job: "build", Ref: "main",
		Origin: protocol.Origin{Kind: "webhook", Actor: "ci"},
	}))

	select {
	case cmd := <-cmds:
		if cmd.Kind != protocol.CommandRun || cmd.Run.ID != "r_inbound" {
			t.Fatalf("received %+v", cmd)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the run command never reached the consumer")
	}

	run, err := h.DB.Get(t.Context(), "r_inbound")
	if err != nil {
		t.Fatalf("the run was acknowledged but never persisted: %v", err)
	}
	if run.State != protocol.StateQueued {
		t.Errorf("run state is %s, want %s", run.State, protocol.StateQueued)
	}
	if run.ExpiresAt == nil {
		t.Error("queue_ttl produced no expires_at; §11's sweep has nothing to act on")
	}
	if run.OriginKind != "webhook" {
		t.Errorf("origin_kind is %q, want the one the control plane sent", run.OriginKind)
	}
}

// TestCommands_ReplayAfterResumeIsIdempotent is the trap this transport differs
// from the local one on.
//
// The local broker's Commands deliberately does not replay: sqlite is the truth
// and its socket is a doorbell. Here the resume frame *is* the replay mechanism,
// so the same run arrives again on every reconnect and every handler has to be
// idempotent by run id (§7.4).
func TestCommands_ReplayAfterResumeIsIdempotent(t *testing.T) {
	h := newHarness(t, nil)

	// The control plane replays a run every time it sees a resume frame, which
	// is what a real one does for work it has not been told the outcome of.
	h.CP.OnResume(func(cp *controlPlane, _ *websocket.Conn, _ protocol.Resume) {
		cp.Command(protocol.NewRunCommand(protocol.RunRequest{
			ID: "r_replayed", Repo: "myapp", Job: "build", Ref: "main",
		}))
	})

	h.AwaitConnected(t)
	cmds, err := h.Commands(t.Context())
	if err != nil {
		t.Fatalf("Commands: %v", err)
	}

	received := 0
	deadline := time.After(15 * time.Second)
	for received < 2 {
		if received == 1 {
			// Force a reconnect, and with it a second resume and a second copy
			// of the same run.
			h.CP.Partition()
			h.CP.Await(t, "the partition", func() bool { return !h.Connected() })
			h.CP.Heal()
		}
		select {
		case cmd := <-cmds:
			if cmd.Run == nil || cmd.Run.ID != "r_replayed" {
				t.Fatalf("received %+v", cmd)
			}
			received++
		case <-deadline:
			t.Fatalf("only %d copies of the replayed run arrived", received)
		}
	}

	run, err := h.DB.Get(t.Context(), "r_replayed")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if run.State != protocol.StateQueued {
		t.Errorf("after a replay the run is %s; the second copy should have been a no-op", run.State)
	}
}

// TestCommands_TerminalRunIsNotResurrected. Re-queueing a finished run under
// its old id would make the run history lie about which build produced which
// artifact, so the replay is refused rather than obeyed.
func TestCommands_TerminalRunIsNotResurrected(t *testing.T) {
	h := newHarness(t, nil)
	seedRun(t, h.DB, "r_done", protocol.StateSucceeded)
	h.AwaitConnected(t)
	h.AwaitResumes(t, 1)

	h.CP.Command(protocol.NewRunCommand(protocol.RunRequest{
		ID: "r_done", Repo: "myapp", Job: "build", Ref: "main",
	}))

	// The refusal comes back as a negative Ack; nothing is queued.
	time.Sleep(200 * time.Millisecond)
	run, err := h.DB.Get(t.Context(), "r_done")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if run.State != protocol.StateSucceeded {
		t.Errorf("a finished run was moved to %s by a replayed command", run.State)
	}
}

// TestClaim_RefusalIsAnOutcomeAndAFailureIsNot.
//
// Losing a claim is ordinary and the interface says so. A transport error is
// not: the answer is unknown, and reporting "somebody else has it" would
// silently idle a machine that could have taken the work.
func TestClaim_RefusalIsAnOutcomeAndAFailureIsNot(t *testing.T) {
	h := newHarness(t, nil)
	h.AwaitConnected(t)

	if err := h.Claim(t.Context(), "r_win"); err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if got := h.CP.Claims(); len(got) != 1 || got[0] != "r_win" {
		t.Errorf("the control plane saw claims %v", got)
	}

	h.CP.RefuseClaims(true)
	err := h.Claim(t.Context(), "r_lose")
	if !errors.Is(err, broker.ErrAlreadyClaimed) {
		t.Errorf("a refused claim returned %v, want broker.ErrAlreadyClaimed", err)
	}
	errorContains(t, err, "another runner claimed it first")

	h.CP.Partition()
	h.CP.Await(t, "the partition", func() bool { return !h.Connected() })
	err = h.Claim(t.Context(), "r_unknown")
	if errors.Is(err, broker.ErrAlreadyClaimed) {
		t.Error("a partition was reported as another runner winning the claim; this machine would sit idle beside work it could do")
	}
	if !errors.Is(err, ErrDisconnected) {
		t.Errorf("Claim during a partition returned %v, want ErrDisconnected", err)
	}

	if err := h.Claim(t.Context(), "../../etc/passwd"); err == nil {
		t.Error("Claim accepted a path traversal as a run id (INV-3, amendment A6)")
	}
}

// TestHeartbeat_PublishesTheCurrentStateTheMomentTheLinkReturns.
//
// A machine that was away for ten minutes must not look idle for another
// fifteen seconds after it comes back.
func TestHeartbeat_PublishesTheCurrentStateTheMomentTheLinkReturns(t *testing.T) {
	h := newHarness(t, nil)
	h.AwaitConnected(t)

	health := protocol.MachineHealth{
		MachineID: "mch_test",
		AgentVer:  "test",
		Runners:   []protocol.RunnerInfo{{Name: "linux-x64", Executor: "docker", Concurrency: 2, Available: true}},
	}
	if err := h.Heartbeat(t.Context(), health); err != nil {
		t.Fatalf("Heartbeat: %v", err)
	}
	h.CP.Await(t, "the first beat", func() bool { return len(h.CP.Heartbeats()) >= 1 })

	before := len(h.CP.Heartbeats())
	h.CP.Partition()
	h.CP.Await(t, "the partition", func() bool { return !h.Connected() })

	// Beating into a partition is not an error the agent should log forty times.
	if err := h.Heartbeat(t.Context(), health); err != nil {
		t.Errorf("Heartbeat during a partition returned %v; the reconnect loop already reported the outage", err)
	}

	h.CP.Heal()
	h.CP.Await(t, "a beat on the new connection", func() bool { return len(h.CP.Heartbeats()) > before })

	last := h.CP.Heartbeats()
	got := last[len(last)-1]
	if len(got.Runners) != 1 || got.Runners[0].Name != "linux-x64" {
		t.Errorf("the beat sent on reconnect was %+v, want the last known health", got)
	}
}

// TestHello_RecordsTheMachineIDTheControlPlaneAssigns. `latem register` writes
// it into `[machine] id`, and it has to come from somewhere.
func TestHello_RecordsTheMachineIDTheControlPlaneAssigns(t *testing.T) {
	h := newHarness(t, nil)
	h.CP.SetHello(&helloFrame{MachineID: "mch_01HXYZ", Version: protocol.Version})
	h.CP.Partition()
	h.CP.Await(t, "the partition", func() bool { return !h.Connected() })
	h.CP.Heal()

	h.CP.Await(t, "the machine id", func() bool { return h.MachineID() == "mch_01HXYZ" })
}

// TestRequires_OnlyToAControlPlaneThatSays22. A 2.1 control plane decodes a
// plan strictly, so a job's requires would cost it the whole plan; a 2.2 one
// gets them, and a heartbeat's toolchains (A61).
func TestRequires_OnlyToAControlPlaneThatSays22(t *testing.T) {
	plan := protocol.PlanResult{ID: "p-1", Workflows: []protocol.PlannedWorkflow{{
		File: ".github/workflows/apple.yml", Name: "apple",
		Jobs: []protocol.PlannedJob{{Key: "apple", Name: "apple", TimeoutMinutes: 360,
			Requires: []protocol.Requirement{{Key: "xcode", Version: "27"}}}},
	}}}
	health := protocol.MachineHealth{MachineID: "m", Toolchains: &protocol.Toolchains{Xcode: []string{"26.0.1"}}}

	for _, c := range []struct {
		version string
		want    bool
	}{{"2.1", false}, {"", false}, {"2.2", true}, {"2.10", true}, {"3.0", true}} {
		h := newHarness(t, nil)
		h.CP.SetHello(&helloFrame{Version: c.version})
		h.CP.Partition()
		h.CP.Await(t, "the partition", func() bool { return !h.Connected() })
		h.CP.Heal()
		h.AwaitConnected(t)
		h.CP.Await(t, "the hello", func() bool { return h.SpeaksRequires() == c.want || c.version == "" })

		if err := h.ReportPlan(t.Context(), plan); err != nil {
			t.Fatalf("%q: ReportPlan: %v", c.version, err)
		}
		if err := h.Heartbeat(t.Context(), health); err != nil {
			t.Fatalf("%q: Heartbeat: %v", c.version, err)
		}
		h.CP.Await(t, "the beat", func() bool { return len(h.CP.Heartbeats()) > 0 })
		plans, beats := h.CP.Plans(), h.CP.Heartbeats()
		gotReq := plans[len(plans)-1].Workflows[0].Jobs[0].Requires != nil
		gotTC := beats[len(beats)-1].Toolchains != nil
		if gotReq != c.want || gotTC != c.want {
			t.Errorf("hello %q: requires sent %v, toolchains sent %v; want %v", c.version, gotReq, gotTC, c.want)
		}
	}
	if plan.Workflows[0].Jobs[0].Requires == nil {
		t.Error("stripping for an old control plane wrote the caller's plan")
	}
}

// A called workflow's jobs are planned, under job paths, only for a control
// plane that says 2.8 (A82): an older one refuses a "/" in a plan's key, and
// the whole plan with it.
func TestCalls_OnlyForAControlPlaneThatSays28(t *testing.T) {
	for _, c := range []struct {
		version string
		want    bool
	}{{"2.7", false}, {"", false}, {"2.8", true}, {"2.10", true}, {"3.0", true}} {
		h := newHarness(t, nil)
		h.CP.SetHello(&helloFrame{Version: c.version})
		h.CP.Partition()
		h.CP.Await(t, "the partition", func() bool { return !h.Connected() })
		h.CP.Heal()
		h.AwaitConnected(t)
		h.CP.Await(t, "the hello", func() bool { return h.SpeaksCalls() == c.want })
	}
}

// A job's artifacts_from goes only to a control plane that says 2.9 (A84),
// which places the job on the machine that ran those jobs: an older one
// decodes a plan strictly. The agent plans such a job with an error for one,
// so this is the decoder's protection, not the job's.
func TestArtifacts_OnlyToAControlPlaneThatSays29(t *testing.T) {
	plan := protocol.PlanResult{ID: "p-1", Workflows: []protocol.PlannedWorkflow{{
		File: ".github/workflows/mac-release.yml", Name: "mac-release",
		Jobs: []protocol.PlannedJob{{Key: "publish", Name: "publish", Needs: []string{"build"}, TimeoutMinutes: 360,
			ArtifactsFrom: []string{"build"}}},
	}}}
	for _, c := range []struct {
		version string
		want    bool
	}{{"2.8", false}, {"", false}, {"2.9", true}, {"2.10", true}, {"3.0", true}} {
		h := newHarness(t, nil)
		h.CP.SetHello(&helloFrame{Version: c.version})
		h.CP.Partition()
		h.CP.Await(t, "the partition", func() bool { return !h.Connected() })
		h.CP.Heal()
		h.AwaitConnected(t)
		h.CP.Await(t, "the hello", func() bool { return h.SpeaksArtifacts() == c.want || c.version == "" })
		if err := h.ReportPlan(t.Context(), plan); err != nil {
			t.Fatalf("%q: ReportPlan: %v", c.version, err)
		}
		plans := h.CP.Plans()
		if got := plans[len(plans)-1].Workflows[0].Jobs[0].ArtifactsFrom != nil; got != c.want {
			t.Errorf("hello %q: artifacts_from sent %v, want %v", c.version, got, c.want)
		}
	}
	if plan.Workflows[0].Jobs[0].ArtifactsFrom == nil {
		t.Error("stripping for an old control plane wrote the caller's plan")
	}
}

// A job's runs_on and reads_vars, and a heartbeat's aliases and digests (A68),
// go only to a control plane that says 2.4: a 2.3 one decodes both strictly.
func TestPlacement_OnlyToAControlPlaneThatSays24(t *testing.T) {
	plan := protocol.PlanResult{ID: "p-1", Workflows: []protocol.PlannedWorkflow{{
		File: ".github/workflows/ci.yml", Name: "ci",
		Jobs: []protocol.PlannedJob{{Key: "web", Name: "web", TimeoutMinutes: 360,
			RunsOn:    []protocol.RunsOnLabel{{Label: "blacksmith-4vcpu-ubuntu-2404"}},
			ReadsVars: []string{"LATEM"},
			Requires:  []protocol.Requirement{{Key: "bun"}}}},
	}}}
	health := protocol.MachineHealth{MachineID: "m",
		LabelAliases: map[string][]string{"blacksmith-4vcpu-ubuntu-2404": {"linux"}},
		Repos:        []protocol.RepoInfo{{Key: "app", Vars: map[string]string{"LATEM": "b5bea41b6c623f7c"}}}}

	for _, c := range []struct {
		version string
		want    bool
	}{{"2.2", false}, {"2.3", false}, {"2.4", true}, {"2.10", true}} {
		h := newHarness(t, nil)
		h.CP.SetHello(&helloFrame{Version: c.version})
		h.CP.Partition()
		h.CP.Await(t, "the partition", func() bool { return !h.Connected() })
		h.CP.Heal()
		h.AwaitConnected(t)
		h.CP.Await(t, "the hello", func() bool { return h.speaksPlacement() == c.want && h.SpeaksRequires() })

		if err := h.ReportPlan(t.Context(), plan); err != nil {
			t.Fatalf("%q: ReportPlan: %v", c.version, err)
		}
		if err := h.Heartbeat(t.Context(), health); err != nil {
			t.Fatalf("%q: Heartbeat: %v", c.version, err)
		}
		h.CP.Await(t, "the beat", func() bool { return len(h.CP.Heartbeats()) > 0 })
		plans, beats := h.CP.Plans(), h.CP.Heartbeats()
		job, beat := plans[len(plans)-1].Workflows[0].Jobs[0], beats[len(beats)-1]
		for what, got := range map[string]bool{
			"runs_on": job.RunsOn != nil, "reads_vars": job.ReadsVars != nil,
			"label_aliases": beat.LabelAliases != nil, "vars": beat.Repos[0].Vars != nil,
		} {
			if got != c.want {
				t.Errorf("hello %q: %s sent %v, want %v", c.version, what, got, c.want)
			}
		}
		if job.Requires == nil {
			t.Errorf("hello %q: a control plane that says 2.2 lost requires", c.version)
		}
	}
	if plan.Workflows[0].Jobs[0].RunsOn == nil || health.Repos[0].Vars == nil {
		t.Error("stripping for an old control plane wrote the caller's values")
	}
}

// A heartbeat's capacity (A63) goes only to a control plane that says 2.3:
// a 2.2 one decodes heartbeats strictly, and would refuse the field.
func TestCapacity_OnlyToAControlPlaneThatSays23(t *testing.T) {
	health := protocol.MachineHealth{MachineID: "m", Toolchains: &protocol.Toolchains{Xcode: []string{"27.2"}},
		Capacity: &protocol.Capacity{CPUs: 8, MemoryGB: 8, Runners: []protocol.RunnerCapacity{{Name: "linux", Recommended: 1, Why: "w"}}}}

	for _, c := range []struct {
		version   string
		toolchain bool
		capacity  bool
	}{{"2.1", false, false}, {"2.2", true, false}, {"2.3", true, true}, {"2.10", true, true}} {
		h := newHarness(t, nil)
		h.CP.SetHello(&helloFrame{Version: c.version})
		h.CP.Partition()
		h.CP.Await(t, "the partition", func() bool { return !h.Connected() })
		h.CP.Heal()
		h.AwaitConnected(t)
		h.CP.Await(t, "the hello", func() bool { return h.SpeaksRequires() == c.toolchain })

		if err := h.Heartbeat(t.Context(), health); err != nil {
			t.Fatalf("%q: Heartbeat: %v", c.version, err)
		}
		h.CP.Await(t, "the beat", func() bool { return len(h.CP.Heartbeats()) > 0 })
		beats := h.CP.Heartbeats()
		last := beats[len(beats)-1]
		if (last.Toolchains != nil) != c.toolchain || (last.Capacity != nil) != c.capacity {
			t.Errorf("hello %q: toolchains sent %v, capacity sent %v; want %v, %v",
				c.version, last.Toolchains != nil, last.Capacity != nil, c.toolchain, c.capacity)
		}
	}
	if health.Capacity == nil {
		t.Error("stripping for an old control plane wrote the caller's heartbeat")
	}
}

// A heartbeat's readiness (A70) goes only to a control plane that says 2.5: a
// 2.4 one decodes heartbeats strictly, and would refuse the whole beat.
func TestReadiness_OnlyToAControlPlaneThatSays25(t *testing.T) {
	health := protocol.MachineHealth{MachineID: "m", Capacity: &protocol.Capacity{CPUs: 8, MemoryGB: 8},
		Readiness: &protocol.Readiness{Kinds: []protocol.ReadinessKind{{Kind: "android", Title: "Android", State: "off"}}}}

	for _, c := range []struct {
		version   string
		readiness bool
	}{{"2.3", false}, {"2.4", false}, {"2.5", true}, {"2.10", true}} {
		h := newHarness(t, nil)
		h.CP.SetHello(&helloFrame{Version: c.version})
		h.CP.Partition()
		h.CP.Await(t, "the partition", func() bool { return !h.Connected() })
		h.CP.Heal()
		h.AwaitConnected(t)
		h.CP.Await(t, "the hello", func() bool { return h.speaksReadiness() == c.readiness && h.speaksCapacity() })

		if err := h.Heartbeat(t.Context(), health); err != nil {
			t.Fatalf("%q: Heartbeat: %v", c.version, err)
		}
		h.CP.Await(t, "the beat", func() bool { return len(h.CP.Heartbeats()) > 0 })
		beats := h.CP.Heartbeats()
		if last := beats[len(beats)-1]; (last.Readiness != nil) != c.readiness {
			t.Errorf("hello %q: readiness sent %v, want %v", c.version, last.Readiness != nil, c.readiness)
		}
	}
	if health.Readiness == nil {
		t.Error("stripping for an old control plane wrote the caller's heartbeat")
	}
}

// TestReport_RefusalIsNotReplayed.
//
// A refusal is the control plane's answer, not a delivery failure. Replaying
// something the far side declined would loop on every reconnect for the life of
// the run.
func TestReport_RefusalIsNotReplayed(t *testing.T) {
	h := newHarness(t, nil)
	seedRun(t, h.DB, "r_refused", protocol.StateRunning)
	h.AwaitConnected(t)

	h.CP.mu.Lock()
	h.CP.refuseUpdates = true
	h.CP.mu.Unlock()

	err := h.Report(t.Context(), "r_refused", protocol.Update{State: protocol.StateCollecting})
	if err == nil {
		t.Fatal("a refused report returned nil")
	}
	if h.pendingCount() != 0 {
		t.Errorf("a refused report was queued for replay (%d pending); it would be re-sent on every reconnect", h.pendingCount())
	}
}

// TestReport_RejectsWhatTheStateMachineCannotMean. The id check is INV-3; the
// state check keeps a typo from reaching a control plane as a run in a state
// nothing defines.
func TestReport_RejectsWhatTheStateMachineCannotMean(t *testing.T) {
	h := newHarness(t, nil)
	h.AwaitConnected(t)

	if err := h.Report(t.Context(), "../x", protocol.Update{State: protocol.StateQueued}); err == nil {
		t.Error("Report accepted a path traversal as a run id")
	}
	err := h.Report(t.Context(), "r_ok", protocol.Update{State: "finished-ish"})
	errorContains(t, err, "unknown state")
}

// TestStoreInterface_IsSatisfiedByTheRealDatabase. The var in store.go is the
// compile-time half; this is the half that proves the methods behave.
func TestStoreInterface_IsSatisfiedByTheRealDatabase(t *testing.T) {
	db := newStore(t)
	var st Store = db

	if err := st.Create(t.Context(), store.Run{
		ID: "r_iface", Repo: "myapp", Job: "build", Ref: "main",
		State: protocol.StateQueued, OriginKind: "manual",
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := st.Enqueue(t.Context(), "r_iface", 0); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if err := st.SetExpiry(t.Context(), "r_iface", time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("SetExpiry: %v", err)
	}
	if err := st.SetLastAckedSeq(t.Context(), store.BrokerRemote, "r_iface", 7); err != nil {
		t.Fatalf("SetLastAckedSeq: %v", err)
	}
	if seq, serr := st.LastAckedSeq(t.Context(), store.BrokerRemote, "r_iface"); serr != nil || seq != 7 {
		t.Fatalf("LastAckedSeq = %d, %v; want 7", seq, serr)
	}
	if err := st.MarkReported(t.Context(), store.BrokerRemote, "r_iface", protocol.StateQueued); err != nil {
		t.Fatalf("MarkReported: %v", err)
	}
	rows, err := st.NonTerminal(t.Context())
	if err != nil || len(rows) != 1 {
		t.Fatalf("NonTerminal = %d rows, %v", len(rows), err)
	}
	if rows, uerr := st.Unreported(t.Context(), store.BrokerRemote); uerr != nil || len(rows) != 0 {
		t.Fatalf("Unreported after MarkReported = %d rows, %v; want none", len(rows), uerr)
	}
	if _, err := st.Get(t.Context(), "r_iface"); err != nil {
		t.Fatalf("Get: %v", err)
	}
}

// TestReport_DeliveredTransitionsLeaveTheReplaySet.
//
// store.Unreported(remote) is the restart-safe backstop, and a backstop that
// never empties is a machine that re-sends its whole history on every reconnect
// — growing with every build it ever runs. Recording the delivery is what
// shrinks it, and nothing else records it: amendment A28 gave this transport its
// own broker_state row precisely because the local broker was marking the shared
// column the instant a transition hit the disk.
//
// Both writers are covered: the connection's own replay of what the store said
// was outstanding, and [Broker.Report] on the live path.
func TestReport_DeliveredTransitionsLeaveTheReplaySet(t *testing.T) {
	const runID = "r_delivered"

	// The run exists before the broker does. Seeded after newHarness, it
	// raced the connection the harness starts: a replay that read the store
	// first never saw the run, and the wait below timed out on a loaded
	// Blacksmith runner. A sleep before seeding made it fail every time.
	db := newStore(t)
	seedRun(t, db, runID, protocol.StateRunning)
	h := newHarness(t, func(o *harnessOptions) { o.db = db })
	h.AwaitConnected(t)
	h.AwaitResumes(t, 1)

	// The connect-time replay: the run was outstanding, it was sent, and the
	// delivery is recorded. Without that record this never empties.
	h.CP.Await(t, "the connection's replay to clear the backstop", func() bool {
		rows, err := h.DB.Unreported(t.Context(), store.BrokerRemote)
		return err == nil && len(rows) == 0
	})

	// A transition the transport has not sent yet puts it back.
	if err := h.DB.Transition(t.Context(), runID, protocol.StateCollecting, protocol.Update{}); err != nil {
		t.Fatal(err)
	}
	rows, err := h.DB.Unreported(t.Context(), store.BrokerRemote)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].ID != runID {
		t.Fatalf("Unreported after an unsent transition = %+v, want the run", rows)
	}

	// The live path records its own delivery too.
	if err := h.Report(t.Context(), runID, protocol.Update{State: protocol.StateCollecting}); err != nil {
		t.Fatalf("Report: %v", err)
	}
	if rows, err = h.DB.Unreported(t.Context(), store.BrokerRemote); err != nil {
		t.Fatal(err)
	} else if len(rows) != 0 {
		t.Errorf("Unreported after a delivered report = %+v; the run would be replayed on every reconnect from now on", rows)
	}

	// A refusal is not a delivery. The control plane declined this state, so
	// pretending it took it would hide the transition from the replay forever.
	h.CP.mu.Lock()
	h.CP.refuseUpdates = true
	h.CP.mu.Unlock()
	finished := time.Now().UTC()
	if err := h.DB.Transition(t.Context(), runID, protocol.StateSucceeded,
		protocol.Update{FinishedAt: &finished}); err != nil {
		t.Fatal(err)
	}
	if rerr := h.Report(t.Context(), runID, protocol.Update{State: protocol.StateSucceeded}); rerr == nil {
		t.Fatal("a refused Report returned nil")
	}
	if rows, err = h.DB.Unreported(t.Context(), store.BrokerRemote); err != nil {
		t.Fatal(err)
	} else if len(rows) != 1 {
		t.Errorf("Unreported after a REFUSED report = %+v; a state the control plane declined was recorded as delivered and would never be replayed", rows)
	}
}
