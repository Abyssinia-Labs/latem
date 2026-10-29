package invariants

// INV-5 — The registry comes from disk, at the pinned commit.
//
// The policy half passes in internal/registry/ref_test.go: normalization
// before matching, `refs/heads/main` and `main` receiving the same answer, tags
// requiring an explicit entry, `refs/pull/*` failing by construction. None of
// it is repeated here.
//
// What this file adds is the two halves that need a machine:
//
//   - jobs.yaml is read from the checked-out workspace at the resolved commit,
//     and never from a broker or from anything ambient on the agent's disk;
//   - approval is a local action bound to a commit (amendment A27), so a ref
//     that moves after an operator says yes parks the run again instead of
//     building what arrived last.
//
// The second is the one worth the machinery. It is a time-of-check-to-time-of-
// use hole, so it is only observable in a test that actually moves the ref
// between the approval and the build.

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Abyssinia-Labs/latem/internal/agent"
	"github.com/Abyssinia-Labs/latem/internal/source"
	"github.com/Abyssinia-Labs/latem/internal/store"
	"github.com/Abyssinia-Labs/latem/pkg/protocol"
)

// v1Jobs is the registry at the repository's first commit.
const v1Jobs = `version: 1

jobs:
  build:
    runs_on: [test]
    timeout: 5m
    steps:
      - name: build
        run: ./ci/build.sh
`

// v2Jobs is the registry at the tip. `build` gained a different command and a
// second job appeared, so a run at the older commit and a run at the tip are
// distinguishable by what they execute rather than only by what they answer.
const v2Jobs = `version: 1

jobs:
  build:
    runs_on: [test]
    timeout: 5m
    steps:
      - name: build
        run: ./ci/v2-build.sh

  onlyv2:
    runs_on: [test]
    timeout: 5m
    steps:
      - name: v2
        run: ./ci/v2-only.sh
`

// ambientJobs is what an attacker, or an accident, would like the agent to
// read instead of the workspace's copy. Nothing may ever execute this.
const ambientJobs = `version: 1

jobs:
  build:
    runs_on: [test]
    timeout: 5m
    steps:
      - name: ambient
        run: ./ci/AMBIENT-REGISTRY.sh
  ambient:
    runs_on: [test]
    timeout: 5m
    steps:
      - name: ambient
        run: ./ci/AMBIENT-REGISTRY.sh
`

// TestINV5_RegistryComesFromTheWorkspaceAtTheResolvedCommit builds the same job
// name at two commits and checks that what ran differs.
//
// A run is supposed to be reproducible from the commit alone. The way that
// fails in practice is not a broker sending a job definition — INV-2 makes
// that unrepresentable — it is the agent reading a registry from somewhere
// whose contents are not pinned: the tip of the branch, a cached copy, its own
// config directory. Each of those produces a green test and a build of
// something nobody reviewed.
func TestINV5_RegistryComesFromTheWorkspaceAtTheResolvedCommit(t *testing.T) {
	m := newMachine(t, machineOptions{
		jobs:        v1Jobs,
		allowedRefs: `["main", "refs/tags/v1"]`,
	})

	// Tag the first commit, then move the branch on. The tag is now a commit
	// whose registry differs from the tip's.
	git(t, m.repo, "tag", "v1")
	writeFile(t, filepath.Join(m.repo, ".latem", "jobs.yaml"), v2Jobs)
	writeFile(t, filepath.Join(m.repo, "ci", "v2-build.sh"), "#!/bin/sh\necho v2\n")
	writeFile(t, filepath.Join(m.repo, "ci", "v2-only.sh"), "#!/bin/sh\necho v2only\n")
	git(t, m.repo, "add", "-A")
	git(t, m.repo, "commit", "-q", "-m", "rewrite the registry")

	// --- positive control: the tip's registry is real ----------------------
	if got := m.submit(t, runFrame("r_tip", "app", "onlyv2", "main", nil)); !got.OK {
		t.Fatalf("the tip run was refused: %s", got.Error)
	}
	tip := m.waitForState(t, "r_tip", 90*time.Second,
		protocol.StateSucceeded, protocol.StateFailed, protocol.StateRejected)
	if tip.State != protocol.StateSucceeded {
		t.Fatalf("a job declared at the tip did not run: %s (%s) %s",
			tip.State, tip.ErrorCode, tip.ErrorMsg)
	}
	if got := commandsFor(m, "r_tip"); len(got) != 1 || got[0] != "./ci/v2-only.sh" {
		t.Fatalf("the tip run executed %q, want [./ci/v2-only.sh]; the recorder is not watching "+
			"the execution path", got)
	}

	// --- the invariant: an older commit gets the older registry ------------
	if got := m.submit(t, runFrame("r_pinned", "app", "build", "v1", nil)); !got.OK {
		t.Fatalf("the tag run was refused: %s", got.Error)
	}
	pinned := m.waitForState(t, "r_pinned", 90*time.Second,
		protocol.StateSucceeded, protocol.StateFailed, protocol.StateRejected)
	if pinned.State != protocol.StateSucceeded {
		t.Fatalf("the tag run finished %s (%s): %s", pinned.State, pinned.ErrorCode, pinned.ErrorMsg)
	}
	if pinned.RefNormalized != "refs/tags/v1" {
		t.Errorf("ref_normalized = %q, want refs/tags/v1 (INV-5, amendment A21)", pinned.RefNormalized)
	}
	if pinned.SHA == "" || pinned.SHA == tip.SHA {
		t.Errorf("the tag run recorded sha %q and the tip run %q; they must differ or the fixture "+
			"is not testing two commits", pinned.SHA, tip.SHA)
	}
	got := commandsFor(m, "r_pinned")
	if len(got) != 1 || got[0] != "./ci/build.sh" {
		t.Errorf("the run at refs/tags/v1 executed %q, want [./ci/build.sh] — the registry at the "+
			"resolved commit, not the one at the tip (INV-5)", got)
	}

	// --- a job that does not exist at that commit is unknown there ---------
	if got := m.submit(t, runFrame("r_future", "app", "onlyv2", "v1", nil)); !got.OK {
		t.Fatalf("the frame was refused at the transport: %s", got.Error)
	}
	future := m.waitForState(t, "r_future", 90*time.Second,
		protocol.StateRejected, protocol.StateFailed, protocol.StateSucceeded)
	if future.State == protocol.StateSucceeded {
		t.Fatal("a job that does not exist at the resolved commit was built; the registry is not " +
			"coming from the workspace (INV-5)")
	}
	if future.ErrorCode != protocol.ErrUnknownJob {
		t.Errorf("error code = %q, want %q — the job exists at the tip and not at this commit, "+
			"which is exactly what gate 12 is for", future.ErrorCode, protocol.ErrUnknownJob)
	}
}

// TestINV5_TheRegistryIsNotReadFromTheAgentsOwnDisk plants a registry
// everywhere except the workspace.
//
// "Never from a broker" is enforced by INV-2 having no field to put one in, so
// it is unrepresentable rather than merely refused. The reachable version of
// the same mistake is an agent that reads a jobs.yaml from its config
// directory, its work root, or its mirror — all of which are on the machine,
// none of which is pinned to the commit, and each of which would make the run
// unreproducible in a way no error message would mention.
func TestINV5_TheRegistryIsNotReadFromTheAgentsOwnDisk(t *testing.T) {
	m := newMachine(t, machineOptions{jobs: v1Jobs})

	for _, dir := range []string{
		m.cfg.Paths.Dir,
		m.home,
		m.cfg.Paths.Work,
		m.cfg.Paths.Mirrors,
		filepath.Dir(m.cfg.Paths.Work),
	} {
		if dir == "" {
			continue
		}
		writeFile(t, filepath.Join(dir, ".latem", "jobs.yaml"), ambientJobs)
		writeFile(t, filepath.Join(dir, "jobs.yaml"), ambientJobs)
	}

	// The job name exists only in the planted copies.
	if got := m.submit(t, runFrame("r_ambient", "app", "ambient", "main", nil)); !got.OK {
		t.Fatalf("the frame was refused at the transport: %s", got.Error)
	}
	r := m.waitForState(t, "r_ambient", 90*time.Second,
		protocol.StateRejected, protocol.StateFailed, protocol.StateSucceeded)
	if r.State == protocol.StateSucceeded {
		t.Error("a job declared only in a jobs.yaml on the agent's own disk was built (INV-5)")
	}
	if r.ErrorCode != protocol.ErrUnknownJob {
		t.Errorf("error code = %q, want %q", r.ErrorCode, protocol.ErrUnknownJob)
	}

	// And the job name the workspace does declare must still run the
	// workspace's command, not the planted one.
	if got := m.submit(t, runFrame("r_shadow", "app", "build", "main", nil)); !got.OK {
		t.Fatalf("the frame was refused at the transport: %s", got.Error)
	}
	shadow := m.waitForState(t, "r_shadow", 90*time.Second,
		protocol.StateSucceeded, protocol.StateFailed, protocol.StateRejected)
	if shadow.State != protocol.StateSucceeded {
		t.Fatalf("the legitimate run finished %s (%s): %s",
			shadow.State, shadow.ErrorCode, shadow.ErrorMsg)
	}
	for _, c := range commandsFor(m, "r_shadow") {
		if strings.Contains(c, "AMBIENT-REGISTRY") {
			t.Errorf("the run executed %q, which comes from a jobs.yaml the agent found on its "+
				"own disk rather than in the checked-out workspace (INV-5)", c)
		}
	}
}

// TestINV5_ApprovalIsLocalOnlyAndPinnedToACommit is amendment A27 end to end.
//
// Five things have to hold together, and each of the first four is a way the
// mechanism has been got wrong somewhere:
//
//  1. a run on a ref outside the policy parks, and parks *with a commit* —
//     gate 4 cannot park because nothing has been fetched yet, so a run parked
//     without a sha would be one nothing could ever approve;
//  2. no inbound command can grant the approval, because there is no third
//     kind and there is deliberately not going to be one;
//  3. a local approval admits the run;
//  4. an approval names the commit, so a ref that moves afterwards parks the
//     run again with the new commit and discards the approval;
//  5. approving the commit that actually arrived then builds it.
//
// Without (4) the whole mechanism is decoration: push something benign, ask
// for approval, force-push, and the runner builds what arrived last.
func TestINV5_ApprovalIsLocalOnlyAndPinnedToACommit(t *testing.T) {
	m := newMachine(t, machineOptions{jobs: v1Jobs, allowedRefs: `["main"]`})

	// A branch a contributor controls, which is the threat INV-5 names.
	git(t, m.repo, "checkout", "-q", "-b", "feature/x")
	writeFile(t, filepath.Join(m.repo, "ci", "build.sh"), "#!/bin/sh\necho benign\n")
	git(t, m.repo, "add", "-A")
	git(t, m.repo, "commit", "-q", "-m", "the commit an operator will be shown")
	shaA := git(t, m.repo, "rev-parse", "HEAD")

	// --- 1. it parks, and it parks with a commit ---------------------------
	if got := m.submit(t, runFrame("r_park", "app", "build", "feature/x", nil)); !got.OK {
		t.Fatalf("the frame was refused at the transport: %s", got.Error)
	}
	parked := m.waitForState(t, "r_park", 90*time.Second,
		protocol.StateAwaitingApproval, protocol.StateRejected, protocol.StateSucceeded,
		protocol.StateFailed)
	if parked.State != protocol.StateAwaitingApproval {
		t.Fatalf("a ref outside allowed_refs finished %s (%s): %s, want awaiting_approval",
			parked.State, parked.ErrorCode, parked.ErrorMsg)
	}
	if parked.SHA != shaA {
		t.Fatalf("parked with sha %q, want %q — a run parked without the commit an operator is "+
			"being asked to approve is one nothing can ever approve (A27)", parked.SHA, shaA)
	}
	if len(commandsFor(m, "r_park")) != 0 {
		t.Fatal("a parked run executed something")
	}

	// --- 2. no inbound command can approve it ------------------------------
	for _, frame := range []string{
		`{"kind":"approve","approve":{"id":"r_park"}}`,
		`{"kind":"approve","run":{"id":"r_park","repo":"app","job":"build","ref":"feature/x",` +
			`"origin":{"kind":"cli","requested_at":"2026-01-01T00:00:00Z"}}}`,
		`{"kind":"run","run":{"id":"r_park","repo":"app","job":"build","ref":"feature/x",` +
			`"approved":true,"origin":{"kind":"cli","requested_at":"2026-01-01T00:00:00Z"}}}`,
		`{"kind":"run","run":{"id":"r_park","repo":"app","job":"build","ref":"feature/x",` +
			`"approved_sha":"` + shaA + `","origin":{"kind":"cli",` +
			`"requested_at":"2026-01-01T00:00:00Z"}}}`,
	} {
		if got := m.submit(t, frame); got.OK {
			t.Errorf("the agent accepted an inbound approval; INV-2 closes the command set and "+
				"INV-5 says approval is a local action only\nframe: %s", frame)
		}
	}
	// Re-submitting the same run id is idempotent by §7.1 and must not
	// launder the run out of awaiting_approval either.
	_ = m.submit(t, runFrame("r_park", "app", "build", "feature/x", nil))
	time.Sleep(200 * time.Millisecond)
	if r := m.get(t, "r_park"); r.State != protocol.StateAwaitingApproval {
		t.Fatalf("the run left awaiting_approval without a local approval: %s", r.State)
	}

	// --- 4. move the ref before approving ----------------------------------
	//
	// This is the force-push. The operator was shown shaA; shaB is what a
	// runner would build if the approval named the ref.
	writeFile(t, filepath.Join(m.repo, "ci", "build.sh"), "#!/bin/sh\necho SOMETHING ELSE\n")
	git(t, m.repo, "add", "-A")
	git(t, m.repo, "commit", "-q", "-m", "the commit nobody was shown")
	shaB := git(t, m.repo, "rev-parse", "HEAD")
	if shaA == shaB {
		t.Fatal("the fixture did not move the ref")
	}

	// --- 3. a local approval, of shaA ---------------------------------------
	approved, err := agent.Approve(context.Background(), m.db, "r_park",
		m.cfg.Agent.QueueTTL.Duration(), time.Now())
	if err != nil {
		t.Fatalf("latem approve: %v", err)
	}
	if approved.ApprovedSHA != shaA {
		t.Fatalf("approved_sha = %q, want %q — approval names a commit, not a ref (A27)",
			approved.ApprovedSHA, shaA)
	}
	if approved.State != protocol.StateQueued {
		t.Fatalf("an approved run is %s, want queued", approved.State)
	}

	// --- the invariant: it parks again, at the commit that actually arrived --
	reparked := m.waitForStateSHA(t, "r_park", shaB, 90*time.Second)
	if reparked.State != protocol.StateAwaitingApproval {
		t.Fatalf("after the ref moved, the run is %s (%s): %s — an approval spent on a commit "+
			"nobody looked at is the hole INV-5 exists to close",
			reparked.State, reparked.ErrorCode, reparked.ErrorMsg)
	}
	if reparked.ApprovedAt != nil || reparked.ApprovedSHA != "" {
		t.Errorf("the stale approval survived the re-park: approved_at=%v approved_sha=%q",
			reparked.ApprovedAt, reparked.ApprovedSHA)
	}
	if !strings.Contains(strings.ToLower(reparked.ErrorMsg), "moved") {
		t.Errorf("the message does not say the ref moved, which is the one thing the operator "+
			"needs to know: %q", reparked.ErrorMsg)
	}
	if got := commandsFor(m, "r_park"); len(got) != 0 {
		t.Fatalf("the run built %q on an approval that named a different commit (INV-5)", got)
	}

	// --- 5. approving what arrived builds it --------------------------------
	again, err := agent.Approve(context.Background(), m.db, "r_park",
		m.cfg.Agent.QueueTTL.Duration(), time.Now())
	if err != nil {
		t.Fatalf("second approval: %v", err)
	}
	if again.ApprovedSHA != shaB {
		t.Fatalf("approved_sha = %q, want %q", again.ApprovedSHA, shaB)
	}
	final := m.waitForState(t, "r_park", 90*time.Second,
		protocol.StateSucceeded, protocol.StateFailed, protocol.StateRejected)
	if final.State != protocol.StateSucceeded {
		t.Fatalf("the approved run finished %s (%s): %s", final.State, final.ErrorCode, final.ErrorMsg)
	}
	if final.SHA != shaB {
		t.Errorf("built %s, approved %s", final.SHA, shaB)
	}
	if got := commandsFor(m, "r_park"); len(got) != 1 || got[0] != "./ci/build.sh" {
		t.Errorf("the approved run executed %q, want [./ci/build.sh]", got)
	}
}

// TestINV5_ApproveRefusesARunWithNoCommit is A27's other consequence.
//
// In practice such a row comes from an agent older than the amendment. There
// is no safe reading of the approval, because the only commit it could name is
// "whatever the ref points at by the time a runner picks this up" — which is
// the value the invariant exists to distrust.
func TestINV5_ApproveRefusesARunWithNoCommit(t *testing.T) {
	m := newMachine(t, machineOptions{jobs: v1Jobs})

	ctx := context.Background()
	row := store.Run{
		ID: "r_nocommit", Repo: "app", Job: "build", Ref: "feature/x",
		RefNormalized: "refs/heads/feature/x",
		State:         protocol.StateAwaitingApproval,
		OriginKind:    "cli", CreatedAt: time.Now(),
	}
	if err := m.db.Create(ctx, row); err != nil {
		t.Fatal(err)
	}

	_, err := agent.Approve(ctx, m.db, "r_nocommit", time.Hour, time.Now())
	if !errors.Is(err, agent.ErrNoCommit) {
		t.Fatalf("approving a run with no sha returned %v, want ErrNoCommit", err)
	}

	// Control: the same call against a row that does have one succeeds, so the
	// refusal above is about the missing commit and not about the harness.
	row.ID = "r_hascommit"
	row.SHA = strings.Repeat("a", 40)
	if err := m.db.Create(ctx, row); err != nil {
		t.Fatal(err)
	}
	if _, err := agent.Approve(ctx, m.db, "r_hascommit", time.Hour, time.Now()); err != nil {
		t.Fatalf("approving a run that does have a commit failed: %v", err)
	}
}

// ---------------------------------------------------------------------------
// helpers

// commandsFor returns what one run was asked to execute, in order.
func commandsFor(m *machine, runID string) []string {
	var out []string
	for _, s := range m.exec.recorded() {
		if s.RunID == runID {
			out = append(out, s.Step.Run)
		}
	}
	return out
}

// waitForStateSHA blocks until a run is parked at a particular commit.
//
// Waiting on the state alone would race: the run is already in
// awaiting_approval when the approval is granted, so "it is parked" is true
// before the agent has done anything, and a test that checked only the state
// would pass without the second gate-8 pass ever running.
func (m *machine) waitForStateSHA(t *testing.T, id, sha string, timeout time.Duration) store.Run {
	t.Helper()
	var last store.Run
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		r, err := m.db.Get(context.Background(), id)
		if err == nil {
			last = r
			if r.SHA == sha && r.State != protocol.StateQueued && r.State != protocol.StateClaimed &&
				r.State != protocol.StatePreparing {
				return r
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("run %s never settled at sha %s; it is %s at %s (%s: %s)",
		id, sha, last.State, last.SHA, last.ErrorCode, last.ErrorMsg)
	return last
}

// checkoutWorkflow has two jobs that move the working tree (A53): one to a
// tag in main's history, which the policy already trusts, and one to a branch
// it does not name.
const checkoutWorkflow = `on: [push]
jobs:
  rollback:
    runs-on: [test]
    steps:
      - uses: actions/checkout@v5
        with:
          ref: v1
      - run: ./ci/build.sh
  sneak:
    runs-on: [test]
    steps:
      - uses: actions/checkout@v5
        with:
          ref: feature/x
      - run: ./ci/build.sh
`

// TestINV5_ACheckoutReachesOnlyWhatThePolicyAdmits is A53's half of INV-5,
// against a real mirror. A checkout may move the working tree to a commit in
// the history of a ref allowed_refs admits — a rollback tag on main — and the
// job it runs is still the run's commit's. It may not move it to a commit
// only an unadmitted branch holds: the job fails before anything of it runs.
func TestINV5_ACheckoutReachesOnlyWhatThePolicyAdmits(t *testing.T) {
	m := newMachine(t, machineOptions{allowedRefs: `["main"]`})
	base := git(t, m.repo, "rev-parse", "HEAD")
	git(t, m.repo, "tag", "v1")
	writeFile(t, filepath.Join(m.repo, ".github", "workflows", "deploy.yml"), checkoutWorkflow)
	git(t, m.repo, "add", "-A")
	git(t, m.repo, "commit", "-q", "-m", "a workflow that checks out elsewhere")
	tip := git(t, m.repo, "rev-parse", "HEAD")
	git(t, m.repo, "checkout", "-q", "-b", "feature/x")
	writeFile(t, filepath.Join(m.repo, "ci", "build.sh"), "#!/bin/sh\necho unreviewed\n")
	git(t, m.repo, "commit", "-q", "-am", "code main has not seen")
	git(t, m.repo, "checkout", "-q", "main")

	// --- positive control: a tag in main's history is reachable ------------
	if got := m.submit(t, runFrame("r_rollback", "app", "rollback", "main", nil)); !got.OK {
		t.Fatalf("refused at the transport: %s", got.Error)
	}
	rb := m.waitForState(t, "r_rollback", 90*time.Second,
		protocol.StateSucceeded, protocol.StateFailed, protocol.StateRejected)
	if rb.State != protocol.StateSucceeded {
		t.Fatalf("the rollback finished %s (%s): %s", rb.State, rb.ErrorCode, rb.ErrorMsg)
	}
	if rb.SHA != tip {
		t.Errorf("the run records %s, want the run's own commit %s: a checkout moves the tree, not the run", rb.SHA, tip)
	}
	var switched string
	for _, s := range m.exec.recorded() {
		if s.RunID == "r_rollback" && s.Step.Run == source.SwitchScript {
			switched = s.Env["LATEM_CHECKOUT_SHA"]
		}
	}
	if switched != base {
		t.Errorf("the checkout switched to %q, want v1's commit %s", switched, base)
	}
	if got := commandsFor(m, "r_rollback"); len(got) != 2 || got[1] != "./ci/build.sh" {
		t.Errorf("the rollback ran %q; its steps are the run's commit's", got)
	}

	// --- the invariant: a commit the policy does not trust is refused ------
	if got := m.submit(t, runFrame("r_sneak", "app", "sneak", "main", nil)); !got.OK {
		t.Fatalf("refused at the transport: %s", got.Error)
	}
	sn := m.waitForState(t, "r_sneak", 90*time.Second,
		protocol.StateSucceeded, protocol.StateFailed, protocol.StateRejected)
	if sn.State != protocol.StateFailed || sn.ErrorCode != protocol.ErrStep {
		t.Fatalf("the checkout of feature/x finished %s (%s): %s; want failed/E_STEP (INV-5, A53)",
			sn.State, sn.ErrorCode, sn.ErrorMsg)
	}
	if !strings.Contains(sn.ErrorMsg, "allowed_refs") {
		t.Errorf("the refusal %q does not say which policy refused it", sn.ErrorMsg)
	}
	if got := commandsFor(m, "r_sneak"); len(got) != 0 {
		t.Errorf("a refused checkout still ran %q", got)
	}
}
