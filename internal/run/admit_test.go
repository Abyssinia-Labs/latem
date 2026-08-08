package run

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Abyssinia-Labs/latem/internal/config"
	"github.com/Abyssinia-Labs/latem/internal/store"
	"github.com/Abyssinia-Labs/latem/pkg/protocol"
)

func TestAdmit_QueuesAnAllowedRequest(t *testing.T) {
	h := newHarness(t)
	o := h.build()

	before := time.Now()
	r, err := o.Admit(t.Context(), h.request("test"), 7)
	if err != nil {
		t.Fatalf("Admit: %v", err)
	}
	if r.State != protocol.StateQueued {
		t.Fatalf("state = %s, want queued", r.State)
	}
	if r.ExpiresAt == nil || r.ExpiresAt.Before(before.Add(23*time.Hour)) {
		t.Errorf("queue_ttl deadline = %v, want ~24h out", r.ExpiresAt)
	}

	row := mustGet(t, h.db, "run1")
	if row.State != protocol.StateQueued {
		t.Errorf("stored state = %s", row.State)
	}
	if row.ExpiresAt == nil {
		t.Error("the deadline did not reach the row, so SweepExpired can never see it")
	}
	if row.OriginKind != "cli" || row.OriginActor != "tester" {
		t.Errorf("origin = %s/%s", row.OriginKind, row.OriginActor)
	}

	// Queued means claimable.
	claimed, err := h.db.ClaimNext(t.Context(), o.RunnerNames())
	if err != nil || claimed.ID != "run1" {
		t.Fatalf("ClaimNext = %+v, %v", claimed, err)
	}
}

func TestAdmit_Gate1UnknownRepo(t *testing.T) {
	h := newHarness(t)
	o := h.build()

	req := h.request("test")
	req.Repo = "not-configured"
	r, err := o.Admit(t.Context(), req, 0)
	if err != nil {
		t.Fatalf("Admit: %v", err)
	}
	if r.State != protocol.StateRejected || r.ErrorCode != protocol.ErrUnknownRepo {
		t.Fatalf("got %s/%s, want rejected/E_UNKNOWN_REPO", r.State, r.ErrorCode)
	}
	if r.FinishedAt == nil {
		t.Error("a terminal admission was not stamped finished")
	}
	if len(h.source.fetched) != 0 {
		t.Error("gate 1 touched the network")
	}
}

func TestAdmit_Gate2InvalidRef(t *testing.T) {
	h := newHarness(t)
	o := h.build()

	req := h.request("test")
	req.Ref = "--upload-pack=/bin/sh"
	r, err := o.Admit(t.Context(), req, 0)
	if err != nil {
		t.Fatalf("Admit: %v", err)
	}
	if r.ErrorCode != protocol.ErrInvalidRef {
		t.Fatalf("code = %s, want E_INVALID_REF", r.ErrorCode)
	}
}

func TestAdmit_Gate3BadInput(t *testing.T) {
	h := newHarness(t)
	o := h.build()

	req := h.request("test")
	req.Inputs = map[string]string{"not an identifier": "x"}
	r, err := o.Admit(t.Context(), req, 0)
	if err != nil {
		t.Fatalf("Admit: %v", err)
	}
	if r.ErrorCode != protocol.ErrBadInput {
		t.Fatalf("code = %s, want E_BAD_INPUT", r.ErrorCode)
	}
}

// Gate 4 is conservative: no expansion of "nowhere-near-policy" can match, so
// the answer is decidable without a fetch.
func TestAdmit_Gate4ParksWithoutTouchingTheNetwork(t *testing.T) {
	h := newHarness(t)
	o := h.build()

	req := h.request("test")
	req.Ref = "feature/experiment"
	r, err := o.Admit(t.Context(), req, 0)
	if err != nil {
		t.Fatalf("Admit: %v", err)
	}
	if r.State != protocol.StateAwaitingApproval {
		t.Fatalf("state = %s, want awaiting_approval", r.State)
	}
	if r.ExpiresAt == nil {
		t.Fatal("no approval_ttl deadline")
	}
	// The repo's own 2h approval_ttl, not the 24h default.
	if d := time.Until(*r.ExpiresAt); d > 3*time.Hour {
		t.Errorf("approval deadline is %s out, want the repo's 2h", d)
	}
	if len(h.source.fetched) != 0 {
		t.Error("gate 4 touched the network")
	}
	// awaiting_approval is not claimable.
	if _, err := h.db.ClaimNext(t.Context(), nil); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("an unapproved run was claimable: %v", err)
	}
}

// With approval disabled the same ref is rejected outright, which is the safe
// default: a non-matching ref is refused rather than queued for a rubber stamp.
func TestAdmit_Gate4RejectsWhenApprovalIsDisabled(t *testing.T) {
	h := newHarness(t)
	o := h.build()

	req := h.request("test")
	req.Repo = "strict"
	req.Ref = "feature/experiment"
	r, err := o.Admit(t.Context(), req, 0)
	if err != nil {
		t.Fatalf("Admit: %v", err)
	}
	if r.State != protocol.StateRejected || r.ErrorCode != protocol.ErrInvalidRef {
		t.Fatalf("got %s/%s, want rejected/E_INVALID_REF", r.State, r.ErrorCode)
	}
}

func TestAdmit_Gate5DiskFloor(t *testing.T) {
	h := newHarness(t)
	h.cfg.Runners = []*config.Runner{{
		Name:     "docker-1",
		Executor: config.ExecutorDocker,
		Docker:   &config.DockerRunner{MinFreeGB: 40},
	}}
	h.caches.free = 3
	o := h.build()

	r, err := o.Admit(t.Context(), h.request("test"), 0)
	if err != nil {
		t.Fatalf("Admit: %v", err)
	}
	if r.ErrorCode != protocol.ErrDisk {
		t.Fatalf("code = %s, want E_DISK", r.ErrorCode)
	}
	if !strings.Contains(r.ErrorMsg, "40") {
		t.Errorf("message does not name the floor: %q", r.ErrorMsg)
	}
}

// A disk gate that cannot measure must not reject: gate 5 guards against a
// full disk, not against an unconfigured one, and gate 17 asks again.
func TestAdmit_Gate5DropsWhenFreeSpaceIsUnknown(t *testing.T) {
	h := newHarness(t)
	h.cfg.Runners = []*config.Runner{{
		Name:     "docker-1",
		Executor: config.ExecutorDocker,
		Docker:   &config.DockerRunner{MinFreeGB: 40},
	}}
	h.caches.freeErr = errors.New("statfs failed")
	o := h.build()

	r, err := o.Admit(t.Context(), h.request("test"), 0)
	if err != nil {
		t.Fatalf("Admit: %v", err)
	}
	if r.State != protocol.StateQueued {
		t.Fatalf("state = %s, want queued", r.State)
	}
}

func TestAdmit_RefusesAnIDThatCouldBecomeAPath(t *testing.T) {
	h := newHarness(t)
	o := h.build()

	req := h.request("test")
	req.ID = "../../../../tmp/x"
	if _, err := o.Admit(t.Context(), req, 0); err == nil {
		t.Fatal("a traversal id was admitted (amendment A6)")
	}
}

func TestAdmit_ReportsTheInitialState(t *testing.T) {
	h := newHarness(t)
	o := h.build()

	if _, err := o.Admit(t.Context(), h.request("test"), 0); err != nil {
		t.Fatalf("Admit: %v", err)
	}
	waitFor(t, "the queued report", func() bool {
		for _, r := range h.broker.updates() {
			if r.runID == "run1" && r.update.State == protocol.StateQueued && r.update.ExpiresAt != nil {
				return true
			}
		}
		return false
	})
}

func TestAdmit_EnqueueFailureIsReported(t *testing.T) {
	h := newHarness(t)
	h.opts.Store = &errStore{Store: h.db, failEnqueue: true}
	o := h.build()

	if _, err := o.Admit(t.Context(), h.request("test"), 0); err == nil {
		t.Fatal("a failed enqueue was swallowed")
	}
}

func TestRequestOf_RoundTripsTheRow(t *testing.T) {
	r := store.Run{
		ID: "run1", Repo: "app", Job: "test", Ref: "main",
		Inputs: map[string]string{"channel": "internal"}, OriginKind: "webhook",
		OriginActor: "bot",
	}
	req := requestOf(r)
	if req.ID != r.ID || req.Repo != r.Repo || req.Job != r.Job || req.Ref != r.Ref {
		t.Fatalf("lost a field: %+v", req)
	}
	if req.Inputs["channel"] != "internal" || req.Origin.Kind != "webhook" {
		t.Fatalf("lost inputs or origin: %+v", req)
	}
}
