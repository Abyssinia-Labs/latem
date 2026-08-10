package store

import (
	"errors"
	"testing"
	"time"

	"github.com/Abyssinia-Labs/latem/pkg/protocol"
)

// The tests in this file are the ones amendment A28 exists for. Every one of
// them passed before the change — against a single column, with one transport —
// and the property none of them could express is the one that matters on a
// machine running two: that the answer to "what has been acknowledged" depends
// on who is asking.

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
	mustCreate(t, d, newer)
	if err := d.MarkReported(t.Context(), BrokerLocal, "newer", protocol.StateSucceeded); err != nil {
		t.Fatal(err)
	}

	// Nothing has acked `older`, so it lags even though it has no row at all.
	got, err := d.Unreported(t.Context(), BrokerLocal)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != "older" {
		t.Fatalf("Unreported = %+v, want just older", got)
	}

	if err := d.MarkReported(t.Context(), BrokerLocal, "older", protocol.StateRunning); err != nil {
		t.Fatal(err)
	}
	if got, err = d.Unreported(t.Context(), BrokerLocal); err != nil || len(got) != 0 {
		t.Fatalf("Unreported after ack = %+v, %v", got, err)
	}

	// A transition after the ack puts it back in the replay set, which is the
	// property the reconnect path depends on.
	if err := d.Transition(t.Context(), "older", protocol.StateCollecting, protocol.Update{}); err != nil {
		t.Fatal(err)
	}
	if got, err = d.Unreported(t.Context(), BrokerLocal); err != nil || len(got) != 1 {
		t.Fatalf("Unreported after a further transition = %+v, %v", got, err)
	}
}

// TestUnreported_AnswersPerTransport is the defect A28 fixed, as one assertion.
//
// The local broker marks a transition reported the instant it is persisted. If
// that mark also satisfied the remote transport — which is exactly what one
// shared column did — then a run that finished while the control plane was
// unreachable would never appear in its replay set, and the control plane would
// show that build as running forever.
func TestUnreported_AnswersPerTransport(t *testing.T) {
	d := newTestDB(t)
	mustCreate(t, d, testRun("both", protocol.StateSucceeded))

	if err := d.MarkReported(t.Context(), BrokerLocal, "both", protocol.StateSucceeded); err != nil {
		t.Fatal(err)
	}

	local, err := d.Unreported(t.Context(), BrokerLocal)
	if err != nil {
		t.Fatal(err)
	}
	if len(local) != 0 {
		t.Errorf("Unreported(local) = %+v after the local broker acked it, want nothing", local)
	}

	remote, err := d.Unreported(t.Context(), BrokerRemote)
	if err != nil {
		t.Fatal(err)
	}
	if len(remote) != 1 || remote[0].ID != "both" {
		t.Fatalf("Unreported(remote) = %+v; the local broker's acknowledgement satisfied the remote transport's replay condition, so a run that finished during an outage would never be sent", remote)
	}

	// And the reverse: acking on one transport does not clear the other.
	if err := d.MarkReported(t.Context(), BrokerRemote, "both", protocol.StateSucceeded); err != nil {
		t.Fatal(err)
	}
	if remote, err = d.Unreported(t.Context(), BrokerRemote); err != nil || len(remote) != 0 {
		t.Fatalf("Unreported(remote) after its own ack = %+v, %v", remote, err)
	}
	if local, err = d.Unreported(t.Context(), BrokerLocal); err != nil || len(local) != 0 {
		t.Fatalf("Unreported(local) = %+v, %v — the remote ack disturbed the local row", local, err)
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

	got, err := d.Unreported(t.Context(), BrokerRemote)
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

// TestUnreported_CarriesTheWholeRow guards the replay's payload. §7.2's Update
// is rebuilt from these fields on reconnect, and the join added by A28 is the
// kind of change that silently returns a projection of NULLs.
func TestUnreported_CarriesTheWholeRow(t *testing.T) {
	d := newTestDB(t)

	exit := 137
	finished := time.Date(2026, 8, 7, 11, 0, 0, 0, time.UTC)
	r := testRun("full", protocol.StateFailed)
	r.ExitCode = &exit
	r.ErrorCode = protocol.ErrStep
	r.ErrorMsg = "step 3 exited 137"
	r.Runner = "mac-tart"
	r.RefNormalized = "refs/heads/main"
	r.SHA = "0123456789abcdef0123456789abcdef01234567"
	r.FinishedAt = &finished
	mustCreate(t, d, r)

	got, err := d.Unreported(t.Context(), BrokerRemote)
	if err != nil || len(got) != 1 {
		t.Fatalf("Unreported = %+v, %v", got, err)
	}
	row := got[0]
	switch {
	case row.ExitCode == nil || *row.ExitCode != exit:
		t.Errorf("exit code = %v", row.ExitCode)
	case row.ErrorCode != protocol.ErrStep || row.ErrorMsg != "step 3 exited 137":
		t.Errorf("error = %q/%q", row.ErrorCode, row.ErrorMsg)
	case row.Runner != "mac-tart":
		t.Errorf("runner = %q", row.Runner)
	case row.RefNormalized != "refs/heads/main" || row.SHA != r.SHA:
		t.Errorf("ref fields = %q/%q", row.RefNormalized, row.SHA)
	case row.FinishedAt == nil || !row.FinishedAt.Equal(finished):
		t.Errorf("finished_at = %v", row.FinishedAt)
	}
}

func TestMarkReported_Rejects(t *testing.T) {
	d := newTestDB(t)
	mustCreate(t, d, testRun("r", protocol.StateRunning))

	if err := d.MarkReported(t.Context(), BrokerLocal, "r", protocol.RunState("made_up")); err == nil {
		t.Error("MarkReported accepted an unknown state")
	}
	if err := d.MarkReported(t.Context(), BrokerLocal, "ghost", protocol.StateRunning); !errors.Is(err, ErrNotFound) {
		t.Errorf("MarkReported of an unknown run = %v, want ErrNotFound", err)
	}
	if err := d.MarkReported(t.Context(), "", "r", protocol.StateRunning); err == nil {
		t.Error("MarkReported accepted an empty broker key; the row would go to a bucket nothing reads")
	}
}

// TestSetLastAckedSeq_IsMonotonic: an out-of-order ack must not rewind the
// resume point. Rewinding re-sends the frames covering the outage, which is
// exactly the region a duplicate is most confusing in.
func TestSetLastAckedSeq_IsMonotonic(t *testing.T) {
	d := newTestDB(t)
	mustCreate(t, d, testRun("seq", protocol.StateRunning))

	// The last write is the low one on purpose. A sequence ending on its own
	// maximum passes whether or not the write is monotonic, which is the shape
	// this test had before A28 and the reason it could not tell the difference.
	for _, seq := range []uint64{10, 40, 25} {
		if err := d.SetLastAckedSeq(t.Context(), BrokerRemote, "seq", seq); err != nil {
			t.Fatal(err)
		}
	}

	got, err := d.LastAckedSeq(t.Context(), BrokerRemote, "seq")
	if err != nil {
		t.Fatal(err)
	}
	if got != 40 {
		t.Errorf("last_acked_seq = %d after acks 10,40,25; want 40 — a rewind re-sends the frames covering the outage", got)
	}
}

// TestLastAckedSeq_IsPerTransport is finding F6 restated for two transports.
//
// The local broker's resume point runs ahead of the remote one by however much
// the network is behind, and while the two shared one monotonic column the
// higher always won. The consequence was specific and silent: the remote
// transport resumed ingestion past everything the local one had written to
// disk, so the frames covering the outage were never sent and never asked for.
func TestLastAckedSeq_IsPerTransport(t *testing.T) {
	d := newTestDB(t)
	mustCreate(t, d, testRun("split", protocol.StateRunning))

	if err := d.SetLastAckedSeq(t.Context(), BrokerLocal, "split", 5000); err != nil {
		t.Fatal(err)
	}
	if err := d.SetLastAckedSeq(t.Context(), BrokerRemote, "split", 10); err != nil {
		t.Fatal(err)
	}

	remote, err := d.LastAckedSeq(t.Context(), BrokerRemote, "split")
	if err != nil {
		t.Fatal(err)
	}
	if remote != 10 {
		t.Errorf("LastAckedSeq(remote) = %d, want 10 — the local transport's position leaked into it and %d frames would be skipped", remote, remote-10)
	}

	local, err := d.LastAckedSeq(t.Context(), BrokerLocal, "split")
	if err != nil {
		t.Fatal(err)
	}
	if local != 5000 {
		t.Errorf("LastAckedSeq(local) = %d, want 5000 — the remote transport's lower position rewound it", local)
	}
}

func TestLastAckedSeq_UnstreamedRunIsZeroAndUnknownRunIsNotFound(t *testing.T) {
	d := newTestDB(t)
	mustCreate(t, d, testRun("quiet", protocol.StateRunning))

	got, err := d.LastAckedSeq(t.Context(), BrokerRemote, "quiet")
	if err != nil {
		t.Fatalf("LastAckedSeq on a run with no frames = %v, want 0 and no error", err)
	}
	if got != 0 {
		t.Errorf("LastAckedSeq = %d, want 0; ingestion begins at 1", got)
	}

	if _, err := d.LastAckedSeq(t.Context(), BrokerRemote, "ghost"); !errors.Is(err, ErrNotFound) {
		t.Errorf("LastAckedSeq of an unknown run = %v, want ErrNotFound", err)
	}
	if _, err := d.LastAckedSeq(t.Context(), "", "quiet"); err == nil {
		t.Error("LastAckedSeq accepted an empty broker key")
	}
}

func TestSetLastAckedSeq_Rejects(t *testing.T) {
	d := newTestDB(t)
	mustCreate(t, d, testRun("seq", protocol.StateRunning))

	// sqlite integers are signed; storing this would read back negative.
	if err := d.SetLastAckedSeq(t.Context(), BrokerLocal, "seq", 1<<63); err == nil {
		t.Error("SetLastAckedSeq accepted a sequence past the sqlite integer range")
	}
	if err := d.SetLastAckedSeq(t.Context(), BrokerLocal, "ghost", 1); !errors.Is(err, ErrNotFound) {
		t.Errorf("SetLastAckedSeq on an unknown run = %v, want ErrNotFound", err)
	}
	if err := d.SetLastAckedSeq(t.Context(), "", "seq", 1); err == nil {
		t.Error("SetLastAckedSeq accepted an empty broker key")
	}
}

// TestBrokerState_RowIsCreatedLazily pins the shape the table is meant to have:
// a run nobody has reported and nobody has streamed occupies nothing, and reads
// exactly as a run whose row says "nothing yet".
func TestBrokerState_RowIsCreatedLazily(t *testing.T) {
	d := newTestDB(t)
	mustCreate(t, d, testRun("lazy", protocol.StateRunning))

	var rows int
	if err := d.db.QueryRowContext(t.Context(),
		`SELECT COUNT(*) FROM broker_state WHERE run_id = 'lazy'`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 0 {
		t.Errorf("a freshly created run already has %d broker_state rows", rows)
	}

	got, err := d.Unreported(t.Context(), BrokerLocal)
	if err != nil || len(got) != 1 {
		t.Fatalf("Unreported = %+v, %v; a run with no row must count as lagging", got, err)
	}
}

// A negative resume point can only come from a hand-edited database, and it has
// exactly one safe answer. Read as unsigned it is a position in the far future,
// so a transport starting there would skip the whole of a build's log without
// anything saying so.
func TestLastAckedSeq_RefusesANegativeStoredValue(t *testing.T) {
	d := newTestDB(t)
	mustCreate(t, d, testRun("bent", protocol.StateRunning))
	if err := d.SetLastAckedSeq(t.Context(), BrokerRemote, "bent", 5); err != nil {
		t.Fatal(err)
	}
	if _, err := d.db.ExecContext(t.Context(),
		`UPDATE broker_state SET last_acked_seq = -1 WHERE run_id = 'bent'`); err != nil {
		t.Fatal(err)
	}

	if got, err := d.LastAckedSeq(t.Context(), BrokerRemote, "bent"); err == nil {
		t.Errorf("LastAckedSeq returned %d for a negative stored value; every frame of the run's log would be skipped", got)
	}
}

// TestUnreported_RefusesAnEmptyBrokerKey. The other two writers are covered
// beside their own tests; this one is the read, and a reader with no key would
// answer "nothing to replay" for every transport at once.
func TestUnreported_RefusesAnEmptyBrokerKey(t *testing.T) {
	d := newTestDB(t)
	if _, err := d.Unreported(t.Context(), ""); err == nil {
		t.Error("Unreported accepted an empty broker key")
	}
}
