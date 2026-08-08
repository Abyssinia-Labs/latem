package local

import (
	"strings"
	"testing"
	"time"

	"github.com/Abyssinia-Labs/latem/pkg/protocol"
)

// A run row with no repo, job, or ref is not something the admission gates can
// later reject cleanly — it is a row that fails at the sqlite constraint with
// a message about a NOT NULL column. Refusing it here means the error names
// the field.
func TestEnqueueRun_RejectsIncompleteRequests(t *testing.T) {
	full := runRequest("r_ok")

	tests := []struct {
		name string
		req  protocol.RunRequest
		want string
	}{
		{
			name: "no id",
			req:  protocol.RunRequest{Repo: full.Repo, Job: full.Job, Ref: full.Ref},
			want: "run id is empty",
		},
		{
			name: "traversal id",
			req:  protocol.RunRequest{ID: "../x", Repo: full.Repo, Job: full.Job, Ref: full.Ref},
			want: "run id",
		},
		{
			name: "no repo",
			req:  protocol.RunRequest{ID: "r_1", Job: full.Job, Ref: full.Ref},
			want: "no repo",
		},
		{
			name: "no job",
			req:  protocol.RunRequest{ID: "r_1", Repo: full.Repo, Ref: full.Ref},
			want: "no job",
		},
		{
			name: "no ref",
			req:  protocol.RunRequest{ID: "r_1", Repo: full.Repo, Job: full.Job},
			want: "no ref",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			db := newStore(t)
			err := enqueueRun(t.Context(), db, tc.req, 0, time.Time{})
			if err == nil {
				t.Fatalf("enqueueRun accepted %+v", tc.req)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to mention %q", err, tc.want)
			}
		})
	}
}

// origin_kind is NOT NULL in the §22 schema and the field is audit-only.
// Refusing a run because a display field was blank would be a worse answer
// than recording the truth, which is that it came from the CLI.
func TestEnqueueRun_DefaultsTheOriginKind(t *testing.T) {
	db := newStore(t)

	req := runRequest("r_noorigin")
	req.Origin = protocol.Origin{}
	if err := enqueueRun(t.Context(), db, req, 0, time.Time{}); err != nil {
		t.Fatalf("enqueueRun: %v", err)
	}

	run, err := db.Get(t.Context(), "r_noorigin")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if run.OriginKind != defaultOriginKind {
		t.Errorf("origin_kind = %q, want %q", run.OriginKind, defaultOriginKind)
	}
}

// Origin.RequestedAt is another machine's clock and RUNNER_SPEC §7.1 forbids
// ordering anything by it. created_at must be this machine's own.
func TestEnqueueRun_IgnoresTheRequestersClock(t *testing.T) {
	db := newStore(t)

	req := runRequest("r_clock")
	req.Origin.RequestedAt = time.Date(1999, 1, 1, 0, 0, 0, 0, time.UTC)
	if err := enqueueRun(t.Context(), db, req, 0, time.Time{}); err != nil {
		t.Fatalf("enqueueRun: %v", err)
	}

	run, err := db.Get(t.Context(), "r_clock")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if run.CreatedAt.Year() < 2020 {
		t.Errorf("created_at = %v; the requester's clock was trusted", run.CreatedAt)
	}
}
