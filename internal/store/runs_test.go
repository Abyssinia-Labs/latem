package store

import (
	"encoding/json"
	"errors"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Abyssinia-Labs/latem/pkg/protocol"
)

func TestCreateGet_RoundTripsEveryField(t *testing.T) {
	d := newTestDB(t)

	started := time.Date(2026, 8, 7, 9, 1, 0, 0, time.UTC)
	finished := time.Date(2026, 8, 7, 9, 9, 30, 123_456_789, time.UTC)
	expires := time.Date(2026, 8, 8, 9, 0, 0, 0, time.UTC)
	exit := 137

	want := Run{
		ID:   "run-full",
		Repo: "myapp",
		Job:  "ios-release",
		// Both spellings are kept so an audit can answer "requested as main,
		// matched refs/heads/main" without re-deriving it from the mirror.
		Ref:           "main",
		RefNormalized: "refs/heads/main",
		SHA:           "9f1c2b3a4d5e6f708192a3b4c5d6e7f809a1b2c3",
		Runner:        "mac-tart",
		State:         protocol.StateFailed,
		ExitCode:      &exit,
		ErrorCode:     protocol.ErrStep,
		ErrorMsg:      "step 3 exited 137",
		OriginKind:    "webhook",
		OriginActor:   "github:octocat",
		CancelReason:  "superseded by a newer push",
		Inputs:        map[string]string{"LANE": "beta", "CLEAN": "true"},
		CreatedAt:     time.Date(2026, 8, 7, 9, 0, 0, 0, time.UTC),
		StartedAt:     &started,
		FinishedAt:    &finished,
		ExpiresAt:     &expires,
	}
	mustCreate(t, d, want)

	got, err := d.Get(t.Context(), want.ID)
	if err != nil {
		t.Fatal(err)
	}

	if got.ID != want.ID || got.Repo != want.Repo || got.Job != want.Job {
		t.Errorf("identity = %q/%q/%q", got.ID, got.Repo, got.Job)
	}
	if got.Ref != want.Ref || got.RefNormalized != want.RefNormalized || got.SHA != want.SHA {
		t.Errorf("ref fields = %q/%q/%q", got.Ref, got.RefNormalized, got.SHA)
	}
	if got.Runner != want.Runner || got.State != want.State {
		t.Errorf("runner/state = %q/%q", got.Runner, got.State)
	}
	if got.ExitCode == nil || *got.ExitCode != exit {
		t.Errorf("exit code = %v, want %d", got.ExitCode, exit)
	}
	if got.ErrorCode != want.ErrorCode || got.ErrorMsg != want.ErrorMsg {
		t.Errorf("error = %q/%q", got.ErrorCode, got.ErrorMsg)
	}
	if got.OriginKind != want.OriginKind || got.OriginActor != want.OriginActor {
		t.Errorf("origin = %q/%q", got.OriginKind, got.OriginActor)
	}
	if got.CancelReason != want.CancelReason {
		t.Errorf("cancel reason = %q", got.CancelReason)
	}
	if len(got.Inputs) != 2 || got.Inputs["LANE"] != "beta" || got.Inputs["CLEAN"] != "true" {
		t.Errorf("inputs = %v", got.Inputs)
	}
	if !got.CreatedAt.Equal(want.CreatedAt) {
		t.Errorf("created_at = %v, want %v", got.CreatedAt, want.CreatedAt)
	}
	// Nanosecond precision survives: the ordering of runs created in the same
	// millisecond depends on it.
	if got.FinishedAt == nil || !got.FinishedAt.Equal(finished) {
		t.Errorf("finished_at = %v, want %v", got.FinishedAt, finished)
	}
	if got.StartedAt == nil || !got.StartedAt.Equal(started) {
		t.Errorf("started_at = %v, want %v", got.StartedAt, started)
	}
	if got.ExpiresAt == nil || !got.ExpiresAt.Equal(expires) {
		t.Errorf("expires_at = %v, want %v", got.ExpiresAt, expires)
	}
}

func TestCreate_EmptyOptionalsReadBackAsZero(t *testing.T) {
	d := newTestDB(t)
	mustCreate(t, d, testRun("bare", protocol.StateQueued))

	got, err := d.Get(t.Context(), "bare")
	if err != nil {
		t.Fatal(err)
	}
	switch {
	case got.RefNormalized != "", got.SHA != "", got.Runner != "":
		t.Errorf("unset text columns came back as %q/%q/%q", got.RefNormalized, got.SHA, got.Runner)
	case got.ExitCode != nil, got.StartedAt != nil, got.FinishedAt != nil, got.ExpiresAt != nil:
		t.Error("unset optional columns came back non-nil")
	case len(got.Inputs) != 0:
		t.Errorf("inputs = %v, want empty", got.Inputs)
	}
}

func TestCreate_Rejects(t *testing.T) {
	d := newTestDB(t)

	tests := []struct {
		name   string
		mutate func(*Run)
	}{
		// The id becomes a primary key, a workspace path, and a container name.
		{"empty id", func(r *Run) { r.ID = "" }},
		{"path traversal in id", func(r *Run) { r.ID = "../../etc/passwd" }},
		{"slash in id", func(r *Run) { r.ID = "a/b" }},
		{"empty repo", func(r *Run) { r.Repo = "" }},
		{"empty job", func(r *Run) { r.Job = "" }},
		{"empty ref", func(r *Run) { r.Ref = "" }},
		{"unknown state", func(r *Run) { r.State = protocol.RunState("almost_done") }},
		{"empty state", func(r *Run) { r.State = "" }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := testRun("valid-id", protocol.StateQueued)
			tc.mutate(&r)
			if err := d.Create(t.Context(), r); err == nil {
				t.Fatalf("Create accepted %s", tc.name)
			}
		})
	}
}

// TestCreate_Duplicate: run ids arrive from outside the trust boundary, so a
// collision must be refused rather than overwrite a run in flight.
func TestCreate_Duplicate(t *testing.T) {
	d := newTestDB(t)
	r := mustCreate(t, d, testRun("dup", protocol.StateQueued))

	err := d.Create(t.Context(), r)
	if !errors.Is(err, ErrExists) {
		t.Fatalf("second Create = %v, want ErrExists", err)
	}
}

func TestCreate_DefaultsCreatedAt(t *testing.T) {
	d := newTestDB(t)

	r := testRun("no-timestamp", protocol.StateQueued)
	r.CreatedAt = time.Time{}
	before := time.Now().Add(-time.Second)
	mustCreate(t, d, r)

	got, err := d.Get(t.Context(), r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.CreatedAt.Before(before) {
		t.Errorf("created_at = %v, want approximately now", got.CreatedAt)
	}
}

// TestCreate_ScrubsInputsAndCancelReason is INV-4 at the persistence boundary
// (§22: "inputs_json and cancel_reason MUST be scrubbed through the redactor
// before persistence"). It asserts the marker is present, not merely that the
// secret is absent — a redactor that dropped the column entirely would satisfy
// the weaker check.
func TestCreate_ScrubsInputsAndCancelReason(t *testing.T) {
	const secret = "s3cr3t-app-store-key"
	d := newTestDB(t, secret)

	r := testRun("scrubbed", protocol.StateQueued)
	r.Inputs = map[string]string{"TOKEN": "prefix-" + secret + "-suffix", "LANE": "beta"}
	r.CancelReason = "cancelled while holding " + secret
	r.ErrorMsg = "failed with " + secret
	mustCreate(t, d, r)

	// Read the raw column, not the decoded struct: the point is what is on disk.
	var inputs, reason, errMsg string
	err := d.db.QueryRowContext(t.Context(),
		`SELECT inputs_json, cancel_reason, error_message FROM runs WHERE id = ?`, r.ID).
		Scan(&inputs, &reason, &errMsg)
	if err != nil {
		t.Fatal(err)
	}

	for name, col := range map[string]string{
		"inputs_json": inputs, "cancel_reason": reason, "error_message": errMsg,
	} {
		if strings.Contains(col, secret) {
			t.Errorf("%s holds the secret verbatim: %q", name, col)
		}
		if !strings.Contains(col, "REDACTED") {
			t.Errorf("%s = %q; the value did not pass through the redactor", name, col)
		}
	}
	if !strings.Contains(inputs, "beta") {
		t.Errorf("inputs_json = %q; a non-secret input was lost", inputs)
	}
}

func TestGet_Missing(t *testing.T) {
	d := newTestDB(t)
	if _, err := d.Get(t.Context(), "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get of an unknown run = %v, want ErrNotFound", err)
	}
}

func TestList(t *testing.T) {
	d := newTestDB(t)
	base := time.Date(2026, 8, 7, 9, 0, 0, 0, time.UTC)

	seed := []struct {
		id    string
		repo  string
		state protocol.RunState
		age   time.Duration
	}{
		{"a-old", "myapp", protocol.StateSucceeded, 3 * time.Hour},
		{"b-mid", "myapp", protocol.StateFailed, 2 * time.Hour},
		{"c-new", "myapp", protocol.StateRunning, time.Hour},
		{"d-other", "otherapp", protocol.StateRunning, time.Minute},
	}
	for _, s := range seed {
		r := testRun(s.id, s.state)
		r.Repo = s.repo
		r.CreatedAt = base.Add(-s.age)
		mustCreate(t, d, r)
	}

	ids := func(runs []Run) []string {
		out := make([]string, len(runs))
		for i, r := range runs {
			out[i] = r.ID
		}
		return out
	}

	tests := []struct {
		name   string
		filter Filter
		want   []string
	}{
		{"newest first", Filter{}, []string{"d-other", "c-new", "b-mid", "a-old"}},
		{"by repo", Filter{Repo: "myapp"}, []string{"c-new", "b-mid", "a-old"}},
		{"by state", Filter{States: []protocol.RunState{protocol.StateRunning}}, []string{"d-other", "c-new"}},
		{
			"repo and state",
			Filter{Repo: "myapp", States: []protocol.RunState{protocol.StateFailed, protocol.StateSucceeded}},
			[]string{"b-mid", "a-old"},
		},
		{"limit", Filter{Limit: 2}, []string{"d-other", "c-new"}},
		{"no match", Filter{Repo: "ghost"}, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := d.List(t.Context(), tc.filter)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Join(ids(got), ",") != strings.Join(tc.want, ",") {
				t.Errorf("List = %v, want %v", ids(got), tc.want)
			}
		})
	}
}

// TestList_DefaultLimit: an unbounded list is fine on a new agent and returns
// a year of history on an old one.
func TestList_DefaultLimit(t *testing.T) {
	d := newTestDB(t)
	base := time.Date(2026, 8, 7, 9, 0, 0, 0, time.UTC)

	for i := range DefaultListLimit + 10 {
		r := testRun("run-"+strconv.Itoa(i), protocol.StateSucceeded)
		r.CreatedAt = base.Add(time.Duration(i) * time.Second)
		mustCreate(t, d, r)
	}

	got, err := d.List(t.Context(), Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != DefaultListLimit {
		t.Errorf("List returned %d runs, want the default cap of %d", len(got), DefaultListLimit)
	}
}

// TestNonTerminal covers RUNNER_SPEC §6 step 5: exactly the runs a crash could
// have left mid-flight, derived from protocol's terminal set rather than a
// hand-written list.
func TestNonTerminal(t *testing.T) {
	d := newTestDB(t)
	base := time.Date(2026, 8, 7, 9, 0, 0, 0, time.UTC)

	var wantIDs []string
	i := 0
	for _, s := range []protocol.RunState{
		protocol.StateQueued, protocol.StateAwaitingApproval, protocol.StateClaimed,
		protocol.StatePreparing, protocol.StateRunning, protocol.StateCollecting,
		protocol.StateSucceeded, protocol.StateFailed, protocol.StateTimedOut,
		protocol.StateCancelled, protocol.StateRejected, protocol.StateInterrupted,
	} {
		r := testRun("run-"+string(s), s)
		r.CreatedAt = base.Add(time.Duration(i) * time.Minute)
		mustCreate(t, d, r)
		if !s.IsTerminal() {
			wantIDs = append(wantIDs, r.ID)
		}
		i++
	}

	got, err := d.NonTerminal(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(wantIDs) {
		t.Fatalf("NonTerminal returned %d runs, want %d", len(got), len(wantIDs))
	}
	for i, r := range got {
		if r.ID != wantIDs[i] {
			t.Errorf("NonTerminal[%d] = %s, want %s (oldest first)", i, r.ID, wantIDs[i])
		}
		if r.State.IsTerminal() {
			t.Errorf("NonTerminal returned %s, which is terminal", r.State)
		}
	}
}

func TestSetRunnerExpiryAndCancelReason(t *testing.T) {
	const secret = "hunter2-token-value"
	d := newTestDB(t, secret)
	mustCreate(t, d, testRun("aux", protocol.StateQueued))

	deadline := time.Date(2026, 8, 9, 0, 0, 0, 0, time.UTC)
	if err := d.SetRunner(t.Context(), "aux", "mac-tart"); err != nil {
		t.Fatal(err)
	}
	if err := d.SetExpiry(t.Context(), "aux", deadline); err != nil {
		t.Fatal(err)
	}
	if err := d.SetCancelReason(t.Context(), "aux", "stopped, token was "+secret); err != nil {
		t.Fatal(err)
	}

	got, err := d.Get(t.Context(), "aux")
	if err != nil {
		t.Fatal(err)
	}
	if got.Runner != "mac-tart" {
		t.Errorf("runner = %q", got.Runner)
	}
	if got.ExpiresAt == nil || !got.ExpiresAt.Equal(deadline) {
		t.Errorf("expires_at = %v, want %v", got.ExpiresAt, deadline)
	}
	if strings.Contains(got.CancelReason, secret) {
		t.Errorf("cancel_reason holds the secret: %q", got.CancelReason)
	}
	if !strings.Contains(got.CancelReason, "REDACTED") {
		t.Errorf("cancel_reason = %q; it did not pass through the redactor", got.CancelReason)
	}

	// A zero time clears the deadline, which is how a run leaves a waiting
	// state by any path other than Transition.
	if err := d.SetExpiry(t.Context(), "aux", time.Time{}); err != nil {
		t.Fatal(err)
	}
	if got, err = d.Get(t.Context(), "aux"); err != nil || got.ExpiresAt != nil {
		t.Errorf("expires_at = %v after clearing, %v", got.ExpiresAt, err)
	}

	for _, tc := range []struct {
		name string
		err  error
	}{
		{"SetRunner", d.SetRunner(t.Context(), "ghost", "docker")},
		{"SetExpiry", d.SetExpiry(t.Context(), "ghost", deadline)},
		{"SetCancelReason", d.SetCancelReason(t.Context(), "ghost", "why")},
	} {
		if !errors.Is(tc.err, ErrNotFound) {
			t.Errorf("%s on an unknown run = %v, want ErrNotFound", tc.name, tc.err)
		}
	}
	if err := d.SetRunner(t.Context(), "aux", ""); err == nil {
		t.Error("SetRunner accepted an empty runner")
	}
}

// A workflow run's job keeps its workflow fields across a restart, and never
// its token (A52).
func TestCreate_WorkflowRunAndToken(t *testing.T) {
	d := newTestDB(t)
	ctx := t.Context()
	req := protocol.RunRequest{
		ID: "r-wf", Repo: "app", Job: "web", Ref: "refs/heads/main",
		Workflow: ".github/workflows/checks.yml", SHA: strings.Repeat("a", 40),
		Event: &protocol.Event{Name: "push", Ref: "refs/heads/main", SHA: strings.Repeat("a", 40),
			Payload: []byte(`{"head_commit":{"message":"hi"}}`)},
		Needs:         map[string]protocol.NeedResult{"lint": {Result: "success", Outputs: map[string]string{"v": "1"}}},
		WorkflowRunID: "wr1", RunNumber: 7, RunAttempt: 1,
		Token: "ghs_secret", FetchToken: "ghs_fetch",
	}
	run := Run{ID: req.ID, Repo: req.Repo, Job: req.Job, Ref: req.Ref, State: protocol.StateQueued,
		OriginKind: "webhook", Workflow: WorkflowRunOf(req)}
	if err := d.Create(ctx, run); err != nil {
		t.Fatal(err)
	}
	d.HoldToken(req.ID, req.Token)
	d.HoldFetchToken(req.ID, req.FetchToken)

	got, err := d.Get(ctx, req.ID)
	if err != nil {
		t.Fatal(err)
	}
	w := got.Workflow
	if w == nil || w.Workflow != req.Workflow || w.SHA != req.SHA || w.Event.Name != "push" ||
		string(w.Event.Payload) != `{"head_commit":{"message":"hi"}}` ||
		w.Needs["lint"].Outputs["v"] != "1" || w.WorkflowRunID != "wr1" || w.RunNumber != 7 {
		t.Fatalf("workflow run did not survive: %+v", w)
	}
	var raw string
	if err := d.db.QueryRowContext(ctx, `SELECT workflow_json FROM runs WHERE id = ?`, req.ID).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(raw, "ghs_secret") || strings.Contains(raw, "ghs_fetch") {
		t.Fatal("a token reached the database")
	}
	if d.Token(req.ID) != "ghs_secret" || d.FetchToken(req.ID) != "ghs_fetch" {
		t.Fatal("a token is not held in memory")
	}
	for _, to := range []protocol.RunState{protocol.StateClaimed, protocol.StatePreparing, protocol.StateSkipped} {
		if err := d.Transition(ctx, req.ID, to, protocol.Update{State: to}); err != nil {
			t.Fatal(err)
		}
	}
	if d.Token(req.ID) != "" || d.FetchToken(req.ID) != "" {
		t.Error("a token outlived the run")
	}
}

// A run latem try made keeps its mark and its approval, and nothing a broker
// sends can carry the mark (A76): a run request has no field for it, so a run
// made from one is never local, and a frame naming one does not decode.
func TestCreate_LocalRunIsMarkedOnlyByItsRow(t *testing.T) {
	d := newTestDB(t)
	ctx := t.Context()
	sha := strings.Repeat("b", 40)
	now := time.Now()
	run := Run{ID: "r-try", Repo: "app", Job: "test", Ref: "refs/heads/feature/x", State: protocol.StateQueued,
		OriginKind: "cli", ApprovedAt: &now, ApprovedSHA: sha,
		Workflow: &WorkflowRun{Workflow: ".github/workflows/checks.yml", SHA: sha, WorkflowRunID: "r-try",
			Event: protocol.Event{Name: "push", Ref: "refs/heads/feature/x", SHA: sha}, Local: true}}
	if err := d.Create(ctx, run); err != nil {
		t.Fatal(err)
	}
	got, err := d.Get(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !got.IsLocal() || got.ApprovedSHA != sha || got.ApprovedAt == nil {
		t.Fatalf("a local run lost its mark or its approval: local=%v approved=%q", got.IsLocal(), got.ApprovedSHA)
	}

	req := protocol.RunRequest{ID: "r-wf", Repo: "app", Job: "test", Ref: "refs/heads/main",
		Workflow: ".github/workflows/checks.yml", SHA: sha, WorkflowRunID: "wr",
		Event: &protocol.Event{Name: "push", Ref: "refs/heads/main", SHA: sha}}
	if w := WorkflowRunOf(req); w == nil || w.Local {
		t.Fatalf("a run request made a local run: %+v", w)
	}
	frame := `{"kind":"run","run":{"id":"r1","repo":"app","job":"test","ref":"main","origin":{"kind":"cli","requested_at":"2026-11-07T00:00:00Z"},` +
		`"workflow":".github/workflows/checks.yml","sha":"` + sha + `","workflow_run_id":"r1",` +
		`"event":{"name":"push","ref":"refs/heads/main","sha":"` + sha + `"},"local":true}}`
	var cmd protocol.Command
	if err := json.Unmarshal([]byte(frame), &cmd); err == nil {
		t.Fatal("a run command carrying local decoded")
	}
}

// A download-artifact finds the run that uploaded what it reads by the
// workflow run they share (A84): the named jobs of that workflow run, in that
// repository, newest first, and nothing of another workflow run or another
// repository however alike — through an index, never a scan of every run.
func TestWorkflowRunJobs(t *testing.T) {
	d := newTestDB(t)
	at := time.Date(2026, 11, 8, 9, 0, 0, 0, time.UTC)
	const sha = "0123456789abcdef0123456789abcdef01234567"
	add := func(id, repo, job, workflowRun string, minute int) {
		r := testRun(id, protocol.StateSucceeded)
		r.Repo, r.Job, r.SHA, r.CreatedAt = repo, job, sha, at.Add(time.Duration(minute)*time.Minute)
		r.Workflow = &WorkflowRun{Workflow: ".github/workflows/mac-release.yml", SHA: sha, WorkflowRunID: workflowRun,
			Event: protocol.Event{Name: "workflow_dispatch", Ref: "refs/heads/main", SHA: sha}}
		mustCreate(t, d, r)
	}
	add("r-build", "take-one", "build", "wr-1", 0)
	add("r-build-again", "take-one", "build", "wr-1", 5) // a second attempt, after a lost machine
	add("r-notes", "take-one", "notes", "wr-1", 1)
	add("r-other-run", "take-one", "build", "wr-2", 2)
	add("r-other-repo", "wake-cloud", "build", "wr-1", 3)
	mustCreate(t, d, testRun("r-native", protocol.StateSucceeded)) // a jobs.yaml run has no workflow run

	got, err := d.WorkflowRunJobs(t.Context(), "take-one", "wr-1", []string{"build", "nope"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, r := range got {
		ids = append(ids, r.ID)
	}
	if !slices.Equal(ids, []string{"r-build-again", "r-build"}) {
		t.Errorf("got %v, want build's two runs of wr-1, newest first", ids)
	}
	if got, _ := d.WorkflowRunJobs(t.Context(), "take-one", "wr-1", []string{"build", "notes"}, 1); len(got) != 1 {
		t.Errorf("limit 1 returned %d", len(got))
	}
	if got, err := d.WorkflowRunJobs(t.Context(), "take-one", "wr-1", nil, 0); err != nil || got != nil {
		t.Errorf("no jobs: %v, %v", got, err)
	}

	var detail strings.Builder
	rows, err := d.db.QueryContext(t.Context(), `EXPLAIN QUERY PLAN SELECT id FROM runs
	 WHERE repo = ? AND workflow_json IS NOT NULL
	   AND json_extract(workflow_json, '$.workflow_run_id') = ?
	   AND job IN (?) ORDER BY created_at DESC, id DESC LIMIT ?`, "take-one", "wr-1", "build", 10)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var id, parent, notused int
		var s string
		if err := rows.Scan(&id, &parent, &notused, &s); err != nil {
			t.Fatal(err)
		}
		detail.WriteString(s + "\n")
	}
	if !strings.Contains(detail.String(), "runs_workflow_run_idx") {
		t.Errorf("the lookup does not use runs_workflow_run_idx:\n%s", detail.String())
	}
}

// An agent asks what this machine built of the commit it pushed (A87): a run
// whose ref resolved to it, and a workflow job of it still in the queue, whose
// sha is not on the row yet — newest first, and nothing of another commit,
// through an index rather than a scan of every run kept.
func TestList_ByCommit(t *testing.T) {
	d := newTestDB(t)
	at := time.Date(2026, 11, 11, 9, 0, 0, 0, time.UTC)
	const sha = "0123456789abcdef0123456789abcdef01234567"
	const other = "fedcba9876543210fedcba9876543210fedcba98"
	add := func(id, runSHA, workflowSHA string, minute int) {
		r := testRun(id, protocol.StateSucceeded)
		r.SHA, r.CreatedAt = runSHA, at.Add(time.Duration(minute)*time.Minute)
		if workflowSHA != "" {
			r.State = protocol.StateQueued
			r.Workflow = &WorkflowRun{Workflow: ".github/workflows/checks.yml", SHA: workflowSHA, WorkflowRunID: "wr-" + id,
				Event: protocol.Event{Name: "push", Ref: "refs/heads/main", SHA: workflowSHA}}
		}
		mustCreate(t, d, r)
	}
	add("r-resolved", sha, "", 0)
	add("r-queued-job", "", sha, 1) // its ref not resolved yet
	add("r-other", other, "", 2)
	add("r-other-job", "", other, 3)
	add("r-unresolved", "", "", 4) // a jobs.yaml run still in the queue has no commit

	got, err := d.List(t.Context(), Filter{SHA: sha})
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, r := range got {
		ids = append(ids, r.ID)
	}
	if !slices.Equal(ids, []string{"r-queued-job", "r-resolved"}) {
		t.Errorf("got %v, want the two runs of %s, newest first", ids, sha)
	}
	if got, _ := d.List(t.Context(), Filter{SHA: sha, Repo: "elsewhere"}); len(got) != 0 {
		t.Errorf("another repository's filter returned %d", len(got))
	}
	if got, _ := d.List(t.Context(), Filter{SHA: sha, States: []protocol.RunState{protocol.StateQueued}}); len(got) != 1 {
		t.Errorf("queued runs of %s: got %d, want 1", sha, len(got))
	}

	var detail strings.Builder
	rows, err := d.db.QueryContext(t.Context(), `EXPLAIN QUERY PLAN SELECT id FROM runs
	 WHERE coalesce(sha, json_extract(workflow_json, '$.sha')) = ?
	 ORDER BY created_at DESC, id DESC LIMIT ?`, sha, 10)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var id, parent, notused int
		var s string
		if err := rows.Scan(&id, &parent, &notused, &s); err != nil {
			t.Fatal(err)
		}
		detail.WriteString(s + "\n")
	}
	if !strings.Contains(detail.String(), "runs_commit_idx") {
		t.Errorf("the lookup does not use runs_commit_idx:\n%s", detail.String())
	}
}
