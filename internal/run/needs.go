package run

import (
	"encoding/json"
	"io/fs"
	"os"

	"github.com/Abyssinia-Labs/latem/internal/needs"
	"github.com/Abyssinia-Labs/latem/internal/registry"
	"github.com/Abyssinia-Labs/latem/internal/workflow"
)

// noteNeeds writes in the run's log what the job needs that the runner it is
// about to run on lacks (A59): "this job needs Xcode 27; this runner has
// Xcode 26.0.1". A note, before any step: it explains a failure the reader
// would otherwise have to work out from a build tool's error, and decides
// nothing — the run goes ahead exactly as it would have.
func (x *execution) noteNeeds(reg *registry.Registry, runner *Runner) {
	if x.o.toolchains == nil || runner == nil || x.ws == nil {
		return
	}
	tc := x.o.toolchains()
	job := reg.Jobs[x.req.Job]
	if tc == nil || job == nil {
		return
	}
	if executorName(runner) == "native" {
		v := tc.XcodeVersions()
		x.xcodeAtStart = &v
	}
	root, err := os.OpenRoot(x.ws.Path)
	if err != nil {
		return
	}
	defer func() { _ = root.Close() }()
	fsys := root.FS()

	var r needs.Report
	if x.workflow != nil {
		data, err := fs.ReadFile(fsys, x.workflow.Path)
		if err != nil {
			return
		}
		wf, err := workflow.Parse(data, x.workflow.Path)
		if err != nil {
			return
		}
		// A called workflow's job is read from its own file (A81).
		declaredIn, key, err := workflow.CalledJob(fsys, wf, x.req.Job)
		if err != nil {
			return
		}
		leg := needs.Leg{Labels: job.RunsOn}
		_ = json.Unmarshal(x.req.Matrix, &leg.Matrix)
		r = needs.Job(fsys, declaredIn, key, leg)
	} else {
		r = needs.RegistryJob(fsys, job)
	}
	for _, g := range needs.Gaps(r, *tc, executorName(runner)) {
		msg := "this job needs " + g.Missing
		if g.Have != "" {
			msg += "; this runner has " + g.Have
		}
		x.note("%s", msg)
	}
}

// noteXcodeChanged says, as a job on a native runner ends, that the machine's
// Xcode changed while it ran (A64). An App Store update replaces Xcode in
// place without asking whether a build is using it, and the failure that
// follows names a missing framework or a dead simulator, never the update.
// The agent probes again within half a minute of an update, so one that lands
// in a job's last seconds can go unsaid.
func (x *execution) noteXcodeChanged() {
	if x.xcodeAtStart == nil {
		return
	}
	tc := x.o.toolchains()
	if tc == nil {
		return
	}
	before, now := *x.xcodeAtStart, tc.XcodeVersions()
	if now == before {
		return
	}
	x.note("this machine's Xcode changed while the job ran (%s → %s); an App Store update replaces Xcode "+
		"in place, so a failure in this job may be the update's", orNone(before), orNone(now))
}

func orNone(s string) string {
	if s == "" {
		return "none"
	}
	return s
}
