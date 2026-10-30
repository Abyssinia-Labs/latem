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
		if err != nil || wf.Jobs[x.req.Job] == nil {
			return
		}
		leg := needs.Leg{Labels: job.RunsOn}
		_ = json.Unmarshal(x.req.Matrix, &leg.Matrix)
		r = needs.Job(fsys, wf, x.req.Job, leg)
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
