package agent

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Abyssinia-Labs/latem/internal/broker/local"
	"github.com/Abyssinia-Labs/latem/internal/config"
	"github.com/Abyssinia-Labs/latem/internal/store"
	"github.com/Abyssinia-Labs/latem/pkg/protocol"
)

const testJobs = `version: 1

jobs:
  test:
    description: a job that does nothing slowly
    runs_on: [test]
    timeout: 2m
    steps:
      - name: hello
        run: echo hi
`

// newRunnableConfig writes a config whose repo is a real git repository on
// disk carrying .latem/jobs.yaml, so gates 6 through 11 run for real.
//
// INV-5 is why the repository has to exist: the registry is read from the
// checked-out workspace at the resolved commit and never from anything the
// caller supplies, so a test that wanted a job to be found had to commit one.
func newRunnableConfig(t *testing.T) *config.Config {
	t.Helper()
	if !hasGit(t) {
		t.Skip("git is not installed; the registry is read from a real checkout (INV-5)")
	}
	repo := shortTempDir(t)
	newRepo(t, repo, testJobs)
	return newConfigDir(t, configOptions{repoURL: repo})
}

// enqueueDirectly writes a queued run the way `latem run` does with no agent
// listening (§7.3): straight into sqlite, with nothing to notify.
func enqueueDirectly(t *testing.T, cfg *config.Config, id, ref string) {
	t.Helper()
	db := openStore(t, cfg)
	ctx := context.Background()
	if err := db.Create(ctx, store.Run{
		ID:         id,
		Repo:       "app",
		Job:        "test",
		Ref:        ref,
		State:      protocol.StateQueued,
		OriginKind: "cli",
		CreatedAt:  time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.Enqueue(ctx, id, 0); err != nil {
		t.Fatal(err)
	}
}

// waitState blocks until a run reaches one of the given states.
func waitState(t *testing.T, cfg *config.Config, id string, timeout time.Duration, want ...protocol.RunState) store.Run {
	t.Helper()
	db := openStore(t, cfg)
	var last store.Run
	waitFor(t, "run "+id+" to reach "+statesString(want), timeout, func() bool {
		r, err := db.Get(context.Background(), id)
		if err != nil {
			return false
		}
		last = r
		for _, w := range want {
			if r.State == w {
				return true
			}
		}
		return false
	})
	return last
}

func statesString(states []protocol.RunState) string {
	out := ""
	for i, s := range states {
		if i > 0 {
			out += " or "
		}
		out += string(s)
	}
	return out
}

// This is the trap internal/broker/local's package comment warns about, and it
// is the single most expensive mistake available in this package.
//
// Broker.Commands deliberately does not replay the backlog: sqlite is the
// truth and the socket is only a doorbell. So a run enqueued while the agent
// was down — which §7.3 makes an ordinary, supported thing rather than an edge
// case — arrives with no notification behind it and is reachable only through
// store.ClaimNext. An agent that waited on Commands alone would sit idle
// beside a full queue, forever, and every symptom of that bug looks like the
// queue being broken rather than the pool never asking.
//
// Nothing here touches the socket. That is the point.
func TestPoolExecutesARunEnqueuedWhileTheAgentWasDown(t *testing.T) {
	cfg := newRunnableConfig(t)
	enqueueDirectly(t, cfg, "r_backlog", "main")

	ta := newTestAgent(t, cfg, nil)
	ta.run(t)

	r := waitState(t, cfg, "r_backlog", 60*time.Second,
		protocol.StateSucceeded, protocol.StateFailed, protocol.StateRejected)
	if r.State != protocol.StateSucceeded {
		t.Fatalf("run finished %s (%s): %s", r.State, r.ErrorCode, r.ErrorMsg)
	}
	if r.Runner != "test" {
		t.Errorf("runner = %q, want the slot gate 13 selected", r.Runner)
	}
	// The §22 audit columns amendment A21 exists for. They are written by
	// internal/run and applied by internal/store; this asserts them from the
	// agent's side because a run driven end to end is the only place all three
	// packages have to agree, and because "requested as main, built
	// refs/heads/main at 3bf1261" is the answer to the only question anyone
	// asks about a surprising build (INV-5).
	if r.RefNormalized != "refs/heads/main" {
		t.Errorf("ref_normalized = %q, want refs/heads/main (INV-5, amendment A21)", r.RefNormalized)
	}
	if r.SHA == "" {
		t.Error("the run should record the commit it resolved (§22, amendment A21)")
	}
}

// The doorbell path: `latem run` against a live agent should start now, not at
// the next poll.
func TestPoolExecutesARunSubmittedOverTheSocket(t *testing.T) {
	cfg := newRunnableConfig(t)
	ta := newTestAgent(t, cfg, func(o *Options) {
		// A poll interval longer than the test proves the run started because
		// of the notification and not because a ticker happened to fire.
		o.ClaimInterval = time.Hour
	})
	ta.run(t)

	db := openStore(t, cfg)
	client, err := local.NewClient(local.ClientOptions{Socket: cfg.Paths.Socket, Store: db})
	if err != nil {
		t.Fatal(err)
	}
	sub, err := client.Submit(context.Background(), protocol.RunRequest{
		ID:     "r_socket",
		Repo:   "app",
		Job:    "test",
		Ref:    "main",
		Origin: protocol.Origin{Kind: "cli", Actor: "test"},
	}, local.SubmitOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !sub.AgentRunning {
		t.Fatal("the agent was up but the client took the queue-only path")
	}

	r := waitState(t, cfg, "r_socket", 60*time.Second,
		protocol.StateSucceeded, protocol.StateFailed, protocol.StateRejected)
	if r.State != protocol.StateSucceeded {
		t.Fatalf("run finished %s (%s): %s", r.State, r.ErrorCode, r.ErrorMsg)
	}
}

// A cancel is the same protocol.Command a phone would send, on a different
// transport (§7.3). It has to reach the goroutine holding the run's context.
func TestCancelOverTheSocketStopsARunningRun(t *testing.T) {
	cfg := newRunnableConfig(t)
	ta := newTestAgent(t, cfg, nil)
	ta.exec.hold = 30 * time.Second
	ta.run(t)

	enqueueDirectly(t, cfg, "r_cancel", "main")
	waitState(t, cfg, "r_cancel", 60*time.Second, protocol.StateRunning)

	db := openStore(t, cfg)
	client, err := local.NewClient(local.ClientOptions{Socket: cfg.Paths.Socket, Store: db})
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Cancel(context.Background(), protocol.CancelRequest{
		ID: "r_cancel", Actor: "tester", Reason: "changed my mind",
	}); err != nil {
		t.Fatal(err)
	}

	r := waitState(t, cfg, "r_cancel", 60*time.Second, protocol.StateCancelled, protocol.StateFailed)
	if r.State != protocol.StateCancelled {
		t.Fatalf("run finished %s (%s): %s", r.State, r.ErrorCode, r.ErrorMsg)
	}
	if r.CancelReason == "" {
		t.Error("the reason should be persisted; it exists so a build stopped from a phone says why")
	}

	// §11: a cancelled run still runs collecting, so its workspace is torn
	// down rather than left for the next reconciliation.
	waitFor(t, "the cancelled run's workspace to be removed", 30*time.Second, func() bool {
		_, err := statPath(filepath.Join(cfg.Paths.Work, "r_cancel"))
		return err != nil
	})
}

// A ref outside allowed_refs parks the run, and only a local action can
// release it — there is no inbound command kind that could (INV-2). Amendment
// A17 is what makes the parking possible at all: gate 8 runs during preparing
// and until A17 the state machine had no edge out of it.
//
// This is INV-5's approval, end to end, against a real git repository and a
// real agent: park → approve → claim → prepare → build. It could not be written
// before amendment A27. §22 had no approved_at column, registry.AdmitRef took
// no "a human already said yes" input, and gate 8 re-evaluated the ref policy
// on every attempt — so a run moved back to queued by `latem approve` was
// claimed, prepared, and parked again by the same gate, having paid for another
// mirror fetch on the way. The end state was awaiting_approval, exactly where
// it started, and the loop never terminated.
func TestARefOutsidePolicyIsParkedAndRunsOnceApproved(t *testing.T) {
	if !hasGit(t) {
		t.Skip("git is not installed")
	}
	repo := shortTempDir(t)
	newRepo(t, repo, testJobs)
	runGit(t, repo, "branch", "sideshow")
	sideshow := runGit(t, repo, "rev-parse", "sideshow")

	cfg := newConfigDir(t, configOptions{repoURL: repo})
	ta := newTestAgent(t, cfg, nil)
	ta.run(t)

	enqueueDirectly(t, cfg, "r_parked", "sideshow")
	parked := waitState(t, cfg, "r_parked", 60*time.Second,
		protocol.StateAwaitingApproval, protocol.StateRejected, protocol.StateFailed)
	if parked.State != protocol.StateAwaitingApproval {
		t.Fatalf("a ref outside allowed_refs gave %s (%s): %s",
			parked.State, parked.ErrorCode, parked.ErrorMsg)
	}
	if parked.ExpiresAt == nil {
		t.Error("a parked run needs an approval deadline or it accumulates forever (finding F7)")
	}
	if parked.ErrorMsg == "" {
		t.Error("a parked run should say which policy parked it")
	}

	// The park names a commit. Without one there is nothing for an approval to
	// mean, because a ref is a value a contributor can force-push.
	if parked.SHA != sideshow {
		t.Fatalf("parked at %q, want the commit sideshow resolves to, %q", parked.SHA, sideshow)
	}
	if parked.RefNormalized != "refs/heads/sideshow" {
		t.Errorf("ref_normalized = %q, want refs/heads/sideshow", parked.RefNormalized)
	}

	approved, err := Approve(context.Background(), openStore(t, cfg),
		"r_parked", 72*time.Hour, time.Now())
	if err != nil {
		t.Fatalf("Approve: %v", err)
	}
	if approved.ApprovedSHA != sideshow {
		t.Fatalf("approved %q, want %q", approved.ApprovedSHA, sideshow)
	}

	done := waitState(t, cfg, "r_parked", 60*time.Second,
		protocol.StateSucceeded, protocol.StateFailed,
		protocol.StateRejected, protocol.StateAwaitingApproval)
	if done.State != protocol.StateSucceeded {
		t.Fatalf("an approved run gave %s (%s): %s", done.State, done.ErrorCode, done.ErrorMsg)
	}
	if done.SHA != sideshow {
		t.Errorf("built %q, approved %q", done.SHA, sideshow)
	}
}

// The same path, with the branch moving between the park and the approval —
// which is the case INV-5's approval exists for. The operator is shown one
// commit; by the time they say yes, a contributor has pushed another. What they
// approved is the commit they were shown, so the run parks again rather than
// building code nobody looked at.
func TestAnApprovalDoesNotCoverACommitThatArrivedAfterIt(t *testing.T) {
	if !hasGit(t) {
		t.Skip("git is not installed")
	}
	repo := shortTempDir(t)
	newRepo(t, repo, testJobs)
	runGit(t, repo, "checkout", "-q", "-b", "sideshow")
	first := runGit(t, repo, "rev-parse", "HEAD")

	cfg := newConfigDir(t, configOptions{repoURL: repo})
	ta := newTestAgent(t, cfg, nil)
	ta.run(t)

	enqueueDirectly(t, cfg, "r_moved", "sideshow")
	parked := waitState(t, cfg, "r_moved", 60*time.Second,
		protocol.StateAwaitingApproval, protocol.StateRejected, protocol.StateFailed)
	if parked.SHA != first {
		t.Fatalf("parked at %q, want %q", parked.SHA, first)
	}

	// The push, after the operator was shown `first` and before they act.
	runGit(t, repo, "commit", "-q", "--allow-empty", "-m", "something else entirely")
	second := runGit(t, repo, "rev-parse", "HEAD")
	if second == first {
		t.Fatal("the branch did not move")
	}

	if _, err := Approve(context.Background(), openStore(t, cfg),
		"r_moved", 72*time.Hour, time.Now()); err != nil {
		t.Fatalf("Approve: %v", err)
	}

	// It comes back parked at the new commit, with the old approval discarded.
	db := openStore(t, cfg)
	var reparked store.Run
	waitFor(t, "the run to be parked again at the commit that arrived", 60*time.Second, func() bool {
		r, err := db.Get(context.Background(), "r_moved")
		if err != nil {
			return false
		}
		reparked = r
		return r.State == protocol.StateAwaitingApproval && r.SHA == second
	})
	if reparked.ApprovedSHA != "" {
		t.Errorf("the approval of %s survived the branch moving to %s", first, second)
	}
	if !strings.Contains(reparked.ErrorMsg, "moved") {
		t.Errorf("the run should say the ref moved: %q", reparked.ErrorMsg)
	}

	// And approving what is now on the branch builds it, so the mechanism is a
	// question that can be answered rather than a dead end.
	approved, err := Approve(context.Background(), db, "r_moved", 72*time.Hour, time.Now())
	if err != nil {
		t.Fatalf("second Approve: %v", err)
	}
	if approved.ApprovedSHA != second {
		t.Fatalf("approved %q, want %q", approved.ApprovedSHA, second)
	}
	done := waitState(t, cfg, "r_moved", 60*time.Second,
		protocol.StateSucceeded, protocol.StateFailed, protocol.StateRejected)
	if done.State != protocol.StateSucceeded {
		t.Fatalf("state = %s (%s): %s", done.State, done.ErrorCode, done.ErrorMsg)
	}
	if done.SHA != second {
		t.Errorf("built %q, approved %q", done.SHA, second)
	}
}

// Shutdown stops accepting and lets in-flight work finish (§6). A run that was
// half way through an archive when somebody typed `systemctl restart` should
// not be thrown away.
func TestGracefulShutdownLetsAnInFlightRunFinish(t *testing.T) {
	cfg := newRunnableConfig(t)
	ta := newTestAgent(t, cfg, nil)
	ta.exec.hold = 2 * time.Second
	agentRun := ta.run(t)

	enqueueDirectly(t, cfg, "r_inflight", "main")
	waitState(t, cfg, "r_inflight", 60*time.Second, protocol.StateRunning)

	agentRun.stop()
	if err := agentRun.wait(t, 60*time.Second); err != nil {
		t.Fatalf("Run returned %v", err)
	}

	db := openStore(t, cfg)
	r, err := db.Get(context.Background(), "r_inflight")
	if err != nil {
		t.Fatal(err)
	}
	if r.State != protocol.StateSucceeded {
		t.Errorf("an in-flight run inside shutdown_grace finished %s (%s), want succeeded",
			r.State, r.ErrorCode)
	}
}

// Past shutdown_grace the run is signalled rather than waited on forever, and
// it must still end up terminal with its resources gone — §6 says containers
// and VMs are torn down "even on the kill path".
func TestShutdownGraceExpiryCancelsAndTearsDown(t *testing.T) {
	cfg := newRunnableConfig(t)
	ta := newTestAgent(t, cfg, func(o *Options) {
		o.ShutdownGrace = 200 * time.Millisecond
	})
	ta.exec.hold = 5 * time.Minute
	agentRun := ta.run(t)

	enqueueDirectly(t, cfg, "r_stuck", "main")
	waitState(t, cfg, "r_stuck", 60*time.Second, protocol.StateRunning)

	agentRun.stop()
	if err := agentRun.wait(t, 60*time.Second); err != nil {
		t.Fatalf("Run returned %v", err)
	}

	db := openStore(t, cfg)
	r, err := db.Get(context.Background(), "r_stuck")
	if err != nil {
		t.Fatal(err)
	}
	if !r.State.IsTerminal() {
		t.Errorf("a run signalled at grace expiry ended %s, want a terminal state", r.State)
	}
	if left := ta.exec.resources(t); len(left) != 0 {
		t.Errorf("shutdown left executor resources behind: %v", left)
	}
}

// --once is a debugging aid, and one that blocks forever on an empty queue is
// one nobody reaches for twice.
func TestOnceExitsWhenTheQueueIsEmpty(t *testing.T) {
	cfg := newConfigDir(t, configOptions{})
	ta := newTestAgent(t, cfg, func(o *Options) { o.Once = true })

	done := make(chan error, 1)
	go func() { done <- ta.Run(context.Background()) }()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("--once returned %v", err)
		}
	case <-time.After(60 * time.Second):
		t.Fatal("--once did not exit on an empty queue")
	}
}

// --once executes exactly one run and stops, so a machine can be debugged
// without it picking up the rest of the queue behind you.
func TestOnceExecutesASingleRun(t *testing.T) {
	cfg := newRunnableConfig(t)
	enqueueDirectly(t, cfg, "r_once1", "main")
	enqueueDirectly(t, cfg, "r_once2", "main")

	ta := newTestAgent(t, cfg, func(o *Options) { o.Once = true })
	done := make(chan error, 1)
	go func() { done <- ta.Run(context.Background()) }()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("--once returned %v", err)
		}
	case <-time.After(90 * time.Second):
		t.Fatal("--once did not finish")
	}

	db := openStore(t, cfg)
	finished := 0
	for _, id := range []string{"r_once1", "r_once2"} {
		r, err := db.Get(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		if r.State.IsTerminal() {
			finished++
		}
	}
	if finished != 1 {
		t.Errorf("%d of 2 queued runs finished, want exactly 1", finished)
	}
}
