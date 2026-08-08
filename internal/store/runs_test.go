package store

import (
	"errors"
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
		ReportedState: protocol.StateRunning,
		LastAckedSeq:  4211,
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
	if got.ReportedState != want.ReportedState || got.LastAckedSeq != want.LastAckedSeq {
		t.Errorf("report state = %q/%d", got.ReportedState, got.LastAckedSeq)
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
	case got.ReportedState != "":
		t.Errorf("reported_state = %q, want empty for a run nothing has acked", got.ReportedState)
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

// TestUnreportedAndMarkReported is what makes broker reporting recoverable
// across a restart (§22): replay every run whose reported_state lags.
func TestUnreportedAndMarkReported(t *testing.T) {
	d := newTestDB(t)
	base := time.Date(2026, 8, 7, 9, 0, 0, 0, time.UTC)

	older := testRun("older", protocol.StateRunning)
	older.CreatedAt = base
	mustCreate(t, d, older)

	newer := testRun("newer", protocol.StateSucceeded)
	newer.CreatedAt = base.Add(time.Hour)
	newer.ReportedState = protocol.StateSucceeded
	mustCreate(t, d, newer)

	// Nothing has acked `older`, so it lags even though reported_state is NULL.
	got, err := d.Unreported(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != "older" {
		t.Fatalf("Unreported = %+v, want just older", got)
	}

	if err := d.MarkReported(t.Context(), "older", protocol.StateRunning); err != nil {
		t.Fatal(err)
	}
	if got, err = d.Unreported(t.Context()); err != nil || len(got) != 0 {
		t.Fatalf("Unreported after ack = %+v, %v", got, err)
	}

	// A transition after the ack puts it back in the replay set, which is the
	// property the reconnect path depends on.
	if err := d.Transition(t.Context(), "older", protocol.StateCollecting, protocol.Update{}); err != nil {
		t.Fatal(err)
	}
	if got, err = d.Unreported(t.Context()); err != nil || len(got) != 1 {
		t.Fatalf("Unreported after a further transition = %+v, %v", got, err)
	}
}

func TestUnreported_OrdersOldestFirst(t *testing.T) {
	d := newTestDB(t)
	base := time.Date(2026, 8, 7, 9, 0, 0, 0, time.UTC)

	for i, id := range []string{"third", "first", "second"} {
		r := testRun(id, protocol.StateRunning)
		r.CreatedAt = base.Add(time.Duration([]int{2, 0, 1}[i]) * time.Hour)
		mustCreate(t, d, r)
	}

	got, err := d.Unreported(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"first", "second", "third"}
	for i, r := range got {
		if r.ID != want[i] {
			t.Fatalf("Unreported = %+v, want replay in the order the runs happened %v", got, want)
		}
	}
}

func TestMarkReported_Rejects(t *testing.T) {
	d := newTestDB(t)
	mustCreate(t, d, testRun("r", protocol.StateRunning))

	if err := d.MarkReported(t.Context(), "r", protocol.RunState("made_up")); err == nil {
		t.Error("MarkReported accepted an unknown state")
	}
	if err := d.MarkReported(t.Context(), "ghost", protocol.StateRunning); !errors.Is(err, ErrNotFound) {
		t.Errorf("MarkReported of an unknown run = %v, want ErrNotFound", err)
	}
}

// TestSetLastAckedSeq_IsMonotonic: an out-of-order ack must not rewind the
// resume point. Rewinding re-sends the frames covering the outage, which is
// exactly the region a duplicate is most confusing in.
func TestSetLastAckedSeq_IsMonotonic(t *testing.T) {
	d := newTestDB(t)
	mustCreate(t, d, testRun("seq", protocol.StateRunning))

	for _, seq := range []uint64{10, 40, 25, 40} {
		if err := d.SetLastAckedSeq(t.Context(), "seq", seq); err != nil {
			t.Fatal(err)
		}
	}

	got, err := d.Get(t.Context(), "seq")
	if err != nil {
		t.Fatal(err)
	}
	if got.LastAckedSeq != 40 {
		t.Errorf("last_acked_seq = %d after acks 10,40,25,40; want 40", got.LastAckedSeq)
	}
}

func TestSetLastAckedSeq_Rejects(t *testing.T) {
	d := newTestDB(t)
	mustCreate(t, d, testRun("seq", protocol.StateRunning))

	// sqlite integers are signed; storing this would read back negative.
	if err := d.SetLastAckedSeq(t.Context(), "seq", 1<<63); err == nil {
		t.Error("SetLastAckedSeq accepted a sequence past the sqlite integer range")
	}
	if err := d.SetLastAckedSeq(t.Context(), "ghost", 1); !errors.Is(err, ErrNotFound) {
		t.Errorf("SetLastAckedSeq on an unknown run = %v, want ErrNotFound", err)
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
