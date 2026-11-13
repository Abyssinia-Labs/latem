package run

import (
	"strings"
	"testing"
	"time"

	"github.com/Abyssinia-Labs/latem/internal/store"
	"github.com/Abyssinia-Labs/latem/pkg/protocol"
)

// changesSHA is a commit a changes run built: in the mirror, on no ref.
const changesSHA = "abcdefabcdefabcdefabcdefabcdefabcdefabcd"

// queueChanges writes the row as a changes run's command writes it (A91): a
// jobs.yaml job pinned to a commit on no ref, with the commit its changes are
// on, approved for exactly that commit. No broker can make this row.
func (h *harness) queueChanges(job, sha, base, approved string) store.Run {
	h.t.Helper()
	ctx := h.t.Context()
	now := time.Now()
	r := store.Run{ID: "run1", Repo: "app", Job: job, Ref: "refs/heads/main", State: protocol.StateQueued,
		OriginKind: "cli", OriginActor: "tester via latem mcp",
		Workflow:   &store.WorkflowRun{SHA: sha, Local: true, Base: base},
		ApprovedAt: &now, ApprovedSHA: approved}
	if err := h.db.Create(ctx, r); err != nil {
		h.t.Fatal(err)
	}
	if err := h.db.Enqueue(ctx, r.ID, 0); err != nil {
		h.t.Fatal(err)
	}
	if err := h.db.Claim(ctx, r.ID); err != nil {
		h.t.Fatal(err)
	}
	return mustGet(h.t, h.db, r.ID)
}

// A changes run builds the commit its command made, unfetched, as the
// jobs.yaml job it names. The job is told the commit the changes are on and
// where the list of changed paths is.
func TestExecute_AChangesRunBuildsItsCommit(t *testing.T) {
	h := newHarness(t)
	base := h.source.sha
	h.source.commits = map[string]bool{changesSHA: false}
	h.source.changed = []string{"src/lib.rs", "tests/it.rs"}
	o := h.build()

	r := h.queueChanges("test", changesSHA, base, changesSHA)
	res, err := o.Execute(t.Context(), r)
	if err != nil {
		t.Fatal(err)
	}
	if res.State != protocol.StateSucceeded {
		t.Fatalf("got %s/%s: %s", res.State, res.ErrorCode, res.ErrorMsg)
	}
	if len(h.source.fetched) != 0 || len(h.source.opened) != 1 {
		t.Errorf("fetched %d time(s) and opened %d: a changes run's commit is already here",
			len(h.source.fetched), len(h.source.opened))
	}
	if got := h.source.checkedOut; len(got) != 1 || got[0] != [2]string{"refs/heads/main", changesSHA} {
		t.Errorf("checked out %v, want the changes' commit", got)
	}

	spec := h.env.runSpec()
	if got := spec.Env[BaseSHAVar]; got != base {
		t.Errorf("%s = %q, want the base %q", BaseSHAVar, got, base)
	}
	if got := spec.Env["LATEM_SHA"]; got != changesSHA {
		t.Errorf("LATEM_SHA = %q, want the changes' commit", got)
	}
	if _, github := spec.Env["GITHUB_SHA"]; github {
		t.Error("a jobs.yaml job of a changes run was given GitHub's environment")
	}
	if got := string(h.env.puts[changedFilesName]); got != "src/lib.rs\ntests/it.rs\n" {
		t.Errorf("the list of changed files holds %q", got)
	}
	steps := h.env.executed()
	if len(steps) == 0 || steps[0].Env[ChangedFilesVar] == "" ||
		!strings.HasSuffix(steps[0].Env[ChangedFilesVar], "/"+changedFilesName) {
		t.Errorf("a step's %s = %q, want the path of the list", ChangedFilesVar, steps[0].Env[ChangedFilesVar])
	}

	logs := h.logContents(r.ID)
	for _, want := range []string{
		"a changes run: abcdefabcdef is " + base[:12], "2 path(s) differ from", "src/lib.rs, tests/it.rs",
		"approved locally at abcdefabcdef",
	} {
		if !strings.Contains(logs, want) {
			t.Errorf("the log does not say %q:\n%s", want, logs)
		}
	}
	if got := mustGet(t, h.db, r.ID); got.SHA != changesSHA || got.ChangesBase() != base {
		t.Errorf("recorded %s on base %q", got.SHA, got.ChangesBase())
	}
}

// A changes run is admitted by an approval of exactly its commit, which the
// command that made it wrote. A row without it is refused, never parked.
func TestExecute_AChangesRunNeedsItsOwnApproval(t *testing.T) {
	h := newHarness(t)
	h.source.commits = map[string]bool{changesSHA: false}
	o := h.build()
	r := h.queueChanges("test", changesSHA, h.source.sha, h.source.sha)
	res, err := o.Execute(t.Context(), r)
	if err != nil {
		t.Fatal(err)
	}
	if res.State != protocol.StateRejected || res.ErrorCode != protocol.ErrInvalidRef {
		t.Fatalf("got %s/%s, want rejected, E_INVALID_REF", res.State, res.ErrorCode)
	}
	if !strings.Contains(res.ErrorMsg, "a run a changes run made is admitted only by an approval") &&
		!strings.Contains(res.ErrorMsg, "admitted only by an approval of its own commit") {
		t.Errorf("the refusal does not say why: %s", res.ErrorMsg)
	}
	if len(h.env.executed()) != 0 {
		t.Error("a step ran")
	}
}

// A commit that is not in the mirror cannot be built, and the answer says to
// make the run again rather than failing somewhere in a checkout.
func TestExecute_AChangesRunWhoseCommitIsGone(t *testing.T) {
	h := newHarness(t)
	o := h.build()
	r := h.queueChanges("test", changesSHA, h.source.sha, changesSHA)
	res, err := o.Execute(t.Context(), r)
	if err != nil {
		t.Fatal(err)
	}
	if res.State != protocol.StateRejected || !strings.Contains(res.ErrorMsg, "is not in this machine's mirror") {
		t.Fatalf("got %s: %s", res.State, res.ErrorMsg)
	}
}
