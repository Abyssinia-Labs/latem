package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/spf13/cobra"

	"github.com/Abyssinia-Labs/latem/internal/agent"
	"github.com/Abyssinia-Labs/latem/internal/broker/local"
	"github.com/Abyssinia-Labs/latem/internal/store"
	"github.com/Abyssinia-Labs/latem/pkg/protocol"
)

// The tools `latem mcp` offers (A87). Each is a command an operator could
// type over ssh — list, logs, run, cancel, workflows check — and goes through
// the same code: reads from state.db and the log files, writes over the
// agent's socket. What is not here is left out on purpose. approve and try
// are INV-5's local approvals, which a person gives; secrets, vars, repo,
// service and setup change what this machine builds and with what.
//
// startRun's changes (A91) are the one way an agent builds what nobody
// pushed, and they run under an approval a person gave beforehand, in this
// machine's config, for one repository: changes.go has the rules.
//
// Each call opens its own [state], as a command does, rather than the server
// holding one: a repository added or a config reloaded while an agent's
// session stays open is what its next call sees.

// mcpTool is one tool: what tools/list says of it, and what tools/call does.
type mcpTool struct {
	name, title, description string
	input                    map[string]any // JSON Schema of its arguments
	readOnly, destructive    bool
	idempotent, openWorld    bool
	call                     func(ctx context.Context, args json.RawMessage) (any, error)
}

func (t mcpTool) describe() map[string]any {
	return map[string]any{
		"name":        t.name,
		"title":       t.title,
		"description": t.description,
		"inputSchema": t.input,
		"annotations": map[string]any{
			"title":           t.title,
			"readOnlyHint":    t.readOnly,
			"destructiveHint": t.destructive,
			"idempotentHint":  t.idempotent,
			"openWorldHint":   t.openWorld,
		},
	}
}

// Bounds on what one call reads and returns.
const (
	mcpDefaultRuns = 20
	mcpMaxRuns     = store.DefaultListLimit
	mcpDefaultTail = 100
	mcpMaxLines    = 1000
	mcpDefaultPage = 200
	// mcpMaxLineBytes cuts a log line an agent would pay for in full and read
	// none of: a minified bundle, a base64 blob.
	mcpMaxLineBytes = 4000
	// A wait is a minute unless asked for longer, and never ten: a client
	// that gives up on a call sooner still gets an answer, and calls again.
	mcpDefaultWait = 60 * time.Second
	mcpMaxWait     = 600 * time.Second
	// mcpPoll is how often a wait reads the run's row: one row of a local
	// sqlite, so cheap, and still well inside what a person notices.
	mcpPoll = 500 * time.Millisecond
)

func mcpTools() []mcpTool {
	runID := schemaString("A run id, r_ and 24 hex characters, as listRuns and startRun give it")
	return []mcpTool{
		{
			name:  "listRuns",
			title: "List runs",
			description: "Runs on this machine, newest first: every state unless state narrows it. " +
				"With sha, the runs of that commit — a workflow job is found from the moment it is queued. " +
				"Each run says whether it has settled (finished, one way or another) and, when it is held, what it waits for.",
			input: schemaObject(nil, map[string]any{
				"repo":  schemaString("A repository as this machine names it: a key under [repos] in its config.toml"),
				"sha":   schemaString("A full commit sha, 40 hex characters (git rev-parse HEAD)"),
				"state": map[string]any{"type": "array", "items": map[string]any{"type": "string", "enum": runStateNames()}, "description": "Only runs in these states"},
				"limit": schemaInteger(fmt.Sprintf("At most this many, %d unless given", mcpDefaultRuns), 1, mcpMaxRuns),
			}),
			readOnly: true, idempotent: true,
			call: mcpListRuns,
		},
		{
			name:        "getRun",
			title:       "Get a run",
			description: "One run on this machine: its state, commit, exit code and error, its workflow run when it is a workflow job, its inputs, and what it waits for when it is held.",
			input:       schemaObject([]string{"runId"}, map[string]any{"runId": runID}),
			readOnly:    true, idempotent: true,
			call: mcpGetRun,
		},
		{
			name:  "getRunLogs",
			title: "Read a run's log",
			description: "A run's log as numbered lines. By default its last 100 lines, which is where a failure says why. " +
				"With after, the lines after that one, a page at a time: pass next back to read on, or to see what a running build has written since.",
			input: schemaObject([]string{"runId"}, map[string]any{
				"runId": runID,
				"tail":  schemaInteger(fmt.Sprintf("The last this many lines, %d unless given", mcpDefaultTail), 1, mcpMaxLines),
				"after": schemaInteger("The lines after this line number, instead of the tail", 0, 0),
				"limit": schemaInteger(fmt.Sprintf("With after, at most this many lines, %d unless given", mcpDefaultPage), 1, mcpMaxLines),
			}),
			readOnly: true, idempotent: true,
			call: mcpGetRunLogs,
		},
		{
			name:  "waitForRun",
			title: "Wait for a run to settle",
			description: "Waits until a run settles — succeeded, failed, cancelled and the rest — and returns it. " +
				"Returns sooner, with settled false, at the timeout, or when waiting cannot help: a run held for a person's approval, or one queued with no agent running here. " +
				"Call it again for longer.",
			input: schemaObject([]string{"runId"}, map[string]any{
				"runId":          runID,
				"timeoutSeconds": schemaInteger(fmt.Sprintf("How long to wait, %d seconds unless given", int(mcpDefaultWait.Seconds())), 1, int(mcpMaxWait.Seconds())),
			}),
			readOnly: true, idempotent: true,
			call: mcpWaitForRun,
		},
		{
			name:  "startRun",
			title: "Start a job",
			description: "Starts a job of the repository's .latem/jobs.yaml on this machine, as `latem run` does, and returns its run id at once: waitForRun follows it. " +
				"The job is read from the repository at the ref's commit. A ref outside the repository's allowed refs is held until a person at this machine approves it. " +
				"To build a commit again, start the same job on the same ref. " +
				"With changes, it builds a commit this machine has with your files changed on top, with no push: " +
				"give the base commit, each changed file whole, and the paths removed. " +
				"That needs the machine's config to allow an agent's changes for the repository, a base in the history of the ref, " +
				"and a job the base declares; .latem/jobs.yaml itself cannot be changed this way. " +
				"The job reads the changed paths from the file $LATEM_CHANGED_FILES names.",
			input: schemaObject([]string{"repo", "job"}, map[string]any{
				"repo":   schemaString("A repository as this machine names it: a key under [repos] in its config.toml"),
				"job":    schemaString("A job in the repository's .latem/jobs.yaml"),
				"ref":    schemaString("A branch, tag or commit; " + DefaultRef + " unless given. With changes, the branch or tag the base is on"),
				"inputs": map[string]any{"type": "object", "additionalProperties": map[string]any{"type": []string{"string", "number", "boolean"}}, "description": "The job's declared inputs, by name"},
				"changes": schemaObject([]string{"base"}, map[string]any{
					"base": schemaString("The commit the changes are on: all 40 hex characters, in the history of ref"),
					"files": map[string]any{
						"type": "array", "description": "Files to write, each whole",
						"items": schemaObject([]string{"path"}, map[string]any{
							"path":   schemaString("The file's path in the repository, with forward slashes"),
							"text":   schemaString("Its content, as text"),
							"base64": schemaString("Its content as standard base64, for bytes that are not UTF-8; instead of text"),
							"mode":   map[string]any{"type": "string", "enum": []string{"644", "755"}, "description": "755 for an executable. Left out, the file keeps the mode the base has it at"},
						}),
					},
					"remove": map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Paths the base has that the result does not"},
				}),
			}),
			call: mcpStartRun,
		},
		{
			name:        "cancelRun",
			title:       "Cancel a run",
			description: "Stops a run on this machine, or takes it out of the queue, as `latem cancel` does. A running build stops shortly after: waitForRun shows when.",
			input: schemaObject([]string{"runId"}, map[string]any{
				"runId":  runID,
				"reason": schemaString("Why, shown with the run"),
			}),
			destructive: true, idempotent: true,
			call: mcpCancelRun,
		},
		{
			name:  "checkWorkflows",
			title: "Check a repository's workflows",
			description: "Which of a repository's workflow jobs this machine can run, and the fix for each it cannot — a missing runner label, an action not allowed, a secret with no value (names only) — as `latem workflows check --repo` does. " +
				"Reads the default branch from GitHub, or with offline from this machine's own mirror of it. Changes nothing.",
			input: schemaObject([]string{"repo"}, map[string]any{
				"repo":    schemaString("A repository as this machine names it: a key under [repos] in its config.toml"),
				"offline": map[string]any{"type": "boolean", "description": "Read this machine's mirror rather than GitHub"},
			}),
			readOnly: true, idempotent: true, openWorld: true,
			call: mcpCheckWorkflows,
		},
	}
}

func schemaObject(required []string, props map[string]any) map[string]any {
	s := map[string]any{"type": "object", "properties": props, "additionalProperties": false}
	if len(required) > 0 {
		s["required"] = required
	}
	return s
}

func schemaString(description string) map[string]any {
	return map[string]any{"type": "string", "description": description}
}

// schemaInteger is an integer of at least lo, and at most hi unless hi is 0.
func schemaInteger(description string, lo, hi int) map[string]any {
	s := map[string]any{"type": "integer", "description": description, "minimum": lo}
	if hi > 0 {
		s["maximum"] = hi
	}
	return s
}

// runStateNames are protocol's run states, in the order a run meets them.
func runStateNames() []string {
	var out []string
	for _, s := range []protocol.RunState{
		protocol.StateQueued, protocol.StateAwaitingApproval, protocol.StateClaimed,
		protocol.StatePreparing, protocol.StateRunning, protocol.StateCollecting,
		protocol.StateSucceeded, protocol.StateFailed, protocol.StateTimedOut,
		protocol.StateCancelled, protocol.StateRejected, protocol.StateInterrupted,
		protocol.StateSkipped,
	} {
		out = append(out, string(s))
	}
	return out
}

// mcpArgs decodes a call's arguments, refusing a name the tool does not take:
// a run_id read as no run id would be a quieter, worse failure. (A name in
// another case, runID, is the same name to encoding/json, and is taken.)
func mcpArgs(raw json.RawMessage, v any) error {
	if len(bytes.TrimSpace(raw)) == 0 || string(raw) == "null" {
		raw = json.RawMessage("{}")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("arguments: %v", strings.TrimPrefix(err.Error(), "json: "))
	}
	return nil
}

// mcpRun is a run as the tools show it.
type mcpRun struct {
	ID        string            `json:"id"`
	Number    int64             `json:"number"`
	Repo      string            `json:"repo"`
	Job       string            `json:"job"`
	Ref       string            `json:"ref"`
	SHA       string            `json:"sha,omitempty"`
	State     protocol.RunState `json:"state"`
	Settled   bool              `json:"settled"`
	ExitCode  *int              `json:"exitCode,omitempty"`
	ErrorCode string            `json:"errorCode,omitempty"`
	Error     string            `json:"error,omitempty"`
	Runner    string            `json:"runner,omitempty"`
	Origin    string            `json:"origin"`
	Workflow  *mcpWorkflow      `json:"workflow,omitempty"`
	// Changes is set for a changes run (A91): the commit its files were
	// changed on. Its sha is the commit that made.
	Changes    *mcpChanges `json:"changes,omitempty"`
	CreatedAt  time.Time   `json:"createdAt"`
	StartedAt  *time.Time  `json:"startedAt,omitempty"`
	FinishedAt *time.Time  `json:"finishedAt,omitempty"`
	// Waiting is what a held run waits for that no tool here can give it.
	Waiting string `json:"waiting,omitempty"`

	// Shown by getRun, not in a list.
	Inputs       map[string]string `json:"inputs,omitempty"`
	CancelReason string            `json:"cancelReason,omitempty"`
	ExpiresAt    *time.Time        `json:"expiresAt,omitempty"`
	ApprovedAt   *time.Time        `json:"approvedAt,omitempty"`
	ApprovedSHA  string            `json:"approvedSha,omitempty"`
}

type mcpChanges struct {
	Base string `json:"base"`
}

type mcpWorkflow struct {
	File          string          `json:"file"`
	Event         string          `json:"event"`
	WorkflowRunID string          `json:"workflowRunId"`
	RunNumber     int64           `json:"runNumber,omitempty"`
	RunAttempt    int64           `json:"runAttempt,omitempty"`
	Matrix        json.RawMessage `json:"matrix,omitempty"`
	// Local is a run `latem try` made from a working tree (A76).
	Local bool `json:"local,omitempty"`
}

func runView(r store.Run, detail bool) mcpRun {
	v := mcpRun{
		ID: r.ID, Number: r.Number, Repo: r.Repo, Job: r.Job, Ref: refOf(r), SHA: r.SHA,
		State: r.State, Settled: r.State.IsTerminal(), ExitCode: r.ExitCode,
		ErrorCode: string(r.ErrorCode), Error: r.ErrorMsg, Runner: r.Runner,
		Origin: r.OriginKind, CreatedAt: r.CreatedAt, StartedAt: r.StartedAt, FinishedAt: r.FinishedAt,
	}
	if r.OriginActor != "" {
		v.Origin += " by " + r.OriginActor
	}
	if base := r.ChangesBase(); base != "" {
		if v.SHA == "" {
			v.SHA = r.Workflow.SHA
		}
		v.Changes = &mcpChanges{Base: base}
	} else if w := r.Workflow; w != nil {
		if v.SHA == "" {
			v.SHA = w.SHA
		}
		v.Workflow = &mcpWorkflow{
			File: w.Workflow, Event: w.Event.Name, WorkflowRunID: w.WorkflowRunID,
			RunNumber: w.RunNumber, RunAttempt: w.RunAttempt, Matrix: w.Matrix, Local: w.Local,
		}
	}
	if r.State == protocol.StateAwaitingApproval {
		v.Waiting = fmt.Sprintf("a person at this machine: its ref is outside the repository's allowed refs, "+
			"so it builds only when they run `latem approve %s` there. An agent cannot approve it.", r.ID)
	}
	if detail {
		v.Inputs, v.CancelReason, v.ExpiresAt = r.Inputs, r.CancelReason, r.ExpiresAt
		v.ApprovedAt, v.ApprovedSHA = r.ApprovedAt, r.ApprovedSHA
	}
	return v
}

// mcpRunOf reads one run, with the answer `latem logs` gives for an unknown id.
func mcpRunOf(ctx context.Context, st *state, runID string) (store.Run, error) {
	if err := protocol.ValidateID(runID); err != nil {
		return store.Run{}, fmt.Errorf("runId %q is not a run id: listRuns gives them", runID)
	}
	r, err := st.db.Get(ctx, runID)
	if isNotFound(err) {
		return store.Run{}, fmt.Errorf("no run %s on this machine: listRuns shows what there is", runID)
	}
	return r, err
}

func mcpListRuns(ctx context.Context, raw json.RawMessage) (any, error) {
	var a struct {
		Repo  string   `json:"repo"`
		SHA   string   `json:"sha"`
		State []string `json:"state"`
		Limit int      `json:"limit"`
	}
	if err := mcpArgs(raw, &a); err != nil {
		return nil, err
	}
	f := store.Filter{Repo: a.Repo, Limit: a.Limit}
	if f.Limit <= 0 {
		f.Limit = mcpDefaultRuns
	}
	f.Limit = min(f.Limit, mcpMaxRuns)
	if a.SHA != "" {
		sha := strings.ToLower(a.SHA)
		if !isFullSHA(sha) {
			return nil, fmt.Errorf("sha %q is not a full commit: give all 40 hex characters, as git rev-parse HEAD prints them", a.SHA)
		}
		f.SHA = sha
	}
	for _, s := range a.State {
		if !protocol.RunState(s).Valid() {
			return nil, fmt.Errorf("state %q is not a run state: %s", s, strings.Join(runStateNames(), ", "))
		}
		f.States = append(f.States, protocol.RunState(s))
	}

	st, closeState, err := openState(ctx)
	if err != nil {
		return nil, err
	}
	defer closeState()
	runs, err := st.db.List(ctx, f)
	if err != nil {
		return nil, err
	}
	out := struct {
		Runs []mcpRun `json:"runs"`
		Note string   `json:"note,omitempty"`
	}{Runs: make([]mcpRun, 0, len(runs))}
	for _, r := range runs {
		out.Runs = append(out.Runs, runView(r, false))
	}
	if len(runs) == 0 && f.SHA != "" {
		out.Note = "nothing of that commit on this machine: it may not be pushed yet, its jobs may be on another machine, " +
			"or its runs may have been pruned. A commit is found once a job of it is queued here."
	}
	return out, nil
}

// isFullSHA is a SHA-1 or SHA-256 object name in lower-case hex.
func isFullSHA(s string) bool {
	if len(s) != 40 && len(s) != 64 {
		return false
	}
	return strings.IndexFunc(s, func(r rune) bool { return !strings.ContainsRune("0123456789abcdef", r) }) < 0
}

func mcpGetRun(ctx context.Context, raw json.RawMessage) (any, error) {
	var a struct {
		RunID string `json:"runId"`
	}
	if err := mcpArgs(raw, &a); err != nil {
		return nil, err
	}
	st, closeState, err := openState(ctx)
	if err != nil {
		return nil, err
	}
	defer closeState()
	r, err := mcpRunOf(ctx, st, a.RunID)
	if err != nil {
		return nil, err
	}
	return runView(r, true), nil
}

type mcpLogLine struct {
	N    int    `json:"n"`
	Text string `json:"text"`
}

func mcpGetRunLogs(ctx context.Context, raw json.RawMessage) (any, error) {
	var a struct {
		RunID string `json:"runId"`
		Tail  int    `json:"tail"`
		After *int   `json:"after"`
		Limit int    `json:"limit"`
	}
	if err := mcpArgs(raw, &a); err != nil {
		return nil, err
	}
	if a.After != nil && a.Tail > 0 {
		return nil, errors.New("tail and after are two ways to read: give one")
	}
	if a.After != nil && *a.After < 0 {
		return nil, errors.New("after is a line number, 0 for the first page")
	}
	st, closeState, err := openState(ctx)
	if err != nil {
		return nil, err
	}
	defer closeState()
	r, err := mcpRunOf(ctx, st, a.RunID)
	if err != nil {
		return nil, err
	}

	out := struct {
		RunID   string            `json:"runId"`
		State   protocol.RunState `json:"state"`
		Settled bool              `json:"settled"`
		Lines   []mcpLogLine      `json:"lines"`
		// Next is the last line number read: after=next reads on from there.
		Next int `json:"next"`
		// More is set when lines after Next were there to read.
		More bool   `json:"more"`
		Note string `json:"note,omitempty"`
	}{RunID: r.ID, State: r.State, Settled: r.State.IsTerminal(), Lines: []mcpLogLine{}}

	rc, err := readLog(st.cfg.Paths.Logs, r.ID)
	if errors.Is(err, os.ErrNotExist) {
		out.Note = "no log: the run has not started, or agent.log_retention has expired it"
		return out, nil
	}
	if err != nil {
		return nil, err
	}
	defer func() { _ = rc.Close() }()

	if a.After != nil {
		limit := a.Limit
		if limit <= 0 {
			limit = mcpDefaultPage
		}
		limit = min(limit, mcpMaxLines)
		out.Next = *a.After
		err = eachLogLine(rc, func(n int, text string) bool {
			if n <= *a.After {
				return true
			}
			if len(out.Lines) == limit {
				out.More = true
				return false
			}
			out.Lines = append(out.Lines, mcpLogLine{n, text})
			out.Next = n
			return true
		})
		return out, err
	}

	tail := a.Tail
	if tail <= 0 {
		tail = mcpDefaultTail
	}
	tail = min(tail, mcpMaxLines)
	// The last lines, in a ring, so a long log is read through once and never
	// held whole.
	ring := make([]mcpLogLine, tail)
	seen := 0
	err = eachLogLine(rc, func(n int, text string) bool {
		ring[seen%tail] = mcpLogLine{n, text}
		seen++
		return true
	})
	out.Next = seen
	if seen <= tail {
		out.Lines = ring[:seen]
	} else {
		start := seen % tail
		out.Lines = append(append(make([]mcpLogLine, 0, tail), ring[start:]...), ring[:start]...)
	}
	return out, err
}

// eachLogLine calls fn with each line of a log and its number from 1, cut to
// mcpMaxLineBytes, until fn returns false.
func eachLogLine(rc io.Reader, fn func(n int, text string) bool) error {
	br := bufio.NewReader(rc)
	for n := 1; ; n++ {
		line, err := br.ReadString('\n')
		if line == "" && err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		line = strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r")
		if len(line) > mcpMaxLineBytes {
			cut := mcpMaxLineBytes
			for cut > 0 && !utf8.RuneStart(line[cut]) {
				cut--
			}
			line = line[:cut] + fmt.Sprintf(" … (%d bytes more)", len(line)-cut)
		}
		if !fn(n, line) {
			return nil
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
	}
}

func mcpWaitForRun(ctx context.Context, raw json.RawMessage) (any, error) {
	var a struct {
		RunID          string `json:"runId"`
		TimeoutSeconds int    `json:"timeoutSeconds"`
	}
	if err := mcpArgs(raw, &a); err != nil {
		return nil, err
	}
	wait := mcpDefaultWait
	if a.TimeoutSeconds > 0 {
		wait = min(time.Duration(a.TimeoutSeconds)*time.Second, mcpMaxWait)
	}
	st, closeState, err := openState(ctx)
	if err != nil {
		return nil, err
	}
	defer closeState()

	type answer struct {
		Settled bool   `json:"settled"`
		Run     mcpRun `json:"run"`
		Note    string `json:"note,omitempty"`
	}
	agentUp := st.client.AgentRunning(ctx)
	deadline := time.NewTimer(wait)
	defer deadline.Stop()
	tick := time.NewTicker(mcpPoll)
	defer tick.Stop()
	for {
		r, err := mcpRunOf(ctx, st, a.RunID)
		if err != nil {
			return nil, err
		}
		v := runView(r, false)
		switch {
		case r.State.IsTerminal():
			return answer{Settled: true, Run: v}, nil
		case r.State == protocol.StateAwaitingApproval:
			return answer{Run: v, Note: "held for a person's approval: waiting cannot settle it"}, nil
		case r.State == protocol.StateQueued && !agentUp:
			return answer{Run: v, Note: "queued, and no agent is running on this machine: it starts when one does (`latem agent`, or the service)"}, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-deadline.C:
			return answer{Run: v, Note: fmt.Sprintf("still %s after %s: call again to go on waiting", r.State, wait)}, nil
		case <-tick.C:
		}
	}
}

func mcpStartRun(ctx context.Context, raw json.RawMessage) (any, error) {
	var a struct {
		Repo    string         `json:"repo"`
		Job     string         `json:"job"`
		Ref     string         `json:"ref"`
		Inputs  map[string]any `json:"inputs"`
		Changes *changeSet     `json:"changes"`
	}
	if err := mcpArgs(raw, &a); err != nil {
		return nil, err
	}
	if a.Repo == "" || a.Job == "" {
		return nil, errors.New("startRun needs repo and job")
	}
	inputs := make(map[string]string, len(a.Inputs))
	for k, v := range a.Inputs {
		if strings.TrimSpace(k) == "" {
			return nil, errors.New("an input has an empty name")
		}
		switch v := v.(type) {
		case string:
			inputs[k] = v
		case bool:
			inputs[k] = strconv.FormatBool(v)
		case float64:
			inputs[k] = strconv.FormatFloat(v, 'f', -1, 64)
		default:
			return nil, fmt.Errorf("input %s is a %T: an input is a string, a number or a boolean", k, v)
		}
	}

	st, closeState, err := openState(ctx)
	if err != nil {
		return nil, err
	}
	defer closeState()
	if _, ok := st.cfg.Repos[a.Repo]; !ok {
		names := make([]string, 0, len(st.cfg.Repos))
		for k := range st.cfg.Repos {
			names = append(names, k)
		}
		slices.Sort(names)
		return nil, fmt.Errorf("this machine builds no repository %q: it builds %s", a.Repo, strings.Join(names, ", "))
	}

	id, err := agent.NewRunID()
	if err != nil {
		return nil, err
	}
	if a.Changes != nil {
		return mcpStartChangesRun(ctx, st, id, a.Repo, a.Job, a.Ref, inputs, a.Changes)
	}
	ref := a.Ref
	if ref == "" {
		ref = DefaultRef
	}
	req := protocol.RunRequest{
		ID: id, Repo: a.Repo, Job: a.Job, Ref: ref, Inputs: inputs,
		Origin: protocol.Origin{Kind: "cli", Actor: currentActor() + " via latem mcp", RequestedAt: time.Now()},
	}
	sub, err := st.client.Submit(ctx, req, local.SubmitOptions{ExpiresAt: time.Now().Add(st.queueTTL())})
	if err != nil {
		return nil, st.agentDown(err)
	}
	out := struct {
		RunID        string `json:"runId"`
		AgentRunning bool   `json:"agentRunning"`
		Note         string `json:"note"`
	}{RunID: sub.RunID, AgentRunning: sub.AgentRunning, Note: "queued: waitForRun follows it"}
	if !sub.AgentRunning {
		out.Note = "queued, and no agent is running on this machine: it starts when one does"
	}
	return out, nil
}

// mcpStartChangesRun is startRun with changes (A91): an agent's files on a
// commit this machine has, as a run nobody pushed. The refusals are the
// operator's rules, and say what a person would have to do.
func mcpStartChangesRun(
	ctx context.Context, st *state, id, repo, job, ref string, inputs map[string]string, cs *changeSet,
) (any, error) {
	normalized, _, err := tryRef(ref, "", DefaultRef)
	if err != nil {
		return nil, err
	}
	queued, err := startChangesRun(ctx, st, id, changesRun{
		repo: repo, job: job, ref: normalized, inputs: inputs,
		actor: currentActor() + " via latem mcp", agent: true,
	}, cs)
	if err != nil {
		return nil, st.agentDown(err)
	}
	out := struct {
		RunID        string `json:"runId"`
		SHA          string `json:"sha"`
		Base         string `json:"base"`
		ChangedPaths int    `json:"changedPaths"`
		AgentRunning bool   `json:"agentRunning"`
		Note         string `json:"note"`
	}{RunID: queued.RunID, SHA: queued.SHA, Base: queued.Base, ChangedPaths: queued.Paths,
		AgentRunning: queued.AgentRunning, Note: "queued: waitForRun follows it"}
	switch {
	case !queued.AgentRunning:
		out.Note = "queued, and no agent is running on this machine: it starts when one does"
	case queued.SHA == queued.Base:
		out.Note = "queued. The changes leave the base as it is, so this builds the base: waitForRun follows it"
	}
	return out, nil
}

func mcpCancelRun(ctx context.Context, raw json.RawMessage) (any, error) {
	var a struct {
		RunID  string `json:"runId"`
		Reason string `json:"reason"`
	}
	if err := mcpArgs(raw, &a); err != nil {
		return nil, err
	}
	st, closeState, err := openState(ctx)
	if err != nil {
		return nil, err
	}
	defer closeState()
	if _, err := mcpRunOf(ctx, st, a.RunID); err != nil {
		return nil, err
	}
	req := protocol.CancelRequest{ID: a.RunID, Actor: currentActor() + " via latem mcp", Reason: a.Reason}
	if err := st.client.Cancel(ctx, req); err != nil {
		return nil, st.agentDown(err)
	}
	r, err := mcpRunOf(ctx, st, a.RunID)
	if err != nil {
		return nil, err
	}
	out := struct {
		Run  mcpRun `json:"run"`
		Note string `json:"note,omitempty"`
	}{Run: runView(r, false)}
	if !r.State.IsTerminal() {
		out.Note = "asked to stop: waitForRun shows when it has"
	}
	return out, nil
}

func mcpCheckWorkflows(ctx context.Context, raw json.RawMessage) (any, error) {
	var a struct {
		Repo    string `json:"repo"`
		Offline bool   `json:"offline"`
	}
	if err := mcpArgs(raw, &a); err != nil {
		return nil, err
	}
	if a.Repo == "" {
		return nil, errors.New("checkWorkflows needs repo")
	}
	// The command itself, with --json, writing to a buffer: one check, one
	// report, whether a person or an agent asked.
	var out, diag bytes.Buffer
	c := &cobra.Command{}
	c.SetContext(ctx)
	c.SetIn(strings.NewReader(""))
	c.SetOut(&out)
	c.SetErr(&diag)
	err := runWorkflowsCheckRepo(c, workflowsCheckFlags{repo: a.Repo, offline: a.Offline, json: true})
	if report := bytes.TrimSpace(out.Bytes()); len(report) > 0 && json.Valid(report) {
		// Exit 1 with a report is a job this machine cannot run, which the
		// report says; it is the answer, not a failure of the tool.
		return json.RawMessage(report), nil
	}
	if err != nil {
		if msg := strings.TrimSpace(diag.String()); msg != "" {
			return nil, fmt.Errorf("%w\n%s", err, msg)
		}
		return nil, err
	}
	return nil, errors.New("workflows check printed no report")
}
