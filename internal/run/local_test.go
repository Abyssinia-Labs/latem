package run

import (
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Abyssinia-Labs/latem/internal/store"
	"github.com/Abyssinia-Labs/latem/pkg/protocol"
)

// localSHA is a commit latem try made: in the mirror, on no ref.
const localSHA = "fedcba9876543210fedcba9876543210fedcba98"

// localRequest is a run latem try queues (A76): a workflow run's job pinned to
// a commit on no ref, named by a branch the mirror does not have, numbered by
// nobody, with no token.
func localRequest(h *harness, job string) protocol.RunRequest {
	req := workflowRequest(h, job)
	req.Ref = "refs/heads/feature/try"
	req.SHA = localSHA
	req.Event = &protocol.Event{Name: "push", Ref: req.Ref, SHA: localSHA, Before: h.source.sha, Actor: "tester",
		Payload: []byte(`{"ref":"refs/heads/feature/try","after":"` + localSHA + `"}`)}
	req.WorkflowRunID, req.RunNumber, req.RunAttempt = req.ID, 0, 1
	req.Token = ""
	return req
}

// queueLocal writes the row as latem try writes it — local, and approved for
// approved — and claims it. No broker path can make this row; that is what the
// store test for Local proves.
func (h *harness) queueLocal(req protocol.RunRequest, approved string) store.Run {
	h.t.Helper()
	ctx := h.t.Context()
	w := store.WorkflowRunOf(req)
	w.Local = true
	now := time.Now()
	r := store.Run{ID: req.ID, Repo: req.Repo, Job: req.Job, Ref: req.Ref, State: protocol.StateQueued,
		OriginKind: "cli", OriginActor: "tester", Workflow: w, ApprovedAt: &now, ApprovedSHA: approved}
	if err := h.db.Create(ctx, r); err != nil {
		h.t.Fatal(err)
	}
	if err := h.db.Enqueue(ctx, req.ID, 0); err != nil {
		h.t.Fatal(err)
	}
	if err := h.db.Claim(ctx, req.ID); err != nil {
		h.t.Fatal(err)
	}
	return mustGet(h.t, h.db, req.ID)
}

// A local run builds the commit latem try made without fetching, under a
// branch name nothing has to resolve, whatever allowed_refs says. Its job's
// if: is not evaluated — deploy's would be false for a push — and its needs
// are taken as succeeded. github.run_number is the machine's, and GITHUB_TOKEN
// is the machine's own secret of that name.
func TestExecute_ALocalRunBuildsItsCommitUnfetched(t *testing.T) {
	h := newHarness(t)
	h.source.registry = ""
	h.source.commits = map[string]bool{localSHA: false}
	h.cfg.Repos["app"].GitHub = "acme/app"
	o := h.build()

	req := localRequest(h, "deploy")
	r := h.queueLocal(req, localSHA)
	h.writeWorkspaceFile(req.ID, ".github/workflows/checks.yml", runWorkflowFile)
	res, err := o.Execute(t.Context(), r)
	if err != nil {
		t.Fatal(err)
	}
	if res.State != protocol.StateSucceeded {
		t.Fatalf("got %s/%s: %s", res.State, res.ErrorCode, res.ErrorMsg)
	}
	if len(h.source.fetched) != 0 || len(h.source.opened) != 1 {
		t.Errorf("fetched %d time(s) and opened %d: a local run's commit is already here", len(h.source.fetched), len(h.source.opened))
	}
	if got := h.source.checkedOut; len(got) != 1 || got[0] != [2]string{"refs/heads/feature/try", localSHA} {
		t.Errorf("checked out %v", got)
	}
	spec := h.env.runSpec()
	for k, want := range map[string]string{
		"GITHUB_REF": "refs/heads/feature/try", "GITHUB_REF_NAME": "feature/try", "GITHUB_SHA": localSHA,
		"GITHUB_EVENT_NAME": "push", "GITHUB_RUN_ID": req.ID, "GITHUB_RUN_NUMBER": strconv.FormatInt(r.Number, 10),
	} {
		if spec.Env[k] != want {
			t.Errorf("%s = %q, want %q", k, spec.Env[k], want)
		}
	}
	if r.Number <= 0 {
		t.Fatalf("the row has no run number (%d), so GITHUB_RUN_NUMBER proves nothing", r.Number)
	}
	logs := h.logContents(req.ID)
	for _, want := range []string{
		"latem try made from a working tree", "if: were not evaluated", "approved locally at fedcba987654",
	} {
		if !strings.Contains(logs, want) {
			t.Errorf("the log does not say %q:\n%s", want, logs)
		}
	}
	if got := mustGet(t, h.db, req.ID); got.RefNormalized != "refs/heads/feature/try" || got.SHA != localSHA {
		t.Errorf("recorded %s at %s", got.RefNormalized, got.SHA)
	}
}

// The machine's GITHUB_TOKEN reaches a try's step that names it, and the log
// says where it came from: no control plane minted one, and latem borrows no
// person's.
func TestExecute_ALocalRunsTokenIsTheMachines(t *testing.T) {
	const machine = "ghp_machineMACHINEmachine0123"
	h := newHarness(t)
	h.source.registry = ""
	h.source.commits = map[string]bool{localSHA: false}
	h.secret.values["app__GITHUB_TOKEN"] = machine
	o := h.build()

	req := localRequest(h, "web")
	r := h.queueLocal(req, localSHA)
	h.writeWorkspaceFile(req.ID, ".github/workflows/checks.yml", runWorkflowFile)
	res, err := o.Execute(t.Context(), r)
	if err != nil {
		t.Fatal(err)
	}
	if res.State != protocol.StateSucceeded {
		t.Fatalf("got %s/%s: %s", res.State, res.ErrorCode, res.ErrorMsg)
	}
	if got := h.env.executed()[0].Env["TOKEN"]; got != machine {
		t.Errorf("TOKEN set=%v; want the machine's own GITHUB_TOKEN", got != "")
	}
	logs := h.logContents(req.ID)
	if !strings.Contains(logs, "a try has no GitHub token") || !strings.Contains(logs, "needs lint taken as succeeded") {
		t.Errorf("the log does not say where the token came from, or what needs were assumed:\n%s", logs)
	}
	if strings.Contains(logs, machine) {
		t.Error("the machine's token reached the log")
	}
}

// What a local run refuses, before anything is checked out: no approval, an
// approval of another commit, a commit the mirror does not have, a machine
// with no mirror, and a ref that is not fully qualified.
func TestExecute_ALocalRunIsAdmittedOnlyByItsOwnApproval(t *testing.T) {
	cases := map[string]struct {
		approved string
		arrange  func(*harness, *protocol.RunRequest)
		want     string
	}{
		"not approved":         {approved: "", want: "admitted only by an approval of its own commit"},
		"approved for another": {approved: strings.Repeat("1", 40), want: "admitted only by an approval of its own commit"},
		"not in the mirror": {approved: localSHA, want: "is not in this machine's mirror",
			arrange: func(h *harness, _ *protocol.RunRequest) { h.source.commits = nil }},
		"no mirror": {approved: localSHA, want: "has no mirror of app",
			arrange: func(h *harness, _ *protocol.RunRequest) { h.source.noMirror = true }},
		"a short ref": {approved: localSHA, want: "neither a branch nor a tag",
			arrange: func(_ *harness, req *protocol.RunRequest) { req.Ref = "feature/try" }},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			h.source.registry = ""
			h.source.commits = map[string]bool{localSHA: false}
			o := h.build()
			req := localRequest(h, "web")
			if tc.arrange != nil {
				tc.arrange(h, &req)
			}
			res, err := o.Execute(t.Context(), h.queueLocal(req, tc.approved))
			if err != nil {
				t.Fatal(err)
			}
			if res.State != protocol.StateRejected || res.ErrorCode != protocol.ErrInvalidRef ||
				!strings.Contains(res.ErrorMsg, tc.want) {
				t.Fatalf("got %s/%s: %s; want rejected naming %q", res.State, res.ErrorCode, res.ErrorMsg, tc.want)
			}
			if len(h.source.checkedOut) != 0 || len(h.source.fetched) != 0 {
				t.Errorf("a refused local run checked out %v and fetched %d time(s)", h.source.checkedOut, len(h.source.fetched))
			}
		})
	}
}

// A workflow run's job from a control plane is unchanged: it fetches, and a
// commit that is not on its ref is refused even though the mirror holds it.
// The local path is reached only by the row's mark.
func TestExecute_ABrokersRunIsNeverLocal(t *testing.T) {
	h := newHarness(t)
	h.source.registry = ""
	h.source.commits = map[string]bool{localSHA: false}
	h.source.refs["refs/heads/feature/try"] = true
	o := h.build()
	req := localRequest(h, "web")
	res := h.run(o, req)
	if res.State != protocol.StateRejected || !strings.Contains(res.ErrorMsg, "is not on refs/heads/feature/try") {
		t.Fatalf("got %s/%s: %s", res.State, res.ErrorCode, res.ErrorMsg)
	}
	if len(h.source.fetched) != 1 || len(h.source.opened) != 0 {
		t.Errorf("fetched %d, opened %d", len(h.source.fetched), len(h.source.opened))
	}
}
