package native

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/Abyssinia-Labs/latem/pkg/protocol"
)

const (
	// GroupRecordPrefix begins the file that names one running step's process
	// group, inside that run's $LATEM_ENV directory. Exported so an operator
	// staring at a leftover directory, and `latem doctor`, can recognise it.
	GroupRecordPrefix = "pgroup-"

	// ReapedProcessGroup is the [executor.Reaped] Kind for an abandoned step
	// process group.
	//
	// The interface documents Kind as "container", "vm", or "workspace",
	// because those were the three things §6 step 5 knew how to leak when it
	// was written. A native step's process group is a fourth: on this executor
	// there is no container whose removal takes the processes with it, so the
	// group *is* the resource, and reporting it as a "workspace" would put an
	// operator-visible kill under a name that means an empty directory. The
	// agent's reconciliation log prints Kind and Ref verbatim
	// (agent/reconcile.go), so a new value names the thing without touching the
	// frozen interface.
	ReapedProcessGroup = "process-group"

	// ReapTerminateGrace is how long an abandoned process group has between
	// SIGTERM and SIGKILL during [Executor.Reap].
	//
	// Far shorter than [TerminateGrace], and the difference is about who is
	// waiting rather than about what is being killed. A step timeout stops a
	// build somebody is watching, and thirty seconds there buy a clean shutdown
	// whose absence the *next* run would pay for. A reap stops a group whose run
	// died with the agent: nothing will read its log, its workspace is about to
	// be removed anyway, and §6 step 5 runs before the runner accepts any work,
	// so every second spent here is a second the runner is unavailable. The wait
	// ends the moment the group is empty ([stopGroup]), so the full grace is
	// only ever paid by something that ignored SIGTERM.
	ReapTerminateGrace = 5 * time.Second

	// groupPollInterval is how often [stopGroup] asks whether the group it has
	// signalled is empty yet. Short enough that the common case — a `sleep` or a
	// compiler that dies on SIGTERM immediately — costs a poll rather than a
	// grace period.
	groupPollInterval = 50 * time.Millisecond

	// maxGroupRecordBytes bounds what is read back from a record. A record holds
	// one decimal integer; the cap is here because the file lives in a directory
	// a job can write to (see the package doc on isolation) and a reaper must
	// not be made to read a gigabyte of it.
	maxGroupRecordBytes = 64
)

// errRefusedPgid is returned instead of signalling a process group whose number
// could not be trusted. It is deliberately loud: reaching it means either a
// corrupt record or a forged one, and both are worth an operator's attention.
var errRefusedPgid = errors.New("refused to signal a process group")

// stepGroup is the on-disk record of one running step's process group, and the
// proof that the number in it still means what it meant when it was written.
//
// # Why a record exists at all
//
// [runEnv.Close] and a step timeout both stop the step's group through the live
// [exec.Cmd], which covers every way a run can end while the agent is alive. It
// covers nothing when the agent is not. `kill -9` on the agent leaves the step's
// `bash -lc`, and everything it started, reparented to launchd and running: a
// `sleep 300` survives, and so does the `xcodebuild` it stands in for — still
// holding DerivedData, still holding a keychain handle, still burning cores
// against the next build. RUNNER_SPEC §6 step 5 reconciles containers, VMs and
// workspaces after such a crash, and on this executor there is no container to
// remove, so the process group *is* the resource. Something the next agent can
// read has to name it, and the run's $LATEM_ENV directory is the one per-run
// place that already outlives the agent's memory.
//
// # How the next agent knows the number still means what it meant
//
// This is the whole difficulty, and the reason this type is not just a file with
// an integer in it. A pgid is a small integer the kernel recycles. A record
// naming 53211, written before a crash and read after a restart, may by then
// name somebody's editor — and `kill(-53211)` would be precisely the class of
// "failure that looks like a code bug" §13.3 warns about, with the agent as the
// unattributable cause.
//
// So the record is not only read, it is *held*. Before the step starts, the file
// is opened, flock'd, and its descriptor handed to the shell through
// [exec.Cmd.ExtraFiles]; every process the step forks inherits it. A BSD flock
// lives on the open file description rather than on the process, so it stays
// held for exactly as long as any member of that step's tree is alive — through
// the death of the agent that took it, and through the death of the shell that
// led the group. A reaper that *can* take the lock has proved that nothing from
// that step is left, and signals nothing. A reaper that cannot take it has
// proved the opposite about this exact inode, which no recycled pid can forge.
//
// That is stronger than the usual defences and costs one open and one flock:
// checking that the pgid is still a group leader cannot tell our leader from a
// recycled one, and comparing a start time against the record's mtime needs a
// `ps` per record and a per-OS parser. Neither is needed here.
//
// # Residual risk, stated rather than hidden
//
//   - A descendant that deliberately leaves the group — `setsid`, or a daemon
//     calling setpgid — while keeping the inherited descriptor open holds the
//     lock for a group it is no longer in. For that to cause a wrong kill the
//     original leader's pid must *also* have been recycled, and the recycling
//     process must *also* have become a group leader, since `kill(-pgid)` cannot
//     touch a pid that is not one. Three coincidences deep; accepted.
//   - The pgid is written immediately after the fork. An agent killed inside
//     that window leaves an empty record, which reaps nothing. The leak it fails
//     to collect is exactly the leak that existed before this code did.
//   - A daemon that closes inherited descriptors on startup (a real habit:
//     that is what daemon(3) does) releases the lock, so a group holding only
//     such processes reads as gone and is left alone. Fail-safe direction — a
//     leak survives, nothing unrelated dies.
//   - A job runs as the agent's own user with no isolation (see the package
//     doc), so it can write its own record beside $LATEM_ENV, hold it open, and
//     have the next reap kill a group of its choosing. It could equally run
//     `kill` itself, so this is worth stating and not worth defending against.
//   - Two agents sharing one [Options.EnvFileDir] would reap each other's live
//     runs. That was already true of the directory removal below it — one agent
//     would delete the other's $LATEM_ENV mid-run — and it is sharper now that
//     the reap also signals. One agent per EnvFileDir.
type stepGroup struct {
	// file is the agent's own descriptor. Held open for the life of the step so
	// the lock survives a step whose shell closes fd 3, and closed by [release].
	file *os.File
	path string
}

// newStepGroup creates the record for a step that is about to start and attaches
// it to cmd, which must not have been started yet: [exec.Cmd.ExtraFiles] is only
// read at Start, and the point of the whole mechanism is that the descriptor is
// in the child's table from its first instruction.
//
// The pgid is not known yet — the kernel picks it at fork, and with
// [setProcessGroup] it is the child's pid — so the file is created empty and
// [stepGroup.note] fills it in immediately afterwards.
func newStepGroup(dir string, cmd *exec.Cmd) (*stepGroup, error) {
	f, err := os.CreateTemp(dir, GroupRecordPrefix)
	if err != nil {
		return nil, wrapErr(protocol.ErrExecutor, err, "create process group record in %s", dir)
	}
	if err := attachGroupRecord(cmd, f); err != nil {
		_ = f.Close()
		_ = os.Remove(f.Name())
		return nil, wrapErr(protocol.ErrExecutor, err, "hold process group record %s", f.Name())
	}
	return &stepGroup{file: f, path: f.Name()}, nil
}

// note records the process group of a step that has just started.
//
// Not fsync'd, deliberately. The failure this record defends against is the
// agent being killed, and a process dying does not lose a write the kernel has
// already taken. The failure it does not defend against is the machine losing
// power, and that reboot takes the orphans with it.
//
// A pgid of 0 or 1 is never written: 0 is what [processGroupOf] reports on a
// platform with no process groups at all, and 1 would name launchd. An empty
// record reaps nothing, which is the right answer in both cases.
func (g *stepGroup) note(pgid int) error {
	if g == nil || pgid <= 1 {
		return nil
	}
	if _, err := fmt.Fprintf(g.file, "%d\n", pgid); err != nil {
		return wrapErr(protocol.ErrExecutor, err, "record process group %d in %s", pgid, g.path)
	}
	return nil
}

// release drops the record once the step is over.
//
// Removing it is what makes a reap mean something: a record that outlives its
// step by design would make every later reap look at a group that either
// finished normally or was already stopped by [stepProc.stop], and the only
// records a reaper ever sees are then the genuinely abandoned ones.
//
// Closing the agent's descriptor does not release the flock while any process
// the step forked still holds its inherited copy — which is the point during a
// crash, and harmless here because the file is gone.
func (g *stepGroup) release() {
	if g == nil {
		return
	}
	_ = g.file.Close()
	_ = os.Remove(g.path)
}

// reapGroups stops the process groups recorded in one abandoned run's
// $LATEM_ENV directory, and reports the ones it actually signalled.
//
// dir is always a directory this executor created under [Options.EnvFileDir]:
// [Executor.Reap] builds it from its own configured root and a name matching
// [EnvDirPrefix], so no path here comes from a broker, a job, or another agent.
// Records are only read from the top level of that directory, must be regular
// files, and must parse as a single integer; anything else is left for the
// directory removal that follows.
//
// The record is removed by that same removal rather than here, which makes this
// one-shot on purpose: a group that could not be verified or could not be
// signalled is reported as an error and not retried, because the alternative is
// a record that outlives every reap and eventually names a recycled pid.
func reapGroups(dir string, grace time.Duration) ([]int, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}

	var (
		killed   []int
		firstErr error
	)
	fail := func(err error) {
		if firstErr == nil {
			firstErr = err
		}
	}
	for _, ent := range entries {
		// Type().IsRegular() rather than !IsDir(): a symlink where a record
		// should be is not something this package ever writes, and following one
		// would take the flock probe outside the directory being reaped.
		if !strings.HasPrefix(ent.Name(), GroupRecordPrefix) || !ent.Type().IsRegular() {
			continue
		}
		path := filepath.Join(dir, ent.Name())

		pgid, err := readGroupRecord(path)
		if err != nil {
			fail(err)
			continue
		}
		if pgid == 0 {
			// Written by a step whose agent died between fork and note.
			continue
		}

		held, err := groupRecordHeld(path)
		if err != nil {
			fail(fmt.Errorf("process group record %s: %w", ent.Name(), err))
			continue
		}
		if !held {
			// Nothing that inherited this descriptor is alive, so whatever the
			// number used to mean, it does not mean it now.
			continue
		}

		if err := stopGroup(pgid, grace); err != nil {
			fail(err)
			continue
		}
		killed = append(killed, pgid)
	}
	return killed, firstErr
}

// readGroupRecord reads the pgid out of one record, returning 0 for a record
// that names nothing yet.
//
// The pgid ≤ 1 check here duplicates the one in [signalPgid] on purpose. This is
// the only primitive in the package that can destroy something it was never
// handed, and the two entry points to it — a step's own [exec.Cmd] and a number
// read off a disk that a job can write to — deserve independent refusals. The
// message names the file, because a record that says 1 was either corrupted or
// planted, and the operator needs to know which file to look at.
func readGroupRecord(path string) (int, error) {
	// #nosec G304 -- path is filepath.Join of this executor's own EnvFileDir, a
	// directory name matching EnvDirPrefix, and a directory entry that has
	// already been checked to be a regular file with the record prefix. No
	// component is broker-supplied.
	f, err := os.Open(path)
	if err != nil {
		return 0, fmt.Errorf("process group record %s: %w", filepath.Base(path), err)
	}
	defer func() { _ = f.Close() }()

	data, err := io.ReadAll(io.LimitReader(f, maxGroupRecordBytes))
	if err != nil {
		return 0, fmt.Errorf("process group record %s: %w", filepath.Base(path), err)
	}
	text := strings.TrimSpace(string(data))
	if text == "" {
		return 0, nil
	}
	pgid, err := strconv.Atoi(text)
	if err != nil {
		return 0, fmt.Errorf("process group record %s: %w", filepath.Base(path), err)
	}
	if pgid <= 1 {
		return 0, fmt.Errorf("%w: record %s names process group %d, which is this agent's own group "+
			"or every process on the machine", errRefusedPgid, filepath.Base(path), pgid)
	}
	return pgid, nil
}

// stopGroup is [stepProc.stop] for a group nobody holds a [exec.Cmd] for
// any more: SIGTERM, a grace, SIGKILL.
//
// It stops waiting the moment the group is empty, so a well-behaved orphan costs
// one poll rather than [ReapTerminateGrace]. Reaching the SIGKILL means the
// group ignored SIGTERM, which for a build tool usually means it is wedged
// rather than shutting down politely.
func stopGroup(pgid int, grace time.Duration) error {
	if err := terminatePgid(pgid); err != nil {
		return err
	}
	for deadline := time.Now().Add(grace); time.Now().Before(deadline); {
		if pgidGone(pgid) {
			return nil
		}
		time.Sleep(groupPollInterval)
	}
	if pgidGone(pgid) {
		return nil
	}
	return killPgid(pgid)
}
