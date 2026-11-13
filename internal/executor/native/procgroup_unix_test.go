//go:build unix

package native

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"

	"github.com/Abyssinia-Labs/latem/internal/executor"
)

// These tests kill real process groups. They need bash, `sleep` and a writable
// directory — no daemon, no Xcode, no privileges — so they belong in the default
// gate: a guard nobody has executed is a guard nobody has tested.

// crashedAgentEnv turns the test binary into the agent that gets killed. See
// TestReap_AfterTheAgentIsKilled.
const crashedAgentEnv = "LATEM_TEST_CRASHED_AGENT"

// crashedRunID is the run the killed agent was in the middle of.
const crashedRunID = "run-crashed"

// crashedAgentStep leaves a child running in the step's group and then blocks.
// The `sleep` stands in for the `xcodebuild` §13.3 is actually worried about:
// a process the step spawned, which nothing links back to the run once the
// agent is gone.
const crashedAgentStep = `sleep 300 & echo $! > child.pid; sleep 300`

// TestHelperCrashedAgent is not a test. It is a whole agent — a real
// [Executor], a real Prepare, a real step — running in a process the parent test
// kills with SIGKILL while the step is in flight. Nothing else reproduces the
// defect honestly: within one process the record's descriptor is still open, so
// the liveness proof would be the test's own, and the interesting claim is
// exactly that the proof outlives the process that took it.
func TestHelperCrashedAgent(t *testing.T) {
	dir := os.Getenv(crashedAgentEnv)
	if dir == "" {
		t.Skip("helper process for TestReap_AfterTheAgentIsKilled")
	}

	e, err := New(Options{EnvFileDir: filepath.Join(dir, "env")})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	env, err := e.Prepare(t.Context(), executor.Spec{
		RunID:     crashedRunID,
		Workspace: filepath.Join(dir, "ws"),
	})
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	// Blocks for five minutes. The parent kills this process long before that,
	// which is the whole point: no Close, no deferred release, no chance to
	// stop the group the way a live agent would.
	_, _ = env.Exec(t.Context(), executor.Step{Name: "never finishes", Run: crashedAgentStep}, nil, nil)
	t.Fatal("the helper agent was supposed to be killed while its step was running")
}

// The defect, end to end: `kill -9` on the agent orphans the step's process
// group, §6 step 5 previously covered containers, VMs and workspaces only, and
// nothing ever collected it. An abandoned `xcodebuild` holds DerivedData and a
// keychain handle and competes with the next build for the machine.
func TestReap_AfterTheAgentIsKilled(t *testing.T) {
	requireShell(t)

	dir := t.TempDir()
	root, ws := filepath.Join(dir, "env"), filepath.Join(dir, "ws")
	for _, d := range []string{root, ws} {
		if err := os.MkdirAll(d, envDirMode); err != nil {
			t.Fatal(err)
		}
	}

	// A file rather than a buffer: the helper's output is read after it has been
	// killed, and os/exec hands a *os.File straight to the child instead of
	// copying it through a goroutine this test would then be racing.
	logPath := filepath.Join(dir, "agent.log")
	agentLog, err := os.Create(logPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = agentLog.Close() }()
	t.Cleanup(func() {
		if !t.Failed() {
			return
		}
		if out, err := os.ReadFile(logPath); err == nil && len(out) > 0 {
			t.Logf("the killed agent said:\n%s", out)
		}
	})

	// #nosec G204 -- os.Args[0] is this test binary and the flags are literals.
	agent := exec.Command(os.Args[0], "-test.run=^TestHelperCrashedAgent$", "-test.timeout=3m")
	agent.Env = append(os.Environ(), crashedAgentEnv+"="+dir)
	agent.Stdout, agent.Stderr = agentLog, agentLog
	if err := agent.Start(); err != nil {
		t.Fatalf("starting the agent that will be killed: %v", err)
	}
	t.Cleanup(func() { _ = agent.Process.Kill() })
	agentDone := make(chan struct{})
	go func() { _, _ = agent.Process.Wait(); close(agentDone) }()

	pidFile := filepath.Join(ws, "child.pid")
	child := waitForPID(t, pidFile)
	envDir := filepath.Join(root, EnvDirPrefix+crashedRunID)
	pgid := recordedGroup(t, envDir)
	t.Cleanup(func() { _ = syscall.Kill(-pgid, syscall.SIGKILL) })

	// §11's E_INTERRUPTED, delivered the way an operator's `killall -9` does.
	if err := agent.Process.Signal(syscall.SIGKILL); err != nil {
		t.Fatalf("killing the agent: %v", err)
	}
	<-agentDone

	// Before reaping: the orphan is real. It is still running, it is not this
	// process's child, and its parent is gone, so nothing on this machine will
	// ever collect it.
	if err := syscall.Kill(child, 0); err != nil {
		t.Fatalf("the step's child did not survive the agent, so there is no leak to reap: %v", err)
	}
	if err := syscall.Kill(pgid, 0); err != nil {
		t.Fatalf("the step's shell did not survive the agent: %v", err)
	}

	// The new agent starts and reconciles (§6 step 5) with nothing live.
	reaper := newExecutor(t, Options{EnvFileDir: root, ReapGrace: 5 * time.Second})
	got, err := reaper.Reap(t.Context(), nil)
	if err != nil {
		t.Fatalf("Reap: %v", err)
	}

	want := executor.Reaped{RunID: crashedRunID, Kind: ReapedProcessGroup, Ref: strconv.Itoa(pgid)}
	if !hasReaped(got, want) {
		t.Errorf("Reaped = %+v, want it to name the process group as %+v — a reap that says nothing "+
			"is indistinguishable from a reap that found nothing", got, want)
	}
	if alive := waitForExit(child, 10*time.Second); alive {
		t.Errorf("child %d outlived the reap: the kill did not reach the whole group", child)
	}
	if alive := waitForExit(pgid, 10*time.Second); alive {
		t.Errorf("the step's shell %d outlived the reap", pgid)
	}
	if _, err := os.Stat(envDir); !os.IsNotExist(err) {
		t.Errorf("the $LATEM_ENV directory survived the reap: %v", err)
	}
}

// The load-bearing property, isolated: the record's lock is held by the step's
// tree, not by the agent, and not by the group leader either. Everything the
// reaper concludes rests on this.
func TestGroupRecord_OutlivesTheAgentAndTheLeader(t *testing.T) {
	requireShell(t)
	dir := t.TempDir()
	ws := t.TempDir()

	// #nosec G204 -- a literal command in a test.
	cmd := exec.Command(DefaultShell, "-c", crashedAgentStep)
	cmd.Dir = ws
	setProcessGroup(cmd)
	g, err := newStepGroup(dir, cmd)
	if err != nil {
		t.Fatalf("newStepGroup: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pgid := processGroupOf(cmd)
	if err := g.note(pgid); err != nil {
		t.Fatalf("note: %v", err)
	}
	t.Cleanup(func() { _ = syscall.Kill(-pgid, syscall.SIGKILL) })
	leaderDone := make(chan struct{})
	go func() { _ = cmd.Wait(); close(leaderDone) }()

	child := waitForPID(t, filepath.Join(ws, "child.pid"))

	if got := recordedGroup(t, dir); got != pgid {
		t.Fatalf("the record names group %d, want %d", got, pgid)
	}
	assertHeld(t, g.path, true, "while the step is running")

	// The agent dies: its own descriptor goes with it. The step's tree keeps the
	// open file description, and a BSD flock lives on the description.
	if err := g.file.Close(); err != nil {
		t.Fatal(err)
	}
	assertHeld(t, g.path, true, "after the agent that took the lock is gone")

	// The group leader dies, which is what makes a pgid reusable. The child it
	// backgrounded is still in the group and still holds the descriptor.
	if err := syscall.Kill(pgid, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	<-leaderDone
	assertHeld(t, g.path, true, "after the group leader is gone but its child is not")

	// Nothing from the step is left. This is the state in which a reaper must
	// refuse to signal, because the number no longer belongs to anyone.
	if err := syscall.Kill(-pgid, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	if alive := waitForExit(child, 10*time.Second); alive {
		t.Fatalf("child %d survived SIGKILL to its group", child)
	}
	// The step left two processes behind, not one: the sleep it backgrounded,
	// which child.pid names, and the sleep its shell was waiting on. Both
	// inherited the descriptor, and SIGKILL reaches each in its own time, so
	// the first being gone says nothing about the second. Until this waited
	// for the group, the assertion below was a race the second sleep usually
	// lost: it failed on a loaded macOS runner, and 1 time in 300 here under
	// load, with "groupRecordHeld = true".
	if !waitForGroupGone(pgid, 10*time.Second) {
		t.Fatalf("process group %d still has a member after SIGKILL", pgid)
	}
	assertHeld(t, g.path, false, "once every process from the step is gone")
}

// waitForGroupGone waits until no process is left in the group, and reports
// whether that happened in time. A process's descriptors are closed before it
// is a zombie, and a zombie is still in its group, so an empty group has
// released everything it held.
func waitForGroupGone(pgid int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if pgidGone(pgid) {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return pgidGone(pgid)
}

// The pid reuse defence, from the wrong end: a record naming a live process
// group that nobody from the step holds must be left completely alone. This is
// the state a stale record is in after the kernel has handed its number to
// something else.
func TestReap_LeavesAGroupNoRecordHolderVouchesForAlone(t *testing.T) {
	requireShell(t)
	root := t.TempDir()

	// #nosec G204 -- a literal command in a test.
	bystander := exec.Command(DefaultShell, "-c", "sleep 30")
	setProcessGroup(bystander)
	if err := bystander.Start(); err != nil {
		t.Fatal(err)
	}
	pgid := processGroupOf(bystander)
	exited := make(chan struct{})
	go func() { _ = bystander.Wait(); close(exited) }()
	t.Cleanup(func() { _ = syscall.Kill(-pgid, syscall.SIGKILL) })

	// Written by hand, exactly as a crashed agent's record would look after the
	// kernel recycled the number.
	writeRecord(t, filepath.Join(root, EnvDirPrefix+"run-stale"), "stale", strconv.Itoa(pgid)+"\n")

	e := newExecutor(t, Options{EnvFileDir: root, ReapGrace: 200 * time.Millisecond})
	got, err := e.Reap(t.Context(), nil)
	if err != nil {
		t.Fatalf("Reap: %v", err)
	}
	for _, r := range got {
		if r.Kind == ReapedProcessGroup {
			t.Errorf("Reap signalled %+v from a record nothing was holding", r)
		}
	}

	select {
	case <-exited:
		t.Fatal("the reap killed an unrelated process group; a recycled pid is all it would take")
	case <-time.After(300 * time.Millisecond):
	}
	if _, err := os.Stat(filepath.Join(root, EnvDirPrefix+"run-stale")); !os.IsNotExist(err) {
		t.Errorf("a stale record's directory was not cleaned up: %v", err)
	}
}

// A record naming a pgid that has gone entirely: nothing to signal, nothing to
// report, no error, and no directory left behind.
func TestReap_ARecordNamingAGroupThatIsGone(t *testing.T) {
	requireShell(t)
	root := t.TempDir()

	// #nosec G204 -- a literal command in a test.
	dead := exec.Command(DefaultShell, "-c", "exit 0")
	setProcessGroup(dead)
	if err := dead.Run(); err != nil {
		t.Fatal(err)
	}
	gone := processGroupOf(dead)

	dir := filepath.Join(root, EnvDirPrefix+"run-old")
	writeRecord(t, dir, "old", strconv.Itoa(gone)+"\n")

	e := newExecutor(t, Options{EnvFileDir: root, ReapGrace: 200 * time.Millisecond})
	got, err := e.Reap(t.Context(), nil)
	if err != nil {
		t.Fatalf("Reap over a record whose group is long gone: %v", err)
	}
	for _, r := range got {
		if r.Kind == ReapedProcessGroup {
			t.Errorf("Reap reported %+v for a group that no longer exists", r)
		}
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("the directory was not removed: %v", err)
	}
}

// The guard that has no second chance. A pgid of 0 is the caller's own group —
// for the agent, the agent — and 1 is launchd's. Neither is recoverable, and
// both are one bad record away.
func TestSignalPgid_RefusesTheNumbersThatWouldTakeOutTheAgent(t *testing.T) {
	for _, pgid := range []int{0, 1, -1, -4242} {
		if err := signalPgid(pgid, syscall.SIGKILL); !errors.Is(err, errRefusedPgid) {
			t.Errorf("signalPgid(%d) = %v, want %v", pgid, err, errRefusedPgid)
		}
	}

	// The one that a stale record could genuinely name: the number the kernel
	// has since handed back to this very process. If this guard is ever removed,
	// this call is the one that kills the test run.
	own := syscall.Getpgrp()
	if err := signalPgid(own, syscall.SIGKILL); !errors.Is(err, errRefusedPgid) {
		t.Errorf("signalPgid(%d) on the agent's own group = %v, want %v", own, err, errRefusedPgid)
	}
	if err := stopGroup(own, 0); !errors.Is(err, errRefusedPgid) {
		t.Errorf("stopGroup(%d) on the agent's own group = %v, want %v", own, err, errRefusedPgid)
	}
	if !pgidGone(0) || !pgidGone(1) {
		t.Error("pgidGone must report the numbers it refuses to signal as gone, or stopGroup would wait on them")
	}
}

// The same guard reached the way a real reap would reach it: through a record
// on disk that a live holder vouches for, with the agent's own group number in
// it. The refusal is the only thing between §6 step 5 and the agent killing
// itself at startup.
func TestReap_RefusesARecordNamingTheAgentsOwnGroup(t *testing.T) {
	requireShell(t)
	root := t.TempDir()
	dir := filepath.Join(root, EnvDirPrefix+"run-forged")
	if err := os.MkdirAll(dir, envDirMode); err != nil {
		t.Fatal(err)
	}

	// A genuine holder, so the record passes the liveness proof and the number
	// check is all that is left.
	// #nosec G204 -- a literal command in a test.
	holder := exec.Command(DefaultShell, "-c", "sleep 30")
	setProcessGroup(holder)
	g, err := newStepGroup(dir, holder)
	if err != nil {
		t.Fatal(err)
	}
	if err := holder.Start(); err != nil {
		t.Fatal(err)
	}
	go func() { _ = holder.Wait() }()
	t.Cleanup(func() { _ = syscall.Kill(-processGroupOf(holder), syscall.SIGKILL) })
	if err := os.WriteFile(g.path, []byte(strconv.Itoa(syscall.Getpgrp())+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	assertHeld(t, g.path, true, "with a live holder")

	e := newExecutor(t, Options{EnvFileDir: root, ReapGrace: 100 * time.Millisecond})
	_, err = e.Reap(t.Context(), nil)

	// Reaching this line at all is most of the assertion.
	if !errors.Is(err, errRefusedPgid) {
		t.Fatalf("Reap over a record naming the agent's own group = %v, want %v", err, errRefusedPgid)
	}
}

// SIGTERM first so a build tool can flush its logs and drop its lock files; a
// group that ignores it must still die, and within the reap's own grace rather
// than the step's thirty seconds.
func TestReap_EscalatesToKill(t *testing.T) {
	requireShell(t)
	const grace = 400 * time.Millisecond
	root := t.TempDir()
	ws := t.TempDir()
	dir := filepath.Join(root, EnvDirPrefix+"run-stubborn")
	if err := os.MkdirAll(dir, envDirMode); err != nil {
		t.Fatal(err)
	}

	// #nosec G204 -- a literal command in a test.
	cmd := exec.Command(DefaultShell, "-c", `trap "" TERM; touch trapped; while :; do :; done`)
	cmd.Dir = ws
	setProcessGroup(cmd)
	g, err := newStepGroup(dir, cmd)
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pgid := processGroupOf(cmd)
	if err := g.note(pgid); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = syscall.Kill(-pgid, syscall.SIGKILL) })
	exited := make(chan struct{})
	go func() { _ = cmd.Wait(); close(exited) }()

	waitForFile(t, filepath.Join(ws, "trapped"))
	// The agent's own descriptor goes; the step's tree keeps the lock, which is
	// the state a crash leaves behind.
	if err := g.file.Close(); err != nil {
		t.Fatal(err)
	}

	e := newExecutor(t, Options{EnvFileDir: root, ReapGrace: grace})
	start := time.Now()
	got, err := e.Reap(t.Context(), nil)
	if err != nil {
		t.Fatalf("Reap: %v", err)
	}
	if !hasReaped(got, executor.Reaped{RunID: "run-stubborn", Kind: ReapedProcessGroup, Ref: strconv.Itoa(pgid)}) {
		t.Errorf("Reaped = %+v, want the stubborn group", got)
	}

	select {
	case <-exited:
	case <-time.After(15 * time.Second):
		t.Fatal("a group ignoring SIGTERM was never escalated to SIGKILL")
	}
	if elapsed := time.Since(start); elapsed < grace {
		t.Errorf("the group died after %s, sooner than the %s grace; SIGTERM appears to have been "+
			"honoured, so this proved nothing about the escalation", elapsed, grace)
	}
}

// A step that finished leaves nothing for a reap to act on. Without this, every
// interrupted run would offer the reaper a number for a group that had already
// been dealt with — and eventually a number the kernel had handed to somebody
// else.
func TestExec_NormalCompletionLeavesNoRecord(t *testing.T) {
	requireShell(t)
	root := t.TempDir()
	ws := t.TempDir()
	e := newExecutor(t, Options{EnvFileDir: root})
	env, err := e.Prepare(t.Context(), executor.Spec{RunID: "run-tidy", Workspace: ws})
	if err != nil {
		t.Fatal(err)
	}

	for _, step := range []executor.Step{
		{Name: "succeeds", Run: "echo ok"},
		{Name: "fails", Run: "exit 3"},
		{Name: "leaves a daemon", Run: `sleep 30 >/dev/null 2>&1 & echo $! > daemon.pid`},
	} {
		if _, err := env.Exec(t.Context(), step, nil, nil); err != nil {
			t.Fatalf("Exec(%q): %v", step.Name, err)
		}
	}
	daemon := readPID(t, filepath.Join(ws, "daemon.pid"))
	if daemon == 0 {
		t.Fatal("the third step did not record the daemon it left behind")
	}
	t.Cleanup(func() { _ = syscall.Kill(daemon, syscall.SIGKILL) })

	dir := filepath.Join(root, EnvDirPrefix+"run-tidy")
	if recs := groupRecords(t, dir); len(recs) != 0 {
		t.Errorf("finished steps left %v behind", recs)
	}

	// So a reap of this run — the agent died between steps — finds only the
	// directory, which is all §6 step 5 had to collect before process groups
	// were recorded at all.
	got, err := e.Reap(t.Context(), nil)
	if err != nil {
		t.Fatalf("Reap: %v", err)
	}
	for _, r := range got {
		if r.Kind == ReapedProcessGroup {
			t.Errorf("Reap signalled %+v after every step had finished", r)
		}
	}
	if !hasReaped(got, executor.Reaped{RunID: "run-tidy", Kind: "workspace", Ref: EnvDirPrefix + "run-tidy"}) {
		t.Errorf("Reaped = %+v, want the $LATEM_ENV directory", got)
	}

	// And the daemon that a *completed* step deliberately left running is still
	// running. That is the same limitation [runEnv.Close] documents — on a host
	// that is not ephemeral, a warm Gradle daemon or Metro bundler is the point
	// of §14 — and a reap that reached it would make the next run as cold as the
	// first.
	if err := syscall.Kill(daemon, 0); err != nil {
		t.Errorf("the daemon %d a finished step left behind was killed by the reap: %v", daemon, err)
	}
	if err := env.Close(t.Context()); err != nil {
		t.Errorf("Close after a reap removed the directory: %v", err)
	}
}

// A step that is running has exactly one record, and it names the group the
// step's shell leads.
func TestExec_RecordsTheRunningStepsGroup(t *testing.T) {
	env, ws := live(t, executor.Spec{RunID: "run-live"}, Options{TerminateGrace: 300 * time.Millisecond})

	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = env.Exec(context.WithoutCancel(t.Context()),
			executor.Step{Name: "running", Run: crashedAgentStep}, nil, nil)
	}()

	waitForFile(t, filepath.Join(ws, "child.pid"))
	pgid := recordedGroup(t, env.envDir)
	if got, err := syscall.Getpgid(pgid); err != nil || got != pgid {
		t.Errorf("the recorded number %d is not a process group leader (getpgid = %d, %v)", pgid, got, err)
	}

	if err := env.Close(t.Context()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("Exec did not return after Close")
	}
}

// A record the reaper cannot even open proves nothing, so it is reported and
// left alone rather than guessed at.
func TestReapGroups_ARecordItCannotRead(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root can read a mode 000 file, so there is nothing to refuse")
	}
	dir := t.TempDir()
	path := writeRecord(t, dir, "unreadable", "4242\n")
	if err := os.Chmod(path, 0o000); err != nil {
		t.Fatal(err)
	}

	killed, err := reapGroups(dir, 0)
	if err == nil {
		t.Fatal("reapGroups reported success over a record it could not read")
	}
	if len(killed) != 0 {
		t.Errorf("reapGroups signalled %v from a record it could not read", killed)
	}
}

// groupRecordHeld's own failure to open is separate from readGroupRecord's: it
// happens after the number has been read, and a reaper that treated it as "not
// held" would silently stop reaping.
func TestGroupRecordHeld_MissingRecord(t *testing.T) {
	held, err := groupRecordHeld(filepath.Join(t.TempDir(), "pgroup-gone"))
	if err == nil {
		t.Fatal("groupRecordHeld reported success on a record that does not exist")
	}
	if held {
		t.Error("groupRecordHeld reported a missing record as held")
	}
}

func TestProcessGroupOf_NotStarted(t *testing.T) {
	if got := processGroupOf(nil); got != 0 {
		t.Errorf("processGroupOf(nil) = %d, want 0", got)
	}
	if got := processGroupOf(&exec.Cmd{}); got != 0 {
		t.Errorf("processGroupOf on an unstarted command = %d, want 0 — a number here would be recorded", got)
	}
}

// recordedGroup reads the one process group record in a run's $LATEM_ENV
// directory.
func recordedGroup(t *testing.T, dir string) int {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); {
		recs := groupRecords(t, dir)
		if len(recs) == 1 {
			if pgid, err := readGroupRecord(recs[0]); err == nil && pgid != 0 {
				return pgid
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("no process group was recorded in %s: %v", dir, groupRecords(t, dir))
	return 0
}

func assertHeld(t *testing.T, path string, want bool, when string) {
	t.Helper()
	got, err := groupRecordHeld(path)
	if err != nil {
		t.Fatalf("groupRecordHeld %s: %v", when, err)
	}
	if got != want {
		t.Errorf("groupRecordHeld = %v %s, want %v", got, when, want)
	}
}

func hasReaped(got []executor.Reaped, want executor.Reaped) bool {
	for _, r := range got {
		if r == want {
			return true
		}
	}
	return false
}
