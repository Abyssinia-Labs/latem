package run

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"slices"
	"strconv"
	"strings"

	"github.com/Abyssinia-Labs/latem/internal/registry"
	"github.com/Abyssinia-Labs/latem/internal/workflow"
	"github.com/Abyssinia-Labs/latem/pkg/protocol"
)

// isWorkflowRun reports whether this run is a workflow run's job (A52): it
// names its workflow file, and carries the commit, the event and its needs.
func (x *execution) isWorkflowRun() bool { return x.req.Workflow != "" }

// pin is a workflow run's job's commit (A52). The job builds the commit its
// event named, not whatever its ref says by the time a runner is free — and
// that commit must be on the ref, so a control plane cannot borrow one ref's
// policy for a commit from another (INV-5).
//
// A pull request's merge ref is the exception to "on the ref": GitHub
// recomputes it whenever the base branch moves, so the merge commit an event
// named stops being reachable from the ref without anything being wrong. It
// still has to be in the mirror. When no ref the mirror fetched put it there —
// the pull request has merged and GitHub deleted its refs, so tip is empty, or
// another machine built it while the merge ref pointed at a commit this one
// never fetched — it is fetched by its name, as a re-run on GitHub fetches it.
// A branch's commit is not: one the branch's fetch did not bring is not on the
// branch.
func (x *execution) pin(ctx context.Context, mirror Mirror, normalized, tip string) (string, registry.Decision, error) {
	want := x.req.SHA
	if want == tip {
		return want, registry.Decision{}, nil
	}
	refuse := func(why string) (string, registry.Decision, error) {
		return "", registry.Decision{
			State: protocol.StateRejected,
			Code:  protocol.ErrInvalidRef,
			Msg:   fmt.Sprintf("the event named %s, which %s", shortSHA(want), why),
		}, nil
	}
	pull := strings.HasPrefix(normalized, "refs/pull/")
	ok, err := mirror.HasCommit(ctx, want)
	if err != nil {
		return "", registry.Decision{}, err
	}
	if !ok && pull {
		x.note("fetching %s by its name: no ref here reaches it", shortSHA(want))
		if err := mirror.FetchCommit(ctx, want); err != nil {
			return refuse(fmt.Sprintf("could not be fetched from %s (%v)", x.req.Repo, err))
		}
		if ok, err = mirror.HasCommit(ctx, want); err != nil {
			return "", registry.Decision{}, err
		}
	}
	if !ok {
		return refuse("is not in this machine's mirror of " + x.req.Repo)
	}
	if pull {
		return want, registry.Decision{}, nil
	}
	on, err := mirror.IsAncestor(ctx, want, tip)
	if err != nil {
		return "", registry.Decision{}, err
	}
	if !on {
		return refuse(fmt.Sprintf("is not on %s (now %s)", normalized, shortSHA(tip)))
	}
	return want, registry.Decision{}, nil
}

// pullGone reports whether gate 7's refusal is a workflow run's job naming a
// pull request's ref that no longer exists: the pull request merged or closed
// and GitHub deleted its refs/pull/N/*. The job is pinned to a commit, so a
// re-run of it builds that commit (pin) rather than refusing a name it does not
// need.
func (x *execution) pullGone(err error) bool {
	return x.isWorkflowRun() && x.req.SHA != "" &&
		strings.HasPrefix(x.req.Ref, "refs/pull/") && codeOf(err) == protocol.ErrInvalidRef
}

// eventJSON is the event's payload for $GITHUB_EVENT_PATH: the webhook body
// as it arrived, with a dispatch's inputs as GitHub puts them there — as
// strings. Nil for a run that is not a workflow run's job.
func (x *execution) eventJSON() []byte {
	if !x.isWorkflowRun() || x.req.Event == nil {
		return nil
	}
	payload := x.eventPayload()
	if x.req.Event.Name == workflow.EventWorkflowDispatch && len(x.req.Inputs) > 0 {
		inputs := make(map[string]any, len(x.req.Inputs))
		for k, v := range x.req.Inputs {
			inputs[k] = v
		}
		payload["inputs"] = inputs
	}
	b, err := json.Marshal(payload)
	if err != nil {
		return []byte("{}")
	}
	return b
}

// eventPayload is the event's payload decoded, for github.event. The
// protocol only lets an object through, so a payload that does not decode is
// an empty event rather than a failed run.
func (x *execution) eventPayload() map[string]any {
	out := map[string]any{}
	if x.req.Event != nil && len(x.req.Event.Payload) > 0 {
		if err := json.Unmarshal(x.req.Event.Payload, &out); err != nil {
			x.note("the event's payload could not be read (%v); github.event is empty", err)
			return map[string]any{}
		}
	}
	return out
}

// workflowRunContext layers a workflow run's facts over github: the event,
// the commit, the workflow run's own id and numbers (A52).
func (x *execution) workflowRunContext(github map[string]any) {
	ev := x.req.Event
	if ev == nil {
		return
	}
	github["event"] = x.eventPayload()
	github["event_name"] = ev.Name
	github["actor"] = ev.Actor
	github["triggering_actor"] = ev.Actor
	github["run_id"] = x.req.WorkflowRunID
	github["run_number"] = float64(x.runNumber())
	github["run_attempt"] = float64(max(x.req.RunAttempt, 1))
	github["base_ref"], github["head_ref"] = "", ""
	if strings.HasPrefix(ev.Name, "pull_request") {
		github["base_ref"], github["head_ref"] = ev.BaseRef, ev.HeadRef
	}
}

// workflowRunEnv is GITHUB_* for a workflow run's job, over the variables
// every workflow job gets (A52).
func (x *execution) workflowRunEnv(env map[string]string) {
	ev := x.req.Event
	if ev == nil {
		return
	}
	env["GITHUB_EVENT_NAME"] = ev.Name
	env["GITHUB_ACTOR"] = ev.Actor
	env["GITHUB_TRIGGERING_ACTOR"] = ev.Actor
	env["GITHUB_RUN_ID"] = x.req.WorkflowRunID
	if n := x.runNumber(); n > 0 {
		env["GITHUB_RUN_NUMBER"] = strconv.FormatInt(n, 10)
	}
	env["GITHUB_RUN_ATTEMPT"] = strconv.FormatInt(max(x.req.RunAttempt, 1), 10)
	if strings.HasPrefix(ev.Name, "pull_request") {
		env["GITHUB_BASE_REF"] = ev.BaseRef
		env["GITHUB_HEAD_REF"] = ev.HeadRef
	}
}

// runNumber is github.run_number: the workflow run's, or for one no control
// plane numbered — a try (A76) — the machine's own, as $LATEM_BUILD_NUMBER.
// A control plane numbers a workflow run from 1, so 0 means "not numbered".
func (x *execution) runNumber() int64 {
	if x.req.RunNumber > 0 {
		return x.req.RunNumber
	}
	return x.run.Number
}

// skip ends a workflow run's job whose if: was false (A52): straight from
// preparing to skipped, which is neither success nor failure. Nothing ran,
// so there is nothing to collect.
func (x *execution) skip(ctx context.Context, cond string) Result {
	msg := "the job's condition is false"
	if cond != "" {
		msg = fmt.Sprintf("if: %s is false", cond)
	}
	return x.terminate(ctx, outcome{state: protocol.StateSkipped, msg: msg})
}

// jobOutputs evaluates a workflow run's job's outputs: as it ends, against
// every step that ran (A52). An output holding a value the redactor knows is
// dropped and named, as GitHub drops one carrying a secret; one that cannot be
// evaluated is named and left out, since the job's own result is already
// decided.
func (x *execution) jobOutputs() map[string]string {
	if x.resolved == nil || x.resolved.Job == nil || len(x.resolved.Job.Outputs) == 0 {
		return nil
	}
	sc := x.stepScope(registry.StatusSuccess)
	out := map[string]string{}
	for _, name := range slices.Sorted(maps.Keys(x.resolved.Job.Outputs)) {
		v, err := x.resolved.Job.Outputs[name].Resolve(sc)
		if err != nil {
			x.note("output %s was not set: %s", name, x.redact(err.Error()))
			continue
		}
		if x.redact(v) != v {
			x.note("output %s was dropped: it holds a secret", name)
			continue
		}
		out[name] = v
	}
	return out
}

// artifactsFrom is the jobs of the workflow run this job's download-artifact
// may read (A84): its artifacts_from, as the plan of the commit in its own
// workspace makes it, and never what the control plane sent (INV-5). A file
// the plan cannot make sense of reads no other job's, and the compiler then
// refuses the download by name. Asked only by a job with a download.
func (x *execution) artifactsFrom() []string {
	if x.ws == nil || x.req.Event == nil {
		return nil
	}
	root, err := os.OpenRoot(x.ws.Path)
	if err != nil {
		x.note("download-artifact reads no other job's artifacts: %v", err)
		return nil
	}
	defer func() { _ = root.Close() }()
	slug, _ := x.githubRepository()
	opts := workflow.PlanOptions{Repository: slug, LabelAliases: x.o.labelAliases()}
	if vars := x.repoVars(); vars != nil {
		opts.Vars = vars("")
	}
	from, err := workflow.ArtifactsFrom(root.FS(), x.req.Workflow, x.req.Job, *x.req.Event, opts)
	if err != nil {
		x.note("download-artifact reads no other job's artifacts: planning %s: %v", x.req.Workflow, err)
		return nil
	}
	return from
}
