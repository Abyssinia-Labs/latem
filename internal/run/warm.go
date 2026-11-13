package run

import (
	"context"
	"errors"
	"io/fs"
	"time"

	"github.com/Abyssinia-Labs/latem/internal/registry"
	"github.com/Abyssinia-Labs/latem/internal/source"
	"github.com/Abyssinia-Labs/latem/internal/workflow"
)

// WarmMirror is a [Mirror] that can keep a job's workspace between its runs
// (A90). *source.Mirror is one. It is a second interface, not three more
// methods on the first, because a mirror that cannot is still a mirror: the
// job builds in a fresh clone.
type WarmMirror interface {
	// ReadDir reads one directory's files at a commit, without a checkout.
	ReadDir(ctx context.Context, sha, dir string, maxFiles, maxBytes int) (fs.FS, error)
	// WarmUnsupported says why a commit cannot be built warm, or "".
	WarmUnsupported(ctx context.Context, sha string) (string, error)
	// WarmCheckout is the kept workspace of key, brought to sha.
	WarmCheckout(ctx context.Context, key source.WarmKey, runID, normalizedRef, sha string) (
		*source.Workspace, source.WarmReport, error)
}

// checkout is gate 10: a new clone at the commit gate 8 admitted, or, for a
// jobs.yaml job that says `workspace: warm`, the directory its last run left,
// brought to that commit (A90).
func (x *execution) checkout(ctx context.Context, mirror Mirror, normalized, sha string) (*source.Workspace, error) {
	ws, err := x.warmCheckout(ctx, mirror, normalized, sha)
	if err != nil || ws != nil {
		return ws, err
	}
	return mirror.Checkout(ctx, x.run.ID, normalized, sha)
}

// warmCheckout returns the job's warm workspace, or nil for a job that does
// not ask for one or cannot have one, which the log explains.
//
// Whether the job asks is in the registry, and gate 11 reads the registry from
// the workspace this gate makes. So the question is put to the mirror first,
// at the same commit: the same bytes, read by object id, by an agent that has
// not yet decided where to check them out. Nothing is decided from that
// reading but which directory gate 10 uses. Gate 11 still reads the job from
// the workspace, and gates 12 to 15 still admit it from there (INV-5). A
// registry the mirror's copy cannot parse is one gate 11 will refuse, with
// its own message, in a fresh clone.
func (x *execution) warmCheckout(ctx context.Context, mirror Mirror, normalized, sha string) (*source.Workspace, error) {
	wm, ok := mirror.(WarmMirror)
	if !ok || x.isWorkflowRun() {
		return nil, nil
	}
	fsys, err := wm.ReadDir(ctx, sha, ".latem", 64, workflow.MaxWorkflowBytes)
	if err != nil {
		return nil, nil
	}
	data, err := fs.ReadFile(fsys, registry.Path)
	if err != nil {
		return nil, nil
	}
	reg, err := registry.Parse(data)
	if err != nil {
		return nil, nil
	}
	job := reg.Jobs[x.req.Job]
	if job == nil || job.Workspace != registry.WorkspaceWarm {
		return nil, nil
	}
	runner := x.o.selectRunner(reg, x.req.Job)
	if runner == nil {
		return nil, nil // gate 13 refuses it, naming the labels
	}

	reason, err := wm.WarmUnsupported(ctx, sha)
	if err != nil {
		return nil, err
	}
	if reason != "" {
		x.note("job %q asks for a warm workspace, and %s, which one cannot keep: building in a fresh clone (A90)",
			x.req.Job, reason)
		return nil, nil
	}

	// Runs of one job take turns on its workspace. The job's own wall clock
	// bounds the wait: it started when this run did (§9).
	wctx, cancel := context.WithDeadline(ctx, x.started.Add(reg.JobTimeout(job)))
	defer cancel()
	ws, report, err := wm.WarmCheckout(wctx, source.WarmKey{Runner: runner.Name, Job: x.req.Job}, x.run.ID, normalized, sha)
	if errors.Is(err, source.ErrWarmUnavailable) {
		x.note("job %q asks for a warm workspace, and this agent keeps none: building in a fresh clone (A90)", x.req.Job)
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	x.noteWarm(report, runner.Name)
	return ws, nil
}

// noteWarm says in the run's log what the warm workspace held and what was
// done to it, so a build that was slow, or fast, says why.
func (x *execution) noteWarm(r source.WarmReport, runner string) {
	switch {
	case r.Rebuilt != "":
		x.note("warm workspace: the one run %s left could not be brought to this commit (%s); "+
			"made again from nothing, so this build is a cold one (A90)", r.PrevRun, x.redact(r.Rebuilt))
	case r.Fresh:
		x.note("warm workspace: new. The next run of this job on runner %q starts from what this one leaves: "+
			"every tracked file at its commit, and whatever the repository ignores (A90)", runner)
	default:
		age := ""
		if !r.PrevUsed.IsZero() {
			age = ", " + x.o.now().Sub(r.PrevUsed).Round(time.Second).String() + " ago"
		}
		x.note("warm workspace: kept from run %s at %s%s; %d tracked file(s) differed from this commit and "+
			"were written, %d untracked path(s) removed (A90)", r.PrevRun, shortSHA(r.PrevSHA), age, r.Written, r.Removed)
	}
}
