package protocol

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
)

// MaxEventPayloadBytes bounds [Event.Payload]. A pull request's webhook body
// is tens of kilobytes; a push of twenty commits rarely passes a hundred. The
// cap leaves room for both inside the transport's one-megabyte frame, with the
// rest of a run request beside it, and stops the payload being a way to make
// the agent hold arbitrary data (A52).
const MaxEventPayloadBytes = 256 << 10

// workflowFilePattern is the only path an inbound field may name: a file
// directly under .github/workflows (A52, INV-2). The first character is not a
// dot, so no name is "." or "..", and there is no separator after the
// directory.
var workflowFilePattern = regexp.MustCompile(`^\.github/workflows/[A-Za-z0-9_-][A-Za-z0-9._-]*\.ya?ml$`)

// shaPattern is a full commit id: SHA-1, or SHA-256 for a repository using it.
var shaPattern = regexp.MustCompile(`^[0-9a-f]{40}([0-9a-f]{24})?$`)

// Event is one GitHub event, as a plan and a workflow job read it (A52). It is
// data throughout: nothing in it names a command, and the payload reaches a
// step only as a file at $GITHUB_EVENT_PATH or through an expression bound to
// an environment variable (A42).
type Event struct {
	// Name is GitHub's event name: push, pull_request, workflow_dispatch.
	Name string `json:"name"`
	// Action is the activity type — a pull request's opened, a release's
	// published — and a repository_dispatch's event_type.
	Action string `json:"action,omitempty"`
	// Ref is the ref the event is about: refs/heads/main for a push or a
	// dispatch, refs/pull/7/merge for a pull request.
	Ref string `json:"ref"`
	// SHA is the commit the event names, which a plan reads and a job builds.
	SHA string `json:"sha"`
	// Before is a push's previous tip, for the diff a paths filter reads.
	// Absent for a new branch, and for every other event.
	Before string `json:"before,omitempty"`
	// BaseRef and HeadRef are a pull request's branches, by name.
	BaseRef string `json:"base_ref,omitempty"`
	HeadRef string `json:"head_ref,omitempty"`
	// Workflow is the file a workflow_dispatch names. No other event names one.
	Workflow string `json:"workflow,omitempty"`
	// Inputs are a workflow_dispatch's, as given.
	Inputs map[string]string `json:"inputs,omitempty"`
	// Actor is who caused the event. Display, and github.actor.
	Actor string `json:"actor,omitempty"`
	// Payload is the webhook's body, a JSON object: github.event.
	Payload json.RawMessage `json:"payload,omitempty"`
}

// validate checks an event's shape. Whether the agent plans that kind of
// event, and whether its refs exist, are the planner's and the mirror's to
// say.
func (e Event) validate() error {
	if e.Name == "" {
		return fmt.Errorf("protocol: event has no name")
	}
	if !shaPattern.MatchString(e.SHA) {
		return fmt.Errorf("protocol: event sha %q is not a commit id", e.SHA)
	}
	if e.Before != "" && !shaPattern.MatchString(e.Before) {
		return fmt.Errorf("protocol: event before %q is not a commit id", e.Before)
	}
	if e.Workflow != "" && !workflowFilePattern.MatchString(e.Workflow) {
		return fmt.Errorf("protocol: event workflow %q must match %s", e.Workflow, workflowFilePattern)
	}
	if len(e.Payload) > 0 {
		if len(e.Payload) > MaxEventPayloadBytes {
			return fmt.Errorf("protocol: event payload is %d bytes, limit %d", len(e.Payload), MaxEventPayloadBytes)
		}
		if p := bytes.TrimSpace(e.Payload); len(p) == 0 || p[0] != '{' {
			return fmt.Errorf("protocol: event payload must be a JSON object")
		}
	}
	return nil
}

// PlanRequest asks which workflow runs an event starts at a commit (A52).
//
// The control plane holds no workflow definitions and does not parse workflow
// files (INV-5): the machine reads them from its own mirror at Event.SHA and
// answers with a [PlanResult]. Like everything inbound, it carries names and
// data — an id, a repository key, and the event.
type PlanRequest struct {
	ID    string `json:"id"`
	Repo  string `json:"repo"` // must exist in local config
	Event Event  `json:"event"`
}

// PlanResult is the machine's answer to a [PlanRequest]: one entry per
// workflow the event triggers, in file order, or why there is no plan.
// Structure and names, never steps.
type PlanResult struct {
	ID        string            `json:"id"`
	Workflows []PlannedWorkflow `json:"workflows"`
	// ErrorCode and Error say why the request could not be planned at all —
	// an unknown repository, a fetch that failed, a dispatch the workflow
	// refuses. A workflow that cannot be planned is not that: it is in
	// Workflows, with its own error.
	ErrorCode ErrorCode `json:"error_code,omitempty"`
	Error     string    `json:"error,omitempty"`
}

// PlannedWorkflow is one workflow run an event starts.
type PlannedWorkflow struct {
	// File is the workflow's path in the repository.
	File string `json:"file"`
	// Name is the workflow's name:, or its path when it has none.
	Name string `json:"name"`
	// RunName is run-name: evaluated; empty when the workflow has none.
	RunName     string       `json:"run_name,omitempty"`
	Concurrency *Concurrency `json:"concurrency,omitempty"`
	// Inputs are a dispatch's, with the workflow's defaults filled in. Every
	// job of the run is given them.
	Inputs map[string]string `json:"inputs,omitempty"`
	// Jobs are ordered so that each comes after every job it needs.
	Jobs []PlannedJob `json:"jobs,omitempty"`
	// Error is why the workflow could not be planned. It is reported as a
	// failed run rather than left out, as GitHub reports an invalid file.
	Error string `json:"error,omitempty"`
}

// PlannedJob is one job of a planned workflow run.
type PlannedJob struct {
	// Key is the job's id under jobs:, which needs: and a run's job name.
	Key string `json:"key"`
	// Name is the job's name: evaluated, or its key. Its check run is named
	// by it.
	Name  string   `json:"name"`
	Needs []string `json:"needs,omitempty"`
	// Labels are runs-on after the machine's aliases and GitHub's hosted
	// labels (A50); a runner whose labels cover them can take the job.
	Labels      []string `json:"labels,omitempty"`
	Environment string   `json:"environment,omitempty"`
	// Permissions scope the job's token, in the workflow's vocabulary
	// (pull-requests, id-token) with levels read or write. A scope it does not
	// name has none.
	Permissions map[string]string `json:"permissions"`
	Concurrency *Concurrency      `json:"concurrency,omitempty"`
	// IfChecksStatus is set when the job's if: calls a status function. Only
	// then may the job run after a job it needs did not succeed, and the
	// machine decides; otherwise the control plane skips it.
	IfChecksStatus  bool    `json:"if_checks_status,omitempty"`
	ContinueOnError bool    `json:"continue_on_error,omitempty"`
	TimeoutMinutes  float64 `json:"timeout_minutes"`
	// Matrix and Strategy are set on a matrix leg (A55, 2.1): the leg's
	// values, a JSON object to send back as run.matrix, and its place among
	// the legs that share its key.
	Matrix   json.RawMessage `json:"matrix,omitempty"`
	Strategy *Strategy       `json:"strategy,omitempty"`
	// Error is why this job cannot run. It fails alone, and the jobs that
	// need it are skipped as after any failure.
	Error string `json:"error,omitempty"`
}

// Strategy is a matrix leg's place among its siblings, and how the control
// plane schedules them (A55). Every leg of a job carries the same FailFast,
// MaxParallel and JobTotal.
type Strategy struct {
	// JobIndex is the leg's position in the expansion, from zero; JobTotal is
	// how many legs there are.
	JobIndex int `json:"job_index"`
	JobTotal int `json:"job_total"`
	// FailFast cancels the queued and running siblings of a leg that fails
	// without continue-on-error. GitHub's default is true.
	FailFast bool `json:"fail_fast"`
	// MaxParallel caps how many legs run at once; zero is no cap.
	MaxParallel int `json:"max_parallel,omitempty"`
}

// MaxMatrixBytes bounds [RunRequest.Matrix]. A leg is a handful of short
// values; the cap stops the field being a way to make the agent hold data.
const MaxMatrixBytes = 16 << 10

// Concurrency is a concurrency: block, evaluated.
type Concurrency struct {
	Group            string `json:"group"`
	CancelInProgress bool   `json:"cancel_in_progress,omitempty"`
}

// The results a needed job can have, as needs.<job>.result reads them.
const (
	NeedSuccess   = "success"
	NeedFailure   = "failure"
	NeedCancelled = "cancelled"
	NeedSkipped   = "skipped"
)

// NeedResult is one needed job's result and outputs, as a dependent job's
// needs context reads them (A52). Outputs are data, and reach a step only
// through an environment variable (A42).
type NeedResult struct {
	Result  string            `json:"result"`
	Outputs map[string]string `json:"outputs,omitempty"`
}

// jobKeyPattern is GitHub's rule for a job's id.
var jobKeyPattern = regexp.MustCompile(`^[_A-Za-z][A-Za-z0-9_-]*$`)

// tokenPattern is printable ASCII without spaces: an installation token is
// one opaque word, and anything else in its place is not one.
var tokenPattern = regexp.MustCompile(`^[!-~]+$`)

// maxTokenBytes bounds [RunRequest.Token]. GitHub's are under a hundred.
const maxTokenBytes = 1024

// validateWorkflowRun checks the fields a workflow run's job carries (A52).
func (r RunRequest) validateWorkflowRun() error {
	set := r.SHA != "" || r.Event != nil || len(r.Needs) > 0 || r.WorkflowRunID != "" ||
		r.RunNumber != 0 || r.RunAttempt != 0 || r.Token != "" || len(r.Matrix) > 0
	if r.Workflow == "" {
		if set {
			return fmt.Errorf("protocol: run %s carries a workflow run's fields and names no workflow", r.ID)
		}
		return nil
	}
	if !workflowFilePattern.MatchString(r.Workflow) {
		return fmt.Errorf("protocol: run workflow %q must match %s", r.Workflow, workflowFilePattern)
	}
	if !jobKeyPattern.MatchString(r.Job) {
		return fmt.Errorf("protocol: run job %q is not a job id", r.Job)
	}
	if !shaPattern.MatchString(r.SHA) {
		return fmt.Errorf("protocol: run sha %q is not a commit id", r.SHA)
	}
	if r.Event == nil {
		return fmt.Errorf("protocol: run %s of a workflow carries no event", r.ID)
	}
	if err := r.Event.validate(); err != nil {
		return err
	}
	if err := ValidateID(r.WorkflowRunID); err != nil {
		return fmt.Errorf("protocol: workflow_run_id: %w", err)
	}
	if r.RunNumber < 0 || r.RunAttempt < 0 {
		return fmt.Errorf("protocol: run_number and run_attempt cannot be negative")
	}
	for key, n := range r.Needs {
		if !jobKeyPattern.MatchString(key) {
			return fmt.Errorf("protocol: needs %q is not a job id", key)
		}
		switch n.Result {
		case NeedSuccess, NeedFailure, NeedCancelled, NeedSkipped:
		default:
			return fmt.Errorf("protocol: needs.%s.result %q is not success, failure, cancelled or skipped", key, n.Result)
		}
	}
	if r.Token != "" && (len(r.Token) > maxTokenBytes || !tokenPattern.MatchString(r.Token)) {
		return fmt.Errorf("protocol: run token is not one printable word")
	}
	if len(r.Matrix) > 0 {
		if len(r.Matrix) > MaxMatrixBytes {
			return fmt.Errorf("protocol: run matrix is %d bytes, limit %d", len(r.Matrix), MaxMatrixBytes)
		}
		var leg map[string]json.RawMessage
		if err := json.Unmarshal(r.Matrix, &leg); err != nil || leg == nil {
			return fmt.Errorf("protocol: run matrix must be a JSON object")
		}
	}
	return nil
}
