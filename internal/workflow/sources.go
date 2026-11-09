package workflow

import (
	"fmt"
	"io/fs"
	"slices"
	"strings"

	"github.com/Abyssinia-Labs/latem/internal/workflow/actions"
	"github.com/Abyssinia-Labs/latem/internal/workflow/expr"
	"github.com/Abyssinia-Labs/latem/pkg/protocol"
)

// Artifacts across jobs (A84).
//
// GitHub's artifacts belong to the workflow run, so one job downloads what an
// earlier one uploaded. latem's belong to a run, which is one job, and stay on
// the machine that ran it (§17): content never crosses the boundary. So a job
// that reads another job's artifacts is placed where that job ran. The plan
// names the jobs, as artifacts_from, and the control plane places by it. The
// machine that runs the job works the same list out again from the commit it
// is pinned to, and its download reads only those jobs' runs (INV-5): what
// the control plane sent places the job and decides nothing else.

// jobArtifacts is what a planned job's own steps say about artifacts, as a
// plan reads them: the names it uploads, and the names its downloads ask for
// that no earlier step of the same job uploads. "" is a name only known as
// the job runs, or, for a download, every artifact.
type jobArtifacts struct {
	uploads []string
	reads   []string
}

// noteArtifacts records what job j's steps upload and download, under its
// planned key. The legs of a matrix share a key and their steps, so the first
// one noted stands for all.
//
// Only the job's own steps are read. An upload inside a composite action is
// not seen, as it is not until the action is expanded, which a plan does not
// do. A name is evaluated against the plan's scope where it can be, and is
// otherwise "": a step's outputs, github.run_number and needs are only known
// as the job runs.
func (p *planner) noteArtifacts(key string, j *Job, sc expr.Scope) {
	if p.artifacts == nil {
		p.artifacts = map[string]*jobArtifacts{}
	}
	if _, seen := p.artifacts[key]; seen {
		return
	}
	a := &jobArtifacts{}
	for _, st := range j.Steps {
		upload, download, name := planArtifactStep(st, sc)
		switch {
		case upload:
			a.uploads = append(a.uploads, name)
		case download && !uploadedBefore(a.uploads, name):
			a.reads = append(a.reads, name)
		}
	}
	if len(a.uploads) > 0 || len(a.reads) > 0 {
		p.artifacts[key] = a
	}
}

// uploadedBefore is the compiler's rule for a download an earlier step of the
// same job could satisfy ([expander.downloadable]).
func uploadedBefore(uploads []string, name string) bool {
	if len(uploads) == 0 {
		return false
	}
	return name == "" || slices.Contains(uploads, "") || slices.Contains(uploads, name)
}

// planArtifactStep says whether st is an upload-artifact or a
// download-artifact step, and the name it gives as a plan can read it.
func planArtifactStep(st Step, sc expr.Scope) (upload, download bool, name string) {
	ref, err := actions.ParseRef(st.Uses)
	if err != nil || ref.Path != "" {
		return false, false, ""
	}
	switch strings.ToLower(ref.Slug()) {
	case uploadArtifactAction:
		upload = true
	case downloadArtifactAction:
		download = true
	default:
		return false, false, ""
	}
	with, err := withStrings(st.With)
	if err != nil {
		return upload, download, ""
	}
	if upload {
		// archive: false names the artifact after its file, which only the
		// step knows; so may an archive: the plan cannot answer.
		if a, ok := lookupFold(with, "archive"); ok && !truthy(strings.TrimSpace(a)) {
			return true, false, ""
		}
		name = "artifact"
	}
	if v, ok := lookupFold(with, "name"); ok && strings.TrimSpace(v) != "" {
		name = strings.TrimSpace(v)
	}
	if expr.Has(name) {
		v, err := planString(name, sc)
		if err != nil {
			return upload, download, ""
		}
		name = strings.TrimSpace(v)
	}
	return upload, download, name
}

func lookupFold(m map[string]string, key string) (string, bool) {
	for k, v := range m {
		if strings.EqualFold(k, key) {
			return v, true
		}
	}
	return "", false
}

// placeArtifacts sets artifacts_from on each job of a workflow run whose
// downloads read what an earlier job uploaded: a job it needs, directly or
// further up. A name matches when both are known and equal, or when either is
// not known until the job runs. A download nothing could satisfy is left for
// the compiler to refuse by name (A77).
//
// For a control plane older than 2.9, which would place the job anywhere,
// the job carries an error instead, as a call did before 2.8 (A82).
func (p *planner) placeArtifacts(jobs []protocol.PlannedJob) {
	if len(p.artifacts) == 0 {
		return
	}
	needs := map[string][]string{}
	for _, pj := range jobs {
		for _, n := range pj.Needs {
			if !slices.Contains(needs[pj.Key], n) {
				needs[pj.Key] = append(needs[pj.Key], n)
			}
		}
	}
	for i := range jobs {
		a := p.artifacts[jobs[i].Key]
		if a == nil || len(a.reads) == 0 {
			continue
		}
		var from []string
		for _, k := range ancestors(jobs[i].Key, needs) {
			up := p.artifacts[k]
			if up != nil && readsAny(a.reads, up.uploads) {
				from = append(from, k)
			}
		}
		if len(from) == 0 {
			continue
		}
		if p.opts.NoPlacedArtifacts {
			addPlanError(&jobs[i], fmt.Sprintf("download-artifact reads what %s uploaded, which is on "+
				"the machine that ran it; the control plane this plan is for does not place a job there: "+
				"that takes protocol 2.9 (A84)", strings.Join(from, ", ")))
			continue
		}
		jobs[i].ArtifactsFrom = from
	}
}

// readsAny reports whether one of a job's downloads could be one of another
// job's uploads.
func readsAny(reads, uploads []string) bool {
	for _, r := range reads {
		for _, u := range uploads {
			if r == "" || u == "" || r == u {
				return true
			}
		}
	}
	return false
}

// ancestors is every job key needs reach from key, sorted, without key.
func ancestors(key string, needs map[string][]string) []string {
	seen := map[string]bool{key: true}
	queue := slices.Clone(needs[key])
	var out []string
	for len(queue) > 0 {
		k := queue[0]
		queue = queue[1:]
		if seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, k)
		queue = append(queue, needs[k]...)
	}
	slices.Sort(out)
	return out
}

// ArtifactsFrom is the jobs of the workflow run whose artifacts job may
// download (A84): its artifacts_from, as this commit's file says, planned for
// ev. The machine that runs a job asks it of the job's own workspace, so the
// runs a download may read are the pinned commit's answer and never the
// control plane's (INV-5). fsys is the commit's tree. A file that cannot be
// planned reads nothing from another job.
func ArtifactsFrom(fsys fs.FS, file, job string, ev protocol.Event, opts PlanOptions) ([]string, error) {
	opts.NoCalls, opts.NoPlacedArtifacts = false, false
	pw, err := PlanFile(fsys, file, ev, opts)
	if err != nil {
		return nil, err
	}
	for _, pj := range pw.Jobs {
		if pj.Key == job {
			return pj.ArtifactsFrom, nil
		}
	}
	return nil, fmt.Errorf("%s plans no job %q", file, job)
}
